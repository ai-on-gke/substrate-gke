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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
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
		"fetch -q --depth 1 " + ShellQuote(RepoURL) + " " + Commit,
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
	cmd := NewBuilder("/tmp/substrate-pin", true).TeardownCommand(testSetup(t), "")
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

// The wizard's in-place teardown carries the cluster in its own env, so it
// can only ever delete from the cluster it names, never whatever context is
// ambient — and a cluster name with a space must not split it.
func TestDeleteAteSystemNamesTheCluster(t *testing.T) {
	spec := NewBuilder("/tmp/substrate-pin", true).DeleteAteSystem("acme", "prod cluster", "us-west1-c")
	script := spec.Argv[len(spec.Argv)-1]
	for _, want := range []string{
		"PROJECT_ID=" + ShellQuote("acme"),
		"CLUSTER_NAME=" + ShellQuote("prod cluster"),
		"CLUSTER_LOCATION=" + ShellQuote("us-west1-c"),
		"NO_DEV_ENV=1 go run ./cmd/ate-setup delete ate-system",
		// ate-setup returns while the namespace is still deleting; the
		// teardown must outlast it, or the immediate re-probe reads (and
		// caches) the half-deleted install as still installed.
		"kubectl wait --for=delete namespace/ate-system",
		`KUBECONFIG=$(mktemp)`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("delete script missing %q:\n%s", want, script)
		}
	}
	if err := exec.Command("bash", "-n", "-c", script).Run(); err != nil {
		t.Errorf("delete script is not valid shell: %v\n%s", err, script)
	}
}

