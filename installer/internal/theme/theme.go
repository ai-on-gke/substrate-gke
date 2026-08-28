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

// Package theme carries the color tokens and lipgloss styles, ported from
// the onboarding TUI's theme.py (Electric Sky over Obsidian).
package theme

import "github.com/charmbracelet/lipgloss"

// Color tokens from the onboarding prototype.
var (
	Sky    = lipgloss.Color("#38BDF8") // primary accent
	Indigo = lipgloss.Color("#818CF8") // secondary accent
	Mint   = lipgloss.Color("#34D399") // success
	Amber  = lipgloss.Color("#FBBF24") // warning
	Rose   = lipgloss.Color("#F43F5E") // error
	Slate  = lipgloss.Color("#94A3B8") // muted text
	Faint  = lipgloss.Color("#475569") // borders, inactive
)

var (
	Title    = lipgloss.NewStyle().Bold(true).Foreground(Sky)
	Subtle   = lipgloss.NewStyle().Foreground(Slate)
	Fainted  = lipgloss.NewStyle().Foreground(Faint)
	Accent   = lipgloss.NewStyle().Foreground(Indigo)
	Good     = lipgloss.NewStyle().Foreground(Mint)
	Warning  = lipgloss.NewStyle().Foreground(Amber)
	Bad      = lipgloss.NewStyle().Foreground(Rose)
	Key      = lipgloss.NewStyle().Bold(true).Foreground(Sky)
	Selected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E2E8F0")).Background(lipgloss.Color("#1E293B"))

	Panel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Faint).
		Padding(0, 1)

	AccentPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Sky).
			Padding(0, 1)

	ErrorPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Rose).
			Padding(0, 1)

	CommandLine = lipgloss.NewStyle().Foreground(Mint)
)

// SpinnerFrames are the Braille frames the prototype's doctor rows use.
var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Glyphs for checklist and step states.
const (
	GlyphDone    = "✓"
	GlyphFail    = "✗"
	GlyphWarn    = "!"
	GlyphPending = "○"
	GlyphActive  = "●"
)

// logoLines is the AGENT SUBSTRATE wordmark, following the prototype's hero
// screen.
var logoLines = []string{
	`▄▀█ █▀▀ █▀▀ █▄░█ ▀█▀   █▀ █░█ █▄▄ █▀ ▀█▀ █▀█ ▄▀█ ▀█▀ █▀▀`,
	`█▀█ █▄█ ██▄ █░▀█ ░█░   ▄█ █▄█ █▄█ ▄█ ░█░ █▀▄ █▀█ ░█░ ██▄`,
}

// gradient interpolated between Sky and Indigo, one stop per logo line.
var gradient = []lipgloss.Color{
	"#38BDF8", "#818CF8",
}

// Logo renders the wordmark with the prototype's blue→indigo gradient.
func Logo() string {
	out := ""
	for i, line := range logoLines {
		c := gradient[i%len(gradient)]
		out += lipgloss.NewStyle().Foreground(c).Render(line)
		if i < len(logoLines)-1 {
			out += "\n"
		}
	}
	return out
}
