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

import (
	"slices"
	"testing"
)

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
    },
    "nodePools": [
      {
        "name": "kvm-pool",
        "version": "1.36.4-gke.1247000",
        "config": {
          "machineType": "n2-standard-8",
          "advancedMachineFeatures": {
            "enableNestedVirtualization": true
          }
        }
      }
    ]
  },
  {
    "name": "legacy",
    "location": "us-central1",
    "status": "RUNNING",
    "currentMasterVersion": "1.33.2-gke.100",
    "currentNodeCount": 12,
    "nodePools": [
      {
        "name": "default-pool",
        "config": {
          "machineType": "e2-standard-4"
        }
      }
    ]
  },
  {
    "name": "metal",
    "location": "us-central1-a",
    "status": "RUNNING",
    "currentMasterVersion": "1.35.5-gke.1163012",
    "currentNodeCount": 3,
    "nodePools": [
      {
        "name": "bare-metal-pool",
        "config": {
          "machineType": "c3-standard-192-metal"
        }
      }
    ]
  },
  {
    "name": "autopilot",
    "location": "us-central1",
    "status": "RUNNING",
    "currentMasterVersion": "1.35.5-gke.1163012",
    "currentNodeCount": 1
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
	if len(clusters) != 5 {
		t.Fatalf("got %d clusters", len(clusters))
	}
	ready := clusters[0]
	if ready.Name != "substrate-poc" || ready.Location != "us-west1-c" || ready.NodeCount != 2 {
		t.Errorf("unexpected cluster: %+v", ready)
	}
	if !ready.SubstrateReady() {
		t.Error("substrate-poc should be substrate-ready")
	}
	if !ready.KVMReady {
		t.Error("substrate-poc should be KVMReady")
	}
	if clusters[1].SubstrateReady() {
		t.Error("legacy (no beta APIs) must not be substrate-ready")
	}
	if clusters[1].KVMReady {
		t.Error("legacy (e2 without nested virt) must not be KVMReady")
	}
	if !clusters[2].KVMReady {
		t.Error("metal (c3-standard-192-metal) should be KVMReady")
	}
	if clusters[3].KVMReady {
		t.Error("autopilot (no nodePools key) must not be KVMReady")
	}
	// From v0.4.0 Substrate discovers v1 and uses it, so a 1.37 cluster with
	// no pool listed below 1.37 needs no beta APIs. Refusing it would send
	// the user through a confirmation, and a ten-minute update, for nothing.
	if !clusters[4].SubstrateReady() {
		t.Error("a 1.37 cluster with no beta APIs should be substrate-ready")
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

// Readiness is a supported release plus a way to serve the PodCertificate
// APIs, and each simplification breaks a real cluster. Require the beta APIs
// everywhere and a 1.37 cluster, which Substrate runs on through v1, is sent
// through a needless repair. Require them nowhere at 1.37 and a 1.37 control
// plane with 1.36 pools is waved through, and Substrate's pods hang on those
// nodes, whose kubelet serves projection only behind the beta gate. Drop the
// release half and a 1.35 cluster carrying the beta APIs is called ready,
// which is a support promise this repo cannot keep.
func TestSubstrateReadyNeedsAReleaseAndAWayToServeTheAPIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Cluster
		want bool
	}{
		{"1.36, no beta APIs", Cluster{MasterVersion: "1.36.4-gke.1"}, false},
		{"1.36, beta APIs", Cluster{MasterVersion: "1.36.4-gke.1", BetaAPIs: RequiredBetaAPIs}, true},
		{"1.37 everywhere, no beta APIs",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"p", "1.37.1-gke.1"}}}, true},
		{"1.40, no pools listed, no beta APIs", Cluster{MasterVersion: "1.40.0-gke.1"}, true},
		{"1.37 control plane, 1.36 pool, no beta APIs",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"old", "1.36.4-gke.1"}}}, false},
		{"1.37 control plane, 1.36 pool, beta APIs",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"old", "1.36.4-gke.1"}}, BetaAPIs: RequiredBetaAPIs}, true},
		{"unreadable version, no beta APIs", Cluster{MasterVersion: "???"}, false},
		{"unreadable version, beta APIs", Cluster{MasterVersion: "???", BetaAPIs: RequiredBetaAPIs}, true},
		{"1.35, beta APIs", Cluster{MasterVersion: "1.35.5-gke.1163012", BetaAPIs: RequiredBetaAPIs}, false},
		{"1.33, beta APIs", Cluster{MasterVersion: "1.33.2-gke.100", BetaAPIs: RequiredBetaAPIs}, false},
	} {
		if got := tc.c.SubstrateReady(); got != tc.want {
			t.Errorf("%s: SubstrateReady() = %v, want %v", tc.name, got, tc.want)
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

func TestParseClusterKVMReady(t *testing.T) {
	ready, err := ParseClusterKVMReady([]byte(`{"nodePools":[{"config":{"machineType":"n2-standard-8","advancedMachineFeatures":{"enableNestedVirtualization":true}}}]}`))
	if err != nil || !ready {
		t.Fatalf("expected KVMReady=true, got ready=%v err=%v", ready, err)
	}
	notReady, err := ParseClusterKVMReady([]byte(`{"nodePools":[{"config":{"machineType":"c3-standard-4"}}]}`))
	if err != nil || notReady {
		t.Fatalf("expected KVMReady=false, got ready=%v err=%v", notReady, err)
	}
}

// Each line is drawn once and read two ways: as numbers by the comparisons and
// as text in everything the user is shown. If the two ever disagreed, the
// wizard would refuse a cluster at the very version its own message says is
// fine.
func TestReleaseTextMatchesItsComparison(t *testing.T) {
	for _, tc := range []struct {
		r     Release
		check func(Cluster) bool
	}{
		{MinSupportedRelease, Cluster.SupportedRelease},
		{BetaAPIsExistRelease, Cluster.BetaAPIsAvailable},
		{PodCertificateGARelease, Cluster.PodCertificateGA},
	} {
		if !tc.check(Cluster{MasterVersion: tc.r.String()}) {
			t.Errorf("a cluster at %s fails the check drawn at %s", tc.r, tc.r)
		}
		below := Release{tc.r.Major, tc.r.Minor - 1}
		if tc.check(Cluster{MasterVersion: below.String() + ".0-gke.1"}) {
			t.Errorf("a cluster at %s passes the check drawn at %s", below, tc.r)
		}
	}
}

// Pool versions come from the same gcloud listing as everything else, so a
// cluster screen never shells out a second time to decide which remedy to
// show. If the field stopped being read, every cluster would fall back to the
// control plane's version and a 1.37 control plane with 1.36 pools would be
// told its nodes need nothing.
func TestParseClustersReadsPoolVersions(t *testing.T) {
	clusters, err := ParseClusters([]byte(clusterListJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := []PoolVersion{{"kvm-pool", "1.36.4-gke.1247000"}}
	if got := clusters[0].PoolVersions; !slices.Equal(got, want) {
		t.Errorf("substrate-poc pools = %v, want %v", got, want)
	}
}

// The kubelet implements pod certificate projection, so it is the pools'
// versions that decide whether a late enablement leaves nodes unable to mount
// — and GKE lets pools trail their control plane. Reading the control plane
// instead tells a 1.37 cluster with 1.36 pools that nothing is needed, and its
// first Substrate pod on an old node hangs on "unimplemented".
func TestPoolsWithoutProjectionReadsThePoolsNotTheControlPlane(t *testing.T) {
	for _, tc := range []struct {
		name      string
		c         Cluster
		wantPools []string
		wantAny   bool
	}{
		{"1.37 control plane, 1.36 pool",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"old", "1.36.4-gke.1"}, {"new", "1.37.1-gke.1"}}},
			[]string{"old"}, true},
		{"every pool on 1.37",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"a", "1.37.1-gke.1"}}},
			nil, false},
		{"unreadable pool version counts as needing it",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"odd", ""}}},
			[]string{"odd"}, true},
		{"no pools listed falls back to the control plane",
			Cluster{MasterVersion: "1.36.4-gke.1"}, nil, true},
		{"no pools listed on 1.37",
			Cluster{MasterVersion: "1.37.1-gke.1"}, nil, false},
	} {
		pools, any := tc.c.PoolsWithoutProjection()
		if !slices.Equal(pools, tc.wantPools) || any != tc.wantAny {
			t.Errorf("%s: got %v, %v; want %v, %v", tc.name, pools, any, tc.wantPools, tc.wantAny)
		}
	}
}

// MissingBetaAPIs is what decides whether provision is announced as a
// ten-minute control-plane update, so it must track what bootstrap will do,
// not readiness: a cluster below the floor that already serves both APIs is
// not ready, and bootstrap has nothing to enable on it.
func TestMissingBetaAPIsIsNotTheInverseOfReadiness(t *testing.T) {
	old := Cluster{MasterVersion: "1.35.5-gke.1", BetaAPIs: RequiredBetaAPIs}
	if old.SubstrateReady() || old.MissingBetaAPIs() {
		t.Errorf("1.35 with both APIs: ready=%v missing=%v, want false/false", old.SubstrateReady(), old.MissingBetaAPIs())
	}
	half := Cluster{MasterVersion: "1.36.4-gke.1", BetaAPIs: RequiredBetaAPIs[:1]}
	if !half.MissingBetaAPIs() {
		t.Error("a cluster with only one of the two APIs must count as missing them")
	}
}
