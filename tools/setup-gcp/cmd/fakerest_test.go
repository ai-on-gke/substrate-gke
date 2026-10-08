// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/proto"
)

// fakeREST is an in-process Cloud Storage and IAM JSON API server covering
// the bucket and service account IAM policy calls setup-gcp makes. Policies
// are keyed by resource: "b/<bucket>" for a bucket and
// "projects/<project>/serviceAccounts/<email>" for a service account. Every
// service account exists.
type fakeREST struct {
	mu sync.Mutex
	// policies holds each resource's stored IAM policy, conditions included.
	policies map[string]*iampb.Policy
	// requestedVersions records each policy read's requested version.
	requestedVersions map[string][]int32
	// written records each policy written.
	written map[string][]*iampb.Policy
}

// newFakeREST starts the fake and points restClientOptions at it for the test.
func newFakeREST(t *testing.T) *fakeREST {
	t.Helper()
	f := &fakeREST{
		policies:          map[string]*iampb.Policy{},
		requestedVersions: map[string][]int32{},
		written:           map[string][]*iampb.Policy{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	orig := restClientOptions
	restClientOptions = []option.ClientOption{
		option.WithEndpoint(srv.URL + "/"),
		option.WithoutAuthentication(),
	}
	t.Cleanup(func() { restClientOptions = orig })
	return f
}

func (f *fakeREST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	// Cloud Storage: GET and PUT b/<bucket>/iam.
	case strings.HasPrefix(path, "b/") && strings.HasSuffix(path, "/iam"):
		resource := strings.TrimSuffix(path, "/iam")
		if r.Method == http.MethodPut {
			f.write(w, r, resource, false)
			return
		}
		version := f.requested(r, resource, "optionsRequestedPolicyVersion")
		f.mu.Lock()
		stored := f.policy(resource)
		f.mu.Unlock()
		// Cloud Storage refuses a version 1 read of a policy with
		// conditions rather than returning a placeholder view.
		if hasConditions(stored) && version < 3 {
			http.Error(w, `{"error":{"code":400,"message":"IAM policy contains conditions, request version 3"}}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, policyToJSON(policyAtVersion(stored, version)))

	// IAM: POST v1/<resource>:getIamPolicy and :setIamPolicy.
	case strings.HasSuffix(path, ":getIamPolicy"):
		resource := strings.TrimSuffix(strings.TrimPrefix(path, "v1/"), ":getIamPolicy")
		version := f.requested(r, resource, "options.requestedPolicyVersion")
		f.mu.Lock()
		p := policyAtVersion(f.policy(resource), version)
		f.mu.Unlock()
		writeJSON(w, policyToJSON(p))
	case strings.HasSuffix(path, ":setIamPolicy"):
		resource := strings.TrimSuffix(strings.TrimPrefix(path, "v1/"), ":setIamPolicy")
		f.write(w, r, resource, true)

	// IAM: GET v1/projects/<project>/serviceAccounts/<email>.
	case r.Method == http.MethodGet && strings.HasPrefix(path, "v1/projects/") && strings.Contains(path, "/serviceAccounts/"):
		writeJSON(w, map[string]string{"name": strings.TrimPrefix(path, "v1/")})

	default:
		http.Error(w, `{"error":{"code":404,"message":"fakeREST: unhandled `+r.Method+" "+path+`"}}`, http.StatusNotFound)
	}
}

// policy returns the stored policy for resource, creating an empty one.
// Callers hold f.mu.
func (f *fakeREST) policy(resource string) *iampb.Policy {
	p, ok := f.policies[resource]
	if !ok {
		p = &iampb.Policy{Etag: []byte("etag-1")}
		f.policies[resource] = p
	}
	return p
}

// requested records and returns the policy version named by query parameter
// param, or 0 when it is absent.
func (f *fakeREST) requested(r *http.Request, resource, param string) int32 {
	v, _ := strconv.Atoi(r.URL.Query().Get(param))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestedVersions[resource] = append(f.requestedVersions[resource], int32(v))
	return int32(v)
}

// write stores and records the policy in the request body. IAM wraps it in a
// {"policy": ...} request; Cloud Storage sends the policy itself.
func (f *fakeREST) write(w http.ResponseWriter, r *http.Request, resource string, wrapped bool) {
	var body struct {
		Policy jsonPolicy `json:"policy"`
	}
	var err error
	if wrapped {
		err = json.NewDecoder(r.Body).Decode(&body)
	} else {
		err = json.NewDecoder(r.Body).Decode(&body.Policy)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := policyFromJSON(body.Policy)
	f.mu.Lock()
	f.written[resource] = append(f.written[resource], p)
	f.policies[resource] = proto.Clone(p).(*iampb.Policy)
	f.mu.Unlock()
	writeJSON(w, body.Policy)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// jsonPolicy is the policy shape shared by the Cloud Storage and IAM JSON
// APIs.
type jsonPolicy struct {
	Bindings []jsonBinding `json:"bindings,omitempty"`
	Etag     string        `json:"etag,omitempty"`
	Version  int32         `json:"version,omitempty"`
}

type jsonBinding struct {
	Role      string    `json:"role"`
	Members   []string  `json:"members"`
	Condition *jsonExpr `json:"condition,omitempty"`
}

type jsonExpr struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression,omitempty"`
}

func policyToJSON(p *iampb.Policy) jsonPolicy {
	jp := jsonPolicy{Etag: string(p.GetEtag()), Version: p.GetVersion()}
	for _, b := range p.GetBindings() {
		jb := jsonBinding{Role: b.GetRole(), Members: b.GetMembers()}
		if c := b.GetCondition(); c != nil {
			jb.Condition = &jsonExpr{Title: c.GetTitle(), Description: c.GetDescription(), Expression: c.GetExpression()}
		}
		jp.Bindings = append(jp.Bindings, jb)
	}
	return jp
}

func policyFromJSON(jp jsonPolicy) *iampb.Policy {
	p := &iampb.Policy{Etag: []byte(jp.Etag), Version: jp.Version}
	for _, jb := range jp.Bindings {
		b := &iampb.Binding{Role: jb.Role, Members: jb.Members}
		if c := jb.Condition; c != nil {
			b.Condition = &expr.Expr{Title: c.Title, Description: c.Description, Expression: c.Expression}
		}
		p.Bindings = append(p.Bindings, b)
	}
	return p
}
