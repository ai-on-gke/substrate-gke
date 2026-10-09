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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// A failed call's error ends up verbatim in an on-screen panel, so it quotes
// only Google's status and message, or the first couple of KB of anything
// else, never a whole proxy error page.
func TestCallAPIErrorQuotesLittleOfTheBody(t *testing.T) {
	googleErr := `{"error": {"code": 403, "status": "PERMISSION_DENIED", "message": "The caller does not have permission", "details": [{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "IAM_PERMISSION_DENIED"}]}}`
	html := "<html><body>" + strings.Repeat("Your proxy says no. ", 25000) + "</body></html>" // ~500KB
	for _, tc := range []struct {
		name, body string
		want       string
		maxLen     int
	}{
		{"google", googleErr, "403 Forbidden: PERMISSION_DENIED: The caller does not have permission", 0},
		{"html", html, "<html><body>Your proxy says no.", maxErrorQuote + 200},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(tc.body))
		}))
		_, err := probeClient(srv.URL).callAPI(context.Background(), http.MethodGet, srv.URL, nil)
		srv.Close()
		if err == nil {
			t.Fatalf("%s: want an error for a 403", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %.300q, want it to contain %q", tc.name, err.Error(), tc.want)
		}
		if tc.name == "google" && strings.Contains(err.Error(), "ErrorInfo") {
			t.Errorf("google: error should carry the message alone, got %q", err.Error())
		}
		if tc.maxLen > 0 && len(err.Error()) > tc.maxLen {
			t.Errorf("%s: error is %d bytes, want at most %d", tc.name, len(err.Error()), tc.maxLen)
		}
	}
}

// Each token fetch is a gcloud spawn (~1s cold), so a Client fetches once
// and its probes, concurrent ones included, share the token until it ages
// out. A failed fetch is not remembered: the next call tries again.
func TestAccessTokenIsFetchedOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Write([]byte(`{"billingEnabled": true, "state": "ENABLED", "permissions": []}`))
	}))
	defer srv.Close()
	var fetches atomic.Int32
	c := &Client{
		billingBase: srv.URL, serviceUsageBase: srv.URL, crmBase: srv.URL,
		token: func(context.Context) (string, error) {
			fetches.Add(1)
			return "test-token\n", nil
		},
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(3)
		go func() { defer wg.Done(); c.BillingEnabled(context.Background(), "acme") }()
		go func() { defer wg.Done(); c.ServiceEnabled(context.Background(), "acme", GKEService) }()
		go func() { defer wg.Done(); c.MissingPermissions(context.Background(), "acme") }()
	}
	wg.Wait()
	if n := fetches.Load(); n != 1 {
		t.Errorf("24 concurrent probes fetched the token %d times, want 1", n)
	}

	// Once the token has aged out, the next call fetches a fresh one.
	c.tokenMu.Lock()
	c.tokenExpiry = time.Now().Add(-time.Second)
	c.tokenMu.Unlock()
	c.BillingEnabled(context.Background(), "acme")
	if n := fetches.Load(); n != 2 {
		t.Errorf("after expiry: %d fetches, want 2", n)
	}
}

func TestAccessTokenFailureIsNotCached(t *testing.T) {
	calls := 0
	c := &Client{token: func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("gcloud: not logged in")
		}
		return "test-token", nil
	}}
	if _, err := c.accessToken(context.Background()); err == nil {
		t.Fatal("want the fetch error")
	}
	if tok, err := c.accessToken(context.Background()); err != nil || tok != "test-token" {
		t.Errorf("after a failed fetch the next call should retry, got (%q, %v)", tok, err)
	}
}

