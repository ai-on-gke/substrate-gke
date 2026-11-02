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

package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// ─── Choose the installed cluster ──────────────────────────────────────────

// upgradeSourceScreen names the cluster to upgrade and reads the version and
// commit its API server reports. It ends with the installed tree and version
// known: the upgrade fetches that tree for rollback, and a rollback from a
// build from source runs as that version. What the cluster cannot say is
// typed in.
type upgradeSourceScreen struct {
	deps *Deps
	// mode: "target" (project, cluster, location), "reading", "manual".
	mode   string
	fields []textinput.Model
	labels []string
	focus  int
	comp   *execComp
	// parsed marks the read's output as consumed: the component keeps
	// reporting ok on later ticks, and the probe must be applied once.
	parsed  bool
	errText string
	note    string
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func newUpgradeSourceScreen(deps *Deps) *upgradeSourceScreen {
	s := &upgradeSourceScreen{deps: deps}
	s.enterTarget()
	return s
}

func (s *upgradeSourceScreen) Init() tea.Cmd      { return nil }
func (s *upgradeSourceScreen) CapturesText() bool { return s.mode == "target" || s.mode == "manual" }
func (s *upgradeSourceScreen) logComp() *execComp { return s.comp }

// Stop ends a read left running when the screen is navigated away from.
func (s *upgradeSourceScreen) Stop() {
	if s.comp != nil {
		s.comp.stop()
	}
}

func (s *upgradeSourceScreen) Hints() []Hint {
	switch s.mode {
	case "reading":
		if s.comp != nil && s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"m", "describe the installed Substrate by hand"}, {"b", "back"}}
		}
		return []Hint{{"esc", "cancel"}}
	}
	return []Hint{{"tab/↓", "next field"}, {"enter", "confirm"}, {"esc", "back"}}
}

func (s *upgradeSourceScreen) enterTarget() tea.Cmd {
	st := s.deps.Setup
	s.mode, s.focus, s.errText, s.note, s.comp = "target", 0, "", "", nil
	s.labels = []string{"Project ID", "Cluster name", "Cluster location"}
	s.fields = []textinput.Model{
		newInput(st.ProjectID, "my-project"),
		newInput(st.ClusterName, "substrate-test"),
		newInput(st.Zone, "us-west1-c"),
	}
	return tea.Batch(s.fields[0].Focus(), textinput.Blink)
}

// enterManual asks for the installed commit and version.
func (s *upgradeSourceScreen) enterManual() tea.Cmd {
	st := s.deps.Setup
	s.mode, s.focus, s.errText, s.comp = "manual", 0, "", nil
	s.labels = []string{"Installed Substrate commit (full SHA)", "Installed version"}
	s.fields = []textinput.Model{
		newInput(st.InstalledCommit, "40 hex characters"),
		newInput(st.InstalledVersion, "v0.6.0"),
	}
	return tea.Batch(s.fields[0].Focus(), textinput.Blink)
}

func (s *upgradeSourceScreen) setFocus(i int) tea.Cmd {
	s.fields[s.focus].Blur()
	s.focus = (i + len(s.fields)) % len(s.fields)
	return s.fields[s.focus].Focus()
}

func (s *upgradeSourceScreen) value(i int) string { return strings.TrimSpace(s.fields[i].Value()) }

// submitTarget records the cluster and starts reading it.
func (s *upgradeSourceScreen) submitTarget() tea.Cmd {
	project, cluster, zone := s.value(0), s.value(1), s.value(2)
	switch {
	case project == "":
		s.errText = "A project ID is required."
		return s.setFocus(0)
	case cluster == "":
		s.errText = "A cluster name is required."
		return s.setFocus(1)
	case zone == "":
		s.errText = "A cluster location is required."
		return s.setFocus(2)
	}
	st := s.deps.Setup
	st.ProjectID, st.ClusterName, st.Zone = project, cluster, zone
	// Whatever an earlier read learned belonged to the cluster it named.
	st.InstalledCommit, st.InstalledVersion = "", ""
	s.mode, s.errText, s.fields, s.parsed = "reading", "", nil, false
	s.comp = newExecComp(s.deps.Runner, snapshot.ProbeCluster(st, true), nil, s.deps.LogPath)
	return s.comp.start()
}

// readDone takes what the API server reported as the installed side, and
// asks for it when the binary could not say.
func (s *upgradeSourceScreen) readDone() tea.Cmd {
	st := s.deps.Setup
	snapshot.ParseProbe(s.comp.lines).Apply(st)
	if st.InstalledCommit != "" {
		return goNext
	}
	s.note = "The API server did not report the version and commit it was built from; enter them here."
	return s.enterManual()
}

