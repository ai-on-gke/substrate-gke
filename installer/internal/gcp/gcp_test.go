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
    }
  },
  {
    "name": "legacy",
    "location": "us-central1",
    "status": "RUNNING",
    "currentMasterVersion": "1.33.2-gke.100",
    "currentNodeCount": 12
  }
]`

func TestParseClusters(t *testing.T) {
	clusters, err := ParseClusters([]byte(clusterListJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
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
