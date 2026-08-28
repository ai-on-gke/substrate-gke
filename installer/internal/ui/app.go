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

// Package ui is the bubbletea port of the onboarding TUI from upstream PR
// #1171: the same 7-step wizard, sidebar, doctor pattern, keymap bar, help
// and exit modals — wired to real installer commands instead of animations.
package ui

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// Deps carries everything screens need to do real work.
type Deps struct {
	Setup   *state.Setup
	Runner  execx.Runner
	GCP     *gcp.Client
	Builder *snapshot.Builder
	Checks  []doctor.Check
	DryRun  bool
}

// Screen is one wizard page. Update returns commands; navigation happens by
// returning a navMsg-producing command.
type Screen interface {
	Init() tea.Cmd
	Update(msg tea.Msg) tea.Cmd
	View(width int) string
	Hints() []Hint
	CapturesText() bool
}

// Hint is one bottom-bar key binding.
type Hint struct{ Key, Label string }

// Navigation messages emitted by screens.
type navMsg int

const (
	navNext navMsg = iota
	navBack
	navQuit
)

func goNext() tea.Msg { return navNext }
func goBack() tea.Msg { return navBack }
func doQuit() tea.Msg { return navQuit }

// frameMsg drives spinners; owner lets stale ticks from replaced screens be
// dropped.
type frameMsg struct{ owner any }

type overlay int

const (
	overlayNone overlay = iota
	overlayHelp
	overlayExit
	overlaySlash
)

// App is the root model.
type App struct {
	deps *Deps
	mach *state.Machine
	cur  Screen

	width, height int
	over          overlay
	slash         textinput.Model
	quitting      bool

	// Completed is set when the user reached the final screen.
	Completed bool
}

// NewApp builds the root model at the Welcome step.
func NewApp(deps *Deps) *App {
	slash := textinput.New()
	slash.Prompt = "/"
	slash.Placeholder = "help · back · skip · exit"
	slash.CharLimit = 32
	a := &App{deps: deps, mach: state.NewMachine(), slash: slash}
	a.cur = a.screenFor(a.mach.Current())
	return a
}

func (a *App) screenFor(s state.Step) Screen {
	switch s {
	case state.Welcome:
		return newWelcomeScreen(a.deps)
	case state.CheckSetup:
		return newDoctorScreen(a.deps)
	case state.Project:
		return newProjectScreen(a.deps)
	case state.Cluster:
		return newClusterScreen(a.deps)
	case state.Provision:
		return newProvisionScreen(a.deps)
	case state.ControlPlane:
		return newControlPlaneScreen(a.deps)
	case state.FilestoreCSI:
		return newFilestoreScreen(a.deps)
	case state.Autoscaling:
		return newAutoscalingScreen(a.deps)
	case state.Demo:
		return newDemoScreen(a.deps)
	case state.Complete:
		return newCompleteScreen(a.deps)
	}
	return newWelcomeScreen(a.deps)
}

// Init implements tea.Model.
func (a *App) Init() tea.Cmd { return a.cur.Init() }

// Update implements tea.Model.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
		return a, nil

	case navMsg:
		switch m {
		case navNext:
			step := a.mach.Next()
			if step == state.Complete {
				a.Completed = true
			}
			a.cur = a.screenFor(step)
			return a, a.cur.Init()
		case navBack:
			if step, ok := a.mach.Prev(); ok {
				a.cur = a.screenFor(step)
				return a, a.cur.Init()
			}
			return a, nil
		case navQuit:
			a.quitting = true
			return a, tea.Quit
		}

	case tea.KeyMsg:
		return a.handleKey(m)
	}

	return a, a.cur.Update(msg)
}

func (a *App) handleKey(m tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := m.String()

	switch a.over {
	case overlayExit:
		if key == "y" || key == "Y" {
			a.quitting = true
			return a, tea.Quit
		}
		a.over = overlayNone
		return a, nil
	case overlayHelp:
		a.over = overlayNone
		return a, nil
	case overlaySlash:
		switch key {
		case "esc":
			a.over = overlayNone
			return a, nil
		case "enter":
			cmdName := a.slash.Value()
			a.over = overlayNone
			a.slash.SetValue("")
			return a, a.runSlash(cmdName)
		}
		var cmd tea.Cmd
		a.slash, cmd = a.slash.Update(m)
		return a, cmd
	}

	switch key {
	case "ctrl+c", "ctrl+d":
		a.over = overlayExit
		return a, nil
	}
	if !a.cur.CapturesText() {
		switch key {
		case "?":
			a.over = overlayHelp
			return a, nil
		case "/":
			a.over = overlaySlash
			a.slash.Focus()
			return a, textinput.Blink
		}
	}
	return a, a.cur.Update(m)
}

// runSlash executes a slash command, mirroring the prototype's command bar.
func (a *App) runSlash(name string) tea.Cmd {
	switch name {
	case "help", "h":
		a.over = overlayHelp
	case "back", "b":
		return goBack
	case "skip", "s":
		// Only the optional steps may be skipped.
		if s := a.mach.Current(); s == state.FilestoreCSI || s == state.Autoscaling || s == state.Demo {
			return goNext
		}
	case "exit", "quit", "q":
		a.over = overlayExit
	}
	return nil
}

// View implements tea.Model.
func (a *App) View() string {
	if a.quitting {
		return ""
	}
	if a.width == 0 {
		return "loading…"
	}

	sidebarW := 30
	contentW := a.width - sidebarW - 3
	if contentW < 40 {
		sidebarW = 0
		contentW = a.width - 2
	}

	header := a.headerView()
	bottom := a.bottomView()
	bodyH := a.height - lipgloss.Height(header) - lipgloss.Height(bottom) - 1

	var content string
	switch a.over {
	case overlayHelp:
		content = a.helpView(contentW)
	case overlayExit:
		content = a.exitView(contentW)
	case overlaySlash:
		content = lipgloss.JoinVertical(lipgloss.Left,
			a.cur.View(contentW),
			theme.AccentPanel.Width(contentW-2).Render(a.slash.View()),
		)
	default:
		content = a.cur.View(contentW)
	}
	content = clampHeight(content, bodyH)

	var body string
	if sidebarW > 0 {
		side := a.sidebarView(sidebarW, bodyH)
		body = lipgloss.JoinHorizontal(lipgloss.Top, side, " ", content)
	} else {
		body = content
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, body, bottom)
}
