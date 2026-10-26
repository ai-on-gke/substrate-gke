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
    "currentMasterVersion": "1.37.1-gke.1000000",
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
        "version": "1.37.1-gke.1000000",
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
		t.Error("legacy (1.33) must not be substrate-ready")
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
	// Substrate uses v1 on 1.37, so the beta APIs have no bearing on readiness.
	if !clusters[4].SubstrateReady() {
		t.Error("a 1.37 cluster with no beta APIs should be substrate-ready")
	}
}

// The floor is 1.37. GKE accepts the beta APIs on 1.35 and 1.36 clusters, so
// nothing in the API surface enforces this — only this constant does. Set it a
// minor low and the wizard badges an unsupported cluster "substrate-ready" and
// installs onto it; set it high and it sends a supported user off to rebuild a
// cluster that was fine.
func TestSupportedRelease(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"1.37.1-gke.1000000", true},
		{"1.37", true},
		{"1.38.0-gke.1", true},
		{"2.0.0-gke.1", true},
		// Both serve the beta APIs, but neither is a release Substrate is
		// supported on.
		{"1.36.4-gke.1247000", false},
		{"1.36", false},
		{"1.35.5-gke.1163012", false},
		{"1.34.11-gke.1102000", false},
		{"1.33.2-gke.100", false},
		{"0.99.0", false},
		// Unreadable reports true: a wrong "too old" costs a cluster rebuild,
		// a wrong "try it" costs one failed install the user can retry.
		{"", true},
		{"not-a-version", true},
	} {
		if got := (Cluster{MasterVersion: tc.version}).SupportedRelease(); got != tc.want {
			t.Errorf("SupportedRelease(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// Readiness is the control plane and every node pool at or above the floor.
// Drop the pool half and a 1.37 control plane with 1.36 pools is waved
// through, and Substrate's pods hang on those nodes, whose kubelet serves
// projection only behind the beta gate. The beta APIs change nothing either
// way.
func TestSubstrateReadyNeedsTheControlPlaneAndEveryPool(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Cluster
		want bool
	}{
		{"1.36, no beta APIs", Cluster{MasterVersion: "1.36.4-gke.1"}, false},
		{"1.36, beta APIs", Cluster{MasterVersion: "1.36.4-gke.1", BetaAPIs: RequiredBetaAPIs}, false},
		{"1.37 everywhere, no beta APIs",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"p", "1.37.1-gke.1"}}}, true},
		{"1.37 everywhere, beta APIs",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"p", "1.37.1-gke.1"}}, BetaAPIs: RequiredBetaAPIs}, true},
		{"1.40, no pools listed", Cluster{MasterVersion: "1.40.0-gke.1"}, true},
		{"1.37 control plane, 1.36 pool",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"old", "1.36.4-gke.1"}}}, false},
		{"1.37 control plane, 1.36 pool, beta APIs",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"old", "1.36.4-gke.1"}}, BetaAPIs: RequiredBetaAPIs}, false},
		{"unreadable control plane", Cluster{MasterVersion: "???"}, true},
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

