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
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// A build from source names its images ateapi-<md5>, a published release
// ateapi:<tag>; the probe has to tell the two apart and recover the registry
// (and tag) ate-setup needs, then render a shell block that sources.
func TestParseProbeReadsSourceAndPrebuiltInstalls(t *testing.T) {
	st := testSetup(t)
	src, err := ParseProbe([]string{
		"Fetching cluster endpoint and auth data.",
		versionsMarker + "substrate-5f0ef402d9c4",
		imageMarker + "gcr.io/acme/ate-images/ateapi-752889f8b0bcdbee32172ac9fe056025@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42",
	})
	if err != nil {
		t.Fatalf("ParseProbe(source): %v", err)
	}
	if src.Prebuilt() || src.KoDockerRepo != "gcr.io/acme/ate-images" || len(src.Running) != 1 {
		t.Errorf("source probe = %+v", src)
	}
	script := src.Exports(st, src.Running[0]) + `; printf '%s|%s|%s' "$KO_DOCKER_REPO" "$VERSION" "$CLUSTER_NAME"`
	out, err := exec.Command("bash", "-c", script).Output()
	if err != nil || string(out) != "gcr.io/acme/ate-images|substrate-5f0ef402d9c4|"+st.ClusterName {
		t.Errorf("source exports gave %q, %v", out, err)
	}

	pre, err := ParseProbe([]string{
		versionsMarker + "v0.1.0 v0.1.1 ",
		imageMarker + ReleaseRepo + "/ateapi:v0.1.1@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42",
	})
	if err != nil {
		t.Fatalf("ParseProbe(prebuilt): %v", err)
	}
	if !pre.Prebuilt() || pre.ImageRepo != ReleaseRepo || pre.ImageTag != "v0.1.1" || len(pre.Running) != 2 {
		t.Errorf("prebuilt probe = %+v", pre)
	}
	if exports := pre.Exports(st, "v0.1.1"); !strings.Contains(exports, "export ATE_IMAGE_TAG='v0.1.1'") || strings.Contains(exports, "KO_DOCKER_REPO") {
		t.Errorf("prebuilt exports:\n%s", exports)
	}
	got := state.NewSetup()
	src.Apply(got, "substrate-5f0ef402d9c4")
	if got.InstalledVersion != "substrate-5f0ef402d9c4" || got.KoDockerRepo != "gcr.io/acme/ate-images" || got.InstalledRepo != RepoURL {
		t.Errorf("Apply() = %+v", got)
	}

	for _, bad := range [][]string{
		{imageMarker + "gcr.io/acme/ate-images/ateapi-752889f8b0bcdbee32172ac9fe056025"},
		{versionsMarker + "v0.1.0"},
		{versionsMarker + "v0.1.0", imageMarker + "gcr.io/acme/something-else:latest"},
	} {
		if _, err := ParseProbe(bad); err == nil {
			t.Errorf("ParseProbe(%v) should fail", bad)
		}
	}
	// The dry run replays a probe of the pin.
	if p, err := ParseProbe(ProbeCluster(st, false).SimLines); err != nil || p.Running[0] != "substrate-"+ShortCommit() || p.Prebuilt() {
		t.Errorf("dry-run probe = %+v, %v", p, err)
	}
	for _, credentials := range []bool{true, false} {
		spec := ProbeCluster(st, credentials)
		if err := exec.Command("bash", "-n", "-c", spec.Argv[len(spec.Argv)-1]).Run(); err != nil {
			t.Errorf("ProbeCluster(credentials=%v) is not valid shell: %v", credentials, err)
		}
	}
}

// git cannot fetch an abbreviated commit from a remote, so the 12 characters
// in a build-from-source version are expanded through GitHub; a release tag
// maps to the commit this repository pinned for it.
func TestInstalledCommit(t *testing.T) {
	const full = "5f0ef402d9c4dfab84bdf9ebef1ed762168f9c9c"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github.sha" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		switch r.URL.Path {
		case "/repos/agent-substrate/substrate/commits/5f0ef402d9c4":
			w.Write([]byte(full))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := gitHubAPI
	gitHubAPI = srv.URL
	defer func() { gitHubAPI = old }()

	if got, err := InstalledCommit(context.Background(), "substrate-5f0ef402d9c4", false); err != nil || got != full {
		t.Errorf("InstalledCommit(source) = %q, %v", got, err)
	}
	if _, err := InstalledCommit(context.Background(), "substrate-000000000000", false); err == nil {
		t.Error("an unknown abbreviation should be an error, not a guess")
	}
	if got, err := InstalledCommit(context.Background(), "dev-haoyu", false); err != nil || got != "" {
		t.Errorf("a version that names no commit = %q, %v; want empty, nil", got, err)
	}
	if got, err := InstalledCommit(context.Background(), ReleaseVersion, true); err != nil || got != Commit {
		t.Errorf("InstalledCommit(release) = %q, %v; want the pin", got, err)
	}
	if got := releasePinIn("\tCommit = \"" + full + "\"\n"); got != full {
		t.Errorf("releasePinIn = %q", got)
	}
	if owner, repo, ok := gitHubOwnerRepo(RepoURL); !ok || owner != "agent-substrate" || repo != "substrate" {
		t.Errorf("gitHubOwnerRepo = %q %q %v", owner, repo, ok)
	}
}
