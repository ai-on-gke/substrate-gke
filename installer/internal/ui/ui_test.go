// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// pump runs the model synchronously: it feeds msg, executes every returned
// command inline, and keeps going until the queue drains. Cursor blink
// messages are dropped so text inputs cannot loop forever.
func pump(t *testing.T, app *App, first tea.Msg) {
	t.Helper()
	queue := []tea.Msg{first}
	deadline := time.Now().Add(30 * time.Second)
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 20000 || time.Now().After(deadline) {
			t.Fatalf("pump did not converge (step %v)", app.mach.Current())
		}
		msg := queue[0]
		queue = queue[1:]
		_, cmd := app.Update(msg)
		queue = append(queue, runCmd(cmd)...)
	}
}

// runCmd executes a command tree and returns the produced messages.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmd(c)...)
		}
		return out
	}
	if strings.Contains(fmt.Sprintf("%T", msg), "BlinkMsg") {
		return nil
	}
	return []tea.Msg{msg}
}

func key(s string) tea.Msg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		panic("unknown key " + s)
	}
}

func testApp(t *testing.T) *App {
	t.Helper()
	deps := &Deps{
		Setup:   state.NewSetup(),
		Runner:  execx.DryRun{Delay: time.Millisecond},
		GCP:     &gcp.Client{DryRun: true},
		Builder: snapshot.NewBuilder(t.TempDir(), false),
		Checks:  doctor.Checks(t.TempDir(), true),
		DryRun:  true,
	}
	return NewApp(deps)
}

// TestDryRunWizardEndToEnd walks the whole flow: welcome → doctor → project
// → cluster (existing, substrate-ready) → provision → control plane →
// autoscaling (skip) → demo (skip) → complete.
func TestDryRunWizardEndToEnd(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, m := range runCmd(app.Init()) {
		pump(t, app, m)
	}

	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}

	press("enter") // welcome → doctor
	if app.mach.Current() != state.CheckSetup {
		t.Fatalf("after welcome: %v", app.mach.Current())
	}
	press("enter") // doctor → images
	if app.mach.Current() != state.Images {
		t.Fatalf("after doctor: %v", app.mach.Current())
	}
	// Take the pre-built images: enter picks them, then the offered registry,
	// tag and commit are accepted in turn. Nothing is built, so the project is
	// never asked for a registry to push to.
	press("enter", "enter", "enter", "enter")
	if app.mach.Current() != state.Project {
		t.Fatalf("after images: %v", app.mach.Current())
	}
	if app.deps.Setup.ImageRepo != snapshot.ReleaseRepo || app.deps.Setup.ImageTag != snapshot.ReleaseVersion {
		t.Fatalf("images step did not record the release: %+v", app.deps.Setup)
	}
	// Project ID was prefilled by the dry-run gcloud client; enter moves
	// focus to zone, then snapshot bucket, then validates and advances.
	press("enter", "enter", "enter")
	if app.mach.Current() != state.Cluster {
		t.Fatalf("after project: %v", app.mach.Current())
	}
	if app.deps.Setup.ProjectNumber == "" {
		t.Fatal("project number not resolved")
	}
	press("1", "enter") // pick the substrate-ready cluster
	if app.mach.Current() != state.Provision {
		t.Fatalf("after cluster: %v", app.mach.Current())
	}
	if app.deps.Setup.ClusterIsNew {
		t.Fatal("picked an existing cluster, ClusterIsNew must be false")
	}
	press("enter") // bootstrap finished (dry-run) → control plane starts
	if app.mach.Current() != state.ControlPlane {
		t.Fatalf("after provision: %v", app.mach.Current())
	}
	press("enter") // deploy finished → filestore CSI
	if app.mach.Current() != state.FilestoreCSI {
		t.Fatalf("after control plane: %v", app.mach.Current())
	}
	press("s") // skip filestore CSI → autoscaling
	if app.mach.Current() != state.Autoscaling {
		t.Fatalf("after filestore: %v", app.mach.Current())
	}
	press("s") // skip autoscaling → sandbox runtime
	if app.mach.Current() != state.Sandbox {
		t.Fatalf("after autoscaling: %v", app.mach.Current())
	}
	press("enter") // gVisor is the default choice → demo
	if app.mach.Current() != state.Demo {
		t.Fatalf("after sandbox: %v", app.mach.Current())
	}
	if app.deps.Setup.SandboxClass != state.SandboxGVisor {
		t.Fatalf("SandboxClass = %q, want %q", app.deps.Setup.SandboxClass, state.SandboxGVisor)
	}
	press("2", "enter") // skip the demo
	if app.mach.Current() != state.Complete {
		t.Fatalf("after demo: %v", app.mach.Current())
	}
	if !app.Completed {
		t.Fatal("Completed not set")
	}
	if view := app.View(); !strings.Contains(view, "SUBSTRATE IS ON") {
		t.Fatal("final view missing completion banner")
	}
}

// TestCreateNewClusterPath exercises the create-new branch, deploy of
// filestore CSI, and deploy of the demo.
func TestCreateNewClusterPath(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, m := range runCmd(app.Init()) {
		pump(t, app, m)
	}

	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}

	press("enter")                            // welcome
	press("enter")                            // doctor
	press("enter", "enter", "enter", "enter") // images: pre-built (the default), then its three fields
	press("enter", "enter", "enter")          // project fields (pid, zone, bucket)
	press("5", "enter")                       // "create a new cluster" row (4 clusters + create)
	pump(t, app, key("enter"))                // accept the default name
	if app.mach.Current() != state.Provision {
		t.Fatalf("after cluster create: %v", app.mach.Current())
	}
	if !app.deps.Setup.ClusterIsNew {
		t.Fatal("ClusterIsNew must be true")
	}
	press("enter", "enter") // provision, control plane
	if app.mach.Current() != state.FilestoreCSI {
		t.Fatalf("after control plane: %v", app.mach.Current())
	}
	press("1", "enter") // deploy the filestore csi driver (dry-run)
	press("enter")      // filestore finished → autoscaling
	if !app.deps.Setup.FilestoreCSIDeployed {
		t.Fatal("FilestoreCSIDeployed not set")
	}
	press("s") // skip autoscaling → sandbox runtime
	if app.mach.Current() != state.Sandbox {
		t.Fatalf("after autoscaling: %v", app.mach.Current())
	}
	press("enter") // gVisor is the default choice → demo
	if app.mach.Current() != state.Demo {
		t.Fatalf("after sandbox: %v", app.mach.Current())
	}
	press("1", "enter") // deploy the counter demo (dry-run)
	press("enter")      // demo finished → complete
	if app.mach.Current() != state.Complete {
		t.Fatalf("after demo deploy: %v", app.mach.Current())
	}
	if !app.deps.Setup.DemoDeployed {
		t.Fatal("DemoDeployed not set")
	}
}

