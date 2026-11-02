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
		width, wantLines  int // wantLines 0: any number
	}{
		{"fits on one line", "  1. ", "short", 40, 1},
		{"wraps", "  1. ", "Upgrade this cluster: exit or restart the installer and choose the upgrade track.", 32, 0},
		{"colored prefix", colored, "The control plane and atelet roll to the new commit at once.", 24, 0},
		{"width below the prefix", "      ", "narrow", 3, 0},
	} {
		got := hangingIndent(tc.first, tc.text, tc.width)
		lines := strings.Split(got, "\n")
		pad := lipgloss.Width(tc.first)
		if tc.wantLines > 0 && len(lines) != tc.wantLines {
			t.Errorf("%s: %d lines, want %d: %q", tc.name, len(lines), tc.wantLines, got)
		}
		if tc.wantLines == 0 && len(lines) < 2 {
			t.Errorf("%s: did not wrap: %q", tc.name, got)
		}
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
// being cut off. Squeezed, it gives up [/] commands first, then the dots,
// then [?] help, and only then the screen's own keys — from the right, with
// an ellipsis standing in — while [ctrl+c] exit survives any width. Each
// cutoff is checked one column either side.
func TestHintBarGivesUpKeysInOrder(t *testing.T) {
	hints := []Hint{{"t", "tear down and reinstall"}, {"esc", "choose another"}, {"r", "re-probe"}}
	global := []Hint{{"?", "help"}, {"/", "commands"}, {"ctrl+c", "exit"}}
	has := func(bar string, keys ...string) bool {
		for _, k := range keys {
			if !strings.Contains(bar, k) {
				return false
			}
		}
		return true
	}
	// Each stage's bar is the widest the next stage has to fit into: a bar
	// one column narrower than a stage's width must have moved on.
	stages := []struct {
		name string
		want func(bar string) bool
	}{
		{"everything, with dots", func(b string) bool { return has(b, "[t]", "[esc]", "[r]", "[?]", "[/]", "[ctrl+c]", " · ") }},
		{"without [/]", func(b string) bool { return has(b, "[t]", "[esc]", "[r]", "[?]", "[ctrl+c]", " · ") && !has(b, "[/]") }},
		{"without the dots", func(b string) bool {
			return has(b, "[t]", "[esc]", "[r]", "[?]", "[ctrl+c]") && !has(b, "[/]") && !has(b, " · ")
		}},
		{"without [?]", func(b string) bool {
			return has(b, "[t]", "[esc]", "[r]", "[ctrl+c]") && !has(b, "[?]") && !has(b, "…")
		}},
		{"without [r], elided", func(b string) bool { return has(b, "[t]", "[esc]", "…", "[ctrl+c]") && !has(b, "[r]") }},
		{"without [esc], elided", func(b string) bool { return has(b, "[t]", "…", "[ctrl+c]") && !has(b, "[esc]") }},
	}
	width := 200
	for i, stage := range stages {
		bar := hintBar(width, hints, global)
		if !stage.want(bar) {
			t.Fatalf("at %d columns, want %s: %q", width, stage.name, bar)
		}
		w := lipgloss.Width(bar)
		if got := hintBar(w, hints, global); got != bar {
			t.Errorf("%s: at its own width %d the bar changed: %q", stage.name, w, got)
		}
		if i+1 < len(stages) {
			next := hintBar(w-1, hints, global)
			if !stages[i+1].want(next) {
				t.Errorf("one column under %s (%d), want %s: %q", stage.name, w, stages[i+1].name, next)
			}
			if lipgloss.Width(next) > w-1 {
				t.Errorf("at %d columns the bar is %d wide: %q", w-1, lipgloss.Width(next), next)
			}
		}
		width = w - 1
	}
	// Down to a width nothing fits in: never wider than asked, down to the
	// narrowest bar there is (the ellipsis and exit), and never without the
	// way out.
	floor := lipgloss.Width(hintBar(1, hints, global))
	for width := 120; width >= 10; width-- {
		bar := hintBar(width, hints, global)
		if lipgloss.Width(bar) > width && width >= floor {
			t.Errorf("at %d columns the bar is %d wide: %q", width, lipgloss.Width(bar), bar)
		}
		if !strings.Contains(bar, "[ctrl+c]") {
			t.Errorf("at %d columns the bar lost [ctrl+c]: %q", width, bar)
		}
	}

	// The log overlay's bar has only exit as its global key.
	if bar := hintBar(30, []Hint{{"esc/q/v", "close log"}, {"g/G", "top/bottom"}}, []Hint{{"ctrl+c", "exit"}}); !strings.Contains(bar, "[ctrl+c]") || lipgloss.Width(bar) > 30 {
		t.Errorf("overlay bar at 30 columns: %q", bar)
	}
}
