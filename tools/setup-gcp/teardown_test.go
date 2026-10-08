// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGcloud answers each call from a rules file: the first rule whose
// pattern is a substring of the arguments sets the exit code, stdout, and
// stderr. Calls matching no rule succeed silently. Every call is logged.
// Fields are separated by ASCII unit separators (0x1f) rather than tabs:
// `read` collapses runs of whitespace separators, which would shift an empty
// stdout field's neighbour into it.
const fakeGcloud = `#!/usr/bin/env bash
echo "gcloud $*" >> "$CALLS"
args="$*"
while IFS=$'\x1f' read -r pat code out err; do
  if [[ -n "$pat" && "$args" == *"$pat"* ]]; then
    [ -n "$out" ] && printf '%s\n' "$out"
    [ -n "$err" ] && printf '%s\n' "$err" >&2
    exit "$code"
  fi
done < "$RULES"
exit 0
`

// rule is one fakeGcloud response.
type rule struct{ pattern, code, stdout, stderr string }

// teardownEnv is what cleanup-gcp passes teardown.sh.
var teardownEnv = map[string]string{
	"PROJECT_ID":       "acme",
	"PROJECT_NUMBER":   "42",
	"CLUSTER_NAME":     "substrate-test",
	"CLUSTER_LOCATION": "us-west1-c",
	"BUCKET_NAME":      "acme-snapshots",
	"NO_DEV_ENV":       "1",
}

type teardownRun struct {
	exitCode int
	output   string
	calls    string
}

