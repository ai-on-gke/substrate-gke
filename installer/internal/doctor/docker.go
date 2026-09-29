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

package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// DefaultRegistryHost is the host of the registry a build from source pushes
// to when none is given, read off state's default so the two cannot drift.
var DefaultRegistryHost = registryHost((&state.Setup{}).DefaultKoDockerRepo())

// DockerChecks returns the probes behind ate-setup's `docker buildx build
// --push` of the envoy-dataplane image to registry, which Substrate 0.2 added.
//
// The doctor runs them against DefaultRegistryHost, before the images step,
// so they are SourceOnly: a user installing pre-built images may skip them.
// The project step runs them again against the real registry whenever the
// install will build with docker (snapshot.Builder.BuildsWithDocker), and
// there they cannot be skipped. The build runs inside the control-plane
// deploy, after the bundle is applied, so any one of them failing there
// leaves a half-installed cluster.
func DockerChecks(registry string) []Check {
	host := registryHost(registry)
	return []Check{
		{
			Key: "docker", Name: "Docker daemon", Fatal: true, SourceOnly: true,
			Run: func(ctx context.Context) Result {
				if _, err := exec.LookPath("docker"); err != nil {
					return Result{Fail, "docker is not installed; a build of Substrate 0.2+ builds its envoy-dataplane image with docker buildx",
						"https://docs.docker.com/engine/install/"}
				}
				out, err := output(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
				if err != nil || out == "" {
					return Result{Fail, "docker is installed, but its daemon is not running or you cannot reach it",
						"sudo systemctl start docker   # or add yourself to the docker group"}
				}
				return Result{Pass, "Docker Engine " + out, ""}
			},
		},
		{
			Key: "buildx", Name: "Docker buildx", Fatal: true, SourceOnly: true,
			Run: func(ctx context.Context) Result {
				out, err := output(ctx, "docker", "buildx", "version")
				if err != nil {
					return Result{Fail, "docker buildx is not available; ate-setup builds envoy-dataplane with it",
						"https://docs.docker.com/go/buildx/"}
				}
				return Result{Pass, strings.SplitN(out, "\n", 2)[0], ""}
			},
		},
		{
			Key: "docker-auth", Name: "Docker credentials for " + host, Fatal: true, SourceOnly: true,
			Run: func(ctx context.Context) Result {
				return dockerAuth(dockerConfigPath(), host)
			},
		},
	}
}

// registryHost is the host part of an image repository such as
// gcr.io/acme/ate-images, which is what docker keys credentials by.
func registryHost(registry string) string {
	registry = strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://")
	host, _, _ := strings.Cut(registry, "/")
	return host
}

// dockerConfigPath is the file docker reads credentials from, honouring
// DOCKER_CONFIG the way the docker CLI does.
func dockerConfigPath() string {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".docker")
	}
	return filepath.Join(dir, "config.json")
}

// dockerConfig is the part of ~/.docker/config.json that decides how docker
// authenticates to a registry.
type dockerConfig struct {
	CredHelpers map[string]string          `json:"credHelpers"`
	Auths       map[string]json.RawMessage `json:"auths"`
}

// dockerAuth reports whether docker has a way to authenticate to host. ko
// finds gcloud's credentials on its own; docker only looks in its config,
// and without an entry there it pushes anonymously and gets a 403 from gcr.io
// and Artifact Registry alike.
//
// Either kind of entry counts: a credHelper for the host, which is what
// `gcloud auth configure-docker` writes, or an auths entry, which is what
// `docker login` leaves whether or not a credsStore holds the secret.
func dockerAuth(path, host string) Result {
	fix := dockerLoginFix(host)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Result{Fail, fmt.Sprintf("docker has no credentials for %s (no %s); ko pushes with gcloud's, but the envoy-dataplane push would be refused", host, path), fix}
	}
	if err != nil {
		return Result{Fail, fmt.Sprintf("cannot read %s: %v", path, err), fix}
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Result{Fail, fmt.Sprintf("%s is not valid JSON: %v", path, err), fix}
	}
	if helper, ok := cfg.CredHelpers[host]; ok {
		bin := "docker-credential-" + helper
		if _, err := exec.LookPath(bin); err != nil {
			return Result{Fail, fmt.Sprintf("%s uses the %q credential helper for %s, but %s is not on PATH", path, helper, host, bin),
				"put " + bin + " on PATH, or re-run: " + fix}
		}
		return Result{Pass, fmt.Sprintf("credential helper %q", helper), ""}
	}
	for key := range cfg.Auths {
		if registryHost(key) == host {
			return Result{Pass, "logged in with docker login", ""}
		}
	}
	return Result{Fail, fmt.Sprintf("docker has no credentials for %s in %s; ko pushes with gcloud's, but the envoy-dataplane push would be refused", host, path), fix}
}

// dockerLoginFix is the command that gives docker credentials for host:
// gcloud's helper for the Google registries the wizard defaults to, and a
// plain login for anything else.
func dockerLoginFix(host string) string {
	if host == "gcr.io" || strings.HasSuffix(host, ".gcr.io") || strings.HasSuffix(host, "-docker.pkg.dev") {
		return "gcloud auth configure-docker " + host
	}
	return "docker login " + host
}
