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
	if !strings.Contains(out, snapshot.TeardownCommand(st, "")) {
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
