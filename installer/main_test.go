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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/ui"
)

// After a managed install the tree is gone, so the summary must not tell the
// user to cd into it.
func TestSummaryDoesNotPointAtTheRemovedCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "substrate-4a2cb262dd62")
	st := state.NewSetup()

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, st, snapshot.NewBuilder(root, true), true)
	})

	if strings.Contains(out, root) {
		t.Errorf("summary points at a directory cleanup removed:\n%s", out)
	}
	if !strings.Contains(out, snapshot.NewBuilder(root, true).TeardownCommand(st, "")) {
		t.Errorf("summary does not offer a self-contained teardown:\n%s", out)
	}
}

// Under --dry-run cleanup never runs, and it can also fail; either way a
// tree (or an older cache) may still be on disk, and the summary must not
// claim it was removed.
func TestSummaryDoesNotClaimARemovalThatDidNotHappen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "substrate-4a2cb262dd62")

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, state.NewSetup(), snapshot.NewBuilder(root, true), false)
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
		printSummary(&ui.App{Completed: true}, state.NewSetup(), snapshot.NewBuilder(root, false), false)
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
// complete GCP cleanup, carrying their own answers, quoted for pasting.
func TestSummaryOffersAFullGCPCleanup(t *testing.T) {
	st := state.NewSetup()
	st.ProjectID = "acme"
	st.BucketName = "ate-snapshots-acme-us-west1-c"

	out := captureStdout(t, func() {
		printSummary(&ui.App{Completed: true}, st, snapshot.NewBuilder(t.TempDir(), true), true)
	})

	for _, want := range []string{
		"./tools/cleanup-gcp",
		"--project " + snapshot.ShellQuote("acme"),
		"--cluster " + snapshot.ShellQuote(st.ClusterName),
		"--location " + snapshot.ShellQuote(st.Zone),
		"--bucket " + snapshot.ShellQuote(st.BucketName),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary cleanup line missing %q:\n%s", want, out)
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

// cleanup-gcp delegates its deletions to the pinned tree's hack/teardown.sh,
// and it reads the pin from the installer source rather than carrying a
// copy. This pins both halves of that contract: the parse must yield the
// exact Commit const, and the delegation must run the full teardown.
func TestCleanupGcpDelegatesAtThePin(t *testing.T) {
	out, err := exec.Command("bash", "../tools/cleanup-gcp", "--print-pin").Output()
	if err != nil {
		t.Fatalf("--print-pin failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != snapshot.Commit {
		t.Errorf("script parsed pin %q, want snapshot.Commit %q", got, snapshot.Commit)
	}

	script, err := os.ReadFile("../tools/cleanup-gcp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "./hack/teardown.sh --all") {
		t.Errorf("cleanup-gcp no longer delegates to hack/teardown.sh --all")
	}
	if strings.Contains(string(script), "remove-iam-policy-binding") {
		t.Errorf("cleanup-gcp still carries its own IAM deletions; that knowledge belongs upstream")
	}
}