// With broken ADC, everyone already waiting on the failing fetch shares its
// error instead of each spawning gcloud again: one submit (warm-up +
// billing + API + permissions) costs one fetch.
func TestFailedTokenFetchIsSharedByItsWaiters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made without a token")
	}))
	defer srv.Close()
	var fetches atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := &Client{billingBase: srv.URL, serviceUsageBase: srv.URL, crmBase: srv.URL,
		token: func(context.Context) (string, error) {
			if fetches.Add(1) == 1 {
				close(started)
			}
			<-release
			return "", errors.New("Reauthentication failed. cannot prompt during non-interactive execution.")
		}}

	go c.WarmToken(context.Background())
	<-started
	errs := make(chan error, 3)
	go func() { _, err := c.BillingEnabled(context.Background(), "acme"); errs <- err }()
	go func() { _, err := c.ServiceEnabled(context.Background(), "acme", GKEService); errs <- err }()
	go func() { _, err := c.MissingPermissions(context.Background(), "acme"); errs <- err }()
	time.Sleep(50 * time.Millisecond) // let the probes queue on the fetch
	close(release)
	for range 3 {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "Reauthentication failed") {
			t.Errorf("each probe should report the shared fetch's error, got %v", err)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("a failing fetch was run %d times for one submit, want 1", n)
	}
}

// tokenMu is never held while gcloud runs, so ResetToken and rejectToken
// return at once even while a fetch is stuck (the UI calls ResetToken).
func TestTokenLockIsNotHeldDuringAFetch(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := &Client{token: func(context.Context) (string, error) {
		close(started)
		<-release
		return "tok", nil
	}}
	defer close(release)
	go c.WarmToken(context.Background())
	<-started
	done := make(chan struct{})
	go func() {
		c.ResetToken()
		c.rejectToken("tok")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ResetToken blocked behind a running token fetch")
	}
	// A caller whose context ends stops waiting, too.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.accessToken(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("accessToken with an expired ctx = %v, want DeadlineExceeded", err)
	}
}

// A submit that ended early (a malformed project ID fails `projects
// describe` in a second) can leave its warm-up still fetching. The next
// submit's ResetToken adopts that fetch instead of discarding it, so its
// probes join it rather than spawning gcloud again.
func TestResetTokenAdoptsAFetchInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"billingEnabled": true}`))
	}))
	defer srv.Close()
	var fetches atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := &Client{billingBase: srv.URL, token: func(context.Context) (string, error) {
		if fetches.Add(1) == 1 {
			close(started)
		}
		<-release
		return "tok", nil
	}}
	go c.WarmToken(context.Background()) // submit 1's warm-up
	<-started
	c.ResetToken() // submit 2 begins while it is still fetching
	go func() { time.Sleep(20 * time.Millisecond); close(release) }()
	if on, err := c.BillingEnabled(context.Background(), "acme"); err != nil || !on {
		t.Fatalf("BillingEnabled = (%v, %v)", on, err)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("submit 2 spawned its own fetch: %d fetches, want 1", n)
	}
}

// A 401 means the cached token is no good (gcloud can hand back its own
// token near expiry): the call drops it, fetches a fresh one and retries
// once. A second 401 is reported, not retried forever.
func TestCallAPIRefetchesTheTokenOn401(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer new" {
			http.Error(w, `{"error": {"status": "UNAUTHENTICATED", "message": "Request had invalid authentication credentials."}}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"billingEnabled": true}`))
	}))
	defer srv.Close()

	tokens := []string{"old", "new"}
	var fetches int
	c := &Client{billingBase: srv.URL, token: func(context.Context) (string, error) {
		tok := tokens[min(fetches, len(tokens)-1)]
		fetches++
		return tok, nil
	}}
	on, err := c.BillingEnabled(context.Background(), "acme")
	if err != nil || !on {
		t.Fatalf("BillingEnabled = (%v, %v), want a retry with the fresh token to succeed", on, err)
	}
	if fetches != 2 || requests.Load() != 2 {
		t.Errorf("fetches = %d, requests = %d; want 2 and 2", fetches, requests.Load())
	}
	// The fresh token is the one cached now.
	c.BillingEnabled(context.Background(), "acme")
	if fetches != 2 {
		t.Errorf("the refetched token should be cached, fetches = %d", fetches)
	}

	// A token gcloud keeps handing back unchanged: one refetch, which
	// returns the same token, so no retry is sent and the 401 is the answer.
	requests.Store(0)
	var badFetches atomic.Int32
	bad := &Client{billingBase: srv.URL, token: func(context.Context) (string, error) {
		badFetches.Add(1)
		return "bad", nil
	}}
	if _, err := bad.BillingEnabled(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "UNAUTHENTICATED") {
		t.Errorf("a persistent 401 should be reported, got %v", err)
	}
	if n, f := requests.Load(), badFetches.Load(); n != 1 || f != 2 {
		t.Errorf("an unchanged rejected token made %d requests and %d fetches, want 1 and 2", n, f)
	}
}

// ResetToken scopes the cache to one user action: the next call asks gcloud
// again, so a re-run `gcloud auth application-default login` takes effect.
func TestResetTokenForcesAFreshFetch(t *testing.T) {
	var fetches int
	c := &Client{token: func(context.Context) (string, error) {
		fetches++
		return fmt.Sprintf("token-%d", fetches), nil
	}}
	first, _ := c.accessToken(context.Background())
	again, _ := c.accessToken(context.Background())
	c.ResetToken()
	fresh, _ := c.accessToken(context.Background())
	if first != "token-1" || again != "token-1" || fresh != "token-2" {
		t.Errorf("got %q, %q, %q; want token-1, token-1, token-2", first, again, fresh)
	}
}

// WarmToken overlaps the token fetch with other work: a probe that starts
// while the warm-up is still fetching waits for it instead of fetching
// again. Under DryRun it does nothing.
func TestWarmTokenIsSharedWithTheProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"billingEnabled": true}`))
	}))
	defer srv.Close()
	var fetches atomic.Int32
	started := make(chan struct{})
	c := &Client{billingBase: srv.URL, token: func(context.Context) (string, error) {
		if fetches.Add(1) == 1 {
			close(started)
		}
		time.Sleep(50 * time.Millisecond) // a cold gcloud
		return "test-token", nil
	}}
	go c.WarmToken(context.Background())
	<-started // the warm-up is mid-fetch
	if on, err := c.BillingEnabled(context.Background(), "acme"); err != nil || !on {
		t.Fatalf("BillingEnabled = (%v, %v)", on, err)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("a probe during the warm-up fetched again: %d fetches, want 1", n)
	}

	dry := &Client{DryRun: true, token: func(context.Context) (string, error) {
		panic("dry-run warmed a token")
	}}
	dry.WarmToken(context.Background())
}

