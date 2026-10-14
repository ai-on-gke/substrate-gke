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
	"fmt"
	"net/http"
	"net/url"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
)

// GKEService is the API every GKE call — listing clusters included — needs
// enabled on the project. setup-gcp bootstrap enables it too, but the
// cluster step lists clusters before bootstrap ever runs.
const GKEService = "container.googleapis.com"

// BillingEnabled reports whether projectID has an active billing account.
// Without one, GKE refuses every request, so the install cannot get past the
// cluster step.
//
// It asks Cloud Billing's projects.getBillingInfo, which needs only
// resourcemanager.projects.get on the project. An error means the question
// could not be asked, not that billing is off; callers should degrade to a
// warning rather than block on it.
func (c *Client) BillingEnabled(ctx context.Context, projectID string) (bool, error) {
	if c.DryRun {
		return true, nil
	}
	base := c.billingBase
	if base == "" {
		base = "https://cloudbilling.googleapis.com"
	}
	body, err := c.callAPI(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/projects/%s/billingInfo?fields=billingEnabled", base, url.PathEscape(projectID)), nil)
	if err != nil {
		return false, fmt.Errorf("reading billing info for %s: %w", projectID, err)
	}
	var info struct {
		BillingEnabled bool `json:"billingEnabled"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return false, fmt.Errorf("parsing billing info: %w", err)
	}
	return info.BillingEnabled, nil
}

// ServiceEnabled reports whether service (e.g. GKEService) is enabled on
// projectID, through Service Usage's services.get. As with BillingEnabled,
// an error means the probe could not run, not that the service is off.
//
// It asks for the state field alone: the full resource carries the
// service's whole config, and for GKEService that is ~66KB of RPC method
// names.
func (c *Client) ServiceEnabled(ctx context.Context, projectID, service string) (bool, error) {
	if c.DryRun {
		return true, nil
	}
	base := c.serviceUsageBase
	if base == "" {
		base = "https://serviceusage.googleapis.com"
	}
	body, err := c.callAPI(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/projects/%s/services/%s?fields=state", base, url.PathEscape(projectID), url.PathEscape(service)), nil)
	if err != nil {
		return false, fmt.Errorf("reading %s state on %s: %w", service, projectID, err)
	}
	var svc struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &svc); err != nil {
		return false, fmt.Errorf("parsing service state: %w", err)
	}
	return svc.State == "ENABLED", nil
}

// EnableServiceCommand is the paste-able command that enables service on
// projectID.
func EnableServiceCommand(projectID, service string) string {
	return fmt.Sprintf("gcloud services enable %s --project=%s", service, projectID)
}

// EnableService returns the command that enables service on projectID. It
// runs through the wizard's Runner rather than Client.run: enabling can take
// a minute or two, longer than cmdTimeout, and the Runner logs it and
// simulates it under --dry-run.
func EnableService(projectID, service string) execx.Spec {
	return execx.Spec{
		Label:    "enable " + service,
		Display:  EnableServiceCommand(projectID, service),
		Argv:     []string{"gcloud", "services", "enable", service, "--project=" + projectID},
		SimLines: []string{"Operation \"operations/acf.p2-123456789012-simulated\" finished successfully."},
	}
}
