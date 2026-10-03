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
	"runtime"
	"strings"
	"time"
)

// dockerTimeout bounds each docker probe. A hung daemon would otherwise hold
// the project step, which cannot be cancelled while it validates, for the
// doctor's full probeTimeout per probe.
const dockerTimeout = 10 * time.Second

// dockerHub is the host Docker Hub's credentials are compared under. docker
// files them as https://index.docker.io/v1/ whatever name the login used.
const dockerHub = "docker.io"

// DockerChecks returns the probes behind ate-setup's `docker buildx build
// --push` of the envoy-dataplane image to registry, which Substrate 0.2 added.
//
// An empty registry checks only the daemon and buildx. Registry credentials
// are checked once the installation has selected a destination. The build runs
// inside the control-plane deploy, after the bundle is applied, so a failure
// there leaves a half-installed cluster.
//
// Without docker the other two are not checked, so a missing docker shows
// as one failure rather than three.
func DockerChecks(registry string) []Check {
	host := registryHost(registry)
	checks := []Check{
		{
			Key: "docker", Name: "Docker daemon", Fatal: true,
			Run: func(ctx context.Context) Result {
				if _, err := exec.LookPath("docker"); err != nil {
					return Result{Fail, "docker is not installed; a build of Substrate 0.2+ builds its envoy-dataplane image with docker buildx",
						"https://docs.docker.com/engine/install/"}
				}
				ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
				defer cancel()
				out, err := output(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
				if err != nil || out == "" {
					return Result{Fail, "docker is installed, but its daemon is not running or you cannot reach it",
						daemonFix(runtime.GOOS)}
				}
				return Result{Pass, "Docker Engine " + out, ""}
			},
		},
		{
			Key: "buildx", Name: "Docker buildx", Fatal: true,
			Run: func(ctx context.Context) Result {
				if res, ok := notChecked(); ok {
					return res
				}
				ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
				defer cancel()
				out, err := output(ctx, "docker", "buildx", "version")
				if err != nil {
					return Result{Fail, "docker buildx is not available; ate-setup builds envoy-dataplane with it",
						"https://docs.docker.com/go/buildx/"}
				}
				return Result{Pass, strings.SplitN(out, "\n", 2)[0], ""}
			},
		},
		{
			Key: "docker-auth", Name: "Docker credentials for " + host, Fatal: true,
			Run: func(ctx context.Context) Result {
				if res, ok := notChecked(); ok {
					return res
				}
				return dockerAuth(ctx, dockerConfigPath(), host)
			},
		},
	}
	if registry == "" {
		return checks[:2]
	}
	return checks
}

// warnOnly is c as the doctor runs it. The doctor comes before the images
// step, so it cannot know whether this install builds anything with docker:
// a failure only warns, and says who it matters to.
func warnOnly(c Check) Check {
	run := c.Run
	c.Fatal, c.SourceOnly = false, true
	c.Run = func(ctx context.Context) Result {
		res := run(ctx)
		if res.Status == Fail {
			res.Status = Warn
			res.Detail += " (only needed to build Substrate 0.2 or later from source)"
		}
		return res
	}
	return c
}

// notChecked stands in for a check that needs docker when there is none; the
// daemon check already reports that.
func notChecked() (Result, bool) {
	if _, err := exec.LookPath("docker"); err != nil {
		return Result{Warn, "not checked: docker is not installed", ""}, true
	}
	return Result{}, false
}

// daemonFix is how to start docker's daemon on goos.
func daemonFix(goos string) string {
	if goos == "darwin" {
		return "open -a Docker   # start Docker Desktop"
	}
	return "sudo systemctl start docker   # or add yourself to the docker group"
}

// registryHost is the host part of an image repository such as
// gcr.io/acme/ate-images, which is what docker keys credentials by. Docker
// Hub's several names all come out as dockerHub.
func registryHost(registry string) string {
	registry = strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://")
	host, _, _ := strings.Cut(registry, "/")
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return dockerHub
	}
	return host
}

// dockerConfigPath is the file docker reads credentials from, honouring
// DOCKER_CONFIG the way the docker CLI does. It is "" when there is no home
// directory to find it in.
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
	CredsStore  string                     `json:"credsStore"`
	Auths       map[string]json.RawMessage `json:"auths"`
}

