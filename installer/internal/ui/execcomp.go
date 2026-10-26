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
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/steps"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// execEvMsg carries one runner event to its owning component.
type execEvMsg struct {
	owner *execComp
	ev    execx.Event
}

const logTail = 8

// execComp runs one external command and renders a live checklist over its
// streamed output. It replaces the prototype's timer-driven fake checklist.
type execComp struct {
	runner  execx.Runner
	spec    execx.Spec
	items   []steps.ChecklistItem
	logPath string

	started  bool
	finished bool
	failed   error
	cause    string
	active   int
	frame    int
	lines    []string
	stderr   []string
	ch       <-chan execx.Event
	cancel   context.CancelFunc
}

func newExecComp(runner execx.Runner, spec execx.Spec, items []steps.ChecklistItem, logPath string) *execComp {
	return &execComp{runner: runner, spec: spec, items: items, logPath: logPath, active: -1}
}

func (c *execComp) LogLines() []string {
	if c == nil {
		return nil
	}
	return c.lines
}

func (c *execComp) LogTitle() string {
	if c == nil {
		return ""
	}
	return c.spec.Display
}

// causeLine matches diagnostic vocabulary on word boundaries, so an echoed
// `kubectl wait --timeout=600s` or a healthy `0 errors, 0 warnings` summary
// is not promoted to the displayed cause. Bare "timeout" is deliberately
// absent — it matches inside every --timeout flag.
var causeLine = regexp.MustCompile(`(?i)\b(error|fatal|failed|failure|denied|forbidden|unauthorized|refused|notfound|not found|does not exist|timed out|deadline exceeded)\b`)

func lineMatchesError(line string) bool { return causeLine.MatchString(line) }

func extractCause(stderr, allLines []string) string {
	// First check recent stderr lines (up to 30) for clear error diagnostics
	var lastStderr string
	start := 0
	if len(stderr) > 30 {
		start = len(stderr) - 30
	}
	for i := len(stderr) - 1; i >= start; i-- {
		line := strings.TrimSpace(stderr[i])
		if line == "" || strings.HasPrefix(line, "exit status ") || strings.HasPrefix(line, "make: ***") {
			continue
		}
		if lineMatchesError(line) {
			return line
		}
		if lastStderr == "" {
			lastStderr = line
		}
	}

	// Next check recent lines in all output (up to 30)
	allStart := 0
	if len(allLines) > 30 {
		allStart = len(allLines) - 30
	}
	for i := len(allLines) - 1; i >= allStart; i-- {
		line := strings.TrimSpace(allLines[i])
		if line == "" || strings.HasPrefix(line, "exit status ") || strings.HasPrefix(line, "make: ***") {
			continue
		}
		if lineMatchesError(line) {
			return line
		}
	}

	if lastStderr != "" {
		return lastStderr
	}
	return ""
}

func (c *execComp) start() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.ch = c.runner.Start(ctx, c.spec)
	c.started = true
	return tea.Batch(c.read(), c.tick())
}

func (c *execComp) restart() tea.Cmd {
	if c.cancel != nil {
		c.cancel()
	}
	c.finished, c.failed, c.cause, c.active, c.lines, c.stderr = false, nil, "", -1, nil, nil
	return c.start()
}

func (c *execComp) stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *execComp) read() tea.Cmd {
	ch := c.ch
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return execEvMsg{owner: c, ev: ev}
	}
}

func (c *execComp) tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return frameMsg{owner: c}
	})
}

func (c *execComp) running() bool { return c.started && !c.finished }
func (c *execComp) ok() bool      { return c.finished && c.failed == nil }

