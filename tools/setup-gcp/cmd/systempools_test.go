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
	"context"
	"slices"
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
		{"ate-system-infra", "ate-system-infra", "e2-standard-4"},
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
		{"system pool by label", true, []*containerpb.NodePool{labeled("ate-system-infra")}, []string{"node-pools create ate-postgres"}, []string{"node-pools create ate-system-infra"}},
		{"both present", true, []*containerpb.NodePool{labeled("ate-system-infra"), labeled("ate-postgres")}, nil, []string{"node-pools create"}},
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
	got := systemPoolCommand(systemPoolsConfig(), systemPools(systemPoolsConfig())[0], "us-west1-c")
	want := "gcloud container node-pools create ate-system-infra --cluster=substrate-poc --project=acme --location=us-west1-c --machine-type=e2-standard-4 --num-nodes=1 --node-labels=ate.dev/workloadType=ate-system-infra --node-taints=ate.dev/workloadType=ate-system-infra:NoSchedule --service-account=" + nodeSA + " --scopes=https://www.googleapis.com/auth/cloud-platform --shielded-secure-boot --shielded-integrity-monitoring"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// On a regional cluster the command pins the pool to one zone, so
// --num-nodes=1 means one node rather than one per zone.
func TestSystemPoolCommandRegional(t *testing.T) {
	c := systemPoolsConfig()
	c.ClusterLocation = c.Region
	p := systemPools(c)[0]
	if got := systemPoolCommand(c, p, "us-west1-a"); !strings.HasSuffix(got, " --location=us-west1 --machine-type=e2-standard-4 --num-nodes=1 --node-labels=ate.dev/workloadType=ate-system-infra --node-taints=ate.dev/workloadType=ate-system-infra:NoSchedule --service-account="+nodeSA+" --scopes=https://www.googleapis.com/auth/cloud-platform --shielded-secure-boot --shielded-integrity-monitoring --node-locations=us-west1-a") {
		t.Errorf("regional command = %s", got)
	}
	if got := systemPoolCommand(c, p, ""); !strings.HasSuffix(got, " --node-locations=ZONE") {
		t.Errorf("command without a known zone = %s, want a ZONE placeholder", got)
	}
	logs := captureLogs(t)
	warnMissingSystemPools(c, &containerpb.Cluster{Locations: []string{"us-west1-c", "us-west1-a", "us-west1-b"}})
	if out := logs.String(); strings.Count(out, "--node-locations=us-west1-a") != 2 {
		t.Errorf("warnings do not pin both pools to the first zone:\n%s", out)
	}
}

// A new regional cluster is created with only the worker pool; each system
// pool is then added with one node in the first of the zones GKE picked.
func TestCreateRegionalClusterPutsSystemPoolsInOneZone(t *testing.T) {
	f := newFakeGCP(t)
	f.zones = []string{"us-west1-c", "us-west1-a", "us-west1-b"}
	c := bootstrapConfig(t)
	c.ClusterLocation = c.Region
	if err := createClusterIdempotent(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	want := []string{"CreateCluster", "CreateNodePool ate-system-infra", "CreateNodePool ate-postgres"}
	if calls := f.calls(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if pools := f.cluster.GetNodePools(); pools[0].GetName() != "substrate-node-pool" || len(pools[0].GetLocations()) != 0 {
		t.Errorf("worker pool = %v, want substrate-node-pool in every cluster zone", pools[0])
	}
	for _, req := range f.nodePoolRequests {
		p := req.GetNodePool()
		if req.GetParent() != "projects/acme/locations/us-west1/clusters/substrate-test" {
			t.Errorf("%s parent = %q", p.GetName(), req.GetParent())
		}
		if !slices.Equal(p.GetLocations(), []string{"us-west1-a"}) || p.GetInitialNodeCount() != 1 {
			t.Errorf("%s = %d node(s) in %v, want 1 in [us-west1-a]", p.GetName(), p.GetInitialNodeCount(), p.GetLocations())
		}
		if p.GetConfig().GetLabels()[workloadTypeLabel] != p.GetName() {
			t.Errorf("%s labels = %v", p.GetName(), p.GetConfig().GetLabels())
		}
	}

	// A re-run finds both pools and adds nothing.
	logs := captureLogs(t)
	if err := createClusterIdempotent(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "node-pools create") || slices.Contains(f.calls()[len(want):], "CreateNodePool ate-system-infra") {
		t.Errorf("re-run touched the system pools: %v\n%s", f.calls(), logs.String())
	}
}

// A zonal cluster gets its system pools in CreateCluster, as before.
func TestCreateZonalClusterIncludesSystemPools(t *testing.T) {
	f := newFakeGCP(t)
	c := bootstrapConfig(t)
	if err := createClusterIdempotent(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); !slices.Equal(calls, []string{"CreateCluster"}) {
		t.Fatalf("calls = %v, want [CreateCluster]", calls)
	}
	var names []string
	for _, p := range f.cluster.GetNodePools() {
		names = append(names, p.GetName())
		if len(p.GetLocations()) != 0 {
			t.Errorf("%s locations = %v, want the cluster's zone", p.GetName(), p.GetLocations())
		}
	}
	if !slices.Equal(names, []string{"substrate-node-pool", "ate-system-infra", "ate-postgres"}) {
		t.Errorf("pools = %v", names)
	}
}

// With an external database ate-setup deploys no PostgreSQL, so there is no
// ate-postgres pool to create or to ask for.
func TestExternalPostgresSkipsThePostgresPool(t *testing.T) {
	for _, env := range []string{"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING", "ATE_API_POSTGRES_CLOUDSQL_INSTANCE"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING", "")
			t.Setenv("ATE_API_POSTGRES_CLOUDSQL_INSTANCE", "")
			t.Setenv(env, "set")
			c := systemPoolsConfig()
			var names []string
			for _, p := range buildCreateClusterRequest("projects/acme/locations/us-west1-c", c).GetCluster().GetNodePools() {
				names = append(names, p.GetName())
			}
			if !slices.Equal(names, []string{"substrate-node-pool", "ate-system-infra"}) {
				t.Errorf("pools = %v, want no ate-postgres", names)
			}
			logs := captureLogs(t)
			warnMissingSystemPools(c, &containerpb.Cluster{})
			if out := logs.String(); strings.Contains(out, "ate-postgres") || !strings.Contains(out, "node-pools create ate-system-infra") {
				t.Errorf("warnings:\n%s", out)
			}
		})
	}
}
