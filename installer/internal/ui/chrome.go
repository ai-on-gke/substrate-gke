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
	"slices"
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
	// The cluster is known once its choosing step is behind us: Cluster on
	// an install, the installed-cluster step on an upgrade.
	chooser := state.Cluster
	if a.deps.Setup.Upgrade {
		chooser = state.UpgradeSource
	}
	chooserPos, _ := a.mach.Position(chooser)
	curPos, _ := a.mach.Position(a.mach.Current())
	if a.mach.Current() == state.Complete || curPos > chooserPos {
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
	total := a.mach.NumberedSteps()

	var b strings.Builder
	num, numbered := a.mach.Position(cur)
	switch {
	case cur == state.Complete && a.deps.Setup.Upgrade:
		b.WriteString(theme.Good.Render(" Upgrade prepared"))
	case cur == state.Complete:
		b.WriteString(theme.Good.Render(" Setup complete"))
	case !numbered:
		b.WriteString(theme.Subtle.Render(" Getting started"))
	default:
		b.WriteString(theme.Subtle.Render(fmt.Sprintf(" Step %d of %d", num, total)))
	}
	b.WriteString("\n")

	filled := num
	if cur == state.Complete {
		filled = total
	}
	bar := strings.Repeat("█", filled*2) + strings.Repeat("░", (total-filled)*2)
	b.WriteString(" " + theme.Accent.Render(bar) + "\n\n")

	for _, s := range a.mach.Order() {
		n, ok := a.mach.Position(s)
		if !ok {
			continue
		}
		glyph, style := theme.GlyphPending, theme.Fainted
		switch {
		case n < num || cur == state.Complete:
			glyph, style = theme.GlyphDone, theme.Good
		case s == cur:
			glyph, style = theme.GlyphActive, theme.Title
		}
		// Some titles are wider than the rail. Wrapped by the Width below,
		// their second line started at column 0, under the glyphs.
		prefix := " " + style.Render(glyph) + " " + style.Render(fmt.Sprintf("%d.", n)) + " "
		b.WriteString(hangingIndent(prefix, style.Render(s.Title()), w) + "\n")
	}

	return lipgloss.NewStyle().Width(w).Height(h).Render(b.String())
}

// hangingIndent wraps text to fit width after first, which prefixes the first
// line; every later line is indented by first's width, so a wrapped list item
// or step title stays aligned under its own text.
func hangingIndent(first, text string, width int) string {
	pad := lipgloss.Width(first)
	wrapped := lipgloss.NewStyle().Width(max(width-pad, 1)).Render(text)
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, " ")
		if i == 0 {
			lines[i] = first + line
		} else {
			lines[i] = strings.Repeat(" ", pad) + line
		}
	}
	return strings.Join(lines, "\n")
}

// bottomView is the persistent keymap bar (k9s-style, from the prototype).
func (a *App) bottomView() string {
	var line string
	if a.over == overlayLog {
		line = hintBar(a.width, []Hint{
			{"esc/q/v", "close log"},
			{"↑/↓, j/k", "scroll"},
			{"g/G", "top/bottom"},
		}, []Hint{{"ctrl+c", "exit"}})
	} else {
		hints := a.cur.Hints()
		if p, ok := a.cur.(execCompProvider); ok && p.logComp() != nil && len(p.logComp().lines) > 0 {
			hints = append(hints, Hint{"v", "view log"})
		}
		line = hintBar(a.width, hints, []Hint{{"?", "help"}, {"/", "commands"}, {"ctrl+c", "exit"}})
	}
	return theme.Fainted.Render(strings.Repeat("─", max(a.width, 1))) + "\n" + line
}

// hintBar lays out one key bar in width columns: the screen's own keys, then
// the global ones. Nothing is left to be cut at the terminal's edge; when
// the bar does not fit, it gives things up in a fixed order instead:
//
//  1. the global keys other than the last, front to back — [/] commands,
//     then [?] help, which every other screen's bar shows;
//  2. the dots between keys;
//  3. the screen's own keys, from the right, with an ellipsis in their place
//     so a hidden key is known to exist. These are shown nowhere else, which
//     is why they outlast help.
//
// The last global key — [ctrl+c] exit, the way out — is never dropped.
func hintBar(width int, hints, global []Hint) string {
	render := func(hints []Hint, elided bool, global []Hint, sep string) string {
		parts := make([]string, 0, len(hints)+len(global)+1)
		for _, h := range hints {
			parts = append(parts, theme.Key.Render("["+h.Key+"]")+" "+theme.Subtle.Render(h.Label))
		}
		if elided {
			parts = append(parts, theme.Fainted.Render("…"))
		}
		for _, h := range global {
			parts = append(parts, theme.Key.Render("["+h.Key+"]")+" "+theme.Subtle.Render(h.Label))
		}
		return " " + strings.Join(parts, theme.Fainted.Render(sep))
	}
	fits := func(line string) bool { return lipgloss.Width(line) <= width }

	line := render(hints, false, global, " · ")
	// The global keys go from the one before exit backwards: [/] commands,
	// then [?] help.
	for !fits(line) && len(global) > 1 {
		global = slices.Concat(global[:len(global)-2], global[len(global)-1:])
		line = render(hints, false, global, " · ")
	}
	if !fits(line) {
		line = render(hints, false, global, "  ")
	}
	for elided := false; !fits(line) && len(hints) > 0; {
		hints, elided = hints[:len(hints)-1], true
		line = render(hints, elided, global, "  ")
	}
	return line
}

// helpView is the [?] modal.
func (a *App) helpView(w int) string {
	rows := [][2]string{
		{"enter", "confirm / continue"},
		{"↑/↓, j/k, 1-9", "move / choose an option"},
		{"b, esc", "go back one step"},
		{"r", "re-run the current step's checks or command"},
		{"v", "view full command output / scrollable log"},
		{"m", "toggle the command drawer (deploy steps)"},
		{"/", "slash commands: /help /back /skip /log /exit"},
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

// clampHeight truncates rendered content to at most h lines, keeping the top.
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

// clampHeightAroundFailure clamps overflowing content to the window that
// keeps the failure banner and what follows it — the cause, the [v]/[r]
// guidance, the log path — on screen. The window starts at the banner
// (guidance below beats context above), sliding up only when the banner is
// close enough to the bottom to leave the window short. The caller decides
// when to use it off the component's own failed state; bottom-anchoring
// unconditionally would drop the banner whenever the tail panel below it is
// taller than the window.
func clampHeightAroundFailure(s string, h int) string {
	if h <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= h {
		return s
	}
	for i, l := range lines {
		if strings.Contains(l, "Command failed") {
			start := min(i, len(lines)-h)
			return strings.Join(lines[start:start+h], "\n")
		}
	}
	return strings.Join(lines[:h], "\n")
}
