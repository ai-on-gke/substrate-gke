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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/proto"
)

func iamConfig() *Config {
	return &Config{ProjectID: "acme", ProjectNumber: "42"}
}

// conditionalBinding is a binding a project owner added outside setup-gcp,
// granted only until a deadline.
func conditionalBinding() *iampb.Binding {
	return &iampb.Binding{
		Role:    "roles/storage.objectViewer",
		Members: []string{"user:auditor@example.com"},
		Condition: &expr.Expr{
			Title:      "until-2027",
			Expression: `request.time < timestamp("2027-01-01T00:00:00Z")`,
		},
	}
}

// Review question: do the project IAM grants (bootstrap steps 4 and 5) break
// on a project that already holds a conditional role binding? IAM's
// documented behaviour is that a GetIamPolicy below version 3 returns the
// policy with each conditional role renamed "<role>_withcond_<hash>" and its
// condition removed, so a read-modify-write at version 1 cannot preserve the
// binding. The grants must request version 3 and write back the conditional
// binding unchanged.
func TestProjectGrantsPreserveConditionalBindings(t *testing.T) {
	grants := []struct {
		name  string
		grant func(context.Context, *Config) error
	}{
		{"GKE node grant", grantGkeNodePermissions},
		{"atelet grant", grantAteletPermissions},
		// Not raised in review: `create cloudsql` reads and writes the same
		// project policy the same way.
		{"Cloud SQL grant", grantCloudSQLProjectRoles},
	}
	for _, g := range grants {
		t.Run(g.name, func(t *testing.T) {
			f := newFakeGCP(t)
			f.policy.Bindings = []*iampb.Binding{conditionalBinding()}
			f.policy.Version = 3

			if err := g.grant(context.Background(), iamConfig()); err != nil {
				t.Fatal(err)
			}

			for _, v := range f.requestedVersions {
				if v < 3 {
					t.Errorf("GetIamPolicy requested policy version %d, want 3", v)
				}
			}
			if len(f.written) != 1 {
				t.Fatalf("SetIamPolicy called %d times, want 1", len(f.written))
			}
			written := f.written[0]
			if written.GetVersion() != 3 {
				t.Errorf("written policy has version %d, want 3", written.GetVersion())
			}
			kept := false
			for _, b := range written.GetBindings() {
				if strings.Contains(b.GetRole(), "_withcond_") {
					t.Errorf("written policy carries the version 1 placeholder role %q", b.GetRole())
				}
				if proto.Equal(b, conditionalBinding()) {
					kept = true
				}
			}
			if !kept {
				t.Errorf("written policy lost the conditional binding; bindings: %v", written.GetBindings())
			}
		})
	}
}

// captureLogs routes slog's default logger to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// Review question: does the atelet grant (bootstrap step 5) log under the
// GKE node and API server labels? Its log lines appear in the installer's
// step output, so they must name atelet in both the skip and the update path.
func TestAteletGrantLogsUnderItsOwnName(t *testing.T) {
	member := "principal://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/acme.svc.id.goog/subject/ns/ate-system/sa/atelet"
	tests := []struct {
		name     string
		bindings []*iampb.Binding
	}{
		{"update path", nil},
		{"skip path", []*iampb.Binding{
			{Role: "roles/storage.objectAdmin", Members: []string{member}},
			{Role: "roles/artifactregistry.reader", Members: []string{member}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGCP(t)
			f.policy.Bindings = tt.bindings
			logs := captureLogs(t)

			if err := grantAteletPermissions(context.Background(), iamConfig()); err != nil {
				t.Fatal(err)
			}

			out := logs.String()
			if !strings.Contains(out, "atelet") {
				t.Errorf("atelet grant logs do not mention atelet:\n%s", out)
			}
			for _, wrong := range []string{"GKE node", "api server"} {
				if strings.Contains(out, wrong) {
					t.Errorf("atelet grant logs %q:\n%s", wrong, out)
				}
			}
		})
	}
}
