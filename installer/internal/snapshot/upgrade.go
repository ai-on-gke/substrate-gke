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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// RunbookURL is upstream's rolling upgrade runbook, which the upgrade track
// prepares for and hands over to.
const RunbookURL = "https://github.com/agent-substrate/substrate/blob/main/docs/upgrade.md"

// DefaultUpgradeDir is where the upgrade track keeps the two source trees.
// They live beside the install cache but outside the directories Cleanup
// sweeps, because the runbook runs from them for as long as the upgrade
// takes, rollback included.
func DefaultUpgradeDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate a cache directory for the upgrade trees: %w", err)
	}
	return filepath.Join(dir, "substrate-gke", "upgrades"), nil
}

// UpgradeTrees returns where the installed tree and the new tree go under
// base: one tree per commit. A tree depends on nothing but its commit, so
// clusters share them, and the paths stay short enough to read off a screen
// and paste whole.
func (b *Builder) UpgradeTrees(base string, st *state.Setup) (installed, next string) {
	return filepath.Join(base, "substrate-"+shorten(st.InstalledCommit)),
		filepath.Join(base, "substrate-"+shorten(b.commit))
}

// FetchTrees is the upgrade track's one command: the installer's shallow
// fetch, done for the installed commit and for the new one, into directories
// that stay. A tree already fetched whole is reused; a partial one is fetched
// again.
func (b *Builder) FetchTrees(st *state.Setup, installedDir, nextDir string) execx.Spec {
	fetch := func(dir, repo, commit string) []string {
		q := ShellQuote(dir)
		return append([]string{
			fmt.Sprintf(`if [ -e %s/%s ]; then echo "Using cached substrate@%s at "%s; else`, q, CompleteMarker, shorten(commit), q),
			fmt.Sprintf(`rm -rf %s && mkdir -p %s`, q, q),
		}, append(fetchAt(q, repo, commit),
			excludeMarker(q),
			fmt.Sprintf(`: > %s/%s`, q, CompleteMarker),
			fmt.Sprintf(`echo "Fetched substrate@%s into "%s`, shorten(commit), q),
			`fi`,
		)...)
	}
	lines := []string{"set -euo pipefail"}
	lines = append(lines, fetch(installedDir, st.InstalledRepo, st.InstalledCommit)...)
	lines = append(lines, fetch(nextDir, b.repo, b.commit)...)
	return execx.Spec{
		Label:   "fetch the installed and the new Substrate trees",
		Display: fmt.Sprintf("git fetch substrate@%s (installed) and substrate@%s (new)", shorten(st.InstalledCommit), shorten(b.commit)),
		Argv:    []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{
			fmt.Sprintf("Fetched substrate@%s into %s", shorten(st.InstalledCommit), installedDir),
			fmt.Sprintf("Fetched substrate@%s into %s", shorten(b.commit), nextDir),
		},
	}
}

// NewExports is the environment for the runbook's ate-setup commands from
// the new tree: the cluster, and the new version with where its images come
// from.
func (b *Builder) NewExports(st *state.Setup) string {
	next := Probe{KoDockerRepo: st.KoDockerRepo}
	if st.Prebuilt() {
		next = Probe{ImageRepo: st.ImageRepo, ImageTag: st.ImageTag}
	}
	return next.Exports(st, b.SubstrateVersion(st))
}

// UpgradeSummary is the hand-over, in the order the runbook reads: the
// runbook, the variables its own commands use, then for its "Checkout and
// environment" section the new tree and the ate-setup environment, and for
// a rollback what changes in them.
func (b *Builder) UpgradeSummary(st *state.Setup, installedDir, nextDir string) string {
	var sb strings.Builder
	block := func(heading string, lines ...string) {
		sb.WriteString(heading + "\n")
		for _, line := range lines {
			sb.WriteString("  " + line + "\n")
		}
		sb.WriteString("\n")
	}
	block("Run upstream's rolling upgrade runbook:", RunbookURL)
	block("Variables the runbook uses. The rest of its Preflight table ($NODEPOOL, $NS,\n$OLD_WORKERPOOL, $NEW_WORKERPOOL, $NODE) is yours to fill in.",
		"export CLUSTER="+ShellQuote(st.ClusterName),
		"export ZONE="+ShellQuote(st.Zone),
		"export OLD_VERSION="+ShellQuote(st.InstalledVersion),
		"export NEW_VERSION="+ShellQuote(b.SubstrateVersion(st)))
	block("When you reach its \"Checkout and environment\" section, check out the new release\nand set this for every ate-setup command of the upgrade:",
		append([]string{"cd " + ShellQuote(nextDir)}, strings.Split(b.NewExports(st), "\n")...)...)
	block("A rollback runs the same commands from the installed release, with its version:",
		append([]string{"cd " + ShellQuote(installedDir)}, strings.Split(InstalledExports(st), "\n")...)...)
	return strings.TrimRight(sb.String(), "\n")
}
