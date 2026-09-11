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
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
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
	// missing are bootstrap permissions the credentials provably lack;
	// permErr means the permission probe itself could not run.
	missing []gcp.RequiredPermission
	permErr error
}

type projectScreen struct {
	deps       *Deps
	fields     []field
	focus      int
	validating bool
	errText    string
	// permAcked is set once a permission problem has been shown, so the next
	// enter proceeds anyway: the probe is advisory (a role might be granted
	// minutes from now), but failing here beats failing mid-bootstrap.
	permAcked bool
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
		newField("Snapshot bucket (leave empty for default)", st.BucketName, "ate-snapshots-<project>-<cluster>-<zone>", func(s *state.Setup, v string) { s.BucketName = v }),
	}
	if st.Track == state.TrackAdvanced {
		fields = append(fields,
			newField("Node machine type", st.MachineType, "c3-standard-4", func(s *state.Setup, v string) { s.MachineType = v }),
			newField("VPC network", st.Network, "default", func(s *state.Setup, v string) { s.Network = v }),
			newField("VPC subnetwork", st.Subnetwork, "default", func(s *state.Setup, v string) { s.Subnetwork = v }),
		)
		// Only a build from source pushes images anywhere, so only it needs a
		// registry to push them to.
		if !st.Prebuilt() {
			fields = append(fields,
				newField("Image registry (leave empty for default)", st.KoDockerRepo, "gcr.io/<project>/ate-images", func(s *state.Setup, v string) { s.KoDockerRepo = v }),
			)
		}
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
	acked := s.permAcked
	return func() tea.Msg {
		msg := projValidMsg{owner: s}
		msg.number, msg.err = s.deps.GCP.ProjectNumber(context.Background(), pid)
		// Check the bootstrap permissions now rather than failing three
		// screens later, mid-provision. Skipped once acknowledged.
		if msg.err == nil && !acked {
			msg.missing, msg.permErr = s.deps.GCP.MissingPermissions(context.Background(), pid)
		}
		return msg
	}
}

