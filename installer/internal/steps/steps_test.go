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
	"testing"

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

func TestBootstrapChecklistTracksSetupGCPOutput(t *testing.T) {
	items := Bootstrap()
	// Item 0 is the substrate fetch, so the seven bootstrap phases sit at 1..7.
	lines := []string{
		snapshot.FetchLine + "@" + snapshot.ShortCommit() + " from https://github.com/agent-substrate/substrate.git...",
		"time=... msg=Starting full bootstrap...",
		"time=... msg=Step 1/7: Enabling required APIs...",
		"time=... msg=Step 2/7: Creating GKE Cluster...",
		"time=... msg=Cluster does not exist. Creating... cluster=substrate-poc",
		"time=... msg=Step 3/7: Creating GCS Bucket for snapshots...",
	}
	if got := feed(items, lines); got != 3 {
		t.Fatalf("active = %d, want 3 (bucket step)", got)
	}
	if got := feed(items, []string{"Step 7/7: Creating Monitoring Dashboards..."}); got != 7 {
		t.Fatalf("active = %d, want 7", got)
	}
}

// A warm cache prints a different line; it must still light up the fetch item.
func TestBootstrapChecklistTracksACachedCheckout(t *testing.T) {
	if got := feed(Bootstrap(), []string{snapshot.CachedLine + snapshot.ShortCommit()}); got != 0 {
		t.Fatalf("active = %d, want 0 (fetch step)", got)
	}
}

func TestDeployChecklistTracksAteSetupOutput(t *testing.T) {
	items := Deploy(false)
	lines := []string{
		"[step]: deploy_ate_system",
		"[step]: deploy_crds",
		"[step]: ensure_apiserver_prerequisites",
		"[step]: create_jwt_authority_pool_secret",
		"Building github.com/agent-substrate/substrate/cmd/atelet for linux/amd64",
	}
	if got := feed(items, lines); got != 2 {
		t.Fatalf("active = %d, want 2 (ko build phase)", got)
	}
	if got := Progress(items, 2, "[step]: Waiting for ATE system components to be ready..."); got != 3 {
		t.Fatalf("active = %d, want 3 (wait phase)", got)
	}
}

func TestProgressNeverMovesBackward(t *testing.T) {
	items := Deploy(false)
	// A late ko "Building" line must not rewind past the wait phase.
	if got := Progress(items, 3, "Building github.com/agent-substrate/substrate/cmd/atenet-dns"); got != 3 {
		t.Fatalf("active = %d, want 3", got)
	}
	// An unrelated line leaves the cursor alone.
	if got := Progress(items, 1, "some noise"); got != 1 {
		t.Fatalf("active = %d, want 1", got)
	}
}

func TestFilestoreCSIChecklistTracksDeployOutput(t *testing.T) {
	items := FilestoreCSI()
	lines := []string{
		"Disabling managed GKE Filestore CSI Driver addon...",
		"Cloning into 'gcp-filestore-csi-driver'...",
		"  🚀 GKE Substrate Filestore CSI Driver Overlay Deployment",
		"🔐 Configuring Service Account & IAM Bindings...",
		"📦 Applying Substrate Filestore CSI Driver Overlay via Kustomize...",
	}
	if got := feed(items, lines[:1]); got != 0 {
		t.Fatalf("active = %d, want 0 (disable addon phase)", got)
	}
	if got := feed(items, lines[:2]); got != 1 {
		t.Fatalf("active = %d, want 1 (fetch phase)", got)
	}
	if got := feed(items, lines[:4]); got != 2 {
		t.Fatalf("active = %d, want 2 (IAM phase)", got)
	}
	if got := feed(items, lines); got != 3 {
		t.Fatalf("active = %d, want 3 (apply phase)", got)
	}
}

func TestFilestoreCSIChecklistTracksDeployOutputWithoutAddonDisable(t *testing.T) {
	items := FilestoreCSI()
	lines := []string{
		"Cloning into 'gcp-filestore-csi-driver'...",
		"  🚀 GKE Substrate Filestore CSI Driver Overlay Deployment",
		"🔐 Configuring Service Account & IAM Bindings...",
		"📦 Applying Substrate Filestore CSI Driver Overlay via Kustomize...",
	}
	if got := feed(items, lines[:1]); got != 1 {
		t.Fatalf("active = %d, want 1 (fetch phase)", got)
	}
	if got := feed(items, lines[:3]); got != 2 {
		t.Fatalf("active = %d, want 2 (IAM phase)", got)
	}
	if got := feed(items, lines); got != 3 {
		t.Fatalf("active = %d, want 3 (apply phase)", got)
	}
}
