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

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/ui"
)

// With mainArgsEnv set, the test binary runs the installer's main instead
// of the tests, so a test can start the real program with its output piped
// and see what a user piping it through tee would see.
const mainArgsEnv = "SUBSTRATE_GKE_INSTALLER_TEST_MAIN_ARGS"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(mainArgsEnv); ok {
		os.Args = append(os.Args[:1], strings.Fields(args)...)
		main()
		return
	}
	os.Exit(m.Run())
}

// Piped or redirected, Bubble Tea never gets a window size and the wizard
// repaints "loading…" forever. It must fail at once and say why, and point
// at the log file as the record a tee capture was meant to be.
func TestWizardRefusesToRunWithoutATerminal(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), mainArgsEnv+"=--dry-run")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("piped wizard: err %v, want exit status 2\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("piped wizard wrote to stdout, which should carry nothing but the wizard:\n%s", stdout.String())
	}
	logDir, err := execx.DefaultLogDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"interactive terminal program", "not a terminal", logDir, "issues/107"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal is missing %q:\n%s", want, stderr.String())
		}
	}
}

// The refusal names the stream that is wrong, so a user who piped only
// stdout is not told to fix stdin.
func TestNeedsTerminalNamesTheStream(t *testing.T) {
	pipe, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	defer w.Close()
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no controlling terminal:", err)
	}
	defer tty.Close()

	if got := needsTerminal(tty, tty); got != "" {
		t.Errorf("needsTerminal(tty, tty) = %q, want no refusal", got)
	}
	if got := needsTerminal(tty, w); !strings.Contains(got, "its output is not a terminal") {
		t.Errorf("piped stdout: %q", got)
	}
	if got := needsTerminal(pipe, tty); !strings.Contains(got, "its input is not a terminal") {
		t.Errorf("piped stdin: %q", got)
	}
	if got := needsTerminal(pipe, w); !strings.Contains(got, "its input and output are not a terminal") {
		t.Errorf("both piped: %q", got)
	}
}

// After a managed install the tree is gone, so the summary must not tell the
// user to cd into it.
func TestSummaryDoesNotPointAtTheRemovedCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "substrate-4a2cb262dd62")
	st := state.NewSetup()

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: st, Builder: snapshot.NewBuilder(root, true)}, true)
	})

	if strings.Contains(out, root) {
		t.Errorf("summary points at a directory cleanup removed:\n%s", out)
	}
	if !strings.Contains(out, snapshot.NewBuilder(root, true).TeardownCommand(st, "")) {
		t.Errorf("summary does not offer a self-contained teardown:\n%s", out)
	}
}

// A pre-built install builds nothing, so its summary must not say the tree
// was fetched to build images; a source build's must.
func TestSummarySaysWhyTheTreeWasFetched(t *testing.T) {
	root := filepath.Join(t.TempDir(), "substrate-4a2cb262dd62")
	for _, tc := range []struct{ name, imageRepo, want, wantNot string }{
		{"source build", "", "fetched to build your images", "deploy manifests"},
		{"pre-built images", "ghcr.io/agent-substrate/substrate", "fetched for its deploy manifests", "to build your images"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.NewSetup()
			st.ImageRepo = tc.imageRepo
			out := captureStdout(t, func() {
				printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: st, Builder: snapshot.NewBuilder(root, true)}, true)
			})
			if !strings.Contains(out, tc.want) || strings.Contains(out, tc.wantNot) {
				t.Errorf("summary should say %q and not %q:\n%s", tc.want, tc.wantNot, out)
			}
		})
	}
}

// Under --dry-run cleanup never runs, and it can also fail; either way a
// tree (or an older cache) may still be on disk, and the summary must not
// claim it was removed.
func TestSummaryDoesNotClaimARemovalThatDidNotHappen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "substrate-4a2cb262dd62")

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: state.NewSetup(), Builder: snapshot.NewBuilder(root, true)}, false)
	})

	if strings.Contains(out, "has been removed") {
		t.Errorf("summary claims a removal that did not happen:\n%s", out)
	}
	if !strings.Contains(out, root) {
		t.Errorf("summary should say where the still-cached tree lives:\n%s", out)
	}
}

