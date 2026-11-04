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
	"regexp"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// fakeKubectl puts a kubectl on PATH that answers the probes' queries for a
// cluster that runs Substrate: the namespace, the atelet DaemonSet, the API
// server Deployment's image, and the API server's --version. gcloud succeeds
// without touching anything.
func fakeKubectl(t *testing.T, image, build string) []string {
	t.Helper()
	bin := t.TempDir()
	kubectl := `#!/bin/sh
case "$*" in
  *"get namespace ate-system"*) echo ate-system ;;
  *"get daemonset atelet"*) echo daemonset.apps/atelet ;;
  *"get deployment ate-api-server"*) echo '` + image + `' ;;
  *"--version"*) echo '` + build + `' ;;
  *) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
`
	for name, script := range map[string]string{"kubectl": kubectl, "gcloud": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The version and commit come from the running binary, which ko stamps with
// the version and Go with the vcs.revision of the tree it was built from. A
// tree with an untracked file in it reports dirty, which does not change the
// commit; a binary that was not built from a checkout reports none, and both
// are then typed in.
func TestParseProbeReadsTheRunningBinary(t *testing.T) {
	const full = "5f0ef402d9c4dfab84bdf9ebef1ed762168f9c9c"
	p := ParseProbe([]string{"Fetching cluster endpoint and auth data.",
		buildMarker + "substrate-5f0ef402d9c4 commit=" + full + "-dirty built=2026-09-03T02:29:02Z linux/amd64"})
	if p.Commit != full || p.Version != "substrate-5f0ef402d9c4" {
		t.Fatalf("ParseProbe(dirty build) = %+v", p)
	}
	st := state.NewSetup()
	p.Apply(st)
	if st.InstalledCommit != full || st.InstalledVersion != "substrate-5f0ef402d9c4" {
		t.Errorf("Apply recorded %q / %q", st.InstalledVersion, st.InstalledCommit)
	}
	if unknown := ParseProbe([]string{buildMarker + "v0.1.0 commit=unknown built=unknown linux/amd64"}); unknown != (Probe{}) {
		t.Errorf("ParseProbe(unknown commit) = %+v; want nothing", unknown)
	}
	if blank := ParseProbe([]string{buildMarker}); blank != (Probe{}) {
		t.Errorf("ParseProbe(no report) = %+v; want nothing", blank)
	}
	// The dry run replays a probe of the pin, commit included.
	st = testSetup(t)
	if p := ParseProbe(ProbeCluster(st, false).SimLines); p.Version != "substrate-"+ShortCommit() || p.Commit != Commit {
		t.Errorf("dry-run probe = %+v", p)
	}
	for _, credentials := range []bool{true, false} {
		spec := ProbeCluster(st, credentials)
		if err := exec.Command("bash", "-n", "-c", spec.Argv[len(spec.Argv)-1]).Run(); err != nil {
			t.Errorf("ProbeCluster(credentials=%v) is not valid shell: %v", credentials, err)
		}
	}
	// End to end against a cluster that runs Substrate.
	spec := ProbeCluster(st, false)
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = fakeKubectl(t, "", "v0.6.0 commit="+full+" built=2026-10-22T00:00:00Z linux/amd64")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ProbeCluster: %v\n%s", err, out)
	}
	if p := ParseProbe(strings.Split(string(out), "\n")); p.Version != "v0.6.0" || p.Commit != full {
		t.Errorf("ProbeCluster read %+v from:\n%s", p, out)
	}
}

// A kubectl that cannot reach or read the cluster has to fail the read with
// its own error, not print bare markers that parse as "nothing installed".
func TestProbeClusterFailsWhenKubectlFails(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\necho 'error: You must be logged in to the server (Unauthorized)' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := ProbeCluster(testSetup(t), false)
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the read should fail with kubectl:\n%s", out)
	}
	if !strings.Contains(string(out), "Unauthorized") || strings.Contains(string(out), buildMarker) {
		t.Errorf("the read should show kubectl's error and no markers:\n%s", out)
	}
}