// Skipping the demo must not leave the completion screen walking the user
// through counter commands that point at nothing.
func TestCompleteScreenShowsDemoStepsOnlyWhenDeployed(t *testing.T) {
	deps := &Deps{Setup: state.NewSetup(), Builder: snapshot.NewBuilder(t.TempDir(), false)}
	scr := newCompleteScreen(deps)

	if out := scr.View(100); strings.Contains(out, "kubectl ate create") {
		t.Errorf("demo steps shown though the demo was skipped:\n%s", out)
	}
	deps.Setup.DemoDeployed = true
	if out := scr.View(100); !strings.Contains(out, "kubectl ate create") {
		t.Errorf("demo steps missing after the demo was deployed:\n%s", out)
	}
}

// TestViewsRenderAtEveryStep guards against panics in any screen's View.
func TestViewsRenderAtEveryStep(t *testing.T) {
	deps := &Deps{
		Setup:   state.NewSetup(),
		Runner:  execx.DryRun{Delay: time.Millisecond},
		GCP:     &gcp.Client{DryRun: true},
		Builder: snapshot.NewBuilder(t.TempDir(), false),
		Checks:  doctor.Checks(t.TempDir(), true),
		DryRun:  true,
	}
	deps.Setup.ProjectID = "acme"
	app := NewApp(deps)
	app.width, app.height = 100, 35
	for _, s := range state.Order {
		scr := app.screenFor(s)
		if out := scr.View(80); out == "" {
			t.Errorf("step %v renders empty", s)
		}
	}
}

func TestClusterSelectionUpdatesDerivedBucket(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, m := range runCmd(app.Init()) {
		pump(t, app, m)
	}

	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}

	press("enter")                            // welcome
	press("enter")                            // doctor
	press("enter", "enter", "enter", "enter") // images: pre-built, then its three fields
	press("enter", "enter", "enter")          // project fields (pid, zone, bucket)
	// Pick row 2: legacy-prod (lacks beta APIs, requires 'y' confirmation)
	press("2", "enter")
	press("y")

	if app.deps.Setup.ClusterName != "legacy-prod" {
		t.Fatalf("ClusterName = %q, want legacy-prod", app.deps.Setup.ClusterName)
	}
	// The cluster name is part of the derivation: two clusters in one
	// project and zone must never share a snapshot bucket.
	wantBucket := "ate-snapshots-my-substrate-project-legacy-prod-us-central1"
	if app.deps.Setup.BucketName != wantBucket {
		t.Fatalf("BucketName = %q, want %q", app.deps.Setup.BucketName, wantBucket)
	}
}

func TestCustomBucketNameQuickstartTrack(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, m := range runCmd(app.Init()) {
		pump(t, app, m)
	}

	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}

	// Welcome: Quickstart (enter)
	press("enter")
	// Doctor: continue (enter)
	press("enter")
	// Images: pre-built, then the offered registry, tag and commit
	press("enter", "enter", "enter", "enter")
	// Project screen:
	// fields: 0:ProjectID, 1:Zone, 2:Bucket
	press("enter", "enter")
	for _, r := range "my-custom-bucket" {
		pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	press("enter") // submit from field 2

	// Cluster screen: pick row 2 (legacy-prod)
	press("2", "enter")
	press("y")

	if app.deps.Setup.BucketName != "my-custom-bucket" {
		t.Fatalf("BucketName = %q, want my-custom-bucket", app.deps.Setup.BucketName)
	}
}

func TestCustomBucketNameAdvancedTrack(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, m := range runCmd(app.Init()) {
		pump(t, app, m)
	}

	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}

	// Welcome: select Advanced track (row 2)
	press("2", "enter")
	// Doctor: continue
	press("enter")
	// Images: build from source, keeping the HEAD commit the screen resolved.
	// Only this path asks for a registry.
	press("2", "enter")
	press("enter")
	// Project screen in Advanced track:
	// fields: 0:ProjectID, 1:Zone, 2:Bucket, 3:MachineType, 4:Network, 5:Subnetwork, 6:Repo
	press("enter", "enter")
	for _, r := range "my-custom-bucket" {
		pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	press("enter", "enter", "enter", "enter", "enter") // submit from field 6

	// Cluster screen: pick row 2 (legacy-prod)
	press("2", "enter")
	press("y")

	if app.deps.Setup.BucketName != "my-custom-bucket" {
		t.Fatalf("BucketName = %q, want my-custom-bucket", app.deps.Setup.BucketName)
	}
}

// The registry is only ever pushed to by a build from source, and the release
// registry is pull-only, so a pre-built install must not be asked for one.
func TestAdvancedProjectScreenAsksForARegistryOnlyWhenBuilding(t *testing.T) {
	labels := func(prebuilt bool) []string {
		app := testApp(t)
		app.deps.Setup.Track = state.TrackAdvanced
		if prebuilt {
			app.deps.Setup.ImageRepo, app.deps.Setup.ImageTag = snapshot.ReleaseRepo, snapshot.ReleaseVersion
		}
		var out []string
		for _, f := range newProjectScreen(app.deps).fields {
			out = append(out, f.label)
		}
		return out
	}
	const registry = "Image registry (leave empty for default)"
	if !slices.Contains(labels(false), registry) {
		t.Errorf("a source build must be asked where to push: %v", labels(false))
	}
	if slices.Contains(labels(true), registry) {
		t.Errorf("a pre-built install pushes nothing: %v", labels(true))
	}
}

// Building from source points the whole run at the commit the user chose: the
// checkout the steps fetch, and the doctor that reports on it.
func TestImagesScreenBuildFromSourceRepointsTheBuilder(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), false)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})

	pump(t, app, key("enter")) // welcome → doctor
	pump(t, app, key("enter")) // doctor → images
	pump(t, app, key("2"))     // build from source
	pump(t, app, key("enter"))
	// The repository is not a field: only the revision in it is asked.
	scr := app.cur.(*imagesScreen)
	if len(scr.fields) != 1 || !strings.Contains(app.View(), snapshot.RepoURL) {
		t.Fatalf("source mode should ask for a revision of %s only, got %d fields:\n%s", snapshot.RepoURL, len(scr.fields), app.View())
	}
	pump(t, app, key("enter")) // submit

	if app.mach.Current() != state.Project {
		t.Fatalf("after images: %v", app.mach.Current())
	}
	if app.deps.Setup.Prebuilt() {
		t.Error("building from source must not record an image repo")
	}
	if len(app.deps.Checks) == 0 {
		t.Error("the doctor checks were not recomputed for the chosen tree")
	}
}

