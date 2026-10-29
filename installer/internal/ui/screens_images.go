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
type resolvedMsg struct {
	owner *imagesScreen
	rev   snapshot.Revision
	err   error
}

// imagesScreen asks where the Substrate images come from: the published
// release, or a build from a revision of the Substrate repository. The
// repository itself is fixed; only the commit read from it moves.
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

// enterRelease offers a registry and tag (see offeredImages): by default
// upstream's release registry and, once the pin's release has images there,
// its tag (see snapshot.ReleaseVersion). All three fields
// are editable: a team publishing its own builds, a staging registry or a
// private rebuild of a release installs them by typing them here rather than
// by building from source.
//
// The commit the images were built from comes with them, because ate-setup
// reads the manifests from the Substrate tree at that commit. It defaults to
// the pin, which only fits images built from the pin.
func (s *imagesScreen) enterRelease() tea.Cmd {
	registry, tag := s.offeredImages()
	s.mode, s.focus, s.errText, s.note = "release", 0, "", ""
	s.labels = []string{"Image registry", "Image tag", "Commit the images were built from"}
	s.fields = []textinput.Model{
		newInput(registry, snapshot.ReleaseRepo),
		newInput(tag, "the release tag the images were published under"),
		newInput(snapshot.Commit, snapshot.Commit),
	}
	return tea.Batch(s.fields[0].Focus(), textinput.Blink)
}

// offeredImages is the registry and tag the pre-built track starts at. A
// registry the run already holds wins, so coming back to the step keeps what
// was typed. On the upgrade track the next registry is the one the cluster's
// record installs from, since PrepareUpgrade writes the offered registry into
// new.yaml and a cluster installed from a mirror upgrades from that mirror.
// Otherwise it is upstream's release registry. The release tag is offered
// only with the release registry; any other registry's tag is the user's to
// give.
func (s *imagesScreen) offeredImages() (registry, tag string) {
	st := s.deps.Setup
	if st.ImageRepo != "" {
		return st.ImageRepo, st.ImageTag
	}
	if st.Upgrade {
		if record, err := snapshot.RecordPath(st); err == nil {
			if repo, _ := snapshot.RecordImageRepo(record); repo != "" {
				registry = repo
			}
		}
	}
	if registry == "" {
		registry = snapshot.ReleaseRepo
	}
	if registry == snapshot.ReleaseRepo {
		tag = snapshot.ReleaseVersion
	}
	return registry, tag
}

// enterSource offers a revision of the Substrate repository, prefilled with
// the pin: the revision this installer was checked against, rather than a
// moving branch that may already need settings it does not pass. Anything the
// user types replaces it.
func (s *imagesScreen) enterSource() tea.Cmd {
	s.mode, s.focus, s.errText, s.note = "source", 0, "", ""
	s.labels = []string{"Branch, tag, or commit"}
	s.fields = []textinput.Model{newInput(snapshot.Commit, "branch, tag, or full commit SHA")}
	return tea.Batch(s.fields[0].Focus(), textinput.Blink)
}

