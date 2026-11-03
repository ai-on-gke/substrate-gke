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
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/spf13/cobra"
)

// Substrate's system components (API server, controller, router, egress
// gateway and the rest) run on a node pool of their own, and its bundled
// PostgreSQL on another, so a sandbox escape on a worker node does not land
// next to them. ate-setup pins them there when
// ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS is set, by node label and taint;
// the installer sets it when the cluster has the pools.
const (
	workloadTypeLabel = "ate.dev/workloadType"

	systemInfraPoolName     = "ate-system-infra"
	systemInfraWorkloadType = "ate-system-infra"

	// ate-setup refuses to deploy bundled PostgreSQL with cordoning on
	// unless a node carries this workload type.
	postgresPoolName     = "ate-postgres"
	postgresWorkloadType = "ate-postgres"
)

// systemPool is one of the pools Substrate's system components run on.
type systemPool struct {
	name, workloadType, machineType string
}

// systemPools are the pools to create: ate-postgres only for the bundled
// PostgreSQL.
func systemPools(cfg *Config) []systemPool {
	pools := []systemPool{{systemInfraPoolName, systemInfraWorkloadType, cfg.SystemInfraMachineType}}
	if !externalPostgres() {
		pools = append(pools, systemPool{postgresPoolName, postgresWorkloadType, cfg.PostgresMachineType})
	}
	return pools
}

// externalPostgres reports whether ate-setup will use an external database
// instead of the bundled PostgreSQL. These are the variables its
// planPostgres reads.
func externalPostgres() bool {
	return os.Getenv("ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING") != "" ||
		os.Getenv("ATE_API_POSTGRES_CLOUDSQL_INSTANCE") != ""
}

// addSystemNodeFlags registers the system node pool flags on cmd.
func addSystemNodeFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&cfg.SeparateSystemNodes, "separate-system-nodes", getEnv("SEPARATE_SYSTEM_NODES", true),
		"Give a new cluster an "+systemInfraPoolName+" and an "+postgresPoolName+" node pool of one node each (in a single zone on a regional cluster), which keep Substrate's system components and bundled PostgreSQL off the worker nodes. No "+postgresPoolName+" pool when ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING or ATE_API_POSTGRES_CLOUDSQL_INSTANCE selects an external database. Turn off with --separate-system-nodes=false [env: SEPARATE_SYSTEM_NODES]")
	cmd.Flags().StringVar(&cfg.SystemInfraMachineType, "system-infra-machine-type", getEnv("SYSTEM_INFRA_MACHINE_TYPE", "e2-standard-4"),
		"Machine type for the "+systemInfraPoolName+" node pool [env: SYSTEM_INFRA_MACHINE_TYPE]")
	// Bundled PostgreSQL requests 2 CPUs, which a 2-vCPU machine cannot
	// allocate once GKE's own daemons take their share.
	cmd.Flags().StringVar(&cfg.PostgresMachineType, "postgres-machine-type", getEnv("POSTGRES_MACHINE_TYPE", "e2-standard-4"),
		"Machine type for the "+postgresPoolName+" node pool [env: POSTGRES_MACHINE_TYPE]")
}

// hardenNodeConfig sets what every node pool this tool creates shares: the
// node service account (see ensureNodeServiceAccount) and Secure Boot.
func hardenNodeConfig(cfg *Config, nc *containerpb.NodeConfig) {
	// The node service account holds only what nodes need, so the broad
	// scope GKE recommends with a custom service account grants nothing
	// beyond its IAM roles.
	nc.ServiceAccount = nodeGSAEmail(cfg)
	nc.OauthScopes = []string{"https://www.googleapis.com/auth/cloud-platform"}
	nc.ShieldedInstanceConfig = &containerpb.ShieldedInstanceConfig{
		EnableSecureBoot:          true,
		EnableIntegrityMonitoring: true,
	}
}

// systemNodePools are the system pools for a new cluster, empty unless
// cfg.SeparateSystemNodes is set. One node each: the system Deployments
// spread across nodes only when they can (ScheduleAnyway), and run fine on
// one. They get none of the worker pool's sandbox settings (nested
// virtualization, high-density sysctls, boot disk).
func systemNodePools(cfg *Config) []*containerpb.NodePool {
	if !cfg.SeparateSystemNodes {
		return nil
	}
	var pools []*containerpb.NodePool
	for _, p := range systemPools(cfg) {
		nc := &containerpb.NodeConfig{
			MachineType: p.machineType,
			Labels:      map[string]string{workloadTypeLabel: p.workloadType},
			Taints: []*containerpb.NodeTaint{{
				Key:    workloadTypeLabel,
				Value:  p.workloadType,
				Effect: containerpb.NodeTaint_NO_SCHEDULE,
			}},
		}
		hardenNodeConfig(cfg, nc)
		pools = append(pools, &containerpb.NodePool{Name: p.name, InitialNodeCount: 1, Config: nc})
	}
	return pools
}