// The published release is a default, not a limit: a team installing its own
// published build types its registry here rather than building from source.
func TestImagesScreenAcceptsAnOverriddenRegistry(t *testing.T) {
	const registry = "us-west1-docker.pkg.dev/acme/substrate"
	const tag = "40ca1ce6"

	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	pump(t, app, key("enter")) // welcome → doctor
	pump(t, app, key("enter")) // doctor → images
	pump(t, app, key("enter")) // pre-built images

	typeOver := func(text string) {
		scr := app.cur.(*imagesScreen)
		scr.fields[scr.focus].SetValue("")
		for _, r := range text {
			pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	typeOver(registry)
	pump(t, app, key("enter")) // registry → tag
	typeOver(tag)
	pump(t, app, key("enter")) // tag → commit
	pump(t, app, key("enter")) // submit, keeping the offered commit

	if app.mach.Current() != state.Project {
		t.Fatalf("after images: %v", app.mach.Current())
	}
	st := app.deps.Setup
	if st.ImageRepo != registry || st.ImageTag != tag {
		t.Fatalf("images = %s:%s, want %s:%s", st.ImageRepo, st.ImageTag, registry, tag)
	}
	// Still a pre-built install: someone else's registry is no more pushed to
	// than the release one.
	if err := st.ApplyProjectDefaults(); err != nil {
		t.Fatal(err)
	}
	if st.KoDockerRepo != "" {
		t.Errorf("KoDockerRepo = %q, want empty for a pre-built install", st.KoDockerRepo)
	}
}

// A tag that cannot be a version has to be caught at the prompt: it becomes the
// node label and the atelet DaemonSet suffix, and ate-setup refuses it after
// the cluster has already been bootstrapped.
func TestImagesScreenRejectsATagThatIsNotALabelValue(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	pump(t, app, key("enter")) // welcome → doctor
	pump(t, app, key("enter")) // doctor → images
	pump(t, app, key("enter")) // pre-built images
	pump(t, app, key("enter")) // registry → tag

	scr := app.cur.(*imagesScreen)
	scr.fields[scr.focus].SetValue("")
	for _, r := range "my/team:v1" {
		pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	pump(t, app, key("enter")) // tag → commit
	pump(t, app, key("enter")) // submit

	if app.mach.Current() != state.Images {
		t.Fatalf("a tag that is not a label value must not advance: %v", app.mach.Current())
	}
	if scr.errText == "" {
		t.Error("no error was shown for the rejected tag")
	}
	if scr.focus != 1 {
		t.Errorf("focus = %d, want the tag field", scr.focus)
	}
}

// ate-setup reads the manifests from a checkout, so pre-built images come with
// the commit to read them from: a registry that is not the release one has no
// commit published alongside it, and the user says which one its images match.
// Moving that commit moves the tree without turning the install into a build.
func TestImagesScreenTakesAManifestRevisionWithPrebuiltImages(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	pump(t, app, key("enter")) // welcome → doctor
	pump(t, app, key("enter")) // doctor → images
	pump(t, app, key("enter")) // pre-built images
	pump(t, app, key("enter")) // registry → tag
	pump(t, app, key("enter")) // tag → commit

	scr := app.cur.(*imagesScreen)
	scr.fields[scr.focus].SetValue("")
	for _, r := range "release-0.1" {
		pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	pump(t, app, key("enter")) // submit; the dry-run resolver answers with the pin

	if app.mach.Current() != state.Project {
		t.Fatalf("after images: %v", app.mach.Current())
	}
	if !app.deps.Setup.Prebuilt() {
		t.Error("naming a manifest revision must not turn this into a build")
	}
	if len(app.deps.Checks) == 0 {
		t.Error("the doctor checks were not recomputed for the chosen tree")
	}
}

func typeText(t *testing.T, app *App, text string) {
	t.Helper()
	for _, r := range text {
		pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestDryRunUpgradeEndToEnd walks the upgrade track: welcome (upgrade) →
// doctor → the cluster named and its record read → images (the release) →
// fetch (dry-run) → complete. Nothing in GCP is touched, and the new version
// is the release the images step offers.
func TestDryRunUpgradeEndToEnd(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}

	press("3", "enter") // upgrade track
	if app.mach.Current() != state.CheckSetup || !app.deps.Setup.Upgrade {
		t.Fatalf("after welcome: %v upgrade=%v", app.mach.Current(), app.deps.Setup.Upgrade)
	}
	press("enter") // doctor → installed cluster
	if app.mach.Current() != state.UpgradeSource {
		t.Fatalf("after doctor: %v", app.mach.Current())
	}
	typeText(t, app, "acme")
	press("enter", "enter", "enter") // cluster and location keep their defaults; the record is read
	if app.mach.Current() != state.Images {
		scr := app.cur.(*upgradeSourceScreen)
		t.Fatalf("after the installed cluster: %v mode=%s err=%q", app.mach.Current(), scr.mode, scr.errText)
	}
	st := app.deps.Setup
	want := "substrate-" + snapshot.ShortCommit()
	if st.ProjectID != "acme" || st.InstalledCommit != snapshot.Commit || st.InstalledVersion != want || st.KoDockerRepo != "gcr.io/acme/ate-images" {
		t.Fatalf("installed cluster not read off the cluster: %+v", st)
	}

	press("enter", "enter", "enter", "enter") // the release, all three fields accepted
	if app.mach.Current() != state.UpgradePlan {
		t.Fatalf("after images: %v", app.mach.Current())
	}
	if !st.Prebuilt() || st.ImageTag != snapshot.ReleaseVersion {
		t.Fatalf("images step did not record the release: %+v", st)
	}
	plan := app.cur.(*upgradePlanScreen)
	if !plan.comp.ok() {
		t.Fatalf("dry-run fetch did not finish: failed=%v", plan.comp.failed)
	}
	if view := app.View(); !strings.Contains(view, snapshot.RunbookURL) || !strings.Contains(view, "Checkout and environment") {
		t.Errorf("plan view lacks the hand-over:\n%s", view)
	}
	press("enter")
	if app.mach.Current() != state.Complete || !app.Completed {
		t.Fatalf("after plan: %v completed=%v", app.mach.Current(), app.Completed)
	}
	if view := app.View(); !strings.Contains(view, "UPGRADE PREPARED") || !strings.Contains(view, snapshot.ReleaseVersion) {
		t.Errorf("final view:\n%s", view)
	}
}

// noRecordRunner fails the cluster read and replays everything else,
// standing in for a cluster the installer cannot reach or read.
type noRecordRunner struct{ inner execx.Runner }

func (r noRecordRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label != "read the installed Substrate" {
		return r.inner.Start(ctx, spec)
	}
	ch := make(chan execx.Event, 1)
	ch <- execx.Event{Done: true, Err: errors.New("error: no atelet daemonset")}
	close(ch)
	return ch
}

// A cluster that cannot be read can still be upgraded: the installed commit
// and version are typed in instead.
func TestUpgradeTrackFallsBackToDescribingTheCluster(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	app.deps.Runner = noRecordRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}
	press("3", "enter", "enter") // upgrade, doctor
	typeText(t, app, "acme")
	press("enter", "enter", "enter") // read fails
	scr := app.cur.(*upgradeSourceScreen)
	if scr.mode != "reading" || scr.comp.failed == nil {
		t.Fatalf("expected the read to fail: mode=%s failed=%v", scr.mode, scr.comp.failed)
	}
	press("m")
	if scr.mode != "manual" {
		t.Fatalf("m should describe by hand, mode=%s", scr.mode)
	}
	const installed = "0123456789abcdef0123456789abcdef01234567"
	typeText(t, app, installed)
	press("enter")
	typeText(t, app, "substrate-0123456789ab")
	press("enter", "enter") // registry blank: a build from source
	if app.mach.Current() != state.Images {
		t.Fatalf("after describing the cluster: %v (%s)", app.mach.Current(), scr.errText)
	}
	st := app.deps.Setup
	if st.InstalledCommit != installed || st.InstalledVersion != "substrate-0123456789ab" || st.InstalledImageRepo != "" {
		t.Fatalf("described cluster not recorded: %+v", st)
	}
	if exports := snapshot.InstalledExports(st); !strings.Contains(exports, "export KO_DOCKER_REPO='gcr.io/acme/ate-images'") {
		t.Errorf("a build from source described by hand rolls back through the project's registry:\n%s", exports)
	}
}

// A pre-built cluster described by hand names its registry, so the rollback
// environment carries the images it runs rather than a build. Leaving the
// images step at the release it already runs is then refused: the runbook
// cannot roll a version onto itself.
func TestUpgradeTrackDescribedByHandAsPrebuiltRefusesTheSameVersion(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	app.deps.Runner = noRecordRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}
	press("3", "enter", "enter")
	typeText(t, app, "acme")
	press("enter", "enter", "enter", "m")
	typeText(t, app, snapshot.Commit)
	press("enter")
	typeText(t, app, snapshot.ReleaseVersion)
	press("enter")
	typeText(t, app, "gcr.io/acme/mirror")
	press("enter")
	if app.mach.Current() != state.Images {
		t.Fatalf("after describing the cluster: %v", app.mach.Current())
	}
	st := app.deps.Setup
	if st.InstalledImageRepo != "gcr.io/acme/mirror" || st.InstalledImageTag != snapshot.ReleaseVersion {
		t.Fatalf("pre-built install not recorded: %+v", st)
	}
	if exports := snapshot.InstalledExports(st); !strings.Contains(exports, "export ATE_IMAGE_TAG="+snapshot.ShellQuote(snapshot.ReleaseVersion)) || strings.Contains(exports, "export KO_DOCKER_REPO") {
		t.Errorf("rollback exports for a pre-built install:\n%s", exports)
	}

	press("enter", "enter", "enter", "enter") // the release again, as installed
	if app.mach.Current() != state.UpgradePlan {
		t.Fatalf("after images: %v", app.mach.Current())
	}
	plan := app.cur.(*upgradePlanScreen)
	if plan.blocked == "" || plan.comp != nil {
		t.Fatalf("upgrading %s onto itself should be refused, got blocked=%q", snapshot.ReleaseVersion, plan.blocked)
	}
	if view := app.View(); !strings.Contains(view, "same as the installed one") {
		t.Errorf("plan view should say why:\n%s", view)
	}
	press("b")
	if app.mach.Current() != state.Images {
		t.Fatalf("back from the refusal should return to the images step, got %v", app.mach.Current())
	}
}

// midUpgradeRunner replays the dry-run read with a second version running,
// as a cluster looks partway through a roll.
type midUpgradeRunner struct{ inner execx.Runner }

func (r midUpgradeRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label == "read the installed Substrate" {
		lines := slices.Clone(spec.SimLines)
		lines[0] += " v9.9.9"
		spec.SimLines = lines
	}
	return r.inner.Start(ctx, spec)
}

// With two versions running, the commit read off the API server belongs to
// the version it reports being. Choosing the other one is allowed, but its
// commit has to be typed in, with everything else already filled.
func TestUpgradeTrackAsksForTheCommitWhenTheAPIServerRunsTheOtherVersion(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	app.deps.Runner = midUpgradeRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}
	press("3", "enter", "enter")
	typeText(t, app, "acme")
	press("enter", "enter", "enter")
	scr := app.cur.(*upgradeSourceScreen)
	if scr.mode != "choose" || len(scr.choices) != 2 {
		t.Fatalf("two running versions should be offered, mode=%s choices=%v", scr.mode, scr.choices)
	}
	press("enter") // the API server's own version: its commit is known
	if app.mach.Current() != state.Images || app.deps.Setup.InstalledCommit != snapshot.Commit {
		t.Fatalf("choosing the API server's version: %v commit=%q", app.mach.Current(), app.deps.Setup.InstalledCommit)
	}

	app = testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	app.deps.Runner = midUpgradeRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press("3", "enter", "enter")
	typeText(t, app, "acme")
	press("enter", "enter", "enter", "j", "enter") // the other version
	scr = app.cur.(*upgradeSourceScreen)
	if scr.mode != "manual" || !strings.Contains(scr.note, "not v9.9.9") {
		t.Fatalf("the other version's commit should be asked for: mode=%s note=%q", scr.mode, scr.note)
	}
	if scr.value(0) != "" || scr.value(1) != "v9.9.9" {
		t.Errorf("manual form should offer the version and an empty commit, got %q / %q", scr.value(0), scr.value(1))
	}
}

// The upgrade track is ahead of the releases, and the first screen says so
// before anyone picks it.
func TestWelcomeSaysUpgradesAreNotYetSupported(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 50})
	if view := app.View(); !strings.Contains(view, "Coming in a later release") || !strings.Contains(view, "reinstall") {
		t.Errorf("welcome should say the upgrade track is not yet supported:\n%s", view)
	}
}

