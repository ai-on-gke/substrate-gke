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

// Everything upstream's ate-setup needs to run against an installed cluster
// can be read off the cluster: the version from the atelet DaemonSet's label,
// where the images came from out of a running image reference, and the
// commit they were built from out of the running binary, which Go stamps
// with its vcs.revision. The installer keeps no record of its own; a Probe
// reads those facts, and the rest is the cluster's name.

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
	versionsMarker  = "SUBSTRATE_GKE_VERSIONS "
	imageMarker     = "SUBSTRATE_GKE_IMAGE "
	buildMarker     = "SUBSTRATE_GKE_BUILD "
	installedMarker = "SUBSTRATE_GKE_INSTALLED "
)

// InstalledProbe is what CheckInstalled found. Installed with no Versions
// means the ate-system namespace exists but no atelet DaemonSet does: an
// interrupted or partially deleted install rather than a running one.
type InstalledProbe struct {
	Installed bool
	Versions  []string
}

// Partial reports the namespace-without-atelet state.
func (p InstalledProbe) Partial() bool { return p.Installed && len(p.Versions) == 0 }

// ateletVersionsQuery reads the versions the atelet DaemonSets carry. It is
// the one definition both probes share, so the install guard and the upgrade
// track cannot drift apart on what "installed" looks like.
const ateletVersionsQuery = `kubectl -n ate-system get daemonsets -l app=atelet -o jsonpath='{range .items[*]}{.metadata.labels.ate\.dev/substrate-version} {end}'`

// credentialLines fetches kubectl credentials for the named cluster into a
// throwaway kubeconfig. Probing must not rewrite the user's ambient
// current-context just for browsing clusters — and with each probe writing
// its own file, a probe cancelled mid-gcloud cannot retarget the kubectl of
// a later probe against a different cluster.
func credentialLines(projectID, cluster, location string) []string {
	return []string{
		`export KUBECONFIG=$(mktemp)`,
		`trap 'rm -f "$KUBECONFIG"' EXIT`,
		fmt.Sprintf("gcloud container clusters get-credentials %s --location %s --project %s >/dev/null",
			ShellQuote(cluster), ShellQuote(location), ShellQuote(projectID)),
	}
}

// CheckInstalled returns the command that probes whether the named cluster
// already runs Substrate: it checks whether the ate-system namespace exists,
// and if so, what atelet versions it runs. It takes the cluster by name
// rather than from Setup so the wizard can probe a selection before
// committing anything to its state.
func CheckInstalled(projectID, cluster, location string) execx.Spec {
	lines := append([]string{"set -euo pipefail"}, credentialLines(projectID, cluster, location)...)
	// No fallback on the DaemonSet query: under set -e a kubectl failure
	// stops the script and surfaces as a probe error, instead of misreading
	// the cluster as a bare-namespace install.
	lines = append(lines,
		`ns=$(kubectl get namespace ate-system --ignore-not-found -o jsonpath='{.metadata.name}')`,
		`if [ -n "$ns" ]; then`,
		`  versions=$(`+ateletVersionsQuery+`)`,
		fmt.Sprintf(`  echo "%strue $versions"`, installedMarker),
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
		sim = installedMarker + "true substrate-" + ShortCommit()
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
		if strings.HasPrefix(line, installedMarker) {
			fields := strings.Fields(strings.TrimPrefix(line, installedMarker))
			if len(fields) > 0 && fields[0] == "true" {
				return InstalledProbe{Installed: true, Versions: fields[1:]}, nil
			}
			return InstalledProbe{Installed: false}, nil
		}
	}
	return InstalledProbe{}, fmt.Errorf("probe did not report installation status")
}

