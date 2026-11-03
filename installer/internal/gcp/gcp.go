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
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
)

// RequiredBetaAPIs are the Kubernetes beta PodCertificate APIs that setup-gcp's
// bootstrap requests on every cluster it creates and turns on for an existing
// one that lacks them. Substrate does not need them on a supported cluster:
// 1.37 serves both resources as certificates.k8s.io/v1, which every Substrate
// component prefers from v0.4.0, and a 1.37 kubelet serves pod certificate
// projection without the beta gate.
//
// Turning them on is a control-plane update of about ten minutes, so the
// provision screen says when bootstrap is about to do it (MissingBetaAPIs).
//
// GKE serves them only when they are listed in the cluster's enableK8sBetaApis,
// which is off by default. They are deprecated from 1.37 and removed in 1.40.
var RequiredBetaAPIs = []string{
	"certificates.k8s.io/v1beta1/podcertificaterequests",
	"certificates.k8s.io/v1beta1/clustertrustbundles",
}

// The oldest release Agent Substrate is supported on, for the control plane
// and for every node pool. A cluster with either below it is rejected; the
// remedy is an upgrade.
//
// https://docs.cloud.google.com/kubernetes-engine/ai-ml/install-overview-substrate#cluster-requirements
//
// Node pools are checked because GKE lets them trail the control plane, and a
// kubelet below 1.37 serves pod certificate projection only behind the beta
// gate: Substrate's pods on such a node fail to mount with "unimplemented".
var MinSupportedRelease = Release{1, 37}

// Release is a Kubernetes minor release, the granularity every line above is
// drawn at. One value holds both the number the comparisons read and the text
// the user is shown, so the two cannot drift apart.
type Release struct{ Major, Minor int }

// String is the bare minor ("1.37"). GKE accepts it wherever it takes a
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
	// Network and Subnetwork are the cluster's networkConfig paths, as gcloud
	// lists them ("projects/P/global/networks/N",
	// "projects/P/regions/R/subnetworks/S"), and DataplaneV2 whether its
	// datapath is ADVANCED_DATAPATH. Empty and false when gcloud listed no
	// networkConfig. The installer hands them to bootstrap for an existing
	// cluster (see state.Setup.BootstrapNetwork).
	Network     string
	Subnetwork  string
	DataplaneV2 bool
	// FilestoreCSIAddon reports whether GKE's managed Filestore CSI driver is
	// enabled. The optional Filestore step turns it off before installing
	// Substrate's driver, so the installer warns before that happens to a
	// cluster whose workloads may be using it.
	FilestoreCSIAddon bool
	// PoolVersions is each node pool's name and Kubernetes version, in
	// gcloud's order. Empty when gcloud listed no pools (Autopilot) or for a
	// fixture that did not bother.
	PoolVersions []PoolVersion
}

// NetworkName and SubnetworkName are the last segments of the cluster's
// network paths: the short names setup-gcp's NETWORK and SUBNETWORK take.
func (c Cluster) NetworkName() string    { return lastSegment(c.Network) }
func (c Cluster) SubnetworkName() string { return lastSegment(c.Subnetwork) }

