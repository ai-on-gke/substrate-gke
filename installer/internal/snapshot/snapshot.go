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

// Package snapshot resolves the pinned agent-substrate/substrate checkout and
// builds the command invocations the wizard executes against it.
//
// The tree is fetched on demand rather than vendored into this repo, the same
// way DeployFilestoreCSI fetches the Filestore CSI driver overlay: a shallow
// git fetch of exactly the revision needed. Unlike that clone, this one is
// pinned to a commit and cached, because several wizard steps run against it.
package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

const (
	// RepoURL is the upstream Substrate repository. It is public, so the
	// fetch needs no credentials.
	RepoURL = "https://github.com/agent-substrate/substrate.git"

	// Commit pins the upstream revision the installer builds from. Bump this
	// to move to a newer Substrate, and update MinGoVersion to match the `go`
	// directive in that revision's go.mod.
	Commit = "4a2cb262dd62ea3504f93d5f98aad25e62d58d05"

	// MinGoVersion mirrors the `go` directive in go.mod at Commit. The doctor
	// prefers the real go.mod once the tree is on disk and falls back to this
	// when checking Go before the first fetch.
	MinGoVersion = "1.27.0"
)

// ShortCommit is Commit abbreviated for display.
func ShortCommit() string { return Commit[:12] }

// Root returns the directory holding the Substrate tree. An explicit path
// wins and must already be a checkout; otherwise the pinned tree lives in a
// per-commit directory under the user cache, so bumping Commit fetches into a
// fresh directory instead of mutating the old one.
//
// The returned path need not exist yet — the install steps fetch it on first
// use. Managed reports whether the installer owns that fetch.
func Root(explicit string) (path string, managed bool, err error) {
	if explicit != "" {
		if !isCheckout(explicit) {
			return "", false, fmt.Errorf("%s does not look like a substrate checkout (no go.mod)", explicit)
		}
		abs, err := filepath.Abs(explicit)
		return abs, false, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", false, fmt.Errorf("cannot locate a cache directory for the substrate checkout: %w", err)
	}
	return filepath.Join(cache, "substrate-gke", treePrefix+ShortCommit()), true, nil
}

func isCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// CompleteMarker is written at the top of a managed checkout once its fetch
// has finished. Presence of the marker — not of go.mod — is what makes the
// cache trustworthy: git checks files out in path order, so an interrupted
// checkout can leave go.mod on disk without internal/ or vendor/, and keying
// the cache on go.mod would treat that half-tree as good forever.
const CompleteMarker = ".substrate-gke-complete"

// Fetched reports whether the tree at root is ready to build from. A managed
// checkout must carry its completion marker; a user-supplied one only has to
// look like a checkout, since the installer never writes to it.
func Fetched(root string, managed bool) bool {
	if !managed {
		return isCheckout(root)
	}
	_, err := os.Stat(filepath.Join(root, CompleteMarker))
	return err == nil
}

// treePrefix names the per-commit cache directories, and is what Cleanup uses
// to recognise trees this installer created.
const treePrefix = "substrate-"

// Cleanup reclaims cache disk after a successful install: the shallow pack the
// working tree was checked out from, plus any trees and staging directories
// left behind by earlier pins. The working tree itself stays, since the exit
// summary points teardown at it.
//
// Both are deliberately deferred to a successful finish rather than done at
// fetch time. While the install can still be retried there is no way to know
// what a retry will need, and deleting is not reversible; once it has worked,
// nothing is going to ask for the pack again.
//
// It never touches a user-supplied checkout — that tree, and its history,
// belong to the user.
func (b *Builder) Cleanup() error {
	if !b.Managed {
		return nil
	}
	errs := []error{os.RemoveAll(filepath.Join(b.Root, ".git"))}

	base, keep := filepath.Dir(b.Root), filepath.Base(b.Root)
	entries, err := os.ReadDir(base)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, e := range entries {
		// Only ever remove siblings we would have created ourselves: trees
		// from other pins, and the ".partial" a killed fetch leaves behind.
		if name := e.Name(); name != keep && strings.HasPrefix(name, treePrefix) {
			errs = append(errs, os.RemoveAll(filepath.Join(base, name)))
		}
	}
	return errors.Join(errs...)
}

// ShellQuote renders s as a single-quoted POSIX shell word. Go's %q produces a
// double-quoted string, which bash still expands — a `$` or a backtick in the
// path would be interpreted rather than taken literally.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Builder assembles execx.Specs rooted at the Substrate checkout.
type Builder struct {
	Root string
	// Managed is true when Root is the pinned checkout the installer fetches
	// itself, false when the user supplied their own tree.
	Managed bool
	// Version stamps the images ko builds (the checkout is detached at a
	// pinned commit, so `git describe` has no tag to report).
	Version string
}

