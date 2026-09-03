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
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// ─── Choose the installed cluster ──────────────────────────────────────────

// commitMsg carries the outcome of finding the installed version's commit.
type commitMsg struct {
	owner *upgradeSourceScreen
	sha   string
	err   error
}

// upgradeSourceScreen names the cluster to upgrade and reads what it runs
// off the cluster itself: the version from the atelet DaemonSet, the images
// from the running API server. It ends with the installed tree and version
// known: the upgrade fetches that tree for rollback, and the runbook reads
// that version as the old one. The commit comes from the version (a build
// from source names it) or from this repository's release pins; when neither
// yields it, it is typed in.
type upgradeSourceScreen struct {
	deps *Deps
	// mode: "target" (project, cluster, location), "reading", "choose"
	// (several running versions), "resolving" (the commit), "manual".
	mode   string
	fields []textinput.Model
	labels []string
	focus  int
	comp   *execComp
	// parsed marks the read's output as consumed: the component keeps
	// reporting ok on later ticks, and the probe must be applied once.
	parsed  bool
	probe   snapshot.Probe
	choices []string
	cursor  int
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

func (s *upgradeSourceScreen) Hints() []Hint {
	switch s.mode {
	case "reading":
		if s.comp != nil && s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"m", "describe the installed Substrate by hand"}, {"b", "back"}}
		}
		return nil
	case "resolving":
		return nil
	case "choose":
		return []Hint{{"↑/↓", "choose"}, {"enter", "confirm"}, {"b", "back"}}
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

