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
	"os"
	"slices"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/proto"
)

// bootstrapConfig returns the Config `setup-gcp bootstrap` runs with when
// invoked with no flags, as the installer does, plus the environment values
// the installer passes for a cluster named substrate-test in us-west1-c.
// Network and Subnetwork are "default" because the wizard sends those unless
// the user changes them; it never reads them from an existing cluster.
func bootstrapConfig(t *testing.T) Config {
	t.Helper()
	if _, set := os.LookupEnv("ENABLE_DATAPLANE_V2"); set {
		t.Skip("ENABLE_DATAPLANE_V2 is set in the environment; the flag defaults under test depend on it")
	}
	if err := bootstrapCmd.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	c := cfg
	c.ProjectID = "acme"
	c.ProjectNumber = "42"
	c.Region = "us-west1"
	c.ClusterLocation = "us-west1-c"
	c.ClusterName = "substrate-test"
	c.Network = "default"
	c.Subnetwork = "default"
	return c
}

// syncedCluster is an existing cluster that already matches bootstrapConfig
// in every attribute createClusterIdempotent checks.
func syncedCluster() *containerpb.Cluster {
	return &containerpb.Cluster{
		Name: "substrate-test",
		NetworkConfig: &containerpb.NetworkConfig{
			Network:          "projects/acme/global/networks/default",
			Subnetwork:       "projects/acme/regions/us-west1/subnetworks/default",
			DatapathProvider: containerpb.DatapathProvider_ADVANCED_DATAPATH,
		},
		WorkloadIdentityConfig:     &containerpb.WorkloadIdentityConfig{WorkloadPool: "acme.svc.id.goog"},
		EnableK8SBetaApis:          &containerpb.K8SBetaAPIConfig{EnabledApis: requiredBetaAPIs},
		ManagedOpentelemetryConfig: &containerpb.ManagedOpenTelemetryConfig{Scope: containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS.Enum()},
		NodePools: []*containerpb.NodePool{{
			Name: "substrate-node-pool",
			Config: &containerpb.NodeConfig{
				AdvancedMachineFeatures: &containerpb.AdvancedMachineFeatures{EnableNestedVirtualization: proto.Bool(true)},
			},
		}},
	}
}

// Review question: bootstrap never registers --enable-dataplane-v2, so is
// cfg.EnableDataplaneV2 always false under bootstrap? `create cluster`
// registers that flag on the same package-level cfg, and pflag writes a
// flag's default into its variable when the flag is defined, so bootstrap
// inherits the same default (true).
func TestBootstrapEnablesDataplaneV2ByDefault(t *testing.T) {
	c := bootstrapConfig(t)
	if !c.EnableDataplaneV2 {
		t.Errorf("bootstrap runs with EnableDataplaneV2=false; it would create clusters without Dataplane V2 and treat existing Dataplane V2 clusters as a mismatch")
	}
	req := buildCreateClusterRequest("projects/acme/locations/us-west1-c", &c)
	if got := req.GetCluster().GetNetworkConfig().GetDatapathProvider(); got != containerpb.DatapathProvider_ADVANCED_DATAPATH {
		t.Errorf("bootstrap would create a cluster with datapath %v, want ADVANCED_DATAPATH", got)
	}
}

// An existing cluster that already matches needs no changes at all. This also
// checks the fake: a synced cluster must produce no mutating calls.
func TestBootstrapLeavesASyncedClusterAlone(t *testing.T) {
	f := newFakeGCP(t)
	f.cluster = syncedCluster()
	c := bootstrapConfig(t)
	if err := createClusterIdempotent(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("synced cluster got mutating calls %v, want none", calls)
	}
}

// Review question: does bootstrap delete and recreate an existing cluster
// whose network, subnetwork, or Dataplane V2 setting differs from its
// configuration? The installer's provision step promises an existing cluster
// is only filled in (bucket, IAM, dashboards), and the wizard sends
// NETWORK=default and SUBNETWORK=default for any cluster the user picks.
// Deleting the cluster destroys the user's workloads, so none of these
// differences may lead to DeleteCluster or CreateCluster.
func TestBootstrapNeverRecreatesAnExistingCluster(t *testing.T) {
	tests := []struct {
		name  string
		drift func(*containerpb.Cluster)
	}{
		{"custom VPC network", func(c *containerpb.Cluster) {
			c.NetworkConfig.Network = "projects/acme/global/networks/team-vpc"
		}},
		{"custom subnetwork", func(c *containerpb.Cluster) {
			c.NetworkConfig.Subnetwork = "projects/acme/regions/us-west1/subnetworks/team-subnet"
		}},
		{"Dataplane V2 off", func(c *containerpb.Cluster) {
			c.NetworkConfig.DatapathProvider = containerpb.DatapathProvider_LEGACY_DATAPATH
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGCP(t)
			f.cluster = syncedCluster()
			tt.drift(f.cluster)
			c := bootstrapConfig(t)
			if err := createClusterIdempotent(context.Background(), &c); err != nil {
				t.Fatal(err)
			}
			calls := f.calls()
			if slices.Contains(calls, "DeleteCluster") || slices.Contains(calls, "CreateCluster") {
				t.Errorf("bootstrap recreated an existing cluster over a %s: calls %v", tt.name, calls)
			}
		})
	}
}
