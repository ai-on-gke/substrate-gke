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
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	artifactregistry "google.golang.org/api/artifactregistry/v1"
)

// imagePullRole lets a member pull from one Artifact Registry repository.
// setup-gcp grants it per repository, never on the project.
const imagePullRole = "roles/artifactregistry.reader"

// Two identities pull images, and from different repositories:
//
//   - The nodes (kubelet, as the node pool's service account) pull Substrate's
//     own images: the control plane and the worker pods.
//   - atelet, as its Workload Identity principal, pulls actor images, which are
//     the user's workloads. Keeping them in a repository of their own means the
//     nodes never get read access to them.
//
// atelet also needs the Substrate image repository when actors run images
// built with Substrate, such as the example actors a source build pushes.

// grantNodeImagePull lets each node service account pull Substrate's images.
func grantNodeImagePull(ctx context.Context, cfg *Config, accounts []string) error {
	members := make([]string, 0, len(accounts))
	for _, sa := range accounts {
		members = append(members, "serviceAccount:"+sa)
	}
	return grantImagePull(ctx, cfg, "the nodes", members)
}

// grantAteletImagePull lets atelet pull actor images: from the Substrate
// image repository, like the nodes, and from the user's own actor image
// repositories, for which it prints the command.
func grantAteletImagePull(ctx context.Context, cfg *Config) error {
	member := workloadIdentityPrincipal(cfg, "atelet")
	if err := grantImagePull(ctx, cfg, "atelet", []string{member}); err != nil {
		return err
	}
	slog.Info("atelet pulls actor images; grant it read access to each repository your actor images come from",
		slog.String("command", imagePullGrantCommand("", member)))
	return nil
}

// grantImagePull grants members imagePullRole on the Substrate image
// repository.
//
// When bootstrap created the repository it grants the role itself. Otherwise
// the registry is the user's, so it prints the command that grants it, filled
// in from imageRepo() where it can parse it. A registry outside Artifact
// Registry (ghcr.io, for example) needs no grant, so nothing is printed.
func grantImagePull(ctx context.Context, cfg *Config, who string, members []string) error {
	if cfg.CreateArtifactRepository {
		return grantRepositoryReader(ctx, cfg, who, members)
	}
	repo := imageRepo()
	if repo != "" && !isArtifactRegistryHost(repo) {
		slog.Info("Image registry is not Artifact Registry; no image pull grant needed", slog.String("who", who), slog.String("registry", repo))
		return nil
	}
	for _, member := range members {
		slog.Warn("Grant "+who+" read access to the Substrate image repository",
			slog.String("member", member),
			slog.String("command", imagePullGrantCommand(repo, member)))
	}
	return nil
}

// imageRepo is the registry the cluster pulls Substrate images from:
// ATE_IMAGE_REPO for pre-built images, which ate-setup also reads, or
// KO_DOCKER_REPO for a source build.
func imageRepo() string {
	if repo := strings.TrimSpace(os.Getenv("ATE_IMAGE_REPO")); repo != "" {
		return repo
	}
	return strings.TrimSpace(os.Getenv("KO_DOCKER_REPO"))
}

// grantRepositoryReader grants members imagePullRole on the repository
// bootstrap created.
func grantRepositoryReader(ctx context.Context, cfg *Config, who string, members []string) error {
	svc, err := artifactregistry.NewService(ctx, restClientOptions...)
	if err != nil {
		return fmt.Errorf("create artifact registry client: %w", err)
	}
	resource := fmt.Sprintf("projects/%s/locations/%s/repositories/%s", cfg.ProjectID, cfg.Region, cfg.ArtifactRegistryRepository)
	repos := svc.Projects.Locations.Repositories
	return retryUntilPropagated(ctx, "Granting "+who+" read access to the artifact repository", strings.Join(members, ","), func() error {
		policy, err := repos.GetIamPolicy(resource).OptionsRequestedPolicyVersion(conditionalPolicyVersion).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("get artifact repository %s iam policy: %w", resource, err)
		}
		changed := false
		for _, member := range members {
			if addRepositoryBinding(policy, imagePullRole, member) {
				changed = true
			}
		}
		if !changed {
			slog.Info("Artifact repository IAM policy already lets "+who+" pull images", slog.String("repository", resource))
			return nil
		}
		slog.Info("Granting "+who+" read access to the artifact repository", slog.String("repository", resource), slog.String("members", strings.Join(members, ",")))
		policy.Version = conditionalPolicyVersion
		if _, err := repos.SetIamPolicy(resource, &artifactregistry.SetIamPolicyRequest{Policy: policy}).Context(ctx).Do(); err != nil {
			return fmt.Errorf("set artifact repository %s iam policy: %w", resource, err)
		}
		return nil
	})
}

// addRepositoryBinding adds member to role in policy, and reports whether it
// added anything. A conditional binding is not the grant, and the member is
// not added under its condition (see addBinding).
func addRepositoryBinding(policy *artifactregistry.Policy, role, member string) bool {
	for _, b := range policy.Bindings {
		if b.Role != role || b.Condition != nil {
			continue
		}
		if slices.Contains(b.Members, member) {
			return false
		}
		b.Members = append(b.Members, member)
		return true
	}
	policy.Bindings = append(policy.Bindings, &artifactregistry.Binding{Role: role, Members: []string{member}})
	return true
}

// gcrLocations maps each gcr.io host to the Artifact Registry multi-region
// that serves it. Artifact Registry names a gcr.io repository after its host.
var gcrLocations = map[string]string{
	"gcr.io":      "us",
	"us.gcr.io":   "us",
	"eu.gcr.io":   "europe",
	"asia.gcr.io": "asia",
}

// isArtifactRegistryHost reports whether repo is served by Artifact Registry.
func isArtifactRegistryHost(repo string) bool {
	host, _, _ := strings.Cut(repo, "/")
	_, gcr := gcrLocations[host]
	return gcr || strings.HasSuffix(host, "-docker.pkg.dev")
}

// parseArtifactRegistryRepo splits an image registry path such as
// us-central1-docker.pkg.dev/PROJECT/REPO[/...] or gcr.io/PROJECT[/...] into
// the project, location and repository an IAM grant names.
func parseArtifactRegistryRepo(repo string) (project, location, repository string, ok bool) {
	parts := strings.Split(repo, "/")
	if len(parts) < 2 || parts[1] == "" {
		return "", "", "", false
	}
	if loc, gcr := gcrLocations[parts[0]]; gcr {
		return parts[1], loc, parts[0], true
	}
	loc, found := strings.CutSuffix(parts[0], "-docker.pkg.dev")
	if !found || loc == "" || len(parts) < 3 || parts[2] == "" {
		return "", "", "", false
	}
	return parts[1], loc, parts[2], true
}

// imagePullGrantCommand is the gcloud command that grants member
// imagePullRole on repo, with placeholders for whatever repo does not say.
func imagePullGrantCommand(repo, member string) string {
	project, location, repository, ok := parseArtifactRegistryRepo(repo)
	if !ok {
		project, location, repository = "<project>", "<location>", "<repository>"
	}
	return fmt.Sprintf("gcloud artifacts repositories add-iam-policy-binding %s --project=%s --location=%s --member=%s --role=%s",
		repository, project, location, member, imagePullRole)
}
