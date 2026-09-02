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
	Project
	Cluster
	Provision
	ControlPlane
	FilestoreCSI
	Autoscaling
	Demo
	Complete
)

// Order is the linear flow of the wizard.
var Order = []Step{
	Welcome, CheckSetup, Project, Cluster, Provision,
	ControlPlane, FilestoreCSI, Autoscaling, Demo, Complete,
}

// Title returns the heading shown for a step.
func (s Step) Title() string {
	switch s {
	case Welcome:
		return "Welcome"
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
	idx     int
	history []int
}

// NewMachine starts at Welcome.
func NewMachine() *Machine { return &Machine{} }

// Current returns the active step.
func (m *Machine) Current() Step { return Order[m.idx] }

// Next advances to the following step and records history for Prev.
func (m *Machine) Next() Step {
	if m.idx < len(Order)-1 {
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
	if s.KoDockerRepo == "" {
		s.KoDockerRepo = "gcr.io/" + s.ProjectID + "/ate-images"
	}
	return nil
}