// Without a cache directory the trees would land relative to the working
// directory, and be swept from there; the track is refused up front.
func TestUpgradeTrackNeedsACacheDirectory(t *testing.T) {
	app := testApp(t)
	app.deps.UpgradeDir = ""
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	pump(t, app, key("3"))
	pump(t, app, key("enter"))
	if app.mach.Current() != state.Welcome {
		t.Fatalf("the upgrade track should not start: %v", app.mach.Current())
	}
	if view := app.View(); !strings.Contains(view, "could not be located") {
		t.Errorf("welcome should say why:\n%s", view)
	}
	pump(t, app, key("1"))
	pump(t, app, key("enter"))
	if app.mach.Current() != state.CheckSetup {
		t.Fatalf("the install track should still start: %v", app.mach.Current())
	}
}

// hangingRunner never finishes the cluster read until its context ends, as a
// gcloud or kubectl that hangs would; it replays everything else.
type hangingRunner struct {
	inner execx.Runner
	ctx   context.Context
}

func (r *hangingRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label != "read the installed Substrate" {
		return r.inner.Start(ctx, spec)
	}
	r.ctx = ctx
	ch := make(chan execx.Event, 1)
	go func() {
		<-ctx.Done()
		ch <- execx.Event{Done: true, Err: ctx.Err()}
		close(ch)
	}()
	return ch
}

// A read that hangs can be cancelled with esc, which ends the process behind
// it rather than leaving a late get-credentials to retarget kubectl.
func TestUpgradeTrackCancelsAHangingRead(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	runner := &hangingRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	app.deps.Runner = runner
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}
	press("3", "enter", "enter")
	typeText(t, app, "acme")
	scr := app.cur.(*upgradeSourceScreen)
	press("enter", "enter") // cluster and location keep their defaults
	// The submit is fed by hand and its command dropped: start() has
	// already handed the runner its context, and the read command would
	// block on a channel that only closes with that context.
	app.Update(key("enter"))
	if scr.mode != "reading" || runner.ctx == nil || runner.ctx.Err() != nil {
		t.Fatalf("the read should be running: mode=%s ctx=%v", scr.mode, runner.ctx)
	}
	app.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if scr.mode != "target" || runner.ctx.Err() == nil {
		t.Errorf("esc should cancel the read and return to the form: mode=%s ctxErr=%v", scr.mode, runner.ctx.Err())
	}
}

