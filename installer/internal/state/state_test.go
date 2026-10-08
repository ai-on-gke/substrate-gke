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

package state

import (
	"strings"
	"testing"
)

func TestMachineWalksTheWholeFlow(t *testing.T) {
	m := NewMachine()
	if m.Current() != Welcome {
		t.Fatalf("start = %v, want Welcome", m.Current())
	}
	for i := 1; i < len(Order); i++ {
		if got := m.Next(); got != Order[i] {
			t.Fatalf("Next() #%d = %v, want %v", i, got, Order[i])
		}
	}
	if got := m.Next(); got != Complete {
		t.Fatalf("Next() past the end = %v, want Complete", got)
	}
}

func TestMachinePrevUsesHistory(t *testing.T) {
	m := NewMachine()
	m.Next() // CheckSetup
	m.Next() // Images
	if got, ok := m.Prev(); !ok || got != CheckSetup {
		t.Fatalf("Prev() = %v/%v, want CheckSetup/true", got, ok)
	}
	if got, ok := m.Prev(); !ok || got != Welcome {
		t.Fatalf("Prev() = %v/%v, want Welcome/true", got, ok)
	}
	if _, ok := m.Prev(); ok {
		t.Fatal("Prev() at Welcome should report no history")
	}
}

func TestStepNumbering(t *testing.T) {
	m := NewMachine()
	for _, s := range []Step{Welcome, Complete} {
		if _, ok := m.Position(s); ok {
			t.Errorf("%v should not be numbered", s)
		}
	}
	for step, want := range map[Step]int{CheckSetup: 1, Images: 2, FilestoreCSI: 7, Autoscaling: 8, Sandbox: 9} {
		if n, ok := m.Position(step); !ok || n != want {
			t.Errorf("Position(%v) = %d/%v, want %d/true", step, n, ok, want)
		}
	}
	if n, _ := m.Position(Demo); n != m.NumberedSteps() {
		t.Errorf("Position(Demo) = %d, want the last numbered step %d", n, m.NumberedSteps())
	}
}

func TestRegionDerivation(t *testing.T) {
	s := NewSetup()
	for _, tc := range []struct{ zone, want string }{
		{"us-west1-c", "us-west1"},
		{"europe-west4-a", "europe-west4"},
		{"us-central1", "us-central1"}, // regional location stays put
	} {
		s.Zone = tc.zone
		if got := s.Region(); got != tc.want {
			t.Errorf("Region(%q) = %q, want %q", tc.zone, got, tc.want)
		}
	}
}

func TestNewSetupDefaults(t *testing.T) {
	s := NewSetup()
	if s.ClusterName != "substrate-test" {
		t.Errorf("ClusterName = %q, want substrate-test", s.ClusterName)
	}
}

// MicroVMActive is the outcome, not the choice: micro-VM chosen but not
// staged (skipped or failed) and staged-then-switched-back both read false.
func TestMicroVMActive(t *testing.T) {
	var nilSetup *Setup
	if nilSetup.MicroVMActive() {
		t.Error("nil Setup: MicroVMActive = true, want false")
	}
	for _, tc := range []struct {
		class  string
		staged bool
		want   bool
	}{
		{"", false, false},
		{"", true, false},
		{SandboxGVisor, false, false},
		{SandboxGVisor, true, false},
		{SandboxMicroVM, false, false},
		{SandboxMicroVM, true, true},
	} {
		s := &Setup{SandboxClass: tc.class, MicroVMDeployed: tc.staged}
		if got := s.MicroVMActive(); got != tc.want {
			t.Errorf("class=%q staged=%v: MicroVMActive = %v, want %v", tc.class, tc.staged, got, tc.want)
		}
	}
}

