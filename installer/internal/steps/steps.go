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

import "strings"

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

// Bootstrap tracks the seven idempotent phases of `setup-gcp bootstrap`.
func Bootstrap() []ChecklistItem {
	return []ChecklistItem{
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
// `[step]:` markers and ko's build progress.
func Deploy() []ChecklistItem {
	return []ChecklistItem{
		{"Apply namespace, CRDs, and RBAC", contains("deploy_crds")},
		{"Generate CAs, JWT pools, and API server config", contains("ensure_apiserver_prerequisites")},
		{"Build control-plane images from source (ko)", containsAny("Building github.com/", "Publishing ")},
		{"Wait for components: postgres, api-server, controller, atenet, atelet", contains("Waiting for ATE system components")},
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
