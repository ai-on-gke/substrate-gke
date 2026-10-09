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
	"strconv"
	"strings"
	"time"
)

// RequiredBetaAPIs are the Kubernetes beta APIs a cluster needs unless it
// serves the same two resources as GA everywhere that matters — a 1.37 control
// plane and only 1.37 node pools (see Cluster.ServesPodCertificatesAsGA).
//
// That line is a fact about the pinned upstream as much as about Kubernetes,
// and it has moved once already, so the reasoning is worth keeping:
//
//   - From v0.4.0, every Substrate component discovers the API version: it
//     prefers certificates.k8s.io/v1 and falls back to v1beta1 only when v1 is
//     not served (upstream #1829 for PodCertificateRequest, #1924 for
//     ClusterTrustBundle). Before #1924, ClusterTrustBundle was v1beta1-only
//     and a plain 1.37 cluster hung at "Waiting for podcertificate
//     ClusterTrustBundles to be ready"; a pin older than v0.4.0 would need
//     the beta APIs on 1.37 again.
//   - Below 1.37 the kubelet serves pod certificate projection only behind the
//     beta gate, so any node pool below 1.37 still needs the beta APIs whatever
//     its control plane serves.
//
// setup-gcp's bootstrap still requests both on every cluster it creates and
// turns both on for an existing one that lacks them, even where they are no
// longer needed. That is harmless but not free — it is a control-plane update
// of about ten minutes — so the provision screen says so (MissingBetaAPIs)
// rather than readiness pretending it is required.
//
// The claim that they cannot be enabled on an existing cluster is wrong: on
// both 1.36 and 1.37, `clusters update --enable-kubernetes-unstable-apis` was
// accepted and the APIs were served afterward (measured 2026-09-21; see
// agent-substrate/substrate#1819).
//
// GKE serves them only when they are listed in the cluster's enableK8sBetaApis,
// which is off by default. They are deprecated from 1.37 and removed in 1.40.
var RequiredBetaAPIs = []string{
	"certificates.k8s.io/v1beta1/podcertificaterequests",
	"certificates.k8s.io/v1beta1/clustertrustbundles",
}

// The oldest release Agent Substrate is supported on, per GKE's own
// requirements: "Runs GKE version 1.36 (with beta flags enabled) or version 1.37
// or later."
//
// https://docs.cloud.google.com/kubernetes-engine/ai-ml/install-overview-substrate#cluster-requirements
//
// This is a *supported*-version floor, deliberately one minor above the
// technical one, and the gap is worth knowing about before anyone lowers it.
// Both beta APIs exist from 1.35 — ClusterTrustBundle reached v1beta1 in 1.33
// but PodCertificateRequest only in 1.35 — and GKE really does accept a 1.35
// create with both of them enabled; that was measured, not assumed. 1.35 is
// simply not a configuration Substrate is tested or supported on, and an
// installer that waves it through is promising something nobody is standing
// behind.
//
// Below 1.35 there is no judgement call left: the enablement cannot be
// requested at all. GKE rejects the create or update outright:
//
//	Beta API "certificates.k8s.io/v1beta1/podcertificaterequests" is not
//	available in version "1.34.11-gke.1102000"
var MinSupportedRelease = Release{1, 36}

// The first Kubernetes release serving the PodCertificate APIs as GA.
//
// It decides two things. A cluster whose control plane and every node pool are
// at or above it needs no beta APIs at all (see RequiredBetaAPIs). For one that
// does need them and lacks them, it decides how the repair goes:
//
//   - At or above it, the projection is GA, so the kubelet implements it on
//     every node. Enabling the beta APIs on the running cluster is the whole fix.
//   - Below it, the projection is itself gated, and a node only picks it up if
//     it was created after the APIs were enabled. Existing nodes keep failing to
//     mount with "unimplemented" until their node pool is replaced.
var PodCertificateGARelease = Release{1, 37}

