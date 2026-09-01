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

package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

func fakeCheckout(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "substrate")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module github.com/agent-substrate/substrate\n\ngo " + MinGoVersion + "\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRootDefaultsToAPinnedCacheDir(t *testing.T) {
	got, managed, err := Root("")
	if err != nil {
		t.Fatal(err)
	}
	if !managed {
		t.Error("the default root must be installer-managed")
	}
	// Keyed by commit so bumping the pin cannot reuse a stale tree.
	if want := "substrate-" + ShortCommit(); filepath.Base(got) != want {
		t.Errorf("Root() = %q, want a directory named %q", got, want)
	}
	// The tree is fetched on demand, so Root must not require it to exist.
	if _, err := os.Stat(got); err == nil {
		t.Skip("cache dir already populated on this machine")
	}
}

func TestRootAcceptsAnExplicitCheckout(t *testing.T) {
	root := fakeCheckout(t)
	got, managed, err := Root(root)
	if err != nil {
		t.Fatal(err)
	}
	if managed {
		t.Error("a user-supplied checkout must not be installer-managed")
	}
	if got != root {
		t.Errorf("Root(explicit) = %q, want %q", got, root)
	}
}

func TestRootRejectsANonCheckout(t *testing.T) {
	if _, _, err := Root(t.TempDir()); err == nil {
		t.Fatal("want an error for a directory without go.mod")
	}
}

func TestPinIsAFullCommitSHA(t *testing.T) {
	if len(Commit) != 40 {
		t.Fatalf("Commit must be a full 40-char SHA, got %d chars: %q", len(Commit), Commit)
	}
	for _, r := range Commit {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("Commit is not lowercase hex: %q", Commit)
		}
	}
}

