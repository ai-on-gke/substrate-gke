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

package gkeoverlay

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// fixture is a cut-down manifests/ate-install as upstream will ship it once
// its GKE content has moved here: atelet tolerates only the sandbox-class
// taint, ate-otel-config has no collector endpoint, and there is no router
// PodMonitoring.
var fixture = map[string]string{
	BaseKustomization: `# Header comment that must survive the edit.
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

# Comment above resources.
resources:
  - ../atelet.yaml
  - ../ate-otel-config.yaml
  - ../atenet-router.yaml
`,
	installDir + "/atelet.yaml": `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: atelet
  namespace: ate-system
spec:
  selector:
    matchLabels:
      app: atelet
  template:
    metadata:
      labels:
        app: atelet
    spec:
      tolerations:
      - key: ate.dev/sandboxClass
        operator: Exists
        effect: NoSchedule
      containers:
      - name: atelet
        image: atelet
`,
	installDir + "/ate-otel-config.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: ate-otel-config
  namespace: ate-system
data:
  OTEL_METRICS_EXPORTER: otlp
`,
	installDir + "/atenet-router.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: atenet-router
  namespace: ate-system
spec:
  selector:
    matchLabels:
      app: atenet-router
  template:
    metadata:
      labels:
        app: atenet-router
    spec:
      containers:
      - name: atenet-router
        image: atenet
`,
}

