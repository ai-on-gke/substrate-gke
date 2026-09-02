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
	script := b.Bootstrap(testSetup(t)).Argv[2]

	for _, want := range []string{
		"fetch -q --depth 1 " + RepoURL + " " + Commit,
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
	script := b.Bootstrap(testSetup(t)).Argv[2]

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
	// Reclaiming disk is Cleanup's job, after the install has succeeded. A
	// fetch that deleted things could not be safely retried.
	if strings.Contains(script, "rm -rf \"${STAGE}/.git\"") {
		t.Errorf("the fetch must not reclaim disk; that is deferred to Cleanup:\n%s", script)
	}
}

// Cleanup runs only after a successful install, so it may delete — but only
// things this installer created.
func TestCleanupReclaimsOnlyWhatTheInstallerCreated(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, treePrefix+"abc123456789")
	mkdir := func(parts ...string) string {
		p := filepath.Join(parts...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkdir(root, ".git")
	mkdir(root, "cmd", "ate-setup")
	stale := mkdir(base, treePrefix+"0ldc0mm1t")
	partial := mkdir(base, treePrefix+"abc123456789.partial.aB3xQ1")
	unrelated := mkdir(base, "some-other-tool")

	if err := NewBuilder(root, true).Cleanup(); err != nil {
		t.Fatal(err)
	}

	// The managed tree is scratch space for one install, so it goes too —
	// nothing here is meant to be worked in or kept.
	for _, gone := range []string{root, stale, partial} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", gone)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("cleanup must not touch directories it did not create: %v", err)
	}
}

// Nothing fetched, nothing to clean — and no error for it.
func TestCleanupToleratesAnAbsentCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "never-created", treePrefix+"abc123456789")
	if err := NewBuilder(root, true).Cleanup(); err != nil {
		t.Errorf("cleanup of an absent cache should be a no-op, got %v", err)
	}
}

// Deleting a tree the user pointed us at would be destroying their work, not
// tidying ours — and now that cleanup removes whole trees, the stakes are the
// user's entire checkout.
func TestCleanupNeverTouchesAnExplicitCheckout(t *testing.T) {
	root := fakeCheckout(t)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewBuilder(root, false).Cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{root, filepath.Join(root, ".git"), filepath.Join(root, "go.mod")} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("cleanup destroyed part of a user-supplied checkout (%s): %v", want, err)
		}
	}
}