// resolve turns a revision of the Substrate repository into an exact commit
// off the main loop, since it talks to the remote. A dry run resolves nothing
// and keeps the pin: the point of --dry-run is to walk the wizard without
// reaching out.
func (s *imagesScreen) resolve(ref string) tea.Cmd {
	const repo = snapshot.RepoURL
	if s.deps.DryRun {
		return func() tea.Msg {
			return resolvedMsg{s, snapshot.Revision{
				Repo: repo, Commit: snapshot.Commit, Describe: "(dry-run) " + snapshot.ShortCommit(),
			}, nil}
		}
	}
	// Only the pre-built track passes --image-repo, so only it needs a tree
	// whose ate-setup knows the flag.
	needImageFlags := s.mode == "release"
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	return func() tea.Msg {
		rev, err := snapshot.Resolve(ctx, repo, ref, needImageFlags)
		return resolvedMsg{s, rev, err}
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

// submitRelease records the chosen images and the commit their manifests come
// from. Neither image value is checked against the registry: ate-setup reads a
// digest for every image it installs, and it reports a registry or tag that is
// not there far better than a probe here could. The revision is resolved,
// because a checkout is what the install steps fetch.
func (s *imagesScreen) submitRelease() tea.Cmd {
	registry, tag, rev := s.value(0), s.value(1), s.value(2)
	switch {
	case registry == "":
		s.errText = "An image registry is required."
		return s.setFocus(0)
	case tag == "":
		s.errText = "An image tag is required."
		return s.setFocus(1)
	case rev == "":
		s.errText = "The commit the images were built from is required."
		return s.setFocus(2)
	}
	// The registry and the tag's existence are ate-setup's to report; the
	// tag's form is checked here, before anything runs.
	if err := snapshot.CheckImageTag(tag); err != nil {
		s.errText = err.Error()
		return s.setFocus(1)
	}
	st := s.deps.Setup
	st.ImageRepo, st.ImageTag = registry, tag
	if rev == snapshot.Commit {
		// The offered commit, unedited. It is the pin every other part of
		// the installer already trusts, so asking the remote about it would
		// only add a round trip, and a way for the default path to fail.
		s.useSource(snapshot.Revision{Repo: snapshot.RepoURL, Commit: snapshot.Commit})
		return goNext
	}
	s.errText = ""
	s.resolving = true
	return s.resolve(rev)
}

// submitSource resolves what the user typed and, once it names a commit,
// points the builder at it.
func (s *imagesScreen) submitSource() tea.Cmd {
	s.errText = ""
	s.resolving = true
	return s.resolve(s.value(0))
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
	return cmd
}

func (s *imagesScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose your images") + "\n")
	b.WriteString(theme.Subtle.Render("Where the Substrate control-plane images come from.") + "\n\n")

	if s.mode == "choose" {
		prebuilt := struct{ name, desc string }{"[1] Install pre-built images",
			"Pulls every image from " + snapshot.ReleaseRepo + "\nat a release tag you give, or from any registry and tag. Needs images\npublished for the commit you install. Nothing is built or pushed."}
		if snapshot.ReleaseVersion != "" {
			prebuilt = struct{ name, desc string }{"[1] Install pre-built images (recommended)",
				"Defaults to the " + snapshot.ReleaseVersion + " release at\n" + snapshot.ReleaseRepo + ";\nany registry and tag can be given instead. Nothing is built or pushed."}
		}
		options := []struct{ name, desc string }{
			prebuilt,
			{"[2] Build from source",
				"Builds every image with ko from a commit of the Substrate repository,\n" + snapshot.ShortCommit() + " by default, and pushes them to your own registry. Pick\nthis when you have no published images."},
		}
		if s.deps.DockerWarned {
			options[1].desc += "\n" + theme.Warning.Render(
				"The setup check found Docker problems. Building Substrate 0.2 or\nlater from source needs Docker. Fix them before building images.")
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

	// The repository is not a field: both tracks read manifests, and one of
	// them code, from upstream, and only the commit moves.
	b.WriteString(theme.Subtle.Render("  Source tree") + "\n")
	b.WriteString(theme.Fainted.Render("  "+snapshot.RepoURL) + "\n")
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
	// The tag counts as much as the registry: the offered commit is the one
	// the offered tag was built from, so another tag from the same registry
	// runs behind manifests that do not match it.
	case s.mode == "release" && snapshot.ReleaseVersion != "" &&
		(s.value(0) != snapshot.ReleaseRepo || s.value(1) != snapshot.ReleaseVersion):
		b.WriteString(theme.Warning.Render(
			"Only " + snapshot.ReleaseRepo + ":" + snapshot.ReleaseVersion + "\n" +
				"is published from the commit offered below. Give the commit these\n" +
				"images were built from; the manifests are read from it."))
	case s.mode == "release" && snapshot.ReleaseVersion != "":
		b.WriteString(theme.Subtle.Render("Defaults to the published release; override any of these to install your\nown build. Every image is pulled at this tag and pinned to the digest it\nnames, and ate-setup reads the manifests from the commit above."))
	case s.mode == "release":
		b.WriteString(theme.Subtle.Render("The pinned commit's release has no published images yet. Give the release\ntag of images published to this registry, or another registry and tag, and\nthe commit those images were built from: ate-setup reads the manifests from\nit. Every image is pulled at this tag and pinned to the digest it names."))
	default:
		b.WriteString(theme.Subtle.Render("Defaults to " + snapshot.ShortCommit() + ", the revision this installer is pinned to.\nOverride it with a branch, a tag, or a full commit SHA."))
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
