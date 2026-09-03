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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Revision is a Substrate source tree to build from: a repository and an
// exact commit in it.
type Revision struct {
	// Repo is the git URL the tree is fetched from.
	Repo string
	// Commit is the full 40-character SHA. Only a full SHA is usable: `git
	// fetch` will not accept an abbreviated one from a remote.
	Commit string
	// Describe says how the input was interpreted, e.g. "branch main
	// (6340d8712a2f)", for the wizard to echo back.
	Describe string
}

const resolveTimeout = 60 * time.Second

var (
	fullSHA  = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	shortSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,39}$`)
)

// Resolve turns a revision of the upstream repository into an exact commit
// the install can be run against. An empty ref means its current HEAD. The
// repository is a parameter because the tests point it at a local one; the
// wizard always passes RepoURL.
//
// Accepted: a branch, a tag, or a full commit SHA. Whatever it was, the commit
// it names is then verified against the remote, because naming a commit and
// being served it are different things: the install steps run `git fetch
// --depth 1 <repo> <sha>`, which a remote refuses unless it allows fetching a
// SHA directly. Better a prompt here than a failure ten minutes into an
// install that has already provisioned a cluster.
//
// needImageFlags additionally requires the tree to be new enough to install
// pre-built images, which only the pre-built track passes.
func Resolve(ctx context.Context, repo, ref string, needImageFlags bool) (Revision, error) {
	repo = strings.TrimSpace(repo)
	if err := checkRepoURL(repo); err != nil {
		return Revision{}, err
	}
	rev, err := lookup(ctx, repo, strings.TrimSpace(ref))
	if err != nil {
		return Revision{}, err
	}
	if err := verifyCommit(ctx, repo, rev.Commit, needImageFlags); err != nil {
		return Revision{}, err
	}
	return rev, nil
}

// Head resolves a repository's default branch and nothing more. It backs the
// revision the wizard prefills, which is a guess made while the user is still
// typing: it costs one `ls-remote` rather than the several seconds a fetch
// negotiation takes, and it is re-resolved through Resolve — verification and
// all — when the user submits.
func Head(ctx context.Context, repo string) (Revision, error) {
	repo = strings.TrimSpace(repo)
	if err := checkRepoURL(repo); err != nil {
		return Revision{}, err
	}
	return lookup(ctx, repo, "")
}

// lookup names the commit a ref points at, without asking whether the remote
// will serve it.
func lookup(ctx context.Context, repo, ref string) (Revision, error) {
	if ref == "" {
		sha, err := lsRemote(ctx, repo, "HEAD")
		if err != nil {
			return Revision{}, err
		}
		if sha == "" {
			return Revision{}, fmt.Errorf("%s has no HEAD; is it an empty repository?", repo)
		}
		return Revision{repo, sha, "HEAD (" + shorten(sha) + ")"}, nil
	}
	if fullSHA.MatchString(ref) {
		sha := strings.ToLower(ref)
		return Revision{repo, sha, "commit " + shorten(sha)}, nil
	}
	// git reads the ref argument of ls-remote as a glob, so a metacharacter
	// would match several refs and silently resolve to whichever sorted first.
	if i := strings.IndexAny(ref, "*?["); i >= 0 {
		return Revision{}, fmt.Errorf(
			"%q contains %q, and a revision has to name one branch, tag, or commit exactly", ref, ref[i:i+1])
	}
	// Branches before tags, deliberately and in that order: git resolves an
	// ambiguous name the other way round, but a name that is both here is
	// almost always a branch someone is iterating on.
	//
	// This runs before the abbreviated-SHA refusal below, because a ref may
	// perfectly well be named like one — `40ca1ce6` is a plausible branch, and
	// a real ref beats a guess about what the user meant.
	for _, k := range []struct {
		kind     string
		patterns []string
	}{
		{"branch", []string{"refs/heads/" + ref}},
		// The peeled "^{}" line of an annotated tag is only advertised when it
		// is asked for by name, and it is the line that names a commit.
		{"tag", []string{"refs/tags/" + ref, "refs/tags/" + ref + "^{}"}},
	} {
		sha, err := lsRemote(ctx, repo, k.patterns...)
		if err != nil {
			return Revision{}, err
		}
		if sha != "" {
			return Revision{repo, sha, fmt.Sprintf("%s %s (%s)", k.kind, ref, shorten(sha))}, nil
		}
	}
	if shortSHA.MatchString(ref) {
		return Revision{}, fmt.Errorf(
			"%q is an abbreviated commit, and git can only fetch a full 40-character SHA from a remote; paste the full SHA, or name the branch or tag", ref)
	}
	return Revision{}, fmt.Errorf("no branch or tag in %s is named %q", repo, ref)
}

