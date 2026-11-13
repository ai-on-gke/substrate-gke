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
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
	"github.com/ai-on-gke/substrate-gke/installer/internal/steps"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// TokenExchangeSummary describes what the optional token-exchange step did.
func TokenExchangeSummary(st *state.Setup) string {
	switch {
	case st.TokenExchangeDeployed:
		return fmt.Sprintf("installed (ate-secret://%s/token-exchange/%s)", state.TokenExchangeProvider, state.TokenExchangeProfile)
	case st.TokenExchangeFailed:
		return "failed  · skipped after the error; see the log"
	default:
		return "skipped"
	}
}

type tokenExchangeScreen struct {
	deps    *Deps
	mode    string
	cursor  int
	fields  []textinput.Model
	labels  []string
	focus   int
	errText string
	comp    *execComp
}

func newTokenExchangeScreen(deps *Deps) *tokenExchangeScreen {
	return &tokenExchangeScreen{deps: deps, mode: "choose"}
}

func (s *tokenExchangeScreen) Init() tea.Cmd      { return nil }
func (s *tokenExchangeScreen) CapturesText() bool { return s.mode == "config" }
func (s *tokenExchangeScreen) logComp() *execComp { return s.comp }
func (s *tokenExchangeScreen) value(i int) string { return strings.TrimSpace(s.fields[i].Value()) }
func (s *tokenExchangeScreen) setFocus(i int) tea.Cmd {
	s.fields[s.focus].Blur()
	s.focus = (i + len(s.fields)) % len(s.fields)
	return s.fields[s.focus].Focus()
}

func (s *tokenExchangeScreen) enterConfig() tea.Cmd {
	st := s.deps.Setup
	projNum := st.ProjectNumber
	if projNum == "" {
		projNum = "<PROJECT_NUMBER>"
	}
	aud := newInput(st.TokenExchangeAudience, "//iam.googleapis.com/projects/"+projNum+"/locations/global/workloadIdentityPools/<POOL>/providers/<PROVIDER>")
	aud.CharLimit = 512
	aud.Width = 74

	atespaces := newInput(strings.Join(st.TokenExchangeAtespaces, ","), "default,agents  (optional; leave blank to configure later)")
	atespaces.CharLimit = 512
	atespaces.Width = 74

	s.fields = []textinput.Model{aud, atespaces}
	s.labels = []string{
		"Workload Identity Pool provider audience",
		"Allowed atespaces (comma-separated, optional)",
	}
	if st.Prebuilt() {
		koDefault := st.TokenExchangeKoDockerRepo
		if koDefault == "" {
			koDefault = strings.TrimSpace(os.Getenv("KO_DOCKER_REPO"))
		}
		koRepo := newInput(koDefault, st.BuildRepository()+"  (default Artifact Registry)")
		koRepo.CharLimit = 256
		koRepo.Width = 74
		s.fields = append(s.fields, koRepo)
		s.labels = append(s.labels, "Image registry for ko (optional; blank uses "+st.BuildRepository()+")")
	}

	s.mode, s.focus, s.errText = "config", 0, ""
	return tea.Batch(s.fields[0].Focus(), textinput.Blink)
}

func (s *tokenExchangeScreen) submit() tea.Cmd {
	aud := s.value(0)
	if err := state.ValidateTokenExchangeAudience(aud); err != nil {
		s.errText = err.Error()
		return s.setFocus(0)
	}
	atespaces, err := state.ParseAtespaces(s.value(1))
	if err != nil {
		s.errText = err.Error()
		return s.setFocus(1)
	}
	if _, err := snapshot.TokenExchangeCredentialProviderJSON(snapshot.CredentialProvider()); err != nil {
		s.errText = err.Error()
		return nil
	}

	st := s.deps.Setup
	st.TokenExchangeAudience = aud
	st.TokenExchangeAtespaces = atespaces
	if st.Prebuilt() && len(s.fields) > 2 {
		st.TokenExchangeKoDockerRepo = s.value(2)
	}
	st.TokenExchangeDeployed, st.TokenExchangeFailed = false, false
	st.TokenExchangeCreatedRepo = st.TokenExchangeCreatesRepository()

	s.errText, s.mode = "", "exec"
	s.comp = newExecComp(
		s.deps.Runner,
		s.deps.Builder.DeployTokenExchange(st),
		steps.TokenExchange(s.deps.Builder.FetchLabel(), st.Prebuilt()),
		s.deps.LogPath,
	)
	return s.comp.start()
}

