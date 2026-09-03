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
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// resolvedMsg carries the outcome of resolving a typed revision to a commit.
// prefill marks the speculative HEAD lookup that fills the field in, as
// opposed to the resolution the user asked for by pressing enter.
type resolvedMsg struct {
	owner   *imagesScreen
	rev     snapshot.Revision
	err     error
	prefill bool
}

// imagesScreen asks where the Substrate images come from: the published
// release, or a build from a source tree the user names.
//
// It runs before the project step because the answer decides what that step
// needs: a pre-built install pushes nothing, so it is never asked for an image
// registry — the release repository is read-only to installs. It runs after
// the doctor because it is the first step to reach the network, and a missing
// git is the doctor's to report.
type imagesScreen struct {
	deps *Deps
	// mode: "choose", "release", "source".
	mode      string
	cursor    int
	fields    []textinput.Model
	labels    []string
	focus     int
	resolving bool
	errText   string
	note      string
	// prefilled is the repository the revision field's prefilled commit was
	// resolved against, and prefillRev the commit put there, so that editing
	// the repository can tell its own prefill from something the user typed.
	prefilled  string
	prefillRev string
	// cancel stops an in-flight resolve, and the git it is waiting on.
	cancel context.CancelFunc
}

func newImagesScreen(deps *Deps) *imagesScreen {
	return &imagesScreen{deps: deps, mode: "choose"}
}

func (s *imagesScreen) Init() tea.Cmd      { return nil }
func (s *imagesScreen) CapturesText() bool { return s.mode != "choose" }

func (s *imagesScreen) Hints() []Hint {
	if s.mode == "choose" {
		return []Hint{{"1/2", "choose"}, {"enter", "confirm"}, {"b", "back"}}
	}
	return []Hint{{"tab/↓", "next field"}, {"enter", "confirm"}, {"esc", "back"}}
}

func newInput(value, placeholder string) textinput.Model {
	in := textinput.New()
	in.SetValue(value)
	in.Placeholder = placeholder
	in.CharLimit = 200
	in.Prompt = "  "
	in.Width = 60
	return in
}

// enterRelease offers the registry and version the installer maintains, both
// editable: a team publishing its own builds — a staging registry, or a
// private rebuild of a release — installs them by typing them here rather than
// by building from source.
//
// The manifest revision comes with them, because ate-setup reads the manifests
// from a source tree and only the release registry is published with a tree
// known to match. Overriding the registry without moving this too would
// install someone's images behind the release's manifests.
func (s *imagesScreen) enterRelease() tea.Cmd {
	s.mode, s.focus, s.errText, s.note = "release", 0, "", ""
	s.prefilled = ""
	s.labels = []string{"Image registry", "Image tag", "Manifest repository", "Manifest commit id"}
	s.fields = []textinput.Model{
		newInput(snapshot.ReleaseRepo, snapshot.ReleaseRepo),
		newInput(snapshot.ReleaseVersion, snapshot.ReleaseVersion),
		newInput(snapshot.RepoURL, snapshot.RepoURL),
		newInput(snapshot.Commit, snapshot.Commit),
	}
	return tea.Batch(s.fields[0].Focus(), textinput.Blink)
}

// enterSource offers a repository and a revision in it, and starts resolving
// that repository's HEAD so the revision field can show a commit id rather
// than an empty box. Anything the user types wins over the prefill.
func (s *imagesScreen) enterSource() tea.Cmd {
	s.mode, s.focus, s.errText, s.note = "source", 0, "", ""
	s.prefilled = ""
	s.labels = []string{"Repository", "Branch, tag, or commit"}
	s.fields = []textinput.Model{
		newInput(snapshot.RepoURL, snapshot.RepoURL),
		newInput("", "resolving the current HEAD…"),
	}
	return tea.Batch(s.fields[0].Focus(), textinput.Blink, s.resolve(snapshot.RepoURL, "", true))
}

