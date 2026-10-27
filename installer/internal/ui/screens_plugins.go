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

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// pluginImagesScreen asks where the GKE plugin images come from, the way the
// images step asks about Substrate's: a source tree, the registry and tag the
// images were published under, and the commit they were built from. The
// plugins are published from their own repositories with their own commits
// and versions, so none of these follows from the Substrate images.
//
// Skipping is the default, because no plugin is published yet and nothing
// deploys one (see snapshot.PluginSourceOrg). Unlike the images step, the
// source tree is a field: no plugin repository is fixed yet.
type pluginImagesScreen struct {
	deps *Deps
	// mode: "choose", "images".
	mode    string
	cursor  int
	fields  []textinput.Model
	labels  []string
	focus   int
	errText string
}

// The images mode's fields, in order.
const (
	pluginSourceField = iota
	pluginRegistryField
	pluginTagField
	pluginCommitField
)

func newPluginImagesScreen(deps *Deps) *pluginImagesScreen {
	return &pluginImagesScreen{deps: deps, mode: "choose", cursor: 1}
}

func (s *pluginImagesScreen) Init() tea.Cmd      { return nil }
func (s *pluginImagesScreen) CapturesText() bool { return s.mode != "choose" }

func (s *pluginImagesScreen) Hints() []Hint {
	if s.mode == "choose" {
		return []Hint{{"1/2", "choose"}, {"enter", "confirm"}, {"b", "back"}}
	}
	return []Hint{{"tab/↓", "next field"}, {"enter", "confirm"}, {"esc", "back"}}
}

// enterImages offers what the run already holds, so coming back to the step
// shows the values given before, and otherwise the plugin organization's
// registry. The source tree starts empty because no repository name is fixed.
func (s *pluginImagesScreen) enterImages() tea.Cmd {
	st := s.deps.Setup
	registry := st.PluginImageRepo
	if registry == "" {
		registry = snapshot.PluginRegistry
	}
	s.mode, s.focus, s.errText = "images", 0, ""
	s.labels = []string{"Source tree", "Image registry", "Image tag", "Commit the images were built from"}
	s.fields = []textinput.Model{
		newInput(st.PluginSourceRepo, snapshot.PluginSourceOrg+"/<repository>"),
		newInput(registry, snapshot.PluginRegistry),
		newInput(st.PluginImageTag, "the tag the plugin images were published under"),
		newInput(st.PluginCommit, "40 hex characters"),
	}
	return tea.Batch(s.fields[0].Focus(), textinput.Blink)
}

func (s *pluginImagesScreen) setFocus(i int) tea.Cmd {
	s.fields[s.focus].Blur()
	s.focus = (i + len(s.fields)) % len(s.fields)
	return s.fields[s.focus].Focus()
}

func (s *pluginImagesScreen) value(i int) string { return strings.TrimSpace(s.fields[i].Value()) }

// skip clears any plugin images given earlier in the run.
func (s *pluginImagesScreen) skip() tea.Cmd {
	st := s.deps.Setup
	st.PluginSourceRepo, st.PluginImageRepo, st.PluginImageTag, st.PluginCommit = "", "", "", ""
	return goNext
}

// submit records the plugin images. Nothing here reaches the network: the
// plugin repositories do not exist yet and nothing fetches or pulls from them,
// so the commit has to be a full SHA rather than a ref to resolve, and only
// the values' form is checked.
func (s *pluginImagesScreen) submit() tea.Cmd {
	repo, registry, tag, commit := s.value(pluginSourceField), s.value(pluginRegistryField), s.value(pluginTagField), s.value(pluginCommitField)
	switch {
	case !strings.HasPrefix(repo, "https://") || len(repo) == len("https://"):
		s.errText = "The source tree has to be an https git URL, as in " + snapshot.PluginSourceOrg + "/<repository>."
		return s.setFocus(pluginSourceField)
	case registry == "":
		s.errText = "An image registry is required."
		return s.setFocus(pluginRegistryField)
	case tag == "":
		s.errText = "An image tag is required."
		return s.setFocus(pluginTagField)
	case !fullSHA.MatchString(commit):
		s.errText = "The commit the images were built from has to be a full 40-character SHA."
		return s.setFocus(pluginCommitField)
	}
	if err := snapshot.CheckImageTag(tag); err != nil {
		s.errText = err.Error()
		return s.setFocus(pluginTagField)
	}
	st := s.deps.Setup
	st.PluginSourceRepo, st.PluginImageRepo, st.PluginImageTag, st.PluginCommit = repo, registry, tag, commit
	return goNext
}

func (s *pluginImagesScreen) Update(msg tea.Msg) tea.Cmd {
	m, ok := msg.(tea.KeyMsg)
	if !ok {
		if s.fields == nil {
			return nil
		}
		var cmd tea.Cmd
		s.fields[s.focus], cmd = s.fields[s.focus].Update(msg)
		return cmd
	}
	key := m.String()
	if s.mode == "choose" {
		switch key {
		case "1", "up", "k":
			s.cursor = 0
		case "2", "down", "j":
			s.cursor = 1
		case "enter":
			if s.cursor == 0 {
				return s.enterImages()
			}
			return s.skip()
		case "b", "esc":
			return goBack
		}
		return nil
	}
	switch key {
	case "esc":
		s.mode = "choose"
		s.fields, s.errText = nil, ""
		return nil
	case "tab", "down":
		return s.setFocus(s.focus + 1)
	case "shift+tab", "up":
		return s.setFocus(s.focus - 1)
	case "enter":
		if s.focus < len(s.fields)-1 {
			return s.setFocus(s.focus + 1)
		}
		return s.submit()
	}
	var cmd tea.Cmd
	s.fields[s.focus], cmd = s.fields[s.focus].Update(msg)
	return cmd
}

func (s *pluginImagesScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose your GKE plugin images") + "\n")
	b.WriteString(theme.Subtle.Render("Where the GKE plugin images, such as the GKE credential provider, come from.") + "\n\n")

	if s.mode == "choose" {
		options := []struct{ name, desc string }{
			{"[1] Install pre-built plugin images",
				"Records a plugin source tree, registry, tag and commit. The plugins are\npublished apart from Substrate, with their own commits and versions."},
			{"[2] Skip — no GKE plugins",
				"No plugin is published yet and the install does not deploy one, so\nthis changes nothing about the install."},
		}
		for i, o := range options {
			panel, title := theme.Panel, theme.Subtle
			if i == s.cursor {
				panel, title = theme.AccentPanel, theme.Title
			}
			b.WriteString(panel.Width(min(w-4, 74)).Render(title.Render(o.name)+"\n"+theme.Subtle.Render(o.desc)) + "\n")
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
	if s.errText != "" {
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 74)).Render(theme.Bad.Render(s.errText)))
		return b.String()
	}
	b.WriteString(theme.Subtle.Render("The plugin repositories live under " + snapshot.PluginSourceOrg + ".\nNothing deploys the plugins yet; these values are recorded for when\nthe install does."))
	return b.String()
}
