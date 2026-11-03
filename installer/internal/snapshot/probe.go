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
	"fmt"
	"regexp"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// An installed cluster says what it runs through its API server: Go stamps
// every binary with the version and commit it was built from, and
// `ateapi --version` prints both. Everything else ate-setup needs for an
// upgrade is in the record it wrote when it installed the cluster.

// SubstrateVersion is the version this run stamps on the cluster: the image
// tag for pre-built images, the build version for a build from source. It is
// the VERSION env() exports.
func (b *Builder) SubstrateVersion(st *state.Setup) string {
	if st.Prebuilt() {
		return imageVersion(st.ImageTag)
	}
	return b.Version
}

// The lines ProbeCluster and CheckInstalled print for ParseProbe and ParseInstalled.
const (
	buildMarker     = "SUBSTRATE_GKE_BUILD "
	installedMarker = "SUBSTRATE_GKE_INSTALLED "
)

// InstalledProbe is what CheckInstalled found. Installed without Atelet
// means the ate-system namespace exists but the atelet DaemonSet does not:
// an interrupted or partially deleted install rather than a running one.
// Version is what the API server reports, empty when it could not say.
type InstalledProbe struct {
	Installed bool
	Atelet    bool
	Version   string
}

// Partial reports the namespace-without-atelet state.
func (p InstalledProbe) Partial() bool { return p.Installed && !p.Atelet }

// ateletQuery and buildQuery are the definitions both probes share, so the
// install guard and the upgrade track cannot drift apart on what "installed"
// looks like. The build report is best effort: an image not built by ko keeps
// the binary somewhere else.
const (
	ateletQuery = `kubectl -n ate-system get daemonset atelet --ignore-not-found -o name`
	buildQuery  = `kubectl -n ate-system exec deploy/ate-api-server -- /ko-app/ateapi --version 2>/dev/null || true`
)

// credentialLines fetches kubectl credentials for the named cluster into a
// throwaway kubeconfig. Probing must not rewrite the user's ambient
// current-context just for browsing clusters — and with each probe writing
// its own file, a probe cancelled mid-gcloud cannot retarget the kubectl of
// a later probe against a different cluster. The file lives in its own
// directory, which is removed whole: gcloud warns about an empty kubeconfig
// and leaves a .backup of it next to the file.
func credentialLines(projectID, cluster, location string) []string {
	return []string{
		`kubeconfig_dir=$(mktemp -d)`,
		`trap 'rm -rf "$kubeconfig_dir"' EXIT`,
		`export KUBECONFIG="$kubeconfig_dir/config"`,
		fmt.Sprintf("gcloud container clusters get-credentials %s --location %s --project %s >/dev/null",
			ShellQuote(cluster), ShellQuote(location), ShellQuote(projectID)),
	}
}

// CheckInstalled returns the command that probes whether the named cluster
// already runs Substrate: it checks whether the ate-system namespace exists,
// and if so, whether the atelet DaemonSet does and what version the API
// server reports. It takes the cluster by name rather than from Setup so the
// wizard can probe a selection before committing anything to its state.
func CheckInstalled(projectID, cluster, location string) execx.Spec {
	lines := append([]string{"set -euo pipefail"}, credentialLines(projectID, cluster, location)...)
	// No fallback on the DaemonSet query: under set -e a kubectl failure
	// stops the script and surfaces as a probe error, instead of misreading
	// the cluster as a bare-namespace install.
	lines = append(lines,
		`ns=$(kubectl get namespace ate-system --ignore-not-found -o jsonpath='{.metadata.name}')`,
		`if [ -n "$ns" ]; then`,
		`  atelet=$(`+ateletQuery+`)`,
		`  build=""`,
		`  if [ -n "$atelet" ]; then build=$(`+buildQuery+`); fi`,
		fmt.Sprintf(`  echo "%strue ${atelet:+atelet} $build"`, installedMarker),
		`else`,
		fmt.Sprintf(`  echo "%sfalse"`, installedMarker),
		`fi`,
	)
	// The dry run keeps the guard's whole story on display: the fixture
	// clusters named for it replay an installed and a partial answer, the
	// rest come back clean.
	sim := installedMarker + "false"
	switch {
	case strings.Contains(cluster, "installed"):
		sim = installedMarker + "true atelet " + simBuild()
	case strings.Contains(cluster, "partial"):
		sim = installedMarker + "true"
	}
	return execx.Spec{
		Label:    "check for existing Substrate installation",
		Display:  "gcloud container clusters get-credentials " + cluster + " && kubectl get namespace ate-system",
		Argv:     []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{sim},
	}
}

