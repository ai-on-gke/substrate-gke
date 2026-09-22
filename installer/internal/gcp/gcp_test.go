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

package gcp

import "testing"

const clusterListJSON = `[
  {
    "name": "substrate-poc",
    "location": "us-west1-c",
    "status": "RUNNING",
    "currentMasterVersion": "1.36.4-gke.1247000",
    "currentNodeCount": 2,
    "enableK8sBetaApis": {
      "enabledApis": [
        "certificates.k8s.io/v1beta1/podcertificaterequests",
        "certificates.k8s.io/v1beta1/clustertrustbundles"
      ]
    }
  },
  {
    "name": "legacy",
    "location": "us-central1",
    "status": "RUNNING",
    "currentMasterVersion": "1.33.2-gke.100",
    "currentNodeCount": 12
  },
  {
    "name": "ga",
    "location": "us-west1-c",
    "status": "RUNNING",
    "currentMasterVersion": "1.37.1-gke.1000000",
    "currentNodeCount": 2
  }
]`

func TestParseClusters(t *testing.T) {
	clusters, err := ParseClusters([]byte(clusterListJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 3 {
		t.Fatalf("got %d clusters", len(clusters))
	}
	ready := clusters[0]
	if ready.Name != "substrate-poc" || ready.Location != "us-west1-c" || ready.NodeCount != 2 {
		t.Errorf("unexpected cluster: %+v", ready)
	}
	if !ready.SubstrateReady() {
		t.Error("substrate-poc should be substrate-ready")
	}
	if clusters[1].SubstrateReady() {
		t.Error("legacy (no beta APIs) must not be substrate-ready")
	}
	// A release that also serves these APIs as GA buys the cluster nothing:
	// upstream's controllers are built against the beta types, so the beta
	// group still has to be served. Calling this one ready would walk the user
	// into an install that hangs waiting on a group nobody serves.
	if clusters[2].SubstrateReady() {
		t.Error("a 1.37 cluster with no beta APIs must not be substrate-ready")
	}
}

// The floor is GKE's supported one, 1.36, not the technical one. Both beta APIs
// exist from 1.35 and GKE will accept a 1.35 cluster carrying them, so nothing
// in the API surface enforces this — only this constant does. Set it a minor low
// and the wizard badges an unsupported cluster "substrate-ready" and installs
// onto it; set it high and it sends a supported user off to rebuild a cluster
// that was fine.
func TestSupportedRelease(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"1.36.4-gke.1247000", true},
		{"1.36", true},
		{"1.37.1-gke.1000000", true},
		{"2.0.0-gke.1", true},
		// Serves both beta APIs, but is not a release Substrate is supported
		// on — the one case where GKE would say yes and the installer says no.
		{"1.35.5-gke.1163012", false},
		{"1.34.11-gke.1102000", false},
		{"1.33.2-gke.100", false},
		{"0.99.0", false},
		// Unreadable reports true, the opposite of PodCertificateGA: a wrong
		// "too old" costs a cluster rebuild, a wrong "try it" costs one
		// failed install the user can retry.
		{"", true},
		{"not-a-version", true},
	} {
		if got := (Cluster{MasterVersion: tc.version}).SupportedRelease(); got != tc.want {
			t.Errorf("SupportedRelease(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// Separate from SupportedRelease by exactly one minor, and the gap is the whole
// point: on 1.35 GKE accepts the enablement, so the confirm panel must not
// quote a rejection GKE would never return at someone whose only real problem
// is that the release is unsupported.
func TestBetaAPIsAvailable(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"1.35.5-gke.1163012", true},
		{"1.36.4-gke.1247000", true},
		{"1.37.1-gke.1000000", true},
		{"1.34.11-gke.1102000", false},
		{"1.33.2-gke.100", false},
		{"", true},
		{"not-a-version", true},
	} {
		if got := (Cluster{MasterVersion: tc.version}).BetaAPIsAvailable(); got != tc.want {
			t.Errorf("BetaAPIsAvailable(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// The version decides which repair the confirm panel offers — enable and go,
// or enable and recycle every node — so it has to be read the way GKE writes
// it, and a version that cannot be read must fall to the costlier advice.
func TestPodCertificateGA(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"1.37.1-gke.1000000", true},
		{"1.37", true},
		{"1.38.0-gke.1", true},
		{"2.0.0-gke.1", true},
		{"1.36.3-gke.1767000", false},
		{"1.33.2-gke.100", false},
		{"0.99.0", false},
		{"", false},
		{"not-a-version", false},
		{"1.x-gke.1", false},
	} {
		if got := (Cluster{MasterVersion: tc.version}).PodCertificateGA(); got != tc.want {
			t.Errorf("PodCertificateGA(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// Readiness needs both halves, and the tempting simplification in either
// direction breaks a real cluster. Drop the beta-API half and a 1.37 cluster is
// waved through to an install that hangs on an API group nobody serves, since
// upstream's controllers watch the v1beta1 types no matter what else the
// release offers. Drop the release half and a 1.35 cluster that happens to
// carry the beta APIs is called ready, which is a support promise this repo
// cannot keep.
func TestSubstrateReadyNeedsBothTheReleaseAndTheAPIs(t *testing.T) {
	for _, version := range []string{"1.36.4-gke.1247000", "1.37.1-gke.1000000", "1.40.0-gke.1", "???"} {
		if (Cluster{MasterVersion: version}).SubstrateReady() {
			t.Errorf("%s with no beta APIs must not be substrate-ready", version)
		}
		if !(Cluster{MasterVersion: version, BetaAPIs: RequiredBetaAPIs}).SubstrateReady() {
			t.Errorf("%s with the beta APIs enabled should be substrate-ready", version)
		}
	}
	for _, version := range []string{"1.33.2-gke.100", "1.35.5-gke.1163012"} {
		if (Cluster{MasterVersion: version, BetaAPIs: RequiredBetaAPIs}).SubstrateReady() {
			t.Errorf("%s is below the supported floor and must not be substrate-ready", version)
		}
	}
}

func TestParseClustersEmpty(t *testing.T) {
	clusters, err := ParseClusters([]byte(`[]`))
	if err != nil || len(clusters) != 0 {
		t.Fatalf("got %v, %v", clusters, err)
	}
}

func TestParseNodePools(t *testing.T) {
	data := `[
	  {"name": "substrate-node-pool", "config": {"machineType": "c3-standard-4"},
	   "autoscaling": {"enabled": true}},
	  {"name": "default-pool", "config": {"machineType": "e2-medium"}, "autoscaling": {}}
	]`
	pools, err := ParseNodePools([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools", len(pools))
	}
	if pools[0].Name != "substrate-node-pool" || !pools[0].Autoscaled || pools[0].MachineType != "c3-standard-4" {
		t.Errorf("unexpected pool: %+v", pools[0])
	}
	if pools[1].Autoscaled {
		t.Error("default-pool should not report autoscaling")
	}
}