// dockerAuthConfigured checks for credential configuration before a registry is chosen.
func dockerAuthConfigured(_ context.Context) Result {
	if res, ok := notChecked(); ok {
		return res
	}
	const fix = "gcloud auth configure-docker <region>-docker.pkg.dev   # or docker login <registry>"
	data, err := os.ReadFile(dockerConfigPath())
	if err != nil {
		return Result{Fail, "cannot read Docker credential configuration: " + err.Error(), fix}
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Result{Fail, "invalid Docker credential configuration: " + err.Error(), fix}
	}
	configured := cfg.CredsStore != ""
	for _, helper := range cfg.CredHelpers {
		configured = configured || helper != ""
	}
	for _, entry := range cfg.Auths {
		configured = configured || hasToken(entry)
	}
	if !configured {
		return Result{Fail, "no Docker credential helper or saved credentials configured", fix}
	}
	return Result{Pass, "credential configuration found; registry credentials are checked after selection", ""}
}

// dockerAuth reports whether docker can authenticate to host. ko finds
// gcloud's credentials on its own; docker only looks in its config, and
// without an entry there it pushes anonymously and gets a 403 from gcr.io and
// Artifact Registry alike.
//
// It asks the way docker does: the credHelper for the host, which is what
// `gcloud auth configure-docker` writes; else the credsStore, which Docker
// Desktop always sets, so only an answer from it counts; else a token saved
// in auths by `docker login`. A helper is run, not just found, since having
// one says nothing about whether it holds a credential: gcloud's helper uses
// the `gcloud auth login` account, not application-default credentials.
func dockerAuth(ctx context.Context, path, host string) Result {
	fix := dockerLoginFix(host)
	if path == "" {
		return Result{Fail, "cannot find your home directory, so cannot read docker's config.json; set DOCKER_CONFIG to the directory that holds it", fix}
	}
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
		return askHelper(ctx, helper, []string{host}, path, host, fix)
	}
	// The names docker may have filed the host's credentials under.
	keys := []string{host}
	if host == dockerHub {
		keys = append(keys, "https://index.docker.io/v1/")
	}
	token := false
	for key, entry := range cfg.Auths {
		if registryHost(key) == host {
			keys = append(keys, key)
			token = token || hasToken(entry)
		}
	}
	if cfg.CredsStore != "" {
		return askHelper(ctx, cfg.CredsStore, keys, path, host, fix)
	}
	if token {
		return Result{Pass, "logged in with docker login", ""}
	}
	return Result{Fail, fmt.Sprintf("docker has no credentials for %s in %s; ko pushes with gcloud's, but the envoy-dataplane push would be refused", host, path), fix}
}

// hasToken reports whether an auths entry holds a credential itself, rather
// than being the empty placeholder docker leaves when a credsStore does.
func hasToken(entry json.RawMessage) bool {
	var e struct {
		Auth          string `json:"auth"`
		IdentityToken string `json:"identitytoken"`
		RegistryToken string `json:"registrytoken"`
	}
	return json.Unmarshal(entry, &e) == nil && (e.Auth != "" || e.IdentityToken != "" || e.RegistryToken != "")
}

// askHelper asks docker-credential-<helper> for a credential under each of
// keys, passing on the first it has.
func askHelper(ctx context.Context, helper string, keys []string, path, host, fix string) Result {
	bin := "docker-credential-" + helper
	if _, err := exec.LookPath(bin); err != nil {
		return Result{Fail, fmt.Sprintf("%s uses the %q credential helper for %s, but %s is not on PATH", path, helper, host, bin),
			"put " + bin + " on PATH, or re-run: " + fix}
	}
	var err error
	for _, key := range keys {
		if err = credential(ctx, bin, key); err == nil {
			return Result{Pass, fmt.Sprintf("credential helper %q", helper), ""}
		}
	}
	return Result{Fail, fmt.Sprintf("the %q credential helper has no credentials for %s: %v", helper, host, err), fix}
}

// credential runs `docker-credential-<helper> get` for key, as docker does
// before a push. Its output on success is the secret, so only a failure's is
// kept.
func credential(ctx context.Context, bin, key string) error {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "get")
	cmd.Stdin = strings.NewReader(key)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(msg)
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
