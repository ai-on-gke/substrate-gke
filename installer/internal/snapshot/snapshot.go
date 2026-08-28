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

// Package snapshot locates the vendored agent-substrate/substrate tree and
// builds the command invocations the wizard executes against it.
package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// Find resolves the vendored substrate tree. An explicit path wins; otherwise
// it walks up from the working directory looking for a substrate/ directory
// containing go.mod (so the installer works from the repo root, from
// installer/, or from a built binary run inside the repo).
func Find(explicit string) (string, error) {
	if explicit != "" {
		if isSnapshot(explicit) {
			return filepath.Abs(explicit)
		}
		return "", fmt.Errorf("%s does not look like a substrate snapshot (no go.mod)", explicit)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "substrate")
		if isSnapshot(candidate) {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find the vendored substrate/ tree; run from the substrate-gke repo or pass --substrate-root")
		}
		dir = parent
	}
}

func isSnapshot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// VendoredCommit reads the commit recorded by hack/vendor-substrate.sh,
// shortened for display. Returns "unknown" when absent.
func VendoredCommit(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "VENDORED_COMMIT"))
	if err != nil {
		return "unknown"
	}
	c := strings.TrimSpace(string(b))
	if len(c) > 12 {
		c = c[:12]
	}
	if c == "" {
		return "unknown"
	}
	return c
}

// Builder assembles execx.Specs rooted at the snapshot.
type Builder struct {
	Root string
	// Version stamps the images ko builds (the snapshot has no .git for
	// `git describe` to consult).
	Version string
}

// NewBuilder returns a Builder whose Version is derived from the vendored
// commit.
func NewBuilder(root string) *Builder {
	return &Builder{Root: root, Version: "vendored-" + VendoredCommit(root)}
}

// env builds the environment both upstream tools read, mirroring
// hack/ate-dev-env.sh.example. NO_DEV_ENV keeps ate-setup from sourcing a
// developer's .ate-dev-env.sh out from under the wizard's answers.
func (b *Builder) env(st *state.Setup) []string {
	return []string{
		"PROJECT_ID=" + st.ProjectID,
		"PROJECT_NUMBER=" + st.ProjectNumber,
		"GCE_REGION=" + st.Region(),
		"CLUSTER_LOCATION=" + st.Zone,
		"CLUSTER_NAME=" + st.ClusterName,
		"NETWORK=" + st.Network,
		"SUBNETWORK=" + st.Subnetwork,
		"GVISOR_NODE_MACHINE_TYPE=" + st.MachineType,
		"BUCKET_NAME=" + st.BucketName,
		"KO_DOCKER_REPO=" + st.KoDockerRepo,
		"KO_DEFAULTPLATFORMS=linux/amd64",
		"NO_DEV_ENV=1",
		"VERSION=" + b.Version,
	}
}

// Bootstrap provisions GCP resources (APIs, cluster, bucket, IAM,
// dashboards) via the vendored tools/setup-gcp. All seven steps are
// idempotent, so it is safe to run against an existing cluster.
func (b *Builder) Bootstrap(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label:   "setup-gcp bootstrap",
		Display: "go run ./tools/setup-gcp bootstrap",
		Dir:     b.Root,
		Argv:    []string{"go", "run", "./tools/setup-gcp", "bootstrap"},
		Env:     b.env(st),
		SimLines: []string{
			"Step 1/7: Enabling required APIs...",
			"Step 2/7: Creating GKE Cluster...",
			"Step 3/7: Creating GCS Bucket for snapshots...",
			"Step 4/7: Granting GKE Node permissions...",
			"Step 5/7: Granting Atelet permissions...",
			"Step 6/7: Creating IAM policy bindings for bucket...",
			"Step 7/7: Creating Monitoring Dashboards...",
			"Bootstrap completed successfully.",
		},
	}
}

