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

package steps

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/gkeoverlay"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
)

// feed applies lines in order and returns the final active index.
func feed(items []ChecklistItem, lines []string) int {
	active := -1
	for _, line := range lines {
		active = Progress(items, active, line)
	}
	return active
}

// fetchLabel stands in for snapshot.Builder.FetchLabel, which the checklists
// take as given.
const fetchLabel = "Fetch the substrate checkout"

func TestBootstrapChecklistTracksSetupGCPOutput(t *testing.T) {
	for _, ar := range []bool{false, true} {
		t.Run(fmt.Sprintf("artifact-registry=%t", ar), func(t *testing.T) {
			items := Bootstrap(fetchLabel, ar, true, false)
			phases := []string{"Enabling required APIs"}
			if ar {
				phases = append(phases, "Creating Artifact Registry repository")
			}
			phases = append(phases, "Creating the GKE node service account", "Creating GKE Cluster", "Creating GCS buckets for snapshots and micro-VM assets",
				"Granting GKE Node permissions", "Granting Atelet permissions",
				"Creating the snapshot transfer service account",
				"Creating IAM policy bindings for the buckets", "Creating Monitoring Dashboards")
			if len(items) != len(phases)+1 {
				t.Fatalf("%d checklist items for %d phases", len(items), len(phases))
			}
			active := feed(items, []string{snapshot.FetchLine})
			for i, phase := range phases {
				line := fmt.Sprintf("time=... msg=Step %d/%d: %s...", i+1, len(phases), phase)
				active = Progress(items, active, line)
				if active != i+1 {
					t.Fatalf("%q: active = %d, want %d", line, active, i+1)
				}
				if got := Progress(items, active, "Cluster does not exist. Creating..."); got != active {
					t.Fatalf("noise moved progress to %d", got)
				}
			}
		})
	}
}

// A warm cache prints a different line; it must still light up the fetch item.
func TestBootstrapChecklistTracksACachedCheckout(t *testing.T) {
	if got := feed(Bootstrap(fetchLabel, false, true, false), []string{snapshot.CachedLine + snapshot.ShortCommit()}); got != 0 {
		t.Fatalf("active = %d, want 0 (fetch step)", got)
	}
}

func TestDeployChecklistTracksAteSetupOutput(t *testing.T) {
	items := Deploy(false)
	if got := feed(items, []string{gkeoverlay.NotNeededLine}); got != 0 {
		t.Fatalf("active = %d, want 0 (GKE overlay check)", got)
	}
	lines := []string{
		gkeoverlay.NotNeededLine,
		"[step]: deploy_ate_system",
		"[step]: deploy_crds",
		"[step]: ensure_apiserver_prerequisites",
		"[step]: create_jwt_authority_pool_secret",
		"Building github.com/agent-substrate/substrate/cmd/atelet for linux/amd64",
	}
	if got := feed(items, lines); got != 3 {
		t.Fatalf("active = %d, want 3 (ko build phase)", got)
	}
	if got := Progress(items, 3, "[step]: Waiting for ATE system components to be ready..."); got != 4 {
		t.Fatalf("active = %d, want 4 (wait phase)", got)
	}
}

// From v0.6.0 the module is go.ate.dev/substrate, and ko names what it builds
// by that path; the build row has to light for both.
func TestDeployChecklistTracksAVanityImportBuild(t *testing.T) {
	items := Deploy(false)
	lines := []string{
		"[step]: deploy_crds",
		"[step]: ensure_apiserver_prerequisites",
		"Building go.ate.dev/substrate/cmd/atelet for linux/amd64",
	}
	if got := feed(items, lines); got != 3 {
		t.Fatalf("active = %d, want 3 (ko build phase)", got)
	}
}

func TestProgressNeverMovesBackward(t *testing.T) {
	items := Deploy(false)
	// A late ko "Building" line must not rewind past the wait phase.
	if got := Progress(items, 4, "Building github.com/agent-substrate/substrate/cmd/atenet-dns"); got != 4 {
		t.Fatalf("active = %d, want 4", got)
	}
	// An unrelated line leaves the cursor alone.
	if got := Progress(items, 1, "some noise"); got != 1 {
		t.Fatalf("active = %d, want 1", got)
	}
}

// The rows follow the script's order (snapshot.DeployFilestoreCSI): fetch and
// the API check before anything changes, then the addon, IAM, the apply, and
// the installer's own check of the roles/file.editor grant.
func TestFilestoreCSIChecklistTracksDeployOutput(t *testing.T) {
	items := FilestoreCSI()
	lines := []string{
		"Cloning into 'gcp-filestore-csi-driver'...",
		snapshot.FilestoreAPICheckLine + " (ate.dev/v1)...",
		"The cluster serves csidriverconfigs.ate.dev as: v1",
		"Disabling managed GKE Filestore CSI Driver addon...",
		"Updating [prod]...",
		"  🚀 GKE Substrate Filestore CSI Driver Overlay Deployment",
		"🔐 Configuring Service Account & IAM Bindings...",
		"📦 Applying Substrate Filestore CSI Driver Overlay via Kustomize...",
		"✅ Deployment complete!",
		snapshot.FilestoreGrantCheckLine + " (substrate-filestore-csi@acme.iam.gserviceaccount.com)...",
		"roles/file.editor is granted to substrate-filestore-csi@acme.iam.gserviceaccount.com.",
	}
	for i, want := range []int{0, 1, 1, 2, 2, 2, 3, 4, 4, 5, 5} {
		if got := feed(items, lines[:i+1]); got != want {
			t.Fatalf("after %d lines active = %d, want %d", i+1, got, want)
		}
	}
	if items[1].Label != "Check the cluster serves the overlay's CSIDriverConfig API" || !strings.HasPrefix(items[2].Label, "Disable managed") {
		t.Errorf("the API check row must come before the addon row: %q, %q", items[1].Label, items[2].Label)
	}
}