// simBuild is what the pinned commit's API server prints for --version.
func simBuild() string {
	return "substrate-" + ShortCommit() + " commit=" + Commit + " built=2026-01-01T00:00:00Z linux/amd64"
}

// CleanupCommand renders cluster cleanup while keeping image repositories.
// An unknown bucket is left as a placeholder for the user to fill in.
func CleanupCommand(projectID, cluster, location, bucket string) string {
	quoted := "<snapshot-bucket>"
	if bucket != "" {
		quoted = ShellQuote(bucket)
	}
	return fmt.Sprintf("./tools/cleanup-gcp --project %s --cluster %s --location %s --bucket %s",
		ShellQuote(projectID), ShellQuote(cluster), ShellQuote(location), quoted)
}

// ParseInstalled reads what CheckInstalled printed.
func ParseInstalled(lines []string) (InstalledProbe, error) {
	for _, line := range lines {
		if !strings.HasPrefix(line, installedMarker) {
			continue
		}
		rest, ok := strings.CutPrefix(strings.TrimPrefix(line, installedMarker), "true")
		if !ok {
			return InstalledProbe{Installed: false}, nil
		}
		probe := InstalledProbe{Installed: true}
		rest = strings.TrimSpace(rest)
		if after, ok := strings.CutPrefix(rest, "atelet"); ok {
			probe.Atelet, rest = true, strings.TrimSpace(after)
		}
		if m := buildLine.FindStringSubmatch(rest); m != nil {
			probe.Version = m[1]
		}
		return probe, nil
	}
	return InstalledProbe{}, fmt.Errorf("probe did not report installation status")
}

// ProbeCluster returns the command that reads whether a cluster runs
// Substrate and what its API server binary says it was built as and from.
// With credentials it first fetches a kubectl context for the cluster named
// in st; without, it uses the current context.
func ProbeCluster(st *state.Setup, credentials bool) execx.Spec {
	lines := []string{"set -euo pipefail"}
	display := "kubectl -n ate-system get daemonset atelet"
	if credentials {
		lines = append(lines, credentialLines(st.ProjectID, st.ClusterName, st.Zone)...)
		display = "gcloud container clusters get-credentials " + st.ClusterName + " && " + display
	}
	// Assignments rather than substitutions inside echo: a kubectl that
	// fails then stops the script with its own error, instead of printing
	// bare markers that read as "nothing installed".
	lines = append(lines,
		`atelet=$(`+ateletQuery+`)`,
		`if [ -z "$atelet" ]; then echo "no atelet DaemonSet in ate-system; is Substrate installed on this cluster?" >&2; exit 1; fi`,
		`build=$(`+buildQuery+`)`,
		fmt.Sprintf(`echo "%s$build"`, buildMarker),
	)
	return execx.Spec{
		Label:    "read the installed Substrate",
		Display:  display,
		Argv:     []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{buildMarker + simBuild()},
	}
}

// Probe is what ProbeCluster found: what the API server's binary reports it
// was built as and from. Both are empty when it could not say.
type Probe struct {
	Version string
	Commit  string
}

// ateapi --version prints "<version> commit=<sha>[-dirty] built=<time> <os>/<arch>".
// A tree with an untracked file in it builds as dirty, which says nothing
// about the commit.
var buildLine = regexp.MustCompile(`^(\S+) commit=([0-9a-f]{40})(-dirty)?(\s|$)`)

// ParseProbe reads what ProbeCluster printed.
func ParseProbe(lines []string) Probe {
	var p Probe
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, buildMarker); ok {
			if m := buildLine.FindStringSubmatch(strings.TrimSpace(rest)); m != nil {
				p.Version, p.Commit = m[1], m[2]
			}
		}
	}
	return p
}

// Apply records what the probe found as the installed side of an upgrade.
func (p Probe) Apply(st *state.Setup) {
	st.InstalledVersion, st.InstalledCommit = p.Version, p.Commit
}
