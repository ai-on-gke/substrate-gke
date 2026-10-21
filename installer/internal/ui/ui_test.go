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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	press("7", "enter")                       // "create a new cluster" row (6 clusters + create)
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
	// fields: 0:ProjectID, 1:Zone, 2:Bucket, 3:MachineType, 4:Network,
	// 5:Subnetwork, 6:ClusterVersion, 7:ReleaseChannel, 8:Repo, 9:RepositoryName
	press("enter", "enter")
	for _, r := range "my-custom-bucket" {
		pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	press("enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter") // submit from field 9

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
		app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
		app.deps.Builder.UseSource(snapshot.Revision{Repo: snapshot.RepoURL, Commit: snapshot.Commit})
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
	source, prebuilt := labels(false), labels(true)
	for _, registry := range []string{"Image registry (leave empty for default)", "Artifact Registry repository (leave empty for default)"} {
		if !slices.Contains(source, registry) {
			t.Errorf("a source build must be asked where to push: %v", source)
		}
		if slices.Contains(prebuilt, registry) {
			t.Errorf("a pre-built install pushes nothing: %v", prebuilt)
		}
	}
}

type permissionTransport func(*http.Request) (*http.Response, error)

func (f permissionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProjectRequestsRepositoryPermissionsOnlyWhenCreating(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte("#!/bin/sh\necho 12345\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	client := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = client })
	http.DefaultClient = &http.Client{Transport: permissionTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"permissions":[]}`
		switch r.URL.Host {
		case "cloudbilling.googleapis.com":
			body = `{"billingEnabled":true}`
		case "serviceusage.googleapis.com":
			body = `{"state":"ENABLED"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	for _, tc := range []struct {
		name, savedRegistry, typedRegistry string
		prebuilt, want                     bool
	}{
		{"default source", "", "", false, true},
		{"typed custom registry", "", "registry.example.com/images", false, false},
		{"cleared custom registry", "registry.example.com/images", "", false, true},
		{"prebuilt images", "", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t)
			app.deps.GCP.DryRun = false
			app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)
			app.deps.Builder.UseSource(snapshot.Revision{Repo: snapshot.RepoURL, Commit: snapshot.Commit})
			st := app.deps.Setup
			st.Track, st.ProjectID, st.KoDockerRepo = state.TrackAdvanced, "acme", tc.savedRegistry
			if tc.prebuilt {
				st.ImageRepo, st.ImageTag = snapshot.ReleaseRepo, snapshot.ReleaseVersion
			}
			scr := newProjectScreen(app.deps)
			for i, f := range scr.fields {
				if f.label == "Image registry (leave empty for default)" {
					scr.fields[i].input.SetValue(tc.typedRegistry)
				}
			}
			msg := scr.submit()().(projValidMsg)
			if msg.err != nil || msg.permErr != nil || len(msg.missing) == 0 {
				t.Fatalf("permission check failed: %+v", msg)
			}
			for _, permission := range gcp.RepositoryPermissions {
				if slices.Contains(msg.missing, permission) != tc.want {
					t.Errorf("missing permissions = %v; want repository permissions = %t", msg.missing, tc.want)
				}
			}
		})
	}
}

// The doctor cannot know yet whether this install builds anything with
// docker, so a docker problem only warns there; the images step repeats it
// next to "Build from source", and installation blocks before provisioning.
func TestDoctorOnlyWarnsAboutDocker(t *testing.T) {
	result := func(status doctor.Status) func(context.Context) doctor.Result {
		return func(context.Context) doctor.Result { return doctor.Result{Status: status, Detail: "d", Fix: "fix"} }
	}
	run := func(checks []doctor.Check) (*App, *doctorScreen, tea.Cmd) {
		app := testApp(t)
		app.deps.Checks = checks
		scr := newDoctorScreen(app.deps)
		for i, c := range scr.checks {
			scr.Update(doctorResMsg{scr, i, c.Run(context.Background())})
		}
		return app, scr, scr.Update(key("enter"))
	}

	app, scr, cmd := run([]doctor.Check{
		{Key: "gcloud", Fatal: true, Run: result(doctor.Pass)},
		{Key: "docker", SourceOnly: true, Run: result(doctor.Warn)},
	})
	if cmd == nil || !app.deps.DockerWarned {
		t.Error("enter should go past a docker warning and record it")
	}
	if !strings.Contains(scr.View(100), "only needed to build Substrate 0.2 or later from source") {
		t.Error("the doctor should say who the docker warning matters to")
	}
	if !strings.Contains(newImagesScreen(app.deps).View(100), "The setup check found Docker problems") {
		t.Error("the images step should repeat the docker warning next to Build from source")
	}

	if app, _, _ := run([]doctor.Check{
		{Key: "gcloud", Fatal: true, Run: result(doctor.Pass)},
		{Key: "docker", SourceOnly: true, Run: result(doctor.Pass)},
	}); app.deps.DockerWarned {
		t.Error("passing docker checks should leave no warning")
	}
	if _, _, cmd := run([]doctor.Check{
		{Key: "gcloud", Fatal: true, Run: result(doctor.Fail)},
		{Key: "docker", SourceOnly: true, Run: result(doctor.Warn)},
	}); cmd != nil {
		t.Error("enter must not go past a check every install needs")
	}
}

// The docker checks run against the registry the install will push to, and
// only when it builds envoy-dataplane with docker at all.
func TestProjectScreenChecksDockerOnlyForAnEnvoyBuild(t *testing.T) {
	t.Setenv("ATE_ATENET_DATAPLANE", "")
	app := testApp(t)
	app.deps.DryRun = false
	app.deps.Builder = snapshot.NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	app.deps.Setup.Track = state.TrackAdvanced

	scr := newProjectScreen(app.deps)
	st := scr.inputSetup("acme")
	if got := scr.dockerRegistry(&st); got != "" {
		t.Errorf("the pin builds nothing with docker, got registry %q", got)
	}
	app.deps.Builder.UseSource(snapshot.Revision{Repo: snapshot.RepoURL, Commit: strings.Repeat("a", 40), EnvoyDataplane: true})
	scr = newProjectScreen(app.deps)
	st = scr.inputSetup("acme")
	if got := scr.dockerRegistry(&st); got != "" {
		t.Errorf("default registry must wait for cluster selection, got %q", got)
	}
	scr.fields[scr.repositoryField-1].input.SetValue("us-docker.pkg.dev/acme/ate")
	st = scr.inputSetup("acme")
	if got := scr.dockerRegistry(&st); got != "us-docker.pkg.dev/acme/ate" {
		t.Errorf("dockerRegistry = %q, want the typed registry", got)
	}
	if app.deps.Setup.KoDockerRepo != "" {
		t.Error("probing the registry must not commit it before the project validates")
	}
	app.deps.Setup.ImageRepo, app.deps.Setup.ImageTag = snapshot.ReleaseRepo, snapshot.ReleaseVersion
	scr = newProjectScreen(app.deps)
	st = scr.inputSetup("acme")
	if got := scr.dockerRegistry(&st); got != "" {
		t.Errorf("a pre-built install builds nothing with docker, got registry %q", got)
	}
}

// A docker problem blocks the step outright, with the fix on screen: unlike a
// missing IAM role there is no one else about to fix it.
func TestProjectScreenBlocksOnDocker(t *testing.T) {
	app := testApp(t)
	scr := newProjectScreen(app.deps)
	failed := projValidMsg{owner: scr, number: "42", docker: []failedCheck{{
		"Docker credentials for gcr.io",
		doctor.Result{Status: doctor.Fail, Detail: "no credentials", Fix: "gcloud auth configure-docker gcr.io"},
	}}}
	if cmd := scr.Update(failed); cmd != nil {
		t.Fatal("a docker failure must not advance the wizard")
	}
	if !strings.Contains(scr.errText, "gcloud auth configure-docker gcr.io") {
		t.Errorf("error should carry the fix, got %q", scr.errText)
	}
	if scr.permAcked {
		t.Error("a docker failure must not count as acknowledging a permission problem")
	}
	if cmd := scr.Update(projValidMsg{owner: scr, number: "42"}); cmd == nil {
		t.Error("once docker passes, the step should advance")
	}
}

// driveProject feeds scr the messages cmd produces, and theirs in turn, until
// they drain. It reports whether the screen asked to advance.
func driveProject(t *testing.T, scr *projectScreen, cmd tea.Cmd) (advanced bool) {
	t.Helper()
	queue := runCmd(cmd)
	deadline := time.Now().Add(30 * time.Second)
	for len(queue) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("project screen did not settle")
		}
		msg := queue[0]
		queue = queue[1:]
		if msg == tea.Msg(navNext) {
			advanced = true
			continue
		}
		queue = append(queue, runCmd(scr.Update(msg))...)
	}
	return advanced
}

// enableRunner records the specs it is handed and, with fail set, fails
// `gcloud services enable` the way a missing role does (or with failLine,
// when set). Everything else it replays.
type enableRunner struct {
	inner    execx.Runner
	fail     bool
	failLine string
	argv     *[][]string
}

func (r enableRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	*r.argv = append(*r.argv, spec.Argv)
	if !r.fail || spec.Label != "enable "+gcp.GKEService {
		return r.inner.Start(ctx, spec)
	}
	line := r.failLine
	if line == "" {
		line = "ERROR: (gcloud.services.enable) PERMISSION_DENIED: Permission denied to enable service [container.googleapis.com]"
	}
	ch := make(chan execx.Event, 2)
	ch <- execx.Event{Line: line, Stderr: true}
	ch <- execx.Event{Done: true, Err: errors.New("exit status 1")}
	close(ch)
	return ch
}

// No billing blocks outright, with the fix on screen, and is not something
// [e] can fix: e keeps typing.
func TestProjectScreenBlocksOnBilling(t *testing.T) {
	app := testApp(t)
	scr := newProjectScreen(app.deps)
	scr.fields[0].input.SetValue("acme")
	if cmd := scr.Update(projValidMsg{owner: scr, number: "42", billingOff: true, apiOff: true}); cmd != nil {
		t.Fatal("a project without billing must not advance the wizard")
	}
	for _, want := range []string{"gcloud billing projects link acme", "billing/linkedaccount?project=acme", gcp.GKEService} {
		if !strings.Contains(scr.errText, want) {
			t.Errorf("error should mention %q, got %q", want, scr.errText)
		}
	}
	if scr.enableFor != "" {
		t.Error("enabling the API cannot work before billing is linked, so it must not be offered")
	}
	scr.Update(key("e"))
	if got := scr.fields[0].input.Value(); got != "acmee" {
		t.Errorf("without an offer e should type, field = %q", got)
	}
	if scr.permAcked || scr.probeAcked {
		t.Error("no billing must not count as acknowledging anything")
	}
	if cmd := scr.Update(projValidMsg{owner: scr, number: "42"}); cmd == nil {
		t.Error("once billing is linked, the step should advance")
	}
}

