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

// Package gcp reads project and cluster facts through the gcloud CLI, which
// the doctor step already requires and which handles auth and retries.
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// RequiredBetaAPIs are the Kubernetes beta APIs Substrate's podcertificate
// controller depends on. GKE only honors them when they are enabled at
// cluster creation time; enabling them on an existing cluster is accepted by
// the API but never served, and the install then hangs waiting for
// ClusterTrustBundles.
var RequiredBetaAPIs = []string{
	"certificates.k8s.io/v1beta1/podcertificaterequests",
	"certificates.k8s.io/v1beta1/clustertrustbundles",
}

// Cluster is one GKE cluster as listed by gcloud.
type Cluster struct {
	Name          string
	Location      string
	Status        string
	MasterVersion string
	NodeCount     int
	BetaAPIs      []string
	// KVMReady reports whether any node pool in the cluster has /dev/kvm
	// available (nested virtualization enabled or a bare-metal machine type).
	KVMReady bool
	// Network and Subnetwork are the cluster's networkConfig paths, as gcloud
	// lists them ("projects/P/global/networks/N",
	// "projects/P/regions/R/subnetworks/S"), and DataplaneV2 whether its
	// datapath is ADVANCED_DATAPATH. Empty and false when gcloud listed no
	// networkConfig. See BootstrapRecreates for why they matter.
	Network     string
	Subnetwork  string
	DataplaneV2 bool
}

// NetworkName and SubnetworkName are the last segments of the cluster's
// network paths: the short names setup-gcp's NETWORK and SUBNETWORK take.
func (c Cluster) NetworkName() string    { return lastSegment(c.Network) }
func (c Cluster) SubnetworkName() string { return lastSegment(c.Subnetwork) }

