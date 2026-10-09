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

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const teardownGcloud = `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${TEARDOWN_TEST_LOG}"
if [[ "$1 $2" = "artifacts repositories" && "${TEARDOWN_TEST_FAILURE:-}" = "$3" ]]; then
  echo "repository $3 denied" >&2
  exit 1
fi
case "$1 $2 $3" in
  "artifacts repositories describe")
    if [ -z "${TEARDOWN_TEST_REPOSITORY:-}" ]; then
      echo "repository not found" >&2
      exit 1
    fi ;;
  "artifacts repositories delete"|"monitoring dashboards list"|"projects remove-iam-policy-binding "*|"storage buckets "*|"storage rm "*|"container clusters delete"|"container node-pools delete") ;;
  *) echo "unexpected gcloud call: $*" >&2; exit 1 ;;
esac
`

func runTeardown(t *testing.T, args, env []string, wantErr string) ([]string, string) {
	t.Helper()
	script, err := filepath.Abs("../teardown.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(teardownGcloud), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls")
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TEARDOWN_TEST_LOG=" + log,
		"PROJECT_ID=test-project",
		"GCE_REGION=europe-west1",
		"KO_DOCKER_REPO=registry.example.com/unrelated/images",
		"TEARDOWN_TEST_REPOSITORY=projects/test-project/locations/europe-west1/repositories/ate-images",
	}, env...)
	out, err := cmd.CombinedOutput()
	if (err != nil) != (wantErr != "") || !strings.Contains(string(out), wantErr) {
		t.Fatalf("teardown: %v\n%s\nwant error %q", err, out, wantErr)
	}
	recorded, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil, string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n"), string(out)
}

func TestTeardown(t *testing.T) {
	const clusterCall = "container clusters delete test-cluster --location=us-central1-a --project=test-project --quiet"
	repositoryCalls := []string{
		"artifacts repositories describe ate-images --project=test-project --location=europe-west1 --quiet",
		"artifacts repositories delete ate-images --project=test-project --location=europe-west1 --quiet",
	}
	customCalls := []string{
		strings.ReplaceAll(repositoryCalls[0], "ate-images", "custom-images"),
		strings.ReplaceAll(repositoryCalls[1], "ate-images", "custom-images"),
	}
	customEnv := []string{
		"ARTIFACT_REGISTRY_REPOSITORY=custom-images",
		"TEARDOWN_TEST_REPOSITORY=projects/test-project/locations/europe-west1/repositories/custom-images",
	}
	clusterEnv := []string{"CLUSTER_NAME=test-cluster", "CLUSTER_LOCATION=us-central1-a"}
	allEnv := slices.Concat(clusterEnv, customEnv, []string{"PROJECT_NUMBER=123456789", "BUCKET_NAME=test-bucket"})
	allCalls := append([]string{clusterCall}, customCalls...)
	keepEnv := append(slices.Clone(allEnv), "GCE_REGION=")
	const lookupWarning = "repository describe denied\nWarning: unable to check Artifact Registry repository"

	for _, tc := range []struct {
		name                string
		args                string
		env                 []string
		want                []string
		wantErr, wantOutput string
	}{
		{"default repository", "--delete-repository", nil, repositoryCalls, "", ""},
		{"custom repository", "--delete-repository", customEnv, customCalls, "", ""},
		{"already deleted", "--delete-repository", []string{"TEARDOWN_TEST_REPOSITORY="}, repositoryCalls[:1], "", "repository not found\nWarning: unable to check Artifact Registry repository"},
		{"lookup denied", "--delete-repository", []string{"TEARDOWN_TEST_FAILURE=describe"}, repositoryCalls[:1], "", lookupWarning},
		{"delete denied", "--delete-repository", []string{"TEARDOWN_TEST_FAILURE=delete"}, repositoryCalls, "repository delete denied", ""},
		{"all", "--all --delete-repository", allEnv, allCalls, "", ""},
		{"all with node pool", "--all", append(slices.Clone(allEnv), "NODE_POOL_NAME=test-pool"), []string{"container node-pools delete test-pool --cluster=test-cluster --location=us-central1-a --project=test-project --quiet", clusterCall}, "", ""},
		{"keep repository by default", "--all", keepEnv, []string{clusterCall}, "", ""},
		{"all lookup denied", "--all --delete-repository", append(slices.Clone(allEnv), "TEARDOWN_TEST_FAILURE=describe"), allCalls[:2], "", lookupWarning},
		{"lookup denied before all", "--delete-repository --all", append(slices.Clone(allEnv), "TEARDOWN_TEST_FAILURE=describe"), []string{customCalls[0], clusterCall}, "", lookupWarning},
		{"all delete denied", "--all --delete-repository", append(slices.Clone(allEnv), "TEARDOWN_TEST_FAILURE=delete"), allCalls, "repository delete denied", ""},
		{"missing project", "--delete-repository", []string{"PROJECT_ID="}, nil, "PROJECT_ID is not set", ""},
		{"missing region", "--delete-repository", []string{"GCE_REGION="}, nil, "GCE_REGION is not set", ""},
		{"missing region before all", "--all --delete-repository", []string{"GCE_REGION="}, nil, "GCE_REGION is not set", ""},
		{"invalid repository before all", "--all --delete-repository", []string{"ARTIFACT_REGISTRY_REPOSITORY=images/other"}, nil, "ARTIFACT_REGISTRY_REPOSITORY must be", ""},
		{"unknown option after all", "--all --unknown", nil, nil, "Usage:", ""},
		{"no options", "", nil, nil, "Usage:", ""},
		{"cluster without repository config", "--delete-cluster", append(slices.Clone(clusterEnv), "GCE_REGION="), []string{clusterCall}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := strings.Fields(tc.args)
			calls, out := runTeardown(t, args, tc.env, tc.wantErr)
			if !strings.Contains(out, tc.wantOutput) {
				t.Errorf("output = %q, want %q", out, tc.wantOutput)
			}
			// Validation failures must not invoke gcloud at all.
			if slices.Contains(args, "--all") && len(tc.want) > 0 {
				calls = slices.DeleteFunc(calls, func(call string) bool {
					return !strings.HasPrefix(call, "artifacts ") && !strings.HasPrefix(call, "container ")
				})
			}
			if !slices.Equal(calls, tc.want) {
				t.Errorf("gcloud calls = %q, want %q", calls, tc.want)
			}
		})
	}
}