// ProbeCluster returns the command that reads a cluster's running Substrate
// versions, the image its API server runs and what that binary says it was
// built from. With credentials it first fetches a kubectl context for the
// cluster named in st; without, it uses the current context.
func ProbeCluster(st *state.Setup, credentials bool) execx.Spec {
	lines := []string{"set -euo pipefail"}
	display := "kubectl -n ate-system get daemonsets,deployments"
	if credentials {
		lines = append(lines, credentialLines(st.ProjectID, st.ClusterName, st.Zone)...)
		display = "gcloud container clusters get-credentials " + st.ClusterName + " && " + display
	}
	// Assignments rather than substitutions inside echo: a kubectl that
	// fails then stops the script with its own error, instead of printing
	// bare markers that read as "nothing installed".
	// The binary's own report is best effort: an image not built by ko
	// has it somewhere else, and the manual screen covers that.
	lines = append(lines,
		`versions=$(`+ateletVersionsQuery+`)`,
		`image=$(kubectl -n ate-system get deployment ate-api-server -o jsonpath='{.spec.template.spec.containers[0].image}')`,
		`build=$(kubectl -n ate-system exec deploy/ate-api-server -- /ko-app/ateapi --version 2>/dev/null || true)`,
		fmt.Sprintf(`echo "%s$versions"`, versionsMarker),
		fmt.Sprintf(`echo "%s$image"`, imageMarker),
		fmt.Sprintf(`echo "%s$build"`, buildMarker),
	)
	return execx.Spec{
		Label:   "read the installed Substrate",
		Display: display,
		Argv:    []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{
			versionsMarker + "substrate-" + ShortCommit(),
			imageMarker + st.Region() + "-docker.pkg.dev/" + st.ProjectID + "/ate-images/ateapi-752889f8b0bcdbee32172ac9fe056025@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42",
			buildMarker + "substrate-" + ShortCommit() + " commit=" + Commit + " built=2026-01-01T00:00:00Z linux/amd64",
		},
	}
}

// Probe is what ProbeCluster found.
type Probe struct {
	// Running lists the versions the atelet DaemonSets carry: one normally,
	// two while a rolling upgrade is under way.
	Running []string
	// BuildVersion and Commit are what the API server's binary reports it
	// was built as and from; both are empty when it could not say.
	BuildVersion string
	Commit       string
	// KoDockerRepo is the registry a build from source pushed to; empty for
	// pre-built images, which set ImageRepo and ImageTag instead.
	KoDockerRepo string
	ImageRepo    string
	ImageTag     string
}

// Prebuilt reports whether the cluster runs published images.
func (p Probe) Prebuilt() bool { return p.ImageRepo != "" }

// ko names an image after its import path plus an md5 of it; a published
// image is named after the import path alone and carries a tag, unless an
// admission policy rewrote the reference to its digest alone.
var (
	koImage       = regexp.MustCompile(`^(.+)/ateapi-[0-9a-f]{32}(@sha256:[0-9a-f]{64})?$`)
	prebuiltImage = regexp.MustCompile(`^(.+)/ateapi:([^@]+)(@sha256:[0-9a-f]{64})?$`)
	digestImage   = regexp.MustCompile(`^(.+)/ateapi@sha256:[0-9a-f]{64}$`)
	// ateapi --version prints "<version> commit=<sha>[-dirty] built=<time> <os>/<arch>".
	// A tree with an untracked file in it builds as dirty, which says nothing
	// about the commit.
	buildLine = regexp.MustCompile(`^(\S+) commit=([0-9a-f]{40})(-dirty)?(\s|$)`)
)

