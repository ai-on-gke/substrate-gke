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

import "testing"

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
	m.Next() // Project
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
	if _, ok := Welcome.Number(); ok {
		t.Fatal("Welcome should not be numbered")
	}
	if _, ok := Complete.Number(); ok {
		t.Fatal("Complete should not be numbered")
	}
	if n, ok := CheckSetup.Number(); !ok || n != 1 {
		t.Fatalf("CheckSetup.Number() = %d/%v, want 1/true", n, ok)
	}
	if n, ok := FilestoreCSI.Number(); !ok || n != 6 {
		t.Fatalf("FilestoreCSI.Number() = %d/%v, want 6/true", n, ok)
	}
	if n, _ := Demo.Number(); n != NumberedSteps {
		t.Fatalf("Demo.Number() = %d, want %d", n, NumberedSteps)
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

func TestApplyProjectDefaultsRespectsOverrides(t *testing.T) {
	s := NewSetup()
	s.ProjectID = "acme"
	s.ApplyProjectDefaults()
	if s.BucketName != defaultBucketName("acme", "substrate-poc") {
		t.Errorf("BucketName = %q, want %q", s.BucketName, defaultBucketName("acme", "substrate-poc"))
	}
	if s.KoDockerRepo != "gcr.io/acme/ate-images" {
		t.Errorf("KoDockerRepo = %q", s.KoDockerRepo)
	}

	custom := NewSetup()
	custom.ProjectID = "acme"
	custom.BucketName = "my-bucket"
	custom.KoDockerRepo = "us-docker.pkg.dev/acme/repo"
	custom.ApplyProjectDefaults()
	if custom.BucketName != "my-bucket" || custom.KoDockerRepo != "us-docker.pkg.dev/acme/repo" {
		t.Errorf("overrides clobbered: %q %q", custom.BucketName, custom.KoDockerRepo)
	}
}

func TestDefaultBucketName(t *testing.T) {
	for _, tc := range []struct {
		project, cluster, want string
	}{
		{"acme", "cluster-1", "ate-snapshots-acme-4afb32cc106e2c92"},
		{"My-Project", "Cluster-A", "ate-snapshots-my-project-34ab3e1c8c468878"},
		{
			"a-very-long-gcp-project-name-123",
			"a-very-long-gke-cluster-name-4567890",
			"ate-snapshots-a-very-long-gcp-project-name-123-297d5453509db54d",
		},
	} {
		got := defaultBucketName(tc.project, tc.cluster)
		if got != tc.want {
			t.Errorf("defaultBucketName(%q, %q) = %q, want %q", tc.project, tc.cluster, got, tc.want)
		}
		if len(got) > 63 {
			t.Errorf("defaultBucketName(%q, %q) length %d > 63", tc.project, tc.cluster, len(got))
		}
	}
}