func (s *upgradeSourceScreen) submitManual() tea.Cmd {
	commit, version := s.value(0), s.value(1)
	switch {
	case !fullSHA.MatchString(strings.ToLower(commit)):
		s.errText = "The installed commit has to be a full 40-character SHA. The running API server prints it: kubectl -n ate-system exec deploy/ate-api-server -- /ko-app/ateapi --version."
		return s.setFocus(0)
	case version == "":
		s.errText = "The installed version is required. The running API server prints it first for --version."
		return s.setFocus(1)
	}
	if err := snapshot.CheckImageTag(version); err != nil {
		s.errText = err.Error()
		return s.setFocus(1)
	}
	st := s.deps.Setup
	st.InstalledCommit, st.InstalledVersion = strings.ToLower(commit), version
	return goNext
}

func (s *upgradeSourceScreen) Update(msg tea.Msg) tea.Cmd {
	if s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			if s.comp.ok() && s.mode == "reading" && !s.parsed {
				s.parsed = true
				return s.readDone()
			}
			return cmd
		}
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		if s.fields == nil {
			return nil
		}
		var cmd tea.Cmd
		s.fields[s.focus], cmd = s.fields[s.focus].Update(msg)
		return cmd
	}
	switch s.mode {
	case "reading":
		switch key.String() {
		case "r":
			if s.comp.failed != nil {
				s.parsed = false
				return s.comp.restart()
			}
		case "m":
			if s.comp.failed != nil {
				return s.enterManual()
			}
		case "b", "esc":
			// Also while it runs: a read that hangs on gcloud or kubectl
			// has no timeout, and its late get-credentials would otherwise
			// retarget kubectl under a later read.
			s.comp.stop()
			return s.enterTarget()
		}
		return nil
	}
	switch key.String() {
	case "esc":
		if s.mode == "manual" {
			return s.enterTarget()
		}
		return goBack
	case "tab", "down":
		return s.setFocus(s.focus + 1)
	case "shift+tab", "up":
		return s.setFocus(s.focus - 1)
	case "enter":
		if s.focus < len(s.fields)-1 {
			return s.setFocus(s.focus + 1)
		}
		if s.mode == "manual" {
			return s.submitManual()
		}
		return s.submitTarget()
	}
	var cmd tea.Cmd
	s.fields[s.focus], cmd = s.fields[s.focus].Update(msg)
	return cmd
}

func (s *upgradeSourceScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose the installed cluster") + "\n")
	b.WriteString(theme.Subtle.Render("The cluster to upgrade, and the Substrate it runs today.") + "\n\n")

	switch s.mode {
	case "reading":
		b.WriteString(s.comp.view(w))
		if s.comp.failed != nil {
			b.WriteString("\n" + theme.Subtle.Render("Could not read the installed Substrate. Press [r] to retry, or [m] to describe it by hand."))
		}
		return b.String()
	}

	for i, f := range s.fields {
		label := theme.Subtle
		if i == s.focus {
			label = theme.Title
		}
		b.WriteString(label.Render("  "+s.labels[i]) + "\n")
		b.WriteString(f.View() + "\n")
	}
	b.WriteString("\n")
	switch {
	case s.errText != "":
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 74)).Render(theme.Bad.Render(s.errText)))
	case s.mode == "manual" && s.note != "":
		b.WriteString(theme.Warning.Render(s.note))
	case s.mode == "manual":
		b.WriteString(theme.Subtle.Render("The running API server prints both for --version: the version first, then the\ncommit the images were built from."))
	default:
		b.WriteString(theme.Subtle.Render("The installer reads the running version and commit off the cluster's API server.\nCredentials are fetched with gcloud."))
	}
	return b.String()
}

// ─── Prepare the upgrade ───────────────────────────────────────────────────

// upgradePlanScreen fetches the two trees the runbook runs from, then, when
// asked, sets up ~/ate-upgrade the way the runbook's "Before you start" does,
// and hands over. Nothing on the cluster changes. It refuses to prepare an
// upgrade the runbook does not support.
type upgradePlanScreen struct {
	deps                  *Deps
	fetch, prepare        *execComp
	installedDir, nextDir string
	// blocked says why nothing was fetched; the only way on is back.
	blocked string
}

func newUpgradePlanScreen(deps *Deps) *upgradePlanScreen {
	s := &upgradePlanScreen{deps: deps}
	st := deps.Setup
	next := deps.Builder.SubstrateVersion(st)
	err := snapshot.CheckUpgrade(st.InstalledVersion, next)
	if err == nil {
		err = snapshot.CheckImageSource(st)
	}
	if err != nil {
		s.blocked = fmt.Sprintf("Cannot upgrade %s to %s in place: %v. Go back and choose other images, or reinstall.", st.InstalledVersion, next, err)
		return s
	}
	s.installedDir, s.nextDir = deps.Builder.UpgradeTrees(deps.UpgradeDir, st)
	s.fetch = newExecComp(deps.Runner, deps.Builder.FetchTrees(st, s.installedDir, s.nextDir), nil, deps.LogPath)
	return s
}

