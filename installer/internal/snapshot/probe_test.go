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
	if got.InstalledCommit != "" {
		t.Errorf("a read without the binary's report cannot know the commit, got %q", got.InstalledCommit)
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
	// The dry run replays a probe of the pin, commit included.
	if p, err := ParseProbe(ProbeCluster(st, false).SimLines); err != nil || p.Running[0] != "substrate-"+ShortCommit() || p.Prebuilt() || p.Commit != Commit {
		t.Errorf("dry-run probe = %+v, %v", p, err)
	}
	for _, credentials := range []bool{true, false} {
		spec := ProbeCluster(st, credentials)
		if err := exec.Command("bash", "-n", "-c", spec.Argv[len(spec.Argv)-1]).Run(); err != nil {
			t.Errorf("ProbeCluster(credentials=%v) is not valid shell: %v", credentials, err)
		}
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
	if !strings.Contains(string(out), "Unauthorized") || strings.Contains(string(out), versionsMarker) {
		t.Errorf("the read should show kubectl's error and no markers:\n%s", out)
	}
}

// The probe reads one image, the API server's; the version it pairs with
// is the one chosen, which mid-upgrade may be the other running one, and
// which a digest-only reference does not name at all.
func TestApplyPairsTheImageTagWithTheChosenVersion(t *testing.T) {
	const digest = "@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42"
	pre, err := ParseProbe([]string{versionsMarker + "v0.1.0 v0.1.1", imageMarker + ReleaseRepo + "/ateapi:v0.1.1" + digest})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v0.1.0", "v0.1.1"} {
		st := state.NewSetup()
		pre.Apply(st, version)
		if st.InstalledImageRepo != ReleaseRepo || st.InstalledImageTag != version {
			t.Errorf("Apply(%s) recorded %s:%s", version, st.InstalledImageRepo, st.InstalledImageTag)
		}
	}

	pinned, err := ParseProbe([]string{versionsMarker + "v0.1.0", imageMarker + ReleaseRepo + "/ateapi" + digest})
	if err != nil {
		t.Fatalf("a digest-only reference should parse: %v", err)
	}
	if !pinned.Prebuilt() || pinned.ImageRepo != ReleaseRepo || pinned.ImageTag != "" {
		t.Errorf("digest-only probe = %+v", pinned)
	}
	st := testSetup(t)
	pinned.Apply(st, "v0.1.0")
	if exports := InstalledExports(st); !strings.Contains(exports, "export ATE_IMAGE_TAG='v0.1.0'") || !strings.Contains(exports, "export ATE_IMAGE_REPO="+ShellQuote(ReleaseRepo)) {
		t.Errorf("digest-only exports:\n%s", exports)
	}
}

// A build from source needs a registry to push to. A cluster that ran
// pre-built images never had one, and a cluster described by hand names
// none; the block then carries the project's default rather than an empty
// value that fails the first ko push.
func TestExportsDefaultTheRegistryForABuildFromSource(t *testing.T) {
	st := testSetup(t)
	st.KoDockerRepo = ""
	for name, exports := range map[string]string{
		"probe":     Probe{}.Exports(st, "substrate-0123456789ab"),
		"installed": InstalledExports(st),
	} {
		if !strings.Contains(exports, "export KO_DOCKER_REPO='gcr.io/acme/ate-images'") || strings.Contains(exports, "KO_DOCKER_REPO=''") {
			t.Errorf("%s exports:\n%s", name, exports)
		}
	}
}

// The commit comes from the running binary, which Go stamps with the
// vcs.revision of the tree it was built from. A tree with an untracked file
// in it reports dirty, which does not change the commit; a binary that was
// not built from a checkout reports none, and the commit is then typed in.
// With two versions running, the commit belongs to the version the binary
// reports being.
func TestParseProbeReadsTheCommitOffTheRunningBinary(t *testing.T) {
	const full = "5f0ef402d9c4dfab84bdf9ebef1ed762168f9c9c"
	image := imageMarker + "gcr.io/acme/ate-images/ateapi-752889f8b0bcdbee32172ac9fe056025@sha256:5249637d3f23159045f6143efd01829d059a9f34a171c15b2464db213e501a42"

	p, err := ParseProbe([]string{versionsMarker + "substrate-5f0ef402d9c4", image,
		buildMarker + "substrate-5f0ef402d9c4 commit=" + full + "-dirty built=2026-09-03T02:29:02Z linux/amd64"})
	if err != nil || p.Commit != full || p.BuildVersion != "substrate-5f0ef402d9c4" {
		t.Fatalf("ParseProbe(dirty build) = %+v, %v", p, err)
	}
	st := state.NewSetup()
	p.Apply(st, "substrate-5f0ef402d9c4")
	if st.InstalledCommit != full {
		t.Errorf("Apply should record the binary's commit, got %q", st.InstalledCommit)
	}

	unknown, err := ParseProbe([]string{versionsMarker + "v0.1.0", image, buildMarker + "v0.1.0 commit=unknown built=unknown linux/amd64"})
	if err != nil || unknown.Commit != "" {
		t.Errorf("ParseProbe(unknown commit) = %+v, %v; want no commit", unknown, err)
	}

	two, err := ParseProbe([]string{versionsMarker + "v0.1.0 v0.1.1", image,
		buildMarker + "v0.1.1 commit=" + full + " built=2026-09-03T02:29:02Z linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	for version, want := range map[string]string{"v0.1.1": full, "v0.1.0": ""} {
		st := state.NewSetup()
		two.Apply(st, version)
		if st.InstalledCommit != want {
			t.Errorf("Apply(%s) with the API server on v0.1.1 recorded commit %q, want %q", version, st.InstalledCommit, want)
		}
	}
}
