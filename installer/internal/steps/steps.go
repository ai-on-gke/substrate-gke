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

// Bootstrap tracks the seven idempotent phases of `setup-gcp bootstrap`,
// preceded by the pinned substrate fetch this step pays for on a cold cache.
func Bootstrap() []ChecklistItem {
	return []ChecklistItem{
		{"Fetch the pinned substrate checkout", containsAny(snapshot.FetchLine, snapshot.CachedLine)},
		{"Enable required GCP APIs", contains("Step 1/7")},
		{"Create the GKE cluster (with PodCertificate beta APIs)", contains("Step 2/7")},
		{"Create the GCS snapshot bucket", contains("Step 3/7")},
		{"Grant GKE node permissions", contains("Step 4/7")},
		{"Grant atelet Workload Identity permissions", contains("Step 5/7")},
		{"Bind bucket IAM policies", contains("Step 6/7")},
		{"Create Cloud Monitoring dashboards", contains("Step 7/7")},
	}
}

// Deploy tracks the phases of `ate-setup deploy ate-system`, keyed off its
// `[step]:` markers and ko's build progress. A pre-built install has no build
// phase at all — resolving a published tag to a digest prints nothing — so
// that row is left out rather than left permanently unlit.
func Deploy(prebuilt bool) []ChecklistItem {
	items := []ChecklistItem{
		{"Apply namespace, CRDs, and RBAC", contains("deploy_crds")},
		{"Generate CAs, JWT pools, and API server config", contains("ensure_apiserver_prerequisites")},
	}
	if !prebuilt {
		items = append(items, ChecklistItem{
			"Build control-plane images from source (ko)", containsAny("Building github.com/", "Publishing "),
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
