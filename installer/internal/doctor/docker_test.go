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
)

func TestRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"gcr.io/acme/ate-images":                   "gcr.io",
		"us-docker.pkg.dev/acme/substrate":         "us-docker.pkg.dev",
		"https://gcr.io":                           "gcr.io",
		"https://index.docker.io/v1/":              "index.docker.io",
		"localhost:5001":                           "localhost:5001",
		"us-west1-docker.pkg.dev/acme/repo/nested": "us-west1-docker.pkg.dev",
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

// ko pushed fine and then the envoy-dataplane push got a 403, because docker
// had no gcr.io entry of its own. Each way of having one has to pass, and its
// absence has to fail with the command that adds it.
func TestDockerAuth(t *testing.T) {
	bin := t.TempDir()
	helper := filepath.Join(bin, "docker-credential-gcloud")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	for name, tc := range map[string]struct {
		config string // "" writes no file
		want   Status
	}{
		"no config":             {"", Fail},
		"invalid json":          {"{", Fail},
		"helper for the host":   {`{"credHelpers": {"gcr.io": "gcloud"}}`, Pass},
		"helper not on PATH":    {`{"credHelpers": {"gcr.io": "missing"}}`, Fail},
		"helper for other host": {`{"credHelpers": {"us-docker.pkg.dev": "gcloud"}}`, Fail},
		"docker login":          {`{"auths": {"gcr.io": {}}, "credsStore": "desktop"}`, Pass},
		"docker login by URL":   {`{"auths": {"https://gcr.io": {"auth": "eDp5"}}}`, Pass},
		"login to other host":   {`{"auths": {"ghcr.io": {}}}`, Fail},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.config != "" {
				if err := os.WriteFile(path, []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := dockerAuth(path, "gcr.io")
			if got.Status != tc.want {
				t.Fatalf("status = %v, want %v (%s)", got.Status, tc.want, got.Detail)
			}
			if got.Status == Fail && !strings.Contains(got.Fix, "gcloud auth configure-docker gcr.io") {
				t.Errorf("fix should name the command that adds the credential, got %q", got.Fix)
			}
		})
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

// The doctor runs before the images step, so it checks docker for everyone,
// against the default registry, and marks it as something a pre-built install
// can skip. Nothing else may be skippable.
func TestChecksIncludeDockerAsSourceOnly(t *testing.T) {
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

// --doctor has no images step to say which track this is, so a failed docker
// check must not fail it for someone installing pre-built images.
func TestRunCLIDoesNotCountSourceOnlyFailures(t *testing.T) {
	fail := func(context.Context) Result { return Result{Fail, "no docker", "fix it"} }
	checks := []Check{
		{Key: "docker", Name: "Docker daemon", Fatal: true, SourceOnly: true, Run: fail},
	}
	if got := RunCLI(context.Background(), checks); got != 0 {
		t.Errorf("RunCLI = %d fatal, want 0 for a source-only failure", got)
	}
	checks = append(checks, Check{Key: "gcloud", Name: "gcloud", Fatal: true, Run: fail})
	if got := RunCLI(context.Background(), checks); got != 1 {
		t.Errorf("RunCLI = %d fatal, want 1", got)
	}
}