// The teardown line is written to be pasted into a shell, so a checkout path
// with a space in it — "/Users/John Smith/substrate" is an ordinary macOS
// home — must not split the `cd` apart.
func TestSummaryTeardownCommandSurvivesAPaste(t *testing.T) {
	root := filepath.Join(t.TempDir(), "John Smith", "My Projects", "substrate")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: state.NewSetup(), Builder: snapshot.NewBuilder(root, false)}, false)
	})

	var teardown string
	for _, line := range strings.Split(out, "\n") {
		if cmd, ok := strings.CutPrefix(line, "  (cd "); ok {
			teardown = "(cd " + cmd
		}
	}
	if teardown == "" {
		t.Fatalf("summary printed no teardown command:\n%s", out)
	}

	// Run the paste for real, with the parts that would touch a cluster
	// swapped for an echo of the working directory.
	script := strings.Replace(teardown, "go run ./cmd/ate-setup delete ate-system", "pwd", 1)
	got, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("pasted teardown command failed: %v\n%s", err, script)
	}
	if strings.TrimSpace(string(got)) != root {
		t.Errorf("teardown command ran in %q, want %q\n%s", strings.TrimSpace(string(got)), root, script)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = saved
	w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Users get stuck with leftover charges unless the summary hands them a
// complete GCP cleanup, carrying their own answers, quoted for pasting. It
// offers two: the cluster-scoped form first — the dashboards and the
// transfer and node service accounts are shared by every Substrate cluster
// in the project — and the complete teardown second, worded as the
// alternative rather than as a second step ("to also remove…" read as one).
func TestSummaryOffersAFullGCPCleanup(t *testing.T) {
	st := state.NewSetup()
	st.ProjectID = "acme"
	st.BucketName = "ate-snapshots-acme-us-west1-c"

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: st, Builder: snapshot.NewBuilder(t.TempDir(), true)}, true)
	})

	var cleanups []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "./tools/cleanup-gcp") {
			cleanups = append(cleanups, line)
		}
	}
	if len(cleanups) != 2 {
		t.Fatalf("summary printed %d cleanup commands, want the cluster-scoped and the project-wide one:\n%s", len(cleanups), out)
	}
	for _, want := range []string{
		"--project " + snapshot.ShellQuote("acme"),
		"--cluster " + snapshot.ShellQuote(st.ClusterName),
		"--location " + snapshot.ShellQuote(st.Zone),
		"--bucket " + snapshot.ShellQuote(st.BucketName),
		"--delete-repository",
	} {
		for _, cleanup := range cleanups {
			if !strings.Contains(cleanup, want) {
				t.Errorf("summary cleanup line missing %q: %s", want, cleanup)
			}
		}
	}
	if !strings.Contains(cleanups[0], "--cluster-scoped") {
		t.Errorf("the first cleanup command is not cluster-scoped: %s", cleanups[0])
	}
	if strings.Contains(cleanups[1], "--cluster-scoped") {
		t.Errorf("the second cleanup command must be the project-wide one: %s", cleanups[1])
	}
	for _, want := range []string{
		"one of two ways:",
		"Option 1 — cluster-scoped (safe when other clusters share the project)",
		"image repository:",
		"Option 2 — complete teardown (only if no other Substrate cluster uses this project)",
		"the dashboards and the",
		"snapshot transfer and node service accounts:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary does not present the two cleanups as alternatives (%q):\n%s", want, out)
		}
	}
	if strings.Contains(out, "To also") {
		t.Errorf("the complete teardown reads as a step after the cluster-scoped one:\n%s", out)
	}
	// The two options' wording is shared with the reinstall guard's exit
	// note, so the summary prints the helper's lines as they are.
	clusterScoped, complete := snapshot.CleanupLeads(true, false)
	for _, line := range append(clusterScoped, complete...) {
		if !strings.Contains(out, line) {
			t.Errorf("summary lacks the shared line %q:\n%s", line, out)
		}
	}
}