// NewBuilder returns a Builder for the tree at root.
func NewBuilder(root string, managed bool) *Builder {
	version := "substrate-local"
	if managed {
		version = "substrate-" + ShortCommit()
	}
	return &Builder{Root: root, Managed: managed, Version: version}
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

// FetchLine and CachedLine are the two outcomes the fetch preamble reports.
// The install checklists key their "fetch" phase off both, so they are
// constants rather than literals: rewording an echo without updating the
// matcher would silently stop that checklist row from ever lighting up.
const (
	FetchLine  = "Fetching substrate"
	CachedLine = "Using cached substrate@"
)

// ensure returns the shell lines that guarantee the checkout exists and leave
// the shell inside it. Fetching only the pinned commit at depth 1 keeps this
// to a few seconds; a tree the user supplied is used as-is.
//
// The tree is staged in a sibling directory and renamed into place only once
// it is complete, so interrupting a fetch leaves no half-tree that later runs
// would mistake for a good cache. The stage is a mktemp name rather than a
// fixed ".partial": two wizards running at once would otherwise share one
// staging path, and the interleaving where the second re-inits the stage the
// first is about to mark complete publishes an empty tree that, marker and
// all, no later run would ever distrust.
//
// Paths are single-quoted because the shell expands `$` and backticks inside
// double quotes.
func (b *Builder) ensure() []string {
	if !b.Managed {
		return []string{"cd " + ShellQuote(b.Root)}
	}
	return []string{
		"SUBSTRATE_DIR=" + ShellQuote(b.Root),
		fmt.Sprintf(`if [ ! -e "${SUBSTRATE_DIR}/%s" ]; then`, CompleteMarker),
		fmt.Sprintf(`    echo "%s@%s from %s..."`, FetchLine, ShortCommit(), RepoURL),
		`    rm -rf "${SUBSTRATE_DIR}"`,
		`    mkdir -p "$(dirname "${SUBSTRATE_DIR}")"`,
		`    STAGE=$(mktemp -d "${SUBSTRATE_DIR}.partial.XXXXXX")`,
		// Nothing else knows this name, so a failure from here on has to
		// clean up after itself. Ctrl-C needs the second trap: bash dies on
		// an untrapped signal without ever running its EXIT handler, and
		// exiting from the handler is what gets us back to that path.
		`    trap 'rm -rf "${STAGE}"' EXIT`,
		`    trap 'exit 130' INT TERM`,
		`    git -C "${STAGE}" init -q`,
		fmt.Sprintf(`    git -C "${STAGE}" remote add origin %s`, RepoURL),
		fmt.Sprintf(`    git -C "${STAGE}" fetch -q --depth 1 origin %s`, Commit),
		`    git -C "${STAGE}" checkout -q FETCH_HEAD`,
		fmt.Sprintf(`    touch "${STAGE}/%s"`, CompleteMarker),
		// If a concurrent run published first, its tree is as good as ours;
		// renaming onto it would only nest our stage inside it.
		fmt.Sprintf(`    if [ -e "${SUBSTRATE_DIR}/%s" ]; then`, CompleteMarker),
		`        rm -rf "${STAGE}"`,
		`    else`,
		`        mv "${STAGE}" "${SUBSTRATE_DIR}"`,
		`    fi`,
		fmt.Sprintf(`    echo "Fetched substrate@%s"`, ShortCommit()),
		`else`,
		fmt.Sprintf(`    echo "%s%s"`, CachedLine, ShortCommit()),
		`fi`,
		`cd "${SUBSTRATE_DIR}"`,
	}
}

// inTree wraps a command so it runs inside the Substrate checkout, fetching
// the tree first if it is not there yet.
func (b *Builder) inTree(command string) []string {
	lines := append([]string{"set -euo pipefail"}, b.ensure()...)
	lines = append(lines, command)
	return []string{"bash", "-c", strings.Join(lines, "\n")}
}

// fetchSimLines is what --dry-run replays for the fetch preamble.
func (b *Builder) fetchSimLines() []string {
	if !b.Managed {
		return nil
	}
	return []string{CachedLine + ShortCommit()}
}

// Bootstrap provisions GCP resources (APIs, cluster, bucket, IAM,
// dashboards) via the upstream tools/setup-gcp. All seven steps are
// idempotent, so it is safe to run against an existing cluster. This is the
// first step to touch the checkout, so it usually pays the fetch.
func (b *Builder) Bootstrap(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label:   "setup-gcp bootstrap",
		Display: "go run ./tools/setup-gcp bootstrap",
		Argv:    b.inTree("go run ./tools/setup-gcp bootstrap"),
		Env:     b.env(st),
		SimLines: append(b.fetchSimLines(),
			"Step 1/7: Enabling required APIs...",
			"Step 2/7: Creating GKE Cluster...",
			"Step 3/7: Creating GCS Bucket for snapshots...",
			"Step 4/7: Granting GKE Node permissions...",
			"Step 5/7: Granting Atelet permissions...",
			"Step 6/7: Creating IAM policy bindings for bucket...",
			"Step 7/7: Creating Monitoring Dashboards...",
			"Bootstrap completed successfully.",
		),
	}
}

// DeployAteSystem installs the Substrate control plane with the upstream
// cmd/ate-setup. ate-setup fetches cluster credentials itself from
// PROJECT_ID/CLUSTER_NAME/CLUSTER_LOCATION, and ko builds and pushes the
// control-plane images from the checkout's source.
func (b *Builder) DeployAteSystem(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label:   "ate-setup deploy ate-system",
		Display: "go run ./cmd/ate-setup deploy ate-system",
		Argv:    b.inTree("go run ./cmd/ate-setup deploy ate-system"),
		Env:     b.env(st),
		SimLines: append(b.fetchSimLines(),
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
		),
	}
}

