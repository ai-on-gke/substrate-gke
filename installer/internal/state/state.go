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

// Package state holds the wizard's linear step machine and the user's
// accumulated setup choices. It is the Go port of the onboarding TUI's
// engine/state_machine.py and UserSetupState.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
)

// Step identifies one screen of the wizard.
type Step int

const (
	Welcome Step = iota
	CheckSetup
	Images
	Project
	Cluster
	Provision
	IngressClients
	ControlPlane
	Sandbox
	FilestoreCSI
	Autoscaling
	Demo
	Complete

	// The upgrade track's own steps. They come after Complete so the install
	// steps keep their numbers; their position on screen comes from the
	// Machine's order, not from these values.
	UpgradeSource
	UpgradePlan

	// PluginImages was added after the upgrade steps for the same reason:
	// its value leaves every other step's number alone.
	PluginImages
)

// Order is the linear flow of the wizard.
//
// Images comes before Project because it decides what the rest of the run
// needs: a pre-built install pushes nothing, so it is never asked for an image
// registry. It comes after CheckSetup because it is the first step to reach
// the network — it resolves a revision against a git remote — and the doctor
// is what reports a missing git or an unreachable network with a fix to paste.
//
// PluginImages follows Images: the GKE plugins are published apart from
// Substrate, with their own commits and versions, so their images get a step
// of their own. It is not in UpgradeOrder, because nothing deploys a plugin
// yet and so there is nothing to upgrade.
//
// IngressClients comes right before ControlPlane, which hands its SPIFFE IDs
// to ate-setup as the ingress router's allowlist.
//
// Sandbox comes after Autoscaling: the steps before it finish the cluster,
// control-plane, storage, and node-pool setup, then it chooses the actor
// sandbox runtime (staging the micro-VM assets and applying the microvm
// SandboxConfig when Micro-VM is chosen).
//
// Order has no demo step: `make run` installs Substrate for your own
// workloads. DemoOrder, for `make run-demo`, adds Demo after Sandbox, which
// deploys the counter demo matching the chosen runtime.
var Order = []Step{
	Welcome, CheckSetup, Images, PluginImages, Project, Cluster, Provision,
	IngressClients, ControlPlane, FilestoreCSI, Autoscaling, Sandbox, Complete,
}

// DemoOrder is the install flow of a demo run (--demo, `make run-demo`).
var DemoOrder = []Step{
	Welcome, CheckSetup, Images, PluginImages, Project, Cluster, Provision,
	IngressClients, ControlPlane, FilestoreCSI, Autoscaling, Sandbox, Demo, Complete,
}

// InstallOrder is the install flow for a run, DemoOrder for a demo run and
// Order otherwise.
func InstallOrder(demo bool) []Step {
	if demo {
		return DemoOrder
	}
	return Order
}

// UpgradeOrder is the flow for upgrading a cluster this installer set up. It
// changes nothing on the cluster: it reads the installed version off the
// cluster, asks where the new version comes from with the same images step an
// install uses, fetches the two source trees upstream's runbook runs from, and
// sets up ~/ate-upgrade the way the runbook does. The upgrade itself is
// upstream's docs/upgrade.md.
var UpgradeOrder = []Step{
	Welcome, CheckSetup, UpgradeSource, Images, UpgradePlan, Complete,
}

// Title returns the heading shown for a step.
func (s Step) Title() string {
	switch s {
	case Welcome:
		return "Welcome"
	case Images:
		return "Choose your images"
	case PluginImages:
		return "Choose your GKE plugin images"
	case CheckSetup:
		return "Check your setup"
	case Project:
		return "Choose your GCP project"
	case Cluster:
		return "Connect your cluster"
	case Provision:
		return "Provision GCP resources"
	case IngressClients:
		return "Allow ingress clients"
	case ControlPlane:
		return "Turn on Substrate"
	case Sandbox:
		return "Choose your sandbox runtime"
	case FilestoreCSI:
		return "Install Filestore CSI driver"
	case Autoscaling:
		return "Configure autoscaling"
	case Demo:
		return "Deploy a demo workload"
	case Complete:
		return "Complete"
	case UpgradeSource:
		return "Choose the installed cluster"
	case UpgradePlan:
		return "Prepare the upgrade"
	}
	return "?"
}

