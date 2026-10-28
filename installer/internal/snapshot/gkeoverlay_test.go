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
	"strings"
	"testing"
)

const (
	testSelf    = "/tmp/go-build/exe/installer"
	testOverlay = "/src/substrate-gke/deploy/gke"
)

// The deploy adds deploy/gke to a tree the installer fetched, inside the
// tree and before ate-setup reads it, for source and pre-built installs
// alike.
func TestDeployAteSystemAddsTheGKEOverlayFirst(t *testing.T) {
	for _, prebuilt := range []bool{false, true} {
		st := testSetup(t)
		if prebuilt {
			st.ImageRepo, st.ImageTag = "registry.example.com/substrate", "v0.6.0"
		}
		b := NewBuilder("/tmp/substrate-pin", true)
		b.Self, b.GKEOverlay = testSelf, testOverlay
		script := b.DeployAteSystem(st).Argv[2]
		add := "'" + testSelf + "' --gke-overlay '" + testOverlay + "' --add-gke-overlay ."
		overlay, deploy, cd := strings.Index(script, add), strings.Index(script, "go run ./cmd/ate-setup deploy ate-system"), strings.Index(script, `cd "${SUBSTRATE_DIR}"`)
		if overlay < 0 || deploy < 0 || cd < 0 {
			t.Fatalf("prebuilt=%v: script lacks the overlay line, the deploy or the cd into the tree:\n%s", prebuilt, script)
		}
		if !(cd < overlay && overlay < deploy) {
			t.Errorf("prebuilt=%v: the overlay line must run inside the tree and before ate-setup:\n%s", prebuilt, script)
		}
	}
}

// A checkout the user supplied is never modified: the deploy only checks it.
func TestDeployAteSystemOnlyChecksAUserCheckout(t *testing.T) {
	b := NewBuilder(fakeCheckout(t), false)
	b.Self, b.GKEOverlay = testSelf, testOverlay
	script := b.DeployAteSystem(testSetup(t)).Argv[2]
	if strings.Contains(script, "--add-gke-overlay") || !strings.Contains(script, "--check-gke-overlay .") {
		t.Errorf("deploy against a user checkout must check, not add:\n%s", script)
	}
}

// Without the two paths, as in tests that build a Builder by hand, the line
// is left out rather than run with empty arguments.
func TestDeployAteSystemWithoutOverlayPaths(t *testing.T) {
	script := NewBuilder("/tmp/substrate-pin", true).DeployAteSystem(testSetup(t)).Argv[2]
	if strings.Contains(script, "gke-overlay") {
		t.Errorf("script runs the overlay step without its paths:\n%s", script)
	}
}

// The runbook deploys from both upgrade trees, so both get the overlay, after
// both fetches.
func TestFetchTreesAddsTheGKEOverlayToBothTrees(t *testing.T) {
	st := testSetup(t)
	st.InstalledCommit = strings.Repeat("a", 40)
	b := NewBuilder("/tmp/substrate-pin", true)
	b.Self, b.GKEOverlay = testSelf, testOverlay
	script := b.FetchTrees(st, "/cache/upgrades/substrate-old", "/cache/upgrades/substrate-new").Argv[2]
	lastFetch := strings.LastIndex(script, "Fetched substrate@")
	for _, dir := range []string{"/cache/upgrades/substrate-old", "/cache/upgrades/substrate-new"} {
		i := strings.Index(script, "--add-gke-overlay '"+dir+"'")
		if i < 0 {
			t.Errorf("FetchTrees does not add the overlay to %s:\n%s", dir, script)
		} else if i < lastFetch {
			t.Errorf("FetchTrees adds the overlay to %s before both trees are fetched:\n%s", dir, script)
		}
	}
}