// A cluster without an atelet DaemonSet runs no Substrate to upgrade.
func TestProbeClusterFailsWithoutAtelet(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := ProbeCluster(testSetup(t), false)
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no atelet DaemonSet") {
		t.Errorf("the read should fail without atelet, got %v:\n%s", err, out)
	}
}

func TestCheckInstalled(t *testing.T) {
	// Clean cluster
	probe, err := ParseInstalled([]string{installedMarker + "false"})
	if err != nil || probe.Installed {
		t.Fatalf("ParseInstalled(false) = %+v, %v; want Installed: false", probe, err)
	}

	// Installed, with the API server's image
	probe, err = ParseInstalled([]string{installedMarker + "true atelet image=ghcr.io/agent-substrate/substrate/ate-api-server:v0.6.0@sha256:" + strings.Repeat("a", 64)})
	if err != nil || !probe.Installed || !probe.Atelet || probe.Partial() || probe.Version != "v0.6.0" {
		t.Fatalf("ParseInstalled(true with atelet) = %+v, %v", probe, err)
	}

	// Installed, with a Deployment that could not be read
	probe, err = ParseInstalled([]string{installedMarker + "true atelet "})
	if err != nil || !probe.Atelet || probe.Partial() || probe.Version != "" {
		t.Fatalf("ParseInstalled(true with atelet, no image) = %+v, %v", probe, err)
	}

	// Installed from source: ko tags the image latest, which names no version.
	probe, err = ParseInstalled([]string{installedMarker + "true atelet image=us-docker.pkg.dev/acme/substrate/ate-api-server:latest@sha256:" + strings.Repeat("a", 64)})
	if err != nil || !probe.Atelet || probe.Partial() || probe.Version != "" {
		t.Fatalf("ParseInstalled(true with atelet, latest) = %+v, %v", probe, err)
	}

	// Installed without atelet: an interrupted install, not a running one.
	probe, err = ParseInstalled([]string{installedMarker + "true  "})
	if err != nil || !probe.Installed || !probe.Partial() {
		t.Fatalf("ParseInstalled(true bare) = %+v, %v; want partial", probe, err)
	}

	// Missing marker
	if _, err := ParseInstalled([]string{"something else"}); err == nil {
		t.Error("ParseInstalled should fail when marker is missing")
	}

	// The dry run replays clean for ordinary names, and the guard's other
	// two states for the fixture clusters named after them.
	if p, err := ParseInstalled(CheckInstalled("acme", "substrate-test", "us-west1-c").SimLines); err != nil || p.Installed {
		t.Fatalf("dry-run check installed = %+v, %v; want not installed", p, err)
	}
	if p, err := ParseInstalled(CheckInstalled("acme", "substrate-installed", "us-west1-c").SimLines); err != nil || !p.Installed || p.Partial() || p.Version == "" {
		t.Fatalf("dry-run check of the installed fixture = %+v, %v; want installed with a version", p, err)
	}
	if p, err := ParseInstalled(CheckInstalled("acme", "substrate-partial", "us-west1-c").SimLines); err != nil || !p.Partial() {
		t.Fatalf("dry-run check of the partial fixture = %+v, %v; want partial", p, err)
	}

	spec := CheckInstalled("acme", "substrate-test", "us-west1-c")
	script := spec.Argv[len(spec.Argv)-1]
	if err := exec.Command("bash", "-n", "-c", script).Run(); err != nil {
		t.Errorf("CheckInstalled is not valid shell: %v", err)
	}
	// The probe must not rewrite the user's kubeconfig just for browsing
	// clusters, and must not swallow a failing DaemonSet query into a
	// bare-namespace answer; only the best-effort version read may fail.
	if !strings.Contains(script, `export KUBECONFIG="$kubeconfig_dir/config"`) {
		t.Errorf("probe does not isolate its kubeconfig:\n%s", script)
	}
	if strings.Count(script, "|| true") != 1 || !strings.Contains(script, imageQuery) {
		t.Errorf("probe swallows kubectl failures:\n%s", script)
	}

	// End to end: a running install reads as installed rather than partial,
	// with the version its API server image is tagged as.
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = fakeKubectl(t, "ghcr.io/agent-substrate/substrate/ate-api-server:v0.6.0@sha256:"+strings.Repeat("a", 64), "")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("CheckInstalled: %v\n%s", err, out)
	}
	if p, err := ParseInstalled(strings.Split(string(out), "\n")); err != nil || !p.Atelet || p.Partial() || p.Version != "v0.6.0" {
		t.Errorf("CheckInstalled read %+v, %v from:\n%s", p, err, out)
	}
}