// Machine is the linear step machine with back-navigation history.
type Machine struct {
	order   []Step
	idx     int
	history []int
}

// NewMachine starts at Welcome on the install flow.
func NewMachine() *Machine { return &Machine{order: Order} }

// SetOrder switches the flow, which the welcome screen does when the user
// picks the upgrade track. Both flows start at Welcome, so switching there
// keeps the current step in place.
func (m *Machine) SetOrder(order []Step) {
	m.order = order
	m.idx = 0
	m.history = nil
}

// Order returns the steps of the active flow.
func (m *Machine) Order() []Step { return m.order }

// Position returns the 1-based position of s among the numbered steps of the
// active flow, and false for Welcome, Complete, and steps not in the flow.
func (m *Machine) Position(s Step) (int, bool) {
	if s == Welcome || s == Complete {
		return 0, false
	}
	for i, o := range m.order {
		if o == s {
			return i, true
		}
	}
	return 0, false
}

// NumberedSteps is how many numbered steps the active flow has.
func (m *Machine) NumberedSteps() int { return len(m.order) - 2 }

// Current returns the active step.
func (m *Machine) Current() Step { return m.order[m.idx] }

// Next advances to the following step and records history for Prev.
func (m *Machine) Next() Step {
	if m.idx < len(m.order)-1 {
		m.history = append(m.history, m.idx)
		m.idx++
	}
	return m.Current()
}

// Prev rolls back to the most recently visited step. The second return is
// false when there is nothing to go back to.
func (m *Machine) Prev() (Step, bool) {
	if len(m.history) == 0 {
		return m.Current(), false
	}
	m.idx = m.history[len(m.history)-1]
	m.history = m.history[:len(m.history)-1]
	return m.Current(), true
}

// Restart returns to Welcome and clears history.
func (m *Machine) Restart() Step {
	m.idx = 0
	m.history = nil
	return m.Current()
}

// Track selects how much the wizard asks.
const (
	TrackQuickstart = "quickstart"
	TrackAdvanced   = "advanced"
)

// Sandbox classes an install can set up. These are the installer's own names
// for the choice; upstream spells them SANDBOX_CLASS_GVISOR and
// SANDBOX_CLASS_MICROVM inside the resources it applies.
//
// gVisor needs nothing from the wizard: ate-setup applies the cluster-wide
// gvisor-default SandboxConfig unconditionally as part of the control-plane
// step. Micro-VM is opt-in upstream and stays opt-in here — its sandbox
// binaries are fetched at runtime from GCS (the snapshot bucket up to Substrate
// v0.5.0, BUCKET_NAME-assets after), so they have to be built and staged
// there, and a SandboxConfig naming them applied, before any micro-VM actor
// can boot.
const (
	SandboxGVisor  = "gvisor"
	SandboxMicroVM = "microvm"
)

// DefaultClusterVersion and DefaultReleaseChannel are what a new cluster is
// created with: 1.37 on the Rapid channel.
//
// 1.37 serves the PodCertificate APIs as v1, which Substrate uses from
// v0.4.0, and its kubelets serve pod certificate projection without the beta
// gate, so a cluster made this way needs no repair of any kind. 1.37 is also
// gcp.MinSupportedRelease, so the version can be raised but not lowered. The
// channel is a choice: the advanced track can pick Regular, which also
// carries 1.37.
//
// The version must be set explicitly. Leaving it unset gets the channel's
// default version, which for Regular is below gcp.MinSupportedRelease.
const (
	DefaultClusterVersion = "1.37"
	DefaultReleaseChannel = "rapid"
)