// permProblem renders a missing-permission report (or a probe failure) with
// the fix, ending with the escape hatch: the probe is authoritative about
// today's policy but not about what an admin grants five minutes from now.
func permProblem(projectID string, missing []gcp.RequiredPermission, permErr error) string {
	var b strings.Builder
	if permErr != nil {
		fmt.Fprintf(&b, "Could not verify your IAM permissions on %s:\n%v\n", projectID, permErr)
	} else {
		fmt.Fprintf(&b, "Your application-default credentials lack permissions setup-gcp needs on %s:\n", projectID)
		for _, p := range missing {
			fmt.Fprintf(&b, "  %s — grant %s\n", p.Permission, p.Role)
		}
		fmt.Fprintf(&b, "Grant them with: gcloud projects add-iam-policy-binding %s --member=user:YOU --role=ROLE\n", projectID)
	}
	b.WriteString("Press [enter] again to continue anyway; the provision step may fail.")
	return b.String()
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
		if len(m.missing) > 0 || m.permErr != nil {
			s.permAcked = true
			s.errText = permProblem(strings.TrimSpace(s.fields[0].input.Value()), m.missing, m.permErr)
			return nil
		}
		st := s.deps.Setup
		for _, f := range s.fields {
			f.set(st, strings.TrimSpace(f.input.Value()))
		}
		st.ProjectNumber = m.number
		if st.KoDockerRepo == "" && !st.Prebuilt() {
			st.KoDockerRepo = st.DefaultKoDockerRepo()
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
	// mode: "list", "name" (new-cluster name input), "probing" (checking
	// cluster for existing install), "installed" (cluster already runs
	// Substrate), "partial" (ate-system namespace without atelet),
	// "teardown-confirm"/"teardown" (deleting the control plane so the
	// install can continue here), "confirm" (incompatible cluster chosen).
	mode              string
	nameInput         textinput.Model
	comp              *execComp
	parsed            bool
	installedVersions []string
	// probed caches results per cluster, so browsing back and forth does not
	// pay the multi-second gcloud+kubectl round trip again. [r] re-probes.
	probed map[string]snapshot.InstalledProbe
	// bgPending marks clusters whose background probe is still in flight,
	// rendered as a "checking" note on the row until the result (or a
	// silent failure) lands.
	bgPending map[string]bool
}

func newClusterScreen(deps *Deps) *clusterScreen {
	in := textinput.New()
	in.SetValue(deps.Setup.ClusterName)
	in.CharLimit = 40
	in.Prompt = "  "
	return &clusterScreen{deps: deps, loading: true, mode: "list", nameInput: in,
		probed: map[string]snapshot.InstalledProbe{}, bgPending: map[string]bool{}}
}

// bgProbeMsg carries one background probe's verdict back to its screen.
type bgProbeMsg struct {
	owner *clusterScreen
	key   string
	res   snapshot.InstalledProbe
	err   error
}

// bgProbes probes the substrate-ready clusters in the background, one
// command per cluster so they run concurrently: the list renders
// immediately and each row picks up its install badge as its result lands.
// Only ready clusters — the others cannot take an install, so their state
// decides nothing — and a probe that fails (say, kubectl cannot reach the
// cluster) just leaves its row unbadged; selecting it still runs the
// foreground probe with its retry and continue-anyway paths.
func (s *clusterScreen) bgProbes() tea.Cmd {
	var cmds []tea.Cmd
	for _, c := range s.clusters {
		key := c.Name + "/" + c.Location
		if !c.SubstrateReady() || s.bgPending[key] {
			continue
		}
		if _, ok := s.probed[key]; ok {
			continue
		}
		s.bgPending[key] = true
		spec := snapshot.CheckInstalled(s.deps.Setup.ProjectID, c.Name, c.Location)
		cmds = append(cmds, func() tea.Msg {
			var lines []string
			for ev := range s.deps.Runner.Start(context.Background(), spec) {
				if ev.Line != "" {
					lines = append(lines, ev.Line)
				}
				if ev.Done && ev.Err != nil {
					return bgProbeMsg{owner: s, key: key, err: ev.Err}
				}
			}
			res, err := snapshot.ParseInstalled(lines)
			return bgProbeMsg{owner: s, key: key, res: res, err: err}
		})
	}
	return tea.Batch(cmds...)
}

func (s *clusterScreen) Init() tea.Cmd {
	s.loading = true
	return func() tea.Msg {
		clusters, err := s.deps.GCP.ListClusters(context.Background(), s.deps.Setup.ProjectID)
		return clustersMsg{s, clusters, err}
	}
}

func (s *clusterScreen) CapturesText() bool { return s.mode == "name" }
func (s *clusterScreen) logComp() *execComp { return s.comp }

func (s *clusterScreen) Hints() []Hint {
	switch s.mode {
	case "name":
		return []Hint{{"enter", "create with this name"}, {"esc", "back to list"}}
	case "probing":
		if s.comp != nil && s.comp.failed != nil {
			// No explicit "v" hint: bottomView appends it whenever the comp
			// has output, and a second copy overflows narrow bottom bars.
			return []Hint{{"r", "retry"}, {"y", "continue without the check"}, {"esc", "back to list"}}
		}
		return []Hint{{"esc", "cancel"}}
	case "installed":
		return []Hint{{"t", "tear down and reinstall"}, {"r", "re-probe"}, {"esc", "choose another"}}
	case "partial":
		return []Hint{{"y", "continue"}, {"r", "re-probe"}, {"esc", "choose another"}}
	case "teardown-confirm":
		return []Hint{{"y", "tear it down"}, {"esc", "back"}}
	case "teardown":
		if s.comp != nil && s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"esc", "back to list"}}
		}
		return []Hint{{"esc", "cancel"}}
	case "confirm":
		return []Hint{{"y", "use it anyway"}, {"esc", "choose another"}}
	}
	return []Hint{{"↑/↓", "select"}, {"enter", "confirm"}, {"r", "reload"}, {"b", "back"}}
}

// choose is the one place a selection reaches Setup. Everything before it —
// probing included — works off the gcp.Cluster alone, so an aborted
// selection leaves no zone, name, or derived bucket behind to leak into the
// create-new path or a later pick.
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

// probe checks the selection for an existing install, from cache when the
// cluster was already probed this visit.
func (s *clusterScreen) probe(c gcp.Cluster) tea.Cmd {
	if res, ok := s.probed[c.Name+"/"+c.Location]; ok {
		return s.decide(c, res)
	}
	s.mode, s.parsed = "probing", false
	s.comp = newExecComp(s.deps.Runner, snapshot.CheckInstalled(s.deps.Setup.ProjectID, c.Name, c.Location), nil, s.deps.LogPath)
	return s.comp.start()
}

// decide routes a probe result: a running install is blocked, a bare
// ate-system namespace (an interrupted install — upstream's deploy is
// documented safe to re-run) asks, and a clean cluster continues through the
// pre-existing beta-API check.
func (s *clusterScreen) decide(c gcp.Cluster, res snapshot.InstalledProbe) tea.Cmd {
	switch {
	case res.Partial():
		s.mode = "partial"
		return nil
	case res.Installed:
		s.mode, s.installedVersions = "installed", res.Versions
		return nil
	case !c.SubstrateReady():
		s.mode = "confirm"
		return nil
	}
	return s.choose(c)
}

