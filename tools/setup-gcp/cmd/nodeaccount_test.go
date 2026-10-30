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
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/iam/apiv1/iampb"
)

func nodeConfig() *Config {
	return &Config{ProjectID: "acme", ProjectNumber: "42", Region: "us-west1", NodeGSAName: DefaultNodeGSAName, ArtifactRegistryRepository: "ate-images"}
}

const nodeSA = "ate-gke-node@acme.iam.gserviceaccount.com"

func TestBuildCreateClusterRequest_NodeHardening(t *testing.T) {
	req := buildCreateClusterRequest("projects/acme/locations/us-west1-c", nodeConfig())
	nc := req.GetCluster().GetNodePools()[0].GetConfig()
	if got := nc.GetServiceAccount(); got != nodeSA {
		t.Errorf("node service account = %q, want %q", got, nodeSA)
	}
	if got := nc.GetOauthScopes(); !slices.Equal(got, []string{"https://www.googleapis.com/auth/cloud-platform"}) {
		t.Errorf("node scopes = %v, want cloud-platform only", got)
	}
	if !nc.GetShieldedInstanceConfig().GetEnableSecureBoot() || !nc.GetShieldedInstanceConfig().GetEnableIntegrityMonitoring() {
		t.Errorf("shielded instance config = %v, want Secure Boot and integrity monitoring", nc.GetShieldedInstanceConfig())
	}
}

func TestNodeGSANameDefault(t *testing.T) {
	c := bootstrapConfig(t)
	if c.NodeGSAName != DefaultNodeGSAName {
		t.Errorf("bootstrap NodeGSAName = %q, want %q", c.NodeGSAName, DefaultNodeGSAName)
	}
	for _, cmd := range []string{"cluster", "iam"} {
		c, _, err := createCmd.Find([]string{cmd})
		if err != nil {
			t.Fatal(err)
		}
		if f := c.Flags().Lookup("node-service-account-name"); f == nil || f.DefValue != DefaultNodeGSAName {
			t.Errorf("create %s --node-service-account-name = %v, want default %q", cmd, f, DefaultNodeGSAName)
		}
	}
}

// The node service account gets the node role on the project and no image
// pull role there; a second run changes nothing.
func TestEnsureNodeServiceAccountGrantsOnlyTheNodeRole(t *testing.T) {
	f := newFakeGCP(t)
	newFakeREST(t)
	f.policy.Bindings = []*iampb.Binding{conditionalBinding()}
	f.policy.Version = 3

	if err := ensureNodeServiceAccount(context.Background(), nodeConfig()); err != nil {
		t.Fatal(err)
	}
	checkConditionalBindingKept(t, f.requestedVersions, f.written, conditionalBinding())
	written := f.written[0]
	if !hasPlainGrant(written, nodeServiceAccountRole, "serviceAccount:"+nodeSA) {
		t.Errorf("node service account lacks %s: %v", nodeServiceAccountRole, written.GetBindings())
	}
	for _, b := range written.GetBindings() {
		if b.GetRole() == imagePullRole {
			t.Errorf("project policy grants %s: %v", imagePullRole, b)
		}
	}

	if err := ensureNodeServiceAccount(context.Background(), nodeConfig()); err != nil {
		t.Fatal(err)
	}
	if len(f.written) != 1 {
		t.Errorf("second run wrote the project policy again: %d writes", len(f.written))
	}
}

func TestClusterNodeServiceAccounts(t *testing.T) {
	pool := func(sa string) *containerpb.NodePool {
		return &containerpb.NodePool{Config: &containerpb.NodeConfig{ServiceAccount: sa}}
	}
	cluster := &containerpb.Cluster{NodePools: []*containerpb.NodePool{
		pool(nodeSA), pool("default"), pool(""), pool(nodeSA), {},
	}}
	got := clusterNodeServiceAccounts(cluster, nodeConfig())
	want := []string{"42-compute@developer.gserviceaccount.com", nodeSA}
	if !slices.Equal(got, want) {
		t.Errorf("clusterNodeServiceAccounts() = %v, want %v", got, want)
	}
}

// With a repository bootstrap created, every node service account gets read
// access on that repository, and a second run changes nothing.
func TestGrantNodeImagePullOnCreatedRepository(t *testing.T) {
	f := newFakeREST(t)
	c := nodeConfig()
	c.CreateArtifactRepository = true
	accounts := []string{"42-compute@developer.gserviceaccount.com", nodeSA}
	const resource = "projects/acme/locations/us-west1/repositories/ate-images"

	for range 2 {
		if err := grantNodeImagePull(context.Background(), c, accounts); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.written[resource]) != 1 {
		t.Fatalf("repository policy written %d times, want 1", len(f.written[resource]))
	}
	for _, sa := range accounts {
		if !hasPlainGrant(f.written[resource][0], imagePullRole, "serviceAccount:"+sa) {
			t.Errorf("%s lacks %s on the repository: %v", sa, imagePullRole, f.written[resource][0].GetBindings())
		}
	}
	for _, v := range f.requestedVersions[resource] {
		if v != conditionalPolicyVersion {
			t.Errorf("repository policy read at version %d, want %d", v, conditionalPolicyVersion)
		}
	}
}

