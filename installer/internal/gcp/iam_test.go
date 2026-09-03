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

package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fakeCRM(t *testing.T, held []string, wantProject string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/v1/projects/" + wantProject + ":testIamPermissions"; r.URL.Path != want {
			t.Errorf("request path = %q, want %q", r.URL.Path, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want the trimmed bearer token", got)
		}
		var req struct {
			Permissions []string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if len(req.Permissions) != len(BootstrapPermissions) {
			t.Errorf("asked about %d permissions, want %d", len(req.Permissions), len(BootstrapPermissions))
		}
		json.NewEncoder(w).Encode(map[string][]string{"permissions": held})
	}))
}

func permClient(base string) *Client {
	return &Client{
		crmBase: base,
		// The token normally comes from gcloud; tests must not shell out.
		// The trailing newline mimics print-access-token output.
		token: func(context.Context) (string, error) { return "test-token\n", nil },
	}
}

func TestMissingPermissionsAllHeld(t *testing.T) {
	var all []string
	for _, p := range BootstrapPermissions {
		all = append(all, p.Permission)
	}
	srv := fakeCRM(t, all, "acme")
	defer srv.Close()

	missing, err := permClient(srv.URL).MissingPermissions(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("all permissions held, but reported missing: %v", missing)
	}
}

func TestMissingPermissionsReportsTheGap(t *testing.T) {
	// Everything except cluster creation and IAM administration.
	held := []string{
		"serviceusage.services.enable",
		"storage.buckets.create",
		"monitoring.dashboards.create",
	}
	srv := fakeCRM(t, held, "acme")
	defer srv.Close()

	missing, err := permClient(srv.URL).MissingPermissions(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	want := []RequiredPermission{
		{"container.clusters.create", "roles/container.admin"},
		{"resourcemanager.projects.setIamPolicy", "roles/resourcemanager.projectIamAdmin"},
	}
	if len(missing) != len(want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Errorf("missing[%d] = %v, want %v", i, missing[i], want[i])
		}
	}
}

// A failed probe must come back as an error, never as "nothing missing" — the
// caller warns on errors but blocks on missing permissions, and an API
// rejection proves neither.
func TestMissingPermissionsSurfacesAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error": {"message": "Cloud Resource Manager API has not been used"}}`, http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := permClient(srv.URL).MissingPermissions(context.Background(), "acme"); err == nil {
		t.Fatal("want an error when the API rejects the probe")
	}
}

// Dry-run must not touch the network or gcloud.
func TestMissingPermissionsDryRun(t *testing.T) {
	c := &Client{DryRun: true, token: func(context.Context) (string, error) {
		panic("dry-run asked for a token")
	}}
	missing, err := c.MissingPermissions(context.Background(), "acme")
	if err != nil || missing != nil {
		t.Errorf("dry-run = (%v, %v), want (nil, nil)", missing, err)
	}
}
