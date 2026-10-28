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
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gkeoverlay"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

// RunbookURL is upstream's upgrade runbook, which the upgrade track prepares
// for and hands over to.
const RunbookURL = "https://github.com/agent-substrate/substrate/blob/main/docs/upgrade.md"

// DefaultUpgradeDir is where the upgrade track keeps the two source trees.
// They live beside the install cache but outside the directories Cleanup
// sweeps, because the runbook runs from them, through the links the hand-over
// makes, for as long as the upgrade takes, rollback included.
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
//
// Trees are shared between clusters and runs, so each is fetched into a
// staging directory beside its final name and published whole with a
// rename, the way the install cache does it: a concurrent run never sees a
// half-fetched tree, and never deletes one from under a fetch in progress.
func (b *Builder) FetchTrees(st *state.Setup, installedDir, nextDir string) execx.Spec {
	fetch := func(stage, dir, repo, commit, missing string) []string {
		q, s := ShellQuote(dir), `"${`+stage+`}"`
		steps := fetchAt(s, repo, commit)
		if missing != "" {
			steps[1] += " || { echo " + ShellQuote(missing) + " >&2; exit 1; }"
		}
		return append(append([]string{
			fmt.Sprintf(`if [ -e %s/%s ]; then echo "Using cached substrate@%s at "%s; else`, q, CompleteMarker, shorten(commit), q),
			fmt.Sprintf(`mkdir -p %s`, ShellQuote(filepath.Dir(dir))),
			fmt.Sprintf(`%s=$(mktemp -d %s)`, stage, ShellQuote(dir+stageInfix+"XXXXXX")),
		}, steps...),
			excludeMarker(s),
			fmt.Sprintf(`: > %s/%s`, s, CompleteMarker),
			// A run that published first has a tree as good as ours.
			fmt.Sprintf(`if [ -e %s/%s ]; then rm -rf %s; echo "Using cached substrate@%s at "%s; else`, q, CompleteMarker, s, shorten(commit), q),
			fmt.Sprintf(`rm -rf %s; mv %s %s; rm -rf %s/"${%s##*/}"`, q, s, q, q, stage),
			fmt.Sprintf(`echo "Fetched substrate@%s into "%s; fi`, shorten(commit), q),
			`fi`,
		)
	}
	// The installed commit was read off the cluster, not resolved against
	// the repository like the new one; a fork or a private hotfix fails here.
	notUpstream := fmt.Sprintf("Commit %s is not in %s: the installed Substrate was built from a fork or a private hotfix. Follow the runbook from a checkout of your own instead.", st.InstalledCommit, b.repo)
	lines := []string{
		"set -euo pipefail",
		`trap 'rm -rf "${stage_installed:-}" "${stage_next:-}"' EXIT`,
		`trap 'exit 130' INT TERM`,
	}
	lines = append(lines, fetch("stage_installed", installedDir, b.repo, st.InstalledCommit, notUpstream)...)
	lines = append(lines, fetch("stage_next", nextDir, b.repo, b.commit, "")...)
	// The runbook deploys the new release from nextDir and rolls back from
	// installedDir, so both get deploy/gke if they need it. Both are trees
	// this installer fetched, so it adds rather than only checks.
	for _, dir := range []string{installedDir, nextDir} {
		if line := b.overlayCommand(ShellQuote(dir), true); line != "" {
			lines = append(lines, line)
		}
	}
	return execx.Spec{
		Label:   "fetch the installed and the new Substrate trees",
		Display: fmt.Sprintf("git fetch substrate@%s (installed) and substrate@%s (new)", shorten(st.InstalledCommit), shorten(b.commit)),
		Argv:    []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{
			fmt.Sprintf("Fetched substrate@%s into %s", shorten(st.InstalledCommit), installedDir),
			fmt.Sprintf("Fetched substrate@%s into %s", shorten(b.commit), nextDir),
			gkeoverlay.NotNeededLine,
			gkeoverlay.NotNeededLine,
		},
	}
}

// RecordPath is the record ate-setup wrote of the cluster st names when it
// deployed there: under ATE_RECORD_DIR, or by default the user cache
// directory, one file per kube context. ate-setup fetched the credentials
// itself, so the context is the one gcloud names.
func RecordPath(st *state.Setup) (string, error) {
	dir := os.Getenv("ATE_RECORD_DIR")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate the cache directory ate-setup records installs in: %w", err)
		}
		dir = filepath.Join(base, "ate-setup")
	}
	context := "gke_" + st.ProjectID + "_" + st.Zone + "_" + st.ClusterName
	return filepath.Join(dir, "installs", context+".yaml"), nil
}

var releaseVersion = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

