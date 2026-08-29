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

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// ─── Choose your GCP project ───────────────────────────────────────────────

type field struct {
	label string
	input textinput.Model
	// set writes the submitted value into Setup.
	set func(st *state.Setup, v string)
}

type prefillMsg struct {
	owner   *projectScreen
	project string
}

type projValidMsg struct {
	owner  *projectScreen
	number string
	err    error
}

type projectScreen struct {
	deps       *Deps
	fields     []field
	focus      int
	validating bool
	errText    string
}

func newField(label, value, placeholder string, set func(*state.Setup, string)) field {
	in := textinput.New()
	in.SetValue(value)
	in.Placeholder = placeholder
	in.CharLimit = 96
	in.Prompt = "  "
	return field{label: label, input: in, set: set}
}

func newProjectScreen(deps *Deps) *projectScreen {
	st := deps.Setup
	fields := []field{
		newField("GCP project ID", st.ProjectID, "my-project", func(s *state.Setup, v string) { s.ProjectID = v }),
		newField("Cluster location (zone)", st.Zone, "us-west1-c", func(s *state.Setup, v string) { s.Zone = v }),
	}
	if st.Track == state.TrackAdvanced {
		fields = append(fields,
			newField("Node machine type", st.MachineType, "c3-standard-4", func(s *state.Setup, v string) { s.MachineType = v }),
			newField("VPC network", st.Network, "default", func(s *state.Setup, v string) { s.Network = v }),
			newField("VPC subnetwork", st.Subnetwork, "default", func(s *state.Setup, v string) { s.Subnetwork = v }),
			newField("Snapshot bucket (blank = derived)", st.BucketName, "ate-snapshots-<project>-<zone>", func(s *state.Setup, v string) { s.BucketName = v }),
			newField("Image registry (blank = derived)", st.KoDockerRepo, "gcr.io/<project>/ate-images", func(s *state.Setup, v string) { s.KoDockerRepo = v }),
		)
	}
	scr := &projectScreen{deps: deps, fields: fields}
	scr.fields[0].input.Focus()
	return scr
}

func (s *projectScreen) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if s.fields[0].input.Value() == "" {
		cmds = append(cmds, func() tea.Msg {
			return prefillMsg{s, s.deps.GCP.CurrentProject(context.Background())}
		})
	}
	return tea.Batch(cmds...)
}

func (s *projectScreen) CapturesText() bool { return true }

func (s *projectScreen) Hints() []Hint {
	return []Hint{{"tab/↓", "next field"}, {"enter", "validate & continue"}, {"esc", "back"}}
}

func (s *projectScreen) setFocus(i int) tea.Cmd {
	s.fields[s.focus].input.Blur()
	s.focus = (i + len(s.fields)) % len(s.fields)
	return s.fields[s.focus].input.Focus()
}

func (s *projectScreen) submit() tea.Cmd {
	pid := strings.TrimSpace(s.fields[0].input.Value())
	if pid == "" {
		s.errText = "A project ID is required."
		return s.setFocus(0)
	}
	s.errText = ""
	s.validating = true
	return func() tea.Msg {
		num, err := s.deps.GCP.ProjectNumber(context.Background(), pid)
		return projValidMsg{s, num, err}
	}
}

func (s *projectScreen) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case prefillMsg:
		if m.owner == s && s.fields[0].input.Value() == "" {
			s.fields[0].input.SetValue(m.project)
		}
		return nil

	case projValidMsg:
		if m.owner != s {
			return nil
		}
		s.validating = false
		if m.err != nil {
			s.errText = m.err.Error()
			return nil
		}
		st := s.deps.Setup
		for _, f := range s.fields {
			f.set(st, strings.TrimSpace(f.input.Value()))
		}
		st.ProjectNumber = m.number
		if st.KoDockerRepo == "" {
			st.KoDockerRepo = "gcr.io/" + st.ProjectID + "/ate-images"
		}
		return goNext

	case tea.KeyMsg:
		if s.validating {
			return nil
		}
		switch m.String() {
		case "esc":
			return goBack
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
		s.fields[s.focus].input, cmd = s.fields[s.focus].input.Update(msg)
		return cmd
	}

	var cmd tea.Cmd
	s.fields[s.focus].input, cmd = s.fields[s.focus].input.Update(msg)
	return cmd
}