// A project with billing but without the GKE API blocks, offers [e], and
// [e] enables it and validates again.
func TestProjectScreenEnablesTheGKEAPI(t *testing.T) {
	app := testApp(t)
	var argv [][]string
	app.deps.Runner = enableRunner{inner: execx.DryRun{Delay: time.Millisecond}, argv: &argv}
	scr := newProjectScreen(app.deps)
	scr.fields[0].input.SetValue("acme")
	if cmd := scr.Update(projValidMsg{owner: scr, number: "42", apiOff: true}); cmd != nil {
		t.Fatal("a project without the GKE API must not advance the wizard")
	}
	if scr.enableFor != "acme" || !strings.Contains(scr.errText, gcp.EnableServiceCommand("acme", gcp.GKEService)) {
		t.Fatalf("expected an offer to enable the API on acme, got enableFor=%q err=%q", scr.enableFor, scr.errText)
	}
	if hints := scr.Hints(); hints[0].Key != "e" {
		t.Errorf("hints should lead with [e], got %v", hints)
	}

	cmd := scr.Update(key("e"))
	if scr.enabling == nil || !strings.Contains(scr.View(100), "Enabling "+gcp.GKEService) {
		t.Fatalf("e should start enabling the API:\n%s", scr.View(100))
	}
	if got := scr.fields[0].input.Value(); got != "acme" {
		t.Errorf("the offer's e must not type, field = %q", got)
	}
	// The dry-run client reports a healthy project, so the check that
	// follows a successful enable passes and the step advances.
	if !driveProject(t, scr, cmd) {
		t.Fatalf("after enabling, the project should validate and advance; err=%q", scr.errText)
	}
	want := strings.Join(gcp.EnableService("acme", gcp.GKEService).Argv, " ")
	if len(argv) != 1 || strings.Join(argv[0], " ") != want {
		t.Errorf("runner got %v, want one %q", argv, want)
	}
}

// The running enable is the screen's execComp, and [v] pressed through the
// App opens the log overlay on its live output, as on every other exec
// screen. Without an enable, v is just a letter for the focused field.
func TestProjectScreenExposesTheEnableToTheLogOverlay(t *testing.T) {
	app := testApp(t)
	var argv [][]string
	app.deps.Runner = enableRunner{inner: execx.DryRun{Delay: time.Millisecond}, argv: &argv}
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	scr := newProjectScreen(app.deps)
	app.cur = scr
	var _ execCompProvider = scr
	if scr.logComp() != nil {
		t.Fatal("no enable is running yet, logComp should be nil")
	}
	scr.fields[0].input.SetValue("acme")
	app.Update(key("v"))
	if app.over == overlayLog || scr.fields[0].input.Value() != "acmev" {
		t.Fatalf("with no enable running, v should type; over=%v field=%q", app.over, scr.fields[0].input.Value())
	}
	scr.fields[0].input.SetValue("acme")

	scr.Update(projValidMsg{owner: scr, number: "42", apiOff: true})
	queue := runCmd(scr.Update(key("e")))
	if scr.logComp() == nil || scr.logComp() != scr.enabling {
		t.Fatal("while enabling, logComp should return the running enable")
	}
	// Let the enable print something, but not finish.
	deadline := time.Now().Add(10 * time.Second)
	for len(scr.enabling.lines) == 0 {
		if len(queue) == 0 || time.Now().After(deadline) {
			t.Fatal("the enable printed nothing")
		}
		msg := queue[0]
		queue = queue[1:]
		queue = append(queue, runCmd(scr.Update(msg))...)
	}
	if scr.enabling == nil {
		t.Fatal("the enable finished before v could be pressed")
	}
	app.Update(key("v"))
	if app.over != overlayLog || !app.logFromComp {
		t.Fatalf("v during the enable should open the log on its output; over=%v fromComp=%v", app.over, app.logFromComp)
	}
	if got := scr.fields[0].input.Value(); got != "acme" {
		t.Errorf("v must not reach the field during the enable, got %q", got)
	}
	app.Update(key("v")) // close the overlay
	driveProject(t, scr, cmdOf(queue))
	if scr.logComp() != nil {
		t.Error("once the enable is over, logComp should be nil again")
	}
}

// cmdOf replays already-produced messages as a command.
func cmdOf(msgs []tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, len(msgs))
	for i, m := range msgs {
		m := m
		cmds[i] = func() tea.Msg { return m }
	}
	return tea.Batch(cmds...)
}

// A failed enable says why and how to fix it, and offers [e] again.
func TestProjectScreenEnableFailureOffersRetry(t *testing.T) {
	app := testApp(t)
	var argv [][]string
	app.deps.Runner = enableRunner{inner: execx.DryRun{Delay: time.Millisecond}, fail: true, argv: &argv}
	scr := newProjectScreen(app.deps)
	scr.fields[0].input.SetValue("acme")
	scr.Update(projValidMsg{owner: scr, number: "42", apiOff: true})

	if driveProject(t, scr, scr.Update(key("e"))) {
		t.Fatal("a failed enable must not advance the wizard")
	}
	if scr.enabling != nil {
		t.Error("the enable should be over")
	}
	for _, want := range []string{"PERMISSION_DENIED", "roles/serviceusage.serviceUsageAdmin", "[e] to try again"} {
		if !strings.Contains(scr.errText, want) {
			t.Errorf("error should mention %q, got %q", want, scr.errText)
		}
	}
	if scr.enableFor != "acme" {
		t.Error("a failed enable should stay retryable with [e]")
	}
}

