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
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// Everything upstream's ate-setup needs to run against an installed cluster
// can be read off the cluster: the version from the atelet DaemonSet's label,
// and where the images came from out of a running image reference. The
// installer keeps no record of its own; a Probe reads those two facts, and
// the rest is the cluster's name.

// SubstrateVersion is the version this run stamps on the cluster: the image
// tag for pre-built images, the build version for a build from source. It is
// the VERSION env() exports.
func (b *Builder) SubstrateVersion(st *state.Setup) string {
	if st.Prebuilt() {
		return imageVersion(st.ImageTag)
	}
	return b.Version
}

// The two lines ProbeCluster prints for ParseProbe.
const (
	versionsMarker = "SUBSTRATE_GKE_VERSIONS "
	imageMarker    = "SUBSTRATE_GKE_IMAGE "
)

// ProbeCluster returns the command that reads a cluster's running Substrate
// versions and the image its API server runs. With credentials it first
// fetches a kubectl context for the cluster named in st; without, it uses
// the current context.
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
	lines = append(lines,
		`versions=$(kubectl -n ate-system get daemonsets -l app=atelet -o jsonpath='{range .items[*]}{.metadata.labels.ate\.dev/substrate-version} {end}')`,
		`image=$(kubectl -n ate-system get deployment ate-api-server -o jsonpath='{.spec.template.spec.containers[0].image}')`,
		fmt.Sprintf(`echo "%s$versions"`, versionsMarker),
		fmt.Sprintf(`echo "%s$image"`, imageMarker),
	)
	return execx.Spec{
		Label:   "read the installed Substrate",
		Display: display,
		Argv:    []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{
			versionsMarker + "substrate-" + ShortCommit(),
			imageMarker + "gcr.io/" + st.ProjectID + "/ate-images/ateapi-752889f8b0bcdbee32172ac9fe056025@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42",
		},
	}
}

// Probe is what ProbeCluster found.
type Probe struct {
	// Running lists the versions the atelet DaemonSets carry: one normally,
	// two while a rolling upgrade is under way.
	Running []string
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

// Exports renders the environment for running upstream's ate-setup against
// the cluster st names, at the given running version.
func (p Probe) Exports(st *state.Setup, version string) string {
	lines := []string{
		"export PROJECT_ID=" + ShellQuote(st.ProjectID),
		"export CLUSTER_NAME=" + ShellQuote(st.ClusterName),
		"export CLUSTER_LOCATION=" + ShellQuote(st.Zone),
		"export NO_DEV_ENV='1'",
		"export VERSION=" + ShellQuote(version),
	}
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
	return strings.Join(lines, "\n")
}

// Apply records what the probe found as the installed side of an upgrade:
// the version, and the registry a build from source used so the new build
// pushes to the same place.
func (p Probe) Apply(st *state.Setup, version string) {
	st.InstalledVersion = version
	st.InstalledRepo = RepoURL
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

// InstalledExports is the environment for the runbook's ate-setup commands
// from the installed tree, which a rollback runs.
func InstalledExports(st *state.Setup) string {
	installed := Probe{KoDockerRepo: st.KoDockerRepo, ImageRepo: st.InstalledImageRepo, ImageTag: st.InstalledImageTag}
	return installed.Exports(st, st.InstalledVersion)
}

// The install labels nodes with substrate-<12 hex> for a build from source,
// which is ShortCommit's shape.
var sourceVersion = regexp.MustCompile(`^substrate-([0-9a-f]{12})$`)

// gitHubAPI is the endpoint InstalledCommit expands short commits through;
// tests point it at a stub. The client is bounded so a network that
// blackholes it hands the flow back within seconds.
var (
	gitHubAPI    = "https://api.github.com"
	gitHubClient = &http.Client{Timeout: 15 * time.Second}
)

// InstalledCommit finds the full commit the installed version was built
// from, which the upgrade fetches for rollback. A build from source names its
// first 12 characters in the version; GitHub expands them, since git cannot
// fetch an abbreviation from a remote. A pre-built version is a release tag,
// and this repository's history pins the commit each release was built from.
// An empty result with a nil error means it could not be found and has to be
// typed in.
func InstalledCommit(ctx context.Context, version string, prebuilt bool) (string, error) {
	if prebuilt {
		return releaseCommitFor(ctx, version)
	}
	m := sourceVersion.FindStringSubmatch(version)
	if m == nil {
		return "", nil
	}
	return expandCommit(ctx, m[1])
}

// expandCommit asks GitHub for the full SHA of an abbreviated upstream
// commit. The sha media type returns it as plain text.
func expandCommit(ctx context.Context, short string) (string, error) {
	owner, repo, ok := gitHubOwnerRepo(RepoURL)
	if !ok {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/commits/%s", gitHubAPI, owner, repo, short), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	resp, err := gitHubClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("expanding commit %s: %w", short, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("expanding commit %s: GitHub answered %s", short, resp.Status)
	}
	sha := strings.TrimSpace(string(body))
	if !fullSHA.MatchString(sha) || !strings.HasPrefix(sha, short) {
		return "", fmt.Errorf("expanding commit %s: unexpected answer %q", short, sha)
	}
	return sha, nil
}

func gitHubOwnerRepo(url string) (owner, repo string, ok bool) {
	rest, found := strings.CutPrefix(url, "https://github.com/")
	if !found {
		return "", "", false
	}
	rest = strings.TrimSuffix(rest, ".git")
	owner, repo, ok = strings.Cut(rest, "/")
	return owner, repo, ok && owner != "" && repo != ""
}

// releasePin matches the pinned commit in installer/internal/snapshot/snapshot.go
// at any revision of this repository.
var releasePin = regexp.MustCompile(`\bCommit\s*=\s*"([0-9a-f]{40})"`)

// releaseCommitFor maps a release tag to the commit it was built from: this
// checkout's pin for the current release, and for an earlier one the pin at
// the revision of this repository that introduced that ReleaseVersion.
func releaseCommitFor(ctx context.Context, tag string) (string, error) {
	if tag == ReleaseVersion {
		return Commit, nil
	}
	const pinFile = "installer/internal/snapshot/snapshot.go"
	root, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", nil
	}
	dir := strings.TrimSpace(string(root))
	revs, err := exec.CommandContext(ctx, "git", "-C", dir, "log", "--format=%H", "-S", fmt.Sprintf(`ReleaseVersion = %q`, tag), "--", pinFile).Output()
	if err != nil || strings.TrimSpace(string(revs)) == "" {
		return "", nil
	}
	// The oldest revision that mentions the tag is the one that pinned it.
	all := strings.Fields(string(revs))
	file, err := exec.CommandContext(ctx, "git", "-C", dir, "show", all[len(all)-1]+":"+pinFile).Output()
	if err != nil {
		return "", nil
	}
	return releasePinIn(string(file)), nil
}

func releasePinIn(source string) string {
	m := releasePin.FindStringSubmatch(source)
	if m == nil {
		return ""
	}
	return m[1]
}