// failsForRunner fails the cluster read of one cluster and replays the rest.
type failsForRunner struct {
	inner   execx.Runner
	cluster string
}

func (r failsForRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label != "read the installed Substrate" || !strings.Contains(spec.Display, r.cluster) {
		return r.inner.Start(ctx, spec)
	}
	ch := make(chan execx.Event, 1)
	ch <- execx.Event{Done: true, Err: errors.New("error: Unauthorized")}
	close(ch)
	return ch
}

// What a read learned belongs to the cluster it named: retargeting another
// cluster whose read fails must not offer the first cluster's facts as the
// second's.
func TestUpgradeTrackForgetsTheInstalledSideOnRetarget(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	app.deps.Runner = failsForRunner{inner: execx.DryRun{Delay: time.Millisecond}, cluster: "other-cluster"}
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}
	press("3", "enter", "enter")
	typeText(t, app, "acme")
	press("enter", "enter", "enter")
	st := app.deps.Setup
	if app.mach.Current() != state.Images || st.InstalledCommit == "" || st.KoDockerRepo == "" {
		t.Fatalf("first read should have succeeded: %v %+v", app.mach.Current(), st)
	}
	pump(t, app, tea.KeyMsg{Type: tea.KeyEsc}) // back to the cluster form
	scr := app.cur.(*upgradeSourceScreen)
	if app.mach.Current() != state.UpgradeSource || scr.mode != "target" {
		t.Fatalf("esc from images should return to the cluster form: %v mode=%s", app.mach.Current(), scr.mode)
	}
	press("enter") // project → cluster
	scr.fields[scr.focus].SetValue("")
	typeText(t, app, "other-cluster")
	press("enter", "enter") // location → read, which fails
	if scr.mode != "reading" || scr.comp.failed == nil {
		t.Fatalf("second read should fail: mode=%s failed=%v", scr.mode, scr.comp.failed)
	}
	press("m")
	if scr.value(0) != "" || scr.value(1) != "" || scr.value(2) != "" || st.KoDockerRepo != "" {
		t.Errorf("manual form offers the first cluster's facts: %q %q %q ko=%q", scr.value(0), scr.value(1), scr.value(2), st.KoDockerRepo)
	}
}

// Backing out of the final screen takes its summary with it: what is printed
// on exit describes the screen the user left from.
func TestBackFromCompleteClearsCompleted(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	app.deps.UpgradeDir = filepath.Join(t.TempDir(), "upgrades")
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}
	press("3", "enter", "enter")
	typeText(t, app, "acme")
	press("enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter")
	if app.mach.Current() != state.Complete || !app.Completed {
		t.Fatalf("expected the upgrade to be prepared: %v completed=%v", app.mach.Current(), app.Completed)
	}
	pump(t, app, navBack)
	if app.mach.Current() != state.UpgradePlan || app.Completed {
		t.Errorf("back from Complete: %v completed=%v", app.mach.Current(), app.Completed)
	}
}

type installedClusterRunner struct {
	inner    execx.Runner
	versions string
	calls    *int
}

func (r installedClusterRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label == "check for existing Substrate installation" {
		if r.calls != nil {
			*r.calls++
		}
		ch := make(chan execx.Event, 2)
		ch <- execx.Event{Line: strings.TrimSpace("SUBSTRATE_GKE_INSTALLED true " + r.versions)}
		ch <- execx.Event{Done: true}
		close(ch)
		return ch
	}
	return r.inner.Start(ctx, spec)
}

// pressToCluster walks a fresh app to the cluster screen.
func pressToCluster(t *testing.T, app *App) func(keys ...string) {
	t.Helper()
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	press := func(keys ...string) {
		for _, k := range keys {
			pump(t, app, key(k))
		}
	}
	press("enter")                            // welcome -> doctor
	press("enter")                            // doctor -> images
	press("enter", "enter", "enter", "enter") // release images -> project
	press("enter", "enter", "enter")          // project fields -> cluster
	if app.mach.Current() != state.Cluster {
		t.Fatalf("after project: %v", app.mach.Current())
	}
	return press
}

