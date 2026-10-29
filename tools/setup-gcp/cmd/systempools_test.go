// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
)

func systemPoolsConfig() *Config {
	c := nodeConfig()
	c.ClusterName, c.ClusterLocation = "substrate-poc", "us-west1-c"
	c.MachineType = "c3-standard-4"
	c.EnableNestedVirtualization = true
	c.SeparateSystemNodes = true
	c.SystemInfraMachineType, c.PostgresMachineType = "e2-standard-4", "e2-standard-4"
	return c
}

func TestBuildCreateClusterRequest_SystemPools(t *testing.T) {
	req := buildCreateClusterRequest("projects/acme/locations/us-west1-c", systemPoolsConfig())
	pools := req.GetCluster().GetNodePools()
	if len(pools) != 3 || pools[0].GetName() != "substrate-node-pool" {
		t.Fatalf("pools = %v, want substrate-node-pool, ate-system-infra, ate-postgres", pools)
	}
	for i, want := range []struct{ name, workloadType, machineType string }{
		{"ate-system-infra", "ate-control-plane", "e2-standard-4"},
		{"ate-postgres", "ate-postgres", "e2-standard-4"},
	} {
		p := pools[i+1]
		nc := p.GetConfig()
		if p.GetName() != want.name || p.GetInitialNodeCount() != 1 || nc.GetMachineType() != want.machineType {
			t.Errorf("pool %d = %s x%d %s, want %s x1 %s", i+1, p.GetName(), p.GetInitialNodeCount(), nc.GetMachineType(), want.name, want.machineType)
		}
		if got := nc.GetLabels()[workloadTypeLabel]; got != want.workloadType {
			t.Errorf("%s label %s = %q, want %q", want.name, workloadTypeLabel, got, want.workloadType)
		}
		if ts := nc.GetTaints(); len(ts) != 1 || ts[0].GetKey() != workloadTypeLabel || ts[0].GetValue() != want.workloadType || ts[0].GetEffect() != containerpb.NodeTaint_NO_SCHEDULE {
			t.Errorf("%s taints = %v, want %s=%s:NoSchedule", want.name, ts, workloadTypeLabel, want.workloadType)
		}
		if nc.GetServiceAccount() != nodeSA || !nc.GetShieldedInstanceConfig().GetEnableSecureBoot() || !nc.GetShieldedInstanceConfig().GetEnableIntegrityMonitoring() {
			t.Errorf("%s is not hardened like the worker pool: %v", want.name, nc)
		}
		// Sandbox settings stay on the worker pool.
		if nc.GetAdvancedMachineFeatures() != nil || nc.GetLinuxNodeConfig() != nil {
			t.Errorf("%s carries worker settings: %v", want.name, nc)
		}
	}

	c := systemPoolsConfig()
	c.SeparateSystemNodes = false
	if n := len(buildCreateClusterRequest("projects/acme/locations/us-west1-c", c).GetCluster().GetNodePools()); n != 1 {
		t.Errorf("--separate-system-nodes=false created %d pools, want 1", n)
	}
}

func TestSystemNodeFlagDefaults(t *testing.T) {
	c := bootstrapConfig(t)
	if !c.SeparateSystemNodes || c.SystemInfraMachineType != "e2-standard-4" || c.PostgresMachineType != "e2-standard-4" {
		t.Errorf("bootstrap defaults = separate %v, %q, %q; want true, e2-standard-4, e2-standard-4", c.SeparateSystemNodes, c.SystemInfraMachineType, c.PostgresMachineType)
	}
	cmd, _, err := createCmd.Find([]string{"cluster"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"separate-system-nodes", "system-infra-machine-type", "postgres-machine-type"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("create cluster has no --%s", name)
		}
	}
}

// An existing cluster gets no pools from this tool, only the commands that
// add the missing ones, matched by label.
func TestWarnMissingSystemPools(t *testing.T) {
	labeled := func(workloadType string) *containerpb.NodePool {
		return &containerpb.NodePool{Name: "custom-name", Config: &containerpb.NodeConfig{Labels: map[string]string{workloadTypeLabel: workloadType}}}
	}
	for _, tc := range []struct {
		name     string
		separate bool
		pools    []*containerpb.NodePool
		want     []string
		notWant  []string
	}{
		{"no system pools", true, nil, []string{"node-pools create ate-system-infra", "node-pools create ate-postgres"}, nil},
		{"system pool by label", true, []*containerpb.NodePool{labeled("ate-control-plane")}, []string{"node-pools create ate-postgres"}, []string{"node-pools create ate-system-infra"}},
		{"both present", true, []*containerpb.NodePool{labeled("ate-control-plane"), labeled("ate-postgres")}, nil, []string{"node-pools create"}},
		{"separation off", false, nil, nil, []string{"node-pools create"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := systemPoolsConfig()
			c.SeparateSystemNodes = tc.separate
			logs := captureLogs(t)
			warnMissingSystemPools(c, &containerpb.Cluster{NodePools: tc.pools})
			out := logs.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("logs lack %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("logs contain %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestSystemPoolCommand(t *testing.T) {
	got := systemPoolCommand(systemPoolsConfig(), systemPools(systemPoolsConfig())[0])
	want := "gcloud container node-pools create ate-system-infra --cluster=substrate-poc --project=acme --location=us-west1-c --machine-type=e2-standard-4 --num-nodes=1 --node-labels=ate.dev/workloadType=ate-control-plane --node-taints=ate.dev/workloadType=ate-control-plane:NoSchedule --service-account=" + nodeSA + " --scopes=https://www.googleapis.com/auth/cloud-platform --shielded-secure-boot --shielded-integrity-monitoring"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
