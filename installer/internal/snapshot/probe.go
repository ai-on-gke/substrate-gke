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

// An installed cluster says what it runs in two places. The API server's
// Deployment names the image it runs, and a pre-built image's tag is the
// version ate-setup installed (VERSION is pinned to the tag). The binary
// itself is stamped with the version and the commit it was built from, and
// `ateapi --version` prints both; that read costs an exec into the pod, so
// only the upgrade track's read of the one cluster the user picked makes it.
// Everything else ate-setup needs for an upgrade is in the record it wrote
// when it installed the cluster.

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
// Version is the tag of the image the API server runs, empty when the
// Deployment could not be read or its tag names no version (a build from
// source is tagged latest).
type InstalledProbe struct {
	Installed bool
	Atelet    bool
	Version   string
}

// Partial reports the namespace-without-atelet state.
func (p InstalledProbe) Partial() bool { return p.Installed && !p.Atelet }

// ateletQuery is the definition both probes share, so the install guard and
// the upgrade track cannot drift apart on what "installed" looks like.
//
// The version comes from one of two reads. imageQuery reads the API server's
// image off its Deployment: a `get`, which is all the install guard may do.
// It probes every ready cluster in the project in the background, and in a
// shared project those are other people's clusters; exec'ing into their
// workloads to render a badge is more access than browsing a list warrants,
// and shows up as such in audit logs. buildQuery asks the running binary,
// which also knows its commit; the upgrade track runs it against the one
// cluster the user chose to upgrade. Both are best effort: a Deployment or
// binary the probe cannot read leaves the version unknown.
const (
	ateletQuery = `kubectl -n ate-system get daemonset atelet --ignore-not-found -o name`
	imageQuery  = `kubectl -n ate-system get deployment ate-api-server --ignore-not-found -o jsonpath='{.spec.template.spec.containers[?(@.name=="ate-api-server")].image}' 2>/dev/null || true`
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
// and if so, whether the atelet DaemonSet does and which image the API
// server runs. It takes the cluster by name rather than from Setup so the
// wizard can probe a selection before committing anything to its state.
// Every command in it reads: see imageQuery for why.
func CheckInstalled(projectID, cluster, location string) execx.Spec {
	lines := append([]string{"set -euo pipefail"}, credentialLines(projectID, cluster, location)...)
	// No fallback on the DaemonSet query: under set -e a kubectl failure
	// stops the script and surfaces as a probe error, instead of misreading
	// the cluster as a bare-namespace install.
	lines = append(lines,
		`ns=$(kubectl get namespace ate-system --ignore-not-found -o jsonpath='{.metadata.name}')`,
		`if [ -n "$ns" ]; then`,
		`  atelet=$(`+ateletQuery+`)`,
		`  image=""`,
		`  if [ -n "$atelet" ]; then image=$(`+imageQuery+`); fi`,
		fmt.Sprintf(`  echo "%strue ${atelet:+atelet} ${image:+image=$image}"`, installedMarker),
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
		sim = installedMarker + "true atelet image=" + simImage()
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

// simImage is the image a dry-run cluster's API server runs: the pin's
// images, tagged as a build of it.
func simImage() string {
	return "ghcr.io/agent-substrate/substrate/ate-api-server:substrate-" + ShortCommit() +
		"@sha256:" + strings.Repeat("0", 64)
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
		if image, ok := strings.CutPrefix(rest, "image="); ok {
			probe.Version = imageTagVersion(image)
		}
		return probe, nil
	}
	return InstalledProbe{}, fmt.Errorf("probe did not report installation status")
}

// imageTagVersion is the version an installed image's reference names: its
// tag, which ate-setup pins to VERSION for pre-built images. A reference may
// carry a digest after the tag, and a registry may carry a port, so the tag
// is what follows the last colon of the last path element. An image built
// from source by ko is tagged latest, which names no version, and a
// reference pinned by digest alone has no tag; both come back empty.
func imageTagVersion(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	name := ref[strings.LastIndex(ref, "/")+1:]
	_, tag, ok := strings.Cut(name, ":")
	if !ok || tag == "latest" {
		return ""
	}
	return tag
}

// ProbeCluster returns the command that reads whether a cluster runs
// Substrate and what its API server binary says it was built as and from.
// With credentials it first fetches a kubectl context for the cluster named
// in st; without, it uses the current context. This is the one probe that
// execs into the API server (buildQuery): the commit is only in the binary,
// and the user has just chosen this cluster to upgrade.
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