// CheckUpgrade reports whether the runbook supports moving from installed to
// next. Per its table, patch releases of one minor can, and from 1.0 so can
// the next minor. A pre-release such as v1.0.0-rc.1 comes before its release.
// A version that is not a release, such as a build from source, is only
// checked for being a different one.
func CheckUpgrade(installed, next string) error {
	if installed == next {
		return errors.New("the cluster already runs that version")
	}
	from, to := releaseVersion.FindStringSubmatch(installed), releaseVersion.FindStringSubmatch(next)
	if from == nil || to == nil {
		return nil
	}
	n := func(m []string, i int) int { v, _ := strconv.Atoi(m[i]); return v }
	major, minor := n(from, 1), n(from, 2)
	older := cmp.Or(cmp.Compare(n(to, 1), major), cmp.Compare(n(to, 2), minor), cmp.Compare(n(to, 3), n(from, 3)), comparePrerelease(to[4], from[4])) < 0
	switch {
	case older:
		return fmt.Errorf("%s is older than the installed %s; the runbook's rollback covers going back", next, installed)
	case n(to, 1) != major:
		return errors.New("a new major release needs a reinstall")
	case major == 0 && n(to, 2) != minor:
		return errors.New("before 1.0, a new minor release needs a reinstall")
	case n(to, 2) > minor+1:
		return fmt.Errorf("minor releases cannot be skipped; upgrade to v%d.%d first", major, minor+1)
	}
	return nil
}

// comparePrerelease orders two pre-release suffixes the way semver does: a
// release ("") after any pre-release of it, then identifier by identifier,
// numbers numerically and before words.
func comparePrerelease(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		var c int
		switch {
		case aErr == nil && bErr == nil:
			c = cmp.Compare(an, bn)
		case aErr == nil:
			c = -1
		case bErr == nil:
			c = 1
		default:
			c = strings.Compare(as[i], bs[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// RecordPrebuilt reports whether the record at path installs pre-built
// images: whether it has a repo under images, read line by line the way the
// runbook's own commands read the record. ok is false when there is no
// record to read.
func RecordPrebuilt(path string) (prebuilt, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	inImages := false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "images:":
			inImages = true
		case !strings.HasPrefix(line, " "):
			inImages = false
		case inImages && strings.HasPrefix(strings.TrimSpace(line), "repo:"):
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "repo:")) != "", true
		}
	}
	return false, true
}

// CheckImageSource reports whether the new version comes from the kind of
// images the cluster's record installs. The runbook copies the record into
// new.yaml and changes only where its pre-built images come from, so it
// cannot move a cluster between pre-built images and a build from source.
// Without a record there is nothing to check yet.
func CheckImageSource(st *state.Setup) error {
	record, err := RecordPath(st)
	if err != nil {
		return nil
	}
	prebuilt, ok := RecordPrebuilt(record)
	switch {
	case !ok || prebuilt == st.Prebuilt():
		return nil
	case prebuilt:
		return errors.New("its record installs pre-built images, and the runbook moves them only to other pre-built images")
	default:
		return errors.New("its record installs a build from source, and the runbook builds the new release the same way")
	}
}

// UpgradeHome is the directory upstream's runbook works in.
const UpgradeHome = "~/ate-upgrade"