// checkRepoURL rejects anything that is not an https git URL. That is narrower
// than what git understands, deliberately: https is the one transport the
// installer needs to read the upstream repository, and every form left out is
// a form nobody has to reason about.
//
// It also rejects anything git would read as an option rather than a remote.
// The URL reaches `git ls-remote` as an argument and the fetch script as a
// shell word, and a value like --upload-pack=... would run a command of the
// user's choosing in the first of those.
func checkRepoURL(repo string) error {
	if repo == "" {
		return errors.New("a repository URL is required")
	}
	if !strings.HasPrefix(repo, "https://") {
		return fmt.Errorf("%q is not an https git URL; the installer fetches over https, as in %s", repo, RepoURL)
	}
	return nil
}

// lsRemote returns the commit ref points at, or "" when the remote has no such
// ref — git exits 0 and prints nothing in that case, so empty is not an error.
//
// An annotated tag, which is what a release tag normally is, is advertised
// twice: as the tag object and again peeled to its commit under a "^{}"
// suffix. The peeled line is the one that names a commit, and a commit is the
// only thing the rest of this package can fetch, name a cache directory after,
// or stamp a build with.
func lsRemote(ctx context.Context, repo string, refs ...string) (string, error) {
	out, err := git(ctx, append([]string{"ls-remote", repo}, refs...)...)
	if err != nil {
		return "", err
	}
	var sha string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, name, ok := strings.Cut(line, "\t")
		if !ok || !fullSHA.MatchString(id) {
			return "", fmt.Errorf("unexpected git ls-remote output: %q", line)
		}
		if strings.HasSuffix(name, "^{}") {
			return id, nil
		}
		if sha == "" {
			sha = id
		}
	}
	return sha, nil
}

// imageFlagsPath is where ate-setup declares --image-repo, and imageFlagsName
// is the declaration to look for. A tree from before those flags existed takes
// an install that passes them all the way through bootstrap — cluster, bucket,
// IAM — and then dies at the deploy with "flag provided but not defined".
const (
	imageFlagsPath = "cmd/ate-setup/internal/cmd/root.go"
	imageFlagsName = "image-repo"
)

// verifyCommit checks the remote will actually serve a SHA, since naming a
// commit and being served it are different things: no ref points at an
// arbitrary commit, so ls-remote cannot answer it, and the install's own
// shallow fetch-by-SHA is refused by a remote that does not allow one.
//
// The fetch runs against a throwaway bare repository rather than the working
// directory, because git refuses to fetch outside a repository at all and the
// installer is normally not run from inside one. It is a real fetch rather
// than --dry-run, which downloads the same pack and skips only the ref update:
// --filter=blob:none makes it cheap, and leaves a tree the flag check can read
// a single blob out of on demand.
func verifyCommit(ctx context.Context, repo, sha string, needImageFlags bool) error {
	dir, err := os.MkdirTemp("", "substrate-gke-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := git(ctx, "init", "--quiet", "--bare", dir); err != nil {
		return err
	}
	if _, err := git(ctx, "--git-dir", dir, "fetch", "--quiet", "--depth", "1", "--filter=blob:none", repo, sha); err != nil {
		return fmt.Errorf("%s does not have commit %s: %w", repo, shorten(sha), err)
	}
	if !needImageFlags {
		return nil
	}
	// A tree that has moved the file elsewhere is left alone: that is upstream
	// restructuring, and refusing the install over it would be a guess. Only a
	// file that is there and does not declare the flag is an answer.
	out, err := git(ctx, "--git-dir", dir, "cat-file", "-p", "FETCH_HEAD:"+imageFlagsPath)
	if err == nil && !strings.Contains(out, imageFlagsName) {
		return fmt.Errorf(
			"substrate %s predates ate-setup's --%s, so it cannot install pre-built images; name a newer commit, or build from source instead",
			shorten(sha), imageFlagsName)
	}
	return nil
}

func git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		// A killed git says only "signal: killed", which would otherwise be
		// reported as the remote not having the commit.
		if ctx.Err() != nil {
			return "", fmt.Errorf("git gave up after %s; is the remote reachable?", resolveTimeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if detail := lastLine(string(ee.Stderr)); detail != "" {
				return "", errors.New(detail)
			}
		}
		return "", fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return string(out), nil
}

// lastLine picks the final non-empty line of git's stderr, which is the part
// that says what actually went wrong.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return strings.TrimPrefix(l, "fatal: ")
		}
	}
	return ""
}