func TestApplyProjectDefaultsRespectsOverrides(t *testing.T) {
	s := NewSetup()
	s.ProjectID = "acme"
	if err := s.ApplyProjectDefaults(); err != nil {
		t.Fatalf("ApplyProjectDefaults failed: %v", err)
	}
	if want := defaultBucketName("acme", s.ClusterName, s.Zone); s.BucketName != want {
		t.Errorf("BucketName = %q, want %q", s.BucketName, want)
	}

	custom := NewSetup()
	custom.ProjectID = "acme"
	custom.BucketName = "my-bucket"
	custom.KoDockerRepo = "us-docker.pkg.dev/acme/repo"
	if err := custom.ApplyProjectDefaults(); err != nil {
		t.Fatalf("ApplyProjectDefaults failed: %v", err)
	}
	if custom.BucketName != "my-bucket" || custom.KoDockerRepo != "us-docker.pkg.dev/acme/repo" {
		t.Errorf("overrides clobbered: %q %q", custom.BucketName, custom.KoDockerRepo)
	}
}

func TestApplyProjectDefaultsValidatesRepositoryName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"", true},
		{"ate-images", true},
		{"My_Images", false},
		{"../images", false},
		{strings.Repeat("a", 64), false},
	} {
		s := NewSetup()
		s.ArtifactRegistryRepository = tc.name
		if err := s.ApplyProjectDefaults(); (err == nil) != tc.valid {
			t.Errorf("repository %q: %v; valid=%t", tc.name, err, tc.valid)
		}
	}
}

func TestDefaultBucketName(t *testing.T) {
	for _, tc := range []struct {
		project, cluster, zone, want string
	}{
		{"acme", "substrate-test", "us-west1-c", "ate-snapshots-acme-substrate-test-us-west1-c"},
		{"My-Project", "Legacy-Prod", "US-CENTRAL1-A", "ate-snapshots-my-project-legacy-prod-us-central1-a"},
	} {
		if got := defaultBucketName(tc.project, tc.cluster, tc.zone); got != tc.want {
			t.Errorf("defaultBucketName(%q, %q, %q) = %q, want %q", tc.project, tc.cluster, tc.zone, got, tc.want)
		}
	}
}

// Two clusters in one project and zone must never derive the same bucket:
// snapshot keys carry no cluster identifier and teardown deletes the whole
// bucket, so a shared bucket loses every sibling cluster's snapshots. That
// has to survive truncation — GCS caps bucket names at 63 characters, and
// project+cluster+zone routinely exceeds it.
func TestDefaultBucketNameIsPerClusterEvenWhenTruncated(t *testing.T) {
	const project, zone = "a-very-long-gcp-project-name-1234567890", "us-central1-a"
	a := defaultBucketName(project, "staging-cluster-with-a-long-name", zone)
	b := defaultBucketName(project, "staging-cluster-with-a-long-nap", zone)

	for _, name := range []string{a, b} {
		if len(name) > 63 {
			t.Errorf("bucket name %q exceeds 63 characters", name)
		}
		if name != strings.ToLower(name) || strings.HasSuffix(name, "-") {
			t.Errorf("bucket name %q is not a valid GCS name", name)
		}
	}
	if a == b {
		t.Errorf("two clusters derived the same bucket %q", a)
	}
	if again := defaultBucketName(project, "staging-cluster-with-a-long-name", zone); again != a {
		t.Errorf("derivation is not deterministic: %q then %q", a, again)
	}
}

// The upgrade flow numbers its own steps: the sidebar counts positions in the
// active order, not the Step constants, which belong to the install flow.
func TestUpgradeOrderPositions(t *testing.T) {
	m := NewMachine()
	m.SetOrder(UpgradeOrder)
	if m.Current() != Welcome {
		t.Fatalf("after SetOrder: %v, want Welcome", m.Current())
	}
	if n := m.NumberedSteps(); n != 4 {
		t.Errorf("NumberedSteps() = %d, want 4", n)
	}
	for step, want := range map[Step]int{CheckSetup: 1, UpgradeSource: 2, Images: 3, UpgradePlan: 4} {
		if got, ok := m.Position(step); !ok || got != want {
			t.Errorf("Position(%v) = %d/%v, want %d/true", step, got, ok, want)
		}
	}
	if _, ok := m.Position(Project); ok {
		t.Error("Project is not part of the upgrade flow")
	}
	for i := 1; i < len(UpgradeOrder); i++ {
		if got := m.Next(); got != UpgradeOrder[i] {
			t.Fatalf("Next() #%d = %v, want %v", i, got, UpgradeOrder[i])
		}
	}
}