// DeployAteSystem installs the Substrate control plane with the vendored
// cmd/ate-setup. ate-setup fetches cluster credentials itself from
// PROJECT_ID/CLUSTER_NAME/CLUSTER_LOCATION, and ko builds and pushes the
// control-plane images from the snapshot source.
func (b *Builder) DeployAteSystem(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label:   "ate-setup deploy ate-system",
		Display: "go run ./cmd/ate-setup deploy ate-system",
		Dir:     b.Root,
		Argv:    []string{"go", "run", "./cmd/ate-setup", "deploy", "ate-system"},
		Env:     b.env(st),
		SimLines: []string{
			"[step]: deploy_ate_system",
			"[step]: deploy_crds",
			"[step]: ensure_apiserver_prerequisites",
			"[step]: create_jwt_authority_pool_secret",
			"[step]: create_actor_id_ca_pool_secret",
			"[step]: create_podcertificate_controller_cas",
			"[step]: create_api_server_env_vars",
			"[step]: create_api_authentication_config",
			"Building github.com/agent-substrate/substrate/cmd/ate-api-server",
			"Building github.com/agent-substrate/substrate/cmd/atelet",
			"[step]: Waiting for ATE system components to be ready...",
			"deployment \"ate-api-server\" successfully rolled out",
			"daemon set \"atelet\" successfully rolled out",
		},
	}
}

// DeployDemo deploys one of the upstream demo applications (for the wizard,
// the counter demo).
func (b *Builder) DeployDemo(st *state.Setup, name string) execx.Spec {
	return execx.Spec{
		Label:   "ate-setup deploy demo " + name,
		Display: "go run ./cmd/ate-setup deploy demo " + name,
		Dir:     b.Root,
		Argv:    []string{"go", "run", "./cmd/ate-setup", "deploy", "demo", name},
		Env:     b.env(st),
		SimLines: []string{
			"[step]: deploy_demo_" + name,
			"workerpool.ate.dev/ate-demo-" + name + " created",
			"actortemplate.ate.dev/" + name + " created",
		},
	}
}

// EnableAutoscaling turns on cluster autoscaling for a node pool.
func (b *Builder) EnableAutoscaling(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label: "enable node-pool autoscaling",
		Display: fmt.Sprintf(
			"gcloud container node-pools update %s --cluster=%s --location=%s --enable-autoscaling --min-nodes=%d --max-nodes=%d",
			st.NodePool, st.ClusterName, st.Zone, st.AutoscaleMin, st.AutoscaleMax),
		Dir: b.Root,
		Argv: []string{
			"gcloud", "container", "node-pools", "update", st.NodePool,
			"--project=" + st.ProjectID,
			"--cluster=" + st.ClusterName,
			"--location=" + st.Zone,
			"--enable-autoscaling",
			fmt.Sprintf("--min-nodes=%d", st.AutoscaleMin),
			fmt.Sprintf("--max-nodes=%d", st.AutoscaleMax),
		},
		SimLines: []string{
			"Updating node pool " + st.NodePool + "...",
			"Updated [" + st.ClusterName + "/" + st.NodePool + "].",
		},
	}
}

// Verify lists the control-plane pods so the user sees Substrate running.
func (b *Builder) Verify(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label:   "verify ate-system",
		Display: "kubectl get pods -n ate-system",
		Dir:     b.Root,
		Argv:    []string{"kubectl", "get", "pods", "-n", "ate-system", "-o", "wide"},
		SimLines: []string{
			"NAME                              READY   STATUS    RESTARTS   AGE",
			"ate-api-server-6f4d9c-x2lkq       1/1     Running   0          2m",
			"ate-controller-58fd7b-mm4tw       1/1     Running   0          2m",
			"atelet-9kzql                      1/1     Running   0          2m",
			"atenet-router-7f66d8-qq2vr        1/1     Running   0          2m",
			"postgres-0                        1/1     Running   0          3m",
		},
	}
}