func (s *upgradePlanScreen) Init() tea.Cmd {
	if s.fetch == nil {
		return nil
	}
	return s.fetch.start()
}
func (s *upgradePlanScreen) CapturesText() bool { return false }

// current is the command the screen is on: the set-up once it has begun,
// else the fetch.
func (s *upgradePlanScreen) current() *execComp {
	if s.prepare != nil {
		return s.prepare
	}
	return s.fetch
}

func (s *upgradePlanScreen) logComp() *execComp { return s.current() }

// Stop ends a command left running when the screen is navigated away from.
func (s *upgradePlanScreen) Stop() {
	if c := s.current(); c != nil {
		c.stop()
	}
}

func (s *upgradePlanScreen) Hints() []Hint {
	c := s.current()
	switch {
	case c == nil:
		return []Hint{{"b", "back"}}
	case c.ok() && s.prepare == nil:
		return []Hint{{"enter", "set up " + snapshot.UpgradeHome}, {"b", "back"}}
	case c.ok():
		return []Hint{{"enter", "finish"}}
	case c.failed != nil:
		return []Hint{{"r", "retry"}, {"b", "back"}}
	}
	return nil
}

func (s *upgradePlanScreen) Update(msg tea.Msg) tea.Cmd {
	c := s.current()
	if c == nil {
		if key, ok := msg.(tea.KeyMsg); ok && (key.String() == "b" || key.String() == "esc") {
			return goBack
		}
		return nil
	}
	if cmd, handled := c.update(msg); handled {
		return cmd
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter":
			switch {
			case c.ok() && s.prepare == nil:
				s.prepare = newExecComp(s.deps.Runner, s.deps.Builder.PrepareUpgrade(s.deps.Setup, s.installedDir, s.nextDir), nil, s.deps.LogPath)
				return s.prepare.start()
			case c.ok():
				return goNext
			}
		case "r":
			if c.failed != nil {
				return c.restart()
			}
		case "b", "esc":
			if !c.running() {
				c.stop()
				return goBack
			}
		}
	}
	return nil
}

func (s *upgradePlanScreen) View(w int) string {
	st := s.deps.Setup
	var b strings.Builder
	b.WriteString(theme.Title.Render("Prepare the upgrade") + "\n")
	if s.blocked != "" {
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 74)).Render(theme.Bad.Render(s.blocked)))
		return b.String()
	}
	if s.prepare == nil {
		b.WriteString(theme.Subtle.Width(w-4).Render(fmt.Sprintf(
			"Fetching the installed tree and the new one for %s. Nothing on the cluster changes here.", st.ClusterName)) + "\n\n")
		b.WriteString(s.fetch.view(w))
		if s.fetch.ok() {
			b.WriteString("\n" + theme.Title.Render("Next: set up "+snapshot.UpgradeHome+", the directory the upgrade runbook works in") + "\n")
			b.WriteString(theme.Subtle.Width(w-4).Render(strings.Join([]string{
				"  · old.yaml and new.yaml: copies of the cluster's ate-setup record",
				"  · old and new: links to the two trees",
				"  · kubectl-ate from the installed release",
				"  · the cluster's credentials and the snapshot transfer service account",
			}, "\n")) + "\n\n")
			b.WriteString(theme.Good.Render("Press [enter] to set up " + snapshot.UpgradeHome + "."))
		}
		return b.String()
	}
	b.WriteString(theme.Subtle.Width(w-4).Render(fmt.Sprintf(
		"Setting up %s for %s. Nothing on the cluster changes here.", snapshot.UpgradeHome, st.ClusterName)) + "\n\n")
	b.WriteString(s.prepare.view(w))
	if s.prepare.ok() {
		b.WriteString("\n" + handOver(s.deps, w))
		b.WriteString("\n" + theme.Good.Render("Press [enter] to finish.") + theme.Subtle.Render("  The line is printed again when the installer exits."))
	}
	return b.String()
}

// handOver renders the upgrade hand-over: the runbook as the next step, and
// the line to run in a panel of its own, at full width because a wrapped
// line is copied short, with the notes under it.
func handOver(deps *Deps, w int) string {
	st, builder := deps.Setup, deps.Builder
	var b strings.Builder
	b.WriteString(theme.Title.Render("Next: run this in the shell you upgrade from, then follow the upgrade runbook from step 1") + "\n")
	b.WriteString(theme.Accent.Render("  "+builder.RunbookURL()) + "\n\n")
	b.WriteString(theme.AccentPanel.Width(w-4).Render(builder.UpgradeShellLine(st)) + "\n")
	for _, note := range builder.UpgradeNotes(st) {
		b.WriteString("\n" + theme.Warning.Width(w-4).Render(note) + "\n")
	}
	return b.String()
}