// The installer hands bootstrap a cluster's own network names, which only
// works if they are read from the same listing, path and all. A field that
// stopped being read would leave every cluster looking like it has no network,
// and bootstrap would be handed the defaults again.
func TestParseClustersReadsTheNetwork(t *testing.T) {
	clusters, err := ParseClusters([]byte(`[{
	  "name": "prod", "location": "us-central1-a",
	  "networkConfig": {
	    "network": "projects/acme/global/networks/default",
	    "subnetwork": "projects/acme/regions/us-central1/subnetworks/gke-prod-subnet-55edbf1e",
	    "datapathProvider": "ADVANCED_DATAPATH"
	  }
	}, {"name": "legacy", "location": "us-central1-a",
	  "networkConfig": {"datapathProvider": "LEGACY_DATAPATH"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	prod := clusters[0]
	if prod.NetworkName() != "default" || prod.SubnetworkName() != "gke-prod-subnet-55edbf1e" || !prod.DataplaneV2 {
		t.Errorf("prod: network=%q subnetwork=%q dpv2=%v", prod.NetworkName(), prod.SubnetworkName(), prod.DataplaneV2)
	}
	if clusters[1].DataplaneV2 {
		t.Error("a LEGACY_DATAPATH cluster must not read as Dataplane V2")
	}
}

// Whether the managed Filestore driver is on decides whether the installer
// warns that provision will turn it off; read from the same listing, or the
// warning silently never fires.
func TestParseClustersReadsTheFilestoreAddon(t *testing.T) {
	clusters, err := ParseClusters([]byte(`[
	  {"name": "on", "addonsConfig": {"gcpFilestoreCsiDriverConfig": {"enabled": true}}},
	  {"name": "off", "addonsConfig": {}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !clusters[0].FilestoreCSIAddon || clusters[1].FilestoreCSIAddon {
		t.Errorf("FilestoreCSIAddon = %v, %v; want true, false", clusters[0].FilestoreCSIAddon, clusters[1].FilestoreCSIAddon)
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
		{MinSupportedRelease, func(c Cluster) bool {
			return len(Cluster{PoolVersions: []PoolVersion{{"p", c.MasterVersion}}}.PoolsBelowFloor()) == 0
		}},
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

// Pool versions come from the same gcloud listing as everything else, so the
// cluster screen never shells out a second time to check them. If the field
// stopped being read, a 1.37 control plane with 1.36 pools would badge ready.
func TestParseClustersReadsPoolVersions(t *testing.T) {
	clusters, err := ParseClusters([]byte(clusterListJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := []PoolVersion{{"kvm-pool", "1.37.1-gke.1000000"}}
	if got := clusters[0].PoolVersions; !slices.Equal(got, want) {
		t.Errorf("substrate-poc pools = %v, want %v", got, want)
	}
}

// The kubelet implements pod certificate projection, so every pool's version
// is checked, not only the control plane's: GKE lets pools trail their
// control plane.
func TestPoolsBelowFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Cluster
		want []string
	}{
		{"1.37 control plane, one 1.36 pool",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"old", "1.36.4-gke.1"}, {"new", "1.37.1-gke.1"}}},
			[]string{"old"}},
		{"every pool on 1.37",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"a", "1.37.1-gke.1"}}},
			nil},
		{"several old pools, in gcloud's order",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"b", "1.35.8-gke.1"}, {"a", "1.36.4-gke.1"}}},
			[]string{"b", "a"}},
		{"unreadable pool version is left for the install to judge",
			Cluster{MasterVersion: "1.37.1-gke.1", PoolVersions: []PoolVersion{{"odd", ""}}},
			nil},
		{"no pools listed",
			Cluster{MasterVersion: "1.36.4-gke.1"}, nil},
	} {
		if got := tc.c.PoolsBelowFloor(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// MissingBetaAPIs is what decides whether provision is announced as a
// ten-minute control-plane update, so it must track what bootstrap will do,
// not readiness: a cluster below the floor that already serves both APIs is
// not ready, and bootstrap has nothing to enable on it.
func TestMissingBetaAPIsIsNotTheInverseOfReadiness(t *testing.T) {
	old := Cluster{MasterVersion: "1.36.4-gke.1", BetaAPIs: RequiredBetaAPIs}
	if old.SubstrateReady() || old.MissingBetaAPIs() {
		t.Errorf("1.36 with both APIs: ready=%v missing=%v, want false/false", old.SubstrateReady(), old.MissingBetaAPIs())
	}
	half := Cluster{MasterVersion: "1.36.4-gke.1", BetaAPIs: RequiredBetaAPIs[:1]}
	if !half.MissingBetaAPIs() {
		t.Error("a cluster with only one of the two APIs must count as missing them")
	}
}

// The channel check reads GKE's own list of what each channel carries, and a
// requested version is usually a bare minor ("1.37") that GKE resolves to a
// build. If either half were wrong, the installer would refuse combinations
// GKE accepts or wave through ones it rejects.
func TestChannelVersionsAndCarries(t *testing.T) {
	carried, err := ParseChannelVersions([]byte(`{"channels": [
	  {"channel": "RAPID", "validVersions": ["1.37.0-gke.3503000", "1.36.4-gke.1495000"]},
	  {"channel": "REGULAR", "validVersions": ["1.36.4-gke.1391000"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		channel, version string
		want             bool
	}{
		{"rapid", "1.37", true},
		{"rapid", "1.37.0", true},
		{"rapid", "1.37.0-gke.3503000", true},
		{"regular", "1.37", false},
		{"regular", "1.36", true},
		{"regular", "1.3", false}, // a prefix of 1.36 is not 1.36
		{"stable", "1.36", false},
	} {
		if got := ChannelCarries(carried[tc.channel], tc.version); got != tc.want {
			t.Errorf("%s carries %s = %v, want %v", tc.channel, tc.version, got, tc.want)
		}
	}
}
