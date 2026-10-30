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

// fakeKubectl logs each call. `apply -f -` saves its stdin to $APPLIED, and
// `get workerpools` prints $WORKERPOOLS, the jsonpath rows the script reads.
const fakeKubectl = `#!/usr/bin/env bash
echo "kubectl $*" >> "$CALLS"
case "$*" in
  *"apply -f -"*) cat > "$APPLIED" ;;
  *"get workerpools"*) [ -n "${WORKERPOOLS:-}" ] && printf '%s\n' "$WORKERPOOLS" ;;
esac
exit 0
`

// fakeAssemble stands in for upstream's hack/microvm-assets/assemble.sh: it
// prints $STAMP for --print-stamp, and otherwise writes the asset set and the
// stamp into $OUT.
const fakeAssemble = `#!/usr/bin/env bash
if [ "${1:-}" = "--print-stamp" ]; then echo "$STAMP"; exit 0; fi
echo "assemble ARCH=$ARCH OUT=$OUT" >> "$CALLS"
mkdir -p "$OUT"
for f in cloud-hypervisor virtiofsd vmlinux rootfs.img; do echo "$f" > "$OUT/$f"; done
echo "$STAMP" > "$OUT/.asset-versions"
`

// fakeTemplate keeps the two variables upstream's template substitutes.
const fakeTemplate = `kind: SandboxConfig
metadata:
  name: microvm
spec:
  url: "${BUCKET_SCHEME}://${ASSET_BUCKET_NAME}/kata-assets/vmlinux"
`

// microVMEnv is what the installer passes the script.
var microVMEnv = map[string]string{
	"PROJECT_ID":  "acme",
	"BUCKET_NAME": "acme-snapshots",
	"ARCH":        "amd64",
	"STAMP":       "kata=4.3.1",
	"NO_DEV_ENV":  "1",
}

type microVMRun struct {
	exitCode        int
	output, calls   string
	applied, outDir string
}