// The first release carrying both beta APIs — the technical floor the comment
// above distinguishes from the supported one, and the only reason to care about
// it is what the user is told. At or above this line GKE accepts the
// enablement, so an unsupported cluster is unsupported and nothing more. Below
// it GKE also rejects the request outright, which is worth quoting back rather
// than letting someone discover it by trying.
var BetaAPIsExistRelease = Release{1, 35}

// Release is a Kubernetes minor release, the granularity every line above is
// drawn at. One value holds both the number the comparisons read and the text
// the user is shown, so the two cannot drift apart.
type Release struct{ Major, Minor int }

// String is the bare minor ("1.36"). GKE accepts it wherever it takes a
// version and resolves it to the newest patch it serves, so it is also safe to
// print into a command.
func (r Release) String() string { return fmt.Sprintf("%d.%d", r.Major, r.Minor) }

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
	// PoolVersions is each node pool's name and Kubernetes version, in
	// gcloud's order. Empty when gcloud listed no pools (Autopilot) or for a
	// fixture that did not bother.
	PoolVersions []PoolVersion
}

// PoolVersion is one node pool's name and the version its kubelets run.
type PoolVersion struct{ Name, Version string }

// PoolReplacement is which of a cluster's node pools may not serve pod
// certificate projection, and so may have to be replaced before Substrate is
// turned on. One value, so the verdict and its two qualifications travel
// together and every consumer reads the same thing.
type PoolReplacement struct {
	// Needed reports whether any pool is below PodCertificateGARelease (or,
	// with no pools listed, whether the control plane is).
	Needed bool
	// Names are those pools, when gcloud listed pools by version; empty
	// when Needed rests on the control plane's version alone.
	Names []string
	// Unsure reports that the beta APIs are already on, so whether these
	// pools' nodes predate them — and so cannot mount — is not something the
	// listing can tell. The usual way here is a re-run after an earlier one
	// turned the APIs on and the user left to replace the pools: dropping the
	// advice then would hang Substrate's pods on the nodes they never
	// replaced.
	Unsure bool
}

// PoolReplacement reports which node pools' kubelets may not serve pod
// certificate projection: every pool below PodCertificateGARelease, where the
// feature is gated through 1.36 and a kubelet already running when the beta
// APIs were turned on never picks it up.
//
// It reads the pools' versions, not the control plane's, because the kubelet
// is what implements the projection and GKE lets node pools run a minor or
// two behind their control plane: a 1.37 cluster can still have 1.36 nodes.
// A pool whose version cannot be read is included, on the same reasoning as
// PodCertificateGA: a spurious "replace this pool" costs minutes, a missing
// one costs an install that hangs on a mount. With no pools listed it falls
// back to the control plane's version and names no pools.
func (c Cluster) PoolReplacement() PoolReplacement {
	var r PoolReplacement
	if len(c.PoolVersions) == 0 {
		r.Needed = !c.PodCertificateGA()
	}
	for _, p := range c.PoolVersions {
		if !(Cluster{MasterVersion: p.Version}).PodCertificateGA() {
			r.Names = append(r.Names, p.Name)
		}
	}
	if len(r.Names) > 0 {
		r.Needed = true
	}
	r.Unsure = r.Needed && !c.MissingBetaAPIs()
	return r
}

// SupportedRelease reports whether this cluster's release is one Substrate is
// supported on. It is half of readiness — a cluster can clear this bar and still
// be missing the beta APIs — and it also selects what a cluster that fails is
// told, since below the floor no amount of enabling helps.
//
// A version that cannot be read reports true, the opposite default to
// PodCertificateGA and for the same reason: each answers toward the outcome
// that wastes the least of the user's time when we are guessing. Being waved
// through onto an unsupported release costs a failed install the user can
// retry; being told a working cluster is too old costs a cluster rebuild.
func (c Cluster) SupportedRelease() bool {
	atLeast, ok := atLeastMinor(c.MasterVersion, MinSupportedRelease)
	return !ok || atLeast
}