// The wizard's "Next steps" panel is gone once the alt screen closes, so a
// demo install must leave the full walkthrough in the summary — and an
// install that skipped the demo must not tell users to poke a counter that
// does not exist.
func TestSummaryRecapsTheDemoNextSteps(t *testing.T) {
	builder := snapshot.NewBuilder(t.TempDir(), true)
	st := state.NewSetup()
	st.DemoDeployed = true
	// The lead points a shell at the cluster, which the summary prints for
	// every install; the demo steps follow it.
	lead, demo := builder.NextSteps(st)

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: st, Builder: builder}, true)
	})
	for _, want := range append([]string{lead}, demo...) {
		if !strings.Contains(out, want) {
			t.Errorf("demo summary is missing next step %q:\n%s", want, out)
		}
	}

	out = captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: state.NewSetup(), Builder: builder}, true)
	})
	if strings.Contains(out, "Next steps") {
		t.Errorf("summary recaps demo steps for an install that skipped the demo:\n%s", out)
	}

	microvm := state.NewSetup()
	microvm.SandboxClass = state.SandboxMicroVM
	microvm.MicroVMDeployed = true
	microvm.DemoDeployed = true
	out = captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: microvm, Builder: builder}, true)
	})
	for _, want := range []string{
		"sandbox    micro-VM  · assets staged, SandboxConfig applied",
		"demo       counter-microvm demo deployed — next steps recapped below",
		"Next steps — try the counter-microvm demo",
		"--template counter-microvm",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("micro-VM summary missing %q:\n%s", want, out)
		}
	}
}

// The summary tells users to paste the script's invocation, so the script has
// to exist, parse, and be executable.
func TestCleanupGcpScriptIsRunnable(t *testing.T) {
	const script = "../tools/cleanup-gcp"
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("the summary points at a script that does not exist: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable", script)
	}
	if err := exec.Command("bash", "-n", script).Run(); err != nil {
		t.Errorf("%s is not valid shell: %v", script, err)
	}
	// Argument validation must fire before anything touches gcloud.
	out, err := exec.Command("bash", script, "--project", "acme").CombinedOutput()
	if err == nil {
		t.Errorf("script accepted incomplete arguments:\n%s", out)
	}
	if !strings.Contains(string(out), "CLUSTER_NAME") {
		t.Errorf("script did not name the missing argument:\n%s", out)
	}
}

// runCleanupGcp runs tools/cleanup-gcp against fake gcloud and git binaries
// and returns its combined output, the calls log, and its exit error.
// gcloudBody is the fake gcloud's script after the line that logs the call;
// the fake git logs its call and fails. It runs from a directory holding a
// .ate-dev-env.sh that names another project, cluster, and bucket, so a test
// can check those values never reach a deletion.
func runCleanupGcp(t *testing.T, gcloudBody string) (out, calls string, err error) {
	t.Helper()
	script, err := filepath.Abs("../tools/cleanup-gcp")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	callsFile := filepath.Join(t.TempDir(), "calls")
	fakes := map[string]string{
		"gcloud": `echo "gcloud $*" >> "$CALLS"` + "\n" + gcloudBody,
		"git":    `echo "git $*" >> "$CALLS"; exit 1`,
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cwd := t.TempDir()
	devEnv := "export PROJECT_ID=wrong-project CLUSTER_NAME=wrong-cluster BUCKET_NAME=wrong-bucket\n"
	if err := os.WriteFile(filepath.Join(cwd, ".ate-dev-env.sh"), []byte(devEnv), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script, "--project", "acme", "--cluster", "substrate-test",
		"--location", "us-west1-c", "--bucket", "acme-snapshots", "--yes")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CALLS="+callsFile)
	raw, runErr := cmd.CombinedOutput()
	if log, err := os.ReadFile(callsFile); err == nil {
		calls = string(log)
	}
	return string(raw), calls, runErr
}

// projectNumberLookup answers cleanup-gcp's project-number lookup; every
// other fake gcloud call succeeds silently.
const projectNumberLookup = `if [ "$1 $2" = "projects describe" ]; then echo 42; exit 0; fi`

// cleanup-gcp delegates its deletions to this repository's
// tools/setup-gcp/teardown.sh, without fetching a Substrate tree. The
// deletions must target the flags' project and cluster, never the values in
// the working directory's .ate-dev-env.sh.
func TestCleanupGcpRunsTheLocalTeardown(t *testing.T) {
	if raw, err := os.ReadFile("../tools/cleanup-gcp"); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(raw), "remove-iam-policy-binding") {
		t.Errorf("cleanup-gcp carries its own IAM deletions; they belong in tools/setup-gcp/teardown.sh")
	}

	out, log, err := runCleanupGcp(t, projectNumberLookup)
	if err != nil {
		t.Fatalf("cleanup-gcp failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"gcloud container clusters delete substrate-test --location=us-west1-c --project=acme",
		"gcloud storage buckets delete gs://acme-snapshots --project=acme",
		"serviceAccount:42-compute@developer.gserviceaccount.com",
		"gcloud monitoring dashboards list --project=acme",
		// The two workload-identity steps, the only ones that combine
		// PROJECT_ID, PROJECT_NUMBER, and BUCKET_NAME. Without these, --all
		// could drop either step and this test would still pass.
		"gcloud storage buckets remove-iam-policy-binding gs://acme-snapshots --member=principal://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/acme.svc.id.goog/subject/ns/ate-system/sa/atelet",
		"gcloud projects remove-iam-policy-binding acme --member=principal://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/acme.svc.id.goog/subject/ns/ate-system/sa/atelet",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("teardown did not run %q; calls:\n%s", want, log)
		}
	}
	if strings.Contains(log, "wrong-") {
		t.Errorf("teardown used values from the working directory's .ate-dev-env.sh:\n%s", log)
	}
	if strings.Contains(log, "git ") {
		t.Errorf("cleanup-gcp fetched a Substrate tree; the teardown is local now:\n%s", log)
	}
}

