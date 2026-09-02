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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FetchTree checks out one Substrate commit into dir, the pinned one when
// commit is empty, and leaves it there. It is the same shallow fetch the
// installer does for itself, offered as a command for the trees the install
// does not leave behind: upstream's rolling upgrade runs from a checkout of
// the installed release and one of the new release.
func FetchTree(ctx context.Context, dir, commit string) error {
	if commit == "" {
		commit = Commit
	}
	return fetchTree(ctx, dir, RepoURL, commit)
}

func fetchTree(ctx context.Context, dir, repoURL, commit string) error {
	if isCheckout(dir) {
		return fmt.Errorf("%s already holds a checkout", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", repoURL, commit},
		{"checkout", "-q", "FETCH_HEAD"},
	} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	fmt.Printf("Fetched substrate@%s into %s\n", commit, abs)
	return nil
}
