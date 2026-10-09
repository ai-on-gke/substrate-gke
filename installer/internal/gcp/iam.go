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
//
// A 401 means the token is no good (gcloud can hand back its own cached
// token close to expiry), so callAPI marks it rejected, gets a fresh one
// and retries once, but only if the fresh token differs. gcloud refreshes
// only what it thinks has expired, so a revoked or scope-limited token comes
// back unchanged, and retrying with it cannot succeed. Concurrent callers
// that hit the same 401 share one refetch (see accessToken and rejectToken).
func (c *Client) callAPI(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	status, respBody, err := c.send(ctx, method, url, body, token)
	if err == nil && status == http.StatusUnauthorized {
		c.rejectToken(token)
		fresh, ferr := c.accessToken(ctx)
		if ferr != nil {
			return nil, ferr
		}
		if fresh != token {
			status, respBody, err = c.send(ctx, method, url, body, fresh)
		}
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%d %s: %s", status, http.StatusText(status), apiErrorText(respBody))
	}
	// A cut-off body would only fail later as unparseable JSON, which
	// reads like a broken API rather than an oversized answer.
	if len(respBody) > maxAPIResponse {
		return nil, fmt.Errorf("response larger than %d bytes", maxAPIResponse)
	}
	return respBody, nil
}

// send makes one HTTP request with token and returns the status and up to
// maxAPIResponse+1 bytes of the body.
func (c *Client) send(ctx context.Context, method, url string, body []byte, token string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// maxAPIResponse caps what callAPI reads. The probes ask for single fields,
// so anything near it means a request forgot to.
const maxAPIResponse = 1 << 20

// tokenTTL caps how long accessToken reuses a token. Callers scope the cache
// to one user action with ResetToken, and a 401 evicts it, so this is only a
// backstop.
const tokenTTL = 5 * time.Minute

// tokenFetch is one `print-access-token` run, shared by every caller that
// wants a token while it is under way.
type tokenFetch struct {
	done  chan struct{} // closed once token and err are set
	token string
	err   error
}

// accessToken returns the application-default access token, asking gcloud
// (or the token override) at most once per tokenTTL.
//
// It is a singleflight: a caller that finds no cached token starts one
// fetch, and every caller arriving while it runs waits for that same fetch
// and gets its result, error included. tokenMu is never held while gcloud
// runs, so ResetToken and rejectToken return at once. A failed fetch is
// not cached for later callers; the next user action (ResetToken) or call
// tries again. Each caller stops waiting when its own ctx ends; the fetch
// itself runs on under cmdTimeout and still serves the rest.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry) {
		token := c.cachedToken
		c.tokenMu.Unlock()
		return token, nil
	}
	f := c.tokenFetch
	if f == nil {
		f = &tokenFetch{done: make(chan struct{})}
		c.tokenFetch = f
		go c.runTokenFetch(f)
	}
	c.tokenMu.Unlock()

	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// runTokenFetch runs f, caches its token on success and wakes its waiters.
// It runs detached from any one caller's context, since several share it.
func (c *Client) runTokenFetch(f *tokenFetch) {
	fetch := c.token
	if fetch == nil {
		fetch = func(ctx context.Context) (string, error) {
			out, err := c.run(ctx, "auth", "application-default", "print-access-token")
			return string(out), err
		}
	}
	token, err := fetch(context.Background())
	token = strings.TrimSpace(token)

	c.tokenMu.Lock()
	if c.tokenFetch == f {
		c.tokenFetch = nil
	}
	if err == nil {
		c.cachedToken = token
		c.tokenExpiry = time.Now().Add(tokenTTL)
	}
	f.token, f.err = token, err
	c.tokenMu.Unlock()
	close(f.done)
}

// ResetToken forgets the cached access token, so the next REST call asks
// gcloud again. Call it at the start of each user action (a submit): the
// calls within that action still share one fetch, but a new action sees a
// re-run `gcloud auth application-default login` right away.
//
// It never waits: tokenMu is not held while gcloud runs. A fetch already
// under way is adopted, not abandoned. It can only be the previous action's
// warm-up, still running because that action ended early (e.g. a malformed
// project ID fails `projects describe` in a second), so its token is as
// fresh as a new fetch's and joining it saves a spawn.
func (c *Client) ResetToken() {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.cachedToken = ""
	c.rejectedToken = ""
}

// WarmToken fetches the access token into the cache ahead of the REST calls
// that need it, so the fetch (a cold gcloud spawn) can overlap other work.
// The token does not depend on the project, so it can start before the
// project is even resolved. REST calls made while it runs wait for it
// rather than fetching again, and share its error if it fails; only calls
// made after a failure fetch again. No-op under DryRun.
func (c *Client) WarmToken(ctx context.Context) {
	if c.DryRun {
		return
	}
	c.accessToken(ctx) //nolint:errcheck // the REST calls report it
}

// rejectToken records that token just got a 401. The first caller to report
// it drops it from the cache, so the next accessToken refetches; callers
// reporting the same token after that leave the cache alone. If gcloud
// handed the same token back, it is cached again and they get it at once,
// with no spawn, see that it is unchanged, and skip the retry. So however
// many concurrent probes hit a 401 with one token, the action pays for one
// refetch. ResetToken forgets the rejection with the token.
func (c *Client) rejectToken(token string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	token = strings.TrimSpace(token)
	if c.rejectedToken == token {
		return
	}
	c.rejectedToken = token
	if c.cachedToken == token {
		c.cachedToken = ""
	}
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