// resolve turns a repository and revision into an exact commit off the main
// loop, since it talks to the remote. A dry run resolves nothing and keeps the
// pin: the point of --dry-run is to walk the wizard without reaching out.
func (s *imagesScreen) resolve(repo, ref string, prefill bool) tea.Cmd {
	if s.deps.DryRun {
		return func() tea.Msg {
			return resolvedMsg{s, snapshot.Revision{
				Repo: repo, Commit: snapshot.Commit, Describe: "(dry-run) " + snapshot.ShortCommit(),
			}, nil, prefill}
		}
	}
	// Only the pre-built track passes --image-repo, so only it needs a tree
	// whose ate-setup knows the flag.
	needImageFlags := s.mode == "release"
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	return func() tea.Msg {
		if prefill {
			// A guess made while the user is still typing, so it skips the
			// fetch negotiation Resolve does; submitting re-resolves it.
			rev, err := snapshot.Head(ctx, repo)
			return resolvedMsg{s, rev, err, true}
		}
		rev, err := snapshot.Resolve(ctx, repo, ref, needImageFlags)
		return resolvedMsg{s, rev, err, false}
	}
}

// stopResolving abandons an in-flight resolve, killing the git it is waiting
// on rather than leaving it to finish into a screen nobody is looking at.
func (s *imagesScreen) stopResolving() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.resolving = false
}

func (s *imagesScreen) setFocus(i int) tea.Cmd {
	s.fields[s.focus].Blur()
	s.focus = (i + len(s.fields)) % len(s.fields)
	return s.fields[s.focus].Focus()
}

func (s *imagesScreen) value(i int) string { return strings.TrimSpace(s.fields[i].Value()) }

// submitRelease records the chosen images and the tree their manifests come
// from. Neither image value is checked against the registry: ate-setup reads a
// digest for every image it installs, and it reports a registry or tag that is
// not there far better than a probe here could. The revision is resolved,
// because a checkout is what the install steps fetch.
func (s *imagesScreen) submitRelease() tea.Cmd {
	registry, tag, repo, rev := s.value(0), s.value(1), s.value(2), s.value(3)
	switch {
	case registry == "":
		s.errText = "An image registry is required."
		return s.setFocus(0)
	case tag == "":
		s.errText = "An image tag is required."
		return s.setFocus(1)
	case repo == "":
		s.errText = "A manifest repository is required."
		return s.setFocus(2)
	case rev == "":
		s.errText = "A manifest commit id is required."
		return s.setFocus(3)
	}
	// The registry and the tag's existence are ate-setup's to report, but the
	// tag doubles as the Substrate version, and that has to be a label value.
	if err := snapshot.CheckImageTag(tag); err != nil {
		s.errText = err.Error()
		return s.setFocus(1)
	}
	st := s.deps.Setup
	st.ImageRepo, st.ImageTag = registry, tag
	if repo == snapshot.RepoURL && rev == snapshot.Commit {
		// The offered tree, unedited. It is the pin every other part of the
		// installer already trusts, so asking the remote about it would only
		// add a round trip — and a way for the default path to fail.
		s.useSource(snapshot.Revision{Repo: snapshot.RepoURL, Commit: snapshot.Commit})
		return goNext
	}
	s.errText = ""
	s.resolving = true
	return s.resolve(repo, rev, false)
}

// submitSource resolves what the user typed and, once it names a commit,
// points the builder at it.
func (s *imagesScreen) submitSource() tea.Cmd {
	s.errText = ""
	s.resolving = true
	return s.resolve(s.value(0), s.value(1), false)
}

// useSource points the run at a tree.
func (s *imagesScreen) useSource(rev snapshot.Revision) {
	s.stopResolving()
	s.deps.Builder.UseSource(rev)
}