// DeployDemo deploys one of the upstream demo applications (for the wizard,
// the counter demo). name is quoted like every other value inTree splices
// into a script: the only caller passes a literal today, but a demo name
// picked from a list or typed in would otherwise reach bash as source.
func (b *Builder) DeployDemo(st *state.Setup, name string) execx.Spec {
	return execx.Spec{
		Label:   "ate-setup deploy demo " + name,
		Display: "go run ./cmd/ate-setup deploy demo " + name,
		Argv:    b.inTree("go run ./cmd/ate-setup deploy demo " + ShellQuote(name)),
		Env:     b.env(st),
		SimLines: append(b.fetchSimLines(),
			"[step]: deploy_demo_"+name,
			"workerpool.ate.dev/ate-demo-"+name+" created",
			"actortemplate.ate.dev/"+name+" created",
		),
	}
}

// DeployFilestoreCSI clones the GCP Filestore CSI Driver repository and invokes
// its Substrate overlay deploy script (deploy/kubernetes/overlays/substrate/deploy.sh).
// It automatically disables the managed GKE Filestore CSI Driver addon if enabled
// to prevent conflicts with the Substrate overlay. It needs no Substrate
// checkout of its own.
func (b *Builder) DeployFilestoreCSI(st *state.Setup) execx.Spec {
	// Wizard answers reach this script as text, so they are quoted for the
	// same reason the fetch preamble quotes its paths.
	project, cluster, zone := ShellQuote(st.ProjectID), ShellQuote(st.ClusterName), ShellQuote(st.Zone)
	deployCmd := fmt.Sprintf("./deploy.sh --project-id %s", project)

	script := strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf(`FILESTORE_ADDON=$(gcloud container clusters describe %s --project=%s --location=%s --format="value(addonsConfig.gcpFilestoreCsiDriverConfig.enabled)" 2>/dev/null || true)`, cluster, project, zone),
		`if [[ "${FILESTORE_ADDON}" == "True" || "${FILESTORE_ADDON}" == "true" ]]; then`,
		`    echo "Disabling managed GKE Filestore CSI Driver addon..."`,
		fmt.Sprintf(`    gcloud container clusters update %s --project=%s --location=%s --update-addons=GcpFilestoreCsiDriver=DISABLED`, cluster, project, zone),
		`fi`,
		`TMP_DIR=$(mktemp -d)`,
		`trap 'rm -rf "${TMP_DIR}"' EXIT`,
		`git clone --depth 1 https://github.com/kubernetes-sigs/gcp-filestore-csi-driver.git "${TMP_DIR}/gcp-filestore-csi-driver"`,
		`cd "${TMP_DIR}/gcp-filestore-csi-driver/deploy/kubernetes/overlays/substrate"`,
		deployCmd,
	}, "\n")

	return execx.Spec{
		Label:   "deploy filestore csi driver",
		Display: deployCmd,
		Argv:    []string{"bash", "-c", script},
		Env:     b.env(st),
		SimLines: []string{
			"Disabling managed GKE Filestore CSI Driver addon...",
			"Updating " + st.ClusterName + "...",
			"Updated [" + st.ClusterName + "].",
			"Cloning into 'gcp-filestore-csi-driver'...",
			"=================================================================",
			"  🚀 GKE Substrate Filestore CSI Driver Overlay Deployment",
			"=================================================================",
			"📋 Configuration:",
			"   - GCP Project ID     : " + st.ProjectID,
			"   - GCP Service Account: substrate-filestore-csi@" + st.ProjectID + ".iam.gserviceaccount.com",
			"=================================================================",
			"🔐 Configuring Service Account & IAM Bindings...",
			"   - Verifying default Service Account existence...",
			"   - Service Account already exists (Skipping creation).",
			"   - Granting 'roles/file.editor' to the default Service Account on project " + st.ProjectID + "...",
			"   - Binding Workload Identity User role to the Kubernetes ServiceAccount...",
			"⚙️  Generating serviceaccount_patch.yaml with service account 'substrate-filestore-csi@" + st.ProjectID + ".iam.gserviceaccount.com'...",
			"📦 Applying Substrate Filestore CSI Driver Overlay via Kustomize...",
			"serviceaccount/gcp-filestore-csi-controller-sa configured",
			"service/csi-filestore-controller created",
			"csidriverconfig.ate.dev/filestore.csi.storage.gke.io created",
			"deployment.apps/gcp-filestore-csi-controller configured",
			"daemonset.apps/gcp-filestore-csi-node configured",
			"=================================================================",
			"✅ Deployment complete!",
			"   - CSI Driver & Substrate CSIDriverConfig applied.",
			"   - Check controller status : kubectl get pods -n gcp-filestore-csi-driver",
			"   - Check Substrate driver  : kubectl get csidriverconfig",
			"=================================================================",
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