// A failed deletion must fail cleanup-gcp (and so `make teardown`) instead of
// ending in "Done.", or the user walks away from a cluster that still bills.
func TestCleanupGcpFailsWhenADeleteFails(t *testing.T) {
	out, _, err := runCleanupGcp(t, projectNumberLookup+`
if [ "$1 $2 $3" = "container clusters delete" ]; then
  echo "ERROR: (gcloud.container.clusters.delete) PERMISSION_DENIED: Required \"container.clusters.delete\" permission" >&2
  exit 1
fi`)
	if err == nil {
		t.Errorf("cleanup-gcp exited 0 after a denied cluster delete:\n%s", out)
	}
	if strings.Contains(out, "Done.") {
		t.Errorf("cleanup-gcp reported Done. after a denied cluster delete:\n%s", out)
	}
}

// An upgrade's summary hands over to the runbook: the line that moves a shell
// into its working directory and the runbook itself, instead of an install
// recap.
func TestSummaryForAnUpgrade(t *testing.T) {
	t.Setenv("ATE_RECORD_DIR", t.TempDir())
	st := state.NewSetup()
	st.Upgrade = true
	st.ProjectID, st.ClusterName = "acme", "prod"
	st.InstalledCommit, st.InstalledVersion = "0123456789abcdef0123456789abcdef01234567", "substrate-0123456789ab"
	deps := &ui.Deps{Setup: st, Builder: snapshot.NewBuilder(t.TempDir(), true), UpgradeDir: "/cache/upgrades"}

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, deps, false)
	})
	for _, want := range []string{"Upgrade prepared", "github.com/agent-substrate/substrate/blob/" + snapshot.Commit + "/docs/guides/upgrade.md", "cd ~/ate-upgrade/new && export KUBECONFIG=~/ate-upgrade/kubeconfig && export VERSION=", "export VERSION=substrate-0123456789ab there"} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade summary is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Substrate installed") {
		t.Errorf("upgrade summary reads like an install:\n%s", out)
	}
}

// The exit summary tells a Filestore run that failed from one that was
// skipped: the failed run may have left an IAM grant or a partial namespace
// behind, and "skipped" would hide that (#114).
func TestSummaryReportsAFailedFilestoreStep(t *testing.T) {
	builder := snapshot.NewBuilder(t.TempDir(), true)
	st := state.NewSetup()
	st.FilestoreCSIFailed = true
	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: st, Builder: builder}, true)
	})
	if !strings.Contains(out, "filestore  CSI driver install failed") || !strings.Contains(out, "gcp-filestore-csi-driver") {
		t.Errorf("summary does not report the failed Filestore step:\n%s", out)
	}
	// The teardown lead names the account deploy.sh created, which the
	// failed run may have left behind with its project role (#143).
	if !strings.Contains(out, "snapshot transfer, Filestore CSI and node service accounts") {
		t.Errorf("teardown lead does not name the Filestore CSI service account after a failed run:\n%s", out)
	}

	out = captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, &ui.Deps{Setup: state.NewSetup(), Builder: builder}, true)
	})
	if strings.Contains(out, "filestore  CSI driver") || strings.Contains(out, "Filestore CSI service account") {
		t.Errorf("summary has a Filestore line for an install that skipped the step:\n%s", out)
	}
}