func (s *imagesScreen) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case resolvedMsg:
		if m.owner != s || s.fields == nil {
			return nil
		}
		if m.prefill {
			if s.mode != "source" {
				return nil
			}
			if m.err != nil {
				s.fields[1].Placeholder = "branch, tag, or full commit SHA"
				s.note = "Could not read the default branch: " + m.err.Error()
				return nil
			}
			// The lookup raced the user, or the repository moved under it;
			// either way what is on screen now is the answer.
			if s.value(1) != "" || s.value(0) != m.rev.Repo {
				return nil
			}
			s.fields[1].SetValue(m.rev.Commit)
			s.prefilled, s.prefillRev = m.rev.Repo, m.rev.Commit
			return nil
		}
		if !s.resolving {
			// Cancelled while it was in flight; the answer is stale.
			return nil
		}
		s.stopResolving()
		if m.err != nil {
			s.errText = m.err.Error()
			return nil
		}
		if s.mode == "source" {
			s.deps.Setup.ImageRepo, s.deps.Setup.ImageTag = "", ""
		}
		s.useSource(m.rev)
		return goNext

	case tea.KeyMsg:
		if s.resolving {
			// esc is advertised while this is on screen, and a remote behind a
			// blackholing proxy takes the full timeout to answer, so it has to
			// be a way out rather than one more swallowed key.
			if m.String() == "esc" {
				s.stopResolving()
				s.note = "Cancelled."
			}
			return nil
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
					return s.enterRelease()
				}
				return s.enterSource()
			case "b", "esc":
				return goBack
			}
			return nil
		}
		switch key {
		case "esc":
			s.stopResolving()
			s.mode = "choose"
			s.fields, s.errText, s.note = nil, "", ""
			s.prefilled, s.prefillRev = "", ""
			return nil
		case "tab", "down":
			return s.setFocus(s.focus + 1)
		case "shift+tab", "up":
			return s.setFocus(s.focus - 1)
		case "enter":
			if s.focus < len(s.fields)-1 {
				return s.setFocus(s.focus + 1)
			}
			if s.mode == "release" {
				return s.submitRelease()
			}
			return s.submitSource()
		}
	}

	if s.fields == nil {
		return nil
	}
	var cmd tea.Cmd
	s.fields[s.focus], cmd = s.fields[s.focus].Update(msg)
	// A prefilled commit belongs to the repository it was read from. Editing
	// the repository has to clear it: a fork normally contains the upstream
	// commit, so a stale one would resolve cleanly and quietly build the tree
	// the user just navigated away from.
	//
	// Only the prefill itself, though. Once the field holds something the user
	// typed, going back to fix a typo in the URL must not wipe it out from
	// under them — their revision is theirs, and it is what they are watching.
	if s.prefilled != "" && s.focus == 0 && s.value(0) != s.prefilled {
		if s.value(1) == s.prefillRev {
			s.fields[1].SetValue("")
			s.fields[1].Placeholder = "branch, tag, or full commit SHA"
		}
		s.prefilled, s.prefillRev = "", ""
	}
	return cmd
}

func (s *imagesScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose your images") + "\n")
	b.WriteString(theme.Subtle.Render("Where the Substrate control-plane images come from.") + "\n\n")

	if s.mode == "choose" {
		options := []struct{ name, desc string }{
			{"[1] Install pre-built images (recommended)",
				"Defaults to the published release (coming soon) at\n" + snapshot.ReleaseRepo + ";\nany registry and tag can be given instead. Nothing is built or pushed."},
			{"[2] Build from source",
				"Builds every image with ko from a Substrate tree and pushes them\nto your own registry. Pick this for a fork, a branch, or a hotfix."},
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
	switch {
	case s.resolving:
		b.WriteString(theme.Accent.Render("Resolving the revision with git ls-remote…"))
	case s.errText != "":
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 74)).Render(theme.Bad.Render(s.errText)))
	case s.note != "":
		b.WriteString(theme.Warning.Render(s.note))
	// The tag counts as much as the registry: the offered manifest commit is
	// the one the offered tag was built from, so a newer tag out of the same
	// registry is just as capable of running behind manifests that predate it.
	case s.mode == "release" && (s.value(0) != snapshot.ReleaseRepo || s.value(1) != snapshot.ReleaseVersion):
		b.WriteString(theme.Warning.Render(
			"Only " + snapshot.ReleaseRepo + ":" + snapshot.ReleaseVersion + "\n" +
				"is published with manifests known to match. Point the two manifest\n" +
				"fields at the repository and commit these images were built from."))
	case s.mode == "release":
		b.WriteString(theme.Accent.Render("The published release is coming soon; until then, give a registry and\ntag you have published to.") + "\n" +
			theme.Subtle.Render("Override any of these to install your own build. Every image is pulled\nat this tag and pinned to the digest it names, and ate-setup reads the\nmanifests from the commit below."))
	default:
		b.WriteString(theme.Subtle.Render("Defaults to the repository's current HEAD, shown as a commit id.\nOverride it with a branch, a tag, or a full commit SHA."))
	}
	// An unmanaged root is a checkout the user handed us with --substrate-root,
	// and it is used exactly as it stands: nothing is fetched, so the commit
	// asked for above is recorded and then not honoured. Say so, rather than
	// echoing back a revision the install will quietly ignore.
	if !s.deps.Builder.Managed {
		b.WriteString("\n\n" + theme.Warning.Render(
			"--substrate-root is set, so the install builds "+s.deps.Builder.Root+"\n"+
				"as it stands. The commit above is not fetched."))
	}
	return b.String()
}
