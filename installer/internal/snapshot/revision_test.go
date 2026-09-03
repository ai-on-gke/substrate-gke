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

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The repository is typed into the wizard and then handed to `git ls-remote`
// as an argument, where a value beginning with a dash is read as an option —
// --upload-pack= runs a command of the caller's choosing. Everything that is
// not plainly a git URL is refused before git ever sees it.
func TestResolveRejectsNonURLRepositories(t *testing.T) {
	for _, repo := range []string{
		"",
		"--upload-pack=touch /tmp/pwned",
		"-u",
		"github.com/agent-substrate/substrate",
		"file:///etc",
	} {
		if _, err := Resolve(context.Background(), repo, "main", false); err == nil {
			t.Errorf("Resolve accepted %q as a repository", repo)
		}
		if _, err := Head(context.Background(), repo); err == nil {
			t.Errorf("Head accepted %q as a repository", repo)
		}
	}
}

// testRemote builds a local repository that will serve a bare SHA, standing in
// for a git host so these tests need no network. It returns the path and a
// runner for further git commands in it.
func testRemote(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// A user's own insteadOf rewrites and hooks have no business here.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--quiet", "--initial-branch=main")
	run("config", "uploadpack.allowAnySHA1InWant", "true")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("commit", "--quiet", "--allow-empty", "-m", "one")
	return dir, run
}

// An abbreviated SHA cannot be fetched from a remote and no ref points at one,
// so there is nothing to resolve it against. Saying so beats a fetch that
// fails ten minutes into the install.
func TestLookupRejectsAnAbbreviatedCommit(t *testing.T) {
	repo, _ := testRemote(t)
	_, err := lookup(context.Background(), repo, ShortCommit())
	if err == nil {
		t.Fatalf("lookup accepted the abbreviated commit %s", ShortCommit())
	}
	if !strings.Contains(err.Error(), "full") {
		t.Errorf("the error should ask for the full SHA, got: %v", err)
	}
}

// Hex is a perfectly ordinary thing to name a branch after — this repository's
// own e2e images are tagged with one — so a ref that exists has to win over the
// guess that the user meant an abbreviated commit.
func TestLookupPrefersARealRefOverAnAbbreviatedCommit(t *testing.T) {
	repo, run := testRemote(t)
	run("branch", "40ca1ce6")
	want := run("rev-parse", "refs/heads/40ca1ce6")

	rev, err := lookup(context.Background(), repo, "40ca1ce6")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if rev.Commit != want {
		t.Errorf("lookup = %s, want the branch at %s", rev.Commit, want)
	}
}

// An annotated tag — the normal shape of a release tag — is advertised as the
// tag object first. Taking that id would name something that is not a commit,
// and every later use of it (cache directory, ko stamp, fetch) wants a commit.
func TestLookupPeelsAnAnnotatedTag(t *testing.T) {
	repo, run := testRemote(t)
	run("tag", "--annotate", "v1.0", "--message", "release")
	want := run("rev-parse", "refs/tags/v1.0^{commit}")
	if tagObject := run("rev-parse", "refs/tags/v1.0"); tagObject == want {
		t.Fatal("the tag is not annotated, so this test proves nothing")
	}

	rev, err := lookup(context.Background(), repo, "v1.0")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if rev.Commit != want {
		t.Errorf("lookup = %s, want the peeled commit %s", rev.Commit, want)
	}
}

// ls-remote reads its ref argument as a glob, so a pattern would match several
// refs and quietly resolve to whichever sorted first.
func TestLookupRejectsAGlobRevision(t *testing.T) {
	repo, _ := testRemote(t)
	for _, ref := range []string{"v0.*", "v0.?", "v[01].0"} {
		if _, err := lookup(context.Background(), repo, ref); err == nil {
			t.Errorf("lookup accepted the pattern %q", ref)
		}
	}
}

// https is the only transport the installer supports. git understands several
// more, but every one left out is a form nobody has to reason about — and a
// fork read over https is the case this exists to serve.
func TestCheckRepoURLTakesHTTPSAndNothingElse(t *testing.T) {
	for _, repo := range []string{
		RepoURL,
		"https://github.com/annapendleton/substrate.git",
		"https://git.internal/substrate",
	} {
		if err := checkRepoURL(repo); err != nil {
			t.Errorf("checkRepoURL(%q) = %v", repo, err)
		}
	}
	for _, repo := range []string{
		"http://git.internal/substrate.git",
		"ssh://git@github.com/annapendleton/substrate.git",
		"git@github.com:annapendleton/substrate.git",
		"HTTPS://github.com/agent-substrate/substrate.git",
	} {
		if err := checkRepoURL(repo); err == nil {
			t.Errorf("checkRepoURL(%q) = nil, want an error", repo)
		}
	}
}

// git will not fetch outside a repository, and the installer is normally run
// from wherever the user's shell happens to be. Verification therefore has to
// bring its own repository, or it reports "not a git repository" for every
// revision anyone ever types.
func TestVerifyCommitRunsOutsideAGitRepository(t *testing.T) {
	remote, run := testRemote(t)
	sha := run("rev-parse", "HEAD")

	// Not a repository, and not inside one.
	t.Chdir(t.TempDir())

	if err := verifyCommit(context.Background(), remote, sha, false); err != nil {
		t.Errorf("verifyCommit(%s) = %v", shorten(sha), err)
	}
	absent := strings.Repeat("0", 39) + "1"
	if err := verifyCommit(context.Background(), remote, absent, false); err == nil {
		t.Error("verifyCommit accepted a commit the remote does not have")
	}
}

func TestLastLineReportsWhatGitActuallySaid(t *testing.T) {
	stderr := "Cloning into 'x'...\nremote: Enumerating\nfatal: repository not found\n\n"
	if got := lastLine(stderr); got != "repository not found" {
		t.Errorf("lastLine = %q", got)
	}
	if got := lastLine("  \n\n"); got != "" {
		t.Errorf("lastLine of blank stderr = %q", got)
	}
}