// Setup accumulates everything the user chose plus values resolved from GCP.
type Setup struct {
	Track string

	// DemoMode is a demo run (--demo, `make run-demo`): the flow ends with
	// the demo step, and the ingress allowlist is the demo drivers'
	// identities.
	DemoMode bool
	// IngressAllowedSPIFFEIDs is the ingress router's client allowlist,
	// passed to ate-setup as ATE_INGRESS_ALLOWED_SPIFFE_IDS. The
	// IngressClients step sets it.
	IngressAllowedSPIFFEIDs []string

	// Upgrade selects the upgrade track: the cluster is already installed,
	// and the run prepares upstream's rolling upgrade instead of installing.
	Upgrade bool
	// InstalledCommit and InstalledVersion describe the Substrate a cluster
	// runs today, as its API server reports them; an upgrade fetches that
	// tree for rollback.
	InstalledCommit  string
	InstalledVersion string

	// ImageRepo and ImageTag name an already-published set of Substrate
	// images for ate-setup to install. When ImageRepo is empty the images are
	// built from source with ko instead, which is what KoDockerRepo is for.
	ImageRepo string
	ImageTag  string

	// PluginSourceRepo, PluginImageRepo, PluginImageTag and PluginCommit name
	// the GKE plugin images: their repository, the registry and tag they were
	// published under, and the commit they were built from. Plugins version
	// separately from Substrate, so these are independent of ImageRepo and
	// ImageTag. All are empty when the plugin step was skipped. Nothing
	// deploys the plugins yet; see snapshot.PluginSourceOrg.
	PluginSourceRepo string
	PluginImageRepo  string
	PluginImageTag   string
	PluginCommit     string

	ProjectID     string
	ProjectNumber string

	// Zone is the cluster location (a GKE zone or region); Region is derived
	// from it for setup-gcp's GCE_REGION.
	Zone string

	Network     string
	Subnetwork  string
	MachineType string

	ClusterName     string
	ClusterIsNew    bool
	ClusterKVMReady bool

	// ClusterNetwork, ClusterSubnetwork and ClusterDataplaneV2 describe the
	// existing cluster the run connects to, as gcloud listed it. Bootstrap is
	// given these rather than Network and Subnetwork, which are the user's
	// choices for a cluster the run creates: it compares an existing
	// cluster's network, subnetwork and datapath with what it was given and
	// warns on each difference, and the defaults match few real clusters.
	// Kept apart so backing out of an existing cluster cannot leak its
	// network into the create path.
	ClusterNetwork     string
	ClusterSubnetwork  string
	ClusterDataplaneV2 bool

	// ClusterFilestoreAddon records that the existing cluster had GKE's
	// managed Filestore CSI driver on. Bootstrap leaves it alone; the
	// optional Filestore step turns it off before installing Substrate's
	// driver, so that step warns about it.
	ClusterFilestoreAddon bool

	// EnableBetaAPIs records that the chosen existing cluster lacks the beta
	// PodCertificate APIs, so provision is about to turn them on: a
	// control-plane update of roughly ten minutes rather than the quick
	// top-up an existing cluster normally gets. Substrate does not need them
	// on a supported cluster, but bootstrap turns them on for every cluster,
	// so the provision screen says so.
	EnableBetaAPIs bool

	// ClusterVersion and ReleaseChannel shape a cluster the install creates
	// and are ignored for one it connects to: bootstrap reads them only on the
	// creation path. A bare minor is fine for the version — GKE resolves it to
	// the newest patch the channel serves, so there is no patch number here to
	// go stale.
	ClusterVersion string
	ReleaseChannel string

	BucketName                 string
	KoDockerRepo               string
	ArtifactRegistryRepository string

	FilestoreCSIDeployed bool

	// SandboxClass is the runtime this install sets up: SandboxGVisor or
	// SandboxMicroVM. MicroVMDeployed records that the micro-VM assets were
	// staged and the SandboxConfig applied, which the completion screen uses
	// to print the right next steps.
	SandboxClass    string
	MicroVMDeployed bool

	NodePool         string
	AutoscaleEnabled bool
	AutoscaleMin     int
	AutoscaleMax     int

	DemoDeployed bool
	Verified     bool
}

