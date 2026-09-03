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
	"fmt"
	"strings"
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
	ControlPlane
	FilestoreCSI
	Autoscaling
	Demo
	Complete

	// The upgrade track's own steps. They come after Complete so the install
	// steps keep their numbers; their position on screen comes from the
	// Machine's order, not from these values.
	UpgradeSource
	UpgradePlan
)

// Order is the linear flow of the wizard.
//
// Images comes before Project because it decides what the rest of the run
// needs: a pre-built install pushes nothing, so it is never asked for an image
// registry. It comes after CheckSetup because it is the first step to reach
// the network — it resolves a revision against a git remote — and the doctor
// is what reports a missing git or an unreachable network with a fix to paste.
var Order = []Step{
	Welcome, CheckSetup, Images, Project, Cluster, Provision,
	ControlPlane, FilestoreCSI, Autoscaling, Demo, Complete,
}

// UpgradeOrder is the flow for upgrading a cluster this installer set up. It
// touches nothing in GCP: it reads the cluster's install record, asks where
// the new version comes from with the same images step an install uses,
// fetches the two source trees upstream's runbook runs from, and records the
// new version. The upgrade itself is upstream's docs/upgrade.md.
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
	case CheckSetup:
		return "Check your setup"
	case Project:
		return "Choose your GCP project"
	case Cluster:
		return "Connect your cluster"
	case Provision:
		return "Provision GCP resources"
	case ControlPlane:
		return "Turn on Substrate"
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

// Number returns the 1-based wizard position for the numbered middle steps
// and false for Welcome and Complete.
func (s Step) Number() (int, bool) {
	if s == Welcome || s == Complete {
		return 0, false
	}
	return int(s), true
}

// NumberedSteps is how many numbered steps the sidebar shows.
const NumberedSteps = int(Demo)

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

// Setup accumulates everything the user chose plus values resolved from GCP.
type Setup struct {
	Track string

	// Upgrade selects the upgrade track: the cluster is already installed,
	// and the run prepares upstream's rolling upgrade instead of installing.
	Upgrade bool
	// InstalledRepo, InstalledCommit and InstalledVersion describe the
	// Substrate a cluster runs today, read off the cluster itself; an
	// upgrade fetches that tree for rollback.
	InstalledRepo    string
	InstalledCommit  string
	InstalledVersion string
	// InstalledImageRepo and InstalledImageTag are set when the installed
	// version runs published images; a build from source leaves them empty
	// and its registry in KoDockerRepo.
	InstalledImageRepo string
	InstalledImageTag  string

	// ImageRepo and ImageTag name an already-published set of Substrate
	// images for ate-setup to install. When ImageRepo is empty the images are
	// built from source with ko instead, which is what KoDockerRepo is for.
	ImageRepo string
	ImageTag  string

	ProjectID     string
	ProjectNumber string

	// Zone is the cluster location (a GKE zone or region); Region is derived
	// from it for setup-gcp's GCE_REGION.
	Zone string

	Network     string
	Subnetwork  string
	MachineType string

	ClusterName  string
	ClusterIsNew bool

	BucketName   string
	KoDockerRepo string

	FilestoreCSIDeployed bool

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
		Track:        TrackQuickstart,
		Zone:         "us-west1-c",
		Network:      "default",
		Subnetwork:   "default",
		MachineType:  "c3-standard-4",
		ClusterName:  "substrate-test",
		NodePool:     "substrate-node-pool",
		AutoscaleMin: 1,
		AutoscaleMax: 5,
	}
}

// Prebuilt reports whether the install pulls published images rather than
// building them.
func (s *Setup) Prebuilt() bool { return s.ImageRepo != "" }

// ImageSummary says where this install's images come from, for the recaps.
func (s *Setup) ImageSummary() string {
	if s.Prebuilt() {
		return s.ImageRepo + ":" + s.ImageTag
	}
	return s.KoDockerRepo + " (built from source)"
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

// defaultBucketName derives the snapshot bucket name for a project and location:
// ate-snapshots-<project_id>-<zone>.
// GCS bucket names must be 3-63 characters, start and end with an alphanumeric
// character, and contain only lowercase letters, numbers, and dashes/underscores.
func defaultBucketName(projectID, zone string) (string, error) {
	projectID = strings.ToLower(projectID)
	zone = strings.ToLower(zone)
	name := fmt.Sprintf("ate-snapshots-%s-%s", projectID, zone)
	if len(name) > 63 {
		return "", fmt.Errorf("bucket name %q exceeds 63 characters", name)
	}
	return name, nil
}

// ApplyProjectDefaults fills the values derived from the project ID and cluster
// location unless the user already overrode them.
func (s *Setup) ApplyProjectDefaults() error {
	if s.BucketName == "" {
		b, err := defaultBucketName(s.ProjectID, s.Zone)
		if err != nil {
			return err
		}
		s.BucketName = b
	}
	if s.KoDockerRepo == "" && !s.Prebuilt() {
		s.KoDockerRepo = s.DefaultKoDockerRepo()
	}
	return nil
}

// DefaultKoDockerRepo is the registry a build from source pushes to when
// none was given: one in the project itself.
func (s *Setup) DefaultKoDockerRepo() string { return "gcr.io/" + s.ProjectID + "/ate-images" }