// enterManual asks for what could not be read or resolved: the installed
// commit and version, and the snapshot bucket.
func (s *upgradeSourceScreen) enterManual() tea.Cmd {
	st := s.deps.Setup
	s.mode, s.focus, s.errText, s.comp = "manual", 0, "", nil
	s.labels = []string{
		"Installed Substrate commit (full SHA)",
		"Installed version (the ate.dev/substrate-version node label)",
		"Snapshot bucket (blank for the default)",
	}
	s.fields = []textinput.Model{
		newInput(st.InstalledCommit, "40 hex characters"),
		newInput(st.InstalledVersion, "kubectl get nodes -L ate.dev/substrate-version"),
		newInput(st.BucketName, "ate-snapshots-<project>-<location>"),
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
	s.mode, s.errText, s.fields, s.parsed = "reading", "", nil, false
	s.comp = newExecComp(s.deps.Runner, snapshot.ProbeCluster(st, true), nil)
	return s.comp.start()
}

// readDone parses what the read printed and moves on: straight to the
// commit when the cluster runs one version, to a choice when it runs two.
func (s *upgradeSourceScreen) readDone() tea.Cmd {
	probe, err := snapshot.ParseProbe(s.comp.lines)
	if err != nil {
		s.comp.failed = err
		return nil
	}
	s.probe = probe
	if len(probe.Running) == 1 {
		return s.use(probe.Running[0])
	}
	s.choices, s.cursor, s.mode = probe.Running, 0, "choose"
	return nil
}

// use takes version as the installed one and goes looking for its commit.
// A dry run resolves nothing and takes the pin.
func (s *upgradeSourceScreen) use(version string) tea.Cmd {
	st := s.deps.Setup
	s.probe.Apply(st, version)
	if s.deps.DryRun {
		st.InstalledCommit = snapshot.Commit
		return goNext
	}
	s.mode = "resolving"
	prebuilt := s.probe.Prebuilt()
	return func() tea.Msg {
		sha, err := snapshot.InstalledCommit(context.Background(), version, prebuilt)
		return commitMsg{s, sha, err}
	}
}

func (s *upgradeSourceScreen) submitManual() tea.Cmd {
	commit, version, bucket := s.value(0), s.value(1), s.value(2)
	switch {
	case !fullSHA.MatchString(strings.ToLower(commit)):
		s.errText = "The installed commit has to be a full 40-character SHA. A build from source shows its first 12 characters in the version, substrate-<commit>; a pre-built install used the commit this repository pinned for that release."
		return s.setFocus(0)
	case version == "":
		s.errText = "The installed version is required; it is the ate.dev/substrate-version label on the nodes."
		return s.setFocus(1)
	}
	if err := snapshot.CheckImageTag(version); err != nil {
		s.errText = err.Error()
		return s.setFocus(1)
	}
	st := s.deps.Setup
	st.InstalledRepo, st.InstalledCommit, st.InstalledVersion = snapshot.RepoURL, strings.ToLower(commit), version
	st.BucketName = bucket
	if err := st.ApplyProjectDefaults(); err != nil {
		s.errText = err.Error()
		return s.setFocus(2)
	}
	return goNext
}

func (s *upgradeSourceScreen) Update(msg tea.Msg) tea.Cmd {
	if m, ok := msg.(commitMsg); ok {
		if m.owner != s || s.mode != "resolving" {
			return nil
		}
		if m.err == nil && m.sha != "" {
			s.deps.Setup.InstalledCommit = m.sha
			return goNext
		}
		if m.err != nil {
			s.note = m.err.Error()
		} else {
			s.note = fmt.Sprintf("Could not find the commit %s was built from; enter it here.", s.deps.Setup.InstalledVersion)
		}
		return s.enterManual()
	}
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
	case "resolving":
		return nil
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
			if !s.comp.running() {
				s.comp.stop()
				return s.enterTarget()
			}
		}
		return nil
	case "choose":
		switch key.String() {
		case "up", "k":
			s.cursor = max(s.cursor-1, 0)
		case "down", "j":
			s.cursor = min(s.cursor+1, len(s.choices)-1)
		case "enter":
			return s.use(s.choices[s.cursor])
		case "b", "esc":
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
	case "resolving":
		b.WriteString(theme.Accent.Render(fmt.Sprintf("Finding the commit %s was built from…", s.deps.Setup.InstalledVersion)))
		return b.String()
	case "choose":
		b.WriteString(theme.Subtle.Render("The cluster runs two versions, as it does mid-upgrade; pick the one to treat as installed.") + "\n\n")
		for i, v := range s.choices {
			if i == s.cursor {
				b.WriteString(theme.Selected.Render(" "+v+" ") + "\n")
			} else {
				b.WriteString(theme.Subtle.Render("  "+v) + "\n")
			}
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
		b.WriteString(theme.Subtle.Render("The version is the ate.dev/substrate-version label on the nodes. For a build\nfrom source it reads substrate-<commit>; the full commit it abbreviates is at\ngithub.com/agent-substrate/substrate/commit/<those 12 characters>."))
	default:
		b.WriteString(theme.Subtle.Render("The installer reads the running version and images off the cluster.\nCredentials are fetched with gcloud."))
	}
	return b.String()
}

// ─── Prepare the upgrade ───────────────────────────────────────────────────

// upgradePlanScreen fetches the two trees the runbook runs from and hands
// over: it changes nothing on the cluster.
type upgradePlanScreen struct {
	deps                  *Deps
	comp                  *execComp
	installedDir, nextDir string
}

func newUpgradePlanScreen(deps *Deps) *upgradePlanScreen {
	installedDir, nextDir := deps.Builder.UpgradeTrees(deps.UpgradeDir, deps.Setup)
	return &upgradePlanScreen{
		deps:         deps,
		installedDir: installedDir,
		nextDir:      nextDir,
		comp:         newExecComp(deps.Runner, deps.Builder.FetchTrees(deps.Setup, installedDir, nextDir), nil),
	}
}

func (s *upgradePlanScreen) Init() tea.Cmd      { return s.comp.start() }
func (s *upgradePlanScreen) CapturesText() bool { return false }

func (s *upgradePlanScreen) Hints() []Hint {
	switch {
	case s.comp.ok():
		return []Hint{{"enter", "finish"}}
	case s.comp.failed != nil:
		return []Hint{{"r", "retry"}, {"b", "back"}}
	}
	return nil
}

func (s *upgradePlanScreen) Update(msg tea.Msg) tea.Cmd {
	if cmd, handled := s.comp.update(msg); handled {
		return cmd
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter":
			if s.comp.ok() {
				return goNext
			}
		case "r":
			if s.comp.failed != nil {
				return s.comp.restart()
			}
		case "b", "esc":
			if !s.comp.running() {
				s.comp.stop()
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
	b.WriteString(theme.Subtle.Render(fmt.Sprintf(
		"Fetching the installed tree and the new one for %s. Nothing on the cluster changes here.", st.ClusterName)) + "\n\n")
	b.WriteString(s.comp.view(w))
	if s.comp.ok() {
		b.WriteString("\n" + theme.AccentPanel.Width(min(w-4, 96)).Render(
			s.deps.Builder.UpgradeSummary(st, s.installedDir, s.nextDir)) + "\n")
		b.WriteString("\n" + theme.Good.Render("Press [enter] to finish."))
	}
	return b.String()
}