// PrepareUpgrade is the runbook's "Before you start" done for the cluster,
// from the trees already fetched and ate-setup's record of the cluster, plus
// the GKE preparation the runbook leaves out. It refuses to replace an
// ~/ate-upgrade left from an earlier upgrade, which may still hold what that
// upgrade's rollback needs, and lays the directory out last, whole, so a
// failed run leaves nothing to delete before a retry.
func (b *Builder) PrepareUpgrade(st *state.Setup, installedDir, nextDir string) execx.Spec {
	record, err := RecordPath(st)
	if err != nil {
		record = ""
	}
	noRecord := fmt.Sprintf("This machine has no ate-setup record of %s at %s. Write one from the settings the cluster was installed with (upstream's docs/operator-install.md lists them), then retry.", st.ClusterName, record)
	lines := []string{
		"set -euo pipefail",
		"record=" + ShellQuote(record),
		`up="$HOME/ate-upgrade"`,
		`[ -f "$record" ] || { echo ` + ShellQuote(noRecord) + ` >&2; exit 1; }`,
		`if [ -e "$up" ] || [ -L "$up" ]; then echo "$up is left from an earlier upgrade. Delete it once that upgrade no longer needs it for a rollback, then retry." >&2; exit 1; fi`,
		// The upgrade track never asks for the bucket; the record names it.
		`bucket=$(sed -n 's/^  bucketName: //p' "$record")`,
		`[ -n "$bucket" ] || { echo "$record names no snapshot bucket (storage.bucketName). Add the bucket the cluster was installed with, then retry." >&2; exit 1; }`,
		fmt.Sprintf("gcloud container clusters get-credentials %s --location %s --project %s",
			ShellQuote(st.ClusterName), ShellQuote(st.Zone), ShellQuote(st.ProjectID)),
		fmt.Sprintf(`echo "Installing kubectl-ate from substrate@%s"`, shorten(st.InstalledCommit)),
		"go -C " + ShellQuote(installedDir) + " install ./cmd/kubectl-ate",
		// Substrate v0.6.0 and later impersonate the snapshot transfer
		// service account; creating it only adds bindings, so it is safe
		// for an older release too.
		fmt.Sprintf(`SNAPSHOT_TRANSFER_GSA_NAME=%s go -C %s run . create iam --project-id %s --bucket "$bucket"`,
			ShellQuote(TransferGSAName), ShellQuote(b.SetupGCP), ShellQuote(st.ProjectID)),
		`stage=$(mktemp -d "$up.XXXXXX")`,
		`trap 'rm -rf "$stage"' EXIT`,
		`cp "$record" "$stage/old.yaml"`,
		`cp "$record" "$stage/new.yaml"`,
	}
	if st.Prebuilt() {
		// The images step may name another registry, such as a mirror.
		lines = append(lines, fmt.Sprintf(`sed -i.bak -e %s -e %s "$stage/new.yaml"`,
			ShellQuote("s|^  repo: .*|  repo: "+st.ImageRepo+"|"), ShellQuote("s|^  tag: .*|  tag: "+b.SubstrateVersion(st)+"|")))
	}
	lines = append(lines,
		`ln -s `+ShellQuote(installedDir)+` "$stage/old"`,
		`ln -s `+ShellQuote(nextDir)+` "$stage/new"`,
		`mv "$stage" "$up"`,
		`echo "Set up $up: old.yaml and new.yaml from $record, old and new linked to the two trees"`,
	)
	return execx.Spec{
		Label:            "prepare ~/ate-upgrade",
		Display:          fmt.Sprintf("Set up %s for %s", UpgradeHome, st.ClusterName),
		DisplayIsSummary: true,
		Argv:             []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{
			"Fetching cluster endpoint and auth data.",
			fmt.Sprintf("kubeconfig entry generated for %s.", st.ClusterName),
			fmt.Sprintf("Installing kubectl-ate from substrate@%s", shorten(st.InstalledCommit)),
			"Creating the snapshot transfer service account",
			fmt.Sprintf("Set up %s: old.yaml and new.yaml from %s, old and new linked to the two trees", UpgradeHome, record),
		},
	}
}

// UpgradeShellLine is what PrepareUpgrade cannot do for the shell the runbook
// runs from: move it to the new tree and set the variables the runbook reads.
// The runbook's step 3 builds a pre-built worker image from NEW_RELEASE. A
// build from source takes its version from VERSION, which the record does not
// keep; pre-built images take it from the tag in new.yaml, unless a VERSION
// left in the shell overrides it.
func (b *Builder) UpgradeShellLine(st *state.Setup) string {
	next := versionWord(b.SubstrateVersion(st))
	if st.Prebuilt() {
		return fmt.Sprintf("cd %s/new && export NEW_RELEASE=%s && unset VERSION", UpgradeHome, next)
	}
	return fmt.Sprintf("cd %s/new && export VERSION=%s", UpgradeHome, next)
}

// versionWord renders a version as a shell word for a line the user pastes.
// A version is an image tag, which needs no quoting; anything else is quoted
// rather than trusted.
func versionWord(v string) string {
	if imageTag.MatchString(v) {
		return v
	}
	return ShellQuote(v)
}

// UpgradeNotes are what the hand-over line does not cover: a new shell, and
// the version a rollback from source runs as.
func (b *Builder) UpgradeNotes(st *state.Setup) []string {
	notes := []string{"Run it again in any new shell before going on."}
	if !st.Prebuilt() {
		notes = append(notes, fmt.Sprintf("A rollback runs from %s/old: export VERSION=%s there first.", UpgradeHome, versionWord(st.InstalledVersion)))
	}
	return notes
}

// UpgradeSummary is the hand-over printed when the installer exits: the
// runbook, the line to run, then the notes.
func (b *Builder) UpgradeSummary(st *state.Setup) string {
	lines := []string{"Run this in the shell you upgrade from, then follow upstream's upgrade runbook from step 1:", "  " + RunbookURL, "", "  " + b.UpgradeShellLine(st)}
	for _, note := range b.UpgradeNotes(st) {
		lines = append(lines, "", note)
	}
	return strings.Join(lines, "\n")
}
