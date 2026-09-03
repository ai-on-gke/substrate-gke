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

// The three lines ProbeCluster prints for ParseProbe.
const (
	versionsMarker = "SUBSTRATE_GKE_VERSIONS "
	imageMarker    = "SUBSTRATE_GKE_IMAGE "
	buildMarker    = "SUBSTRATE_GKE_BUILD "
)

// ProbeCluster returns the command that reads a cluster's running Substrate
// versions, the image its API server runs and what that binary says it was
// built from. With credentials it first fetches a kubectl context for the
// cluster named in st; without, it uses the current context.
func ProbeCluster(st *state.Setup, credentials bool) execx.Spec {
	lines := []string{"set -euo pipefail"}
	display := "kubectl -n ate-system get daemonsets,deployments"
	if credentials {
		lines = append(lines, fmt.Sprintf("gcloud container clusters get-credentials %s --location %s --project %s >/dev/null",
			ShellQuote(st.ClusterName), ShellQuote(st.Zone), ShellQuote(st.ProjectID)))
		display = "gcloud container clusters get-credentials " + st.ClusterName + " && " + display
	}
	// Assignments rather than substitutions inside echo: a kubectl that
	// fails then stops the script with its own error, instead of printing
	// bare markers that read as "nothing installed".
	// The binary's own report is best effort: an image not built by ko
	// has it somewhere else, and the manual screen covers that.
	lines = append(lines,
		`versions=$(kubectl -n ate-system get daemonsets -l app=atelet -o jsonpath='{range .items[*]}{.metadata.labels.ate\.dev/substrate-version} {end}')`,
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
			imageMarker + "gcr.io/" + st.ProjectID + "/ate-images/ateapi-752889f8b0bcdbee32172ac9fe056025@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42",
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
	return strings.Join(append(clusterExports(st), p.versionExports(st, version)...), "\n")
}

// versionExports is the part of the environment that names a version: the
// version itself and where its images come from.
func (p Probe) versionExports(st *state.Setup, version string) []string {
	lines := []string{"export VERSION=" + ShellQuote(version)}
	if p.Prebuilt() {
		lines = append(lines, "export ATE_IMAGE_REPO="+ShellQuote(p.ImageRepo), "export ATE_IMAGE_TAG="+ShellQuote(p.ImageTag))
	} else {
		// A build from source needs a registry to push to. A cluster that
		// ran pre-built images never had one, and one described by hand
		// names none; both get the project's default, as an install does.
		repo := p.KoDockerRepo
		if repo == "" {
			repo = st.DefaultKoDockerRepo()
		}
		lines = append(lines, "export KO_DOCKER_REPO="+ShellQuote(repo), "export KO_DEFAULTPLATFORMS='linux/amd64'")
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
	if !p.Prebuilt() {
		st.KoDockerRepo = p.KoDockerRepo
	}
}

// InstalledExports is what a rollback changes in the environment: the
// installed version and where its images come from. The cluster stays.
func InstalledExports(st *state.Setup) string {
	installed := Probe{KoDockerRepo: st.KoDockerRepo, ImageRepo: st.InstalledImageRepo, ImageTag: st.InstalledImageTag}
	return strings.Join(installed.versionExports(st, st.InstalledVersion), "\n")
}
