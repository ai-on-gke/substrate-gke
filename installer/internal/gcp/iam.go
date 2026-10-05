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
	"time"
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
	respBody, err := c.callAPI(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/projects/%s:testIamPermissions", base, projectID), body)
	if err != nil {
		return nil, fmt.Errorf("testIamPermissions on %s: %w", projectID, err)
	}

	var held struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(respBody, &held); err != nil {
		return nil, fmt.Errorf("parsing testIamPermissions response: %w", err)
	}
	return missingFrom(held.Permissions), nil
}

// callAPI sends one request to a Google REST API as the application-default
// credentials identity and returns the response body. A non-200 status is an
// error carrying the status and the body, which is where Google APIs put the
// reason ("API has not been used in project…", "permission denied").
func (c *Client) callAPI(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, apiErrorText(respBody))
	}
	// A cut-off body would only fail later as unparseable JSON, which
	// reads like a broken API rather than an oversized answer.
	if len(respBody) > maxAPIResponse {
		return nil, fmt.Errorf("response larger than %d bytes", maxAPIResponse)
	}
	return respBody, nil
}

// maxAPIResponse caps what callAPI reads. The probes ask for single fields,
// so anything near it means a request forgot to.
const maxAPIResponse = 1 << 20

// tokenTTL is how long accessToken reuses a token. ADC access tokens live
// about an hour; reusing one for a few minutes saves a gcloud spawn (~1s
// cold) per REST call without ever handing out one near expiry.
const tokenTTL = 5 * time.Minute

// accessToken returns the application-default access token, asking gcloud
// (or the token override) at most once per tokenTTL. Concurrent callers
// share one fetch. A failed fetch is not cached.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry) {
		return c.cachedToken, nil
	}
	fetch := c.token
	if fetch == nil {
		fetch = func(ctx context.Context) (string, error) {
			out, err := c.run(ctx, "auth", "application-default", "print-access-token")
			return string(out), err
		}
	}
	token, err := fetch(ctx)
	if err != nil {
		return "", err
	}
	c.cachedToken = strings.TrimSpace(token)
	c.tokenExpiry = time.Now().Add(tokenTTL)
	return c.cachedToken, nil
}

// maxErrorQuote caps how much of a non-JSON error body an error quotes.
// These errors end up verbatim in on-screen panels, and a proxy or
// captive-portal HTML page can run to hundreds of KB.
const maxErrorQuote = 2 << 10

// apiErrorText is what an error says about a failed call's body: the
// Google error envelope's status and message when it is one, or else the
// start of the body.
func apiErrorText(body []byte) string {
	var envelope struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		if envelope.Error.Status != "" {
			return envelope.Error.Status + ": " + envelope.Error.Message
		}
		return envelope.Error.Message
	}
	text := strings.TrimSpace(string(body))
	if len(text) > maxErrorQuote {
		text = strings.ToValidUTF8(text[:maxErrorQuote], "") + "…"
	}
	return text
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