// ParseProbe reads what ProbeCluster printed.
func ParseProbe(lines []string) (Probe, error) {
	var p Probe
	image := ""
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, versionsMarker):
			p.Running = strings.Fields(strings.TrimPrefix(line, versionsMarker))
		case strings.HasPrefix(line, imageMarker):
			image = strings.TrimSpace(strings.TrimPrefix(line, imageMarker))
		case strings.HasPrefix(line, buildMarker):
			if m := buildLine.FindStringSubmatch(strings.TrimSpace(strings.TrimPrefix(line, buildMarker))); m != nil {
				p.BuildVersion, p.Commit = m[1], m[2]
			}
		}
	}
	if len(p.Running) == 0 {
		return Probe{}, fmt.Errorf("no atelet DaemonSet with an %s label; is Substrate installed on this cluster?", "ate.dev/substrate-version")
	}
	switch {
	case image == "":
		return Probe{}, fmt.Errorf("no ate-api-server deployment found")
	case koImage.MatchString(image):
		p.KoDockerRepo = koImage.FindStringSubmatch(image)[1]
	case prebuiltImage.MatchString(image):
		m := prebuiltImage.FindStringSubmatch(image)
		p.ImageRepo, p.ImageTag = m[1], m[2]
	case digestImage.MatchString(image):
		// The tag is gone from the reference; the running version is it.
		p.ImageRepo = digestImage.FindStringSubmatch(image)[1]
	default:
		return Probe{}, fmt.Errorf("cannot tell where image %s came from", image)
	}
	return p, nil
}

// clusterExports names the cluster to ate-setup, which fetches credentials
// for it before touching it and derives the API server's token issuer from
// it; without them it deploys to whatever the current context is.
func clusterExports(st *state.Setup) []string {
	return []string{
		"export PROJECT_ID=" + ShellQuote(st.ProjectID),
		"export CLUSTER_NAME=" + ShellQuote(st.ClusterName),
		"export CLUSTER_LOCATION=" + ShellQuote(st.Zone),
	}
}

// Exports renders the environment for running upstream's ate-setup against
// the cluster st names, at the given running version.
func (p Probe) Exports(st *state.Setup, version string) string {
	return strings.Join(append(clusterExports(st), p.versionExports(version)...), "\n")
}

// versionExports is the part of the environment that names a version: the
// version itself and where its images come from.
func (p Probe) versionExports(version string) []string {
	// ate-setup installs pre-built images whenever ATE_IMAGE_REPO is set, so
	// each block unsets the other family: the two are pasted into one shell
	// when an upgrade rolls back, and a leftover would silently deploy the
	// wrong images under the right version.
	lines := []string{"export VERSION=" + ShellQuote(version)}
	if p.Prebuilt() {
		lines = append(lines, "unset KO_DOCKER_REPO KO_DEFAULTPLATFORMS",
			"export ATE_IMAGE_REPO="+ShellQuote(p.ImageRepo), "export ATE_IMAGE_TAG="+ShellQuote(p.ImageTag))
	} else {
		lines = append(lines, "unset ATE_IMAGE_REPO ATE_IMAGE_TAG",
			"export KO_DOCKER_REPO="+ShellQuote(p.KoDockerRepo), "export KO_DEFAULTPLATFORMS='linux/amd64'")
	}
	return lines
}

// Apply records what the probe found as the installed side of an upgrade:
// the version, the commit it was built from, and the registry a build from
// source used so the new build pushes to the same place.
//
// The commit is the API server's. With two versions running it belongs to
// the one the API server reports being, so choosing the other leaves the
// commit unknown for the caller to ask for.
func (p Probe) Apply(st *state.Setup, version string) {
	st.InstalledVersion = version
	st.InstalledRepo = RepoURL
	st.InstalledCommit = ""
	if p.Commit != "" && (len(p.Running) == 1 || p.BuildVersion == version) {
		st.InstalledCommit = p.Commit
	}
	st.InstalledImageRepo, st.InstalledImageTag = p.ImageRepo, p.ImageTag
	if p.Prebuilt() && imageVersion(p.ImageTag) != version {
		// The probe read one image, the API server's. Mid-upgrade it may
		// carry the other running version, and a digest-only reference
		// carries none; the tag is the version either way.
		st.InstalledImageTag = version
	}
	st.KoDockerRepo = p.KoDockerRepo
}

// InstalledExports is what a rollback changes in the environment: the
// installed version and where its images come from. The cluster stays.
func InstalledExports(st *state.Setup) string {
	installed := Probe{KoDockerRepo: st.KoDockerRepo, ImageRepo: st.InstalledImageRepo, ImageTag: st.InstalledImageTag}
	return strings.Join(installed.versionExports(st.InstalledVersion), "\n")
}