func (s *clusterScreen) probeDone() tea.Cmd {
	res, err := snapshot.ParseInstalled(s.comp.lines)
	if err != nil {
		s.comp.failed = err
		return nil
	}
	c := s.clusters[s.cursor]
	s.probed[c.Name+"/"+c.Location] = res
	return s.decide(c, res)
}

// reprobe drops the cached result and runs the check again.
func (s *clusterScreen) reprobe() tea.Cmd {
	c := s.clusters[s.cursor]
	delete(s.probed, c.Name+"/"+c.Location)
	return s.probe(c)
}

func (s *clusterScreen) Stop() {
	if s.comp != nil {
		s.comp.stop()
	}
}

func (s *clusterScreen) Update(msg tea.Msg) tea.Cmd {
	if s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			if s.comp.ok() && !s.parsed {
				switch s.mode {
				case "probing":
					s.parsed = true
					return s.probeDone()
				case "teardown":
					// The cluster just changed; the cached verdict did not.
					s.parsed = true
					if s.deps.DryRun {
						// The sim would replay "installed" forever; the
						// simulated teardown's story is a clean cluster.
						c := s.clusters[s.cursor]
						s.probed[c.Name+"/"+c.Location] = snapshot.InstalledProbe{}
						return s.decide(c, snapshot.InstalledProbe{})
					}
					return s.reprobe()
				}
			}
			return cmd
		}
	}

	switch m := msg.(type) {
	case clustersMsg:
		if m.owner != s {
			return nil
		}
		s.loading = false
		s.clusters, s.err = m.clusters, m.err
		s.cursor = len(s.clusters) // default to "create new"
		return s.bgProbes()

	case bgProbeMsg:
		if m.owner != s {
			return nil
		}
		delete(s.bgPending, m.key)
		// A foreground probe may have landed first; it is at least as fresh.
		if _, ok := s.probed[m.key]; !ok && m.err == nil {
			s.probed[m.key] = m.res
		}
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
				// A listed cluster typed by name is still that cluster: it
				// goes through the same probe as a selection, or the guard
				// would be one typed name away from the mixed-version
				// install it exists to prevent.
				for i, c := range s.clusters {
					if c.Name == name {
						s.cursor, s.mode = i, "list"
						return s.probe(c)
					}
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

		case "probing":
			switch key {
			case "r":
				if s.comp != nil && s.comp.failed != nil {
					s.parsed = false
					return s.comp.restart()
				}
			case "y":
				// The guard is advisory when the cluster cannot be probed —
				// a private control plane or missing kubectl access must not
				// make a listed cluster permanently unselectable.
				if s.comp != nil && s.comp.failed != nil {
					return s.decide(s.clusters[s.cursor], snapshot.InstalledProbe{})
				}
			case "b", "esc":
				if s.comp != nil {
					s.comp.stop()
				}
				s.mode = "list"
				return nil
			}
			return nil

		case "installed", "partial":
			switch key {
			case "r":
				return s.reprobe()
			case "y":
				if s.mode == "partial" {
					return s.decide(s.clusters[s.cursor], snapshot.InstalledProbe{})
				}
			case "t":
				if s.mode == "installed" {
					s.mode = "teardown-confirm"
					return nil
				}
			case "b", "esc":
				s.mode = "list"
				return nil
			}
			return nil

		case "teardown-confirm":
			switch key {
			case "y":
				c := s.clusters[s.cursor]
				s.mode, s.parsed = "teardown", false
				s.comp = newExecComp(s.deps.Runner,
					s.deps.Builder.DeleteAteSystem(s.deps.Setup.ProjectID, c.Name, c.Location), nil, s.deps.LogPath)
				return s.comp.start()
			case "b", "esc":
				s.mode = "installed"
				return nil
			}
			return nil

		case "teardown":
			switch key {
			case "r":
				if s.comp != nil && s.comp.failed != nil {
					s.parsed = false
					return s.comp.restart()
				}
			case "b", "esc":
				if s.comp != nil {
					s.comp.stop()
				}
				// The teardown may have half-run; the cached "installed"
				// verdict is the safe answer until the user re-probes.
				s.mode = "list"
				return nil
			}
			return nil

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
			return s.probe(s.clusters[s.cursor])
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
		// "substrate-ready" is capability (the beta APIs), not install state:
		// what the probe learned about an actual install is its own badge, so
		// a teardown visibly clears it while readiness rightly stays.
		badge := theme.Good.Render(theme.GlyphDone + " substrate-ready")
		if !c.SubstrateReady() {
			badge = theme.Bad.Render(theme.GlyphFail + " beta APIs missing")
		}
		if res, ok := s.probed[c.Name+"/"+c.Location]; ok && res.Installed {
			label := " · substrate installed"
			if res.Partial() {
				label = " · partial install"
			}
			badge += theme.Warning.Render(label)
		} else if s.bgPending[c.Name+"/"+c.Location] {
			badge += theme.Fainted.Render(" · checking…")
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
	case "probing":
		sel := s.clusters[s.cursor]
		b.WriteString("\n" + theme.Subtle.Render(fmt.Sprintf("Checking %s for existing Substrate installation…", sel.Name)) + "\n\n")
		b.WriteString(s.comp.view(w))
		if s.comp.failed != nil {
			b.WriteString("\n" + theme.Subtle.Render("Could not check the cluster. "+theme.Key.Render("[y]")+" continues without the check;\nonly do that for a cluster you know has no Substrate on it."))
		}
	case "installed":
		sel := s.clusters[s.cursor]
		var verStr string
		if len(s.installedVersions) > 0 {
			verStr = fmt.Sprintf(" (version: %s)", strings.Join(s.installedVersions, ", "))
		}
		b.WriteString("\n" + theme.ErrorPanel.Width(min(w-4, 88)).Render(
			theme.Bad.Render(fmt.Sprintf("Cluster %q already runs Substrate%s.", sel.Name, verStr))+"\n\n"+
				"Re-running the install track against an installed cluster is unsupported\n"+
				"and produces a broken, mixed-version cluster:\n"+
				"  • Control plane deployments roll to the new commit immediately.\n"+
				"  • atelet and worker pools remain pinned to the older version,\n"+
				"    causing router contract mismatches (e.g. 421 Misdirected Request).\n\n"+
				theme.Title.Render("Supported paths forward:")+"\n"+
				"  1. Upgrade this cluster: exit or restart the installer and choose\n"+
				"     \"Upgrade an installed cluster\" to follow the rolling upgrade runbook.\n\n"+
				"  2. Tear down and reinstall: press "+theme.Key.Render("[t]")+" to delete the Substrate control\n"+
				"     plane on this cluster now (the cluster and its snapshots are kept)\n"+
				"     and continue the install here. For the full GCP cleanup — cluster,\n"+
				"     bucket, IAM, dashboards — run instead:\n"+
				"     "+snapshot.CleanupCommand(s.deps.Setup.ProjectID, sel.Name, sel.Location, "")+"\n"+
				"     (--bucket: the snapshot bucket that install used)\n\n"+
				theme.Key.Render("[t]")+" tear down here   "+theme.Key.Render("[esc]")+" choose another   "+theme.Key.Render("[r]")+" re-probe"))
	case "teardown-confirm":
		sel := s.clusters[s.cursor]
		b.WriteString("\n" + theme.ErrorPanel.Width(min(w-4, 76)).Render(
			theme.Warning.Render(fmt.Sprintf("Tear down Substrate on %q?", sel.Name))+"\n\n"+
				"Runs `ate-setup delete ate-system` against the cluster: the control\n"+
				"plane and every running actor are deleted. The cluster, its nodes,\n"+
				"and the snapshot bucket are kept. The install then continues here.\n\n"+
				theme.Key.Render("[y]")+" tear it down   "+theme.Key.Render("[esc]")+" back"))
	case "teardown":
		sel := s.clusters[s.cursor]
		b.WriteString("\n" + theme.Subtle.Render(fmt.Sprintf("Tearing down Substrate on %s…", sel.Name)) + "\n\n")
		b.WriteString(s.comp.view(w))
	case "partial":
		sel := s.clusters[s.cursor]
		b.WriteString("\n" + theme.ErrorPanel.Width(min(w-4, 76)).Render(
			theme.Warning.Render(fmt.Sprintf("Cluster %q has an ate-system namespace but no atelet.", sel.Name))+"\n\n"+
				"That looks like an interrupted install or an unfinished delete, not a\n"+
				"running Substrate. Re-running the install over it is safe: upstream's\n"+
				"deploy steps are idempotent.\n\n"+
				theme.Key.Render("[y]")+" continue   "+theme.Key.Render("[r]")+" re-probe   "+theme.Key.Render("[esc]")+" choose another"))
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