func (s *projectScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose your GCP project") + "\n")
	b.WriteString(theme.Subtle.Render("Where the cluster, snapshot bucket, and images will live.") + "\n\n")

	for i, f := range s.fields {
		label := theme.Subtle
		if i == s.focus {
			label = theme.Title
		}
		b.WriteString(label.Render("  "+f.label) + "\n")
		b.WriteString(f.input.View() + "\n")
	}

	b.WriteString("\n")
	switch {
	case s.validating:
		b.WriteString(theme.Accent.Render("Validating project with gcloud…"))
	case s.errText != "":
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 74)).Render(theme.Bad.Render(s.errText)))
	default:
		b.WriteString(theme.Subtle.Render("The project is validated with `gcloud projects describe` on submit."))
	}
	return b.String()
}

// ─── Connect your cluster ──────────────────────────────────────────────────

type clustersMsg struct {
	owner    *clusterScreen
	clusters []gcp.Cluster
	err      error
}

type clusterScreen struct {
	deps     *Deps
	loading  bool
	err      error
	clusters []gcp.Cluster
	cursor   int
	// mode: "list", "name" (new-cluster name input), "confirm" (incompatible
	// cluster chosen).
	mode      string
	nameInput textinput.Model
}

func newClusterScreen(deps *Deps) *clusterScreen {
	in := textinput.New()
	in.SetValue(deps.Setup.ClusterName)
	in.CharLimit = 40
	in.Prompt = "  "
	return &clusterScreen{deps: deps, loading: true, mode: "list", nameInput: in}
}

func (s *clusterScreen) Init() tea.Cmd {
	s.loading = true
	return func() tea.Msg {
		clusters, err := s.deps.GCP.ListClusters(context.Background(), s.deps.Setup.ProjectID)
		return clustersMsg{s, clusters, err}
	}
}

func (s *clusterScreen) CapturesText() bool { return s.mode == "name" }

func (s *clusterScreen) Hints() []Hint {
	switch s.mode {
	case "name":
		return []Hint{{"enter", "create with this name"}, {"esc", "back to list"}}
	case "confirm":
		return []Hint{{"y", "use it anyway"}, {"esc", "choose another"}}
	}
	return []Hint{{"↑/↓", "select"}, {"enter", "confirm"}, {"r", "reload"}, {"b", "back"}}
}

func (s *clusterScreen) choose(c gcp.Cluster) tea.Cmd {
	st := s.deps.Setup
	st.ClusterName = c.Name
	st.Zone = c.Location
	st.ClusterIsNew = false
	if err := st.ApplyProjectDefaults(); err != nil {
		s.err = err
		return nil
	}
	return goNext
}

