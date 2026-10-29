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
	"fmt"
	"log/slog"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/spf13/cobra"
)

// Substrate's system components (API server, controller, router, egress
// gateway and the rest) run on a node pool of their own, and its bundled
// PostgreSQL on another, so a sandbox escape on a worker node does not land
// next to them. ate-setup pins them there when ATE_INSTALL_CORDON_CONTROL_PLANE
// is set, by node label and taint; the installer sets it when the cluster has
// both pools.
//
// The values are those of the Substrate release the installer pins (v0.6.0).
// Later releases call the system pool's workload type ate-system-infra and the
// switch ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS.
const (
	workloadTypeLabel = "ate.dev/workloadType"

	systemInfraPoolName     = "ate-system-infra"
	systemInfraWorkloadType = "ate-control-plane"

	// ate-setup refuses to deploy bundled PostgreSQL with cordoning on
	// unless a node carries this workload type.
	postgresPoolName     = "ate-postgres"
	postgresWorkloadType = "ate-postgres"
)

// systemPool is one of the pools Substrate's system components run on.
type systemPool struct {
	name, workloadType, machineType string
}

func systemPools(cfg *Config) []systemPool {
	return []systemPool{
		{systemInfraPoolName, systemInfraWorkloadType, cfg.SystemInfraMachineType},
		{postgresPoolName, postgresWorkloadType, cfg.PostgresMachineType},
	}
}

// addSystemNodeFlags registers the system node pool flags on cmd.
func addSystemNodeFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&cfg.SeparateSystemNodes, "separate-system-nodes", getEnv("SEPARATE_SYSTEM_NODES", true),
		"Give a new cluster an "+systemInfraPoolName+" and an "+postgresPoolName+" node pool, which keep Substrate's system components and bundled PostgreSQL off the worker nodes. Turn off with --separate-system-nodes=false [env: SEPARATE_SYSTEM_NODES]")
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

// systemNodePools are the pools added to a new cluster when
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
// the settings a new cluster gets.
func systemPoolCommand(cfg *Config, p systemPool) string {
	return strings.Join([]string{
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
	}, " ")
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
			slog.String("command", systemPoolCommand(cfg, p)))
	}
}