func lastSegment(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// PoolVersion is one node pool's name and the version its kubelets run.
type PoolVersion struct{ Name, Version string }

// SupportedRelease reports whether the control plane's release is at or above
// MinSupportedRelease. It is half of readiness; PoolsBelowFloor is the other.
//
// A version that cannot be read reports true and is left for the install to
// judge: being waved through onto an unsupported release costs a failed
// install the user can retry; being told a working cluster is too old costs a
// cluster rebuild.
func (c Cluster) SupportedRelease() bool {
	return releaseSupported(c.MasterVersion)
}

// PoolsBelowFloor names the node pools whose version is below
// MinSupportedRelease, in gcloud's order. A pool whose version cannot be read
// is not named, on the same reasoning as SupportedRelease.
func (c Cluster) PoolsBelowFloor() []string {
	var names []string
	for _, p := range c.PoolVersions {
		if !releaseSupported(p.Version) {
			names = append(names, p.Name)
		}
	}
	return names
}

func releaseSupported(version string) bool {
	atLeast, ok := atLeastMinor(version, MinSupportedRelease)
	return !ok || atLeast
}

// MissingBetaAPIs reports whether any of RequiredBetaAPIs is not enabled on
// the cluster, which is exactly when setup-gcp's bootstrap will run its
// control-plane update to turn them on. It has no bearing on readiness.
func (c Cluster) MissingBetaAPIs() bool {
	for _, want := range RequiredBetaAPIs {
		if !slices.Contains(c.BetaAPIs, want) {
			return true
		}
	}
	return false
}

// SubstrateReady reports whether the cluster can run Substrate: its control
// plane and every node pool are at or above MinSupportedRelease.
func (c Cluster) SubstrateReady() bool {
	return c.SupportedRelease() && len(c.PoolsBelowFloor()) == 0
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
	// billingBase and serviceUsageBase override the Cloud Billing and
	// Service Usage endpoints the same way.
	billingBase      string
	serviceUsageBase string
	// token overrides how the REST probes obtain an access token; nil
	// means asking gcloud for the application-default one.
	token func(ctx context.Context) (string, error)
	// onTokenWait, when set, is called each time a caller joins a token
	// fetch already under way. Tests use it to know every caller is
	// queued before releasing the fetch, instead of sleeping.
	onTokenWait func()

	// tokenMu guards the access token accessToken caches, so one submit's
	// REST probes, run concurrently, share a single gcloud spawn.
	// tokenFetch is the fetch under way, if any; it is never run with
	// tokenMu held. rejectedToken is the last token a 401 rejected, until
	// the next ResetToken; see rejectToken.
	// tokenGen counts ResetToken calls; a fetch records the generation it
	// started in, so a caller from a later action can tell it adopted an
	// older action's fetch (see accessToken).
	tokenMu       sync.Mutex
	cachedToken   string
	tokenExpiry   time.Time
	tokenFetch    *tokenFetch
	rejectedToken string
	tokenGen      uint64

	// The Client's lifetime: base is cancelled by Close, and every gcloud
	// run and REST request derives from it, so quitting kills gcloud
	// children rather than leaving them orphaned. lifeMu guards closed
	// and orders inflight.Add before Close's Wait.
	baseOnce   sync.Once
	baseCtx    context.Context
	baseCancel context.CancelFunc
	lifeMu     sync.Mutex
	closed     bool
	inflight   sync.WaitGroup
}

const cmdTimeout = 60 * time.Second

// closeWait bounds how long Close waits for cancelled work to exit: a
// killed gcloud is gone in milliseconds, but Close runs on the way out and
// must never hang the exit.
const closeWait = 2 * time.Second

// errClosed is what work started after Close returns.
var errClosed = errors.New("gcp client closed")

// base is the Client's lifetime context, created on first use so the zero
// Client (as tests build it) works.
func (c *Client) base() context.Context {
	c.baseOnce.Do(func() {
		c.baseCtx, c.baseCancel = context.WithCancel(context.Background())
	})
	return c.baseCtx
}

// begin registers one piece of work against the Client's lifetime and
// reports false once the Client is closed. Every true must be paired with
// a call to end.
func (c *Client) begin() bool {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.closed {
		return false
	}
	c.inflight.Add(1)
	return true
}

func (c *Client) end() { c.inflight.Done() }

// bound derives ctx so it also ends when the Client is closed. The returned
// stop must be called once the work is done.
func (c *Client) bound(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.base(), cancel)
	return ctx, func() { stop(); cancel() }
}

