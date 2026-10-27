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

// The GKE plugins, starting with the GCP egress credential injection plugin
// (the GKE credential provider), are to be published from their own
// repositories under the agent-substrate-plugins GitHub organization, with
// their own commits, versions and images, separate from Substrate's. None of
// those repositories exists yet, and the install does not deploy any plugin:
// the wizard records the plugin images it is given and nothing reads them.
//
// When the credential provider exists, the install deploys it from these
// images and passes its address to ate-setup as
// ATE_CREDENTIAL_PROVIDER={"name":...,"address":...} in place of
// defaultCredentialProvider, the bundled Kubernetes Secrets provider.
const (
	// PluginSourceOrg is the GitHub organization the plugin repositories are
	// to live under. The wizard offers it as the start of the plugin source
	// tree's URL, since no repository name is fixed yet.
	PluginSourceOrg = "https://github.com/agent-substrate-plugins"

	// PluginRegistry is the registry path the wizard offers for plugin images:
	// GHCR's namespace for that organization, where its repositories' images
	// are expected to be published.
	PluginRegistry = "ghcr.io/agent-substrate-plugins"
)
