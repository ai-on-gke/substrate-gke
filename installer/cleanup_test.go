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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupGcpDelegatesToLocalTool(t *testing.T) {
	script, err := filepath.Abs("../tools/cleanup-gcp")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, args, location, region, repository, wantErr string
		decline, failure                                  bool
	}{
		{name: "default keeps repository", location: "us-west1-c", region: "us-west1", repository: "ate-images"},
		{name: "custom target", args: "--delete-repository --region europe-west4 --repository shared-images", location: "us-west1-c", region: "europe-west4", repository: "shared-images"},
		{name: "keep overrides deletion", args: "--delete-repository --keep-repository", location: "us-west1-c", region: "us-west1", repository: "ate-images"},
		{name: "regional cluster", location: "us-central1", region: "us-central1", repository: "ate-images"},
		{name: "declined", decline: true, location: "us-west1-c", wantErr: "aborted"},
		{name: "deletion failure", failure: true, location: "us-west1-c", wantErr: "deletion failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin, root, log := filepath.Join(dir, "bin"), filepath.Join(dir, "user's checkout"), filepath.Join(dir, "calls")
			write := func(path, body string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			teardown := `#!/bin/bash
set -eu
if [ -z "${NO_DEV_ENV:-}" ] && [ -f .ate-dev-env.sh ]; then source .ate-dev-env.sh; fi
printf 'teardown %s|%s|%s|%s|%s|%s|%s|%s|%s\n' "$*" "$PROJECT_ID" "$PROJECT_NUMBER" "$CLUSTER_NAME" "$CLUSTER_LOCATION" "$BUCKET_NAME" "$GCE_REGION" "$ARTIFACT_REGISTRY_REPOSITORY" "$NODE_POOL_NAME" >> "$CLEANUP_TEST_LOG"
`
			if tc.failure {
				teardown += "echo 'deletion failed' >&2; exit 9\n"
			}
			data, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(root, "tools/cleanup-gcp")
			write(wrapper, string(data))
			write(filepath.Join(root, "tools/setup-gcp/teardown.sh"), teardown)
			write(filepath.Join(root, ".ate-dev-env.sh"), "echo 'sourced local dev env' >&2; exit 8\n")
			write(filepath.Join(bin, "gcloud"), `#!/bin/bash
set -eu
[[ "$*" == 'projects describe acme --format=value(projectNumber)' ]] || exit 7
echo 12345
`)
			args := []string{wrapper, "--project", "acme", "--cluster", "cluster", "--location", tc.location, "--bucket", "snapshots"}
			args = append(args, strings.Fields(tc.args)...)
			if !tc.decline {
				args = append(args, "--yes")
			}
			cmd := exec.CommandContext(t.Context(), "bash", args...)
			cmd.Dir = root
			cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "CLEANUP_TEST_LOG=" + log, "NODE_POOL_NAME=unrelated-pool", "KO_DOCKER_REPO=registry.example.com/unrelated"}
			cmd.Stdin = strings.NewReader("no\n")
			out, err := cmd.CombinedOutput()
			if (err != nil) != (tc.wantErr != "") || !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("cleanup: %v\n%s; want error %q", err, out, tc.wantErr)
			}
			data, readErr := os.ReadFile(log)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			calls := string(data)
			if tc.wantErr != "" {
				if !tc.failure && strings.Contains(calls, "teardown ") {
					t.Fatal("teardown ran despite failed precondition")
				}
				if strings.Contains(string(out), "Done.") {
					t.Fatal("failure reported success")
				}
				return
			}
			flags := "--all"
			if strings.Contains(tc.args, "--delete-repository") && !strings.Contains(tc.args, "--keep-repository") {
				flags += " --delete-repository"
			}
			want := "teardown " + flags + "|acme|12345|cluster|" + tc.location + "|snapshots|" + tc.region + "|" + tc.repository + "|\n"
			if !strings.Contains(calls, want) {
				t.Fatalf("calls = %s; want %s", calls, want)
			}
		})
	}
}

func TestCleanupGcpRejectsInvalidOptionsBeforeCloudCalls(t *testing.T) {
	for _, args := range [][]string{
		{"--region"}, {"--repository", "../images"}, {"--commit", "main"},
		{"--substrate-root", "/unused"},
		{"--unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", append([]string{"../tools/cleanup-gcp", "--project", "acme", "--cluster", "cluster", "--location", "us-west1-c", "--bucket", "snapshots", "--yes"}, args...)...)
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte("#!/bin/sh\necho unexpected-cloud-call >&2\nexit 99\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
			out, err := cmd.CombinedOutput()
			if err == nil || strings.Contains(string(out), "unexpected-cloud-call") {
				t.Fatalf("validation did not stop execution: %v\n%s", err, out)
			}
		})
	}
}
