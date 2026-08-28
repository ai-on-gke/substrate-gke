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
	runner execx.Runner
	spec   execx.Spec
	items  []steps.ChecklistItem

	started  bool
	finished bool
	failed   error
	active   int
	frame    int
	lines    []string
	ch       <-chan execx.Event
	cancel   context.CancelFunc
}

func newExecComp(runner execx.Runner, spec execx.Spec, items []steps.ChecklistItem) *execComp {
	return &execComp{runner: runner, spec: spec, items: items, active: -1}
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
	c.finished, c.failed, c.active, c.lines = false, nil, -1, nil
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
			return nil, true
		}
		c.lines = append(c.lines, m.ev.Line)
		if len(c.lines) > 200 {
			c.lines = c.lines[len(c.lines)-200:]
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
		b.WriteString(theme.ErrorPanel.Width(w-4).Render(
			theme.Bad.Render("Command failed: ")+c.failed.Error()+"\n"+
				theme.Subtle.Render("The full output is below; press [r] to retry.")) + "\n")
	}

	if len(c.lines) > 0 {
		tail := c.lines
		if len(tail) > logTail {
			tail = tail[len(tail)-logTail:]
		}
		var log strings.Builder
		for i, line := range tail {
			if lw := w - 8; lw > 10 && len(line) > lw {
				line = line[:lw] + "…"
			}
			log.WriteString(theme.Subtle.Render(line))
			if i < len(tail)-1 {
				log.WriteString("\n")
			}
		}
		b.WriteString(theme.Panel.Width(w - 4).Render(log.String()))
	}

	return b.String()
}