// NewSetup returns a Setup with the same defaults the upstream docs use.
func NewSetup() *Setup {
	return &Setup{
		Track:          TrackQuickstart,
		Zone:           "us-west1-c",
		Network:        "default",
		Subnetwork:     "default",
		MachineType:    "c3-standard-4",
		ClusterName:    "substrate-test",
		ClusterVersion: DefaultClusterVersion,
		ReleaseChannel: DefaultReleaseChannel,
		NodePool:       "substrate-node-pool",
		SandboxClass:   SandboxGVisor,
		AutoscaleMin:   1,
		AutoscaleMax:   5,
	}
}

// MicroVM reports whether this install sets up the micro-VM sandbox class.
// An empty SandboxClass reads as gVisor, so a Setup built without NewSetup —
// as the tests and the upgrade track do — keeps the behaviour it had before
// the choice existed.
func (s *Setup) MicroVM() bool { return s.SandboxClass == SandboxMicroVM }

// MicroVMActive reports whether the micro-VM sandbox is actually in effect:
// chosen and its assets staged. MicroVM is only the choice — the staging step
// can still be skipped or fail — so anything that acts on the outcome (the
// demo variant, the next steps) must use this instead. A nil Setup reads as
// gVisor.
func (s *Setup) MicroVMActive() bool {
	return s != nil && s.MicroVM() && s.MicroVMDeployed
}

// BootstrapNetwork returns the network, subnetwork and Dataplane V2 setting to
// hand setup-gcp's bootstrap. For a cluster the run creates, the user's
// choices, with Dataplane V2 on as upstream has always defaulted it. For an
// existing cluster, the cluster's own, so bootstrap's comparison usually finds
// no difference to warn about. (Not for Shared VPC: setup-gcp expects the
// network in this project, so a host-project network still gets a warning.)
// A network gcloud did not list falls back to the user's, which bootstrap
// then does not compare, since it skips the check when the cluster reports
// none.
func (s *Setup) BootstrapNetwork() (network, subnetwork string, dataplaneV2 bool) {
	if s.ClusterIsNew {
		return s.Network, s.Subnetwork, true
	}
	network, subnetwork = s.Network, s.Subnetwork
	if s.ClusterNetwork != "" {
		network = s.ClusterNetwork
	}
	if s.ClusterSubnetwork != "" {
		subnetwork = s.ClusterSubnetwork
	}
	return network, subnetwork, s.ClusterDataplaneV2
}

// Prebuilt reports whether the install pulls published images rather than
// building them.
func (s *Setup) Prebuilt() bool { return s.ImageRepo != "" }

// GKEPlugins reports whether GKE plugin images were given.
func (s *Setup) GKEPlugins() bool { return s.PluginImageRepo != "" }

// CreatesArtifactRepository reports whether this install provisions its build repository.
func (s *Setup) CreatesArtifactRepository() bool {
	return !s.Prebuilt() && s.KoDockerRepo == ""
}

// BuildRepository resolves the image destination for this install.
func (s *Setup) BuildRepository() string {
	if s.KoDockerRepo != "" {
		return s.KoDockerRepo
	}
	return s.Region() + "-docker.pkg.dev/" + s.ProjectID + "/" + s.RepositoryName()
}

// ImageSummary describes where this install's images come from.
func (s *Setup) ImageSummary() string {
	if s.Prebuilt() {
		return s.ImageRepo + ":" + s.ImageTag
	}
	return s.BuildRepository() + " (built from source)"
}