// rejectToken only drops the token it was told about: a concurrent caller's
// fresher token must survive another caller's stale 401.
func TestRejectTokenKeepsAFresherToken(t *testing.T) {
	c := &Client{token: func(context.Context) (string, error) { return "fresh", nil }}
	c.accessToken(context.Background())
	c.rejectToken("stale")
	c.tokenMu.Lock()
	got := c.cachedToken
	c.tokenMu.Unlock()
	if got != "fresh" {
		t.Errorf("cachedToken = %q, want the fresher token kept", got)
	}
}

// gcloud refreshes only what it thinks has expired, so a revoked token comes
// back unchanged. Concurrent probes that all get a 401 with it must not each
// refetch and retry: the action pays one refetch, and no retry is sent with
// a token known to be rejected.
func TestRevokedTokenCostsOneRefetchAndNoRetries(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, `{"error": {"status": "UNAUTHENTICATED", "message": "Request had invalid authentication credentials."}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	var fetches atomic.Int32
	c := &Client{billingBase: srv.URL, serviceUsageBase: srv.URL, crmBase: srv.URL,
		token: func(context.Context) (string, error) {
			fetches.Add(1)
			time.Sleep(10 * time.Millisecond)
			return "revoked", nil
		}}

	errs := make(chan error, 3)
	go func() { _, err := c.BillingEnabled(context.Background(), "acme"); errs <- err }()
	go func() { _, err := c.ServiceEnabled(context.Background(), "acme", GKEService); errs <- err }()
	go func() { _, err := c.MissingPermissions(context.Background(), "acme"); errs <- err }()
	for range 3 {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "UNAUTHENTICATED") {
			t.Errorf("each probe should report the 401, got %v", err)
		}
	}
	if n := fetches.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2 (the first, and one refetch for all three 401s)", n)
	}
	if n := requests.Load(); n != 3 {
		t.Errorf("requests = %d, want 3 (no retry with an unchanged rejected token)", n)
	}

	// The next user action forgets the rejection: it fetches afresh and,
	// on the same 401, gets its own one refetch.
	c.ResetToken()
	c.BillingEnabled(context.Background(), "acme")
	if n := fetches.Load(); n != 4 {
		t.Errorf("after ResetToken: fetches = %d, want 4", n)
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
