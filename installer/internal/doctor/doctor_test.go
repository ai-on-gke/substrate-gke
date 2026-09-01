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

func fatalKeys(t *testing.T, managed bool) map[string]bool {
	t.Helper()
	fatal := map[string]bool{}
	for _, c := range Checks(t.TempDir(), managed) {
		if c.Fatal {
			fatal[c.Key] = true
		}
	}
	return fatal
}

func TestChecksIncludeTheFatalSet(t *testing.T) {
	fatal := fatalKeys(t, true)
	// git is fatal when the installer owns the fetch: the tree comes down
	// with it, rather than being vendored.
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

// With --substrate-root the fetch preamble is a bare `cd`, so a git-less host
// can still install. Blocking there would refuse a flow the README advertises.
func TestGitIsOnlyFatalWhenTheInstallerFetches(t *testing.T) {
	if fatal := fatalKeys(t, false); fatal["git"] {
		t.Error(`check "git" must not be fatal for a user-supplied checkout`)
	}
	// The rest of the fatal set does not depend on who owns the checkout.
	for _, key := range []string{"gcloud", "adc", "go", "network"} {
		if !fatalKeys(t, false)[key] {
			t.Errorf("check %q should stay fatal regardless of --substrate-root", key)
		}
	}
}

// Only "auto" and "<version>+auto" download a newer toolchain. Treating every
// non-"local" value as safe passed the doctor and then died mid-bootstrap.
func TestToolchainSwitching(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want switchMode
	}{
		{"auto", switchDownloads},
		{"go1.21.0+auto", switchDownloads},
		{"local", switchNever},
		{"go1.24.0", switchNever}, // a bare pin never switches
		{"path", switchFromPath},
		{"go1.24.0+path", switchFromPath},
	} {
		if got := toolchainSwitching(tc.mode); got != tc.want {
			t.Errorf("toolchainSwitching(%q) = %v, want %v", tc.mode, got, tc.want)
		}
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
	for _, c := range Checks(t.TempDir(), true) {
		if c.Key != "go" {
			continue
		}
		if toolchainSwitching(goToolchainMode(context.Background())) != switchDownloads {
			t.Skip("GOTOOLCHAIN cannot fetch on this machine; the strict path is expected")
		}
		if got := c.Run(context.Background()); got.Status == Fail {
			t.Errorf("go check must not fail when GOTOOLCHAIN can fetch the toolchain: %q", got.Detail)
		}
		return
	}
	t.Fatal(`no "go" check found`)
}

// A missing checkout should warn rather than fail, and say where it will land.
func TestSnapshotCheckWarnsBeforeTheFirstFetch(t *testing.T) {
	root := t.TempDir()
	for _, c := range Checks(root, true) {
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