// update consumes this component's messages; handled is false for messages
// that belong to someone else.
func (c *execComp) update(msg tea.Msg) (cmd tea.Cmd, handled bool) {
	switch m := msg.(type) {
	case execEvMsg:
		if m.owner != c {
			return nil, false
		}
		if m.ev.Done {
			c.finished = true
			c.failed = m.ev.Err
			if c.failed != nil {
				c.cause = extractCause(c.stderr, c.lines)
			}
			return nil, true
		}
		c.lines = append(c.lines, m.ev.Line)
		if len(c.lines) > 5000 {
			c.lines = c.lines[len(c.lines)-5000:]
		}
		if m.ev.Stderr {
			c.stderr = append(c.stderr, m.ev.Line)
			if len(c.stderr) > 100 {
				c.stderr = c.stderr[len(c.stderr)-100:]
			}
		}
		c.active = steps.Progress(c.items, c.active, m.ev.Line)
		return c.read(), true

	case frameMsg:
		if m.owner != c {
			return nil, false
		}
		if c.running() {
			c.frame++
			return c.tick(), true
		}
		return nil, true
	}
	return nil, false
}

// view renders the command line, the checklist, and the log tail.
func (c *execComp) view(w int) string {
	var b strings.Builder

	b.WriteString(theme.CommandLine.Render("$ "+c.spec.Display) + "\n\n")

	active := c.active
	if active < 0 && c.running() {
		active = 0
	}
	for i, item := range c.items {
		var glyph string
		var style = theme.Subtle
		switch {
		case c.ok() || i < active:
			glyph, style = theme.GlyphDone, theme.Good
		case i == active && c.failed != nil:
			glyph, style = theme.GlyphFail, theme.Bad
		case i == active && c.running():
			glyph, style = theme.SpinnerFrames[c.frame%len(theme.SpinnerFrames)], theme.Title
		default:
			glyph, style = theme.GlyphPending, theme.Fainted
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", style.Render(glyph), style.Render(item.Label)))
	}
	if len(c.items) > 0 {
		b.WriteString("\n")
	}

	if c.failed != nil {
		var panel strings.Builder
		panel.WriteString(theme.Bad.Render("Command failed: ") + c.failed.Error())
		// c.cause was extracted once when the failure landed; rescanning
		// the 5000-line buffer here would run on every frame.
		if c.cause != "" && c.cause != c.failed.Error() {
			panel.WriteString("\n\n" + theme.Warning.Render("Cause: ") + theme.Subtle.Render(c.cause))
		}
		panel.WriteString("\n\n" + theme.Subtle.Render("Press ") + theme.Key.Render("[v]") + theme.Subtle.Render(" to view full log, ") + theme.Key.Render("[r]") + theme.Subtle.Render(" to retry."))
		if c.logPath != "" {
			panel.WriteString("\n" + theme.Subtle.Render("Log file: ") + theme.Accent.Render(c.logPath))
		}
		b.WriteString(theme.ErrorPanel.Width(w-4).Render(panel.String()) + "\n")
	}

	if len(c.lines) > 0 {
		lw := max(w-8, 20)
		var visualRows []string
		for i := len(c.lines) - 1; i >= 0 && len(visualRows) < logTail; i-- {
			// Chunk by runes, not bytes: every over-long line ends in a
			// multi-byte '…' from Clean, and a byte cut mid-rune renders
			// U+FFFD garbage on adjacent rows.
			runes := []rune(c.lines[i])
			if len(runes) <= lw {
				visualRows = append([]string{string(runes)}, visualRows...)
			} else {
				var chunks []string
				for len(runes) > lw {
					chunks = append(chunks, string(runes[:lw]))
					runes = runes[lw:]
				}
				if len(runes) > 0 {
					chunks = append(chunks, string(runes))
				}
				needed := logTail - len(visualRows)
				if len(chunks) > needed {
					chunks = chunks[len(chunks)-needed:]
				}
				visualRows = append(chunks, visualRows...)
			}
		}

		var log strings.Builder
		for i, row := range visualRows {
			log.WriteString(theme.Subtle.Render(row))
			if i < len(visualRows)-1 {
				log.WriteString("\n")
			}
		}
		b.WriteString(theme.Panel.Width(w - 4).Render(log.String()))
	}

	return b.String()
}
