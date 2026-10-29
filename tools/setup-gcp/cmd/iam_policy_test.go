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
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/spf13/cobra"
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

			checkConditionalBindingKept(t, f.requestedVersions, f.written, conditionalBinding())
		})
	}
}

// checkConditionalBindingKept asserts that a grant read the policy at
// version 3 and wrote it back once, at version 3, with kept unchanged.
func checkConditionalBindingKept(t *testing.T, requested []int32, written []*iampb.Policy, kept *iampb.Binding) {
	t.Helper()
	for _, v := range requested {
		if v < 3 {
			t.Errorf("policy read requested version %d, want 3", v)
		}
	}
	if len(written) != 1 {
		t.Fatalf("policy written %d times, want 1", len(written))
	}
	if written[0].GetVersion() != 3 {
		t.Errorf("written policy has version %d, want 3", written[0].GetVersion())
	}
	found := false
	for _, b := range written[0].GetBindings() {
		if strings.Contains(b.GetRole(), "_withcond_") {
			t.Errorf("written policy carries the version 1 placeholder role %q", b.GetRole())
		}
		if proto.Equal(b, kept) {
			found = true
		}
	}
	if !found {
		t.Errorf("written policy lost the conditional binding; bindings: %v", written[0].GetBindings())
	}
}

// hasPlainGrant reports whether policy grants role to member in a binding
// without a condition.
func hasPlainGrant(policy *iampb.Policy, role, member string) bool {
	for _, b := range policy.GetBindings() {
		if b.GetCondition() == nil && b.GetRole() == role && slices.Contains(b.GetMembers(), member) {
			return true
		}
	}
	return false
}

// withCondition returns a binding of role to member under the deadline
// condition conditionalBinding uses.
func withCondition(role, member string) *iampb.Binding {
	b := conditionalBinding()
	b.Role, b.Members = role, []string{member}
	return b
}

// Review question: does the snapshot bucket grant (bootstrap step 6) keep a
// conditional binding on the bucket? It read and wrote the bucket policy at
// version 1, which Cloud Storage refuses for a policy with conditions. A
// conditional grant of the same role to the same member must also not count
// as the grant, or the member is left with access only while it holds.
func TestBucketGrantPreservesConditionalBindings(t *testing.T) {
	f := newFakeREST(t)
	cfg := iamConfig()
	cfg.BucketName = "acme-snapshots"
	apiServer := "principal://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/acme.svc.id.goog/subject/ns/ate-system/sa/ate-api-server"
	existing := withCondition("roles/storage.objectAdmin", apiServer)
	f.policies["b/acme-snapshots"] = &iampb.Policy{Etag: []byte("etag-1"), Version: 3, Bindings: []*iampb.Binding{existing}}

	if err := createIamPolicyBindings(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	resource := "b/acme-snapshots"
	checkConditionalBindingKept(t, f.requestedVersions[resource], f.written[resource], existing)
	if written := f.written[resource]; len(written) == 1 && !hasPlainGrant(written[0], "roles/storage.objectAdmin", apiServer) {
		t.Errorf("ate-api-server's conditional objectAdmin binding was taken as the grant; bindings: %v", written[0].GetBindings())
	}
}

// Review question: does the Cloud SQL service account's Workload Identity
// binding keep a conditional binding on the service account? It read the
// policy without requesting version 3 and wrote it back as version 1, and it
// took a conditional workloadIdentityUser binding for the KSA as the grant.
func TestCloudSQLGSAPreservesConditionalBindings(t *testing.T) {
	f := newFakeREST(t)
	cfg := iamConfig()
	cfg.CloudSQLGSAName = "ate-api-server"
	resource := "projects/acme/serviceAccounts/" + cloudSQLGSAEmail(cfg)
	member := workloadIdentityMember(cfg.ProjectID)
	const wiRole = "roles/iam.workloadIdentityUser"
	existing := withCondition(wiRole, member)
	f.policies[resource] = &iampb.Policy{Etag: []byte("etag-1"), Version: 3, Bindings: []*iampb.Binding{existing}}

	if err := createCloudSQLGSA(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	checkConditionalBindingKept(t, f.requestedVersions[resource], f.written[resource], existing)
	if written := f.written[resource]; len(written) == 1 && !hasPlainGrant(written[0], wiRole, member) {
		t.Errorf("the conditional workloadIdentityUser binding was taken as the grant; bindings: %v", written[0].GetBindings())
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

// atelet gets artifactregistry.reader on the project and no storage role:
// snapshot transfers run on per-actor credentials, and atelet reads micro-VM
// assets through its asset bucket grant only.
func TestAteletProjectGrantHasNoStorageRole(t *testing.T) {
	member := "principal://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/acme.svc.id.goog/subject/ns/ate-system/sa/atelet"
	f := newFakeGCP(t)

	if err := grantAteletPermissions(context.Background(), iamConfig()); err != nil {
		t.Fatal(err)
	}

	if len(f.written) != 1 {
		t.Fatalf("policy written %d times, want 1", len(f.written))
	}
	if !hasPlainGrant(f.written[0], "roles/artifactregistry.reader", member) {
		t.Errorf("atelet lacks artifactregistry.reader; bindings: %v", f.written[0].GetBindings())
	}
	for _, b := range f.written[0].GetBindings() {
		if strings.HasPrefix(b.GetRole(), "roles/storage.") && slices.Contains(b.GetMembers(), member) {
			t.Errorf("atelet has project-level %s", b.GetRole())
		}
	}
}

// A set ATELET_SNAPSHOT_ACCESS is ignored with a warning, whatever its value.
func TestRemovedAteletSnapshotAccessEnvWarns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      bool
		wantWarn bool
	}{
		{"unset", false, false},
		{"set", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("ATELET_SNAPSHOT_ACCESS", "true")
			}
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			warnRemovedAteletSnapshotAccessEnv()

			if got := strings.Contains(buf.String(), "Ignoring ATELET_SNAPSHOT_ACCESS"); got != tc.wantWarn {
				t.Errorf("warned = %t, want %t (log: %q)", got, tc.wantWarn, buf.String())
			}
		})
	}
}

// --atelet-snapshot-access is removed but still parses, so scripts that pass
// it keep working.
func TestRemovedAteletSnapshotAccessFlagStillParses(t *testing.T) {
	for _, cmd := range []*cobra.Command{bootstrapCmd, iamCmd} {
		if err := cmd.ParseFlags([]string{"--atelet-snapshot-access=true"}); err != nil {
			t.Errorf("%s: ParseFlags(--atelet-snapshot-access=true) = %v", cmd.Name(), err)
		}
		if f := cmd.Flags().Lookup("atelet-snapshot-access"); f == nil || !f.Hidden {
			t.Errorf("%s: --atelet-snapshot-access should be registered and hidden", cmd.Name())
		}
	}
}