// The kubectl-ate install has the same constraints as the teardown — the
// managed checkout is gone by the time the user reads it — plus one of its
// own: it cannot be `go install <module>@<pin>`, which Go refuses for this
// module because its go.mod replaces k8s.io/apimachinery with a local
// third_party path. It must build from a fetched checkout instead.
func TestKubectlAteInstallStandsAlone(t *testing.T) {
	cmd := NewBuilder("/tmp/substrate-pin", true).KubectlAteInstall()
	for _, want := range []string{
		RepoURL, Commit, "mktemp -d",
		`trap 'rm -rf "$d"' EXIT`,
		"go install ./cmd/kubectl-ate",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("kubectl-ate install missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, ModulePath+"/cmd/kubectl-ate@") {
		t.Errorf("`go install module@version` is refused for a module with replace directives:\n%s", cmd)
	}
	if err := exec.Command("bash", "-n", "-c", cmd).Run(); err != nil {
		t.Errorf("kubectl-ate install is not valid shell: %v\n%s", err, cmd)
	}
}

// The variant for a user-supplied checkout cds there instead of fetching, and
// still pins the target cluster through the environment.
func TestTeardownCommandUsesAnExplicitCheckout(t *testing.T) {
	root := "/Users/John Smith/My Projects/substrate"
	cmd := NewBuilder("/tmp/substrate-pin", true).TeardownCommand(testSetup(t), root)
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
			prelude, _, _ = strings.Cut(script, "\ngo -C ")
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

// The micro-VM script uploads assets and applies a cluster-scoped resource, so
// pointing it at the wrong cluster is not a no-op. It must target the cluster
// the wizard chose, never whatever context happens to be ambient.
func TestInstallMicroVMDepsTargetsTheChosenCluster(t *testing.T) {
	st := testSetup(t)
	st.ClusterName = "clu'ster $HOME"
	st.Zone = "us-central1-a; id -u"

	script := NewBuilder(fakeCheckout(t), false).InstallMicroVMDeps(st).Argv[2]
	for _, want := range []string{
		"gcloud container clusters get-credentials " + ShellQuote(st.ClusterName),
		"--location " + ShellQuote(st.Zone),
		"--project " + ShellQuote(st.ProjectID),
		"hack/install-microvm-deps.sh --install",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

// The assets are executed by the cluster's nodes, not by the workstation
// running the wizard, and they land in the project's GCS bucket, not in a
// kind-local one. Both are the script's inputs rather than its defaults, so
// an inherited ARCH or ATE_INSTALL_KIND from the operator's shell would
// silently produce a broken install.
func TestMicroVMDepsPinArchAndStagingTarget(t *testing.T) {
	st := testSetup(t)
	st.SandboxClass = state.SandboxMicroVM
	st.MicroVMDeployed = true
	b := NewBuilder(fakeCheckout(t), false)

	for _, spec := range []execx.Spec{b.InstallMicroVMDeps(st), b.DeployDemo(st, "counter")} {
		for _, want := range []string{"ARCH=amd64", "ATE_INSTALL_KIND=false"} {
			if !slices.Contains(spec.Env, want) {
				t.Errorf("%s: env missing %q:\n%v", spec.Label, want, spec.Env)
			}
		}
	}
}

// A developer who sourced .ate-dev-env.sh has KUBECTL_CONTEXT exported, and
// every spec inherits it. ate-setup would take it over CLUSTER_NAME — skip
// get-credentials and install into, or delete from, whatever cluster it
// names — and the scripts would pass it as --context into a throwaway
// KUBECONFIG that does not have it. Every spec and pasted command that
// reaches either must blank it, and the blank must win at exec time.
func TestSpecsBlankAnInheritedKubectlContext(t *testing.T) {
	t.Setenv("KUBECTL_CONTEXT", "gke_dev_some-other-cluster")
	st := testSetup(t)
	micro := testSetup(t)
	micro.SandboxClass = state.SandboxMicroVM
	micro.MicroVMDeployed = true
	b := NewBuilder(fakeCheckout(t), false)

	for _, spec := range []execx.Spec{
		b.Bootstrap(st),
		b.DeployAteSystem(st),
		b.DeployDemo(st, "counter"),
		b.DeployFilestoreCSI(st),
		b.InstallMicroVMDeps(micro),
		b.DeployDemo(micro, "counter"),
	} {
		if !slices.Contains(spec.Env, "KUBECTL_CONTEXT=") {
			t.Errorf("%s: env does not blank KUBECTL_CONTEXT:\n%v", spec.Label, spec.Env)
		}
	}

	// Env is layered over os.Environ(), so the blank has to be the value the
	// child actually sees, not just an entry in the slice.
	probe := execx.Spec{
		Argv: []string{"bash", "-c", `printf '[%s]' "${KUBECTL_CONTEXT-unset}"`},
		Env:  b.DeployAteSystem(st).Env,
	}
	var out strings.Builder
	for ev := range (&execx.Real{}).Start(context.Background(), probe) {
		if ev.Done && ev.Err != nil {
			t.Fatalf("probe failed: %v", ev.Err)
		}
		out.WriteString(ev.Line)
	}
	if got := out.String(); got != "[]" {
		t.Errorf("child saw KUBECTL_CONTEXT=%s, want it blank", got)
	}

	del := b.DeleteAteSystem("acme", "substrate-test", "us-west1-c")
	for name, cmd := range map[string]string{
		"teardown":          b.TeardownCommand(st, ""),
		"explicit teardown": b.TeardownCommand(st, "/tmp/substrate"),
		"delete":            del.Argv[len(del.Argv)-1],
	} {
		if !strings.Contains(cmd, "KUBECTL_CONTEXT= NO_DEV_ENV=1 go run ./cmd/ate-setup delete ate-system") {
			t.Errorf("%s command does not blank KUBECTL_CONTEXT for ate-setup:\n%s", name, cmd)
		}
	}
}

func TestDeployDemoMicroVMAndNextSteps(t *testing.T) {
	st := testSetup(t)
	st.SandboxClass = state.SandboxMicroVM
	b := NewBuilder(fakeCheckout(t), false)

	// When MicroVM was chosen but assets were NOT staged, DeployDemo and
	// NextSteps must fall back to the gVisor counter demo.
	unstagedSpec := b.DeployDemo(st, "counter")
	if !strings.Contains(unstagedSpec.Argv[2], "go run ./cmd/ate-setup deploy demo "+ShellQuote("counter")) {
		t.Errorf("unstaged micro-VM should fall back to gVisor counter demo, got:\n%s", unstagedSpec.Argv[2])
	}
	_, unstagedSteps := b.NextSteps(st)
	if !strings.Contains(strings.Join(unstagedSteps, "\n"), "-a ate-demo-counter --template counter") {
		t.Errorf("unstaged micro-VM NextSteps should target ate-demo-counter, got:\n%v", unstagedSteps)
	}

	// Once staged, DeployDemo runs the counter-microvm demo (through
	// ate-setup where the tree has it, install-ate.sh otherwise), and
	// NextSteps targets ate-demo-counter-microvm.
	st.MicroVMDeployed = true
	stagedSpec := b.DeployDemo(st, "counter")
	for _, want := range []string{
		"go run ./cmd/ate-setup deploy demo counter-microvm",
		"./hack/install-ate.sh --deploy-demo-counter-microvm",
	} {
		if !strings.Contains(stagedSpec.Argv[2], want) {
			t.Errorf("staged micro-VM DeployDemo missing %q:\n%s", want, stagedSpec.Argv[2])
		}
	}
	_, stagedSteps := b.NextSteps(st)
	joined := strings.Join(stagedSteps, "\n")
	if want := "kubectl ate create actor my-counter-1 -a ate-demo-counter-microvm --template counter-microvm"; !strings.Contains(joined, want) {
		t.Errorf("staged micro-VM NextSteps missing %q:\n%s", want, joined)
	}

	// On a pre-built install (even if ImageTag carries a digest), DeployDemo
	// resolves a per-image digest and edits templates portably with sed -i.bak.
	prebuilt := prebuiltSetup(t)
	prebuilt.SandboxClass = state.SandboxMicroVM
	prebuilt.MicroVMDeployed = true
	prebuilt.ImageTag = "v0.1.0-gke.1@sha256:" + strings.Repeat("ab", 32)
	prebuiltScript := b.DeployDemo(prebuilt, "counter").Argv[2]
	for _, want := range []string{
		"ATEOM_REF=" + ShellQuote(testImageRepo+"/ateom-microvm:v0.1.0-gke.1"),
		"COUNTER_REF=" + ShellQuote(testImageRepo+"/counter:v0.1.0-gke.1"),
		`ATEOM_DIGEST=$(go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 digest "${ATEOM_REF}")`,
		`COUNTER_DIGEST=$(go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 digest "${COUNTER_REF}")`,
		`sed -i.bak "s|ko://github.com/agent-substrate/substrate/cmd/ateom-microvm|${ATEOM_REF}@${ATEOM_DIGEST}|g"`,
		`sed -i.bak "s|ko://github.com/agent-substrate/substrate/demos/counter|${COUNTER_REF}@${COUNTER_DIGEST}|g"`,
		"trap 'exit 130' INT TERM",
	} {
		if !strings.Contains(prebuiltScript, want) {
			t.Errorf("pre-built micro-VM DeployDemo script missing %q:\n%s", want, prebuiltScript)
		}
	}
	// ImageRepo is free text; a GCR/Artifact Registry-only lookup would
	// fail the demo for any other registry.
	if strings.Contains(prebuiltScript, "gcloud container images describe") {
		t.Errorf("pre-built micro-VM DeployDemo must resolve digests registry-neutrally, got:\n%s", prebuiltScript)
	}
	if err := exec.Command("bash", "-n", "-c", prebuiltScript).Run(); err != nil {
		t.Errorf("pre-built micro-VM DeployDemo script is not valid shell: %v\n%s", err, prebuiltScript)
	}
}

// The pre-built micro-VM demo rewrites two tracked templates in place. They
// must come back from the .bak copies sed made — never `git checkout`, which
// would discard a --substrate-root user's uncommitted edits — and a killed
// run's leftovers must be restored before the next run edits anything, on
// the source path too.
func TestMicroVMDemoRestoresTemplatesFromBackups(t *testing.T) {
	b := NewBuilder(fakeCheckout(t), false)
	source := testSetup(t)
	source.SandboxClass = state.SandboxMicroVM
	source.MicroVMDeployed = true
	prebuilt := prebuiltSetup(t)
	prebuilt.SandboxClass = state.SandboxMicroVM
	prebuilt.MicroVMDeployed = true

	for name, st := range map[string]*state.Setup{"source": source, "pre-built": prebuilt} {
		script := b.DeployDemo(st, "counter").Argv[2]
		if strings.Contains(script, "git checkout") {
			t.Errorf("%s: templates must not be restored from git:\n%s", name, script)
		}
		restore := strings.Index(script, "\nrestore_demo_templates\n")
		demo := strings.Index(script, "./hack/install-ate.sh --deploy-demo-counter-microvm")
		if restore < 0 || demo < 0 || restore > demo {
			t.Errorf("%s: leftover templates are not restored before the demo runs:\n%s", name, script)
		}
		if sed := strings.Index(script, "sed -i.bak"); sed >= 0 && sed < restore {
			t.Errorf("%s: templates are edited before leftovers are restored:\n%s", name, script)
		}
	}
	prebuiltScript := b.DeployDemo(prebuilt, "counter").Argv[2]
	if !strings.Contains(prebuiltScript, "trap restore_demo_templates EXIT") {
		t.Errorf("pre-built: the EXIT trap does not restore from the backups:\n%s", prebuiltScript)
	}
	if strings.Contains(prebuiltScript, "rm -f "+demoPoolTemplate+".bak") {
		t.Errorf("pre-built: the backups are deleted before the trap can restore from them:\n%s", prebuiltScript)
	}
}

// Runs the restore helper and the rewrite the demo uses under real bash and
// sed: a killed run's leftover is undone first, and a user's uncommitted edit
// survives the rewrite-and-restore round trip.
func TestRestoreDemoTemplatesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "demos", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	pool := filepath.Join(dir, demoPoolTemplate)
	const original = "image: ko://github.com/agent-substrate/substrate/cmd/ateom-microvm # user edit\n"
	// A previous run was SIGKILLed after its sed: the template carries the
	// old digest and the .bak holds the user's file.
	if err := os.WriteFile(pool, []byte("image: repo/ateom-microvm:old@sha256:old # user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pool+".bak", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	script := strings.Join(append(append([]string{"set -euo pipefail"}, restoreDemoTemplates...),
		"(",
		"  trap restore_demo_templates EXIT",
		`  sed -i.bak "s|ko://github.com/agent-substrate/substrate/cmd/ateom-microvm|repo/ateom-microvm:new@sha256:new|g" `+demoPoolTemplate,
		"  cp "+demoPoolTemplate+" seen",
		")",
	), "\n")
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	if seen, _ := os.ReadFile(filepath.Join(dir, "seen")); !strings.Contains(string(seen), "sha256:new") {
		t.Errorf("the demo saw %q, want the new digest (the leftover was not restored first)", seen)
	}
	if got, _ := os.ReadFile(pool); string(got) != original {
		t.Errorf("template after the run = %q, want the user's file %q", got, original)
	}
	if _, err := os.Stat(pool + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup left behind after the run (err=%v)", err)
	}
}

// A tree whose ate-setup has counter-microvm (upstream v0.2.0 and main)
// deploys the micro-VM demo through it, as the gVisor demo does, so it
// survives install-ate.sh's removal upstream; only older trees, the pinned
// Commit among them, fall back to install-ate.sh. On a pre-built install
// ate-setup gets the image flags and resolves digests itself, and nothing of
// the fallback's crane/sed/ko.local rewrite reaches it.
func TestMicroVMDemoPrefersAteSetup(t *testing.T) {
	b := NewBuilder(fakeCheckout(t), false)
	source := testSetup(t)
	source.SandboxClass = state.SandboxMicroVM
	source.MicroVMDeployed = true
	prebuilt := prebuiltSetup(t)
	prebuilt.SandboxClass = state.SandboxMicroVM
	prebuilt.MicroVMDeployed = true
	imageFlags := " --image-repo " + ShellQuote(testImageRepo) + " --image-tag " + ShellQuote(testImageTag)

	for name, tc := range map[string]struct {
		st           *state.Setup
		ateSetup     string
		fallbackOnly []string
	}{
		"source":    {source, "go run ./cmd/ate-setup deploy demo counter-microvm\n", nil},
		"pre-built": {prebuilt, "go run ./cmd/ate-setup deploy demo counter-microvm" + imageFlags + "\n", []string{craneDigest, "sed -i.bak", "export KO_DOCKER_REPO=ko.local"}},
	} {
		spec := b.DeployDemo(tc.st, "counter")
		script := spec.Argv[2]
		check := strings.Index(script, "if [ -d "+counterMicroVMDemoDir+" ]; then\n")
		ateSetup := strings.Index(script, tc.ateSetup)
		elseAt := strings.Index(script, "\nelse\n")
		fallback := strings.Index(script, "./hack/install-ate.sh --deploy-demo-counter-microvm")
		restore := strings.Index(script, "\nrestore_demo_templates\n")
		if check < 0 || ateSetup < check || elseAt < ateSetup || fallback < elseAt {
			t.Errorf("%s: want `if [ -d %s ]` → %q → else → install-ate.sh:\n%s", name, counterMicroVMDemoDir, tc.ateSetup, script)
		}
		if restore < 0 || restore > check {
			t.Errorf("%s: leftover templates are not restored before either branch reads them:\n%s", name, script)
		}
		for _, want := range tc.fallbackOnly {
			if at := strings.Index(script, want); at < elseAt {
				t.Errorf("%s: %q must only run in the install-ate.sh fallback:\n%s", name, want, script)
			}
		}
		if slices.Contains(spec.Env, "KO_DOCKER_REPO=ko.local") {
			t.Errorf("%s: KO_DOCKER_REPO=ko.local is in the step env, where ate-setup sees it:\n%v", name, spec.Env)
		}
		// Display is fixed before the tree is inspected, so it must not
		// name one branch as the command that runs; each branch announces
		// itself instead.
		if strings.HasPrefix(spec.Display, "go run") || strings.HasPrefix(spec.Display, "./hack") {
			t.Errorf("%s: display names one branch as the command: %q", name, spec.Display)
		}
		if !spec.DisplayIsSummary {
			t.Errorf("%s: the step title is not marked as a summary, so it is shown as a command", name)
		}
		for _, want := range []string{
			"then\n  echo 'Deploying with: go run ./cmd/ate-setup deploy demo counter-microvm'\n",
			"else\n  echo 'Deploying with: hack/install-ate.sh --deploy-demo-counter-microvm'\n",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: script does not announce its branch with %q:\n%s", name, want, script)
			}
		}
		if err := exec.Command("bash", "-n", "-c", script).Run(); err != nil {
			t.Errorf("%s: script is not valid shell: %v\n%s", name, err, script)
		}
	}
}

// Runs the branch the micro-VM demo takes under real bash, with go and
// install-ate.sh stubbed, in a tree with and without ate-setup's
// counter-microvm demo.
func TestMicroVMDemoBranchesOnTheTree(t *testing.T) {
	b := NewBuilder(fakeCheckout(t), false)
	source := testSetup(t)
	source.SandboxClass = state.SandboxMicroVM
	source.MicroVMDeployed = true
	prebuilt := prebuiltSetup(t)
	prebuilt.SandboxClass = state.SandboxMicroVM
	prebuilt.MicroVMDeployed = true

	const viaAteSetup = "Deploying with: go run ./cmd/ate-setup deploy demo counter-microvm"
	const viaInstallAte = "Deploying with: hack/install-ate.sh --deploy-demo-counter-microvm"
	for _, tc := range []struct {
		name     string
		st       *state.Setup
		hasDemo  bool
		want     string
		announce string
	}{
		{"source, new tree", source, true, "go run ./cmd/ate-setup deploy demo counter-microvm KO=unset", viaAteSetup},
		{"source, old tree", source, false, "install-ate.sh --deploy-demo-counter-microvm", viaInstallAte},
		{"pre-built, new tree", prebuilt, true, "go run ./cmd/ate-setup deploy demo counter-microvm --image-repo " + testImageRepo + " --image-tag " + testImageTag + " KO=unset", viaAteSetup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			for path, body := range map[string]string{
				filepath.Join(bin, "go"):                     `echo "go $* KO=${KO_DOCKER_REPO-unset}" >> calls`,
				filepath.Join(dir, "hack", "install-ate.sh"): `echo "install-ate.sh $*" >> calls`,
			} {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.hasDemo {
				if err := os.MkdirAll(filepath.Join(dir, counterMicroVMDemoDir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			// Everything from the restore helper on: the fetch and the
			// get-credentials before it need a network.
			script := b.DeployDemo(tc.st, "counter").Argv[2]
			script = "set -euo pipefail\n" + script[strings.Index(script, "restore_demo_templates() {"):]
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + dir}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if got := strings.TrimSpace(string(calls)); got != tc.want {
				t.Errorf("ran %q, want %q", got, tc.want)
			}
			if got := strings.TrimSpace(string(out)); got != tc.announce {
				t.Errorf("printed %q, want %q", got, tc.announce)
			}
		})
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

// testImageRepo and testImageTag stand in for a published image set, so the
// tests do not depend on ReleaseRepo and ReleaseVersion, which move with the
// pin.
const (
	testImageRepo = "registry.example.com/substrate"
	testImageTag  = "v0.6.0"
)

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

// The fetch has to produce a usable checkout of exactly the commit asked for,
// and refuse to write into a directory that already holds one.
func TestFetchTreeChecksOutTheCommit(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", origin}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "first")
	if err := os.WriteFile(filepath.Join(origin, "go.mod"), []byte("module example.com/substrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "go.mod")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "second")
	want := run("rev-parse", "HEAD")

	dir := filepath.Join(t.TempDir(), "tree")
	if err := fetchTree(context.Background(), dir, origin, want); err != nil {
		t.Fatalf("fetchTree: %v", err)
	}
	got, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse in the fetched tree: %v", err)
	}
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("fetched tree is at %s, want %s", got, want)
	}
	if !isCheckout(dir) {
		t.Errorf("fetched tree has no go.mod")
	}
	if _, err := os.Stat(filepath.Join(dir, CompleteMarker)); err != nil {
		t.Errorf("a fetched tree should carry the completion marker: %v", err)
	}
	if status, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); err != nil || len(status) != 0 {
		t.Errorf("the marker must not dirty the tree (%v):\n%s", err, status)
	}
	if err := fetchTree(context.Background(), dir, origin, want); err == nil {
		t.Errorf("fetching over an existing checkout should be refused")
	}
	// An interrupted checkout has go.mod and little else; it is refused with
	// a way out, not mistaken for a tree.
	partial := filepath.Join(t.TempDir(), "partial")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fetchTree(context.Background(), partial, origin, want); err == nil || !strings.Contains(err.Error(), "remove it") {
		t.Errorf("a partial tree should be refused with instructions, got %v", err)
	}
}

func TestBuilderEnvCarriesTheDevEnvContract(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	b.SetupGCP = "/src/substrate-gke/tools/setup-gcp"
	spec := b.Bootstrap(testSetup(t))

	for _, want := range []string{
		"PROJECT_ID=acme",
		"PROJECT_NUMBER=42",
		"CLUSTER_NAME=substrate-test",
		"CLUSTER_LOCATION=us-west1-c",
		"GCE_REGION=us-west1",
		"BUCKET_NAME=ate-snapshots-acme-substrate-test-us-west1-c",
		"KO_DOCKER_REPO=us-west1-docker.pkg.dev/acme/ate-images",
		"NO_DEV_ENV=1",
		"VERSION=substrate-" + ShortCommit(),
	} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("Bootstrap env missing %q", want)
		}
	}
	// setup-gcp runs from this repository, not from the Substrate checkout,
	// but only after the checkout is fetched.
	script := spec.Argv[2]
	run := "go -C '/src/substrate-gke/tools/setup-gcp' run . bootstrap"
	if !strings.Contains(script, run) {
		t.Errorf("unexpected argv: %v", spec.Argv)
	}
	if strings.Contains(script, "./tools/setup-gcp") {
		t.Errorf("Bootstrap must not run the checkout's tools/setup-gcp: %v", spec.Argv)
	}
	if strings.Index(script, `cd "${SUBSTRATE_DIR}"`) > strings.Index(script, run) {
		t.Errorf("Bootstrap must fetch the checkout before running setup-gcp: %v", spec.Argv)
	}
}

// writeMainModule creates dir as a Go main module with the given module path.
func writeMainModule(t *testing.T, dir, module string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":  "module " + module + "\n\ngo 1.26.4\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindSetupGCP(t *testing.T) {
	repo := t.TempDir()
	tool := filepath.Join(repo, SetupGCPPath)
	writeMainModule(t, tool, "github.com/ai-on-gke/substrate-gke/tools/setup-gcp")
	installer := filepath.Join(repo, "installer")
	if err := os.MkdirAll(installer, 0o755); err != nil {
		t.Fatal(err)
	}

	// From installer/ (go run . / make run) and from bin/ (make build), with
	// an unrelated first start such as a go-build temp dir.
	for _, starts := range [][]string{
		{installer},
		{t.TempDir(), filepath.Join(repo, "bin")},
	} {
		got, err := FindSetupGCP("", starts...)
		if err != nil || got != tool {
			t.Errorf("FindSetupGCP(%v) = %q, %v; want %q", starts, got, err, tool)
		}
	}

	if got, err := FindSetupGCP(tool); err != nil || got != tool {
		t.Errorf("explicit FindSetupGCP = %q, %v; want %q", got, err, tool)
	}
	if _, err := FindSetupGCP(installer); err == nil {
		t.Error("explicit path without the module must fail")
	}
	if _, err := FindSetupGCP("", t.TempDir()); err == nil {
		t.Error("search outside a checkout must fail")
	}

	// This repository's own copy must pass the check, from this package's
	// directory just as from installer/.
	real, err := filepath.Abs(filepath.Join("..", "..", "..", SetupGCPPath))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := FindSetupGCP("", "."); err != nil || got != real {
		t.Errorf("FindSetupGCP from this repository = %q, %v; want %q", got, err, real)
	}
}

// Review question: does FindSetupGCP accept any Go main module? installer/
// also has a go.mod and a main.go, and Bootstrap would then run
// `go -C installer run . bootstrap`, relaunching the installer instead of
// provisioning. Only the setup-gcp module may pass, explicitly or found by
// the upward search.
func TestFindSetupGCPRejectsOtherMainModules(t *testing.T) {
	repo := t.TempDir()
	installer := filepath.Join(repo, "installer")
	writeMainModule(t, installer, "github.com/ai-on-gke/substrate-gke/installer")
	if got, err := FindSetupGCP(installer); err == nil {
		t.Errorf("FindSetupGCP(%q) = %q, want an error: it is the installer module, not setup-gcp", installer, got)
	}

	// A directory at the setup-gcp path holding some other main module.
	impostor := filepath.Join(t.TempDir(), "checkout")
	writeMainModule(t, filepath.Join(impostor, SetupGCPPath), "example.com/other")
	if got, err := FindSetupGCP("", impostor); err == nil {
		t.Errorf("FindSetupGCP search accepted %q, which holds module example.com/other", got)
	}
}

// The module line may use a tab separator, quote the path, or carry a
// trailing comment; only the path itself decides whether it is setup-gcp.
func TestFindSetupGCPModuleLineForms(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"module github.com/x/tools/setup-gcp", true},
		{"module\tgithub.com/x/tools/setup-gcp", true},
		{`module "github.com/x/tools/setup-gcp"`, true},
		{"module github.com/x/tools/setup-gcp // fork", true},
		{"module github.com/x/installer // tools/setup-gcp", false},
		{"module github.com/x/tools/setup-gcp-extra", false},
	}
	for _, tt := range tests {
		dir := t.TempDir()
		files := map[string]string{
			"go.mod":  tt.line + "\n\ngo 1.26.4\n",
			"main.go": "package main\n\nfunc main() {}\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, err := FindSetupGCP(dir)
		if got := err == nil; got != tt.want {
			t.Errorf("FindSetupGCP with go.mod line %q accepted = %v, want %v (err: %v)", tt.line, got, tt.want, err)
		}
	}
}

// Review question: does Bootstrap still export the deprecated
// GVISOR_NODE_MACHINE_TYPE? setup-gcp now lives in this repository and reads
// NODE_MACHINE_TYPE, warning on every run that sees only the old name. No
// other consumer of the environment reads the old name: upstream ate-setup
// on release-0.2 and main does not reference either variable.
func TestBootstrapEnvUsesNodeMachineType(t *testing.T) {
	st := testSetup(t)
	st.MachineType = "c3-standard-8"
	spec := NewBuilder("/tmp/substrate-pin", true).Bootstrap(st)
	if !slices.Contains(spec.Env, "NODE_MACHINE_TYPE=c3-standard-8") {
		t.Errorf("Bootstrap env lacks NODE_MACHINE_TYPE=c3-standard-8: %v", spec.Env)
	}
	for _, kv := range spec.Env {
		if strings.HasPrefix(kv, "GVISOR_NODE_MACHINE_TYPE=") {
			t.Errorf("Bootstrap env still exports the deprecated %s", kv)
		}
	}
}

// Review question: is Bootstrap's Display a command that works where it is
// shown? The step runs from the Substrate checkout, so a Display relative to
// this repository names the checkout's own (stale or missing) copy if
// replayed there. Display must name the resolved setup-gcp directory, and
// running it must not depend on the working directory.
func TestBootstrapDisplayNamesTheResolvedSetupGCP(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	b.SetupGCP = "/home/dev/substrate-gke/tools/setup-gcp"
	spec := b.Bootstrap(testSetup(t))
	if !strings.Contains(spec.Display, b.SetupGCP) {
		t.Errorf("Bootstrap Display %q does not name the resolved setup-gcp directory %q", spec.Display, b.SetupGCP)
	}
	if strings.Contains(spec.Display, "-C "+SetupGCPPath) || strings.Contains(spec.Display, "./"+SetupGCPPath) {
		t.Errorf("Bootstrap Display %q is relative to this repository, but the step runs in the Substrate checkout", spec.Display)
	}
}

// Bootstrap compares an existing cluster's network, subnetwork and Dataplane
// V2 setting with the ones it is handed and warns on each difference, and the
// defaults match few real clusters: GKE now gives many their own
// gke-<name>-subnet-<hash> subnet. So an existing cluster is handed its own
// values, and ENABLE_DATAPLANE_V2 is explicit either way, since bootstrap
// registers no flag for it and otherwise expects true. A new cluster still
// gets the user's choices, not an abandoned selection's.
func TestBootstrapIsHandedAnExistingClustersOwnNetwork(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)

	existing := testSetup(t)
	existing.ClusterIsNew = false
	existing.ClusterNetwork = "default"
	existing.ClusterSubnetwork = "gke-prod-subnet-55edbf1e"
	existing.ClusterDataplaneV2 = false
	env := b.Bootstrap(existing).Env
	for _, want := range []string{"NETWORK=default", "SUBNETWORK=gke-prod-subnet-55edbf1e", "ENABLE_DATAPLANE_V2=false"} {
		if !slices.Contains(env, want) {
			t.Errorf("existing cluster: bootstrap env missing %q", want)
		}
	}

	created := testSetup(t)
	created.ClusterIsNew = true
	created.Subnetwork = "my-subnet"
	created.ClusterSubnetwork = "left-over-from-an-abandoned-selection"
	env = b.Bootstrap(created).Env
	for _, want := range []string{"SUBNETWORK=my-subnet", "ENABLE_DATAPLANE_V2=true"} {
		if !slices.Contains(env, want) {
			t.Errorf("new cluster: bootstrap env missing %q", want)
		}
	}
}

// Bootstrap reads the target release and channel only from the environment.
// Drop the version and a new cluster gets its channel's default, which on
// Regular is below the floor the installer enforces; drop the channel and it
// lands on Regular, which does not offer the default 1.37 at all.
func TestBuilderEnvCarriesTheClusterVersionAndChannel(t *testing.T) {
	st := testSetup(t)
	spec := NewBuilder("/tmp/substrate-pin", true).Bootstrap(st)

	// Both, always: without the channel a new cluster lands on Regular,
	// which does not offer the default 1.37.
	for _, want := range []string{"CLUSTER_VERSION=" + st.ClusterVersion, "RELEASE_CHANNEL=" + st.ReleaseChannel} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("Bootstrap env missing %q: %v", want, spec.Env)
		}
	}
	if st.ReleaseChannel != state.DefaultReleaseChannel {
		t.Errorf("test setup channel = %q, want the default %q", st.ReleaseChannel, state.DefaultReleaseChannel)
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
	if demo.Display != "go run ./cmd/ate-setup deploy demo counter" || demo.DisplayIsSummary {
		t.Errorf("demo display = %q (summary %v), want the command itself", demo.Display, demo.DisplayIsSummary)
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

// A digest-qualified tag is a valid --image-tag upstream, but only the tag part
// of it is a valid version label, so VERSION must be cut at the "@".
func TestPrebuiltVersionDropsADigestQualifiedTag(t *testing.T) {
	st := prebuiltSetup(t)
	st.ImageTag = "v0.1.0@sha256:" + strings.Repeat("ab", 32)

	env := NewBuilder("/tmp/substrate-pin", true).DeployAteSystem(st).Env
	if want := "VERSION=v0.1.0"; !slices.Contains(env, want) {
		t.Errorf("pre-built env missing %q: %v", want, env)
	}
}

// ate-setup refuses to deploy the control plane without a credential provider,
// so the deploy passes the bundled one unless the caller chose another.
func TestDeployAteSystemCredentialProvider(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)

	t.Setenv("ATE_CREDENTIAL_PROVIDER", "")
	env := b.DeployAteSystem(prebuiltSetup(t)).Env
	if want := `ATE_CREDENTIAL_PROVIDER={"name":"k8s.io"}`; !slices.Contains(env, want) {
		t.Errorf("deploy env missing %q: %v", want, env)
	}

	t.Setenv("ATE_CREDENTIAL_PROVIDER", `{"enabled":false}`)
	for _, e := range b.DeployAteSystem(prebuiltSetup(t)).Env {
		if strings.HasPrefix(e, "ATE_CREDENTIAL_PROVIDER=") {
			t.Errorf("deploy env overrides the caller's ATE_CREDENTIAL_PROVIDER with %q", e)
		}
	}
}

// A registry refuses a tag outside the image tag grammar. Catching it at the
// prompt beats finding out once the cluster is half installed.
func TestCheckImageTag(t *testing.T) {
	ok := []string{
		testImageTag,
		"v0.1.0@sha256:" + strings.Repeat("ab", 32), // the digest is not part of the version
		"40ca1ce6",
		"1",
		"release_2026.09.02-rc1",
		"v0.1.0-",
		"_v1",
		strings.Repeat("v", 128),
	}
	for _, tag := range ok {
		if err := CheckImageTag(tag); err != nil {
			t.Errorf("CheckImageTag(%q) = %v, want nil", tag, err)
		}
	}
	bad := []string{
		"",
		"-v0.1.0",                // cannot begin with '-'
		".v1",                    // or '.'
		"my/team:v1",             // a registry path is not a tag
		"v0.1.0 ",                // trailing space
		"héllo",                  // not ASCII alphanumeric
		strings.Repeat("v", 129), // one over the tag limit
	}
	for _, tag := range bad {
		if err := CheckImageTag(tag); err == nil {
			t.Errorf("CheckImageTag(%q) = nil, want an error", tag)
		}
	}
}

// prebuiltSetup is testSetup after the images step chose a published release.
func prebuiltSetup(t *testing.T) *state.Setup {
	t.Helper()
	st := state.NewSetup()
	st.ProjectID = "acme"
	st.ProjectNumber = "42"
	st.ImageRepo, st.ImageTag = testImageRepo, testImageTag
	if err := st.ApplyProjectDefaults(); err != nil {
		t.Fatalf("ApplyProjectDefaults: %v", err)
	}
	return st
}

// A pre-built install builds nothing and pushes nothing, so it must not carry
// a registry to push to. VERSION is left out for a subtler reason: ate-setup
// takes the Substrate version from the image tag, and that tag is what the
// atelet actually is — a checkout's commit stamp would overwrite it with a lie.
func TestPrebuiltEnvDropsTheBuildVariables(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	b.UseSource(Revision{Repo: RepoURL, Commit: Commit})
	env := b.DeployAteSystem(prebuiltSetup(t)).Env

	for _, unwanted := range []string{"KO_DOCKER_REPO", "KO_DEFAULTPLATFORMS"} {
		for _, e := range env {
			if strings.HasPrefix(e, unwanted+"=") {
				t.Errorf("pre-built env must not set %s: %v", unwanted, env)
			}
		}
	}
	// VERSION names the atelet DaemonSet and labels the nodes, so it has to be
	// the tag the images came from — not b.Version, which describes the
	// checkout, and not whatever the caller's shell happens to export.
	if want := "VERSION=" + testImageTag; !slices.Contains(env, want) {
		t.Errorf("pre-built env missing %q: %v", want, env)
	}
	// The rest of the dev-env contract still applies: the checkout is fetched
	// either way, because ate-setup reads the manifests from source.
	for _, want := range []string{"PROJECT_ID=acme", "CLUSTER_NAME=substrate-test", "NO_DEV_ENV=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("pre-built env missing %q: %v", want, env)
		}
	}
}

// The image flags are what actually point ate-setup at the published images,
// and they are persistent flags, so the demo deploy needs them too.
func TestPrebuiltDeployPassesTheImageFlags(t *testing.T) {
	b := NewBuilder("/tmp/substrate-pin", true)
	st := prebuiltSetup(t)

	// A tag is user input, so the script quotes it; Display is prose the user
	// reads, so it does not.
	wantArgv := " --image-repo " + ShellQuote(testImageRepo) + " --image-tag " + ShellQuote(testImageTag)
	wantDisplay := " --image-repo " + testImageRepo + " --image-tag " + testImageTag
	for _, spec := range []execx.Spec{b.DeployAteSystem(st), b.DeployDemo(st, "counter")} {
		if !strings.Contains(spec.Argv[2], wantArgv) {
			t.Errorf("%s argv missing the image flags:\n%s", spec.Label, spec.Argv[2])
		}
		if !strings.Contains(spec.Display, wantDisplay) {
			t.Errorf("%s display missing the image flags: %q", spec.Label, spec.Display)
		}
		if err := exec.Command("bash", "-n", "-c", spec.Argv[2]).Run(); err != nil {
			t.Errorf("%s script is not valid shell: %v", spec.Label, err)
		}
	}
	// A build from source must not pass them; ate-setup would then skip the
	// build it was asked for.
	if strings.Contains(b.DeployAteSystem(testSetup(t)).Argv[2], "--image-repo") {
		t.Error("a source build must not pass --image-repo")
	}
}

// Two revisions must never share a cache directory or a ko version stamp, or
// one build's images get installed as another's.
func TestUseSourceMovesTheManagedTree(t *testing.T) {
	const fork = "https://github.com/annapendleton/substrate.git"
	const sha = "0123456789abcdef0123456789abcdef01234567"

	b := NewBuilder(filepath.Join(t.TempDir(), treePrefix+ShortCommit()), true)
	before := b.Root
	b.UseSource(Revision{Repo: fork, Commit: sha})

	if b.Root == before {
		t.Errorf("Root did not follow the revision: %s", b.Root)
	}
	if !strings.HasSuffix(b.Root, treePrefix+shorten(sha)) {
		t.Errorf("Root = %q, want a tree named for %s", b.Root, shorten(sha))
	}
	if b.Version != "substrate-"+shorten(sha) {
		t.Errorf("Version = %q, want the new commit's stamp", b.Version)
	}
	script := b.Bootstrap(testSetup(t)).Argv[2]
	if !strings.Contains(script, "fetch -q --depth 1 "+ShellQuote(fork)+" "+sha) {
		t.Errorf("fetch does not name the chosen fork and commit:\n%s", script)
	}
	// A URL is user input now, so it reaches bash as one word whatever is in it.
	if err := exec.Command("bash", "-n", "-c", script).Run(); err != nil {
		t.Errorf("fetch script is not valid shell: %v\n%s", err, script)
	}
}

// A checkout the user passed with --substrate-root is theirs: the revision is
// still recorded for the standalone teardown, but nothing moves on disk.
func TestUseSourceLeavesAnExplicitCheckoutInPlace(t *testing.T) {
	root := fakeCheckout(t)
	b := NewBuilder(root, false)
	b.UseSource(Revision{Repo: "https://example.com/substrate.git", Commit: strings.Repeat("a", 40)})

	if b.Root != root {
		t.Errorf("Root = %q, want the user's checkout %q", b.Root, root)
	}
	if b.Version != "substrate-local" {
		t.Errorf("Version = %q, want substrate-local", b.Version)
	}
}

// The upgrade's one command fetches the installed commit and the new one into
// directories that stay, reuses a tree it already fetched whole, and then
// records the new version (best effort: no cluster here, so it only warns).
func TestFetchTreesFetchesBothCommits(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", origin}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(origin, "go.mod"), []byte("module example.com/substrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "go.mod")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "old")
	oldSHA := run("rev-parse", "HEAD")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "new")
	newSHA := run("rev-parse", "HEAD")

	b := NewBuilder(filepath.Join(t.TempDir(), "cache", "substrate-x"), true)
	b.UseSource(Revision{Repo: origin, Commit: newSHA})
	st := testSetup(t)
	st.InstalledCommit, st.InstalledVersion = oldSHA, "substrate-"+shorten(oldSHA)

	// A path with a space and a $ in it: the script must quote it
	// everywhere, messages included, or set -u aborts on the expansion.
	base := filepath.Join(t.TempDir(), "up grades $HOME_x")
	installedDir, nextDir := b.UpgradeTrees(base, st)
	if installedDir == nextDir || !strings.HasPrefix(installedDir, base) {
		t.Fatalf("UpgradeTrees = %q, %q", installedDir, nextDir)
	}
	spec := b.FetchTrees(st, installedDir, nextDir)
	if script := spec.Argv[len(spec.Argv)-1]; strings.Contains(script, "kubectl") || strings.Contains(script, "gcloud") {
		t.Fatalf("FetchTrees must not touch the cluster:\n%s", script)
	}
	// A tree left half-fetched, without its marker, is replaced whole.
	if err := os.MkdirAll(installedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installedDir, "leftover"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out, err := exec.Command(spec.Argv[0], spec.Argv[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("fetch run %d: %v\n%s", i, err, out)
		}
		if i == 1 && !strings.Contains(string(out), "Using cached") {
			t.Errorf("second run should reuse the trees:\n%s", out)
		}
		if !strings.Contains(string(out), " "+installedDir) {
			t.Errorf("run %d should print the tree's path as it is:\n%s", i, out)
		}
	}
	for dir, want := range map[string]string{installedDir: oldSHA, nextDir: newSHA} {
		got, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil || strings.TrimSpace(string(got)) != want {
			t.Errorf("%s is at %q (%v), want %s", dir, strings.TrimSpace(string(got)), err, want)
		}
		// The marker must not count as a change: Go would stamp a binary
		// built here as dirty.
		if status, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); err != nil || len(status) != 0 {
			t.Errorf("%s is not clean after the fetch (%v):\n%s", dir, err, status)
		}
	}
	if _, err := os.Stat(filepath.Join(installedDir, "leftover")); err == nil {
		t.Errorf("a marker-less tree should have been replaced, not fetched into")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), stageInfix) {
			t.Errorf("staging directory left behind: %s", e.Name())
		}
	}
	// The installed commit was read off the cluster, not resolved against
	// the repository; one it cannot serve has to say so, not die in git.
	st.InstalledCommit = "0123456789abcdef0123456789abcdef01234567"
	missingDir, _ := b.UpgradeTrees(base, st)
	missing := b.FetchTrees(st, missingDir, nextDir)
	if out, err := exec.Command(missing.Argv[0], missing.Argv[1:]...).CombinedOutput(); err == nil || !strings.Contains(string(out), "is not in "+origin) {
		t.Errorf("a commit the repository cannot serve should be reported (%v):\n%s", err, out)
	}
	if _, err := os.Stat(missingDir); err == nil {
		t.Errorf("a failed fetch should leave no tree at %s", missingDir)
	}
	// Setting up ~/ate-upgrade lays it out the way the runbook's own block
	// does, from the trees just fetched and the cluster's record.
	home, records := t.TempDir(), t.TempDir()
	t.Setenv("ATE_RECORD_DIR", records)
	record, err := RecordPath(st)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(records, "installs", "gke_acme_"+st.Zone+"_"+st.ClusterName+".yaml"); record != want {
		t.Fatalf("RecordPath = %q, want %q", record, want)
	}
	prepare := b.PrepareUpgrade(st, installedDir, nextDir)
	if out, err := runPrepare(t, prepare, home); err == nil || !strings.Contains(out, "no ate-setup record") || !strings.Contains(out, record) {
		t.Errorf("a missing record should stop the set-up (%v):\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(home, "ate-upgrade")); err == nil {
		t.Errorf("a set-up without a record should leave no ~/ate-upgrade")
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatal(err)
	}
	// A record that names no bucket cannot grant the transfer account on one.
	if err := os.WriteFile(record, []byte("cluster:\n  substrateVersion: "+st.InstalledVersion+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPrepare(t, prepare, home); err == nil || !strings.Contains(out, "names no snapshot bucket") {
		t.Errorf("a record without a bucket should stop the set-up (%v):\n%s", err, out)
	}
	if err := os.WriteFile(record, []byte("cluster:\n  substrateVersion: "+st.InstalledVersion+"\nstorage:\n  bucketName: ate-snapshots-acme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// It creates the service account for the bucket the record names, which
	// the upgrade track never asks for, and installs the installed release's
	// kubectl-ate.
	if out, err := runPrepare(t, prepare, home); err != nil {
		t.Fatalf("set-up: %v\n%s", err, out)
	}
	if got, _ := os.Readlink(filepath.Join(home, "ate-upgrade", "old")); got != installedDir {
		t.Errorf("~/ate-upgrade/old links to %q, want %q", got, installedDir)
	}
	if got, _ := os.Readlink(filepath.Join(home, "ate-upgrade", "new")); got != nextDir {
		t.Errorf("~/ate-upgrade/new links to %q, want %q", got, nextDir)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "ate-upgrade", "old.yaml")); !strings.Contains(string(got), "bucketName: ate-snapshots-acme") {
		t.Errorf("old.yaml is not the record:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "go-args")); !strings.Contains(string(got), "-C\n"+installedDir+"\ninstall\n./cmd/kubectl-ate") || !strings.HasSuffix(string(got), "--bucket\nate-snapshots-acme\n") {
		t.Errorf("go ran with:\n%s", got)
	}
	// A second set-up must not replace the first: it may hold what that
	// upgrade's rollback needs.
	if out, err := runPrepare(t, prepare, home); err == nil || !strings.Contains(out, "left from an earlier upgrade") {
		t.Errorf("a second set-up should refuse (%v):\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, "ate-upgrade", "old.yaml")); err != nil {
		t.Errorf("the refused set-up touched the first one: %v", err)
	}
	// The hand-over line moves a shell to the new tree with the version a
	// build from source needs.
	line := b.UpgradeShellLine(st)
	out, err := exec.Command("bash", "-c", "export HOME="+ShellQuote(home)+"\n"+line+"\nprintf '%s|%s' \"$PWD\" \"$VERSION\"").Output()
	if want := filepath.Join(home, "ate-upgrade", "new") + "|" + b.SubstrateVersion(st); err != nil || string(out) != want {
		t.Errorf("the hand-over line gave %q, %v; want %q", out, err, want)
	}
	if notes := strings.Join(b.UpgradeNotes(st), "\n"); !strings.Contains(notes, "export VERSION="+st.InstalledVersion+" there") {
		t.Errorf("a build from source needs the rollback version:\n%s", notes)
	}
	if summary := b.UpgradeSummary(st); !strings.Contains(summary, RunbookURL) || !strings.Contains(summary, line) {
		t.Errorf("UpgradeSummary lacks the runbook or the line:\n%s", summary)
	}
}

// runPrepare runs PrepareUpgrade's script with home as HOME, and with gcloud
// and go stubbed: go records its arguments in ~/go-args.
func runPrepare(t *testing.T, spec execx.Spec, home string) (string, error) {
	t.Helper()
	bin := t.TempDir()
	for name, body := range map[string]string{
		"gcloud": "#!/bin/sh\n",
		"go":     "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$HOME/go-args\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Pre-built images take their version from the tag in the record, so the
// set-up moves new.yaml to the new registry and tag, and the hand-over sets
// the release the runbook builds worker images from and clears any VERSION
// that would override the tag; a rollback needs nothing from the shell.
func TestUpgradeSummaryForPrebuiltImages(t *testing.T) {
	records, home := t.TempDir(), t.TempDir()
	t.Setenv("ATE_RECORD_DIR", records)
	b := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	st := testSetup(t)
	st.ImageRepo, st.ImageTag = testImageRepo, "v0.6.1@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42"
	st.InstalledVersion = "v0.6.0"
	record, err := RecordPath(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte("images:\n  repo: registry.example.com/old\n  tag: v0.6.0\nstorage:\n  bucketName: ate-snapshots-acme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPrepare(t, b.PrepareUpgrade(st, "/trees/old", "/trees/new"), home); err != nil {
		t.Fatalf("set-up: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "ate-upgrade", "new.yaml")); !strings.Contains(string(got), "  repo: "+testImageRepo+"\n  tag: v0.6.1\n") {
		t.Errorf("new.yaml should name the new registry and tag:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "ate-upgrade", "old.yaml")); !strings.Contains(string(got), "  repo: registry.example.com/old\n  tag: v0.6.0\n") {
		t.Errorf("old.yaml should stay the record:\n%s", got)
	}
	if line := b.UpgradeShellLine(st); line != "cd ~/ate-upgrade/new && export NEW_RELEASE=v0.6.1 && unset VERSION" {
		t.Errorf("pre-built hand-over line: %s", line)
	}
	if summary := b.UpgradeSummary(st); strings.Contains(summary, "export VERSION") {
		t.Errorf("a pre-built hand-over should not export a version:\n%s", summary)
	}
}

// The runbook copies the record and changes only its tag, so the new
// version has to come from the kind of images the record installs.
func TestCheckImageSourceReadsTheRecord(t *testing.T) {
	t.Setenv("ATE_RECORD_DIR", t.TempDir())
	st := testSetup(t)
	if err := CheckImageSource(st); err != nil {
		t.Errorf("without a record there is nothing to check, got %v", err)
	}
	record, err := RecordPath(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(record, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const prebuilt = "cluster:\n  substrateVersion: v0.6.0\nimages:\n  repo: registry.example.com/substrate\n  tag: v0.6.0\nko:\n  defaultPlatforms: linux/amd64\n"
	const source = "cluster:\n  substrateVersion: substrate-0123456789ab\nko:\n  dockerRepo: us-west1-docker.pkg.dev/acme/ate-images\n"
	for _, tc := range []struct {
		record       string
		nextPrebuilt bool
		ok           bool
	}{
		{prebuilt, true, true},
		{prebuilt, false, false},
		{source, false, true},
		{source, true, false},
	} {
		write(tc.record)
		st.ImageRepo, st.ImageTag = "", ""
		if tc.nextPrebuilt {
			st.ImageRepo, st.ImageTag = testImageRepo, "v0.6.1"
		}
		if err := CheckImageSource(st); (err == nil) != tc.ok {
			t.Errorf("record %q, next pre-built=%v: %v, want ok=%v", tc.record, tc.nextPrebuilt, err, tc.ok)
		}
	}
}

// The runbook's table: patch releases of one minor, and from 1.0 the next
// minor. Versions that are not releases are only compared for equality.
func TestCheckUpgrade(t *testing.T) {
	for _, tc := range []struct {
		installed, next string
		ok              bool
	}{
		{"v0.6.0", "v0.6.1", true},
		{"v0.6.1", "v0.6.5", true},
		{"v1.0.0-rc.1", "v1.0.0-rc.2", true},
		{"v1.0.0-rc.2", "v1.0.0", true},
		{"v1.0.0-rc.9", "v1.0.0-rc.10", true},
		{"v1.0.3", "v1.1.0", true},
		{"substrate-0123456789", "substrate-abcdef0123", true},
		{"substrate-0123456789", "v0.6.1", true},
		{"v0.6.0", "v0.6.0", false},
		{"substrate-0123456789", "substrate-0123456789", false},
		{"v0.5.2", "v0.6.0", false},
		{"v1.0.0", "v1.2.0", false},
		{"v1.4.0", "v2.0.0", false},
		{"v0.6.1", "v0.6.0", false},
		{"v1.1.0", "v1.0.5", false},
		{"v1.2.3", "v1.2.3-rc.1", false},
		{"v1.0.0-rc.10", "v1.0.0-rc.9", false},
	} {
		if err := CheckUpgrade(tc.installed, tc.next); (err == nil) != tc.ok {
			t.Errorf("CheckUpgrade(%s, %s) = %v, want ok=%v", tc.installed, tc.next, err, tc.ok)
		}
	}
}

// Only a build from source of a tree with the envoy-dataplane image runs
// docker. Asking anyone else for docker would block
// installs that never touch it.
func TestBuildsWithDockerOnlyForAnEnvoyBuildFromSource(t *testing.T) {
	t.Setenv("ATE_ATENET_DATAPLANE", "")
	b := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	st := testSetup(t)
	if b.BuildsWithDocker(st) {
		t.Error("the pin predates envoy-dataplane, so it must not need docker")
	}
	b.UseSource(Revision{Repo: RepoURL, Commit: strings.Repeat("a", 40), EnvoyDataplane: true})
	if !b.BuildsWithDocker(st) {
		t.Error("a source build of a tree with envoy-dataplane must need docker")
	}
	st.ImageRepo, st.ImageTag = testImageRepo, "v0.2.0"
	if b.BuildsWithDocker(st) {
		t.Error("a pre-built install must not need docker")
	}
}

// --substrate-root is built as it stands, so the tree on disk decides, not the
// revision the images step recorded.
func TestBuildsWithDockerReadsAUserSuppliedTree(t *testing.T) {
	t.Setenv("ATE_ATENET_DATAPLANE", "")
	root := fakeCheckout(t)
	b := NewBuilder(root, false)
	b.UseSource(Revision{Repo: RepoURL, Commit: strings.Repeat("a", 40), EnvoyDataplane: true})
	st := testSetup(t)
	if b.BuildsWithDocker(st) {
		t.Error("a tree without the Dockerfile must not need docker")
	}
	path := filepath.Join(root, EnvoyDockerfile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !b.BuildsWithDocker(st) {
		t.Error("a tree with the Dockerfile must need docker")
	}
}

func TestArtifactRegistryBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name, override, registry  string
		managed, prebuilt, create bool
	}{
		{"default source registry", "", "europe-west4-docker.pkg.dev/acme/build-images", true, false, true},
		{"custom source registry", "registry.example.com/shared", "registry.example.com/shared", true, false, false},
		{"prebuilt images", "", "", true, true, false},
		{"local source checkout", "", "europe-west4-docker.pkg.dev/acme/build-images", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CREATE_ARTIFACT_REPOSITORY", fmt.Sprint(!tc.create))
			st := testSetup(t)
			st.Zone, st.ArtifactRegistryRepository = "europe-west4-a", "build-images"
			st.KoDockerRepo = tc.override
			if tc.prebuilt {
				st.ImageRepo, st.ImageTag = testImageRepo, testImageTag
			}
			b := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), tc.managed)
			b.SetupGCP = "/src/substrate-gke/tools/setup-gcp"
			b.UseSource(Revision{Repo: RepoURL, Commit: strings.Repeat("a", 40)})
			spec := b.Bootstrap(st)
			for _, want := range []string{"CREATE_ARTIFACT_REPOSITORY=" + fmt.Sprint(tc.create), "ARTIFACT_REGISTRY_REPOSITORY=build-images", "GCE_REGION=europe-west4"} {
				if !slices.Contains(spec.Env, want) {
					t.Errorf("missing %q in %v", want, spec.Env)
				}
			}
			if !tc.prebuilt && !slices.Contains(spec.Env, "KO_DOCKER_REPO="+tc.registry) {
				t.Errorf("build must use %q", tc.registry)
			}
			steps := 8
			if tc.create {
				steps++
			}
			if slices.Contains(spec.SimLines, "Step 2/9: Creating Artifact Registry repository...") != tc.create || !slices.Contains(spec.SimLines, fmt.Sprintf("Step %d/%d: Creating Monitoring Dashboards...", steps, steps)) {
				t.Errorf("unexpected bootstrap output: %v", spec.SimLines)
			}
			if strings.Contains(b.CleanupCommand(st), "--delete-repository") != tc.create {
				t.Errorf("cleanup does not match repository creation: %s", b.CleanupCommand(st))
			}
		})
	}
}

func TestCleanupCommandRepositoryOptions(t *testing.T) {
	b := NewBuilder(t.TempDir(), true)
	for _, tc := range []struct{ name, repository, registry, imageRepo, suffix string }{
		{"default repository", "", "", "", " --delete-repository"},
		{"explicit default", "ate-images", "", "", " --delete-repository"},
		{"custom repository", "build-images", "", "", " --repository 'build-images' --delete-repository"},
		{"custom registry", "build-images", "registry.example.com/shared", "", ""},
		{"prebuilt images", "build-images", "", testImageRepo, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testSetup(t)
			st.Zone = "europe-west4-a"
			st.ArtifactRegistryRepository, st.KoDockerRepo, st.ImageRepo = tc.repository, tc.registry, tc.imageRepo
			want := CleanupCommand(st.ProjectID, st.ClusterName, st.Zone, st.BucketName) + tc.suffix
			if got := b.CleanupCommand(st); got != want {
				t.Errorf("cleanup = %q, want %q", got, want)
			}
		})
	}
}

// The fetch row names the revision the run uses: the pin, a commit the images
// step chose instead, or a tree the user supplied, which is not fetched.
func TestFetchLabelNamesTheRevision(t *testing.T) {
	pinned := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	if got, want := pinned.FetchLabel(), "Fetch the pinned substrate checkout ("+ShortCommit()+")"; got != want {
		t.Errorf("pinned: %q, want %q", got, want)
	}
	selected := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	selected.UseSource(Revision{Repo: RepoURL, Commit: strings.Repeat("b", 40)})
	if got, want := selected.FetchLabel(), "Fetch the selected substrate checkout (bbbbbbbbbbbb)"; got != want {
		t.Errorf("selected commit: %q, want %q", got, want)
	}
	fork := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	fork.UseSource(Revision{Repo: "https://github.com/someone/substrate.git", Commit: Commit})
	if got := fork.FetchLabel(); !strings.HasPrefix(got, "Fetch the selected substrate checkout") {
		t.Errorf("the pinned commit from another repository: %q, want the selected label", got)
	}
	root := t.TempDir()
	local := NewBuilder(root, false)
	if got, want := local.FetchLabel(), "Use the substrate checkout at "+root; got != want {
		t.Errorf("local tree: %q, want %q", got, want)
	}
}

// Client IDs are entered comma-separated: trimmed, empty entries and
// duplicates dropped, each a spiffe:// URI with a trust domain, and at least
// one, since ate-setup refuses an empty allowlist.
func TestParseSPIFFEIDs(t *testing.T) {
	const frontend = "spiffe://cluster.local/ns/shop/sa/frontend"
	const jobs = "spiffe://cluster.local/ns/shop/sa/jobs"
	ids, err := ParseSPIFFEIDs(" " + frontend + ", ," + jobs + "," + frontend)
	if err != nil || !slices.Equal(ids, []string{frontend, jobs}) {
		t.Errorf("ParseSPIFFEIDs = %v, %v; want %v", ids, err, []string{frontend, jobs})
	}
	for _, bad := range []string{"", " , ", "cluster.local/ns/shop/sa/frontend", "https://cluster.local/ns/a/sa/b", "spiffe:///ns/a/sa/b", "spiffe://cluster.local/ns/a/sa/b?x=1"} {
		if _, err := ParseSPIFFEIDs(bad); err == nil {
			t.Errorf("ParseSPIFFEIDs(%q) accepted it", bad)
		}
	}
}

// The control-plane deploy passes exactly the allowlist the IngressClients
// step set, whatever the caller exported: no merge.
func TestDeployCarriesTheEnteredIngressAllowlist(t *testing.T) {
	const frontend = "spiffe://cluster.local/ns/shop/sa/frontend"
	t.Setenv("ATE_INGRESS_ALLOWED_SPIFFE_IDS", "spiffe://cluster.local/ns/old/sa/default")
	st := testSetup(t)
	st.IngressAllowedSPIFFEIDs = []string{frontend}
	b := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	env := b.DeployAteSystem(st).Env
	if !slices.Contains(env, "ATE_INGRESS_ALLOWED_SPIFFE_IDS="+frontend) {
		t.Errorf("deploy env lacks the entered allowlist: %v", env)
	}
}

// The Envoy router admits only allowlisted clients, so the tour cannot curl an
// actor from the user's machine; a tree with the counter driver runs such a
// client.
func TestNextStepsFollowTheRouter(t *testing.T) {
	st := testSetup(t)
	root := fakeCheckout(t)
	b := NewBuilder(root, false)

	lead, demo := b.NextSteps(st)
	joined := strings.Join(demo, "\n")
	if lead != "" || strings.Contains(joined, "curl ") || !strings.Contains(joined, "kubectl ate create actor my-counter-1 -a ate-demo-counter --template counter") {
		t.Errorf("Envoy without a driver: lead %q, demo %v", lead, demo)
	}
	if note := b.RouterNote(st); !strings.Contains(note, "allowlist") || !strings.Contains(note, b.RouterAuthURL()) {
		t.Errorf("Envoy router note = %q", note)
	}

	if err := os.MkdirAll(filepath.Join(root, CounterDriverDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if lead, demo = b.NextSteps(st); lead != "" || !slices.Equal(demo, []string{"kubectl logs -n ate-demo-counter -l app=counter-driver -f"}) {
		t.Errorf("Envoy with the driver: lead %q, demo %v", lead, demo)
	}
	st.SandboxClass, st.MicroVMDeployed = state.SandboxMicroVM, true
	if _, demo = b.NextSteps(st); !slices.Equal(demo, []string{"kubectl logs -n ate-demo-counter-microvm -l app=counter-driver -f"}) {
		t.Errorf("micro-VM with the driver: demo %v", demo)
	}
}

// From v0.6.0 ate-setup needs a cluster-wide snapshot location. The install's
// own bucket is the one setup-gcp grants access to, so the deploy names its
// root.
func TestDeployCarriesTheSnapshotLocation(t *testing.T) {
	st := testSetup(t)
	st.BucketName = "ate-snapshots-acme-c-z"
	b := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	if env := b.DeployAteSystem(st).Env; !slices.Contains(env, "SNAPSHOT_LOCATION=gs://ate-snapshots-acme-c-z/") {
		t.Errorf("deploy env lacks the snapshot location: %v", env)
	}
}

// The control-plane deploy names the envoy dataplane, whatever the caller
// exported.
func TestDeployNamesTheEnvoyDataplane(t *testing.T) {
	t.Setenv("ATE_ATENET_DATAPLANE", "agentgateway")
	st := testSetup(t)
	b := NewBuilder(filepath.Join(t.TempDir(), "substrate-x"), true)
	if env := b.DeployAteSystem(st).Env; !slices.Contains(env, "ATE_ATENET_DATAPLANE=envoy") {
		t.Errorf("deploy env lacks ATE_ATENET_DATAPLANE=envoy: %v", env)
	}
}