// For an Artifact Registry repository bootstrap didn't create, the node
// service account is granted read access on that repository. Nothing is
// granted for other registries, and only the command is printed when the
// repository can't be named.
func TestGrantNodeImagePullOnUserRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, ateImageRepo, koDockerRepo, resource, printed string
	}{
		{"pre-built on Artifact Registry", "us-docker.pkg.dev/other/images", "", "projects/other/locations/us/repositories/images", ""},
		{"source build on gcr.io", "", "gcr.io/acme/ate", "projects/acme/locations/us/repositories/gcr.io", ""},
		{"pre-built wins over KO_DOCKER_REPO", "eu.gcr.io/other", "gcr.io/acme", "projects/other/locations/europe/repositories/eu.gcr.io", ""},
		{"unknown registry", "", "", "", "add-iam-policy-binding <repository> --project=<project> --location=<location>"},
		{"not Artifact Registry", "ghcr.io/agent-substrate", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeREST(t)
			t.Setenv("ATE_IMAGE_REPO", tc.ateImageRepo)
			t.Setenv("KO_DOCKER_REPO", tc.koDockerRepo)
			logs := captureLogs(t)

			if err := grantNodeImagePull(context.Background(), nodeConfig(), []string{nodeSA}); err != nil {
				t.Fatal(err)
			}
			if tc.resource == "" {
				if len(f.written) != 0 {
					t.Errorf("granted on %v, want no grant", f.written)
				}
			} else if w := f.written[tc.resource]; len(w) != 1 || !hasPlainGrant(w[0], imagePullRole, "serviceAccount:"+nodeSA) {
				t.Errorf("%s lacks %s on %s; written: %v", nodeSA, imagePullRole, tc.resource, f.written)
			}
			out := logs.String()
			if tc.printed == "" {
				if strings.Contains(out, "add-iam-policy-binding") {
					t.Errorf("printed a grant command:\n%s", out)
				}
			} else if !strings.Contains(out, tc.printed) {
				t.Errorf("logs do not contain %q:\n%s", tc.printed, out)
			}
		})
	}
}

// When the grant on the user's repository is denied or the repository is not
// found, bootstrap prints the command and carries on, without retrying. Any
// other error fails it.
func TestGrantNodeImagePullOnUserRegistryFailure(t *testing.T) {
	orig := nodeAccountRetryDelay
	nodeAccountRetryDelay = 0
	t.Cleanup(func() { nodeAccountRetryDelay = orig })
	const resource = "projects/other/locations/us/repositories/images"
	for _, tc := range []struct {
		name    string
		code    int
		wantErr bool
	}{
		{"permission denied", http.StatusForbidden, false},
		{"repository not found", http.StatusNotFound, false},
		{"server error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeREST(t)
			f.fail[resource] = tc.code
			t.Setenv("ATE_IMAGE_REPO", "us-docker.pkg.dev/other/images")
			logs := captureLogs(t)

			err := grantNodeImagePull(context.Background(), nodeConfig(), []string{nodeSA})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if f.calls[resource] != 1 {
				t.Errorf("repository IAM called %d times, want 1 (no retry)", f.calls[resource])
			}
			want := "add-iam-policy-binding images --project=other --location=us --member=serviceAccount:" + nodeSA
			if !strings.Contains(logs.String(), want) {
				t.Errorf("logs do not contain %q:\n%s", want, logs.String())
			}
		})
	}
}

func TestParseArtifactRegistryRepo(t *testing.T) {
	for _, tc := range []struct {
		repo, project, location, repository string
		ok                                  bool
	}{
		{"gcr.io/my-project", "my-project", "us", "gcr.io", true},
		{"eu.gcr.io/my-project/image", "my-project", "europe", "eu.gcr.io", true},
		{"asia.gcr.io/my-project", "my-project", "asia", "asia.gcr.io", true},
		{"us-central1-docker.pkg.dev/my-project/my-repo", "my-project", "us-central1", "my-repo", true},
		{"us-central1-docker.pkg.dev/my-project/my-repo/sub", "my-project", "us-central1", "my-repo", true},
		{"us-central1-docker.pkg.dev/my-project", "", "", "", false},
		{"-docker.pkg.dev/my-project/my-repo", "", "", "", false},
		{"ghcr.io/agent-substrate/substrate", "", "", "", false},
		{"gcr.io", "", "", "", false},
		{"", "", "", "", false},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			p, l, r, ok := parseArtifactRegistryRepo(tc.repo)
			if p != tc.project || l != tc.location || r != tc.repository || ok != tc.ok {
				t.Errorf("parseArtifactRegistryRepo(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tc.repo, p, l, r, ok, tc.project, tc.location, tc.repository, tc.ok)
			}
		})
	}
}
