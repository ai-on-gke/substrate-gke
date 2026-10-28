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

package snapshot

// The GKE plugins are published from their own repositories under the
// agent-substrate-plugins GitHub organization, with their own commits,
// versions and images, separate from Substrate's. The first is the GKE
// credential provider: the Google Secret Manager egress credential provider
// in agent-substrate-plugins/credprovider-gcp-secret-manager.
//
// The install does not deploy it yet: the wizard records the plugin images it
// is given and nothing reads them. Two things stand in the way:
//
//   - The repository publishes no images yet: it has no release tags and no
//     publishing workflow, so PluginReleaseVersion is empty.
//   - It needs a Substrate newer than v0.6.0, whose egress gateway calls the
//     substrate.credprovider.v1 API (its README's prerequisites); Commit is
//     v0.6.0.
//
// Deploying it means applying the repository's config/ kustomization at
// PluginCommit with the image resolved from the plugin registry and tag,
// granting its Kubernetes service account (ate-system/gsm-credential-provider)
// roles/secretmanager.secretAccessor, and passing ate-setup
// ATE_CREDENTIAL_PROVIDER={"name":"secretmanager.googleapis.com","address":"gsm-credential-provider.ate-system.svc:50051"}
// in place of defaultCredentialProvider, the bundled Kubernetes Secrets
// provider. The repository's README is the contract for all three.
const (
	// PluginSourceOrg is the GitHub organization the plugin repositories live
	// under.
	PluginSourceOrg = "https://github.com/agent-substrate-plugins"

	// PluginRepoURL is the GKE credential provider's repository, the source
	// tree the wizard offers for the plugin images.
	PluginRepoURL = PluginSourceOrg + "/credprovider-gcp-secret-manager"

	// PluginCommit is the commit of PluginRepoURL the wizard offers as the one
	// the plugin images were built from: the head of its main branch, since it
	// has no release tag yet. Bump it, and set PluginReleaseVersion, when the
	// repository publishes a release.
	PluginCommit = "55a0595b22c506838b761ecb8001a939fd9ecfbd"

	// PluginRegistry is the registry the wizard offers for the plugin images.
	// Nothing is published there yet. The path follows Substrate's release
	// registry, ghcr.io/<organization>/<repository>, which holds one image per
	// binary named by ko's --base-import-paths: the provider's image would be
	// PluginRegistry + "/gsm-credential-provider". Confirm it against the
	// repository's publishing workflow once it has one.
	PluginRegistry = "ghcr.io/agent-substrate-plugins/credprovider-gcp-secret-manager"

	// PluginReleaseVersion is the tag the wizard offers for the plugin images:
	// the release tag naming PluginCommit, whose images PluginRegistry holds.
	// It is empty while the repository publishes none, and then the wizard
	// asks for the tag.
	PluginReleaseVersion = ""
)
