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
	"os"
	"path/filepath"
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
//
// The install steps use kubeconfigLines instead: a probe only reads, so its
// file can go when it exits, while a deploy's KUBECONFIG ends up in
// ate-setup's record and has to outlive the step.
func credentialLines(projectID, cluster, location string) []string {
	return []string{
		`kubeconfig_dir=$(mktemp -d)`,
		`trap 'rm -rf "$kubeconfig_dir"' EXIT`,
		`export KUBECONFIG="$kubeconfig_dir/config"`,
		getCredentials(projectID, cluster, location),
	}
}

// getCredentials is the gcloud command that writes the named cluster's
// credentials into the kubeconfig $KUBECONFIG names, creating the file and
// its directory if they are missing.
func getCredentials(projectID, cluster, location string) string {
	return fmt.Sprintf("gcloud container clusters get-credentials %s --location %s --project %s >/dev/null",
		ShellQuote(cluster), ShellQuote(location), ShellQuote(projectID))
}

// KubeconfigPath is the installer's own kubeconfig for the named cluster.
// Every step that reaches the cluster — kubectl, gcloud, ate-setup and the
// scripts ate-setup runs — is pointed at it through KUBECONFIG, so the
// user's ~/.kube/config is never written and their current context never
// moves. Left to the ambient kubeconfig, ate-setup's own get-credentials
// added the cluster there and made it current, and the verify and Filestore
// steps then only worked by running against that switched context.
//
// Unlike the probes' throwaway file it is one per cluster and it stays:
// ate-setup records the KUBECONFIG it ran with in the install record, which
// the upgrade runbook's `--config <record>` runs read back, so the path has
// to still resolve then. It lives beside the install cache, which Cleanup
// sweeps only for fetched trees. The name is the context gcloud writes into
// it, the same one that names ate-setup's record.
func KubeconfigPath(projectID, cluster, location string) string {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "substrate-gke", "kubeconfig", kubeContext(projectID, cluster, location)+".yaml")
}

// kubeContext is the context name gcloud's get-credentials writes for a GKE
// cluster, and so the name of ate-setup's record of it.
func kubeContext(projectID, cluster, location string) string {
	return "gke_" + projectID + "_" + location + "_" + cluster
}

// kubeconfigLines points a step's script at KubeconfigPath and fetches the
// cluster's credentials into it. ate-setup takes a single-file KUBECONFIG
// for its own client and exports it to the gcloud and scripts it runs, so
// the get-credentials it does itself refreshes this file rather than the
// user's. The export reaches every child of the step, including the checks
// cordonLines and placementLines run in shells of their own.
func kubeconfigLines(projectID, cluster, location string) []string {
	return []string{
		"export KUBECONFIG=" + ShellQuote(KubeconfigPath(projectID, cluster, location)),
		getCredentials(projectID, cluster, location),
	}
}

// KubeconfigExport is the line that points a shell at the installer's
// kubeconfig for the cluster, for the kubectl commands the completion screen
// and the exit summary hand over.
func KubeconfigExport(st *state.Setup) string {
	return "export KUBECONFIG=" + ShellQuote(KubeconfigPath(st.ProjectID, st.ClusterName, st.Zone))
}

// GetCredentialsCommand is the gcloud command that adds the cluster to the
// user's own kubeconfig instead, which also makes it their current context:
// the exit summary offers it as the alternative to KubeconfigExport.
func GetCredentialsCommand(st *state.Setup) string {
	return fmt.Sprintf("gcloud container clusters get-credentials %s --location %s --project %s",
		ShellQuote(st.ClusterName), ShellQuote(st.Zone), ShellQuote(st.ProjectID))
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
// clusterScoped adds --cluster-scoped, which keeps what every Substrate
// cluster in the project shares — the dashboards and the snapshot transfer
// and node service accounts (see CleanupLeads).
func CleanupCommand(projectID, cluster, location, bucket string, clusterScoped bool) string {
	quoted := "<snapshot-bucket>"
	if bucket != "" {
		quoted = ShellQuote(bucket)
	}
	command := fmt.Sprintf("./tools/cleanup-gcp --project %s --cluster %s --location %s --bucket %s",
		ShellQuote(projectID), ShellQuote(cluster), ShellQuote(location), quoted)
	if clusterScoped {
		command += " --cluster-scoped"
	}
	return command
}

// CleanupLeads introduces the two GCP cleanup commands, as lines to print
// ahead of each: the cluster-scoped form first, then the complete teardown.
// They are alternatives, each a whole teardown on its own, and the copy
// numbers them so that the second does not read as a step after the first.
//
// The cluster-scoped form leads because the dashboards and the snapshot
// transfer and node service accounts are shared by every Substrate cluster
// in the project, and the installer cannot tell whether another one still
// depends on them — the provision step finding them already there is one
// sign. repository reports whether the cluster-scoped command also deletes
// the image repository this install provisioned (--delete-repository).
// filestore reports whether the Filestore step ran, so the complete
// teardown also names the Filestore CSI service account upstream's deploy.sh
// created (a run that failed may have created it too); teardown.sh deletes
// it in either case, as an account that was never created counts as
// already deleted.
func CleanupLeads(repository, filestore bool) (clusterScoped, complete []string) {
	clusterScoped = []string{
		"Option 1 — cluster-scoped (safe when other clusters share the project): delete only",
		"this cluster, its snapshot and asset buckets, and the IAM bindings on them:",
	}
	if repository {
		clusterScoped = []string{
			clusterScoped[0],
			"this cluster, its snapshot and asset buckets, the IAM bindings on them, and the",
			"image repository:",
		}
	}
	complete = []string{
		"Option 2 — complete teardown (only if no other Substrate cluster uses this project):",
		"delete this cluster and the shared resources in one go — the dashboards and the",
		"snapshot transfer and node service accounts:",
	}
	if filestore {
		complete[2] = "snapshot transfer, Filestore CSI and node service accounts:"
	}
	return clusterScoped, complete
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
