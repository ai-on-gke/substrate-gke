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
    "currentMasterVersion": "1.35.5-gke.1163012",
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
  }
]`

func TestParseClusters(t *testing.T) {
	clusters, err := ParseClusters([]byte(clusterListJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 4 {
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

// Bootstrap builds the network paths it expects from PROJECT_ID and
// GCE_REGION, so for some clusters no value the installer can pass will
// match, and bootstrap would delete them. Those have to be caught before
// bootstrap runs; and the ordinary case — the cluster's own project and
// region, under any subnet name — must not be, or the guard turns away the
// clusters it exists to protect.
func TestBootstrapRecreatesOnlyClustersNoSettingCanMatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		c       Cluster
		refused bool
	}{
		{"own project and region, GKE-made subnet", Cluster{
			Network:    "projects/acme/global/networks/default",
			Subnetwork: "projects/acme/regions/us-central1/subnetworks/gke-prod-subnet-55edbf1e"}, false},
		{"no networkConfig listed", Cluster{}, false},
		{"full URLs, as some APIs spell them", Cluster{
			Network:    "https://www.googleapis.com/compute/v1/projects/acme/global/networks/default",
			Subnetwork: "https://www.googleapis.com/compute/v1/projects/acme/regions/us-central1/subnetworks/default"}, false},
		{"Shared VPC network in the host project", Cluster{
			Network:    "projects/host/global/networks/shared",
			Subnetwork: "projects/host/regions/us-central1/subnetworks/shared-sub"}, true},
		{"subnet in another region", Cluster{
			Network:    "projects/acme/global/networks/default",
			Subnetwork: "projects/acme/regions/europe-west3/subnetworks/default"}, true},
	} {
		if got := tc.c.BootstrapRecreates("acme", "us-central1") != ""; got != tc.refused {
			t.Errorf("%s: refused=%v, want %v (%q)", tc.name, got, tc.refused, tc.c.BootstrapRecreates("acme", "us-central1"))
		}
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
