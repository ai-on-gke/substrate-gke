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

package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
)

func TestGoVersionOf(t *testing.T) {
	if got := goVersionOf("go version go1.26.4 darwin/arm64"); got != "1.26.4" {
		t.Fatalf("goVersionOf = %q", got)
	}
	if got := goVersionOf("gibberish"); got != "" {
		t.Fatalf("goVersionOf(garbage) = %q", got)
	}
}

func TestGoVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		ok         bool
	}{
		{"1.26.4", "1.26.3", true},
		{"1.26.3", "1.26.3", true},
		{"1.25.9", "1.26.3", false},
		{"1.26", "1.26.3", false},
		{"2.0", "1.26.3", true},
		{"", "1.26.3", true}, // unparseable output never blocks
	} {
		if got := goVersionAtLeast(tc.have, tc.want); got != tc.ok {
			t.Errorf("goVersionAtLeast(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func TestRequiredGoVersionReadsGoMod(t *testing.T) {
	dir := t.TempDir()
	mod := "module example.com/x\n\ngo 1.26.3\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := requiredGoVersion(dir); got != "1.26.3" {
		t.Fatalf("requiredGoVersion = %q", got)
	}
	// The checkout is absent until the first install step fetches it, so the
	// Go check must still have a version to compare against.
	if got := requiredGoVersion(t.TempDir()); got != snapshot.MinGoVersion {
		t.Fatalf("requiredGoVersion(missing) = %q, want the pinned fallback %q", got, snapshot.MinGoVersion)
	}
}

func TestChecksIncludeTheFatalSet(t *testing.T) {
	fatal := map[string]bool{}
	for _, c := range Checks(t.TempDir()) {
		if c.Fatal {
			fatal[c.Key] = true
		}
	}
	// git is fatal: the substrate tree is fetched with it, not vendored.
	for _, key := range []string{"gcloud", "adc", "go", "network", "git"} {
		if !fatal[key] {
			t.Errorf("check %q should be fatal", key)
		}
	}
	// The checkout is fetched on demand, so its absence must not block.
	if fatal["snapshot"] {
		t.Error(`check "snapshot" must not be fatal; it self-heals by fetching`)
	}
}

// The pinned substrate can need a newer Go than the one installed. With
// toolchain switching on (the default) that is not a blocker, so the check
// must not fail the install for it.
func TestGoCheckDefersToToolchainSwitching(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	// An empty root makes requiredGoVersion fall back to the pinned version,
	// which is newer than plenty of installed toolchains.
	for _, c := range Checks(t.TempDir()) {
		if c.Key != "go" {
			continue
		}
		got := c.Run(context.Background())
		if goToolchainMode(context.Background()) == "local" {
			t.Skip("GOTOOLCHAIN=local on this machine; the strict path is expected")
		}
		if got.Status == Fail {
			t.Errorf("go check must not fail when GOTOOLCHAIN can fetch the toolchain: %q", got.Detail)
		}
		return
	}
	t.Fatal(`no "go" check found`)
}

// A missing checkout should warn rather than fail, and say where it will land.
func TestSnapshotCheckWarnsBeforeTheFirstFetch(t *testing.T) {
	root := t.TempDir()
	for _, c := range Checks(root) {
		if c.Key != "snapshot" {
			continue
		}
		got := c.Run(context.Background())
		if got.Status != Warn {
			t.Fatalf("status = %v, want Warn", got.Status)
		}
		if !strings.Contains(got.Detail, root) || !strings.Contains(got.Detail, snapshot.ShortCommit()) {
			t.Errorf("detail should name the destination and pin, got %q", got.Detail)
		}
		return
	}
	t.Fatal(`no "snapshot" check found`)
}
