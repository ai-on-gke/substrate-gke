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
// stdout field's neighbour into it. The file holds one rule per line, so a
// line break inside a field travels as a record separator (0x1e); that is how
// a rule reproduces gcloud's multi-line errors.
const fakeGcloud = `#!/usr/bin/env bash
echo "gcloud $*" >> "$CALLS"
args="$*"
while IFS=$'\x1f' read -r pat code out err; do
  if [[ -n "$pat" && "$args" == *"$pat"* ]]; then
    [ -n "$out" ] && printf '%s\n' "${out//$'\x1e'/$'\n'}"
    [ -n "$err" ] && printf '%s\n' "${err//$'\x1e'/$'\n'}" >&2
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
		fields := []string{r.pattern, r.code, r.stdout, r.stderr}
		for i, f := range fields {
			fields[i] = strings.ReplaceAll(f, "\n", "\x1e")
		}
		lines = append(lines, strings.Join(fields, "\x1f"))
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

// Cloud Storage's two wordings for a missing bucket. gcloud (588.0.0,
// googlecloudsdk/api_lib/storage/errors.py GcsNotFoundError) prints the first
// when the failed request's URL names the bucket and the second when the
// error carries no URL.
const (
	bucketNotFound    = "ERROR: (gcloud.storage.buckets.describe) gs://acme-snapshots not found: 404."
	bucketHTTPError   = "ERROR: (gcloud.storage.buckets.describe) HTTPError 404: The specified bucket does not exist."
	bindingNotPresent = "ERROR: Policy binding with the specified principal, role, and condition not found!"
)

// How gcloud (583.0.0) reports deleting a cluster that is not in the given
// location: the code=404 is on the line after "ERROR:", in the Python
// representation of the failed request's arguments (its \n is literal).
const clusterNotFound = "ERROR: (gcloud.container.clusters.delete) Some requests did not succeed:\n" +
	` - args: ["ResponseError: code=404, message=Not found: projects/acme/zones/us-west1-c/clusters/substrate-test. This command is authenticated as deployer@acme.iam.gserviceaccount.com which is the active account specified by the [core/account] property.\nNo cluster named 'substrate-test' in acme."]` + "\n" +
	"   exit_code: 1"

// How IAM reports deleting a service account that is gone: a deleted account
// can be undeleted for 30 days, and for one IAM answers with
// PERMISSION_DENIED "(or it may not exist)" rather than NOT_FOUND. A caller
// who lacks iam.serviceAccounts.delete on an existing account gets the same
// words.
const serviceAccountDenied = "ERROR: (gcloud.iam.service-accounts.delete) PERMISSION_DENIED: Permission 'iam.serviceAccounts.delete' denied on resource (or it may not exist). This command is authenticated as deployer@acme.iam.gserviceaccount.com which is the active account specified by the [core/account] property.\n" +
	"- '@type': type.googleapis.com/google.rpc.ErrorInfo\n" +
	"  domain: iam.googleapis.com\n" +
	"  metadata:\n" +
	"    permission: iam.serviceAccounts.delete\n" +
	"  reason: IAM_PERMISSION_DENIED"

// notFoundRules make every deletion fail the way gcloud reports a resource
// that is already gone, as on a re-run after a partial teardown.
var notFoundRules = []rule{
	{"container clusters delete", "1", "", clusterNotFound},
	{"container clusters list", "0", "", ""},
	{"storage buckets describe", "1", "", bucketNotFound},
	{"projects remove-iam-policy-binding", "1", "", bindingNotPresent},
	{"iam service-accounts delete", "1", "", "ERROR: (gcloud.iam.service-accounts.delete) NOT_FOUND: Service account projects/acme/serviceAccounts/ate-snapshot-transfer@acme.iam.gserviceaccount.com does not exist."},
}

// Re-running after a partial teardown must still succeed: a resource that is
// already gone is the outcome teardown wants, not a failure. This guards the
// fix for the review question below against swallowing too little.
func TestTeardownToleratesAlreadyDeletedResources(t *testing.T) {
	run := runTeardown(t, "", teardownEnv, notFoundRules, "--all")
	if run.exitCode != 0 {
		t.Errorf("teardown of already-deleted resources exited %d, want 0:\n%s", run.exitCode, run.output)
	}
	if !strings.Contains(run.calls, "iam service-accounts delete ate-snapshot-transfer@acme.iam.gserviceaccount.com") {
		t.Errorf("--all did not delete the snapshot transfer service account; calls:\n%s", run.calls)
	}
}

// A service account deleted by an earlier run answers its delete with
// PERMISSION_DENIED "(or it may not exist)". Teardown then lists the
// project's accounts: absent means deleted; present means the delete really
// was denied, which fails the step and says so; a denied listing warns and
// counts the account as deleted, as a denied cluster listing does.
func TestTeardownTellsADeletedServiceAccountFromADeniedDelete(t *testing.T) {
	tests := []struct {
		name       string
		list       rule
		wantExit   bool
		wantOutput string
	}{
		{"deleted", rule{"iam service-accounts list", "0", "", ""}, false, "(already deleted)"},
		{"exists", rule{"iam service-accounts list", "0", "ate-gke-node@acme.iam.gserviceaccount.com", ""}, true, "exists; deleting it was denied"},
		{"list denied", rule{"iam service-accounts list", "1", "", "ERROR: (gcloud.iam.service-accounts.list) PERMISSION_DENIED: Permission 'iam.serviceAccounts.list' denied on resource 'projects/acme'."}, false, "WARNING: could not verify"},
		{"list failed", rule{"iam service-accounts list", "1", "", "ERROR: (gcloud.iam.service-accounts.list) UNAVAILABLE: The service is currently unavailable."}, true, "FAILED: delete node service account"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := []rule{{"iam service-accounts delete", "1", "", serviceAccountDenied}, tt.list}
			run := runTeardown(t, "", teardownEnv, rules, "--delete-node-service-account")
			if failed := run.exitCode != 0; failed != tt.wantExit {
				t.Errorf("teardown exit code %d, want failure = %v:\n%s", run.exitCode, tt.wantExit, run.output)
			}
			if !strings.Contains(run.output, tt.wantOutput) {
				t.Errorf("teardown output lacks %q:\n%s", tt.wantOutput, run.output)
			}
			if !strings.Contains(run.calls, "iam service-accounts list --project=acme --filter=email=ate-gke-node@acme.iam.gserviceaccount.com") {
				t.Errorf("teardown did not list the project's service accounts; calls:\n%s", run.calls)
			}
			if tt.wantExit == strings.Contains(run.output, "(already deleted)") {
				t.Errorf("teardown reported the account deleted = %v, want %v:\n%s", !tt.wantExit, tt.wantExit, run.output)
			}
		})
	}
}

// A PERMISSION_DENIED without "(or it may not exist)" is a plain denial, and
// must not trigger the listing or count as deleted.
func TestTeardownReportsADeniedServiceAccountDelete(t *testing.T) {
	rules := []rule{{"iam service-accounts delete", "1", "", "ERROR: (gcloud.iam.service-accounts.delete) PERMISSION_DENIED: Permission 'iam.serviceAccounts.delete' denied on resource 'projects/acme/serviceAccounts/ate-gke-node@acme.iam.gserviceaccount.com'."}}
	run := runTeardown(t, "", teardownEnv, rules, "--delete-node-service-account")
	if run.exitCode == 0 || strings.Contains(run.output, "(already deleted)") {
		t.Errorf("teardown exit %d after a denied delete:\n%s", run.exitCode, run.output)
	}
	if strings.Contains(run.calls, "iam service-accounts list") {
		t.Errorf("teardown listed service accounts after a plain denial; calls:\n%s", run.calls)
	}
}

// Every remove-iam-policy-binding passes --format=none: gcloud otherwise
// prints the whole updated policy, hundreds of lines naming every member of
// a shared project, on each revoke.
func TestTeardownRevokesWithoutPrintingThePolicy(t *testing.T) {
	run := runTeardown(t, "", teardownEnv, nil, "--all")
	if run.exitCode != 0 {
		t.Fatalf("teardown --all exited %d, want 0:\n%s", run.exitCode, run.output)
	}
	revokes := 0
	for _, call := range strings.Split(run.calls, "\n") {
		if !strings.Contains(call, "remove-iam-policy-binding") {
			continue
		}
		revokes++
		if !strings.Contains(call, " --format=none") {
			t.Errorf("revoke prints the policy: %s", call)
		}
	}
	if revokes == 0 {
		t.Fatalf("--all ran no remove-iam-policy-binding; calls:\n%s", run.calls)
	}
}

// Review question: does a re-run after the bucket is deleted fail? gcloud
// words a missing bucket as "HTTPError 404: The specified bucket does not
// exist." on some paths, which the "already gone" match did not cover, so the
// bucket and bucket-binding steps reported FAILED. Teardown checks the bucket
// once; for either wording of a 404 it skips those steps without calling them.
func TestTeardownSkipsBucketStepsWhenBucketIsGone(t *testing.T) {
	for _, stderr := range []string{bucketNotFound, bucketHTTPError} {
		rules := []rule{{"storage buckets describe", "1", "", stderr}}
		run := runTeardown(t, "", teardownEnv, rules, "--delete-iam-policy-bindings", "--delete-snapshot-bucket")
		if run.exitCode != 0 {
			t.Errorf("with describe error %q, teardown exited %d, want 0:\n%s", stderr, run.exitCode, run.output)
		}
		// Once each for the snapshot and asset buckets.
		if n := strings.Count(run.calls, "storage buckets describe"); n != 2 {
			t.Errorf("with describe error %q, teardown described the buckets %d times, want 2; calls:\n%s", stderr, n, run.calls)
		}
		for _, skipped := range []string{"storage rm", "storage buckets delete", "remove-iam-policy-binding"} {
			if strings.Contains(run.calls, skipped) {
				t.Errorf("with describe error %q, teardown ran %q against a missing bucket; calls:\n%s", stderr, skipped, run.calls)
			}
		}
	}
}

// A bucket that exists but is empty and no longer holds the bindings is also a
// partial teardown: the missing objects and bindings count as deleted.
func TestTeardownToleratesAnEmptyBucketWithoutBindings(t *testing.T) {
	rules := []rule{
		{"storage rm", "1", "", "ERROR: (gcloud.storage.rm) The following URLs matched no objects or files: -gs://acme-snapshots/**"},
		{"storage buckets remove-iam-policy-binding", "1", "", bindingNotPresent},
	}
	run := runTeardown(t, "", teardownEnv, rules, "--delete-iam-policy-bindings", "--delete-snapshot-bucket")
	if run.exitCode != 0 {
		t.Errorf("teardown exited %d, want 0:\n%s", run.exitCode, run.output)
	}
	if !strings.Contains(run.calls, "storage buckets delete gs://acme-snapshots") {
		t.Errorf("teardown did not delete the bucket; calls:\n%s", run.calls)
	}
	for _, want := range []string{
		"storage buckets delete gs://acme-snapshots-assets",
		"storage buckets remove-iam-policy-binding gs://acme-snapshots-assets",
		"--member=serviceAccount:ate-snapshot-transfer@acme.iam.gserviceaccount.com",
	} {
		if !strings.Contains(run.calls, want) {
			t.Errorf("teardown did not run %q for the asset bucket; calls:\n%s", want, run.calls)
		}
	}
}

// Only a 404 means the bucket is gone. If the describe fails for another
// reason, the bucket steps still run so they can report their own outcome.
func TestTeardownRunsBucketStepsWhenDescribeFails(t *testing.T) {
	rules := []rule{
		{"storage buckets describe", "1", "", "ERROR: (gcloud.storage.buckets.describe) HTTPError 403: deployer@acme.iam.gserviceaccount.com does not have storage.buckets.get access to the Google Cloud Storage bucket."},
		{"storage buckets delete", "1", "", "ERROR: (gcloud.storage.buckets.delete) HTTPError 403: deployer@acme.iam.gserviceaccount.com does not have storage.buckets.delete access to the Google Cloud Storage bucket."},
	}
	run := runTeardown(t, "", teardownEnv, rules, "--delete-snapshot-bucket")
	if run.exitCode == 0 {
		t.Errorf("teardown exited 0 after a denied describe and delete:\n%s", run.output)
	}
	if !strings.Contains(run.calls, "storage buckets delete gs://acme-snapshots") {
		t.Errorf("teardown skipped the bucket delete after a describe failure other than 404; calls:\n%s", run.calls)
	}
	if !strings.Contains(run.output, "storage.buckets.get") {
		t.Errorf("teardown hid the describe error:\n%s", run.output)
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

// teardownTools are the external commands teardown.sh runs besides gcloud.
var teardownTools = []string{"bash", "cat", "grep", "mktemp", "rm", "sed", "tee", "tr"}

// Review question: does a missing gcloud read as "already deleted"? The
// shell's own "gcloud: command not found" went through the same log as
// gcloud's errors and matched the "not found" pattern, so single-step runs
// printed "(already deleted)" and exited 0 without deleting anything.
// Teardown must stop before its first step instead. PATH holds only the
// other tools teardown uses, since a developer machine has gcloud on its
// usual PATH.
func TestTeardownFailsWithoutGcloud(t *testing.T) {
	script, err := filepath.Abs("teardown.sh")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, tool := range teardownTools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s is not on PATH; this test needs it to run teardown.sh: %v", tool, err)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"--delete-snapshot-bucket", "--revoke-gke-node-permissions"},
		{"--delete-iam-policy-bindings"},
		{"--all"},
	} {
		cmd := exec.Command(filepath.Join(bin, "bash"), append([]string{script}, args...)...)
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=" + bin, "HOME=" + t.TempDir()}
		for k, v := range teardownEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("teardown %v exited 0 without gcloud:\n%s", args, out)
		}
		if !strings.Contains(string(out), "gcloud not found on PATH") {
			t.Errorf("teardown %v did not say gcloud is missing:\n%s", args, out)
		}
		if strings.Contains(string(out), "already deleted") {
			t.Errorf("teardown %v reported a deletion without gcloud:\n%s", args, out)
		}
	}
}

// Only what gcloud prints from its own "ERROR: ..." line on can mean a
// resource is already gone. A "not found" from anything else, such as a
// gcloud wrapper whose Python is missing, or printed before the ERROR line,
// is a failure.
func TestTeardownOnlyTrustsGcloudErrorLines(t *testing.T) {
	for _, stderr := range []string{
		"/usr/lib/google-cloud-sdk/bin/gcloud: line 192: exec: python3: not found",
		"WARNING: config file not found; using defaults.\nERROR: (gcloud.storage.buckets.delete) HTTPError 403: deployer@acme.iam.gserviceaccount.com does not have storage.buckets.delete access to the Google Cloud Storage bucket.",
	} {
		rules := []rule{{"storage buckets delete", "1", "", stderr}}
		run := runTeardown(t, "", teardownEnv, rules, "--delete-snapshot-bucket")
		if run.exitCode == 0 {
			t.Errorf("teardown exited 0 after %q:\n%s", stderr, run.output)
		}
		if !strings.Contains(run.output, "FAILED: delete snapshot bucket") {
			t.Errorf("teardown did not report the bucket delete as failed after %q:\n%s", stderr, run.output)
		}
	}
}

// Review question: does a caller without container.clusters.list fail the
// teardown of a cluster that is gone? The delete returns NOT_FOUND and the
// list of other locations is denied. That must warn, not fail; a list error
// other than PERMISSION_DENIED still fails the run.
func TestTeardownClusterListErrors(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		wantExit bool
	}{
		{"denied", "ERROR: (gcloud.container.clusters.list) ResponseError: code=403, message=Required \"container.clusters.list\" permission(s) for \"projects/acme\". PERMISSION_DENIED", false},
		{"unavailable", "ERROR: (gcloud.container.clusters.list) ResponseError: code=503, message=The service is currently unavailable.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := []rule{notFoundRules[0], {"container clusters list", "1", "", tt.stderr}}
			run := runTeardown(t, "", teardownEnv, rules, "--delete-cluster")
			if failed := run.exitCode != 0; failed != tt.wantExit {
				t.Errorf("teardown exit code %d, want failure = %v:\n%s", run.exitCode, tt.wantExit, run.output)
			}
			if !tt.wantExit && !strings.Contains(run.output, "WARNING: could not verify") {
				t.Errorf("teardown did not warn that it could not check other locations:\n%s", run.output)
			}
		})
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

// --cluster-scoped deletes this cluster, its buckets and the bindings on
// them, and nothing that other Substrate clusters in the project share: the
// dashboards, the transfer service account and the project-level bindings.
func TestTeardownClusterScopedKeepsProjectWideResources(t *testing.T) {
	run := runTeardown(t, "", teardownEnv, nil, "--cluster-scoped")
	if run.exitCode != 0 {
		t.Fatalf("teardown --cluster-scoped exited %d, want 0:\n%s", run.exitCode, run.output)
	}
	for _, want := range []string{
		"storage buckets remove-iam-policy-binding gs://acme-snapshots",
		"storage buckets remove-iam-policy-binding gs://acme-snapshots-assets",
		"storage buckets delete gs://acme-snapshots ",
		"storage buckets delete gs://acme-snapshots-assets ",
		"container clusters delete substrate-test",
	} {
		if !strings.Contains(run.calls, want) {
			t.Errorf("--cluster-scoped did not run %q; calls:\n%s", want, run.calls)
		}
	}
	for _, unwanted := range []string{
		"monitoring dashboards",
		"iam service-accounts delete",
		"projects remove-iam-policy-binding",
		"artifacts repositories delete",
		"substrate-filestore-csi",
	} {
		if strings.Contains(run.calls, unwanted) {
			t.Errorf("--cluster-scoped ran a project-wide step (%q); calls:\n%s", unwanted, run.calls)
		}
	}
}

// Like --all, --cluster-scoped checks every variable it needs before the
// first gcloud call.
func TestTeardownClusterScopedChecksEveryVariableFirst(t *testing.T) {
	run := runTeardown(t, "", map[string]string{"PROJECT_ID": "acme", "NO_DEV_ENV": "1"}, nil, "--cluster-scoped")
	if run.exitCode == 0 {
		t.Errorf("teardown --cluster-scoped exited 0 with only PROJECT_ID set:\n%s", run.output)
	}
	if run.calls != "" {
		t.Errorf("teardown --cluster-scoped ran gcloud before finding a variable missing; calls:\n%s", run.calls)
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

// --all deletes the node service account, and only after the cluster: nodes
// still running as it would lose logging, monitoring and image pulls.
func TestTeardownAllDeletesNodeServiceAccountAfterCluster(t *testing.T) {
	run := runTeardown(t, "", teardownEnv, nil, "--all")
	if run.exitCode != 0 {
		t.Fatalf("teardown --all exited %d, want 0:\n%s", run.exitCode, run.output)
	}
	cluster := strings.Index(run.calls, "container clusters delete substrate-test")
	role := strings.Index(run.calls, "projects remove-iam-policy-binding acme --member=serviceAccount:ate-gke-node@acme.iam.gserviceaccount.com --role=roles/container.defaultNodeServiceAccount")
	account := strings.Index(run.calls, "iam service-accounts delete ate-gke-node@acme.iam.gserviceaccount.com")
	if cluster < 0 || role < 0 || account < 0 {
		t.Fatalf("--all skipped the cluster or the node service account; calls:\n%s", run.calls)
	}
	if !(cluster < role && role < account) {
		t.Errorf("--all must delete the cluster, then revoke the node role, then delete the account; calls:\n%s", run.calls)
	}

	vars := map[string]string{"PROJECT_ID": "acme", "NODE_GSA_NAME": "custom-node", "NO_DEV_ENV": "1"}
	run = runTeardown(t, "", vars, nil, "--delete-node-service-account")
	if run.exitCode != 0 || !strings.Contains(run.calls, "iam service-accounts delete custom-node@acme.iam.gserviceaccount.com") {
		t.Errorf("--delete-node-service-account ignored NODE_GSA_NAME (exit %d); calls:\n%s", run.exitCode, run.calls)
	}
}

// The Filestore step's deploy.sh (kubernetes-sigs/gcp-filestore-csi-driver)
// creates substrate-filestore-csi@<project> and grants it roles/file.editor
// on the project, which nothing in setup-gcp creates and teardown left
// behind (#143). --all revokes the role, then deletes the account, after the
// cluster whose driver runs as it; the revoke passes --format=none like the
// others. An account that was never created, or a role that was never
// granted (a Filestore run that failed before deploy.sh, or was skipped),
// counts as deleted; an account that exists but whose delete was denied
// fails the step and says so.
func TestTeardownDeletesTheFilestoreServiceAccount(t *testing.T) {
	const (
		revoke = "projects remove-iam-policy-binding acme --member=serviceAccount:substrate-filestore-csi@acme.iam.gserviceaccount.com --role=roles/file.editor --condition=None --quiet --format=none"
		remove = "iam service-accounts delete substrate-filestore-csi@acme.iam.gserviceaccount.com --project=acme --quiet"
	)
	run := runTeardown(t, "", teardownEnv, nil, "--all")
	if run.exitCode != 0 {
		t.Fatalf("teardown --all exited %d, want 0:\n%s", run.exitCode, run.output)
	}
	cluster := strings.Index(run.calls, "container clusters delete substrate-test")
	role := strings.Index(run.calls, revoke)
	account := strings.Index(run.calls, remove)
	if cluster < 0 || role < 0 || account < 0 {
		t.Fatalf("--all skipped the cluster or the Filestore service account; calls:\n%s", run.calls)
	}
	if !(cluster < role && role < account) {
		t.Errorf("--all must delete the cluster, then revoke the Filestore role, then delete the account; calls:\n%s", run.calls)
	}

	tests := []struct {
		name       string
		rules      []rule
		wantExit   bool
		wantOutput string
	}{
		{"deleted", nil, false, "Deleting the Filestore CSI service account..."},
		{"already gone", []rule{
			{"projects remove-iam-policy-binding", "1", "", bindingNotPresent},
			{"iam service-accounts delete", "1", "", serviceAccountDenied},
			{"iam service-accounts list", "0", "", ""},
		}, false, "(already deleted)"},
		{"role already revoked", []rule{
			{"projects remove-iam-policy-binding", "1", "", bindingNotPresent},
		}, false, "(already deleted)"},
		{"denied but exists", []rule{
			{"iam service-accounts delete", "1", "", serviceAccountDenied},
			{"iam service-accounts list", "0", "substrate-filestore-csi@acme.iam.gserviceaccount.com", ""},
		}, true, "exists; deleting it was denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := runTeardown(t, "", teardownEnv, tt.rules, "--delete-filestore-service-account")
			if failed := run.exitCode != 0; failed != tt.wantExit {
				t.Errorf("teardown exit code %d, want failure = %v:\n%s", run.exitCode, tt.wantExit, run.output)
			}
			if !strings.Contains(run.output, tt.wantOutput) {
				t.Errorf("teardown output lacks %q:\n%s", tt.wantOutput, run.output)
			}
			for _, want := range []string{revoke, remove} {
				if !strings.Contains(run.calls, want) {
					t.Errorf("teardown did not run %q; calls:\n%s", want, run.calls)
				}
			}
			if tt.wantExit != strings.Contains(run.output, "FAILED: delete Filestore CSI service account") {
				t.Errorf("teardown named the step as failed = %v, want %v:\n%s", !tt.wantExit, tt.wantExit, run.output)
			}
			if strings.Contains(run.calls, "ate-gke-node") || strings.Contains(run.calls, "ate-snapshot-transfer") {
				t.Errorf("--delete-filestore-service-account touched another account; calls:\n%s", run.calls)
			}
		})
	}
}