// BetaAPIsAvailable reports whether this cluster's release carries the beta
// APIs at all, and so whether asking GKE to enable them would be accepted. It
// is not a readiness question — every release this returns true for can still
// be below the supported floor. An unreadable version reports true, matching
// SupportedRelease: neither guess should invent an error GKE never returned.
func (c Cluster) BetaAPIsAvailable() bool {
	atLeast, ok := atLeastMinor(c.MasterVersion, BetaAPIsExistRelease)
	return !ok || atLeast
}

// PodCertificateGA reports whether this cluster's release serves the
// PodCertificate APIs as GA. It says nothing about readiness — see the constants
// above for what it is actually for, which is choosing the repair instructions.
//
// A version that cannot be read reports false, which buys the wordier of the two
// remedies. Telling someone to replace node pools that did not need it wastes
// their time; omitting it leaves them staring at a pod that will never mount.
func (c Cluster) PodCertificateGA() bool {
	atLeast, ok := atLeastMinor(c.MasterVersion, PodCertificateGARelease)
	return ok && atLeast
}

// MissingBetaAPIs reports whether any of RequiredBetaAPIs is not enabled on
// the cluster, which is exactly when setup-gcp's bootstrap will run its
// control-plane update to turn them on. It is deliberately not the inverse of
// SubstrateReady: a cluster below the floor that already serves both APIs is
// not ready, yet bootstrap has nothing to enable on it, and anything that
// promises the user a ten-minute update has to ask this question instead.
func (c Cluster) MissingBetaAPIs() bool {
	for _, want := range RequiredBetaAPIs {
		if !slices.Contains(c.BetaAPIs, want) {
			return true
		}
	}
	return false
}

// ServesPodCertificatesAsGA reports whether the cluster serves everything
// Substrate needs without the beta APIs: a control plane at or above
// PodCertificateGARelease, which serves both resources under v1, and no node
// pool below it, whose kubelet would serve projection only behind the beta
// gate. The pools are read rather than assumed because GKE lets them trail the
// control plane.
func (c Cluster) ServesPodCertificatesAsGA() bool {
	return c.PodCertificateGA() && !c.PoolReplacement().Needed
}

// SubstrateReady reports whether the cluster can run Substrate: a supported
// release that either serves the PodCertificate APIs as GA throughout or has
// the beta APIs enabled. The release half is never optional — a 1.35 cluster
// with the beta APIs is not ready, because the release itself is outside the
// supported set.
func (c Cluster) SubstrateReady() bool {
	return c.SupportedRelease() && (c.ServesPodCertificatesAsGA() || !c.MissingBetaAPIs())
}