// The fetch must name an exact commit: a branch or tag would silently drift.
func TestEnsureFetchesThePinnedCommitShallowly(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	script := b.Bootstrap(testSetup()).Argv[2]

	for _, want := range []string{
		"fetch -q --depth 1 origin " + Commit,
		"remote add origin " + RepoURL,
		"checkout -q FETCH_HEAD",
		`cd "${SUBSTRATE_DIR}"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("fetch preamble missing %q:\n%s", want, script)
		}
	}
	// Idempotent: a complete tree is reused rather than re-fetched.
	if !strings.Contains(script, `if [ ! -e "${SUBSTRATE_DIR}/`+CompleteMarker+`" ]; then`) {
		t.Errorf("fetch preamble is not guarded by the completion marker:\n%s", script)
	}
}

// An interrupted fetch must not leave anything the next run would trust, so
// the tree is built in a staging directory and moved into place at the end.
func TestEnsureStagesTheTreeAndPublishesItAtomically(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	script := b.Bootstrap(testSetup()).Argv[2]

	if strings.Contains(script, `git -C "${SUBSTRATE_DIR}"`) {
		t.Errorf("git must work in the staging dir, never in the published one:\n%s", script)
	}
	marker := strings.Index(script, `touch "${STAGE}/`+CompleteMarker)
	publish := strings.Index(script, `mv "${STAGE}" "${SUBSTRATE_DIR}"`)
	switch {
	case marker < 0:
		t.Errorf("fetch never writes the completion marker:\n%s", script)
	case publish < 0:
		t.Errorf("fetch never moves the staged tree into place:\n%s", script)
	case marker > publish:
		t.Errorf("the marker must land before the tree is published:\n%s", script)
	}
	// The shallow pack is dead weight once the working tree exists.
	if !strings.Contains(script, `rm -rf "${STAGE}/.git"`) {
		t.Errorf("fetch keeps the .git directory:\n%s", script)
	}
}

// Go's %q yields a double-quoted string, which bash still expands. A cache or
// --substrate-root path containing `$` or a backtick must survive verbatim.
func TestFetchPreambleQuotesPathsSafely(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a $HOME `id -u` dir")
	for _, managed := range []bool{true, false} {
		script := NewBuilder(root, managed).Bootstrap(testSetup()).Argv[2]
		// Run just the assignments, then ask the shell what it resolved.
		prelude, _, ok := strings.Cut(script, "\nif [ ")
		if !ok {
			prelude, _, _ = strings.Cut(script, "\ngo run ")
			prelude = strings.Replace(prelude, "cd ", "SUBSTRATE_DIR=", 1)
		}
		out, err := exec.Command("bash", "-c", prelude+"\nprintf '%s' \"${SUBSTRATE_DIR}\"").Output()
		if err != nil {
			t.Fatalf("managed=%v: %v\n%s", managed, err, prelude)
		}
		if string(out) != root {
			t.Errorf("managed=%v: shell resolved the path to %q, want the literal %q", managed, out, root)
		}
	}
}

// A user-supplied tree must be used as-is, never overwritten by a fetch.
func TestEnsureLeavesAnExplicitCheckoutAlone(t *testing.T) {
	b := NewBuilder(fakeCheckout(t), false)
	script := b.Bootstrap(testSetup()).Argv[2]

	if strings.Contains(script, "git ") {
		t.Errorf("an explicit checkout must not be fetched over:\n%s", script)
	}
	if !strings.Contains(script, "cd "+shellQuote(b.Root)) {
		t.Errorf("script does not cd into the explicit checkout:\n%s", script)
	}
}

// git checks files out in path order, so an interrupted fetch can leave go.mod
// behind without the rest of the tree. Only the marker proves completeness.
func TestFetchedRequiresTheCompletionMarker(t *testing.T) {
	partial := fakeCheckout(t)
	if Fetched(partial, true) {
		t.Error("a managed checkout without its marker must not count as fetched")
	}
	// A user-supplied tree never carries a marker and must still be usable.
	if !Fetched(partial, false) {
		t.Error("an explicit checkout should be usable as-is")
	}
	if err := os.WriteFile(filepath.Join(partial, CompleteMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !Fetched(partial, true) {
		t.Error("a marked checkout must count as fetched")
	}
}

func testSetup() *state.Setup {
	st := state.NewSetup()
	st.ProjectID = "acme"
	st.ProjectNumber = "42"
	st.ApplyProjectDefaults()
	return st
}

func TestBuilderEnvCarriesTheDevEnvContract(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	spec := b.Bootstrap(testSetup())

	for _, want := range []string{
		"PROJECT_ID=acme",
		"PROJECT_NUMBER=42",
		"CLUSTER_NAME=substrate-poc",
		"CLUSTER_LOCATION=us-west1-c",
		"GCE_REGION=us-west1",
		"BUCKET_NAME=ate-snapshots-acme-2c2cf930b4f9d8c2",
		"KO_DOCKER_REPO=gcr.io/acme/ate-images",
		"NO_DEV_ENV=1",
		"VERSION=substrate-" + ShortCommit(),
	} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("Bootstrap env missing %q", want)
		}
	}
	if !strings.Contains(spec.Argv[2], "go run ./tools/setup-gcp bootstrap") {
		t.Errorf("unexpected argv: %v", spec.Argv)
	}
}

func TestDeploySpecs(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	st := testSetup()

	deploy := b.DeployAteSystem(st)
	if !strings.Contains(deploy.Argv[2], "go run ./cmd/ate-setup deploy ate-system") {
		t.Errorf("deploy argv = %v", deploy.Argv)
	}
	// The script cds into the checkout itself, so Dir must stay unset — the
	// tree may not exist yet when the command starts.
	if deploy.Dir != "" {
		t.Errorf("deploy must not pin Dir to the checkout, got %q", deploy.Dir)
	}

	demo := b.DeployDemo(st, "counter")
	if !strings.Contains(demo.Argv[2], "deploy demo counter") {
		t.Errorf("demo argv = %v", demo.Argv)
	}

	st.AutoscaleMin, st.AutoscaleMax = 2, 9
	scale := b.EnableAutoscaling(st)
	if !slices.Contains(scale.Argv, "--min-nodes=2") || !slices.Contains(scale.Argv, "--max-nodes=9") {
		t.Errorf("autoscaling argv = %v", scale.Argv)
	}

	filestore := b.DeployFilestoreCSI(st)
	if filestore.Label != "deploy filestore csi driver" {
		t.Errorf("filestore label = %q", filestore.Label)
	}
	if filestore.Argv[0] != "bash" || filestore.Argv[1] != "-c" {
		t.Errorf("filestore argv = %v", filestore.Argv)
	}
	if !strings.Contains(filestore.Argv[2], "GcpFilestoreCsiDriver=DISABLED") {
		t.Errorf("filestore script missing addon disable check: %s", filestore.Argv[2])
	}
	if !slices.Contains(filestore.Env, "PROJECT_ID=acme") {
		t.Errorf("filestore env missing project ID: %v", filestore.Env)
	}
}