// newTree writes fixture into a temporary directory and returns it.
func newTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range fixture {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// overlay is this repository's deploy/gke, so the tests check the files the
// installer ships rather than a copy of them.
func overlay(t *testing.T) string {
	t.Helper()
	dir, err := Find("", ".")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// snapshotFiles reads every file under root, keyed by relative path.
func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestAddIsIdempotent(t *testing.T) {
	root, src := newTree(t), overlay(t)
	got, err := Add(root, src)
	if err != nil || got != Added {
		t.Fatalf("first Add = %v, %v; want Added", got, err)
	}
	first := snapshotFiles(t, root)
	got, err = Add(root, src)
	if err != nil || got != AlreadyPresent {
		t.Fatalf("second Add = %v, %v; want AlreadyPresent", got, err)
	}
	second := snapshotFiles(t, root)
	if len(first) != len(second) {
		t.Fatalf("second Add changed the file set: %d files, then %d", len(first), len(second))
	}
	for path, body := range first {
		if second[path] != body {
			t.Errorf("second Add rewrote %s:\n%s\nthen:\n%s", path, body, second[path])
		}
	}
	base := second[BaseKustomization]
	for _, entry := range []string{componentEntry, resourcesEntry + "atenet-router-monitoring.yaml"} {
		if n := strings.Count(base, entry); n != 1 {
			t.Errorf("%s lists %s %d times, want once:\n%s", BaseKustomization, entry, n, base)
		}
	}
	for _, comment := range []string{"# Header comment that must survive the edit.", "# Comment above resources."} {
		if !strings.Contains(base, comment) {
			t.Errorf("the edit dropped %q:\n%s", comment, base)
		}
	}
	if _, ok := second[filepath.Join(ComponentDir, "kustomization.yaml")]; !ok {
		t.Errorf("Add did not copy the Component to %s", ComponentDir)
	}
	if _, ok := second[filepath.Join(ResourcesDir, "atenet-router-monitoring.yaml")]; !ok {
		t.Errorf("Add did not copy the PodMonitoring to %s", ResourcesDir)
	}
}

// A tree that still ships upstream's PodMonitoring (v0.4.0 to v0.6.0, and
// main until the GKE content moves) also still has the toleration and the
// endpoint. Adding deploy/gke there would duplicate them.
func TestAddSkipsTreeWithCoreGKEContent(t *testing.T) {
	root := newTree(t)
	core := filepath.Join(root, CoreMonitoring)
	if err := os.WriteFile(core, []byte("kind: PodMonitoring\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotFiles(t, root)
	got, err := Add(root, overlay(t))
	if err != nil || got != NotNeeded {
		t.Fatalf("Add = %v, %v; want NotNeeded", got, err)
	}
	after := snapshotFiles(t, root)
	if len(before) != len(after) {
		t.Errorf("Add changed the file set of a tree that ships its own GKE content")
	}
	for path, body := range before {
		if after[path] != body {
			t.Errorf("Add changed %s in a tree that ships its own GKE content", path)
		}
	}
}

func TestAddRejectsTreeWithoutBase(t *testing.T) {
	if _, err := Add(t.TempDir(), overlay(t)); err == nil {
		t.Fatal("Add succeeded on a directory that is not a substrate tree")
	}
}

// A file an older deploy/gke had and the current one does not is removed, so
// a cached tree never keeps a patch this repository dropped.
func TestAddRemovesStaleFiles(t *testing.T) {
	root := newTree(t)
	if _, err := Add(root, overlay(t)); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ComponentDir, "old-patch.yaml")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Add(root, overlay(t))
	if err != nil || got != Added {
		t.Fatalf("Add = %v, %v; want Added", got, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("Add left %s behind", stale)
	}
}

// build renders the kustomization at dir the way ate-setup's kustomize.Build
// does: kustomize's API with the load restrictor off.
func build(t *testing.T, dir string) resmap.ResMap {
	t.Helper()
	opts := krusty.MakeDefaultOptions()
	opts.LoadRestrictions = types.LoadRestrictionsNone
	m, err := krusty.MakeKustomizer(opts).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		t.Fatalf("kustomize build %s: %v", dir, err)
	}
	return m
}

// find returns the one resource of the given kind and name.
func find(t *testing.T, m resmap.ResMap, kind, name string) *yaml.RNode {
	t.Helper()
	for _, r := range m.Resources() {
		if r.GetKind() == kind && r.GetName() == name {
			node, err := yaml.Parse(r.MustString())
			if err != nil {
				t.Fatal(err)
			}
			return node
		}
	}
	t.Fatalf("no %s %s in the build", kind, name)
	return nil
}

func TestAddedTreeBuilds(t *testing.T) {
	root := newTree(t)
	if _, err := Add(root, overlay(t)); err != nil {
		t.Fatal(err)
	}
	m := build(t, filepath.Join(root, filepath.Dir(BaseKustomization)))

	tolerations, err := find(t, m, "DaemonSet", "atelet").Pipe(yaml.Lookup("spec", "template", "spec", "tolerations"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, el := range tolerations.Content() {
		keys = append(keys, yaml.NewRNode(el).Field("key").Value.YNode().Value)
	}
	if strings.Join(keys, ",") != "ate.dev/sandboxClass,cloud.google.com/compute-class" {
		t.Errorf("atelet tolerations = %v, want atelet's own followed by the ComputeClass one", keys)
	}

	endpoint, err := find(t, m, "ConfigMap", "ate-otel-config").Pipe(yaml.Lookup("data", "OTEL_EXPORTER_OTLP_ENDPOINT"))
	if err != nil || endpoint == nil {
		t.Fatalf("ate-otel-config has no OTEL_EXPORTER_OTLP_ENDPOINT: %v", err)
	}
	if got := endpoint.YNode().Value; got != "http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:4317" {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want GKE's managed collector", got)
	}
	exporter, _ := find(t, m, "ConfigMap", "ate-otel-config").Pipe(yaml.Lookup("data", "OTEL_METRICS_EXPORTER"))
	if exporter == nil || exporter.YNode().Value != "otlp" {
		t.Errorf("the endpoint patch dropped ate-otel-config's other keys")
	}

	find(t, m, "PodMonitoring", "atenet-router-envoy")
}

// The Component must stay usable on its own, so it can later be handed to
// `ate-setup deploy --component` unchanged: patches only, and harmless on a
// stream without its targets.
func TestComponentIsPatchesOnly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(overlay(t), componentSrc, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k types.Kustomization
	if err := yaml.Unmarshal(data, &k); err != nil {
		t.Fatal(err)
	}
	if k.Kind != types.ComponentKind {
		t.Errorf("kind = %q, want %q", k.Kind, types.ComponentKind)
	}
	if len(k.Resources) > 0 || len(k.Components) > 0 || len(k.ConfigMapGenerator) > 0 || len(k.SecretGenerator) > 0 {
		t.Errorf("the Component adds resources; ate-setup's deploy --component rejects that")
	}

	// Compose it over a stream with neither target.
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n  namespace: ate-system\n")
	write(t, filepath.Join(dir, "kustomization.yaml"), "resources:\n- cm.yaml\ncomponents:\n- "+rel(t, dir, filepath.Join(overlay(t), componentSrc))+"\n")
	if n := build(t, dir).Size(); n != 1 {
		t.Errorf("composing over an unrelated stream produced %d objects, want 1", n)
	}
}

func TestCloudSQLComponentBuilds(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "deploy.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ate-api-server
  namespace: ate-system
spec:
  selector:
    matchLabels:
      app: ate-api-server
  template:
    metadata:
      labels:
        app: ate-api-server
    spec:
      containers:
      - name: ateapi
        image: ateapi
`)
	write(t, filepath.Join(dir, "kustomization.yaml"), "resources:\n- deploy.yaml\ncomponents:\n- "+rel(t, dir, filepath.Join(overlay(t), "components", "cloudsql"))+"\n")
	init, err := find(t, build(t, dir), "Deployment", "ate-api-server").Pipe(yaml.Lookup("spec", "template", "spec", "initContainers", "[name=cloud-sql-proxy]", "restartPolicy"))
	if err != nil || init == nil || init.YNode().Value != "Always" {
		t.Errorf("ate-api-server has no cloud-sql-proxy native sidecar: %v", err)
	}
}

// rel returns target relative to dir, through symlinks: kustomize refuses
// absolute component paths.
func rel(t *testing.T, dir, target string) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	tg, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	r, err := filepath.Rel(d, tg)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(r)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// In an installer-fetched tree the overlay must not show in `git status`:
// Go would stamp every binary built from the tree as modified.
func TestHideFromGitKeepsTreeClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := newTree(t)
	gitRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gitRun("init", "-q")
	gitRun("add", ".")
	gitRun("commit", "-q", "-m", "fixture")

	if _, err := Add(root, overlay(t)); err != nil {
		t.Fatal(err)
	}
	if status := gitRun("status", "--porcelain"); status == "" {
		t.Fatal("Add left no change for git to see; the test proves nothing")
	}
	for range 2 {
		if err := HideFromGit(root); err != nil {
			t.Fatal(err)
		}
	}
	if status := gitRun("status", "--porcelain"); status != "" {
		t.Errorf("git status after HideFromGit:\n%s", status)
	}
	exclude, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(exclude), "/"+ComponentDir+"/"); n != 1 {
		t.Errorf(".git/info/exclude lists %s %d times, want once:\n%s", ComponentDir, n, exclude)
	}
}

func TestFindFailsClearly(t *testing.T) {
	_, err := Find("", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), Dir) {
		t.Errorf("Find outside a checkout = %v; want an error naming %s", err, Dir)
	}
	if _, err := Find(t.TempDir()); err == nil {
		t.Error("Find accepted an explicit directory without deploy/gke's layout")
	}
}

// Every line starts with LinePrefix, which the deploy checklist keys off.
func TestApplyAndCheckReport(t *testing.T) {
	root := newTree(t)
	var out strings.Builder
	if err := Check(&out, root, "ADD-COMMAND"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, LinePrefix+"warning:") || !strings.Contains(got, "ADD-COMMAND") {
		t.Errorf("Check on a tree that needs the overlay printed:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, ComponentDir)); !os.IsNotExist(err) {
		t.Errorf("Check modified the tree")
	}

	out.Reset()
	if err := Apply(&out, root, overlay(t), false); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, LinePrefix) {
			t.Errorf("Apply printed a line without LinePrefix: %q", line)
		}
	}
	if !strings.Contains(out.String(), msgAdded) {
		t.Errorf("Apply did not report the addition:\n%s", out.String())
	}

	out.Reset()
	if err := Check(&out, root, "ADD-COMMAND"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != LinePrefix+msgPresent+"\n" {
		t.Errorf("Check after Apply printed %q", got)
	}
}

// An entry for a resource deploy/gke no longer has is dropped with the file,
// or kustomize would fail on the missing file.
func TestAddDropsStaleResourceEntries(t *testing.T) {
	root := newTree(t)
	base := filepath.Join(root, BaseKustomization)
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	write(t, base, string(data)+"  - "+resourcesEntry+"dropped.yaml\n")
	if _, err := Add(root, overlay(t)); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dropped.yaml") || !strings.Contains(string(data), "../atelet.yaml") {
		t.Errorf("Add should drop only the stale substrate-gke entry:\n%s", data)
	}
	build(t, filepath.Dir(base))
}
