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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGET serves body at wantPath and fails the test on any other request.
// The probes must ask for their one field: full responses can be huge.
func fakeGET(t *testing.T, wantPath, wantFields, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != wantPath {
			t.Errorf("request path = %q, want %q", r.URL.Path, wantPath)
		}
		if got := r.URL.Query().Get("fields"); got != wantFields {
			t.Errorf("fields = %q, want %q", got, wantFields)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want the trimmed bearer token", got)
		}
		w.Write([]byte(body))
	}))
}

func probeClient(base string) *Client {
	return &Client{
		billingBase:      base,
		serviceUsageBase: base,
		token:            func(context.Context) (string, error) { return "test-token\n", nil },
	}
}

func TestBillingEnabled(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"name": "projects/acme/billingInfo", "billingAccountName": "billingAccounts/0X0X0X", "billingEnabled": true}`, true},
		{`{"name": "projects/acme/billingInfo", "billingAccountName": "", "billingEnabled": false}`, false},
		// The API omits false fields entirely.
		{`{"name": "projects/acme/billingInfo"}`, false},
	} {
		srv := fakeGET(t, "/v1/projects/acme/billingInfo", "billingEnabled", tc.body)
		got, err := probeClient(srv.URL).BillingEnabled(context.Background(), "acme")
		srv.Close()
		if err != nil {
			t.Fatalf("BillingEnabled(%s): %v", tc.body, err)
		}
		if got != tc.want {
			t.Errorf("BillingEnabled(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestServiceEnabled(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"name": "projects/123/services/container.googleapis.com", "state": "ENABLED"}`, true},
		{`{"name": "projects/123/services/container.googleapis.com", "state": "DISABLED"}`, false},
		{`{"name": "projects/123/services/container.googleapis.com", "state": "STATE_UNSPECIFIED"}`, false},
	} {
		srv := fakeGET(t, "/v1/projects/acme/services/container.googleapis.com", "state", tc.body)
		got, err := probeClient(srv.URL).ServiceEnabled(context.Background(), "acme", GKEService)
		srv.Close()
		if err != nil {
			t.Fatalf("ServiceEnabled(%s): %v", tc.body, err)
		}
		if got != tc.want {
			t.Errorf("ServiceEnabled(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// A rejected probe must come back as an error, never as "disabled": the
// caller blocks on disabled but only warns on an error, and a 403 proves
// neither.
func TestProjectProbesSurfaceAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error": {"message": "Cloud Billing API has not been used in project"}}`, http.StatusForbidden)
	}))
	defer srv.Close()
	c := probeClient(srv.URL)

	if _, err := c.BillingEnabled(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "has not been used") {
		t.Errorf("BillingEnabled error = %v, want one carrying the API's reason", err)
	}
	if _, err := c.ServiceEnabled(context.Background(), "acme", GKEService); err == nil {
		t.Error("ServiceEnabled: want an error when the API rejects the probe")
	}
}

// The full container.googleapis.com resource is ~66KB, which once overran a
// 64KB read cap: the truncated JSON failed to parse, and that probe error
// was waved through as advisory. An oversized answer must be a clear error,
// and one under the cap must parse whole.
func TestCallAPIHandlesLargeResponses(t *testing.T) {
	methods := strings.Repeat(`{"name": "SomeRpcMethod"},`, 3000) // ~80KB
	big := `{"config": {"apis": [{"methods": [` + strings.TrimSuffix(methods, ",") + `]}]}, "state": "DISABLED"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()
	c := probeClient(srv.URL)

	on, err := c.ServiceEnabled(context.Background(), "acme", GKEService)
	if err != nil {
		t.Fatalf("an 80KB response should parse, got %v", err)
	}
	if on {
		t.Error("state DISABLED read as enabled")
	}

	huge := strings.Repeat("x", maxAPIResponse+10)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(huge))
	}))
	defer srv2.Close()
	if _, err := probeClient(srv2.URL).ServiceEnabled(context.Background(), "acme", GKEService); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized response should say so, got %v", err)
	}
}

// Dry-run must not touch the network or gcloud, and must report a healthy
// project so the walkthrough continues.
func TestProjectProbesDryRun(t *testing.T) {
	c := &Client{DryRun: true, token: func(context.Context) (string, error) {
		panic("dry-run asked for a token")
	}}
	if ok, err := c.BillingEnabled(context.Background(), "acme"); !ok || err != nil {
		t.Errorf("dry-run BillingEnabled = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := c.ServiceEnabled(context.Background(), "acme", GKEService); !ok || err != nil {
		t.Errorf("dry-run ServiceEnabled = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestEnableServiceSpec(t *testing.T) {
	spec := EnableService("acme", GKEService)
	want := []string{"gcloud", "services", "enable", "container.googleapis.com", "--project=acme"}
	if strings.Join(spec.Argv, " ") != strings.Join(want, " ") {
		t.Errorf("Argv = %v, want %v", spec.Argv, want)
	}
	if spec.Display != EnableServiceCommand("acme", GKEService) {
		t.Errorf("Display = %q, want the paste-able command", spec.Display)
	}
	if len(spec.SimLines) == 0 {
		t.Error("a dry run needs something to replay")
	}
}
