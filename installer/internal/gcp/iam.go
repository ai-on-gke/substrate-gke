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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RequiredPermission is one permission the bootstrap step exercises, paired
// with the predefined role that grants it so a failure can name the fix.
type RequiredPermission struct {
	Permission string
	Role       string
}

// BootstrapPermissions samples one permission from each thing `setup-gcp
// bootstrap` does: enable APIs, create the cluster, create the bucket, bind
// project IAM, and create the dashboards. Holding these does not prove the
// whole install will succeed, but missing one proves a step will fail.
var BootstrapPermissions = []RequiredPermission{
	{"serviceusage.services.enable", "roles/serviceusage.serviceUsageAdmin"},
	{"container.clusters.create", "roles/container.admin"},
	{"storage.buckets.create", "roles/storage.admin"},
	{"resourcemanager.projects.setIamPolicy", "roles/resourcemanager.projectIamAdmin"},
	{"monitoring.dashboards.create", "roles/monitoring.editor"},
}

// MissingPermissions reports which of the bootstrap permissions the active
// application-default credentials do not hold on projectID. It asks Cloud
// Resource Manager's testIamPermissions, which evaluates the caller's full
// effective policy — group memberships and org- or folder-inherited roles
// included — where reading the project policy could not. The identity tested
// is the ADC identity, which is exactly what setup-gcp will run as.
//
// An error means the question could not be asked (no token, no network, API
// rejection), not that permissions are missing; callers should degrade to a
// warning rather than block on it.
func (c *Client) MissingPermissions(ctx context.Context, projectID string) ([]RequiredPermission, error) {
	if c.DryRun {
		return nil, nil
	}
	fetchToken := c.token
	if fetchToken == nil {
		fetchToken = func(ctx context.Context) (string, error) {
			out, err := c.run(ctx, "auth", "application-default", "print-access-token")
			return string(out), err
		}
	}
	token, err := fetchToken(ctx)
	if err != nil {
		return nil, err
	}

	perms := make([]string, len(BootstrapPermissions))
	for i, p := range BootstrapPermissions {
		perms[i] = p.Permission
	}
	body, err := json.Marshal(map[string][]string{"permissions": perms})
	if err != nil {
		return nil, err
	}

	base := c.crmBase
	if base == "" {
		base = "https://cloudresourcemanager.googleapis.com"
	}
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/projects/%s:testIamPermissions", base, projectID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("testIamPermissions on %s: %s: %s",
			projectID, resp.Status, strings.TrimSpace(string(respBody)))
	}

	var held struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(respBody, &held); err != nil {
		return nil, fmt.Errorf("parsing testIamPermissions response: %w", err)
	}
	return missingFrom(held.Permissions), nil
}

// missingFrom returns the bootstrap permissions absent from held, in the
// stable BootstrapPermissions order.
func missingFrom(held []string) []RequiredPermission {
	heldSet := make(map[string]bool, len(held))
	for _, p := range held {
		heldSet[p] = true
	}
	var missing []RequiredPermission
	for _, p := range BootstrapPermissions {
		if !heldSet[p.Permission] {
			missing = append(missing, p)
		}
	}
	return missing
}