// The install guard probes every ready cluster in the project, in the
// background, before the user has chosen one; in a shared project those are
// other people's clusters. Browsing a list warrants reading, nothing more:
// no exec into their pods, no logs, no port-forward, and nothing that writes.
func TestCheckInstalledOnlyReads(t *testing.T) {
	spec := CheckInstalled("acme", "substrate-test", "us-west1-c")
	script := spec.Argv[len(spec.Argv)-1]
	for _, verb := range []string{"exec", "logs", "port-forward", "attach", "cp ", "debug", "apply", "create", "delete", "patch", "edit", "replace", "scale", "rollout", "annotate", "label"} {
		if strings.Contains(script, verb) {
			t.Errorf("probe runs %q:\n%s", verb, script)
		}
	}
	kubectls := regexp.MustCompile(`kubectl(?:\s+-n\s+\S+)?\s+(\S+)`).FindAllStringSubmatch(script, -1)
	if len(kubectls) == 0 {
		t.Fatalf("probe runs no kubectl:\n%s", script)
	}
	for _, m := range kubectls {
		if m[1] != "get" {
			t.Errorf("probe runs kubectl %s, not a get:\n%s", m[1], script)
		}
	}
	gclouds := regexp.MustCompile(`gcloud\s+container\s+clusters\s+(\S+)`).FindAllStringSubmatch(script, -1)
	if len(gclouds) != 1 || gclouds[0][1] != "get-credentials" {
		t.Errorf("probe runs gcloud beyond get-credentials:\n%s", script)
	}
}

// The version is the tag of the image the API server runs. ate-setup pins a
// pre-built image's tag to VERSION, and appends the digest it resolved to;
// a registry may carry a port; ko tags a build from source latest.
func TestImageTagVersion(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/agent-substrate/substrate/ate-api-server:v1.0.0-rc.1@sha256:" + strings.Repeat("0", 64): "v1.0.0-rc.1",
		"ghcr.io/agent-substrate/substrate/ate-api-server:v1.0.0-rc.1":                                   "v1.0.0-rc.1",
		"localhost:5000/substrate/ate-api-server:v0.6.0":                                                 "v0.6.0",
		"localhost:5000/substrate/ate-api-server@sha256:" + strings.Repeat("0", 64):                      "",
		"us-docker.pkg.dev/acme/substrate/ate-api-server:latest@sha256:" + strings.Repeat("0", 64):       "",
		"ate-api-server": "",
		"":               "",
	} {
		if got := imageTagVersion(ref); got != want {
			t.Errorf("imageTagVersion(%q) = %q; want %q", ref, got, want)
		}
	}
	// The dry run's fixture names the pin.
	if got := imageTagVersion(simImage()); got != "substrate-"+ShortCommit() {
		t.Errorf("imageTagVersion(simImage()) = %q", got)
	}
}

func TestCheckInstalledFailsWhenKubectlFails(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\necho 'error: You must be logged in to the server (Unauthorized)' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// The check always fetches credentials; a gcloud that succeeds without
	// touching anything lets the script reach the failing kubectl.
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := CheckInstalled("acme", "substrate-test", "us-west1-c")
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("CheckInstalled should fail when kubectl fails:\n%s", out)
	}
	if !strings.Contains(string(out), "Unauthorized") || strings.Contains(string(out), installedMarker) {
		t.Errorf("CheckInstalled should show kubectl's error and no markers:\n%s", out)
	}
}