// A failed enable asks the billing API why before picking a panel:
//   - billing provably off: the billing panel, [e] withdrawn
//   - billing provably on: gcloud's text decides permission vs. other, and
//     a "billing" in it is not believed
//   - billing probe cannot answer: gcloud's text is the fallback, billing
//     included
//
// Status names count, never bare codes: an id containing 403 is "other".
func TestProjectScreenEnableFailureHintFollowsTheCause(t *testing.T) {
	const (
		billingLine = "ERROR: (gcloud.services.enable) FAILED_PRECONDITION: Billing must be enabled for activation of service(s) 'container.googleapis.com' to proceed."
		permLine    = "ERROR: (gcloud.services.enable) PERMISSION_DENIED: Permission denied to enable service [container.googleapis.com]"
		// gcloud's PERMISSION_DENIED variants can name billing too.
		permBillLine = "ERROR: (gcloud.services.enable) PERMISSION_DENIED: Permission denied to enable service [container.googleapis.com]; caller lacks serviceusage.services.enable, which is checked before billing."
		otherLine    = "ERROR: (gcloud.services.enable) UNAVAILABLE: The service is currently unavailable."
		id403Line    = "ERROR: (gcloud.services.enable) INTERNAL: Operation operations/acf.p2-403712345678-5d1e failed."
	)
	type probe struct {
		on  bool
		err error
	}
	var (
		billingOn  = probe{on: true}
		billingOff = probe{on: false}
		cannotTell = probe{err: errors.New("403 Forbidden: PERMISSION_DENIED on billingInfo")}
	)
	// Panels, recognized by what only they say.
	const (
		billingPanel = "Billing is not enabled on acme"
		enableBill   = "Billing must be enabled on acme"
		permPanel    = "roles/serviceusage.serviceUsageAdmin"
		retry        = "[e] to try again"
	)
	for _, tc := range []struct {
		name       string
		probe      probe
		line       string
		want, not  []string
		keepsOffer bool
	}{
		{"off/billing text", billingOff, billingLine, []string{billingPanel, "gcloud billing projects link acme"}, []string{permPanel, retry}, false},
		{"off/permission text", billingOff, permLine, []string{billingPanel}, []string{permPanel, retry}, false},
		{"off/other text", billingOff, otherLine, []string{billingPanel}, []string{permPanel, retry}, false},

		{"on/billing text", billingOn, billingLine, []string{"FAILED_PRECONDITION", retry}, []string{billingPanel, enableBill, permPanel}, true},
		{"on/permission text", billingOn, permLine, []string{"PERMISSION_DENIED", permPanel, retry}, []string{billingPanel, enableBill}, true},
		// Billing is confirmed on, so a permission line that mentions
		// billing is a permission failure, not a billing one.
		{"on/permission text naming billing", billingOn, permBillLine, []string{"PERMISSION_DENIED", permPanel, retry}, []string{billingPanel, enableBill}, true},
		{"on/other text", billingOn, otherLine, []string{"UNAVAILABLE", gcp.EnableServiceCommand("acme", gcp.GKEService), retry}, []string{permPanel, enableBill}, true},
		{"on/403 in an id", billingOn, id403Line, []string{"INTERNAL", retry}, []string{permPanel, enableBill}, true},

		{"unknown/billing text", cannotTell, billingLine, []string{"FAILED_PRECONDITION", enableBill, "billing/linkedaccount?project=acme"}, []string{permPanel, retry}, false},
		{"unknown/permission text", cannotTell, permLine, []string{permPanel, retry}, []string{enableBill}, true},
		{"unknown/other text", cannotTell, otherLine, []string{"UNAVAILABLE", retry}, []string{permPanel, enableBill}, true},
	} {
		app := testApp(t)
		var argv [][]string
		app.deps.Runner = enableRunner{inner: execx.DryRun{Delay: time.Millisecond}, fail: true, failLine: tc.line, argv: &argv}
		scr := newProjectScreen(app.deps)
		var asked []string
		scr.billingCheck = func(_ context.Context, pid string) (bool, error) {
			asked = append(asked, pid)
			return tc.probe.on, tc.probe.err
		}
		scr.fields[0].input.SetValue("acme")
		scr.Update(projValidMsg{owner: scr, number: "42", apiOff: true})
		if driveProject(t, scr, scr.Update(key("e"))) {
			t.Fatalf("%s: a failed enable must not advance the wizard", tc.name)
		}
		if len(asked) != 1 || asked[0] != "acme" {
			t.Errorf("%s: billing should be asked once about acme, asked %v", tc.name, asked)
		}
		if scr.validating || scr.checkingEnable {
			t.Errorf("%s: the check should be over", tc.name)
		}
		for _, w := range tc.want {
			if !strings.Contains(scr.errText, w) {
				t.Errorf("%s: error should mention %q, got %q", tc.name, w, scr.errText)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(scr.errText, n) {
				t.Errorf("%s: error should not mention %q, got %q", tc.name, n, scr.errText)
			}
		}
		// A billing failure cannot be retried away, so it withdraws [e]:
		// the hints drop it and e types again. Anything else keeps it.
		if offered := scr.enableFor != ""; offered != tc.keepsOffer {
			t.Errorf("%s: enableFor = %q, want offer kept = %v", tc.name, scr.enableFor, tc.keepsOffer)
		}
		if hints := scr.Hints(); (hints[0].Key == "e") != tc.keepsOffer {
			t.Errorf("%s: hints = %v", tc.name, hints)
		}
		if !tc.keepsOffer {
			before := len(argv)
			scr.Update(key("e"))
			if scr.enabling != nil || len(argv) != before || scr.fields[0].input.Value() != "acmee" {
				t.Errorf("%s: e should type, not re-run the enable; field=%q", tc.name, scr.fields[0].input.Value())
			}
		}
	}
}

// The hint bar only advertises keys that do something: during the billing
// check that is esc alone (not the [e]/[enter] of the offer still standing
// behind it), and while the project validates, nothing.
func TestProjectScreenHintsDuringChecks(t *testing.T) {
	app := testApp(t)
	scr, check, ctxs := startHungCheck(t, app, "ERROR: (gcloud.services.enable) UNAVAILABLE: try again")
	go check()
	<-ctxs
	if hints := scr.Hints(); len(hints) != 1 || hints[0] != (Hint{"esc", "stop waiting"}) {
		t.Errorf("during the billing check hints = %v, want only [esc] stop waiting", hints)
	}
	scr.abandonCheck()

	scr.submit()
	if hints := scr.Hints(); len(hints) != 1 || hints[0] != (Hint{"esc", "stop waiting"}) {
		t.Errorf("while validating hints = %v, want only [esc] stop waiting", hints)
	}
	scr.Stop()
}

// A submit's checks are bounded as a whole and can be abandoned, like the
// billing check: with the network down every gcloud call would otherwise
// wait its own timeout in turn while every key is swallowed. esc stops
// waiting at once (a second esc goes back), v / ? reach the app meanwhile,
// and the abandoned submit's late result must not advance the wizard.
func TestProjectScreenSubmitIsBoundedAndCancellable(t *testing.T) {
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	scr := newProjectScreen(app.deps)
	app.cur = scr
	ctxs := make(chan context.Context, 1)
	scr.projectNumber = func(ctx context.Context, _ string) (string, error) {
		ctxs <- ctx
		<-ctx.Done()
		// A healthy answer that would advance the wizard if this late
		// result were not dropped.
		return "42", nil
	}
	scr.fields[0].input.SetValue("acme")
	cmd := scr.submit()
	if scr.CapturesText() {
		t.Error("while validating the app's keys must get through")
	}
	if hints := scr.Hints(); len(hints) != 1 || hints[0].Key != "esc" {
		t.Errorf("while validating hints = %v, want only [esc]", hints)
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	ctx := <-ctxs
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > submitTimeout {
		t.Errorf("the submit must be bounded by %v, deadline set=%v", submitTimeout, ok)
	}

	app.Update(key("v"))
	if app.over != overlayLog {
		t.Error("v while validating should open the log overlay")
	}
	app.Update(key("v")) // close it

	start := time.Now()
	app.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("esc took %v; it must not wait for the checks", time.Since(start))
	}
	if scr.validating || !strings.Contains(scr.errText, "Stopped validating acme") {
		t.Fatalf("esc should stop validating at once; validating=%v err=%q", scr.validating, scr.errText)
	}
	if ctx.Err() == nil {
		t.Error("esc should cancel the submit's context")
	}
	if !scr.CapturesText() {
		t.Error("after esc the fields should take input again")
	}

	select {
	case late := <-done:
		if cmd := scr.Update(late); cmd != nil {
			t.Error("an abandoned submit's late result must not advance the wizard")
		}
		if !strings.Contains(scr.errText, "Stopped validating acme") {
			t.Errorf("a late result replaced the panel: %q", scr.errText)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled submit did not return")
	}

	// A second esc goes back, as on the plain screen.
	if cmd := scr.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd == nil {
		t.Error("esc with nothing running should go back")
	}
}

// A check cut off by the submit's own deadline says so, whichever check it
// was: the project lookup names its step, and the probe and IAM errors,
// whose panels already name theirs, just gain the time. An error from a
// live context, or no error, is passed through unchanged.
func TestDeadlineHitWordsSubmitTimeouts(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	probe := errors.New(`reading billing info for acme: Get "https://cloudbilling.googleapis.com/…": context deadline exceeded`)

	if got := deadlineHit(expired, probe, ""); !strings.HasPrefix(got.Error(), "timed out after 30s: reading billing info") || !errors.Is(got, probe) {
		t.Errorf("probe error at the deadline = %q, want it prefixed with the submit timeout and still wrapping the original", got)
	}
	if got := deadlineHit(expired, probe, "looking up project acme"); !strings.HasPrefix(got.Error(), "timed out after 30s looking up project acme: ") {
		t.Errorf("lookup error at the deadline = %q, want it to name the step", got)
	}
	if got := deadlineHit(context.Background(), probe, ""); got != probe {
		t.Errorf("an error from a live context should pass through unchanged, got %q", got)
	}
	if got := deadlineHit(expired, nil, ""); got != nil {
		t.Errorf("no error should stay no error, got %v", got)
	}
}

// On a track that builds with docker, the docker checks must not run once
// the submit's deadline has passed: on the dead context every one of them
// fails at once ("daemon is not running"), and Update shows that docker
// panel ahead of probeErr/permErr, hiding the "timed out" that is the real
// story. Here the lookup returns only as the deadline fires, so everything
// after it would run on the expired context.
func TestProjectScreenSkipsDockerChecksAfterTheDeadline(t *testing.T) {
	t.Setenv("ATE_ATENET_DATAPLANE", "")
	defer func(d time.Duration) { submitTimeout = d }(submitTimeout)
	submitTimeout = 20 * time.Millisecond

	app := testApp(t)
	app.deps.DryRun = false
	app.deps.Builder = snapshot.NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	app.deps.Setup.Track = state.TrackAdvanced
	app.deps.Builder.UseSource(snapshot.Revision{Repo: snapshot.RepoURL, Commit: strings.Repeat("a", 40), EnvoyDataplane: true})
	scr := newProjectScreen(app.deps)
	scr.fields[0].input.SetValue("acme")
	scr.fields[scr.repositoryField-1].input.SetValue("us-docker.pkg.dev/acme/ate")
	if st := scr.inputSetup("acme"); scr.dockerRegistry(&st) == "" {
		t.Fatal("setup: this install should build with docker")
	}
	scr.projectNumber = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "42", nil
	}

	msg, ok := scr.submit()().(projValidMsg)
	if !ok {
		t.Fatal("submit should produce a projValidMsg")
	}
	if len(msg.docker) != 0 {
		t.Errorf("docker checks ran on the expired context and reported %d bogus failures, first: %+v", len(msg.docker), msg.docker[0])
	}
}

// startHungCheck drives a failing enable up to the billing check, whose
// billingCheck blocks until its context ends, and returns the check's
// command unrun along with the context the check was handed.
func startHungCheck(t *testing.T, app *App, failLine string) (*projectScreen, tea.Cmd, <-chan context.Context) {
	t.Helper()
	var argv [][]string
	app.deps.Runner = enableRunner{inner: execx.DryRun{Delay: time.Millisecond}, fail: true, failLine: failLine, argv: &argv}
	scr := newProjectScreen(app.deps)
	app.cur = scr
	ctxs := make(chan context.Context, 1)
	scr.billingCheck = func(ctx context.Context, _ string) (bool, error) {
		ctxs <- ctx
		<-ctx.Done()
		// A definite "billing off", which would replace the panel if a
		// late answer were not dropped.
		return false, nil
	}
	scr.fields[0].input.SetValue("acme")
	scr.Update(projValidMsg{owner: scr, number: "42", apiOff: true})
	queue := runCmd(scr.Update(key("e")))
	deadline := time.Now().Add(10 * time.Second)
	for !scr.checkingEnable {
		if len(queue) == 0 || time.Now().After(deadline) {
			t.Fatal("the failed enable never started the billing check")
		}
		msg := queue[0]
		queue = queue[1:]
		cmd := scr.Update(msg)
		if scr.checkingEnable {
			return scr, cmd, ctxs
		}
		queue = append(queue, runCmd(cmd)...)
	}
	t.Fatal("unreachable")
	return nil, nil, nil
}

// The billing check after a failed enable is bounded and can be abandoned:
// the enable often failed because the network is down, and before the
// check its panel appeared at once. esc shows the failure from gcloud's
// text straight away and drops the check's late answer; meanwhile v, / and
// ? reach the app as during the enable.
func TestProjectScreenBillingCheckIsBoundedAndCancellable(t *testing.T) {
	const permLine = "ERROR: (gcloud.services.enable) PERMISSION_DENIED: Permission denied to enable service [container.googleapis.com]"
	app := testApp(t)
	pump(t, app, tea.WindowSizeMsg{Width: 120, Height: 40})
	scr, check, ctxs := startHungCheck(t, app, permLine)
	if scr.CapturesText() {
		t.Error("during the check the app's keys must get through")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- check() }()
	ctx := <-ctxs
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > enableCheckTimeout {
		t.Errorf("the check must be bounded by %v, deadline set=%v in %v", enableCheckTimeout, ok, time.Until(dl))
	}

	app.Update(key("v"))
	if app.over != overlayLog {
		t.Error("v during the check should open the log overlay")
	}
	app.Update(key("v")) // close it

	start := time.Now()
	app.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("esc took %v; it must not wait for the check", time.Since(start))
	}
	if scr.checkingEnable || scr.validating {
		t.Fatal("esc should end the check")
	}
	if !strings.Contains(scr.errText, "roles/serviceusage.serviceUsageAdmin") || scr.enableFor != "acme" {
		t.Errorf("esc should show the failure from gcloud's text (permission, [e] kept); err=%q enableFor=%q", scr.errText, scr.enableFor)
	}
	if ctx.Err() == nil {
		t.Error("esc should cancel the check's context")
	}

	// The abandoned check's answer ("billing off") arrives late and must
	// not replace the panel.
	select {
	case late := <-done:
		shown := scr.errText
		scr.Update(late)
		if scr.errText != shown {
			t.Errorf("a late check result replaced the panel: %q", scr.errText)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled check did not return")
	}
}

// The offer is for the project as validated: editing the fields withdraws
// it, and e types again.
func TestProjectScreenEditWithdrawsTheEnableOffer(t *testing.T) {
	app := testApp(t)
	scr := newProjectScreen(app.deps)
	scr.fields[0].input.SetValue("acme")
	scr.Update(projValidMsg{owner: scr, number: "42", apiOff: true})
	scr.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if scr.enableFor != "" {
		t.Fatal("editing the project should withdraw the offer")
	}
	scr.Update(key("e"))
	if got := scr.fields[0].input.Value(); got != "acme" {
		t.Errorf("field = %q, want the backspaced-then-typed %q", got, "acme")
	}
	if scr.enabling != nil {
		t.Error("e must not enable once the offer is withdrawn")
	}
}

// Moving to another field withdraws the offer too: an e typed into the zone
// or machine type must land there, not start the enable.
func TestProjectScreenFocusChangeWithdrawsTheEnableOffer(t *testing.T) {
	for _, move := range []string{"tab", "down", "shift+tab", "up"} {
		app := testApp(t)
		var argv [][]string
		app.deps.Runner = enableRunner{inner: execx.DryRun{Delay: time.Millisecond}, argv: &argv}
		scr := newProjectScreen(app.deps)
		scr.fields[0].input.SetValue("acme")
		scr.Update(projValidMsg{owner: scr, number: "42", apiOff: true})
		var msg tea.KeyMsg
		switch move {
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "shift+tab":
			msg = tea.KeyMsg{Type: tea.KeyShiftTab}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		}
		scr.Update(msg)
		if scr.enableFor != "" {
			t.Errorf("%s: moving focus should withdraw the offer", move)
		}
		before := scr.fields[scr.focus].input.Value()
		scr.Update(key("e"))
		if scr.enabling != nil || len(argv) != 0 {
			t.Errorf("%s: e must not enable after a focus change", move)
		}
		if got := scr.fields[scr.focus].input.Value(); got != before+"e" {
			t.Errorf("%s: e should type into the focused field, got %q", move, got)
		}
	}
}

// A billing or API probe that could not run proves nothing: it is shown
// once, and the next enter goes on, as a failed permission probe does.
func TestProjectScreenProbeErrorIsAdvisory(t *testing.T) {
	app := testApp(t)
	scr := newProjectScreen(app.deps)
	scr.fields[0].input.SetValue("acme")
	failed := projValidMsg{owner: scr, number: "42", probeErr: errors.New("403 Forbidden: Cloud Billing API has not been used")}
	if cmd := scr.Update(failed); cmd != nil {
		t.Fatal("the first probe error should be shown, not skipped")
	}
	if !strings.Contains(scr.errText, "Could not verify billing") || !strings.Contains(scr.errText, "has not been used") {
		t.Errorf("error should explain what could not be checked, got %q", scr.errText)
	}
	if cmd := scr.Update(failed); cmd == nil {
		t.Error("once shown, a probe error should not block the next enter")
	}
}

// An ack is for the project it was given on. Ack a probe failure (and a
// permission failure) on one project, change the field, and the next
// project's failures are shown again rather than waved through.
func TestProjectScreenEditResetsTheAcks(t *testing.T) {
	app := testApp(t)
	scr := newProjectScreen(app.deps)
	scr.fields[0].input.SetValue("acme")
	probeFail := projValidMsg{owner: scr, number: "42", probeErr: errors.New("403 Forbidden")}
	permFail := projValidMsg{owner: scr, number: "42", missing: gcp.BootstrapPermissions[:1]}
	scr.Update(probeFail)
	scr.Update(permFail)
	if !scr.probeAcked || !scr.permAcked {
		t.Fatalf("both problems should be acknowledged on acme, probe=%v perm=%v", scr.probeAcked, scr.permAcked)
	}

	for _, r := range "-b" {
		scr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if scr.probeAcked || scr.permAcked {
		t.Fatal("editing the project should reset both acks")
	}
	if cmd := scr.Update(probeFail); cmd != nil {
		t.Error("acme-b's probe error must be shown, not swallowed by acme's ack")
	}
	if !strings.Contains(scr.errText, "acme-b") {
		t.Errorf("the warning should be about acme-b, got %q", scr.errText)
	}
	if cmd := scr.Update(permFail); cmd != nil {
		t.Error("acme-b's missing permissions must be shown, not swallowed by acme's ack")
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
	if st.ProjectID != "acme" || st.InstalledCommit != snapshot.Commit || st.InstalledVersion != want || st.KoDockerRepo != "us-west1-docker.pkg.dev/acme/ate-images" {
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
	press("enter", "enter", "enter")
	if app.mach.Current() != state.UpgradeSource || scr.errText == "" {
		t.Fatal("a source install without its registry was accepted")
	}
	if scr.focus != 2 || !strings.Contains(scr.errText, "pre-built") || !strings.Contains(scr.errText, "source") {
		t.Fatalf("missing registry: focus=%d, error=%q", scr.focus, scr.errText)
	}
	press("enter")
	const registry = "europe-west4-docker.pkg.dev/acme/shared-images"
	typeText(t, app, registry)
	press("enter")
	if app.mach.Current() != state.Images {
		t.Fatalf("after describing the cluster: %v (%s)", app.mach.Current(), scr.errText)
	}
	st := app.deps.Setup
	if st.InstalledCommit != installed || st.InstalledVersion != "substrate-0123456789ab" || st.InstalledImageRepo != "" {
		t.Fatalf("described cluster not recorded: %+v", st)
	}
	if exports := snapshot.InstalledExports(st); !strings.Contains(exports, "export KO_DOCKER_REPO="+snapshot.ShellQuote(registry)) {
		t.Errorf("rollback must use the installed source registry:\n%s", exports)
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
	scr := app.cur.(*upgradeSourceScreen)
	typeText(t, app, "gcr.io/acme/old-source")
	press("enter")
	if app.mach.Current() != state.UpgradeSource || scr.errText == "" {
		t.Fatal("two installed registries were accepted")
	}
	scr.fields[3].SetValue("")
	press("enter")
	if app.mach.Current() != state.Images {
		t.Fatalf("after describing the cluster: %v", app.mach.Current())
	}
	st := app.deps.Setup
	if st.InstalledImageRepo != "gcr.io/acme/mirror" || st.InstalledImageTag != snapshot.ReleaseVersion || st.KoDockerRepo != "" {
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
	if scr.value(0) != "" || scr.value(1) != "" || scr.value(2) != "" || scr.value(3) != "" || st.KoDockerRepo != "" {
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
	// The list load already background-probed the four substrate-ready
	// clusters (substrate-ga is ready through v1); legacy-prod is not ready,
	// so selecting it probes fresh.
	if calls != 4 {
		t.Fatalf("background probes on load = %d, want 4", calls)
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
	// " installed" matches the long badge ("· substrate installed") and the
	// short one ("· installed") the list uses when the long ones do not fit,
	// but not the dry-run cluster named substrate-installed.
	if view := app.View(); !strings.Contains(view, " installed") {
		t.Errorf("list row missing the installed badge:\n%s", view)
	}
	// Re-selecting the same cluster answers from the cache instead of paying
	// another gcloud+kubectl round trip.
	press("enter")
	if scr.mode != "installed" || calls != 5 {
		t.Errorf("re-selection: mode=%q probes=%d, want installed from cache after 5 probes", scr.mode, calls)
	}
	// Pressing 'r' invalidates the cache and re-probes.
	press("r")
	if scr.mode != "installed" || calls != 6 {
		t.Errorf("re-probe: mode=%q probes=%d, want installed after 6 probes", scr.mode, calls)
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

	if calls != 4 {
		t.Fatalf("background probes = %d, want 4 (only the substrate-ready clusters)", calls)
	}
	if view := app.View(); !strings.Contains(view, " installed") {
		t.Errorf("list row missing the background-probed badge:\n%s", view)
	}
}

// A --dry-run walkthrough shows the guard's whole story off the fixture
// clusters: badges from the background probes, the blocked panel, a
// simulated teardown that ends clean instead of replaying "installed", and the
// warning that provision turns off a cluster's managed Filestore driver. A
// screen nobody can reach without a GCP project is a screen that regresses
// unseen.
func TestDryRunShowsGuardStates(t *testing.T) {
	app := testApp(t)
	press := pressToCluster(t, app)

	view := app.View()
	// With a leading space, so the cluster names substrate-installed and
	// substrate-partial cannot satisfy them; the short and long badges both do.
	for _, want := range []string{" installed", " partial"} {
		if !strings.Contains(view, want) {
			t.Errorf("dry-run list missing %q badge:\n%s", want, view)
		}
	}
	press("5", "enter") // substrate-installed
	scr := app.cur.(*clusterScreen)
	if scr.mode != "installed" {
		t.Fatalf("mode = %q, want installed", scr.mode)
	}
	press("t", "y") // simulated teardown, then the install continues
	if app.mach.Current() != state.Provision || app.deps.Setup.ClusterName != "substrate-installed" {
		t.Errorf("after dry-run teardown: step=%v cluster=%q, want Provision/substrate-installed",
			app.mach.Current(), app.deps.Setup.ClusterName)
	}

	poc := testApp(t)
	press = pressToCluster(t, poc)
	press("4", "enter") // substrate-ga, ready, managed Filestore driver on
	if poc.mach.Current() != state.Provision || !strings.Contains(poc.cur.View(120), "managed Filestore CSI driver") {
		t.Errorf("substrate-ga: step=%v, want Provision with the Filestore warning:\n%s", poc.mach.Current(), poc.cur.View(120))
	}
}

// networkScreen is a cluster screen showing one hand-written cluster, driven
// directly so a test controls its network configuration.
func networkScreen(t *testing.T, c gcp.Cluster) (*clusterScreen, func(tea.Msg)) {
	t.Helper()
	deps := &Deps{
		Setup:   state.NewSetup(),
		Runner:  execx.DryRun{Delay: time.Millisecond},
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
	drive(clustersMsg{owner: s, clusters: []gcp.Cluster{c}})
	s.cursor = 0
	return s, drive
}

// An existing cluster carries its own network into Setup for bootstrap — here
// a subnet GKE named for it, the usual case — rather than the "default"
// settings meant for a new cluster, which would have bootstrap warn about
// three differences that are not problems.
func TestChoosingAnExistingClusterRecordsItsOwnNetwork(t *testing.T) {
	s, drive := networkScreen(t, gcp.Cluster{
		Name: "prod", Location: "us-central1-a", Status: "RUNNING", MasterVersion: "1.36.4-gke.1247000",
		NodeCount: 3, BetaAPIs: gcp.RequiredBetaAPIs, DataplaneV2: true,
		Network:    "projects/acme/global/networks/default",
		Subnetwork: "projects/acme/regions/us-central1/subnetworks/gke-prod-subnet-55edbf1e",
	})
	drive(key("enter"))
	st := s.deps.Setup
	if st.ClusterName != "prod" || st.ClusterIsNew {
		t.Fatalf("prod was not chosen: cluster=%q new=%v mode=%q", st.ClusterName, st.ClusterIsNew, s.mode)
	}
	if st.ClusterNetwork != "default" || st.ClusterSubnetwork != "gke-prod-subnet-55edbf1e" || !st.ClusterDataplaneV2 {
		t.Errorf("recorded network=%q subnetwork=%q dpv2=%v, want the cluster's own",
			st.ClusterNetwork, st.ClusterSubnetwork, st.ClusterDataplaneV2)
	}
}

// Provision's bootstrap turns off GKE's managed Filestore CSI driver on an
// existing cluster (agent-substrate/substrate#2357), before the user has
// chosen whether the optional Filestore step installs Substrate's. Workloads
// on Filestore volumes lose their driver at that moment, so the user has to
// hear it then — and hear again, at the Filestore step, that skipping leaves
// the cluster with none. A new cluster has no such workloads and gets neither.
func TestTheManagedFilestoreDriverIsNotTurnedOffSilently(t *testing.T) {
	deps := &Deps{
		Setup:   state.NewSetup(),
		Runner:  execx.DryRun{Delay: time.Millisecond},
		GCP:     &gcp.Client{DryRun: true},
		Builder: snapshot.NewBuilder(t.TempDir(), false),
	}
	st := deps.Setup
	st.ProjectID, st.ClusterName, st.ClusterIsNew, st.ClusterFilestoreAddon = "acme", "prod", false, true

	if view := newProvisionScreen(deps).View(120); !strings.Contains(view, "turns off GKE's managed Filestore CSI driver") {
		t.Errorf("provision should warn that the managed Filestore driver is turned off:\n%s", view)
	}
	if view := newFilestoreScreen(deps).View(120); !strings.Contains(view, "already turned off") || !strings.Contains(view, "no Filestore CSI driver") {
		t.Errorf("the Filestore step should say the driver is already off and skipping leaves none:\n%s", view)
	}

	st.ClusterIsNew = true
	if view := newProvisionScreen(deps).View(120); strings.Contains(view, "Filestore") {
		t.Errorf("a new cluster should get no Filestore warning:\n%s", view)
	}
	if view := newFilestoreScreen(deps).View(120); strings.Contains(view, "already turned off") {
		t.Errorf("a new cluster's Filestore step should not claim the driver was turned off:\n%s", view)
	}
}

// The flag reaches Setup from the cluster listing at selection, and does not
// survive into the create path.
func TestChoosingAClusterRecordsItsFilestoreAddon(t *testing.T) {
	s, drive := networkScreen(t, gcp.Cluster{
		Name: "prod", Location: "us-central1-a", Status: "RUNNING", MasterVersion: "1.36.4-gke.1247000",
		NodeCount: 3, BetaAPIs: gcp.RequiredBetaAPIs, FilestoreCSIAddon: true,
	})
	drive(key("enter"))
	if !s.deps.Setup.ClusterFilestoreAddon {
		t.Error("choosing a cluster with the managed Filestore driver did not record it")
	}
}

// flat collapses a rendered view to single-spaced words, dropping panel
// borders, so assertions about copy survive the panel re-wrapping it at a
// different width.
func flat(view string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(view)), " ")
}

// What a cluster without the beta APIs is told depends on its release, and
// the cases are materially different. On 1.37 throughout it needs nothing:
// Substrate uses the v1 APIs there. Below 1.36 nothing that can be enabled
// makes the cluster supported. In between, the repair works but costs
// replacing the pools below 1.37. Offering the wrong one sends someone to
// rebuild a cluster that needed ten minutes, or leaves them watching pods
// that will never mount.
func TestConfirmationOffersTheRemedyForTheRelease(t *testing.T) {
	app := testApp(t)
	press := pressToCluster(t, app)

	press("4", "enter") // substrate-ga: 1.37 everywhere, no beta APIs
	if app.mach.Current() != state.Provision {
		t.Fatalf("a 1.37 cluster with no beta APIs: step=%v, want straight to provision", app.mach.Current())
	}

	app = testApp(t)
	press = pressToCluster(t, app)
	scr := app.cur.(*clusterScreen)
	press("3", "enter") // ml-staging: 1.36 pools, no beta APIs enabled
	if scr.mode != "confirm" {
		t.Fatalf("1.36 cluster: mode=%q, want the confirmation", scr.mode)
	}
	// Never an upgrade to the version the pool already runs: that is the
	// command anyone would reach for, and GKE skips it without replacing a
	// node, so the pods keep failing to mount.
	view := flat(scr.View(120))
	if !strings.Contains(view, "2 node pools run below "+gcp.PodCertificateGARelease.String()+" and must be replaced before Substrate starts") {
		t.Errorf("a 1.36 confirmation should say the pools must be replaced before Substrate starts:\n%s", view)
	}
	if strings.Contains(view, "clusters upgrade") {
		t.Errorf("a 1.36 confirmation must not offer a same-version upgrade, which GKE skips:\n%s", view)
	}

	press("n")          // back to the list
	press("2", "enter") // legacy-prod: 1.33, below the supported floor
	if scr.mode != "confirm" {
		t.Fatalf("1.33 cluster: mode=%q, want the confirmation", scr.mode)
	}
	view = flat(scr.View(120))
	if !strings.Contains(view, "Upgrade its control plane to "+gcp.MinSupportedRelease.String()) {
		t.Errorf("a pre-%s confirmation should ask for a control-plane upgrade:\n%s", gcp.MinSupportedRelease, view)
	}
	// Enabling the beta APIs is not the fix below the floor: on 1.33 GKE
	// rejects the request outright, and even where it would be accepted the
	// release is not one Substrate runs on. 'y' still works, but must not be
	// sold as the way through.
	if strings.Contains(view, "has provision turn them on") {
		t.Errorf("a pre-%s confirmation must not offer enablement as the fix:\n%s", gcp.MinSupportedRelease, view)
	}
	if !strings.Contains(flat(app.View()), "use it anyway (unsupported)") {
		t.Errorf("a pre-%s confirmation should mark 'y' as unsupported:\n%s", gcp.MinSupportedRelease, app.View())
	}
}

// The panels a cluster without the beta APIs leads to have to be readable in
// a small terminal, beside the sidebar. At 80×24 the first version was clamped
// after its first lines, hiding every remedy and the commands, and wrapped API
// names mid-word; then the replacement block lost its last command line once a
// cluster had a third pool. The confirmation's bottom border and the block's
// last command line are what get cut first, so those are what is checked, for
// one to eight pools and both the certain and the advisory wording, and a
// split pool name is checked for directly.
//
// One walk per size, not one per check: a walk costs ~6 s, and the screens
// under test are reachable from one another with a few keys.
func TestConfirmAndReplacementPanelsFitSmallTerminals(t *testing.T) {
	pools := []string{"default-pool", "gpu-pool", "highmem-pool", "batch-workers", "spot-pool", "ingress-nodes", "kvm-pool-2", "system-pool"}
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		app := testApp(t)
		press := pressToCluster(t, app)
		pump(t, app, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, row := range []string{"2", "3"} { // the two confirmations
			press(row, "enter")
			if view := app.View(); !strings.Contains(view, "╰") {
				t.Errorf("%dx%d, row %s: the confirmation is clamped before its end:\n%s", size[0], size[1], row, view)
			}
			press("n") // back to the list
		}

		press("3", "enter", "y") // ml-staging, through provision
		st := app.deps.Setup
		for n := 1; n <= len(pools); n++ {
			for _, unsure := range []bool{false, true} {
				st.ReplacePools = gcp.PoolReplacement{Needed: true, Names: pools[:n], Unsure: unsure}
				view := app.View()
				if strings.Count(view, "--location 'us-central1'") < 2 {
					t.Errorf("%dx%d, %d pools, unsure=%v: the commands are clamped:\n%s", size[0], size[1], n, unsure, view)
				}
				for _, p := range pools[:n] {
					if !strings.Contains(view, p) {
						t.Errorf("%dx%d, %d pools: %q is split or missing:\n%s", size[0], size[1], n, p, view)
					}
				}
			}
		}
	}
}

// The advisory wording is what keeps a re-run from losing the replacement:
// after an earlier run turned the APIs on, ml-staging badges ready and goes
// straight through, and nothing else would tell the user its old pools still
// need replacing before Substrate's pods land on them.
func TestARerunStillTellsTheUserToCheckOldPools(t *testing.T) {
	s, drive, _ := drivenClusterScreen(t)
	drive(clustersMsg{owner: s, clusters: []gcp.Cluster{{
		Name: "ml-staging", Location: "us-central1", Status: "RUNNING", MasterVersion: "1.36.4-gke.1247000", NodeCount: 6,
		BetaAPIs:     gcp.RequiredBetaAPIs, // turned on by the earlier run
		PoolVersions: []gcp.PoolVersion{{Name: "default-pool", Version: "1.36.4-gke.1247000"}},
	}}})
	s.cursor = 0
	drive(key("enter"))
	r := s.deps.Setup.ReplacePools
	if s.deps.Setup.ClusterName != "ml-staging" || !r.Needed || !r.Unsure || !slices.Equal(r.Names, []string{"default-pool"}) {
		t.Fatalf("re-run: cluster=%q pools=%+v, want ml-staging chosen with default-pool to check", s.deps.Setup.ClusterName, r)
	}
}

// A cluster on 1.37 throughout badges ready and goes straight to provision,
// where bootstrap turns the beta APIs on anyway. Announcing that as if the
// cluster needed it, with nothing on the cluster screen to prepare the user,
// reads as the installer contradicting itself.
func TestProvisionSaysWhenTheBetaAPIsAreNotNeeded(t *testing.T) {
	app := testApp(t)
	press := pressToCluster(t, app)
	press("4", "enter") // substrate-ga: 1.37 throughout, no beta APIs
	if view := flat(app.cur.View(120)); !strings.Contains(view, "needs no beta APIs, but bootstrap turns them on for every cluster") {
		t.Errorf("provision should say the update is not needed for this cluster:\n%s", view)
	}
}

// An unreadable control-plane version over pools that are all on 1.37 is not
// ready — the control plane might not serve v1 — but there is no pool to
// replace, and none is recorded. The confirmation must not promise that
// provision will list some.
func TestAnUnreadableControlPlaneOverGoodPoolsPromisesNoReplacement(t *testing.T) {
	s, drive, _ := drivenClusterScreen(t)
	drive(clustersMsg{owner: s, clusters: []gcp.Cluster{{
		Name: "odd", Location: "us-west1-c", Status: "RUNNING", MasterVersion: "???", NodeCount: 2,
		PoolVersions: []gcp.PoolVersion{{Name: "a", Version: "1.37.1-gke.1000000"}},
	}}})
	s.cursor = 0
	drive(key("enter"))
	if s.mode != "confirm" {
		t.Fatalf("mode = %q, want the confirmation", s.mode)
	}
	view := flat(s.View(120))
	if strings.Contains(view, "must be replaced") || !strings.Contains(view, "need nothing") {
		t.Errorf("the confirmation should say the pools are fine:\n%s", view)
	}
}

// The cluster version and release channel only matter when the run creates a
// cluster, so they are advanced-track fields — but they have to actually be
// reachable there, and to reach upstream with whatever the user typed.
func TestAdvancedTrackOffersTheClusterVersionAndChannel(t *testing.T) {
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
	typed := func(s string) {
		for _, r := range s {
			pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}

	press("2", "enter") // welcome: advanced track
	press("enter")      // doctor
	press("1", "enter") // images: pre-built, so the registry field is left out
	press("enter", "enter", "enter")

	view := app.View()
	if !strings.Contains(view, "Cluster version") {
		t.Fatalf("advanced project screen missing the cluster version field:\n%s", view)
	}
	st := app.deps.Setup
	if st.ClusterVersion != state.DefaultClusterVersion {
		t.Fatalf("default version=%q, want %q", st.ClusterVersion, state.DefaultClusterVersion)
	}

	if st.ReleaseChannel != state.DefaultReleaseChannel {
		t.Fatalf("default channel=%q, want %q", st.ReleaseChannel, state.DefaultReleaseChannel)
	}

	// fields: 0:ProjectID, 1:Zone, 2:Bucket, 3:MachineType, 4:Network,
	// 5:Subnetwork, 6:ClusterVersion, 7:ReleaseChannel
	press("enter", "enter", "enter", "enter", "enter", "enter")
	pump(t, app, tea.KeyMsg{Type: tea.KeyCtrlU})
	typed("1.36")
	press("enter") // to the channel field
	pump(t, app, tea.KeyMsg{Type: tea.KeyCtrlU})
	typed("regular")
	press("enter") // submit from the last field

	if app.mach.Current() != state.Cluster {
		t.Fatalf("after project: %v", app.mach.Current())
	}
	if st.ClusterVersion != "1.36" || st.ReleaseChannel != "regular" {
		t.Errorf("edited: version=%q channel=%q, want 1.36 on regular", st.ClusterVersion, st.ReleaseChannel)
	}
}

// The version field is the only thing standing between the advanced track and
// an unsupported cluster: the new-cluster path never passes the cluster
// screen's readiness check, so whatever is typed here is created and installed
// onto. A release below the floor has to stop here, and a cleared field has to
// mean the default rather than "no version", which would hand the choice to
// GKE's Regular channel and its below-the-floor default.
func TestClusterVersionFieldRefusesAnUnsupportedRelease(t *testing.T) {
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
	typed := func(s string) {
		for _, r := range s {
			pump(t, app, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}

	press("2", "enter") // welcome: advanced track
	press("enter")      // doctor
	press("1", "enter") // images: pre-built
	press("enter", "enter", "enter")
	press("enter", "enter", "enter", "enter", "enter", "enter") // to the version field

	pump(t, app, tea.KeyMsg{Type: tea.KeyCtrlU})
	typed("1.35")
	press("enter", "enter") // past the channel field, then submit
	if app.mach.Current() != state.Project {
		t.Fatalf("1.35 was accepted: step=%v, want to stay on the project screen", app.mach.Current())
	}
	if view := app.View(); !strings.Contains(view, "below "+gcp.MinSupportedRelease.String()) {
		t.Errorf("the refusal should name the floor:\n%s", view)
	}

	pump(t, app, tea.KeyMsg{Type: tea.KeyCtrlU})
	press("enter", "enter")
	if app.mach.Current() != state.Cluster {
		t.Fatalf("an empty version was refused: step=%v, want Cluster", app.mach.Current())
	}
	if got := app.deps.Setup.ClusterVersion; got != state.DefaultClusterVersion {
		t.Errorf("an empty version became %q, want the default %q", got, state.DefaultClusterVersion)
	}
}

// The advanced track lets the user pick the release a new cluster comes up at,
// so the cluster screen's advice must quote that choice. Someone who asked for
// 1.38 and is told new clusters come at 1.36 has reason to doubt the rest.
func TestConfirmationQuotesTheChosenClusterVersion(t *testing.T) {
	app := testApp(t)
	app.deps.Setup.ClusterVersion = "1.38"
	press := pressToCluster(t, app)

	press("3", "enter") // ml-staging: 1.36, no beta APIs
	view := flat(app.View())
	if !strings.Contains(view, "made at 1.38 with the APIs on") {
		t.Errorf("the confirmation should quote the chosen version 1.38:\n%s", view)
	}
	if strings.Contains(view, "made at "+state.DefaultClusterVersion) {
		t.Errorf("the confirmation quotes the default rather than the chosen version:\n%s", view)
	}
}

// After 'y' on a cluster without the beta APIs, provision is about to run a
// control-plane update of about ten minutes — the confirmation just said so.
// A provision screen that then promises to "only fill in the bucket" and a
// checklist that says it is creating the cluster leave the user thinking the
// install has hung. Where the pools predate the APIs, provision is also where
// the user learns which to replace: after it, Substrate's own control plane
// is scheduled, and it mounts pod certificates.
func TestProvisionSaysWhenItTurnsOnTheBetaAPIs(t *testing.T) {
	app := testApp(t)
	press := pressToCluster(t, app)
	press("4", "enter") // substrate-ga: ready through v1, but bootstrap still enables the beta APIs
	if app.mach.Current() != state.Provision || !app.deps.Setup.EnableBetaAPIs {
		t.Fatalf("step=%v EnableBetaAPIs=%v, want Provision/true", app.mach.Current(), app.deps.Setup.EnableBetaAPIs)
	}
	// substrate-ga needs no beta APIs, so its header is the "done anyway"
	// one (TestProvisionSaysWhenTheBetaAPIsAreNotNeeded); the checklist
	// still names the update, because bootstrap still makes it.
	view := flat(app.cur.View(120))
	if !strings.Contains(view, "Turn on the beta PodCertificate APIs (control-plane update)") {
		t.Errorf("provision's checklist should name the beta-API update:\n%s", view)
	}
	if strings.Contains(view, "only fills in the bucket") || strings.Contains(view, "pools before Substrate starts") {
		t.Errorf("provision on 1.37 pools: wrong header or a replacement nobody needs:\n%s", view)
	}

	// A cluster that does need them gets the header that says so. Rendered
	// directly: driven through provision, a cluster with old pools ends on
	// the replacement block, which takes the header's place.
	needs := &Deps{Setup: state.NewSetup(), Runner: execx.DryRun{Delay: time.Millisecond},
		GCP: &gcp.Client{DryRun: true}, Builder: snapshot.NewBuilder(t.TempDir(), false)}
	needs.Setup.ClusterName, needs.Setup.EnableBetaAPIs = "ml-staging", true
	if view := flat(newProvisionScreen(needs).View(120)); !strings.Contains(view, "Turning on the beta PodCertificate APIs for ml-staging") {
		t.Errorf("provision should say it is turning the APIs on for a cluster that needs them:\n%s", view)
	}

	old := testApp(t)
	press = pressToCluster(t, old)
	press("3", "enter", "y") // ml-staging: 1.36 pools
	if got := strings.Join(old.deps.Setup.ReplacePools.Names, ","); got != "default-pool,gpu-pool" {
		t.Errorf("pools to replace = %q, want default-pool,gpu-pool", got)
	}
	if view := flat(old.View()); !strings.Contains(view, "Replace these pools before Substrate starts") ||
		!strings.Contains(view, "pools replaced, turn on Substrate") {
		t.Errorf("provision on 1.36 pools should end on the replacement, not 'turn on Substrate':\n%s", view)
	}

	ready := testApp(t)
	press = pressToCluster(t, ready)
	press("1", "enter") // substrate-poc: ready
	if ready.mach.Current() != state.Provision || ready.deps.Setup.EnableBetaAPIs {
		t.Errorf("a ready cluster: step=%v EnableBetaAPIs=%v, want Provision/false",
			ready.mach.Current(), ready.deps.Setup.EnableBetaAPIs)
	}
}

// Every mode but the list and the name prompt reads one cluster through the
// cursor, and a reload resets the cursor. With two reloads in flight, the
// second can land while a confirmation is open; it must keep the selection if
// the cluster is still listed, and drop back to the list if it is not, rather
// than index past the end of the new list and crash the wizard.
func TestAReloadDuringTheConfirmationKeepsTheSelection(t *testing.T) {
	deps := &Deps{
		Setup:   state.NewSetup(),
		Runner:  execx.DryRun{Delay: time.Millisecond},
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
	s.cursor = 2 // ml-staging: 1.36, no beta APIs
	drive(key("enter"))
	if s.mode != "confirm" {
		t.Fatalf("mode = %q, want confirm", s.mode)
	}

	drive(clustersMsg{owner: s, clusters: clusters})
	if s.mode != "confirm" || s.clusters[s.cursor].Name != "ml-staging" {
		t.Errorf("reload with the cluster still listed: mode=%q cursor=%d, want confirm on ml-staging", s.mode, s.cursor)
	}
	_ = s.View(120)

	drive(clustersMsg{owner: s, clusters: clusters[:1]})
	if s.mode != "list" {
		t.Errorf("reload without the cluster: mode=%q, want list", s.mode)
	}
	_ = s.View(120)
	_ = s.Hints()
}

// drivenClusterScreen is a cluster screen on the dry-run fixtures, driven
// directly so a test can deliver a clustersMsg at any moment it likes.
func drivenClusterScreen(t *testing.T) (*clusterScreen, func(tea.Msg), []gcp.Cluster) {
	t.Helper()
	deps := &Deps{
		Setup:   state.NewSetup(),
		Runner:  execx.DryRun{Delay: time.Millisecond},
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
	return s, drive, clusters
}

// A reload that lands mid-confirmation after the APIs were turned on out of
// band must not leave the panel saying they are missing: 'y' through it
// would record nothing to enable, and provision would then contradict the
// screen the user just read.
func TestAReloadAfterTheAPIsWereTurnedOnLeavesTheConfirmation(t *testing.T) {
	s, drive, clusters := drivenClusterScreen(t)
	s.cursor = 2 // ml-staging
	drive(key("enter"))
	if s.mode != "confirm" {
		t.Fatalf("mode = %q, want confirm", s.mode)
	}
	fixed := slices.Clone(clusters)
	fixed[2].BetaAPIs = gcp.RequiredBetaAPIs
	drive(clustersMsg{owner: s, clusters: fixed})
	if s.mode != "list" || s.clusters[s.cursor].Name != "ml-staging" {
		t.Errorf("mode=%q cursor=%d, want the list with ml-staging still selected", s.mode, s.cursor)
	}
}

// A reload is no reason to disturb work in progress on the selected cluster.
// A failed one would otherwise empty the list under an open confirmation, and
// one that lands during a teardown would swap the list the teardown's
// completion reads its cluster from — or, as an earlier version did, stop the
// ate-setup delete partway through.
func TestAReloadDoesNotDisturbWorkInProgress(t *testing.T) {
	s, drive, clusters := drivenClusterScreen(t)
	s.cursor = 2 // ml-staging
	drive(key("enter"))
	drive(clustersMsg{owner: s, err: errors.New("transient gcloud failure")})
	if s.mode != "confirm" || s.err != nil || len(s.clusters) != len(clusters) {
		t.Errorf("failed reload during confirm: mode=%q err=%v clusters=%d, want it ignored", s.mode, s.err, len(s.clusters))
	}

	s.mode = "teardown"
	drive(clustersMsg{owner: s, clusters: clusters[:1]})
	drive(clustersMsg{owner: s, err: errors.New("transient gcloud failure")})
	if s.mode != "teardown" || len(s.clusters) != len(clusters) || s.clusters[s.cursor].Name != "ml-staging" {
		t.Errorf("reload during teardown: mode=%q clusters=%d, want the teardown and its list untouched", s.mode, len(s.clusters))
	}
}

// A 1.37 control plane does not make 1.36 pools safe: their kubelets serve
// pod certificate projection only behind the beta gate. Such a cluster without
// the beta APIs has to stop at the confirmation and name the old pool; waving
// it through on the control plane's version alone hangs Substrate's pods on
// those nodes.
func TestA137ControlPlaneWithOldPoolsStillNeedsTheRepair(t *testing.T) {
	s, drive, _ := drivenClusterScreen(t)
	drive(clustersMsg{owner: s, clusters: []gcp.Cluster{{
		Name: "mixed", Location: "us-west1-c", Status: "RUNNING", MasterVersion: "1.37.1-gke.1000000", NodeCount: 3,
		PoolVersions: []gcp.PoolVersion{{Name: "old-pool", Version: "1.36.4-gke.1247000"}, {Name: "new-pool", Version: "1.37.1-gke.1000000"}},
	}}})
	s.cursor = 0
	drive(key("enter"))
	if s.mode != "confirm" {
		t.Fatalf("mode = %q, want the confirmation", s.mode)
	}
	if view := flat(s.View(120)); !strings.Contains(view, "1 node pool runs below") {
		t.Errorf("the confirmation should name one old pool:\n%s", view)
	}
	drive(key("y"))
	if got := strings.Join(s.deps.Setup.ReplacePools.Names, ","); got != "old-pool" {
		t.Errorf("pools to replace = %q, want old-pool", got)
	}
}

// A cluster below the floor that already serves both APIs is not ready, but
// bootstrap has nothing to enable on it. Recording an enablement would have
// provision announce a ten-minute update while setup-gcp logs that the APIs
// "match perfectly" and does nothing.
func TestABelowTheFloorClusterWithTheAPIsRecordsNothingToEnable(t *testing.T) {
	s, drive, _ := drivenClusterScreen(t)
	drive(clustersMsg{owner: s, clusters: []gcp.Cluster{{
		Name: "old-with-apis", Location: "us-west1-c", Status: "RUNNING",
		MasterVersion: "1.35.5-gke.1163012", NodeCount: 1, BetaAPIs: gcp.RequiredBetaAPIs,
	}}})
	s.cursor = 0
	drive(key("enter"))
	if s.mode != "confirm" {
		t.Fatalf("mode = %q, want the below-the-floor confirmation", s.mode)
	}
	drive(key("y"))
	// Nothing to enable. The pools are still worth checking, but only as
	// advice: the APIs were on already, so whether its nodes predate them is
	// not something the listing can say.
	if st := s.deps.Setup; st.ClusterName != "old-with-apis" || st.EnableBetaAPIs || (st.ReplacePools.Needed && !st.ReplacePools.Unsure) {
		t.Errorf("chose %q with EnableBetaAPIs=%v pools=%+v, want nothing to enable and any pool advice unsure",
			st.ClusterName, st.EnableBetaAPIs, st.ReplacePools)
	}
}

// The version and channel have to go together, or GKE refuses the create:
// a channel only offers the versions it carries, and today 1.37 is in Rapid
// alone. The installer asks GKE which channel carries what (canned under
// --dry-run) and says so on the project screen, pointing at a channel that
// works. It is a warning, not a wall: submitting the same pair again goes on,
// because the pair only matters if the next step creates a cluster, and GKE
// rolls versions out per location. When no channel offers the version, the
// cursor goes to the version, the field to change. Extended can never work,
// since it refuses the beta APIs setup-gcp turns on, so it is refused outright.
func TestTheChannelHasToOfferTheVersion(t *testing.T) {
	// The project screen alone, not a walk through the wizard to reach it:
	// the walk cost ~35 s of the ui package's 10-minute budget.
	deps := testApp(t).deps
	deps.Setup.Track = state.TrackAdvanced
	deps.Setup.ImageRepo, deps.Setup.ImageTag = snapshot.ReleaseRepo, snapshot.ReleaseVersion
	deps.Setup.ProjectID = "acme"
	scr := newProjectScreen(deps)
	set := func(label, value string) {
		_, at := scr.fieldValue(label)
		scr.fields[at].input.SetValue(value)
	}
	// submit runs one submit through to its result and reports whether the
	// screen moved on.
	submit := func() bool {
		advanced := false
		for _, m := range runCmd(scr.submit()) {
			for _, n := range runCmd(scr.Update(m)) {
				if n == navNext {
					advanced = true
				}
			}
		}
		return advanced
	}

	set(clusterVersionLabel, "1.99")
	if submit() || !strings.Contains(scr.errText, "no channel that offers version 1.99") {
		t.Fatalf("1.99 should be flagged, got %q", scr.errText)
	}
	if _, at := scr.fieldValue(clusterVersionLabel); scr.focus != at {
		t.Errorf("no channel offers the version, so the cursor belongs on the version field; focus=%d want %d", scr.focus, at)
	}

	set(clusterVersionLabel, "1.37")
	set(releaseChannelLabel, "extended")
	if submit() || !strings.Contains(scr.errText, "extended channel does not allow the beta APIs") {
		t.Errorf("extended should be refused with its reason, got %q", scr.errText)
	}

	set(releaseChannelLabel, "regular")
	if submit() {
		t.Fatal("1.37 on regular went through without a word")
	}
	if !strings.Contains(scr.errText, "regular channel does not offer version 1.37") || !strings.Contains(scr.errText, "rapid does") {
		t.Errorf("the warning should say regular lacks 1.37 and rapid has it, got %q", scr.errText)
	}

	// An edit anywhere re-arms the warning: the check is per location, so
	// acknowledging it for one zone must not wave a different zone through.
	// A real keystroke, since that is where the reset lives.
	scr.setFocus(1) // the zone
	scr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	set("Cluster location (zone)", "us-central1-a")
	if submit() || !strings.Contains(scr.errText, "regular channel does not offer version 1.37 in us-central1-a") {
		t.Fatalf("after editing the zone the warning should come back, got %q", scr.errText)
	}

	if !submit() || deps.Setup.ReleaseChannel != "regular" {
		t.Errorf("a second submit of the same pair should go on: channel=%q err=%q", deps.Setup.ReleaseChannel, scr.errText)
	}
}

// The channel lookup is advisory and silent when GKE cannot be asked, except
// when it is what ran out the submit's deadline: then it has to say "timed
// out" like every other check (#59), or the submit advances after 30 seconds
// with no word. Esc cancels rather than times out, and that stays silent.
func TestAChannelLookupThatTimesOutSaysSo(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if problem, _ := channelProblem(expired, &gcp.Client{}, "acme", "us-central1-a", "1.37", "rapid"); !strings.Contains(problem, "timed out") {
		t.Errorf("a lookup cut off by the deadline should say so, got %q", problem)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if problem, _ := channelProblem(cancelled, &gcp.Client{}, "acme", "us-central1-a", "1.37", "rapid"); problem != "" {
		t.Errorf("a cancelled lookup should stay silent, got %q", problem)
	}
}

// The cluster list is read for its badges, and a row wider than the list is
// cut at the edge, badge first — which at 80 columns, beside the sidebar,
// was every row. So at every width a row has to fit and keep its badge, the
// rows have to share one layout so their columns line up, and names stay
// whole until nothing else is left to drop: clusters often share a prefix,
// and truncated they look alike.
func TestClusterRowsFitAndKeepTheirBadges(t *testing.T) {
	clusters, err := (&gcp.Client{DryRun: true}).ListClusters(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	badges := make([]string, len(clusters))
	shorts := make([]string, len(clusters))
	for i := range clusters {
		badges[i], shorts[i] = "✓ substrate-ready · substrate installed", "✓ ready · installed"
	}
	for _, width := range []int{30, 45, 65, 85, 120, 200} {
		rows := clusterRows(clusters, badges, shorts, width)
		if len(rows) != len(clusters) {
			t.Fatalf("width %d: %d rows for %d clusters", width, len(rows), len(clusters))
		}
		badgeAt := -1
		for i, row := range rows {
			if lipgloss.Width(row) > width && width >= 45 {
				t.Errorf("width %d: row %d is %d wide:\n%s", width, i, lipgloss.Width(row), row)
			}
			if !strings.HasSuffix(row, badges[i]) && !strings.HasSuffix(row, shorts[i]) {
				t.Errorf("width %d: row %d lost its badge: %q", width, i, row)
			}
			at := strings.Index(row, "✓")
			if badgeAt >= 0 && at != badgeAt {
				t.Errorf("width %d: badges do not line up (%d vs %d):\n%s", width, at, badgeAt, strings.Join(rows, "\n"))
			}
			badgeAt = at
			if width >= 45 && !strings.Contains(row, clusters[i].Name) {
				t.Errorf("width %d: %q was truncated while columns could have gone: %q", width, clusters[i].Name, row)
			}
		}
		if width >= 200 && !strings.Contains(rows[0], clusters[0].MasterVersion) {
			t.Errorf("width %d: the full layout should be used when it fits: %q", width, rows[0])
		}
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
			if got := SandboxSummary(st); !strings.Contains(got, tc.want) {
				t.Errorf("SandboxSummary() = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// TestDemoSummary checks the summary text for gVisor and micro-VM demo deploys.
func TestDemoSummary(t *testing.T) {
	if got := DemoSummary(&state.Setup{}); got != "skipped" {
		t.Errorf("no demo: got %q, want %q", got, "skipped")
	}
	if got := DemoSummary(&state.Setup{DemoDeployed: true}); got != "counter demo deployed" {
		t.Errorf("gvisor demo: got %q", got)
	}
	if got := DemoSummary(&state.Setup{DemoDeployed: true, SandboxClass: state.SandboxMicroVM, MicroVMDeployed: false}); got != "counter demo deployed" {
		t.Errorf("unstaged microvm demo should report counter demo deployed, got %q", got)
	}
	got := DemoSummary(&state.Setup{DemoDeployed: true, SandboxClass: state.SandboxMicroVM, MicroVMDeployed: true})
	if got != "counter-microvm demo deployed" {
		t.Errorf("microvm install: got %q, want %q", got, "counter-microvm demo deployed")
	}
}

// TestMicroVMSandboxStepStagesAssetsAndDeploysMicroVMDemo verifies that the
// sandbox step (Choose your sandbox runtime) reflects the selected cluster's
// KVM status, stages the micro-VM assets when option 2 is picked, and that
// the demo step after it deploys the counter-microvm demo.
func TestMicroVMSandboxStepStagesAssetsAndDeploysMicroVMDemo(t *testing.T) {
	app := testApp(t)
	press := pressToCluster(t, app)

	// Pick row 1 (substrate-poc, which is KVMReady).
	press("1", "enter")
	if !app.deps.Setup.ClusterKVMReady {
		t.Fatal("ClusterKVMReady must be true for substrate-poc")
	}
	press("enter", "enter") // provision -> control plane -> filestore CSI
	press("s", "s")         // skip filestore CSI and autoscaling -> sandbox
	if app.mach.Current() != state.Sandbox {
		t.Fatalf("expected Sandbox step right after Autoscaling, got %v", app.mach.Current())
	}
	if view := app.View(); !strings.Contains(view, "has a KVM-capable node pool") {
		t.Errorf("sandbox view should confirm substrate-poc has KVM:\n%s", view)
	}
	press("2", "enter") // choose Micro-VM and stage assets
	press("enter")      // continue from Sandbox -> Demo
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

	t.Run("/skip while staging is running is refused", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = true
		jumpToSandbox(app)

		scr := app.cur.(*sandboxScreen)
		app.deps.Setup.SandboxClass = state.SandboxMicroVM
		scr.comp = &execComp{started: true, finished: false}

		for _, ch := range "/skip" {
			pump(t, app, key(string(ch)))
		}
		pump(t, app, key("enter"))
		if app.mach.Current() != state.Sandbox {
			t.Fatalf("expected /skip to be refused while staging is running, got step %v", app.mach.Current())
		}
		if app.deps.Setup.SandboxClass != state.SandboxMicroVM {
			t.Errorf("SandboxClass = %q, want %q", app.deps.Setup.SandboxClass, state.SandboxMicroVM)
		}
	})

	t.Run("choosing gVisor after staging preserves MicroVMDeployed", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = true
		app.deps.Setup.SandboxClass = state.SandboxMicroVM
		app.deps.Setup.MicroVMDeployed = true
		jumpToSandbox(app)

		pump(t, app, key("1"))
		pump(t, app, key("enter"))
		if app.mach.Current() != state.Demo {
			t.Fatalf("expected Demo step after choosing gVisor, got %v", app.mach.Current())
		}
		if app.deps.Setup.SandboxClass != state.SandboxGVisor {
			t.Errorf("SandboxClass = %q, want %q", app.deps.Setup.SandboxClass, state.SandboxGVisor)
		}
		if !app.deps.Setup.MicroVMDeployed {
			t.Error("MicroVMDeployed should stay true when switching back to gVisor after staging")
		}
	})

	t.Run("KVM probe error surfaces in view", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = false
		jumpToSandbox(app)

		pump(t, app, clusterKVMMsg{cluster: app.deps.Setup.ClusterName, err: fmt.Errorf("context deadline exceeded")})
		view := app.View()
		if !strings.Contains(view, "could not check whether cluster") || !strings.Contains(view, "context deadline exceeded") {
			t.Errorf("expected KVM probe error in view, got:\n%s", view)
		}
	})

	// pendingSandbox puts the app on a Sandbox screen whose Init started a
	// real probe. The probe command itself is never run, so its answer is
	// whatever the test pumps.
	pendingSandbox := func(t *testing.T, app *App) *sandboxScreen {
		t.Helper()
		app.deps.GCP = &gcp.Client{}
		jumpToSandbox(app)
		scr := app.cur.(*sandboxScreen)
		if scr.Init() == nil {
			t.Fatal("a live GCP client should start the KVM probe")
		}
		if !scr.kvmPending {
			t.Fatal("Init started the probe but did not mark it pending")
		}
		return scr
	}
	answer := func(app *App, ready bool, err error) tea.Msg {
		return clusterKVMMsg{cluster: app.deps.Setup.ClusterName, ready: ready, err: err}
	}
	hasHint := func(scr *sandboxScreen, k string) bool {
		for _, h := range scr.Hints() {
			if h.Key == k {
				return true
			}
		}
		return false
	}

	t.Run("dry run starts no probe and is not pending", func(t *testing.T) {
		app := testApp(t)
		scr := newSandboxScreen(app.deps)
		if scr.Init() != nil || scr.kvmPending {
			t.Fatalf("dry-run GCP client should not probe: pending=%v", scr.kvmPending)
		}
	})

	t.Run("pending probe shows checking and holds Micro-VM until it answers", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = false
		scr := pendingSandbox(t, app)

		view := app.View()
		if !strings.Contains(view, "Checking whether cluster") || strings.Contains(view, "no KVM-capable node pool yet") {
			t.Errorf("expected checking note while the probe is pending, got:\n%s", view)
		}

		// enter on Micro-VM neither stages nor claims the cluster has no KVM.
		pump(t, app, key("2"))
		pump(t, app, key("enter"))
		if scr.confirmingNoKVM || scr.comp != nil || app.mach.Current() != state.Sandbox {
			t.Fatalf("enter while checking should wait: confirming=%v staging=%v step=%v",
				scr.confirmingNoKVM, scr.comp != nil, app.mach.Current())
		}
		if hasHint(scr, "enter") {
			t.Errorf("hints offer enter while Micro-VM cannot be confirmed: %v", scr.Hints())
		}
		if view := app.View(); !strings.Contains(view, "Wait for the check to finish before selecting Micro-VM") {
			t.Errorf("expected the view to say why enter waits, got:\n%s", view)
		}

		pump(t, app, answer(app, false, nil))
		if view := app.View(); !strings.Contains(view, "no KVM-capable node pool yet") {
			t.Errorf("expected no-KVM warning once the probe answers, got:\n%s", view)
		}
		pump(t, app, key("enter"))
		if !scr.confirmingNoKVM {
			t.Fatal("expected the no-KVM confirmation once the probe says not ready")
		}
	})

	t.Run("probe answering ready lets enter stage", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = false
		pendingSandbox(t, app)

		pump(t, app, key("2"))
		pump(t, app, key("enter")) // waits
		pump(t, app, answer(app, true, nil))
		pump(t, app, key("enter"))
		if !app.deps.Setup.MicroVMActive() {
			t.Fatalf("expected enter to stage Micro-VM on a KVM-ready cluster, got %+v", app.deps.Setup)
		}
	})

	t.Run("probe error falls back to the confirmation", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = false
		scr := pendingSandbox(t, app)

		pump(t, app, key("2"))
		pump(t, app, answer(app, false, fmt.Errorf("context deadline exceeded")))
		pump(t, app, key("enter"))
		if !scr.confirmingNoKVM {
			t.Fatal("an unanswerable probe should still ask before staging")
		}
		// Nothing was verified, so the prompt must not claim workers will
		// stay Pending.
		view := app.View()
		if !strings.Contains(view, "Could not verify a KVM-capable node pool") || !strings.Contains(view, "Press [y] to stage anyway") {
			t.Errorf("expected a conditional stage-anyway prompt after a failed probe, got:\n%s", view)
		}
		if strings.Contains(view, "workers will stay Pending on this cluster") {
			t.Errorf("prompt asserts an outcome the failed probe could not check:\n%s", view)
		}
	})

	t.Run("cluster already known KVM-ready does not wait for the probe", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = true
		pendingSandbox(t, app)

		pump(t, app, key("2"))
		pump(t, app, key("enter"))
		if !app.deps.Setup.MicroVMActive() {
			t.Fatalf("expected staging without waiting on a known KVM-ready cluster, got %+v", app.deps.Setup)
		}
	})

	t.Run("probe answer for another cluster is ignored", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = false
		scr := pendingSandbox(t, app)

		pump(t, app, clusterKVMMsg{cluster: "some-other-cluster", ready: true})
		if !scr.kvmPending || app.deps.Setup.ClusterKVMReady {
			t.Fatalf("stale answer applied: pending=%v ready=%v", scr.kvmPending, app.deps.Setup.ClusterKVMReady)
		}
	})

	t.Run("KVM-ready answer clears an outstanding confirmation", func(t *testing.T) {
		// A revisit can leave a second probe in flight after the first
		// answered not-ready and the user was asked to confirm.
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = false
		jumpToSandbox(app)
		scr := app.cur.(*sandboxScreen)

		pump(t, app, key("2"))
		pump(t, app, key("enter"))
		if !scr.confirmingNoKVM {
			t.Fatal("expected the no-KVM confirmation")
		}
		pump(t, app, answer(app, true, nil))
		if scr.confirmingNoKVM || hasHint(scr, "y") {
			t.Fatalf("prompt not cleared after KVM-ready answer: confirming=%v hints=%v", scr.confirmingNoKVM, scr.Hints())
		}
		pump(t, app, key("enter"))
		if !app.deps.Setup.MicroVMActive() {
			t.Fatalf("expected enter to stage Micro-VM on a KVM-ready cluster, got %+v", app.deps.Setup)
		}
	})

	t.Run("missing host tools disables option 2 with reason", func(t *testing.T) {
		app := testApp(t)
		app.deps.Setup.ClusterKVMReady = true
		jumpToSandbox(app)

		scr := app.cur.(*sandboxScreen)
		scr.missingTools = []string{"sha256sum", "zstd"}

		view := app.View()
		if !strings.Contains(view, "unavailable on this host") || !strings.Contains(view, "missing sha256sum, zstd on PATH") {
			t.Errorf("expected missing tools note in view, got:\n%s", view)
		}

		// Pressing 2 then enter cannot select Micro-VM; it keeps gVisor and moves to Demo.
		pump(t, app, key("2"))
		pump(t, app, key("enter"))
		if app.mach.Current() != state.Demo {
			t.Fatalf("expected Demo step, got %v", app.mach.Current())
		}
		if app.deps.Setup.SandboxClass != state.SandboxGVisor {
			t.Errorf("SandboxClass = %q, want %q", app.deps.Setup.SandboxClass, state.SandboxGVisor)
		}
	})
}

// A step whose Display is a summary rather than a pasteable command must not
// wear a shell prompt; every other step keeps it.
func TestExecCompPromptOnlyForCommands(t *testing.T) {
	cmd := newExecComp(nil, execx.Spec{Display: "go run ./cmd/ate-setup deploy demo counter"}, nil, "")
	if got := cmd.view(80); !strings.Contains(got, "$ go run ./cmd/ate-setup deploy demo counter") {
		t.Errorf("a command step lost its prompt:\n%s", got)
	}
	summary := newExecComp(nil, execx.Spec{Display: "deploy demo counter-microvm", DisplayIsSummary: true}, nil, "")
	got := summary.view(80)
	if strings.Contains(got, "$ ") {
		t.Errorf("a summary step is shown as a command:\n%s", got)
	}
	if !strings.Contains(got, "deploy demo counter-microvm") {
		t.Errorf("a summary step lost its title:\n%s", got)
	}
}

func TestAutomaticArtifactRegistryWaitsForClusterRegion(t *testing.T) {
	t.Setenv("ATE_ATENET_DATAPLANE", "")
	app := testApp(t)
	app.deps.DryRun = false
	app.deps.Builder = snapshot.NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	app.deps.Builder.UseSource(snapshot.Revision{Repo: snapshot.RepoURL, Commit: strings.Repeat("a", 40), EnvoyDataplane: true})
	st := app.deps.Setup
	st.ProjectID, st.Zone = "acme", "us-west1-c"
	scr := newProjectScreen(app.deps)
	if got := scr.dockerRegistry(st); got != "" {
		t.Fatalf("project checked a region-dependent default: %q", got)
	}
	st.Zone = "europe-west4-a"
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho 29.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	config := t.TempDir()
	t.Setenv("DOCKER_CONFIG", config)
	provision := newProvisionScreen(app.deps)
	cmd := provision.Init()
	if !provision.checkingDocker || provision.comp.started {
		t.Fatal("bootstrap started before credential check")
	}
	if next := provision.Update(cmd()); next != nil || provision.comp.started {
		t.Fatal("missing credentials allowed bootstrap")
	}
	if !strings.Contains(provision.dockerError, "gcloud auth configure-docker europe-west4-docker.pkg.dev") || !strings.Contains(provision.dockerError, "press [r]") {
		t.Fatal(provision.dockerError)
	}
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"auths":{"europe-west4-docker.pkg.dev":{"auth":"dXNlcjpwYXNz"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	retry := provision.Update(key("r"))
	if retry == nil {
		t.Fatal("retry did not check credentials")
	}
	provision.Update(retry())
	defer provision.comp.stop()
	if !provision.comp.started || provision.dockerError != "" {
		t.Fatal("successful retry did not start bootstrap")
	}
	if !slices.Contains(provision.comp.spec.Env, "KO_DOCKER_REPO=europe-west4-docker.pkg.dev/acme/ate-images") {
		t.Fatal(provision.comp.spec.Env)
	}
	st.ImageRepo, st.ImageTag = snapshot.ReleaseRepo, snapshot.ReleaseVersion
	prebuilt := newProvisionScreen(app.deps)
	prebuilt.Init()
	defer prebuilt.comp.stop()
	if prebuilt.checkingDocker || !prebuilt.comp.started {
		t.Fatal("prebuilt install checked Docker credentials")
	}
}

func TestProjectValidatesRepositoryBeforeCloudChecks(t *testing.T) {
	app := testApp(t)
	app.deps.Builder = snapshot.NewBuilder(t.TempDir(), true)

	app.deps.Setup.Track, app.deps.Setup.ProjectID = state.TrackAdvanced, "acme"
	scr := newProjectScreen(app.deps)
	for _, f := range scr.fields {
		if strings.HasPrefix(f.label, "Image registry") && f.input.Placeholder != "<region>-docker.pkg.dev/<project>/ate-images" {
			t.Errorf("registry placeholder = %q", f.input.Placeholder)
		}
	}
	scr.fields[scr.repositoryField].label = "Repository"
	scr.fields[scr.repositoryField].input.SetValue("My_Images")
	scr.submit()
	if scr.validating || scr.errText == "" || scr.focus != scr.repositoryField {
		t.Fatalf("invalid name: validating=%t, error=%q, focus=%d", scr.validating, scr.errText, scr.focus)
	}
	scr.fields[scr.repositoryField].input.SetValue("ate-images")
	scr.submit()
	if !scr.validating || scr.errText != "" {
		t.Fatalf("valid name: validating=%t, error=%q", scr.validating, scr.errText)
	}
}

func TestProvisionCanGoBackDuringDockerCheck(t *testing.T) {
	for _, press := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("b")}, {Type: tea.KeyEsc}} {
		t.Run(press.String(), func(t *testing.T) {
			app := testApp(t)
			scr := newProvisionScreen(app.deps)
			scr.checkingDocker = true
			if len(scr.Hints()) == 0 {
				t.Error("no hint for returning during the check")
			}
			cmd := scr.Update(press)
			if cmd == nil || cmd() != navBack {
				t.Fatal("cannot go back during Docker check")
			}
			next := newProvisionScreen(app.deps)
			next.checkingDocker = true
			if cmd := next.Update(provisionDockerMsg{owner: scr}); cmd != nil || next.comp.started || !next.checkingDocker {
				t.Fatal("old check result changed the new screen")
			}
		})
	}
}