// regional reports whether the cluster is regional: validateClusterLocation
// allows only the region itself or a zone in it.
func regional(cfg *Config) bool {
	return cfg.ClusterLocation != "" && cfg.ClusterLocation == cfg.Region
}

// systemPoolZone is the one zone a regional cluster's system pools run in:
// the first of the cluster's node locations, so the choice is stable across
// runs. Empty when the cluster reports none.
func systemPoolZone(cluster *containerpb.Cluster) string {
	zones := slices.Sorted(slices.Values(cluster.GetLocations()))
	if len(zones) == 0 {
		return ""
	}
	return zones[0]
}

// addRegionalSystemPools adds the system pools to a regional cluster this
// tool just created, in one zone, so each is one node rather than one per
// zone. Users can scale them up or add zones later.
//
// They are added after the cluster exists, not in CreateCluster, because
// GKE picks a regional cluster's zones at creation (Cluster.Locations) and
// a pool without Locations gets them all. Taking the zone from the created
// cluster's Locations keeps the pool inside them, without relying on GKE
// accepting a zone the cluster has no nodes in.
func addRegionalSystemPools(ctx context.Context, cfg *Config, client *container.ClusterManagerClient, clusterName string) error {
	pools := systemNodePools(cfg)
	if len(pools) == 0 {
		return nil
	}
	cluster, err := client.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName})
	if err != nil {
		return fmt.Errorf("getting cluster: %w", err)
	}
	zone := systemPoolZone(cluster)
	if zone == "" {
		return fmt.Errorf("cluster %s reports no node locations to put the system node pools in", cfg.ClusterName)
	}
	for _, pool := range pools {
		pool.Locations = []string{zone}
		slog.Info("Creating node pool", slog.String("pool", pool.GetName()), slog.String("zone", zone))
		op, err := client.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: clusterName, NodePool: pool})
		if err != nil {
			return fmt.Errorf("create node pool %s: %w", pool.GetName(), err)
		}
		if err := waitContainerOperation(ctx, client, op.Name, cfg); err != nil {
			return err
		}
	}
	return nil
}

// missingSystemPools returns the system pools an existing cluster has no
// pool for, matched by workload type label rather than by name.
func missingSystemPools(cluster *containerpb.Cluster, cfg *Config) []systemPool {
	have := map[string]bool{}
	for _, pool := range cluster.GetNodePools() {
		have[pool.GetConfig().GetLabels()[workloadTypeLabel]] = true
	}
	var missing []systemPool
	for _, p := range systemPools(cfg) {
		if !have[p.workloadType] {
			missing = append(missing, p)
		}
	}
	return missing
}

// systemPoolCommand is the command that adds p to an existing cluster with
// the settings a new cluster gets. On a regional cluster it pins the pool to
// zone (ZONE when unknown), so --num-nodes=1 is one node, not one per zone.
func systemPoolCommand(cfg *Config, p systemPool, zone string) string {
	args := []string{
		"gcloud container node-pools create " + p.name,
		"--cluster=" + cfg.ClusterName,
		"--project=" + cfg.ProjectID,
		"--location=" + cfg.ClusterLocation,
		"--machine-type=" + p.machineType,
		"--num-nodes=1",
		fmt.Sprintf("--node-labels=%s=%s", workloadTypeLabel, p.workloadType),
		fmt.Sprintf("--node-taints=%s=%s:NoSchedule", workloadTypeLabel, p.workloadType),
		"--service-account=" + nodeGSAEmail(cfg),
		"--scopes=https://www.googleapis.com/auth/cloud-platform",
		"--shielded-secure-boot",
		"--shielded-integrity-monitoring",
	}
	if regional(cfg) {
		if zone == "" {
			zone = "ZONE"
		}
		args = append(args, "--node-locations="+zone)
	}
	return strings.Join(args, " ")
}

// warnMissingSystemPools tells the user how to separate system nodes on an
// existing cluster. This tool never adds pools to a cluster it did not create;
// without them the installer leaves the system components on the worker
// nodes.
func warnMissingSystemPools(cfg *Config, cluster *containerpb.Cluster) {
	if !cfg.SeparateSystemNodes {
		return
	}
	for _, p := range missingSystemPools(cluster, cfg) {
		slog.Warn("Cluster has no "+p.name+" node pool, so Substrate's system components will share the worker nodes; add it to separate them",
			slog.String("cluster", cfg.ClusterName),
			slog.String("command", systemPoolCommand(cfg, p, systemPoolZone(cluster))))
	}
}