func TestClusterScreenBlocksAlreadyInstalledCluster(t *testing.T) {
	app := testApp(t)
	calls := 0
	app.deps.Runner = installedClusterRunner{inner: execx.DryRun{Delay: time.Millisecond}, versions: "substrate-71e7623", calls: &calls}
	press := pressToCluster(t, app)
	// The list load already background-probed the three substrate-ready
	// clusters; legacy-prod is not ready, so selecting it probes fresh.
	if calls != 3 {
		t.Fatalf("background probes on load = %d, want 3", calls)
	}

	press("2", "enter") // pick legacy-prod (us-central1)
	// Must NOT advance to Provision! Must stay at Cluster and enter "installed" mode
	if app.mach.Current() != state.Cluster {
		t.Fatalf("wizard should remain on Cluster step when installed, got: %v", app.mach.Current())
	}
	scr := app.cur.(*clusterScreen)
	if scr.mode != "installed" {
		t.Fatalf("clusterScreen mode = %q, want %q", scr.mode, "installed")
	}

	view := app.View()
	if !strings.Contains(view, "already runs Substrate") || !strings.Contains(view, "substrate-71e7623") {
		t.Errorf("view missing installed version warning:\n%s", view)
	}
	if !strings.Contains(view, "Upgrade an installed cluster") {
		t.Errorf("view missing upgrade track recommendation:\n%s", view)
	}
	if !strings.Contains(view, "cleanup-gcp") {
		t.Errorf("view missing cleanup-gcp recommendation:\n%s", view)
	}

	// Pressing esc returns to cluster list
	pump(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if scr.mode != "list" {
		t.Errorf("after esc: mode = %q, want list", scr.mode)
	}
	// An aborted selection must leave nothing behind: neither the rejected
	// cluster's name and zone nor a bucket derived from them.
	st := app.deps.Setup
	if st.ClusterName != "substrate-test" || st.Zone != "us-west1-c" || st.BucketName != "" {
		t.Errorf("aborted selection leaked into Setup: cluster=%q zone=%q bucket=%q",
			st.ClusterName, st.Zone, st.BucketName)
	}
	// Back in the list, the probe's verdict is visible on the row — distinct
	// from the "substrate-ready" capability badge, which rightly stays.
	if view := app.View(); !strings.Contains(view, "substrate installed") {
		t.Errorf("list row missing the installed badge:\n%s", view)
	}
	// Re-selecting the same cluster answers from the cache instead of paying
	// another gcloud+kubectl round trip.
	press("enter")
	if scr.mode != "installed" || calls != 4 {
		t.Errorf("re-selection: mode=%q probes=%d, want installed from cache after 4 probes", scr.mode, calls)
	}
	// Pressing 'r' invalidates the cache and re-probes.
	press("r")
	if scr.mode != "installed" || calls != 5 {
		t.Errorf("re-probe: mode=%q probes=%d, want installed after 5 probes", scr.mode, calls)
	}
}

// The list learns install state on its own: substrate-ready clusters are
// probed in the background as the list loads, so their rows carry a badge
// without the user selecting anything. Not-ready clusters are skipped —
// they cannot take an install, so their state decides nothing.
func TestListBackgroundProbesReadyClusters(t *testing.T) {
	app := testApp(t)
	calls := 0
	app.deps.Runner = installedClusterRunner{inner: execx.DryRun{Delay: time.Millisecond}, versions: "substrate-0b3d2d078f64", calls: &calls}
	pressToCluster(t, app)

	if calls != 3 {
		t.Fatalf("background probes = %d, want 3 (only the substrate-ready clusters)", calls)
	}
	if view := app.View(); !strings.Contains(view, "substrate installed") {
		t.Errorf("list row missing the background-probed badge:\n%s", view)
	}
}

// A --dry-run walkthrough shows the guard's whole story off the fixture
// clusters: badges from the background probes, the blocked panel, and a
// simulated teardown that ends clean instead of replaying "installed".
func TestDryRunShowsGuardStates(t *testing.T) {
	app := testApp(t)
	press := pressToCluster(t, app)

	view := app.View()
	for _, want := range []string{"substrate installed", "partial install"} {
		if !strings.Contains(view, want) {
			t.Errorf("dry-run list missing %q badge:\n%s", want, view)
		}
	}
	press("3", "enter") // substrate-installed
	scr := app.cur.(*clusterScreen)
	if scr.mode != "installed" {
		t.Fatalf("mode = %q, want installed", scr.mode)
	}
	press("t", "y") // simulated teardown, then the install continues
	if app.mach.Current() != state.Provision || app.deps.Setup.ClusterName != "substrate-installed" {
		t.Errorf("after dry-run teardown: step=%v cluster=%q, want Provision/substrate-installed",
			app.mach.Current(), app.deps.Setup.ClusterName)
	}
}

// Outside --dry-run the teardown must re-probe for real: the runner flips to
// clean only after the delete spec has run, and the screen has to see that
// rather than assume it.
func TestTeardownReprobesForReal(t *testing.T) {
	deps := &Deps{
		Setup:   state.NewSetup(),
		Runner:  &teardownClusterRunner{inner: execx.DryRun{Delay: time.Millisecond}},
		GCP:     &gcp.Client{DryRun: true},
		Builder: snapshot.NewBuilder(t.TempDir(), false),
	}
	deps.Setup.ProjectID = "acme"
	s := newClusterScreen(deps)
	drive := func(msg tea.Msg) {
		t.Helper()
		for queue := []tea.Msg{msg}; len(queue) > 0; {
			m := queue[0]
			queue = queue[1:]
			queue = append(queue, runCmd(s.Update(m))...)
		}
	}
	clusters, err := deps.GCP.ListClusters(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	drive(clustersMsg{owner: s, clusters: clusters})
	s.cursor = 0 // substrate-poc, background-probed as installed
	drive(key("enter"))
	if s.mode != "installed" {
		t.Fatalf("mode = %q, want installed", s.mode)
	}
	drive(key("t"))
	drive(key("y"))
	if deps.Setup.ClusterName != "substrate-poc" {
		t.Errorf("teardown+reprobe: mode=%q cluster=%q, want substrate-poc chosen after the clean reprobe",
			s.mode, deps.Setup.ClusterName)
	}
}

// Typing an installed cluster's name into "Create a new cluster" must hit
// the same guard as selecting it: Bootstrap is idempotent, so an adopted
// existing cluster would otherwise get the exact mixed-version install the
// guard exists to prevent.
func TestTypedInstalledClusterNameIsStillGuarded(t *testing.T) {
	app := testApp(t)
	app.deps.Runner = installedClusterRunner{inner: execx.DryRun{Delay: time.Millisecond}, versions: "substrate-71e7623"}
	press := pressToCluster(t, app)

	press("enter") // cursor defaults to "Create a new cluster" -> name mode
	scr := app.cur.(*clusterScreen)
	if scr.mode != "name" {
		t.Fatalf("mode = %q, want name", scr.mode)
	}
	scr.nameInput.SetValue("legacy-prod")
	press("enter")
	if app.mach.Current() != state.Cluster || scr.mode != "installed" {
		t.Fatalf("typed installed cluster: step=%v mode=%q, want Cluster/installed", app.mach.Current(), scr.mode)
	}
	if app.deps.Setup.ClusterName != "substrate-test" || app.deps.Setup.ClusterIsNew {
		t.Errorf("blocked name leaked into Setup: %q (new=%v)", app.deps.Setup.ClusterName, app.deps.Setup.ClusterIsNew)
	}
}

// teardownClusterRunner reports the cluster installed until a teardown spec
// has run, then clean — the runner-side view of [t] from the blocked panel.
type teardownClusterRunner struct {
	inner execx.Runner
	torn  bool
}

func (r *teardownClusterRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	switch spec.Label {
	case "ate-setup delete ate-system":
		r.torn = true
	case "check for existing Substrate installation":
		line := "SUBSTRATE_GKE_INSTALLED true substrate-0b3d2d078f64"
		if r.torn {
			line = "SUBSTRATE_GKE_INSTALLED false"
		}
		ch := make(chan execx.Event, 2)
		ch <- execx.Event{Line: line}
		ch <- execx.Event{Done: true}
		close(ch)
		return ch
	}
	return r.inner.Start(ctx, spec)
}

// The blocked panel can run the teardown itself: [t] asks, [y] deletes the
// control plane (keeping the cluster), the guard re-probes, and the install
// continues on the now-clean cluster without leaving the wizard.
func TestInstalledClusterTeardownFromWizard(t *testing.T) {
	app := testApp(t)
	app.deps.Runner = &teardownClusterRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	press := pressToCluster(t, app)

	press("1", "enter") // substrate-poc, reported installed
	scr := app.cur.(*clusterScreen)
	if scr.mode != "installed" {
		t.Fatalf("mode = %q, want installed", scr.mode)
	}
	press("t")
	if scr.mode != "teardown-confirm" {
		t.Fatalf("mode = %q, want teardown-confirm", scr.mode)
	}
	if view := app.View(); !strings.Contains(view, "delete ate-system") {
		t.Errorf("confirm view does not say what it runs:\n%s", view)
	}
	// Backing out returns to the blocked panel, not the list.
	pump(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if scr.mode != "installed" {
		t.Fatalf("after esc: mode = %q, want installed", scr.mode)
	}
	press("t", "y") // tear down, re-probe clean, continue the install
	if app.mach.Current() != state.Provision || app.deps.Setup.ClusterName != "substrate-poc" {
		t.Errorf("after teardown: step=%v cluster=%q, want Provision/substrate-poc",
			app.mach.Current(), app.deps.Setup.ClusterName)
	}
}

// A bare ate-system namespace with no atelet is an interrupted install, not
// a running one; upstream's deploy is documented idempotent, so the screen
// asks instead of hard-blocking with teardown as the only exit.
func TestPartialInstallOffersContinue(t *testing.T) {
	app := testApp(t)
	app.deps.Runner = installedClusterRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	press := pressToCluster(t, app)

	press("1", "enter") // substrate-poc, namespace only
	scr := app.cur.(*clusterScreen)
	if scr.mode != "partial" {
		t.Fatalf("mode = %q, want partial", scr.mode)
	}
	if view := app.View(); !strings.Contains(view, "no atelet") {
		t.Errorf("view missing partial-install explanation:\n%s", view)
	}
	press("y")
	if app.mach.Current() != state.Provision || app.deps.Setup.ClusterName != "substrate-poc" {
		t.Errorf("continue: step=%v cluster=%q, want Provision/substrate-poc",
			app.mach.Current(), app.deps.Setup.ClusterName)
	}
}

type probeFailRunner struct {
	inner execx.Runner
}

func (r probeFailRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label == "check for existing Substrate installation" {
		ch := make(chan execx.Event, 1)
		ch <- execx.Event{Done: true, Err: errors.New("error: Unauthorized")}
		close(ch)
		return ch
	}
	return r.inner.Start(ctx, spec)
}

func TestClusterScreenProbeFails(t *testing.T) {
	app := testApp(t)
	app.deps.Runner = probeFailRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	press := pressToCluster(t, app)

	press("1", "enter")
	scr := app.cur.(*clusterScreen)
	if scr.mode != "probing" || scr.comp.failed == nil {
		t.Fatalf("probe should have failed: mode=%s failed=%v", scr.mode, scr.comp.failed)
	}
	view := app.View()
	if !strings.Contains(view, "Command failed") || !strings.Contains(view, "Unauthorized") {
		t.Errorf("view missing failure message:\n%s", view)
	}

	// esc returns to list
	pump(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if scr.mode != "list" {
		t.Errorf("after esc: mode = %q, want list", scr.mode)
	}

	// A cluster gcloud lists but kubectl cannot reach must stay selectable:
	// the guard is advisory when it cannot run, not a wall. [y] proceeds
	// through the pre-existing readiness check.
	press("enter", "y")
	if app.mach.Current() != state.Provision || app.deps.Setup.ClusterName != "substrate-poc" {
		t.Errorf("continue without check: step=%v cluster=%q, want Provision/substrate-poc",
			app.mach.Current(), app.deps.Setup.ClusterName)
	}
}

func TestLogViewerOverlay(t *testing.T) {
	app := testApp(t)
	app.deps.LogPath = "/tmp/test-installer.log"
	press := pressToCluster(t, app)

	// Pick the substrate-ready cluster (1) and advance to Provision
	press("1", "enter")
	if app.mach.Current() != state.Provision {
		t.Fatalf("expected Provision step, got %v", app.mach.Current())
	}

	// Press 'v' to open log viewer overlay
	press("v")
	if app.over != overlayLog {
		t.Fatalf("expected overlayLog, got %v", app.over)
	}
	view := app.View()
	if !strings.Contains(view, "Log:") || !strings.Contains(view, "press [esc] or [v] to close") {
		t.Errorf("view missing log viewer chrome:\n%s", view)
	}

	// Press 'esc' to dismiss
	pump(t, app, tea.KeyMsg{Type: tea.KeyEsc})
	if app.over != overlayNone {
		t.Fatalf("expected overlayNone after esc, got %v", app.over)
	}

	// Test slash command /log also opens it
	press("/", "l", "o", "g", "enter")
	if app.over != overlayLog {
		t.Fatalf("expected overlayLog via /log, got %v", app.over)
	}

	// Press 'v' to toggle off
	press("v")
	if app.over != overlayNone {
		t.Fatalf("expected overlayNone after v, got %v", app.over)
	}

	// Press 'v' to reopen and verify ctrl+c triggers exit modal
	press("v")
	if app.over != overlayLog {
		t.Fatalf("expected overlayLog, got %v", app.over)
	}
	press("ctrl+c")
	if app.over != overlayExit {
		t.Fatalf("expected overlayExit after ctrl+c in log overlay, got %v", app.over)
	}
}

// errorDetailRunner fails only the provision step: earlier specs (the
// cluster probe among them) must succeed for the wizard to get there.
type errorDetailRunner struct {
	inner execx.Runner
}

func (r errorDetailRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label != "setup-gcp bootstrap" {
		return r.inner.Start(ctx, spec)
	}
	ch := make(chan execx.Event, 4)
	ch <- execx.Event{Line: "Step 1: initializing"}
	ch <- execx.Event{Line: "Error from server (Forbidden): clusterrolebindings is forbidden", Stderr: true}
	ch <- execx.Event{Line: "Cleaning up temporary resources"}
	ch <- execx.Event{Done: true, Err: errors.New("exit status 1")}
	close(ch)
	return ch
}

func TestErrorSnippetAndLogPathSurfaced(t *testing.T) {
	app := testApp(t)
	app.deps.Runner = errorDetailRunner{inner: execx.DryRun{Delay: time.Millisecond}}
	app.deps.LogPath = "/path/to/installer-run.log"
	press := pressToCluster(t, app)

	// Pick the substrate-ready cluster (1) and advance to Provision, which will fail with errorDetailRunner
	press("1", "enter")
	if app.mach.Current() != state.Provision {
		t.Fatalf("expected Provision step, got %v", app.mach.Current())
	}
	scr := app.cur.(*provisionScreen)
	if scr.comp.failed == nil {
		t.Fatalf("expected command to fail")
	}

	view := app.View()
	if !strings.Contains(view, "Command failed: exit status 1") {
		t.Errorf("view missing command failed error code:\n%s", view)
	}
	if !strings.Contains(view, "Cause: Error from server (Forbidden): clusterrolebindings is forbidden") {
		t.Errorf("view missing extracted cause:\n%s", view)
	}
	if !strings.Contains(view, "[v] to view full log") {
		t.Errorf("view missing [v] hint in error panel:\n%s", view)
	}
	if !strings.Contains(view, "/path/to/installer-run.log") {
		t.Errorf("view missing log file path:\n%s", view)
	}
}

func TestClampHeightPreservesFailure(t *testing.T) {
	// Realistic failure layout: Command failed with cause, hints, log path, and 10-line tail panel below it (18+ lines)
	var b strings.Builder
	b.WriteString("Header\nLine 1\nLine 2\nLine 3\nLine 4\n")
	b.WriteString("Command failed: exit status 1\n")
	b.WriteString("Cause: something went wrong\n\n")
	b.WriteString("Press [v] to view full log, [r] to retry.\n")
	b.WriteString("Log file: /path/to/log\n")
	b.WriteString("╭─ Log output ────────╮\n")
	for i := 1; i <= 8; i++ {
		b.WriteString(fmt.Sprintf("│ tail line %d          │\n", i))
	}
	b.WriteString("╰─────────────────────╯\n")

	content := b.String()
	// Even when the tail panel below the banner is taller than the window,
	// the failure-aware clamp keeps the banner and the guidance under it.
	clamped := clampHeightAroundFailure(content, 10)
	if !strings.Contains(clamped, "Command failed") || !strings.Contains(clamped, "Cause:") {
		t.Errorf("clampHeightAroundFailure dropped failure lines:\n%s", clamped)
	}
	if !strings.Contains(clamped, "Press [v]") {
		t.Errorf("clamp dropped the guidance below the banner:\n%s", clamped)
	}
	// The plain clamp keeps the top and never re-anchors on output content.
	if plain := clampHeight(content, 3); !strings.HasPrefix(plain, "Header") {
		t.Errorf("clampHeight no longer keeps the top:\n%s", plain)
	}
}

// TestSandboxSummary pins the three states the completion screen can report.
// The third exists because choosing micro-VM and staging its assets are
// separate steps: an install can name the runtime without having set it up.
func TestSandboxSummary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class string
		done  bool
		want  string
	}{
		{"default is gvisor", "", false, "gVisor"},
		{"explicit gvisor", state.SandboxGVisor, false, "gVisor"},
		{"microvm staged", state.SandboxMicroVM, true, "assets staged"},
		{"microvm not staged", state.SandboxMicroVM, false, "were not staged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.Setup{SandboxClass: tc.class, MicroVMDeployed: tc.done}
			if got := sandboxSummary(st); !strings.Contains(got, tc.want) {
				t.Errorf("sandboxSummary() = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// TestDemoSummary checks the summary text for gVisor and micro-VM demo deploys.
func TestDemoSummary(t *testing.T) {
	if got := demoSummary(&state.Setup{}); got != "skipped" {
		t.Errorf("no demo: got %q, want %q", got, "skipped")
	}
	if got := demoSummary(&state.Setup{DemoDeployed: true}); got != "counter demo deployed" {
		t.Errorf("gvisor demo: got %q", got)
	}
	if got := demoSummary(&state.Setup{DemoDeployed: true, SandboxClass: state.SandboxMicroVM, MicroVMDeployed: false}); got != "counter demo deployed" {
		t.Errorf("unstaged microvm demo should report counter demo deployed, got %q", got)
	}
	got := demoSummary(&state.Setup{DemoDeployed: true, SandboxClass: state.SandboxMicroVM, MicroVMDeployed: true})
	if got != "counter-microvm demo deployed" {
		t.Errorf("microvm install: got %q, want %q", got, "counter-microvm demo deployed")
	}
}

// TestMicroVMSandboxStepStagesAssetsAndDeploysMicroVMDemo verifies that Step 9
// (Choose your sandbox runtime) reflects the selected cluster's KVM status,
// stages the micro-VM assets when option 2 is picked, and deploys the
// counter-microvm demo at Step 10.
func TestMicroVMSandboxStepStagesAssetsAndDeploysMicroVMDemo(t *testing.T) {
	app := testApp(t)
	press := pressToCluster(t, app)

	// Pick row 1 (substrate-poc, which is KVMReady).
	press("1", "enter")
	if !app.deps.Setup.ClusterKVMReady {
		t.Fatal("ClusterKVMReady must be true for substrate-poc")
	}
	press("enter", "enter") // provision -> control plane -> filestore CSI
	press("s", "s")         // skip filestore CSI and autoscaling -> sandbox (Step 9)
	if app.mach.Current() != state.Sandbox {
		t.Fatalf("expected Sandbox step at position 9, got %v", app.mach.Current())
	}
	if view := app.View(); !strings.Contains(view, "has a KVM-capable node pool") {
		t.Errorf("Step 9 view should confirm substrate-poc has KVM:\n%s", view)
	}
	press("2", "enter") // choose Micro-VM and stage assets
	press("enter")      // continue from Sandbox -> Demo (Step 10)
	if app.mach.Current() != state.Demo {
		t.Fatalf("expected Demo step after Sandbox, got %v", app.mach.Current())
	}
	if !app.deps.Setup.MicroVM() || !app.deps.Setup.MicroVMDeployed {
		t.Fatalf("expected MicroVM and MicroVMDeployed to be set: %+v", app.deps.Setup)
	}
	press("1", "enter", "enter") // deploy counter-microvm demo -> Complete
	if app.mach.Current() != state.Complete {
		t.Fatalf("expected Complete step, got %v", app.mach.Current())
	}
	if view := app.View(); !strings.Contains(view, "counter-microvm") {
		t.Errorf("Complete view should mention counter-microvm:\n%s", view)
	}
}

func TestSandboxStepSkipFailureAndNoKVMConfirmation(t *testing.T) {
	jumpToSandbox := func(app *App) {
		pump(t, app, tea.WindowSizeMsg{Width: 100, Height: 35})
		for app.mach.Current() != state.Sandbox {
			app.mach.Next()
		}
		app.cur = app.screenFor(state.Sandbox)
	}

	t.Run("non-KVM cluster requires y confirmation or n cancels", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterName = "no-kvm-cluster"
		app.deps.Setup.ClusterKVMReady = false
		jumpToSandbox(app)

		// Pressing 2 then enter should NOT start staging yet; it asks for [y]/[n].
		pump(t, app, key("2"))
		pump(t, app, key("enter"))
		if view := app.View(); !strings.Contains(view, "Press [y] to stage anyway") {
			t.Fatalf("expected confirmation prompt on non-KVM cluster, got:\n%s", view)
		}
		if app.deps.Setup.MicroVMDeployed {
			t.Fatal("staging should not have started before pressing y")
		}

		// Pressing n cancels and resets cursor to gVisor.
		pump(t, app, key("n"))
		if strings.Contains(app.View(), "Press [y] to stage anyway") {
			t.Fatal("confirmation prompt should be dismissed after pressing n")
		}

		// Pressing 2 -> enter -> y stages the assets and sets MicroVMDeployed immediately.
		pump(t, app, key("2"))
		pump(t, app, key("enter"))
		pump(t, app, key("y"))
		if !app.deps.Setup.MicroVM() || !app.deps.Setup.MicroVMDeployed {
			t.Fatalf("expected MicroVMDeployed=true as soon as staging finishes, got %+v", app.deps.Setup)
		}
	})

	t.Run("/skip before staging resets SandboxClass to gVisor", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.SandboxClass = state.SandboxMicroVM
		app.deps.Setup.MicroVMDeployed = false
		jumpToSandbox(app)

		for _, ch := range "/skip" {
			pump(t, app, key(string(ch)))
		}
		pump(t, app, key("enter"))
		if app.mach.Current() != state.Demo {
			t.Fatalf("expected Demo step after /skip, got %v", app.mach.Current())
		}
		if app.deps.Setup.SandboxClass != state.SandboxGVisor {
			t.Errorf("SandboxClass = %q, want %q", app.deps.Setup.SandboxClass, state.SandboxGVisor)
		}
	})

	t.Run("failed stage then s resets SandboxClass to gVisor", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = true
		jumpToSandbox(app)

		scr := app.cur.(*sandboxScreen)
		app.deps.Setup.SandboxClass = state.SandboxMicroVM
		scr.comp = &execComp{failed: fmt.Errorf("staging failed")}

		pump(t, app, key("s"))
		if app.mach.Current() != state.Demo {
			t.Fatalf("expected Demo step after pressing s on failed stage, got %v", app.mach.Current())
		}
		if app.deps.Setup.SandboxClass != state.SandboxGVisor {
			t.Errorf("SandboxClass = %q, want %q", app.deps.Setup.SandboxClass, state.SandboxGVisor)
		}
	})
}