// runTeardown runs teardown.sh with only the given variables set (plus PATH
// and HOME), from cwd, against fakeGcloud.
func runTeardown(t *testing.T, cwd string, vars map[string]string, rules []rule, args ...string) teardownRun {
	t.Helper()
	script, err := filepath.Abs("teardown.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(fakeGcloud), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range rules {
		lines = append(lines, strings.Join([]string{r.pattern, r.code, r.stdout, r.stderr}, "\x1f"))
	}
	rulesFile := filepath.Join(dir, "rules")
	if err := os.WriteFile(rulesFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(dir, "calls")

	if cwd == "" {
		cwd = t.TempDir()
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + dir,
		"CALLS=" + callsFile,
		"RULES=" + rulesFile,
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	run := teardownRun{output: string(out)}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	if calls, err := os.ReadFile(callsFile); err == nil {
		run.calls = string(calls)
	}
	return run
}

// notFoundRules make every deletion fail the way gcloud reports a resource
// that is already gone, as on a re-run after a partial teardown.
var notFoundRules = []rule{
	{"container clusters delete", "1", "", "ERROR: (gcloud.container.clusters.delete) ResponseError: code=404, message=Not found: projects/acme/zones/us-west1-c/clusters/substrate-test."},
	{"container clusters list", "0", "", ""},
	{"storage rm", "1", "", "ERROR: (gcloud.storage.rm) The following URLs matched no objects or files: -gs://acme-snapshots/**"},
	{"storage buckets delete", "1", "", "ERROR: (gcloud.storage.buckets.delete) gs://acme-snapshots not found: 404."},
	{"storage buckets remove-iam-policy-binding", "1", "", "ERROR: (gcloud.storage.buckets.remove-iam-policy-binding) gs://acme-snapshots not found: 404."},
	{"projects remove-iam-policy-binding", "1", "", "ERROR: Policy binding with the specified principal, role, and condition not found!"},
}

// Re-running after a partial teardown must still succeed: a resource that is
// already gone is the outcome teardown wants, not a failure. This guards the
// fix for the review question below against swallowing too little.
func TestTeardownToleratesAlreadyDeletedResources(t *testing.T) {
	run := runTeardown(t, "", teardownEnv, notFoundRules, "--all")
	if run.exitCode != 0 {
		t.Errorf("teardown of already-deleted resources exited %d, want 0:\n%s", run.exitCode, run.output)
	}
}

// Review question: does a failed delete still exit 0? Every gcloud call ends
// in `|| true`, so a denied or failed delete prints an error and the script
// carries on to report success while the resource keeps billing. A failure
// other than "already gone" must make the run exit non-zero and name the
// step, after the remaining steps have still been attempted.
func TestTeardownReportsAFailedDelete(t *testing.T) {
	rules := []rule{{"storage buckets delete", "1", "",
		"ERROR: (gcloud.storage.buckets.delete) HTTPError 403: deployer@acme.iam.gserviceaccount.com does not have storage.buckets.delete access to the Google Cloud Storage bucket."}}
	run := runTeardown(t, "", teardownEnv, rules, "--all")
	if run.exitCode == 0 {
		t.Errorf("teardown exited 0 after a denied bucket delete:\n%s", run.output)
	}
	named := false
	for _, line := range strings.Split(run.output, "\n") {
		if strings.Contains(strings.ToLower(line), "fail") && strings.Contains(line, "snapshot bucket") {
			named = true
		}
	}
	if !named {
		t.Errorf("teardown output has no failure line naming the snapshot bucket step:\n%s", run.output)
	}
	if !strings.Contains(run.calls, "container clusters delete substrate-test") {
		t.Errorf("teardown stopped at the failure instead of attempting the remaining steps; calls:\n%s", run.calls)
	}
}

// Review question: does a denied dashboard list count as "nothing to
// delete"? The list's stderr is discarded and its failure ignored, so the
// dashboards survive and the run reports success.
func TestTeardownReportsAFailedDashboardList(t *testing.T) {
	rules := []rule{{"monitoring dashboards list", "1", "",
		"ERROR: (gcloud.monitoring.dashboards.list) PERMISSION_DENIED: Permission monitoring.dashboards.list denied on resource 'projects/acme'."}}
	run := runTeardown(t, "", teardownEnv, rules, "--delete-dashboards")
	if run.exitCode == 0 {
		t.Errorf("teardown exited 0 after a denied dashboard list:\n%s", run.output)
	}
	if !strings.Contains(run.output, "PERMISSION_DENIED") {
		t.Errorf("teardown hid the dashboard list error:\n%s", run.output)
	}
}

// Review example: a wrong --location. GKE answers the delete with NOT_FOUND,
// exactly as for a cluster that is already gone, so treating every NOT_FOUND
// as success would report a cluster that is still running as deleted. When
// the cluster exists in another location, the run must fail and say where.
func TestTeardownReportsAClusterInAnotherLocation(t *testing.T) {
	rules := []rule{
		notFoundRules[0],
		{"container clusters list", "0", "us-central1-a", ""},
	}
	run := runTeardown(t, "", teardownEnv, rules, "--delete-cluster")
	if run.exitCode == 0 {
		t.Errorf("teardown exited 0 while the cluster still runs in another location:\n%s", run.output)
	}
	if !strings.Contains(run.output, "us-central1-a") {
		t.Errorf("teardown output does not name the location the cluster is in:\n%s", run.output)
	}
}

// Review question: does --all check its variables lazily? Each step requires
// only its own variables, so with just PROJECT_ID set the dashboards step
// runs and a later step aborts: the mid-deletion failure `require` exists to
// prevent. --all must check every variable it needs before any gcloud call.
func TestTeardownAllChecksEveryVariableFirst(t *testing.T) {
	run := runTeardown(t, "", map[string]string{"PROJECT_ID": "acme", "NO_DEV_ENV": "1"}, nil, "--all")
	if run.exitCode == 0 {
		t.Errorf("teardown --all exited 0 with only PROJECT_ID set:\n%s", run.output)
	}
	if run.calls != "" {
		t.Errorf("teardown --all ran gcloud before finding a variable missing; calls:\n%s", run.calls)
	}
}

// Review question: is the missing-variable message stale? It points at
// hack/ate-dev-env.sh.example, which lives in agent-substrate/substrate, and
// creating a dev-env file does nothing under cleanup-gcp (NO_DEV_ENV=1).
func TestTeardownMissingVariableMessage(t *testing.T) {
	run := runTeardown(t, "", map[string]string{"NO_DEV_ENV": "1"}, nil, "--delete-cluster")
	if run.exitCode == 0 {
		t.Fatalf("teardown exited 0 with no variables set:\n%s", run.output)
	}
	if !strings.Contains(run.output, "PROJECT_ID is not set") {
		t.Errorf("message does not name the missing variable:\n%s", run.output)
	}
	if strings.Contains(run.output, "hack/") || !strings.Contains(run.output, "tools/setup-gcp/README.md") {
		t.Errorf("message should point at this repository's tools/setup-gcp/README.md, not upstream's hack/:\n%s", run.output)
	}
}

// Review question: does teardown.sh itself ignore .ate-dev-env.sh, as the
// top-level README says? Run directly, it sources the file unless
// NO_DEV_ENV=1, which cleanup-gcp sets; this pins the behaviour the README
// has to describe.
func TestTeardownSourcesDevEnvUnlessNoDevEnv(t *testing.T) {
	cwd := t.TempDir()
	devEnv := "export PROJECT_ID=dev-env-project\n"
	if err := os.WriteFile(filepath.Join(cwd, ".ate-dev-env.sh"), []byte(devEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"PROJECT_ID": "acme"}

	direct := runTeardown(t, cwd, vars, nil, "--delete-dashboards")
	if !strings.Contains(direct.calls, "--project=dev-env-project") {
		t.Errorf("teardown.sh run directly did not source .ate-dev-env.sh; calls:\n%s", direct.calls)
	}

	vars["NO_DEV_ENV"] = "1"
	guarded := runTeardown(t, cwd, vars, nil, "--delete-dashboards")
	if strings.Contains(guarded.calls, "dev-env-project") || !strings.Contains(guarded.calls, "--project=acme") {
		t.Errorf("teardown.sh with NO_DEV_ENV=1 used .ate-dev-env.sh; calls:\n%s", guarded.calls)
	}
}
