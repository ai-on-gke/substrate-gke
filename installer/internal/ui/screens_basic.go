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
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// ─── Welcome ───────────────────────────────────────────────────────────────

type welcomeScreen struct {
	deps   *Deps
	cursor int
}

func newWelcomeScreen(deps *Deps) *welcomeScreen {
	cursor := 0
	switch {
	case deps.Setup.Upgrade:
		cursor = 2
	case deps.Setup.Track == state.TrackAdvanced:
		cursor = 1
	}
	return &welcomeScreen{deps: deps, cursor: cursor}
}

func (s *welcomeScreen) Init() tea.Cmd      { return nil }
func (s *welcomeScreen) CapturesText() bool { return false }

func (s *welcomeScreen) Hints() []Hint {
	return []Hint{{"1/2/3", "choose a track"}, {"enter", "begin"}}
}

func (s *welcomeScreen) Update(msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "up", "k":
		s.cursor = max(s.cursor-1, 0)
	case "down", "j":
		s.cursor = min(s.cursor+1, 2)
	case "1":
		s.cursor = 0
	case "2":
		s.cursor = 1
	case "3":
		s.cursor = 2
	case "enter":
		st := s.deps.Setup
		st.Upgrade = s.cursor == 2
		if s.cursor == 1 {
			st.Track = state.TrackAdvanced
		} else {
			st.Track = state.TrackQuickstart
		}
		return goNext
	}
	return nil
}

func (s *welcomeScreen) View(w int) string {
	var b strings.Builder
	b.WriteString("\n" + theme.Logo() + "\n\n")
	b.WriteString(theme.Subtle.Render("High-density AI agent sandboxing on Kubernetes — GKE installer") + "\n")
	b.WriteString(theme.Fainted.Render("substrate: "+s.deps.Builder.Version) + "\n\n")

	tracks := []struct{ name, desc string }{
		{"Quickstart (recommended)", "Sensible defaults; you pick the project, zone, and cluster."},
		{"Advanced", "Also configure machine type, network, bucket, and image registry."},
		{"Upgrade an installed cluster", "Fetch the trees and write the environment for upstream's rolling\nupgrade runbook. Nothing in GCP is touched."},
	}
	for i, t := range tracks {
		label := fmt.Sprintf("[%d] %s", i+1, t.name)
		panel := theme.Panel
		title := theme.Subtle
		if i == s.cursor {
			panel, title = theme.AccentPanel, theme.Title
		}
		b.WriteString(panel.Width(min(w-4, 70)).Render(title.Render(label)+"\n"+theme.Subtle.Render(t.desc)) + "\n")
	}

	b.WriteString("\n" + theme.Subtle.Render("This wizard provisions GCP resources, then builds and installs the"))
	b.WriteString("\n" + theme.Subtle.Render("Substrate control plane onto a GKE cluster. Every step shows the real"))
	b.WriteString("\n" + theme.Subtle.Render("command it runs and streams its output."))
	return b.String()
}

// ─── Check your setup (doctor) ─────────────────────────────────────────────

type doctorResMsg struct {
	owner *doctorScreen
	index int
	res   doctor.Result
}

type doctorScreen struct {
	deps    *Deps
	checks  []doctor.Check
	results []*doctor.Result
	next    int
	frame   int
}

func newDoctorScreen(deps *Deps) *doctorScreen {
	return &doctorScreen{
		deps:    deps,
		checks:  deps.Checks,
		results: make([]*doctor.Result, len(deps.Checks)),
	}
}

func (s *doctorScreen) Init() tea.Cmd {
	return tea.Batch(s.runCheck(0), s.tick())
}

func (s *doctorScreen) tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return frameMsg{owner: s} })
}

func (s *doctorScreen) runCheck(i int) tea.Cmd {
	if i >= len(s.checks) {
		return nil
	}
	check := s.checks[i]
	return func() tea.Msg {
		if s.deps.DryRun {
			time.Sleep(300 * time.Millisecond)
			return doctorResMsg{s, i, doctor.Result{Status: doctor.Pass, Detail: "(dry-run) simulated"}}
		}
		res := check.Run(context.Background())
		return doctorResMsg{s, i, res}
	}
}

func (s *doctorScreen) done() bool { return s.next >= len(s.checks) }

func (s *doctorScreen) blocked() bool {
	for i, r := range s.results {
		if r != nil && r.Status == doctor.Fail && s.checks[i].Fatal {
			return true
		}
	}
	return false
}

func (s *doctorScreen) CapturesText() bool { return false }

func (s *doctorScreen) Hints() []Hint {
	hints := []Hint{{"r", "re-run checks"}, {"b", "back"}}
	if s.done() && !s.blocked() {
		hints = append([]Hint{{"enter", "continue"}}, hints...)
	}
	return hints
}

func (s *doctorScreen) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case doctorResMsg:
		if m.owner != s {
			return nil
		}
		res := m.res
		s.results[m.index] = &res
		s.next = m.index + 1
		return s.runCheck(s.next)

	case frameMsg:
		if m.owner != s {
			return nil
		}
		if !s.done() {
			s.frame++
			return s.tick()
		}
		return nil

	case tea.KeyMsg:
		switch m.String() {
		case "enter":
			if s.done() && !s.blocked() {
				return goNext
			}
		case "r":
			s.results = make([]*doctor.Result, len(s.checks))
			s.next = 0
			return tea.Batch(s.runCheck(0), s.tick())
		case "b", "esc", "left":
			return goBack
		}
	}
	return nil
}

func (s *doctorScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Check your setup") + "\n")
	b.WriteString(theme.Subtle.Render("Real probes of the tools this installer drives — nothing is mocked.") + "\n\n")

	for i, check := range s.checks {
		res := s.results[i]
		var glyph string
		style := theme.Fainted
		detail := ""
		switch {
		case res == nil && i == s.next:
			glyph, style = theme.SpinnerFrames[s.frame%len(theme.SpinnerFrames)], theme.Title
			detail = "checking…"
		case res == nil:
			glyph = theme.GlyphPending
		case res.Status == doctor.Pass:
			glyph, style, detail = theme.GlyphDone, theme.Good, res.Detail
		case res.Status == doctor.Warn:
			glyph, style, detail = theme.GlyphWarn, theme.Warning, res.Detail
		default:
			glyph, style, detail = theme.GlyphFail, theme.Bad, res.Detail
		}
		b.WriteString(fmt.Sprintf("  %s %-34s %s\n", style.Render(glyph), check.Name, theme.Subtle.Render(detail)))
		if res != nil && res.Status != doctor.Pass && res.Fix != "" {
			b.WriteString(theme.ErrorPanel.Width(min(w-6, 74)).Render(
				theme.Warning.Render("Action required")+"\n"+theme.CommandLine.Render(res.Fix)) + "\n")
		}
	}

	b.WriteString("\n")
	switch {
	case !s.done():
		b.WriteString(theme.Subtle.Render("Running preflight checks…"))
	case s.blocked():
		b.WriteString(theme.Bad.Render("Fix the failed checks above, then press [r] to re-run."))
	default:
		b.WriteString(theme.Good.Render("All checks passed. Press [enter] to continue."))
	}
	return b.String()
}
