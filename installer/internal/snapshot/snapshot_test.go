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
	"path/filepath"
	"slices"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

func fakeSnapshot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "substrate")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":          "module github.com/agent-substrate/substrate\n",
		"VENDORED_COMMIT": "0123456789abcdef0123\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindExplicitAndWalkUp(t *testing.T) {
	root := fakeSnapshot(t)

	if got, err := Find(root); err != nil || got == "" {
		t.Fatalf("Find(explicit) = %q, %v", got, err)
	}

	// From a subdirectory of the repo, Find should walk up to substrate/.
	sub := filepath.Join(filepath.Dir(root), "installer", "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	got, err := Find("")
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("Find(walk) = %q, want %q", got, root)
	}
}

func TestFindRejectsNonSnapshot(t *testing.T) {
	if _, err := Find(t.TempDir()); err == nil {
		t.Fatal("want an error for a directory without go.mod")
	}
}

func TestVendoredCommitShortens(t *testing.T) {
	root := fakeSnapshot(t)
	if got := VendoredCommit(root); got != "0123456789ab" {
		t.Fatalf("VendoredCommit = %q", got)
	}
	if got := VendoredCommit(t.TempDir()); got != "unknown" {
		t.Fatalf("VendoredCommit(missing) = %q", got)
	}
}

func testSetup() *state.Setup {
	st := state.NewSetup()
	st.ProjectID = "acme"
	st.ProjectNumber = "42"
	st.ApplyProjectDefaults()
	return st
}

func TestBuilderEnvCarriesTheDevEnvContract(t *testing.T) {
	b := NewBuilder(fakeSnapshot(t))
	spec := b.Bootstrap(testSetup())

	for _, want := range []string{
		"PROJECT_ID=acme",
		"PROJECT_NUMBER=42",
		"CLUSTER_NAME=substrate-poc",
		"CLUSTER_LOCATION=us-west1-c",
		"GCE_REGION=us-west1",
		"BUCKET_NAME=substrate-snapshots-acme-substrate-poc",
		"KO_DOCKER_REPO=gcr.io/acme/ate-images",
		"NO_DEV_ENV=1",
		"VERSION=vendored-0123456789ab",
	} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("Bootstrap env missing %q", want)
		}
	}
	if spec.Argv[0] != "go" || spec.Argv[2] != "./tools/setup-gcp" {
		t.Errorf("unexpected argv: %v", spec.Argv)
	}
}

func TestDeploySpecs(t *testing.T) {
	b := NewBuilder(fakeSnapshot(t))
	st := testSetup()

	deploy := b.DeployAteSystem(st)
	if got := deploy.Argv; got[2] != "./cmd/ate-setup" || got[3] != "deploy" || got[4] != "ate-system" {
		t.Errorf("deploy argv = %v", got)
	}
	if deploy.Dir != b.Root {
		t.Errorf("deploy must run from the snapshot root, got %q", deploy.Dir)
	}

	demo := b.DeployDemo(st, "counter")
	if got := demo.Argv[len(demo.Argv)-1]; got != "counter" {
		t.Errorf("demo argv = %v", demo.Argv)
	}

	st.AutoscaleMin, st.AutoscaleMax = 2, 9
	scale := b.EnableAutoscaling(st)
	if !slices.Contains(scale.Argv, "--min-nodes=2") || !slices.Contains(scale.Argv, "--max-nodes=9") {
		t.Errorf("autoscaling argv = %v", scale.Argv)
	}
}
