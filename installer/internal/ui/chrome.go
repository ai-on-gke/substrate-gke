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
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// headerView is the top brand bar with the target context badge.
func (a *App) headerView() string {
	left := theme.Title.Render(" AGENT SUBSTRATE ") + theme.Subtle.Render("· GKE installer")

	var badges []string
	if a.deps.DryRun {
		badges = append(badges, theme.Warning.Render("DRY-RUN"))
	}
	if p := a.deps.Setup.ProjectID; p != "" {
		badges = append(badges, theme.Subtle.Render("project: ")+theme.Accent.Render(p))
	}
	if a.mach.Current() > state.Cluster {
		badges = append(badges, theme.Subtle.Render("cluster: ")+theme.Accent.Render(a.deps.Setup.ClusterName))
	}
	right := strings.Join(badges, theme.Fainted.Render("  │  "))

	gap := a.width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	return line + "\n" + theme.Fainted.Render(strings.Repeat("─", max(a.width, 1)))
}

// sidebarView is the step rail: progress plus the numbered timeline.
func (a *App) sidebarView(w, h int) string {
	cur := a.mach.Current()

	var b strings.Builder
	num, numbered := cur.Number()
	switch {
	case cur == state.Complete:
		b.WriteString(theme.Good.Render(" Setup complete"))
	case !numbered:
		b.WriteString(theme.Subtle.Render(" Getting started"))
	default:
		b.WriteString(theme.Subtle.Render(fmt.Sprintf(" Step %d of %d", num, state.NumberedSteps)))
	}
	b.WriteString("\n")

	filled := int(cur)
	if cur == state.Complete {
		filled = state.NumberedSteps
	}
	bar := strings.Repeat("█", filled*2) + strings.Repeat("░", (state.NumberedSteps-filled)*2)
	b.WriteString(" " + theme.Accent.Render(bar) + "\n\n")

	for _, s := range state.Order {
		n, ok := s.Number()
		if !ok {
			continue
		}
		glyph, style := theme.GlyphPending, theme.Fainted
		switch {
		case s < cur:
			glyph, style = theme.GlyphDone, theme.Good
		case s == cur:
			glyph, style = theme.GlyphActive, theme.Title
		}
		b.WriteString(fmt.Sprintf(" %s %s\n", style.Render(glyph), style.Render(fmt.Sprintf("%d. %s", n, s.Title()))))
	}

	return lipgloss.NewStyle().Width(w).Height(h).Render(b.String())
}

// bottomView is the persistent keymap bar (k9s-style, from the prototype).
func (a *App) bottomView() string {
	hints := a.cur.Hints()
	hints = append(hints,
		Hint{"?", "help"},
		Hint{"/", "commands"},
		Hint{"ctrl+c", "exit"},
	)
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, theme.Key.Render("["+hint.Key+"]")+" "+theme.Subtle.Render(hint.Label))
	}
	line := " " + strings.Join(parts, theme.Fainted.Render("  ·  "))
	return theme.Fainted.Render(strings.Repeat("─", max(a.width, 1))) + "\n" + line
}

// helpView is the [?] modal.
func (a *App) helpView(w int) string {
	rows := [][2]string{
		{"enter", "confirm / continue"},
		{"↑/↓, j/k, 1-9", "move / choose an option"},
		{"b, esc", "go back one step"},
		{"r", "re-run the current step's checks or command"},
		{"m", "toggle the command drawer (deploy steps)"},
		{"/", "slash commands: /help /back /skip /exit"},
		{"ctrl+c", "exit (asks for confirmation)"},
	}
	var b strings.Builder
	b.WriteString(theme.Title.Render("Keyboard reference") + "\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %s  %s\n", theme.Key.Render(fmt.Sprintf("%-14s", r[0])), r[1]))
	}
	b.WriteString("\n" + theme.Subtle.Render("Press any key to close."))
	return theme.AccentPanel.Width(min(w-2, 64)).Render(b.String())
}

// exitView is the quit confirmation modal.
func (a *App) exitView(w int) string {
	msg := theme.Warning.Render("Exit setup?") + "\n\n" +
		"Your GCP resources are left exactly as they are; re-running the\n" +
		"installer is safe (every step is idempotent).\n\n" +
		theme.Key.Render("[y]") + " exit   " + theme.Key.Render("[n]") + " keep going"
	return theme.ErrorPanel.Width(min(w-2, 64)).Render(msg)
}

// clampHeight truncates rendered content to at most h lines.
func clampHeight(s string, h int) string {
	if h <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= h {
		return s
	}
	return strings.Join(lines[:h], "\n")
}