// Region derives the GCE region from Zone: a zonal location like us-west1-c
// maps to us-west1, and a regional location is returned unchanged.
func (s *Setup) Region() string {
	parts := strings.Split(s.Zone, "-")
	if len(parts) == 3 && len(parts[2]) == 1 {
		return parts[0] + "-" + parts[1]
	}
	return s.Zone
}

// SelectCluster records an existing cluster as the run's target, and
// NewCluster a cluster the run will create. Every Cluster* field is written
// by both, here and only here: a field one of them forgot would carry an
// abandoned selection's value into the create path, or a stale one into the
// next selection — the leak these fields are kept apart to prevent. A new
// field gets its line in both, next to each other.
func (s *Setup) SelectCluster(c gcp.Cluster) {
	s.ClusterName = c.Name
	s.Zone = c.Location
	s.ClusterIsNew = false
	s.ClusterKVMReady = c.KVMReady
	s.ClusterNetwork = c.NetworkName()
	s.ClusterSubnetwork = c.SubnetworkName()
	s.ClusterDataplaneV2 = c.DataplaneV2
	s.ClusterFilestoreAddon = c.FilestoreCSIAddon
	s.EnableBetaAPIs = c.MissingBetaAPIs()
}

// NewCluster records a cluster the run will create, named name, in the
// location already in Zone (see SelectCluster).
func (s *Setup) NewCluster(name string) {
	s.ClusterName = name
	s.ClusterIsNew = true
	s.ClusterKVMReady = false
	s.ClusterNetwork = ""
	s.ClusterSubnetwork = ""
	s.ClusterDataplaneV2 = false
	s.ClusterFilestoreAddon = false
	s.EnableBetaAPIs = false
}

// defaultBucketName derives the snapshot bucket name for a project, cluster,
// and location: ate-snapshots-<project_id>-<cluster>-<zone>. The cluster name
// is part of the derivation so two clusters in one project and zone never
// share a bucket: snapshot object keys carry no cluster identifier, and
// teardown deletes the whole bucket, so a shared bucket mixes clusters'
// snapshots and loses every sibling's history when one cluster is deleted.
// GCS bucket names must be 3-63 characters, start and end with an alphanumeric
// character, and contain only lowercase letters, numbers, and dashes/underscores.
// setup-gcp also creates NAME-assets for micro-VM assets, so NAME can have at
// most maxBucketNameLength characters. A longer derivation is truncated and
// suffixed with a short hash of the full name, keeping it deterministic and
// distinct per cluster.
func defaultBucketName(projectID, clusterName, zone string) string {
	name := strings.ToLower(fmt.Sprintf("ate-snapshots-%s-%s-%s", projectID, clusterName, zone))
	if len(name) > maxBucketNameLength {
		sum := sha256.Sum256([]byte(name))
		name = strings.TrimRight(name[:maxBucketNameLength-9], "-_") + "-" + hex.EncodeToString(sum[:])[:8]
	}
	return name
}

// maxBucketNameLength is the longest snapshot bucket name setup-gcp accepts:
// GCS's 63 characters less the "-assets" suffix of the asset bucket.
const maxBucketNameLength = 63 - len("-assets")

// ApplyProjectDefaults fills the values derived from the project ID, cluster
// name, and cluster location unless the user already overrode them. It runs
// once the cluster is chosen, so the cluster name is always set by then.
func (s *Setup) ApplyProjectDefaults() error {
	if s.BucketName == "" {
		s.BucketName = defaultBucketName(s.ProjectID, s.ClusterName, s.Zone)
	}
	return nil
}

var repositoryNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateRepositoryName accepts an empty name to use the default repository.
func ValidateRepositoryName(name string) error {
	if name != "" && !repositoryNameRE.MatchString(name) {
		return fmt.Errorf("repository names must be 1–63 lowercase letters, digits or hyphens, starting with a letter and ending with a letter or digit")
	}
	return nil
}

func (s *Setup) RepositoryName() string {
	if s.ArtifactRegistryRepository != "" {
		return s.ArtifactRegistryRepository
	}
	return "ate-images"
}