// atLeastMinor reports whether a GKE version names a Kubernetes release at or
// after r. Only the leading two segments are read, since a GKE
// version carries a patch and a build suffix ("1.37.1-gke.1163012") and the
// distinction here is a minor-release one.
//
// ok is false for anything that does not start with two numbers, leaving the
// caller to decide what an unreadable version means; there is no answer that is
// safe in both directions.
func atLeastMinor(version string, r Release) (atLeast, ok bool) {
	majorPart, rest, found := strings.Cut(version, ".")
	if !found {
		return false, false
	}
	minorPart, _, _ := strings.Cut(rest, ".")
	gotMajor, err := strconv.Atoi(majorPart)
	if err != nil {
		return false, false
	}
	gotMinor, err := strconv.Atoi(minorPart)
	if err != nil {
		return false, false
	}
	if gotMajor != r.Major {
		return gotMajor > r.Major, true
	}
	return gotMinor >= r.Minor, true
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
		// Between them these cover every verdict the cluster screen can reach,
		// so a --dry-run walkthrough shows the whole story without a GCP
		// project. Two lack the beta APIs and need them, and land on the two
		// branches of the confirm panel — too old to enable, and enable and
		// replace pools; substrate-ga lacks them and needs none. The last two
		// names are also cues for CheckInstalled's sim, which supplies the
		// installed and partial states of the reinstall guard.
		return []Cluster{
			{Name: "substrate-poc", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.36.4-gke.1247000", NodeCount: 2, BetaAPIs: RequiredBetaAPIs, KVMReady: true},
			// Below the supported floor, and far enough below it that GKE would
			// refuse the enablement outright: the one cluster here that has to
			// be upgraded or replaced rather than repaired.
			{Name: "legacy-prod", Location: "us-central1", Status: "RUNNING",
				MasterVersion: "1.33.2-gke.100", NodeCount: 12},
			// Supported, but too old for the projection to be GA: the repair
			// works and costs replacing the pool behind all 6 nodes.
			{Name: "ml-staging", Location: "us-central1", Status: "RUNNING",
				MasterVersion: "1.36.4-gke.1247000", NodeCount: 6,
				PoolVersions: []PoolVersion{{"default-pool", "1.36.4-gke.1247000"}, {"gpu-pool", "1.36.4-gke.1247000"}}},
			// No beta APIs, and none needed: control plane and pool are both on
			// 1.37, which serves the APIs as GA. Ready, though bootstrap will
			// still turn the beta APIs on, so provision says so.
			{Name: "substrate-ga", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.37.1-gke.1000000", NodeCount: 2,
				PoolVersions: []PoolVersion{{"default-pool", "1.37.1-gke.1000000"}}},
			{Name: "substrate-installed", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.36.4-gke.1247000", NodeCount: 3, BetaAPIs: RequiredBetaAPIs, KVMReady: true},
			{Name: "substrate-partial", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.36.4-gke.1247000", NodeCount: 1, BetaAPIs: RequiredBetaAPIs},
		}, nil
	}
	out, err := c.run(ctx, "container", "clusters", "list", "--project="+projectID, "--format=json")
	if err != nil {
		return nil, err
	}
	return ParseClusters(out)
}

type rawNodePool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Config  struct {
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

func poolVersions(pools []rawNodePool) []PoolVersion {
	var out []PoolVersion
	for _, p := range pools {
		out = append(out, PoolVersion{p.Name, p.Version})
	}
	return out
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
		NodePools []rawNodePool `json:"nodePools"`
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
			PoolVersions:  poolVersions(r.NodePools),
		})
	}
	return clusters, nil
}

// ChannelVersions lists, per release channel (lower-cased: "rapid",
// "regular", …), the cluster versions GKE will create in location. Which
// versions a channel carries changes every few weeks, so the installer asks
// rather than encoding it: today 1.37 is in Rapid alone, and in a couple of
// months Regular will carry it too.
func (c *Client) ChannelVersions(ctx context.Context, projectID, location string) (map[string][]string, error) {
	if c.DryRun {
		return map[string][]string{
			"rapid":    {"1.37.0-gke.3503000", "1.36.4-gke.1495000"},
			"regular":  {"1.36.4-gke.1391000", "1.35.8-gke.1225000"},
			"stable":   {"1.35.6-gke.1250001"},
			"extended": {"1.36.4-gke.1391000", "1.35.8-gke.1225000"},
		}, nil
	}
	out, err := c.run(ctx, "container", "get-server-config", "--project="+projectID, "--location="+location, "--format=json")
	if err != nil {
		return nil, err
	}
	return ParseChannelVersions(out)
}

// ParseChannelVersions decodes the channels of `gcloud container
// get-server-config --format=json`.
func ParseChannelVersions(data []byte) (map[string][]string, error) {
	var raw struct {
		Channels []struct {
			Channel       string   `json:"channel"`
			ValidVersions []string `json:"validVersions"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing server config: %w", err)
	}
	out := make(map[string][]string, len(raw.Channels))
	for _, ch := range raw.Channels {
		out[strings.ToLower(ch.Channel)] = ch.ValidVersions
	}
	return out, nil
}

// ChannelCarries reports whether a channel's versions include want, the way
// GKE reads a requested version: a bare minor ("1.37") or a minor and patch
// ("1.37.0") names the newest build under it, and a full version
// ("1.37.0-gke.3503000") names itself.
func ChannelCarries(versions []string, want string) bool {
	for _, v := range versions {
		if v == want || strings.HasPrefix(v, want+".") || strings.HasPrefix(v, want+"-") {
			return true
		}
	}
	return false
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