func lastSegment(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// BootstrapRecreates reports why setup-gcp's bootstrap would delete and
// recreate this cluster even when it is given the cluster's own network
// names, or "" when it would not.
//
// Bootstrap's reconcile path deletes an existing cluster and creates a new
// one, without asking, when its network, subnetwork or Dataplane V2 setting
// differs from what it was configured with (agent-substrate/substrate#2341).
// The installer configures it with the cluster's own names and datapath, so
// for most clusters it finds nothing to change. But it builds the expected
// paths from PROJECT_ID and GCE_REGION, not from anything the installer can
// pass, so two cases can never match:
//
//   - a network that lives in another project, which is what Shared VPC is;
//   - a subnetwork outside the region the installer derives from the
//     cluster's location.
//
// Those clusters are refused rather than handed to bootstrap. A cluster
// whose networkConfig was not listed is not refused: bootstrap skips both
// comparisons when the cluster reports no network.
//
// The comparisons mirror bootstrap's own: it checks that the cluster's path
// ends with the one it built, so this does the same with the names the
// installer will pass, and accepts whatever prefix gcloud puts in front.
func (c Cluster) BootstrapRecreates(projectID, region string) string {
	if c.Network != "" && !strings.HasSuffix(c.Network, "projects/"+projectID+"/global/networks/"+c.NetworkName()) {
		return fmt.Sprintf("its network %s belongs to another project (Shared VPC), and setup-gcp only matches networks in %s", c.Network, projectID)
	}
	if c.Subnetwork != "" && !strings.HasSuffix(c.Subnetwork, "projects/"+projectID+"/regions/"+region+"/subnetworks/"+c.SubnetworkName()) {
		return fmt.Sprintf("its subnetwork %s is not in project %s, region %s, the only place setup-gcp looks", c.Subnetwork, projectID, region)
	}
	return ""
}

// SubstrateReady reports whether the cluster serves the beta APIs Substrate
// needs.
func (c Cluster) SubstrateReady() bool {
	for _, want := range RequiredBetaAPIs {
		if !slices.Contains(c.BetaAPIs, want) {
			return false
		}
	}
	return true
}

// NodePool is one GKE node pool.
type NodePool struct {
	Name        string
	MachineType string
	Autoscaled  bool
}

// Client shells out to gcloud. With DryRun set it returns canned data so the
// wizard can be exercised without a GCP project.
type Client struct {
	DryRun bool
	// crmBase overrides the Cloud Resource Manager endpoint; tests point it
	// at a local server. Empty means the real one.
	crmBase string
	// token overrides how MissingPermissions obtains an access token; nil
	// means asking gcloud for the application-default one.
	token func(ctx context.Context) (string, error)
}

const cmdTimeout = 60 * time.Second

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gcloud", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gcloud %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// CurrentProject returns the active gcloud project, or "" when unset.
func (c *Client) CurrentProject(ctx context.Context) string {
	if c.DryRun {
		return "my-substrate-project"
	}
	out, err := c.run(ctx, "config", "get-value", "project")
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "(unset)" {
		return ""
	}
	return p
}

// ProjectNumber resolves the numeric project number setup-gcp requires.
func (c *Client) ProjectNumber(ctx context.Context, projectID string) (string, error) {
	if c.DryRun {
		return "123456789012", nil
	}
	out, err := c.run(ctx, "projects", "describe", projectID, "--format=value(projectNumber)")
	if err != nil {
		return "", err
	}
	n := strings.TrimSpace(string(out))
	if n == "" {
		return "", fmt.Errorf("project %s has no project number", projectID)
	}
	return n, nil
}

// ListClusters lists the project's GKE clusters with their beta-API status.
func (c *Client) ListClusters(ctx context.Context, projectID string) ([]Cluster, error) {
	if c.DryRun {
		// The last two names are cues for CheckInstalled's dry-run sim, so a
		// --dry-run walkthrough shows the install guard's whole story:
		// clean, blocked-installed, and partial.
		return []Cluster{
			{Name: "substrate-poc", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.35.5-gke.1163012", NodeCount: 2, BetaAPIs: RequiredBetaAPIs, KVMReady: true},
			{Name: "legacy-prod", Location: "us-central1", Status: "RUNNING",
				MasterVersion: "1.33.2-gke.100", NodeCount: 12},
			{Name: "substrate-installed", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.35.5-gke.1163012", NodeCount: 3, BetaAPIs: RequiredBetaAPIs, KVMReady: true},
			{Name: "substrate-partial", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.35.5-gke.1163012", NodeCount: 1, BetaAPIs: RequiredBetaAPIs},
		}, nil
	}
	out, err := c.run(ctx, "container", "clusters", "list", "--project="+projectID, "--format=json")
	if err != nil {
		return nil, err
	}
	return ParseClusters(out)
}

type rawNodePool struct {
	Config struct {
		MachineType             string `json:"machineType"`
		AdvancedMachineFeatures struct {
			EnableNestedVirtualization bool `json:"enableNestedVirtualization"`
		} `json:"advancedMachineFeatures"`
	} `json:"config"`
}

// kvmReady reports whether at least one node pool exposes /dev/kvm to workloads,
// either via GCE nested virtualization or via a bare-metal machine shape (whose
// GCE names end in "-metal", e.g. "c3-standard-192-metal", "c3d-standard-360-metal").
func kvmReady(pools []rawNodePool) bool {
	for _, np := range pools {
		if np.Config.AdvancedMachineFeatures.EnableNestedVirtualization ||
			strings.HasSuffix(np.Config.MachineType, "-metal") {
			return true
		}
	}
	return false
}

// ClusterKVMReady queries gcloud to check whether the named cluster currently
// has a KVM-capable node pool. It describes only the target cluster rather than
// listing every cluster in the project so large projects do not time out.
func (c *Client) ClusterKVMReady(ctx context.Context, projectID, cluster, location string) (bool, error) {
	if c.DryRun {
		clusters, err := c.ListClusters(ctx, projectID)
		if err != nil {
			return false, err
		}
		for _, cl := range clusters {
			if cl.Name == cluster && (location == "" || cl.Location == location) {
				return cl.KVMReady, nil
			}
		}
		return false, nil
	}
	out, err := c.run(ctx, "container", "clusters", "describe", cluster,
		"--project="+projectID, "--location="+location, "--format=json")
	if err != nil {
		return false, err
	}
	return ParseClusterKVMReady(out)
}

// ParseClusterKVMReady decodes `gcloud container clusters describe --format=json`
// and reports whether any of the cluster's node pools is KVM-capable.
func ParseClusterKVMReady(data []byte) (bool, error) {
	var raw struct {
		NodePools []rawNodePool `json:"nodePools"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, fmt.Errorf("parsing cluster description: %w", err)
	}
	return kvmReady(raw.NodePools), nil
}

// ParseClusters decodes `gcloud container clusters list --format=json`.
func ParseClusters(data []byte) ([]Cluster, error) {
	var raw []struct {
		Name                 string `json:"name"`
		Location             string `json:"location"`
		Status               string `json:"status"`
		CurrentMasterVersion string `json:"currentMasterVersion"`
		CurrentNodeCount     int    `json:"currentNodeCount"`
		EnableK8sBetaApis    struct {
			EnabledApis []string `json:"enabledApis"`
		} `json:"enableK8sBetaApis"`
		NodePools     []rawNodePool `json:"nodePools"`
		NetworkConfig struct {
			Network          string `json:"network"`
			Subnetwork       string `json:"subnetwork"`
			DatapathProvider string `json:"datapathProvider"`
		} `json:"networkConfig"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing cluster list: %w", err)
	}
	clusters := make([]Cluster, 0, len(raw))
	for _, r := range raw {
		clusters = append(clusters, Cluster{
			Name:          r.Name,
			Location:      r.Location,
			Status:        r.Status,
			MasterVersion: r.CurrentMasterVersion,
			NodeCount:     r.CurrentNodeCount,
			BetaAPIs:      r.EnableK8sBetaApis.EnabledApis,
			KVMReady:      kvmReady(r.NodePools),
			Network:       r.NetworkConfig.Network,
			Subnetwork:    r.NetworkConfig.Subnetwork,
			DataplaneV2:   r.NetworkConfig.DatapathProvider == "ADVANCED_DATAPATH",
		})
	}
	return clusters, nil
}

// ListNodePools lists a cluster's node pools.
func (c *Client) ListNodePools(ctx context.Context, projectID, cluster, location string) ([]NodePool, error) {
	if c.DryRun {
		return []NodePool{{Name: "substrate-node-pool", MachineType: "c3-standard-4"}}, nil
	}
	out, err := c.run(ctx, "container", "node-pools", "list",
		"--project="+projectID, "--cluster="+cluster, "--location="+location, "--format=json")
	if err != nil {
		return nil, err
	}
	return ParseNodePools(out)
}

// ParseNodePools decodes `gcloud container node-pools list --format=json`.
func ParseNodePools(data []byte) ([]NodePool, error) {
	var raw []struct {
		Name   string `json:"name"`
		Config struct {
			MachineType string `json:"machineType"`
		} `json:"config"`
		Autoscaling struct {
			Enabled bool `json:"enabled"`
		} `json:"autoscaling"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing node pool list: %w", err)
	}
	pools := make([]NodePool, 0, len(raw))
	for _, r := range raw {
		pools = append(pools, NodePool{
			Name:        r.Name,
			MachineType: r.Config.MachineType,
			Autoscaled:  r.Autoscaling.Enabled,
		})
	}
	return pools, nil
}