// The teardown command has to work on a machine where the managed tree is
// already gone and in a shell opened weeks later, so it must not reference
// the cache, must carry the install's own target in its environment rather
// than trusting whatever kubectl context is ambient, and must reclaim its
// temporary checkout — a paste must not strand a full tree in $TMPDIR.
func TestTeardownCommandStandsAlone(t *testing.T) {
	cmd := TeardownCommand(testSetup(t), "")
	for _, want := range []string{
		RepoURL, Commit, "mktemp -d",
		`trap 'rm -rf "$d"' EXIT`,
		"PROJECT_ID=" + ShellQuote("acme"),
		"CLUSTER_NAME=" + ShellQuote("substrate-test"),
		"CLUSTER_LOCATION=" + ShellQuote("us-west1-c"),
		"NO_DEV_ENV=1 go run ./cmd/ate-setup delete ate-system",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("teardown command missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, treePrefix+ShortCommit()) {
		t.Errorf("teardown command points at the removed cache:\n%s", cmd)
	}
	// It is offered as a copy-paste, so it has to parse as one.
	if err := exec.Command("bash", "-n", "-c", cmd).Run(); err != nil {
		t.Errorf("teardown command is not valid shell: %v\n%s", err, cmd)
	}
}

// The variant for a user-supplied checkout cds there instead of fetching, and
// still pins the target cluster through the environment.
func TestTeardownCommandUsesAnExplicitCheckout(t *testing.T) {
	root := "/Users/John Smith/My Projects/substrate"
	cmd := TeardownCommand(testSetup(t), root)
	for _, want := range []string{
		"cd " + ShellQuote(root),
		"CLUSTER_NAME=" + ShellQuote("substrate-test"),
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("teardown command missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "mktemp") {
		t.Errorf("an explicit checkout needs no temporary fetch:\n%s", cmd)
	}
}

// Go's %q yields a double-quoted string, which bash still expands. A cache or
// --substrate-root path containing `$` or a backtick must survive verbatim.
func TestFetchPreambleQuotesPathsSafely(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a $HOME `id -u` dir")
	for _, managed := range []bool{true, false} {
		script := NewBuilder(root, managed).Bootstrap(testSetup(t)).Argv[2]
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

// The Filestore script interpolates wizard answers into gcloud invocations for
// the same reason the fetch preamble interpolates paths, and needs the same
// guarantee: whatever the user typed reaches the command as literal text.
func TestFilestoreScriptQuotesWizardAnswers(t *testing.T) {
	st := testSetup(t)
	st.ProjectID = "acme `id -u`"
	st.ClusterName = "clu'ster $HOME"
	st.Zone = "us-central1-a; id -u"

	script := NewBuilder(fakeCheckout(t), false).DeployFilestoreCSI(st).Argv[2]
	for _, answer := range []string{st.ProjectID, st.ClusterName, st.Zone} {
		quoted := ShellQuote(answer)
		if !strings.Contains(script, quoted) {
			t.Errorf("answer %q is not quoted in the script:\n%s", answer, script)
		}
		// Quoting is only worth anything if bash agrees, including for the
		// embedded single quote, which cannot be escaped inside '...'.
		out, err := exec.Command("bash", "-c", "printf '%s' "+quoted).Output()
		if err != nil {
			t.Fatalf("%q: %v", answer, err)
		}
		if string(out) != answer {
			t.Errorf("shell resolved %q to %q, want it verbatim", answer, out)
		}
	}
}

// inTree splices its argument into a bash script, so every value a caller
// puts in that argument has to be quoted. Today's only demo name is a
// literal; this keeps the seam closed if one ever comes from the user.
func TestDeployDemoQuotesTheDemoName(t *testing.T) {
	script := NewBuilder("/tmp/substrate-pin", true).DeployDemo(testSetup(t), "counter; id -u").Argv[2]
	if strings.Contains(script, "deploy demo counter; id -u") {
		t.Errorf("demo name reaches bash unquoted:\n%s", script)
	}
	if !strings.Contains(script, "deploy demo "+ShellQuote("counter; id -u")) {
		t.Errorf("demo name is not quoted:\n%s", script)
	}
}

// Two wizards racing must not leave a marked-complete tree that is empty.
func TestEnsureStagesUnderAUniquePath(t *testing.T) {
	script := NewBuilder("/tmp/substrate-pin", true).Bootstrap(testSetup(t)).Argv[2]

	if strings.Contains(script, `STAGE="${SUBSTRATE_DIR}.partial"`) {
		t.Errorf("the staging path is shared between concurrent runs:\n%s", script)
	}
	if !strings.Contains(script, `STAGE=$(mktemp -d "${SUBSTRATE_DIR}.partial.XXXXXX")`) {
		t.Errorf("the stage is not a per-process mktemp path:\n%s", script)
	}
	// The stage still has to be a sibling, or the publishing rename would
	// cross a filesystem and stop being atomic.
	if !strings.Contains(script, `mkdir -p "$(dirname "${SUBSTRATE_DIR}")"`) {
		t.Errorf("mktemp has no parent directory to stage in:\n%s", script)
	}
	// A run that dies between mktemp and the rename must not strand the
	// stage, since nothing else knows its name. The EXIT trap alone is not
	// enough: bash skips it entirely when it dies on an untrapped signal,
	// which is the Ctrl-C case.
	if !strings.Contains(script, `trap 'rm -rf "${STAGE}"' EXIT`) {
		t.Errorf("an interrupted fetch leaks its staging directory:\n%s", script)
	}
	if !strings.Contains(script, `trap 'exit 130' INT TERM`) {
		t.Errorf("a signalled fetch never reaches its EXIT trap:\n%s", script)
	}
	// And if a concurrent run published first, renaming onto its tree would
	// nest the stage inside it rather than replace it.
	if !strings.Contains(script, `rm -rf "${STAGE}"`+"\n    else") {
		t.Errorf("the loser of a publish race must discard its stage:\n%s", script)
	}
	// The pre-publish check cannot be atomic with the mv, so the loser of
	// that residual window has to mop up the stage the mv nested inside the
	// winner's tree.
	if !strings.Contains(script, `rm -rf "${SUBSTRATE_DIR}/${STAGE##*/}"`) {
		t.Errorf("a stage nested by a lost publish race is never reclaimed:\n%s", script)
	}
}

// The first run to finish must not tidy away the tree — or the in-flight
// stage — of a second run that is still installing.
func TestCleanupSparesEntriesOfLiveRuns(t *testing.T) {
	base := t.TempDir()
	mkdir := func(parts ...string) string {
		p := filepath.Join(parts...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ours := mkdir(base, treePrefix+"ourc0mm1t")
	theirs := mkdir(base, treePrefix+"theirc0mm1t")

	// The concurrent run locks its tree first, then stages its fetch — the
	// same order main() and the fetch preamble use.
	other := NewBuilder(theirs, true)
	other.Lock()
	if other.lock == nil {
		t.Skip("advisory locks unsupported on this platform")
	}
	t.Cleanup(func() { other.lock.Close() })
	theirStage := mkdir(base, treePrefix+"theirc0mm1t"+stageInfix+"aB3xQ1")

	b := NewBuilder(ours, true)
	b.Lock()
	if err := b.Cleanup(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Errorf("our own tree should have been removed")
	}
	for _, kept := range []string{theirs, theirStage} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("cleanup deleted %s out from under a live run: %v", kept, err)
		}
	}
}

// A fetch killed outright runs no traps and nothing else knows its mktemp
// name, so the next run of the installer sweeps orphaned stages up front —
// waiting for a successful install would keep ~185MB around indefinitely.
func TestLockReclaimsOrphanedStages(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, treePrefix+"abc123456789")
	orphan := filepath.Join(base, treePrefix+"abc123456789"+stageInfix+"dEadPr")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	NewBuilder(root, true).Lock()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("an orphaned stage survived the startup sweep")
	}
}

// A user-supplied tree must be used as-is, never overwritten by a fetch.
func TestEnsureLeavesAnExplicitCheckoutAlone(t *testing.T) {
	b := NewBuilder(fakeCheckout(t), false)
	script := b.Bootstrap(testSetup(t)).Argv[2]

	if strings.Contains(script, "git ") {
		t.Errorf("an explicit checkout must not be fetched over:\n%s", script)
	}
	if !strings.Contains(script, "cd "+ShellQuote(b.Root)) {
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

func testSetup(t *testing.T) *state.Setup {
	t.Helper()
	st := state.NewSetup()
	st.ProjectID = "acme"
	st.ProjectNumber = "42"
	if err := st.ApplyProjectDefaults(); err != nil {
		t.Fatalf("ApplyProjectDefaults: %v", err)
	}
	return st
}

func TestBuilderEnvCarriesTheDevEnvContract(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	spec := b.Bootstrap(testSetup(t))

	for _, want := range []string{
		"PROJECT_ID=acme",
		"PROJECT_NUMBER=42",
		"CLUSTER_NAME=substrate-test",
		"CLUSTER_LOCATION=us-west1-c",
		"GCE_REGION=us-west1",
		"BUCKET_NAME=ate-snapshots-acme-us-west1-c",
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
	st := testSetup(t)

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
	if !strings.Contains(demo.Argv[2], "deploy demo "+ShellQuote("counter")) {
		t.Errorf("demo argv = %v", demo.Argv)
	}
	// Display is for the user to read, not for a shell to run.
	if demo.Display != "go run ./cmd/ate-setup deploy demo counter" {
		t.Errorf("demo display = %q", demo.Display)
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