func (s *clusterScreen) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case clustersMsg:
		if m.owner != s {
			return nil
		}
		s.loading = false
		s.clusters, s.err = m.clusters, m.err
		s.cursor = len(s.clusters) // default to "create new"
		return nil

	case tea.KeyMsg:
		key := m.String()
		switch s.mode {
		case "name":
			switch key {
			case "esc":
				s.mode = "list"
				return nil
			case "enter":
				name := strings.TrimSpace(s.nameInput.Value())
				if name == "" {
					return nil
				}
				st := s.deps.Setup
				st.ClusterName = name
				st.ClusterIsNew = true
				if err := st.ApplyProjectDefaults(); err != nil {
					s.err = err
					return nil
				}
				return goNext
			}
			var cmd tea.Cmd
			s.nameInput, cmd = s.nameInput.Update(msg)
			return cmd

		case "confirm":
			if key == "y" {
				return s.choose(s.clusters[s.cursor])
			}
			s.mode = "list"
			return nil
		}

		// list mode
		switch key {
		case "up", "k":
			if s.cursor > 0 {
				s.cursor--
			}
		case "down", "j":
			if s.cursor < len(s.clusters) {
				s.cursor++
			}
		case "r":
			return s.Init()
		case "b", "esc", "left":
			return goBack
		case "enter":
			if s.loading {
				return nil
			}
			if s.cursor == len(s.clusters) {
				s.mode = "name"
				return tea.Batch(s.nameInput.Focus(), textinput.Blink)
			}
			sel := s.clusters[s.cursor]
			if !sel.SubstrateReady() {
				s.mode = "confirm"
				return nil
			}
			return s.choose(sel)
		default:
			// number keys jump: 1..9 select row, matching the prototype.
			if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
				if i := int(key[0] - '1'); i <= len(s.clusters) {
					s.cursor = i
				}
			}
		}
	}
	return nil
}

func (s *clusterScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Connect your cluster") + "\n")
	b.WriteString(theme.Subtle.Render(fmt.Sprintf("GKE clusters in %s (via `gcloud container clusters list`).", s.deps.Setup.ProjectID)) + "\n\n")

	switch {
	case s.loading:
		b.WriteString(theme.Accent.Render("  Loading clusters…"))
		return b.String()
	case s.err != nil:
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 74)).Render(theme.Bad.Render(s.err.Error()) + "\n" +
			theme.Subtle.Render("Press [r] to retry, or [b] to change project.")))
		return b.String()
	}

	for i, c := range s.clusters {
		badge := theme.Good.Render(theme.GlyphDone + " substrate-ready")
		if !c.SubstrateReady() {
			badge = theme.Bad.Render(theme.GlyphFail + " beta APIs missing")
		}
		row := fmt.Sprintf("[%d] %-24s %-14s %-18s %2d nodes  %s", i+1, c.Name, c.Location, c.MasterVersion, c.NodeCount, badge)
		if i == s.cursor {
			b.WriteString(theme.Selected.Render(" "+row+" ") + "\n")
		} else {
			b.WriteString(theme.Subtle.Render("  "+row) + "\n")
		}
	}
	createRow := fmt.Sprintf("[%d] ＋ Create a new cluster (recommended)", len(s.clusters)+1)
	if s.cursor == len(s.clusters) {
		b.WriteString(theme.Selected.Render(" "+createRow+" ") + "\n")
	} else {
		b.WriteString(theme.Subtle.Render("  "+createRow) + "\n")
	}

	switch s.mode {
	case "name":
		b.WriteString("\n" + theme.AccentPanel.Width(min(w-4, 60)).Render(
			theme.Title.Render("New cluster name")+"\n"+s.nameInput.View()+"\n"+
				theme.Subtle.Render("Created in "+s.deps.Setup.Zone+" by setup-gcp in the next step.")))
	case "confirm":
		b.WriteString("\n" + theme.ErrorPanel.Width(min(w-4, 74)).Render(
			theme.Warning.Render("This cluster cannot run Substrate as-is.")+"\n\n"+
				"It was created without the PodCertificate beta APIs\n"+
				"("+strings.Join(gcp.RequiredBetaAPIs, ",\n ")+").\n"+
				"GKE only honors these at cluster creation time — enabling them later\n"+
				"is accepted but never served, and the install will hang.\n\n"+
				theme.Key.Render("[y]")+" use it anyway (not recommended)   "+theme.Key.Render("[esc]")+" choose another"))
	default:
		b.WriteString("\n" + theme.Subtle.Render("Substrate needs the PodCertificate beta APIs, which GKE can only\nenable at cluster creation — that's why creating a new cluster is\nthe recommended path."))
	}
	return b.String()
}