// Close cancels every gcloud run, token fetch and REST request still in
// flight and waits (up to closeWait) for them to exit, so none outlives the
// installer. Work started afterwards fails with errClosed. Call it once the
// TUI has quit; it is safe to call more than once.
func (c *Client) Close() {
	c.lifeMu.Lock()
	c.closed = true
	c.lifeMu.Unlock()
	c.base()
	c.baseCancel()

	done := make(chan struct{})
	go func() { c.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeWait):
	}
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	if !c.begin() {
		return nil, errClosed
	}
	defer c.end()
	ctx, stop := c.bound(ctx)
	defer stop()
	// One gcloud at a time, installer-wide (see execx.LockGcloud): this run
	// queues behind the Runner's commands and the other runs, the token
	// fetch included. The wait is bounded by the caller's ctx and Close, not
	// by cmdTimeout, which times gcloud itself.
	release, err := execx.LockGcloud(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcloud %s: %w", strings.Join(args, " "), err)
	}
	defer release()
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
		// project. Two are not ready and land on the two branches of the
		// rejection panel — a control plane below the floor, and node pools
		// below it. substrate-ga lacks the beta APIs, so provision says
		// bootstrap will turn them on. The last two names are also cues for
		// CheckInstalled's sim, which supplies the installed and partial
		// states of the reinstall guard. substrate-ga has the managed
		// Filestore driver on, so its Filestore step shows that warning.
		return []Cluster{
			{Name: "substrate-poc", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.37.1-gke.1000000", NodeCount: 2, BetaAPIs: RequiredBetaAPIs, KVMReady: true},
			// Control plane below the floor.
			{Name: "legacy-prod", Location: "us-central1", Status: "RUNNING",
				MasterVersion: "1.33.2-gke.100", NodeCount: 12},
			// Supported control plane, but both node pools are below the floor.
			{Name: "ml-staging", Location: "us-central1", Status: "RUNNING",
				MasterVersion: "1.37.1-gke.1000000", NodeCount: 6,
				PoolVersions: []PoolVersion{{"default-pool", "1.36.4-gke.1247000"}, {"gpu-pool", "1.36.4-gke.1247000"}}},
			// Control plane and pool both on 1.37, no beta APIs. Ready, though
			// bootstrap will still turn the beta APIs on, so provision says so.
			{Name: "substrate-ga", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.37.1-gke.1000000", NodeCount: 2,
				PoolVersions: []PoolVersion{{"default-pool", "1.37.1-gke.1000000"}}, FilestoreCSIAddon: true},
			{Name: "substrate-installed", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.37.1-gke.1000000", NodeCount: 3, BetaAPIs: RequiredBetaAPIs, KVMReady: true},
			{Name: "substrate-partial", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.37.1-gke.1000000", NodeCount: 1, BetaAPIs: RequiredBetaAPIs},
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
		NodePools     []rawNodePool `json:"nodePools"`
		NetworkConfig struct {
			Network          string `json:"network"`
			Subnetwork       string `json:"subnetwork"`
			DatapathProvider string `json:"datapathProvider"`
		} `json:"networkConfig"`
		AddonsConfig struct {
			GcpFilestoreCsiDriverConfig struct {
				Enabled bool `json:"enabled"`
			} `json:"gcpFilestoreCsiDriverConfig"`
		} `json:"addonsConfig"`
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
			Network:       r.NetworkConfig.Network,
			Subnetwork:    r.NetworkConfig.Subnetwork,
			DataplaneV2:   r.NetworkConfig.DatapathProvider == "ADVANCED_DATAPATH",

			FilestoreCSIAddon: r.AddonsConfig.GcpFilestoreCsiDriverConfig.Enabled,
		})
	}
	return clusters, nil
}

// ChannelVersions lists, per release channel (lower-cased: "rapid",
// "regular", …), the cluster versions GKE will create in location. Which
// versions a channel carries changes every few weeks, so the installer asks
// rather than encoding it.
func (c *Client) ChannelVersions(ctx context.Context, projectID, location string) (map[string][]string, error) {
	if c.DryRun {
		return map[string][]string{
			"rapid":    {"1.37.0-gke.3503000", "1.36.4-gke.1495000"},
			"regular":  {"1.37.0-gke.3503000", "1.36.4-gke.1391000", "1.35.8-gke.1225000"},
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