// fakeSubstrate returns a Substrate tree with the two upstream files the
// script reads.
func fakeSubstrate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range map[string]string{
		"hack/microvm-assets/assemble.sh":                   fakeAssemble,
		"manifests/microvm/sandboxconfig-microvm.yaml.tmpl": fakeTemplate,
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runMicroVMDeps runs install-microvm-deps.sh against fakeGcloud and
// fakeKubectl, with SUBSTRATE_ROOT set to root (unless vars sets it) and only
// the given variables in the environment.
func runMicroVMDeps(t *testing.T, root string, vars map[string]string, rules []rule, args ...string) microVMRun {
	t.Helper()
	script, err := filepath.Abs("install-microvm-deps.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"gcloud": fakeGcloud, "kubectl": fakeKubectl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
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
	appliedFile := filepath.Join(dir, "applied")

	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + dir,
		"CALLS=" + callsFile,
		"RULES=" + rulesFile,
		"APPLIED=" + appliedFile,
		"SUBSTRATE_ROOT=" + root,
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	run := microVMRun{output: string(out), outDir: filepath.Join(root, "bin", "microvm-assets", "amd64")}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	if b, err := os.ReadFile(callsFile); err == nil {
		run.calls = string(b)
	}
	if b, err := os.ReadFile(appliedFile); err == nil {
		run.applied = string(b)
	}
	return run
}

// The assets go to the asset bucket, the only one setup-gcp lets nodes read,
// and the SandboxConfig points there with the gs scheme the GCS object-store
// plugin serves.
func TestInstallMicroVMDepsStagesToTheAssetBucket(t *testing.T) {
	run := runMicroVMDeps(t, fakeSubstrate(t), microVMEnv, nil, "--install")
	if run.exitCode != 0 {
		t.Fatalf("install exited %d:\n%s", run.exitCode, run.output)
	}
	if !strings.Contains(run.calls, "assemble ARCH=amd64 OUT="+run.outDir) {
		t.Errorf("install did not assemble into %s; calls:\n%s", run.outDir, run.calls)
	}
	for _, f := range []string{"cloud-hypervisor", "virtiofsd", "vmlinux", "rootfs.img"} {
		want := "gcloud --project=acme storage cp " + filepath.Join(run.outDir, f) + " gs://acme-snapshots-assets/kata-assets/" + f
		if !strings.Contains(run.calls, want) {
			t.Errorf("install did not run %q; calls:\n%s", want, run.calls)
		}
	}
	if strings.Contains(run.calls, "gs://acme-snapshots/") {
		t.Errorf("install wrote to the snapshot bucket; calls:\n%s", run.calls)
	}
	if want := `url: "gs://acme-snapshots-assets/kata-assets/vmlinux"`; !strings.Contains(run.applied, want) {
		t.Errorf("applied SandboxConfig lacks %q:\n%s", want, run.applied)
	}
	// The installer's checklist follows these lines.
	for _, want := range []string{
		"[install-microvm-deps]: Assembling micro-VM assets",
		"[install-microvm-deps]: Uploading assets to gs://",
		"[install-microvm-deps]: Applying microvm SandboxConfig",
	} {
		if !strings.Contains(run.output, want) {
			t.Errorf("output lacks %q:\n%s", want, run.output)
		}
	}
}

// An asset dir assembled at the current versions is reused; one assembled
// before a version bump is not, because its files keep their names.
func TestInstallMicroVMDepsReassemblesOnlyStaleAssets(t *testing.T) {
	root := fakeSubstrate(t)
	if run := runMicroVMDeps(t, root, microVMEnv, nil, "--install"); run.exitCode != 0 {
		t.Fatalf("first install exited %d:\n%s", run.exitCode, run.output)
	}

	run := runMicroVMDeps(t, root, microVMEnv, nil, "--install")
	if run.exitCode != 0 || strings.Contains(run.calls, "assemble ") {
		t.Errorf("second install at the same stamp re-assembled (exit %d); calls:\n%s", run.exitCode, run.calls)
	}
	if !strings.Contains(run.output, "Assets already present") {
		t.Errorf("second install did not report reusing the assets:\n%s", run.output)
	}

	bumped := map[string]string{}
	for k, v := range microVMEnv {
		bumped[k] = v
	}
	bumped["STAMP"] = "kata=4.4.0"
	run = runMicroVMDeps(t, root, bumped, nil, "--install")
	if run.exitCode != 0 || !strings.Contains(run.calls, "assemble ") {
		t.Errorf("install after a version bump did not re-assemble (exit %d); calls:\n%s", run.exitCode, run.calls)
	}
}

// Inputs that would stage into the wrong place, or nowhere, fail before
// anything is downloaded, uploaded or applied.
func TestInstallMicroVMDepsFailsEarly(t *testing.T) {
	noBucket := map[string]string{}
	for k, v := range microVMEnv {
		if k != "BUCKET_NAME" {
			noBucket[k] = v
		}
	}
	for name, tc := range map[string]struct {
		root  string
		vars  map[string]string
		rules []rule
		want  string
	}{
		"no BUCKET_NAME":       {fakeSubstrate(t), noBucket, nil, "BUCKET_NAME is not set"},
		"not a Substrate tree": {t.TempDir(), microVMEnv, nil, "is not an agent-substrate/substrate"},
		"no asset bucket": {fakeSubstrate(t), microVMEnv,
			[]rule{{"storage buckets describe", "1", "", "ERROR: (gcloud.storage.buckets.describe) gs://acme-snapshots-assets not found: 404."}},
			"cannot read gs://acme-snapshots-assets"},
	} {
		t.Run(name, func(t *testing.T) {
			run := runMicroVMDeps(t, tc.root, tc.vars, tc.rules, "--install")
			if run.exitCode == 0 || !strings.Contains(run.output, tc.want) {
				t.Errorf("exit %d, want non-zero with %q:\n%s", run.exitCode, tc.want, run.output)
			}
			for _, side := range []string{"assemble ", "storage cp", "apply"} {
				if strings.Contains(run.calls, side) {
					t.Errorf("ran %q before failing; calls:\n%s", side, run.calls)
				}
			}
		})
	}
}

// A WorkerPool that uses the config holds it with a finalizer, so delete
// leaves it and names the pool instead of hanging on a Terminating object.
func TestDeleteMicroVMDepsKeepsAConfigInUse(t *testing.T) {
	inUse := map[string]string{"WORKERPOOLS": "default/counter-microvm gvisor-default microvm"}
	for k, v := range microVMEnv {
		inUse[k] = v
	}
	run := runMicroVMDeps(t, t.TempDir(), inUse, nil, "--delete")
	if run.exitCode != 0 || strings.Contains(run.calls, "delete") {
		t.Errorf("delete with a pool using the config: exit %d; calls:\n%s", run.exitCode, run.calls)
	}
	if !strings.Contains(run.output, "default/counter-microvm") {
		t.Errorf("delete did not name the pool using the config:\n%s", run.output)
	}

	// Delete needs no Substrate checkout: it removes the config by name.
	run = runMicroVMDeps(t, t.TempDir(), microVMEnv, nil, "--delete")
	if run.exitCode != 0 || !strings.Contains(run.calls, "kubectl delete --ignore-not-found --timeout=60s sandboxconfig microvm") {
		t.Errorf("delete with no users: exit %d; calls:\n%s", run.exitCode, run.calls)
	}
}
