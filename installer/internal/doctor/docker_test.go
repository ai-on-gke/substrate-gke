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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
)

func TestRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"gcr.io/acme/ate-images":                   "gcr.io",
		"us-docker.pkg.dev/acme/substrate":         "us-docker.pkg.dev",
		"https://gcr.io":                           "gcr.io",
		"localhost:5001":                           "localhost:5001",
		"us-west1-docker.pkg.dev/acme/repo/nested": "us-west1-docker.pkg.dev",
		// Docker Hub goes by several names; docker files a login to any of
		// them under the last.
		"docker.io/acme/ate-images":     "docker.io",
		"registry-1.docker.io/acme/x":   "docker.io",
		"https://index.docker.io/v1/":   "docker.io",
		"index.docker.io/acme/ate-imgs": "docker.io",
	} {
		if got := registryHost(in); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The fix has to be one that works for the host: configure-docker only knows
// Google's registries.
func TestDockerLoginFix(t *testing.T) {
	for host, want := range map[string]string{
		"gcr.io":                  "gcloud auth configure-docker gcr.io",
		"eu.gcr.io":               "gcloud auth configure-docker eu.gcr.io",
		"us-west1-docker.pkg.dev": "gcloud auth configure-docker us-west1-docker.pkg.dev",
		"ghcr.io":                 "docker login ghcr.io",
	} {
		if got := dockerLoginFix(host); got != want {
			t.Errorf("dockerLoginFix(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestDaemonFixFitsThePlatform(t *testing.T) {
	if got := daemonFix("darwin"); !strings.Contains(got, "open -a Docker") {
		t.Errorf("macOS should be told to start Docker Desktop, got %q", got)
	}
	if got := daemonFix("linux"); !strings.Contains(got, "systemctl start docker") {
		t.Errorf("linux should be told to start the daemon, got %q", got)
	}
}

// fakeHelpers puts docker-credential-* stand-ins on PATH: gcloud and gcr have
// a credential for anything, nocreds for nothing, and desktop, like Docker
// Desktop's keychain, only for what was logged in to, https://gcr.io and Hub.
func fakeHelpers(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{
		"gcloud":  "#!/bin/sh\necho '{\"Secret\":\"token\"}'\n",
		"gcr":     "#!/bin/sh\necho '{\"Secret\":\"token\"}'\n",
		"nocreds": "#!/bin/sh\necho 'credentials not found in native keychain'\nexit 1\n",
		"desktop": "#!/bin/sh\nk=$(cat)\ncase \"$k\" in https://gcr.io|https://index.docker.io/v1/) echo '{}' ;; *) echo 'credentials not found in native keychain'; exit 1 ;; esac\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, "docker-credential-"+name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// ko pushed fine and then the envoy-dataplane push got a 403, because docker
// had no gcr.io entry of its own. Each way of having one has to pass, and its
// absence has to fail with the command that adds it.
func TestDockerAuth(t *testing.T) {
	fakeHelpers(t)

	for name, tc := range map[string]struct {
		config string // "" writes no file
		host   string // "" is gcr.io
		want   Status
	}{
		"no config":                 {"", "", Fail},
		"invalid json":              {"{", "", Fail},
		"helper for the host":       {`{"credHelpers": {"gcr.io": "gcloud"}}`, "", Pass},
		"helper without credential": {`{"credHelpers": {"gcr.io": "nocreds"}}`, "", Fail},
		"helper not on PATH":        {`{"credHelpers": {"gcr.io": "missing"}}`, "", Fail},
		"helper for other host":     {`{"credHelpers": {"us-docker.pkg.dev": "gcloud"}}`, "", Fail},
		// Docker consults credsStore for every host with no helper of its own.
		"store with a credential": {`{"credsStore": "gcr"}`, "", Pass},
		// Docker Desktop sets credsStore whether or not you logged in.
		"desktop never logged in": {`{"credsStore": "desktop"}`, "", Fail},
		"desktop logged in":       {`{"auths": {"https://gcr.io": {}}, "credsStore": "desktop"}`, "", Pass},
		"docker login by URL":     {`{"auths": {"https://gcr.io": {"auth": "eDp5"}}}`, "", Pass},
		"identity token":          {`{"auths": {"gcr.io": {"identitytoken": "t"}}}`, "", Pass},
		// With no store to hold it, an empty entry has no credential.
		"empty login, no store": {`{"auths": {"gcr.io": {}}}`, "", Fail},
		"login to other host":   {`{"auths": {"ghcr.io": {"auth": "eDp5"}}}`, "", Fail},
		"docker hub login":      {`{"auths": {"https://index.docker.io/v1/": {"auth": "eDp5"}}}`, "docker.io", Pass},
		"docker hub in desktop": {`{"credsStore": "desktop"}`, "docker.io", Pass},
	} {
		t.Run(name, func(t *testing.T) {
			host := tc.host
			if host == "" {
				host = "gcr.io"
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.config != "" {
				if err := os.WriteFile(path, []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := dockerAuth(context.Background(), path, host)
			if got.Status != tc.want {
				t.Fatalf("status = %v, want %v (%s)", got.Status, tc.want, got.Detail)
			}
			if got.Status == Fail && !strings.Contains(got.Fix, dockerLoginFix(host)) {
				t.Errorf("fix should name the command that adds the credential, got %q", got.Fix)
			}
		})
	}
}

func TestDockerAuthSaysWhyTheHelperFailed(t *testing.T) {
	fakeHelpers(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"credHelpers": {"gcr.io": "nocreds"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dockerAuth(context.Background(), path, "gcr.io"); !strings.Contains(got.Detail, "credentials not found") {
		t.Errorf("detail should carry the helper's own error, got %q", got.Detail)
	}
}

func TestDockerAuthWithoutAHomeDirectory(t *testing.T) {
	got := dockerAuth(context.Background(), "", "gcr.io")
	if got.Status != Fail || !strings.Contains(got.Detail, "DOCKER_CONFIG") {
		t.Errorf("want a failure naming DOCKER_CONFIG, got %+v", got)
	}
}

func TestDockerConfigPathHonoursDockerConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	if got := dockerConfigPath(); got != filepath.Join(dir, "config.json") {
		t.Errorf("dockerConfigPath = %q", got)
	}
}

// A failure in any of them surfaces only after the control-plane bundle is
// applied, so none of them may be a warning.
func TestDockerChecksAreFatalAndNameTheRegistry(t *testing.T) {
	checks := DockerChecks("us-west1-docker.pkg.dev/acme/ate-images")
	keys := map[string]bool{}
	for _, c := range checks {
		keys[c.Key] = true
		if !c.Fatal {
			t.Errorf("check %q should be fatal", c.Key)
		}
		if c.Key == "docker-auth" && !strings.Contains(c.Name, "us-west1-docker.pkg.dev") {
			t.Errorf("credential check should name the host, got %q", c.Name)
		}
	}
	for _, key := range []string{"docker", "buildx", "docker-auth"} {
		if !keys[key] {
			t.Errorf("missing check %q", key)
		}
	}
}

// With no docker at all, only the daemon check fails; the other two say they
// were not checked instead of stacking two more failures under it.
func TestDockerChecksWithoutDockerFailOnce(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	failed := 0
	for _, c := range DockerChecks("gcr.io/acme/ate-images") {
		res := c.Run(context.Background())
		if res.Status == Fail {
			failed++
			if c.Key != "docker" {
				t.Errorf("check %q failed; it should not be checked without docker", c.Key)
			}
		}
	}
	if failed != 1 {
		t.Errorf("%d failures, want 1", failed)
	}
}

// The doctor runs before the images step, so it checks docker for everyone,
// against the default registry, and marks it as something a pre-built install
// can skip. Nothing else may be skippable.
func TestChecksIncludeDockerAsSourceOnly(t *testing.T) {
	t.Setenv("ATE_ATENET_DATAPLANE", "")
	sourceOnly := map[string]bool{}
	for _, c := range Checks(t.TempDir(), true) {
		if c.SourceOnly {
			sourceOnly[c.Key] = true
			if !c.Fatal {
				t.Errorf("check %q should still be fatal for a build from source", c.Key)
			}
		}
		if c.Key == "docker-auth" && c.Name != "Docker credentials for gcr.io" {
			t.Errorf("the doctor should check the default registry's host, got %q", c.Name)
		}
	}
	for _, key := range []string{"docker", "buildx", "docker-auth"} {
		if !sourceOnly[key] {
			t.Errorf("check %q should be in the doctor, marked SourceOnly", key)
		}
	}
	if len(sourceOnly) != 3 {
		t.Errorf("only the docker checks may be skippable, got %v", sourceOnly)
	}
}

// The project step's advice for a docker problem is to use agentgateway,
// which builds nothing with docker; the doctor must not then ask for it. Nor
// for a tree of the user's own that has no envoy-dataplane to build.
func TestChecksLeaveOutDockerWhenNothingIsBuiltWithIt(t *testing.T) {
	hasDocker := func(root string, managed bool) bool {
		for _, c := range Checks(root, managed) {
			if c.Key == "docker" {
				return true
			}
		}
		return false
	}
	withEnvoy := t.TempDir()
	dockerfile := filepath.Join(withEnvoy, snapshot.EnvoyDockerfile)
	if err := os.MkdirAll(filepath.Dir(dockerfile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerfile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ATE_ATENET_DATAPLANE", "agentgateway")
	if hasDocker(t.TempDir(), true) {
		t.Error("agentgateway builds nothing with docker")
	}
	t.Setenv("ATE_ATENET_DATAPLANE", "envoy")
	if hasDocker(t.TempDir(), false) {
		t.Error("a supplied tree without the envoy Dockerfile builds nothing with docker")
	}
	if !hasDocker(withEnvoy, false) {
		t.Error("a supplied tree with the envoy Dockerfile builds it with docker")
	}
}

// --doctor has no images step to say which track this is, so a failed docker
// check must not fail it for someone installing pre-built images, but it is
// counted so the summary does not call it all good.
func TestRunCLICountsSourceOnlyFailuresApart(t *testing.T) {
	fail := func(context.Context) Result { return Result{Fail, "no docker", "fix it"} }
	checks := []Check{
		{Key: "docker", Name: "Docker daemon", Fatal: true, SourceOnly: true, Run: fail},
	}
	if fatal, sourceOnly := RunCLI(context.Background(), checks); fatal != 0 || sourceOnly != 1 {
		t.Errorf("RunCLI = %d fatal, %d source-only; want 0, 1", fatal, sourceOnly)
	}
	checks = append(checks, Check{Key: "gcloud", Name: "gcloud", Fatal: true, Run: fail})
	if fatal, _ := RunCLI(context.Background(), checks); fatal != 1 {
		t.Errorf("RunCLI = %d fatal, want 1", fatal)
	}
}