func (s *tokenExchangeScreen) Hints() []Hint {
	switch s.mode {
	case "choose":
		return []Hint{{"1/2", "choose"}, {"enter", "confirm"}, {"s", "skip"}, {"b", "back"}}
	case "config":
		return []Hint{{"tab", "next field"}, {"enter", "continue"}, {"esc", "back"}}
	case "exec":
		if s.comp.ok() {
			return []Hint{{"enter", "continue"}}
		}
		if s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"s", "skip"}}
		}
	}
	return nil
}

func (s *tokenExchangeScreen) Update(msg tea.Msg) tea.Cmd {
	if s.mode == "exec" && s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			return cmd
		}
		if key, ok := msg.(tea.KeyMsg); ok {
			switch key.String() {
			case "enter":
				if s.comp.ok() {
					s.deps.Setup.TokenExchangeDeployed, s.deps.Setup.TokenExchangeFailed = true, false
					return goNext
				}
			case "r":
				if s.comp.failed != nil {
					return s.comp.restart()
				}
			case "s":
				if s.comp.failed != nil {
					s.deps.Setup.TokenExchangeDeployed, s.deps.Setup.TokenExchangeFailed = false, true
					return goNext
				}
			}
		}
		return nil
	}

	if key, ok := msg.(tea.KeyMsg); ok {
		if s.mode == "choose" {
			switch key.String() {
			case "1", "up", "k":
				s.cursor = 0
			case "2", "down", "j":
				s.cursor = 1
			case "s":
				s.deps.Setup.TokenExchangeDeployed = false
				return goNext
			case "b", "esc":
				return goBack
			case "enter":
				if s.cursor == 1 {
					s.deps.Setup.TokenExchangeDeployed = false
					return goNext
				}
				return s.enterConfig()
			}
			return nil
		}

		switch key.String() {
		case "esc":
			s.fields[s.focus].Blur()
			s.mode, s.fields, s.errText = "choose", nil, ""
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
	}

	if s.mode == "config" && s.fields != nil {
		s.errText = ""
		var cmd tea.Cmd
		s.fields[s.focus], cmd = s.fields[s.focus].Update(msg)
		return cmd
	}
	return nil
}

func (s *tokenExchangeScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render(state.TokenExchange.Title()) + "\n")
	wrap := theme.Subtle.Width(max(w-2, 20))
	b.WriteString(wrap.Render("Optional: install credprovider-token-exchange so actors can exchange actor JWTs at Google Cloud STS for federated access tokens via EgressPolicy (ate-secret://sts/token-exchange/gcp).") + "\n\n")

	switch s.mode {
	case "choose":
		options := []string{
			"[1] Install credprovider-token-exchange (Google Cloud STS)",
			"[2] Skip — keep the bundled credential provider only",
		}
		for i, opt := range options {
			if i == s.cursor {
				b.WriteString(theme.Selected.Render(" "+opt+" ") + "\n")
			} else {
				b.WriteString(theme.Subtle.Render("  "+opt) + "\n")
			}
		}

	case "config":
		for i := range s.fields {
			b.WriteString(theme.Title.Render("  "+s.labels[i]) + "\n")
			b.WriteString(s.fields[i].View() + "\n")
		}
		if s.errText != "" {
			b.WriteString("\n" + theme.ErrorPanel.Width(min(w-4, 90)).Render(theme.Bad.Render(s.errText)) + "\n")
		}

	case "exec":
		b.WriteString(s.comp.view(w))
		if s.comp.ok() {
			b.WriteString("\n" + theme.Good.Render("credprovider-token-exchange deployed and registered. Press [enter] to continue."))
		}
	}
	return b.String()
}
