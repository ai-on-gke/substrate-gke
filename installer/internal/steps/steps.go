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

// Package steps defines the live checklists for the long-running install
// commands. Each item names a phase and matches the output line that marks
// the phase starting; when a later item matches, earlier items are complete.
package steps

import (
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/gkeoverlay"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
)

// ChecklistItem is one row of a step's progress checklist.
type ChecklistItem struct {
	Label string
	// Match reports whether an output line marks this phase as started.
	Match func(line string) bool
}

func contains(sub string) func(string) bool {
	return func(line string) bool { return strings.Contains(line, sub) }
}

func containsAny(subs ...string) func(string) bool {
	return func(line string) bool {
		for _, s := range subs {
			if strings.Contains(line, s) {
				return true
			}
		}
		return false
	}
}

// Bootstrap tracks the idempotent phases of `setup-gcp bootstrap`,
// preceded by the substrate fetch this step pays for on a cold cache.
// fetchLabel names that fetch; see snapshot.Builder.FetchLabel.
//
// The cluster phase is the one whose work depends on the cluster, so its label
// does too. setup-gcp logs "Creating GKE Cluster" either way, but on an
// existing cluster it creates nothing: it either checks the cluster, or turns
// the beta APIs on in a control-plane update the user was just told to expect.
func Bootstrap(fetchLabel string, artifactRegistry, clusterIsNew, enableBetaAPIs bool) []ChecklistItem {
	clusterStep := "Check the existing GKE cluster"
	switch {
	case clusterIsNew:
		clusterStep = "Create the GKE cluster (with the beta PodCertificate APIs)"
	case enableBetaAPIs:
		clusterStep = "Turn on the beta PodCertificate APIs (control-plane update)"
	}
	items := []ChecklistItem{
		{fetchLabel, containsAny(snapshot.FetchLine, snapshot.CachedLine)},
		{"Enable required GCP APIs", contains("Enabling required APIs")},
	}
	if artifactRegistry {
		items = append(items, ChecklistItem{"Create the Artifact Registry repository", contains("Creating Artifact Registry repository")})
	}
	return append(items,
		ChecklistItem{clusterStep, contains("Creating GKE Cluster")},
		ChecklistItem{"Create the GCS snapshot and asset buckets", contains("Creating GCS buckets for snapshots")},
		ChecklistItem{"Grant GKE node permissions", contains("Granting GKE Node permissions")},
		ChecklistItem{"Grant atelet Workload Identity permissions", contains("Granting Atelet permissions")},
		ChecklistItem{"Create the snapshot transfer service account", contains("Creating the snapshot transfer service account")},
		ChecklistItem{"Bind bucket IAM policies", contains("Creating IAM policy bindings for the buckets")},
		ChecklistItem{"Create Cloud Monitoring dashboards", contains("Creating Monitoring Dashboards")},
	)
}

// Deploy tracks the phases of `ate-setup deploy ate-system`, keyed off its
// `[step]:` markers and ko's build progress, preceded by the check that adds
// this repository's deploy/gke to the tree when the tree lacks upstream's GKE
// manifests (see gkeoverlay). A pre-built install has no build phase at all —
// resolving a published tag to a digest prints nothing — so that row is left
// out rather than left permanently unlit.
func Deploy(prebuilt bool) []ChecklistItem {
	items := []ChecklistItem{
		{"Add the GKE manifests (deploy/gke) if the tree lacks them", contains(gkeoverlay.LinePrefix)},
		{"Apply namespace, CRDs, and RBAC", contains("deploy_crds")},
		{"Generate CAs, JWT pools, and API server config", contains("ensure_apiserver_prerequisites")},
	}
	if !prebuilt {
		items = append(items, ChecklistItem{
			"Build control-plane images from source (ko)", containsAny("Building github.com/", "Building go.ate.dev/", "Publishing "),
		})
	}
	return append(items, ChecklistItem{
		"Wait for components: postgres, api-server, controller, atenet, atelet", contains("Waiting for ATE system components"),
	})
}

// FilestoreCSI tracks the phases of deploying the Filestore CSI driver substrate overlay.
func FilestoreCSI() []ChecklistItem {
	return []ChecklistItem{
		{"Disable managed Filestore CSI driver addon (if enabled)", containsAny("Disabling managed", "GcpFilestoreCsiDriver=DISABLED", "Updating [")},
		{"Fetch Filestore CSI driver overlay repository", containsAny("Cloning into", "Fetching")},
		{"Configure Service Account & IAM Bindings", contains("Configuring Service Account")},
		{"Apply Kustomize overlay", contains("Applying Substrate Filestore CSI Driver Overlay")},
	}
}

// MicroVMDeps tracks the three phases of `install-microvm-deps.sh --install`,
// preceded by the same fetch the other in-tree steps pay for, named by
// fetchLabel.
//
// The assemble row matches either outcome: a first run builds the asset set,
// and a later one finds it already in bin/microvm-assets and skips. Both mean
// the phase is done, and leaving the row unlit on the fast path would read as
// a stall on the step's slowest phase.
func MicroVMDeps(fetchLabel string) []ChecklistItem {
	return []ChecklistItem{
		{fetchLabel, containsAny(snapshot.FetchLine, snapshot.CachedLine)},
		{"Assemble the micro-VM asset set", containsAny("Assembling micro-VM assets", "Assets already present", "re-assembling")},
		{"Upload assets to GCS (kata-assets/)", contains("Uploading assets to gs://")},
		{"Apply the cluster-wide microvm SandboxConfig", contains("Applying microvm SandboxConfig")},
	}
}

// Progress applies one output line to a checklist, returning the updated
// index of the active item (-1 when nothing has matched yet). Matches only
// ever move the cursor forward.
func Progress(items []ChecklistItem, active int, line string) int {
	for i := len(items) - 1; i > active; i-- {
		if items[i].Match(line) {
			return i
		}
	}
	return active
}
