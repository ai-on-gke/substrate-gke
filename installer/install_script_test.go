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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// install.sh is piped into bash by the README one-liner, with REPO_BRANCH
// naming a branch or a release tag. An earlier install leaves a shallow,
// single-branch clone in SUBSTRATE_DIR, where a plain `git fetch origin <ref>`
// lands the ref in FETCH_HEAD only, so moving that checkout to a tag (or to a
// branch it was not cloned from) failed with "pathspec did not match". Each
// step runs the script against a local remote, with a stub make in place of
// the installer, and checks the commit it would launch from.
func TestInstallScriptMovesBetweenBranchesAndTags(t *testing.T) {
	script, err := filepath.Abs("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gitEnv := []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir,
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = append(os.Environ(), gitEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// A remote with two commits on main and an annotated tag on each.
	upstream := filepath.Join(dir, "upstream")
	git("init", "-q", "-b", "main", upstream)
	commit := func(msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(upstream, "file"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git("-C", upstream, "add", "file")
		git("-C", upstream, "commit", "-q", "-m", msg)
		return git("-C", upstream, "rev-parse", "HEAD")
	}
	first := commit("first")
	git("-C", upstream, "tag", "-a", "v1.0.0-gke.0", "-m", "first")
	second := commit("second")
	git("-C", upstream, "tag", "-a", "v1.0.0-gke.1", "-m", "second")
	git("-C", upstream, "reset", "-q", "--hard", first) // main starts at the first commit

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\necho \"LAUNCH $(git -C \"$2\" rev-parse HEAD)\"\n"
	if err := os.WriteFile(filepath.Join(bin, "make"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	// go only has to exist: install.sh checks for it, and the stub make
	// launches nothing.
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(dir, "home", ".substrate-gke")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	install := func(ref, want string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "bash")
		cmd.Dir = work // no installer/ here, so the script uses SUBSTRATE_DIR
		cmd.Stdin = strings.NewReader(string(body))
		cmd.Env = append(gitEnv,
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"REPO_URL=file://"+upstream, "REPO_BRANCH="+ref, "SUBSTRATE_DIR="+checkout)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "LAUNCH "+want) {
			t.Fatalf("install.sh with REPO_BRANCH=%s: %v, want a launch from %.12s\n%s", ref, err, want, out)
		}
		if strings.Contains(string(out), "detached HEAD") {
			t.Errorf("install.sh with REPO_BRANCH=%s printed git's detached-HEAD advice:\n%s", ref, out)
		}
	}

	install("v1.0.0-gke.0", first)  // fresh install from a tag
	install("v1.0.0-gke.1", second) // that checkout, to a newer tag
	install("v1.0.0-gke.1", second) // the same tag again
	install("main", first)          // to main, which this clone never had
	git("-C", upstream, "reset", "-q", "--hard", second)
	install("main", second) // main moved: a fast-forward

	if err := os.RemoveAll(checkout); err != nil {
		t.Fatal(err)
	}
	install("main", second)        // fresh install from main
	install("v1.0.0-gke.0", first) // that checkout, to a tag
	install("main", second)        // back to main, which it has
}
