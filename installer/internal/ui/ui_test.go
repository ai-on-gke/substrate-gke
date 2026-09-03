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
	"fmt"
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
	// Take the pre-built images: [2] picks them, then the offered registry,
	// tag, manifest repository, and manifest commit are accepted in turn.
	// Nothing is built, so the project is never asked for a registry to push to.
	press("2", "enter", "enter", "enter", "enter", "enter")
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
	press("s") // skip autoscaling → demo
	if app.mach.Current() != state.Demo {
		t.Fatalf("after autoscaling: %v", app.mach.Current())
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

	press("enter")                                          // welcome
	press("enter")                                          // doctor
	press("2", "enter", "enter", "enter", "enter", "enter") // images: pre-built, then its four fields
	press("enter", "enter", "enter")                        // project fields (pid, zone, bucket)
	press("3", "enter")                                     // "create a new cluster" row (2 clusters + create)
	pump(t, app, key("enter"))                              // accept the default name
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
	press("s")          // skip autoscaling
	press("1", "enter") // deploy the counter demo (dry-run)
	press("enter")      // demo finished → complete
	if app.mach.Current() != state.Complete {
		t.Fatalf("after demo deploy: %v", app.mach.Current())
	}
	if !app.deps.Setup.DemoDeployed {
		t.Fatal("DemoDeployed not set")
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

	press("enter")                                          // welcome
	press("enter")                                          // doctor
	press("2", "enter", "enter", "enter", "enter", "enter") // images: pre-built, then its four fields
	press("enter", "enter", "enter")                        // project fields (pid, zone, bucket)
	// Pick row 2: legacy-prod (lacks beta APIs, requires 'y' confirmation)
	press("2", "enter")
	press("y")

	if app.deps.Setup.ClusterName != "legacy-prod" {
		t.Fatalf("ClusterName = %q, want legacy-prod", app.deps.Setup.ClusterName)
	}
	wantBucket := "ate-snapshots-my-substrate-project-us-central1"
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
	// Images: pre-built, then the offered registry, tag, repository, and commit
	press("2", "enter", "enter", "enter", "enter", "enter")
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
	// Images: build from source, keeping the offered repository and the HEAD
	// commit the screen resolved. Only this path asks for a registry.
	press("1", "enter")
	press("enter", "enter")
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
	pump(t, app, key("1"))     // build from source
	pump(t, app, key("enter"))
	pump(t, app, key("enter")) // repository → revision
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
	pump(t, app, key("2"))     // pre-built images
	pump(t, app, key("enter"))

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
	pump(t, app, key("enter")) // tag → manifest repository
	pump(t, app, key("enter")) // manifest repository → manifest commit
	pump(t, app, key("enter")) // submit, keeping the offered tree

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
	pump(t, app, key("2"))     // pre-built images
	pump(t, app, key("enter"))
	pump(t, app, key("enter")) // registry → tag

	scr := app.cur.(*imagesScreen)
	scr.fields[scr.focus].SetValue("")
	for _, r := range "my/team:v1" {
		pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	pump(t, app, key("enter")) // tag → manifest repository
	pump(t, app, key("enter")) // manifest repository → manifest commit
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
// the revision to read them from: a registry that is not the release one has no
// tree published alongside it, and the user says which commit its images match.
// Moving that revision moves the tree without turning the install into a build.
func TestImagesScreenTakesAManifestRevisionWithPrebuiltImages(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	pump(t, app, key("enter")) // welcome → doctor
	pump(t, app, key("enter")) // doctor → images
	pump(t, app, key("2"))     // pre-built images
	pump(t, app, key("enter"))
	pump(t, app, key("enter")) // registry → tag
	pump(t, app, key("enter")) // tag → manifest repository
	pump(t, app, key("enter")) // manifest repository → manifest commit

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