// A check that fails stops the step at its row: nothing after it has run.
func TestFilestoreCSIChecklistStopsAtAFailedAPICheck(t *testing.T) {
	lines := []string{
		"Cloning into 'gcp-filestore-csi-driver'...",
		snapshot.FilestoreAPICheckLine + " (ate.dev/v1alpha1)...",
		"error: the Filestore overlay's CSIDriverConfig is written against ate.dev/v1alpha1, but this cluster serves csidriverconfigs.ate.dev as: v1",
		"Nothing was changed: no IAM binding was granted and the managed Filestore addon was left as it is.",
	}
	if got := feed(FilestoreCSI(), lines); got != 1 {
		t.Fatalf("active = %d, want 1 (the check row)", got)
	}
}

func TestMicroVMDepsChecklistTracksInstallOutput(t *testing.T) {
	items := MicroVMDeps(fetchLabel)
	lines := []string{
		snapshot.FetchLine + "@" + snapshot.ShortCommit() + " from https://github.com/agent-substrate/substrate.git...",
		"[install-microvm-deps]: Assembling micro-VM assets into bin/microvm-assets/amd64 (ARCH=amd64)...",
		"[install-microvm-deps]: Uploading assets to gs://ate-snapshots-p-c-z/kata-assets/ ...",
		"[install-microvm-deps]: Applying microvm SandboxConfig from manifests/microvm/sandboxconfig-microvm.yaml.tmpl...",
	}
	for i := range lines {
		if got := feed(items, lines[:i+1]); got != i {
			t.Fatalf("after %d lines active = %d, want %d", i+1, got, i)
		}
	}
}

// From agent-substrate/substrate#2092 the same phases come from ate-setup,
// which spells the assets "microVM" and stages under microvm-assets/.
func TestMicroVMDepsChecklistTracksATESetupOutput(t *testing.T) {
	items := MicroVMDeps(fetchLabel)
	lines := []string{
		snapshot.FetchLine + "@" + snapshot.ShortCommit() + " from https://github.com/agent-substrate/substrate.git...",
		"Assembling microVM assets into bin/microvm-assets/amd64 (arch=amd64)...",
		"Uploading assets to gs://ate-snapshots-p-c-z-assets/microvm-assets/ ...",
		"Applying microvm SandboxConfig from manifests/ate-install/sandboxconfig-microvm.yaml.tmpl...",
	}
	for i := range lines {
		if got := feed(items, lines[:i+1]); got != i {
			t.Fatalf("after %d lines active = %d, want %d", i+1, got, i)
		}
	}
}

// A second install finds the asset set already built and says so instead of
// assembling. The row has to light either way: it is the step's slowest phase,
// and an unlit row on the fast path reads as a stall.
func TestMicroVMDepsChecklistTracksCachedAssets(t *testing.T) {
	items := MicroVMDeps(fetchLabel)
	lines := []string{
		snapshot.CachedLine + snapshot.ShortCommit(),
		"[install-microvm-deps]: Assets already present in bin/microvm-assets/amd64; skipping assemble.",
	}
	if got := feed(items, lines); got != 1 {
		t.Fatalf("active = %d, want 1 (assemble phase satisfied by the skip)", got)
	}
}

// A cluster without the managed driver has nothing to disable. The script
// says so, and the row lights on that line rather than staying unlit while
// deploy.sh runs.
func TestFilestoreCSIChecklistTracksDeployOutputWithoutAddonDisable(t *testing.T) {
	items := FilestoreCSI()
	lines := []string{
		"Cloning into 'gcp-filestore-csi-driver'...",
		snapshot.FilestoreAPICheckLine + " (ate.dev/v1)...",
		snapshot.FilestoreAddonOffLine,
		"  🚀 GKE Substrate Filestore CSI Driver Overlay Deployment",
		"🔐 Configuring Service Account & IAM Bindings...",
		"📦 Applying Substrate Filestore CSI Driver Overlay via Kustomize...",
	}
	for i, want := range []int{0, 1, 2, 2, 3, 4} {
		if got := feed(items, lines[:i+1]); got != want {
			t.Fatalf("after %d lines active = %d, want %d", i+1, got, want)
		}
	}
}

// Upstream logs "Creating GKE Cluster" for step 3 whatever the cluster, but on
// an existing one it creates nothing. The label has to say what is actually
// happening — above all when it is a ten-minute control-plane update, which a
// "create" label makes look like the install is rebuilding the user's cluster.
func TestBootstrapLabelsTheClusterStepForTheSituation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		isNew, enableBetas bool
		want               string
	}{
		{"new cluster", true, false, "Create the GKE cluster"},
		{"existing, missing the beta APIs", false, true, "Turn on the beta PodCertificate APIs"},
		{"existing and ready", false, false, "Check the existing GKE cluster"},
	} {
		if got := Bootstrap(fetchLabel, false, tc.isNew, tc.enableBetas)[3].Label; !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: step 3 = %q, want it to start %q", tc.name, got, tc.want)
		}
	}
}
