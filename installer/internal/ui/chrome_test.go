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
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// hangingIndent keeps a wrapped list item or step title under its own text.
// The prefix goes on the first line only, every later line is indented by
// the prefix's visible width — not its byte length, which a colored prefix
// inflates — and no line is wider than asked for.
func TestHangingIndent(t *testing.T) {
	const colored = "\x1b[31m• \x1b[0m" // two columns, many bytes
	for _, tc := range []struct {
		name, first, text string
		width             int
	}{
		{"fits on one line", "  1. ", "short", 40},
		{"wraps", "  1. ", "Upgrade this cluster: exit or restart the installer and choose the upgrade track.", 32},
		{"colored prefix", colored, "The control plane and atelet roll to the new commit at once.", 24},
		{"width below the prefix", "      ", "narrow", 3},
	} {
		got := hangingIndent(tc.first, tc.text, tc.width)
		lines := strings.Split(got, "\n")
		pad := lipgloss.Width(tc.first)
		if !strings.HasPrefix(lines[0], tc.first) {
			t.Errorf("%s: first line %q lacks the prefix %q", tc.name, lines[0], tc.first)
		}
		for i, line := range lines[1:] {
			if !strings.HasPrefix(line, strings.Repeat(" ", pad)) || strings.HasPrefix(line, strings.Repeat(" ", pad+1)) {
				t.Errorf("%s: line %d %q is not indented by exactly %d columns", tc.name, i+2, line, pad)
			}
		}
		// A width below the prefix leaves one column for text; every line
		// is then as wide as the prefix plus that column, and no wider.
		limit := max(tc.width, pad+1)
		for i, line := range lines {
			if w := lipgloss.Width(line); w > limit {
				t.Errorf("%s: line %d is %d wide, want at most %d: %q", tc.name, i+1, w, limit, line)
			}
		}
		// Nothing but whitespace changes: a width below the prefix breaks
		// the text a character per line, so spaces are not compared.
		squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
		if squash(strings.ReplaceAll(got, tc.first, "")) != squash(tc.text) {
			t.Errorf("%s: text changed in wrapping: %q", tc.name, got)
		}
	}
}

// The key bar never runs past the terminal's edge, where the last keys were
// being cut off. Squeezed, it gives up [/] commands first, then [?] help,
// then the dots, and only then the screen's own keys — from the right, with
// an ellipsis standing in — while [ctrl+c] exit survives any width.
func TestHintBarGivesUpKeysInOrder(t *testing.T) {
	hints := []Hint{{"t", "tear down and reinstall"}, {"r", "re-probe"}, {"esc", "choose another"}}
	global := []Hint{{"?", "help"}, {"/", "commands"}, {"ctrl+c", "exit"}}
	full := hintBar(200, hints, global)
	for _, want := range []string{"[t]", "[r]", "[esc]", "[?]", "[/]", "[ctrl+c]", " · "} {
		if !strings.Contains(full, want) {
			t.Errorf("a wide bar lost %q: %q", want, full)
		}
	}
	w := lipgloss.Width(full)

	// Just above and below the threshold: one column short drops exactly
	// [/] commands and nothing else.
	if got := hintBar(w, hints, global); got != full {
		t.Errorf("at its own width the bar changed: %q", got)
	}
	short := hintBar(w-1, hints, global)
	if strings.Contains(short, "[/]") || !strings.Contains(short, "[?]") || !strings.Contains(short, "[esc]") {
		t.Errorf("one column short should drop only [/] commands: %q", short)
	}
	if lipgloss.Width(short) > w-1 {
		t.Errorf("bar is wider than %d: %q", w-1, short)
	}

	// Narrowing further: help goes before the screen's keys, the screen's
	// keys go last and leave an ellipsis, and exit is always there. At no
	// width does the bar overflow.
	seenEllipsis := false
	for width := w - 1; width >= 20; width-- {
		bar := hintBar(width, hints, global)
		if lipgloss.Width(bar) > width {
			t.Errorf("at %d columns the bar is %d wide: %q", width, lipgloss.Width(bar), bar)
		}
		if !strings.Contains(bar, "[ctrl+c]") {
			t.Errorf("at %d columns the bar lost [ctrl+c]: %q", width, bar)
		}
		if strings.Contains(bar, "[?]") && !strings.Contains(bar, "[esc]") {
			t.Errorf("at %d columns help outlived the screen's own keys: %q", width, bar)
		}
		if strings.Contains(bar, "…") {
			seenEllipsis = true
			if strings.Contains(bar, "[esc]") {
				t.Errorf("at %d columns the ellipsis shows with nothing hidden: %q", width, bar)
			}
		}
	}
	if !seenEllipsis {
		t.Error("no width hid a screen key behind an ellipsis")
	}

	// The log overlay's bar has only exit as its global key.
	if bar := hintBar(30, []Hint{{"esc/q/v", "close log"}, {"g/G", "top/bottom"}}, []Hint{{"ctrl+c", "exit"}}); !strings.Contains(bar, "[ctrl+c]") || lipgloss.Width(bar) > 30 {
		t.Errorf("overlay bar at 30 columns: %q", bar)
	}
}
