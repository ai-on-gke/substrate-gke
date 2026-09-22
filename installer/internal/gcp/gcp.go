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

// RequiredBetaAPIs are the Kubernetes beta APIs Substrate's podcertificate
// controller depends on. They are required on every release the installer
// supports, including those that also serve the same APIs as GA: upstream's
// controllers are built against the v1beta1 types, so the beta group has to be
// served whatever the cluster's release offers alongside it.
//
// GKE serves them only when they are listed in the cluster's enableK8sBetaApis,
// which is off by default. That is enough to make an otherwise healthy cluster
// unable to run Substrate.
//
// The beta APIs are deprecated from 1.37 and removed in 1.40, at which point
// upstream must move to certificates.k8s.io/v1 and this list goes away.
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
const (
	MinSupportedMajor   = 1
	MinSupportedMinor   = 36
	MinSupportedVersion = "1.36"
)

// The first Kubernetes release serving the PodCertificate APIs as GA.
//
// This does not decide whether a cluster can run Substrate — upstream needs the
// beta APIs either way, so SubstrateReady ignores it. What it decides is how a
// cluster that lacks them is repaired, which genuinely differs across this line:
//
//   - At or above it, the projection is GA, so the kubelet implements it on
//     every node. Enabling the beta APIs on the running cluster is the whole fix.
//   - Below it, the projection is itself gated, and a node only picks it up if
//     it was created after the APIs were enabled. Existing nodes keep failing to
//     mount with "unimplemented" until they are recycled.
//
// PodCertificateGAVersion is a bare minor, which GKE resolves to the newest
// patch it serves — there is no patch number here to go stale.
const (
	PodCertificateGAMajor   = 1
	PodCertificateGAMinor   = 37
	PodCertificateGAVersion = "1.37"
)

// The first release carrying both beta APIs — the technical floor the comment
// above distinguishes from the supported one, and the only reason to care about
// it is what the user is told. At or above this line GKE accepts the
// enablement, so an unsupported cluster is unsupported and nothing more. Below
// it GKE also rejects the request outright, which is worth quoting back rather
// than letting someone discover it by trying.
const (
	BetaAPIsExistMajor   = 1
	BetaAPIsExistMinor   = 35
	BetaAPIsExistVersion = "1.35"
)

// Cluster is one GKE cluster as listed by gcloud.
type Cluster struct {
	Name          string
	Location      string
	Status        string
	MasterVersion string
	NodeCount     int
	BetaAPIs      []string
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
	atLeast, ok := atLeastMinor(c.MasterVersion, MinSupportedMajor, MinSupportedMinor)
	return !ok || atLeast
}

// BetaAPIsAvailable reports whether this cluster's release carries the beta
// APIs at all, and so whether asking GKE to enable them would be accepted. It
// is not a readiness question — every release this returns true for can still
// be below the supported floor. An unreadable version reports true, matching
// SupportedRelease: neither guess should invent an error GKE never returned.
func (c Cluster) BetaAPIsAvailable() bool {
	atLeast, ok := atLeastMinor(c.MasterVersion, BetaAPIsExistMajor, BetaAPIsExistMinor)
	return !ok || atLeast
}

// PodCertificateGA reports whether this cluster's release serves the
// PodCertificate APIs as GA. It says nothing about readiness — see the constants
// above for what it is actually for, which is choosing the repair instructions.
//
// A version that cannot be read reports false, which buys the wordier of the two
// remedies. Telling someone to recycle nodes they did not need to recycle wastes
// their time; omitting it leaves them staring at a pod that will never mount.
func (c Cluster) PodCertificateGA() bool {
	atLeast, ok := atLeastMinor(c.MasterVersion, PodCertificateGAMajor, PodCertificateGAMinor)
	return ok && atLeast
}

// SubstrateReady reports whether the cluster can run Substrate: a supported
// release, with the beta APIs enabled on it. Both halves are load-bearing and
// neither substitutes for the other — a 1.37 cluster without the beta APIs is
// not ready, because upstream's controllers watch the v1beta1 types whatever
// else the release serves; and a 1.35 cluster with them is not ready either,
// because the release itself is outside the supported set.
func (c Cluster) SubstrateReady() bool {
	if !c.SupportedRelease() {
		return false
	}
	for _, want := range RequiredBetaAPIs {
		if !slices.Contains(c.BetaAPIs, want) {
			return false
		}
	}
	return true
}

// atLeastMinor reports whether a GKE version names a Kubernetes release at or
// after major.minor. Only the leading two segments are read, since a GKE
// version carries a patch and a build suffix ("1.37.1-gke.1163012") and the
// distinction here is a minor-release one.
//
// ok is false for anything that does not start with two numbers, leaving the
// caller to decide what an unreadable version means; there is no answer that is
// safe in both directions.
func atLeastMinor(version string, major, minor int) (atLeast, ok bool) {
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
	if gotMajor != major {
		return gotMajor > major, true
	}
	return gotMinor >= minor, true
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
		// project. The three that lack the beta APIs each land on a different
		// branch of the confirm panel — too old to enable, enable and recycle,
		// enable and go — and the last two names are also cues for
		// CheckInstalled's sim, which supplies the installed and partial
		// states of the reinstall guard.
		return []Cluster{
			{Name: "substrate-poc", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.36.4-gke.1247000", NodeCount: 2, BetaAPIs: RequiredBetaAPIs},
			// Below the supported floor, and far enough below it that GKE would
			// refuse the enablement outright: the one cluster here that has to
			// be upgraded or replaced rather than repaired.
			{Name: "legacy-prod", Location: "us-central1", Status: "RUNNING",
				MasterVersion: "1.33.2-gke.100", NodeCount: 12},
			// Supported, but too old for the projection to be GA: the repair
			// works and costs a recycle of all 6 nodes.
			{Name: "ml-staging", Location: "us-central1", Status: "RUNNING",
				MasterVersion: "1.36.4-gke.1247000", NodeCount: 6},
			// Missing the beta APIs on a release that also serves them as GA:
			// not ready either, but repaired by enabling them on the running
			// cluster, with none of the node recycling ml-staging needs.
			{Name: "substrate-ga", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.37.1-gke.1000000", NodeCount: 2},
			{Name: "substrate-installed", Location: "us-west1-c", Status: "RUNNING",
				MasterVersion: "1.36.4-gke.1247000", NodeCount: 3, BetaAPIs: RequiredBetaAPIs},
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
