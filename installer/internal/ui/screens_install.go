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
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/steps"
	"github.com/ai-on-gke/substrate-gke/installer/internal/theme"
)

// ─── Provision GCP resources ───────────────────────────────────────────────

type provisionScreen struct {
	deps *Deps
	comp *execComp
}

func newProvisionScreen(deps *Deps) *provisionScreen {
	return &provisionScreen{
		deps: deps,
		comp: newExecComp(deps.Runner, deps.Builder.Bootstrap(deps.Setup), steps.Bootstrap()),
	}
}

func (s *provisionScreen) Init() tea.Cmd      { return s.comp.start() }
func (s *provisionScreen) CapturesText() bool { return false }

func (s *provisionScreen) Hints() []Hint {
	switch {
	case s.comp.ok():
		return []Hint{{"enter", "continue"}}
	case s.comp.failed != nil:
		return []Hint{{"r", "retry"}, {"b", "back"}}
	}
	return nil
}

func (s *provisionScreen) Update(msg tea.Msg) tea.Cmd {
	if cmd, handled := s.comp.update(msg); handled {
		return cmd
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter":
			if s.comp.ok() {
				return goNext
			}
		case "r":
			if s.comp.failed != nil {
				return s.comp.restart()
			}
		case "b", "esc":
			if !s.comp.running() {
				s.comp.stop()
				return goBack
			}
		}
	}
	return nil
}

func (s *provisionScreen) View(w int) string {
	st := s.deps.Setup
	var b strings.Builder
	b.WriteString(theme.Title.Render("Provision GCP resources") + "\n")
	if st.ClusterIsNew {
		b.WriteString(theme.Subtle.Render(fmt.Sprintf(
			"Creating cluster %s in %s — expect 8–12 minutes. All steps are idempotent.", st.ClusterName, st.Zone)) + "\n\n")
	} else {
		b.WriteString(theme.Subtle.Render(fmt.Sprintf(
			"Cluster %s already exists; bootstrap is idempotent and only fills in the bucket, IAM, and dashboards.", st.ClusterName)) + "\n\n")
	}
	b.WriteString(s.comp.view(w))
	if s.comp.ok() {
		b.WriteString("\n" + theme.Good.Render("GCP resources are ready. Press [enter] to turn on Substrate."))
	}
	return b.String()
}

// ─── Turn on Substrate (control plane) ─────────────────────────────────────

type controlPlaneScreen struct {
	deps       *Deps
	comp       *execComp
	showDrawer bool
}

func newControlPlaneScreen(deps *Deps) *controlPlaneScreen {
	return &controlPlaneScreen{
		deps: deps,
		comp: newExecComp(deps.Runner, deps.Builder.DeployAteSystem(deps.Setup), steps.Deploy(deps.Setup.Prebuilt())),
	}
}

func (s *controlPlaneScreen) Init() tea.Cmd      { return s.comp.start() }
func (s *controlPlaneScreen) CapturesText() bool { return false }

func (s *controlPlaneScreen) Hints() []Hint {
	hints := []Hint{{"m", "command drawer"}}
	switch {
	case s.comp.ok():
		hints = append([]Hint{{"enter", "continue"}}, hints...)
	case s.comp.failed != nil:
		hints = append([]Hint{{"r", "retry"}, {"b", "back"}}, hints...)
	}
	return hints
}

func (s *controlPlaneScreen) Update(msg tea.Msg) tea.Cmd {
	if cmd, handled := s.comp.update(msg); handled {
		return cmd
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "m":
			s.showDrawer = !s.showDrawer
		case "enter":
			if s.comp.ok() {
				return goNext
			}
		case "r":
			if s.comp.failed != nil {
				return s.comp.restart()
			}
		case "b", "esc":
			if !s.comp.running() {
				s.comp.stop()
				return goBack
			}
		}
	}
	return nil
}

func (s *controlPlaneScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Turn on Substrate") + "\n")
	subtitle := "Builds the control-plane images from the substrate checkout with ko and\ninstalls CRDs, the API server, controller, atenet, and atelet."
	if s.deps.Setup.Prebuilt() {
		subtitle = "Installs CRDs, the API server, controller, atenet, and atelet from\n" + s.deps.Setup.ImageSummary() + "."
	}
	b.WriteString(theme.Subtle.Render(subtitle) + "\n\n")
	b.WriteString(s.comp.view(w))

	if s.showDrawer {
		env := strings.Join(s.comp.spec.Env, "\n")
		b.WriteString("\n" + theme.Panel.Width(min(w-4, 90)).Render(
			theme.Title.Render("Environment for "+s.comp.spec.Display)+"\n"+theme.Subtle.Render(env)))
	}
	if s.comp.ok() {
		b.WriteString("\n" + theme.Good.Render("The Substrate control plane is running. Press [enter] to continue."))
	}
	return b.String()
}

// ─── Install Filestore CSI driver ──────────────────────────────────────────

type filestoreScreen struct {
	deps   *Deps
	cursor int
	comp   *execComp
}

func newFilestoreScreen(deps *Deps) *filestoreScreen {
	return &filestoreScreen{deps: deps}
}

func (s *filestoreScreen) Init() tea.Cmd      { return nil }
func (s *filestoreScreen) CapturesText() bool { return false }

func (s *filestoreScreen) Hints() []Hint {
	if s.comp != nil {
		if s.comp.ok() {
			return []Hint{{"enter", "continue"}}
		}
		if s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"s", "skip"}}
		}
		return nil
	}
	return []Hint{{"1/2", "choose"}, {"enter", "confirm"}, {"s", "skip"}, {"b", "back"}}
}

func (s *filestoreScreen) Update(msg tea.Msg) tea.Cmd {
	if s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			return cmd
		}
		if key, ok := msg.(tea.KeyMsg); ok {
			switch key.String() {
			case "enter":
				if s.comp.ok() {
					s.deps.Setup.FilestoreCSIDeployed = true
					return goNext
				}
			case "r":
				if s.comp.failed != nil {
					return s.comp.restart()
				}
			case "s":
				if s.comp.failed != nil {
					return goNext
				}
			}
		}
		return nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "1", "up", "k":
		s.cursor = 0
	case "2", "down", "j":
		s.cursor = 1
	case "s":
		return goNext
	case "b", "esc":
		return goBack
	case "enter":
		if s.cursor == 1 {
			return goNext
		}
		s.comp = newExecComp(s.deps.Runner, s.deps.Builder.DeployFilestoreCSI(s.deps.Setup), steps.FilestoreCSI())
		return s.comp.start()
	}
	return nil
}

func (s *filestoreScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Install Filestore CSI driver") + "\n")
	b.WriteString(theme.Subtle.Render("Optional: install GCP Filestore CSI Driver configured for Substrate.\nNote: This will disable the managed Filestore CSI driver if enabled.") + "\n\n")

	if s.comp != nil {
		b.WriteString(s.comp.view(w))
		if s.comp.ok() {
			b.WriteString("\n" + theme.Good.Render("Filestore CSI driver deployed. Press [enter] to continue."))
		}
		return b.String()
	}

	options := []string{
		"[1] Install Filestore CSI driver (Substrate overlay)",
		"[2] Skip — I'll configure storage drivers later",
	}
	for i, opt := range options {
		if i == s.cursor {
			b.WriteString(theme.Selected.Render(" "+opt+" ") + "\n")
		} else {
			b.WriteString(theme.Subtle.Render("  "+opt) + "\n")
		}
	}
	return b.String()
}

// ─── Configure autoscaling ─────────────────────────────────────────────────

type poolsMsg struct {
	owner *autoscalingScreen
	pools []gcp.NodePool
	err   error
}

type autoscalingScreen struct {
	deps *Deps
	// mode: "loading", "choose", "bounds", "exec"
	mode   string
	pools  []gcp.NodePool
	err    error
	cursor int
	minIn  textinput.Model
	maxIn  textinput.Model
	focus  int
	comp   *execComp
}

func newAutoscalingScreen(deps *Deps) *autoscalingScreen {
	mk := func(v int) textinput.Model {
		in := textinput.New()
		in.SetValue(strconv.Itoa(v))
		in.CharLimit = 4
		in.Prompt = "  "
		in.Width = 6
		return in
	}
	return &autoscalingScreen{
		deps:  deps,
		mode:  "loading",
		minIn: mk(deps.Setup.AutoscaleMin),
		maxIn: mk(deps.Setup.AutoscaleMax),
	}
}

func (s *autoscalingScreen) Init() tea.Cmd {
	st := s.deps.Setup
	return func() tea.Msg {
		pools, err := s.deps.GCP.ListNodePools(context.Background(), st.ProjectID, st.ClusterName, st.Zone)
		return poolsMsg{s, pools, err}
	}
}

func (s *autoscalingScreen) CapturesText() bool { return s.mode == "bounds" }

func (s *autoscalingScreen) Hints() []Hint {
	switch s.mode {
	case "choose":
		return []Hint{{"↑/↓", "select"}, {"enter", "confirm"}, {"s", "skip"}, {"b", "back"}}
	case "bounds":
		return []Hint{{"tab", "min/max"}, {"enter", "apply"}, {"esc", "back"}}
	case "exec":
		if s.comp.ok() {
			return []Hint{{"enter", "continue"}}
		}
		if s.comp != nil && s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"s", "skip"}}
		}
	}
	return nil
}

func (s *autoscalingScreen) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case poolsMsg:
		if m.owner != s {
			return nil
		}
		s.pools, s.err = m.pools, m.err
		s.mode = "choose"
		// Preselect the pool setup-gcp creates.
		for i, p := range s.pools {
			if p.Name == s.deps.Setup.NodePool {
				s.cursor = i
			}
		}
		return nil
	}

	if s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			return cmd
		}
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}

	switch s.mode {
	case "choose":
		switch key.String() {
		case "up", "k":
			if s.cursor > 0 {
				s.cursor--
			}
		case "down", "j":
			if s.cursor < len(s.pools) {
				s.cursor++
			}
		case "s":
			return goNext
		case "b", "esc":
			return goBack
		case "enter":
			if s.err != nil || s.cursor == len(s.pools) {
				return goNext // skip row (or nothing listable)
			}
			s.deps.Setup.NodePool = s.pools[s.cursor].Name
			s.mode = "bounds"
			s.focus = 0
			return tea.Batch(s.minIn.Focus(), textinput.Blink)
		}

	case "bounds":
		switch key.String() {
		case "esc":
			s.mode = "choose"
			s.minIn.Blur()
			s.maxIn.Blur()
			return nil
		case "tab", "down", "up", "shift+tab":
			s.focus = 1 - s.focus
			if s.focus == 0 {
				s.maxIn.Blur()
				return s.minIn.Focus()
			}
			s.minIn.Blur()
			return s.maxIn.Focus()
		case "enter":
			minNodes, err1 := strconv.Atoi(strings.TrimSpace(s.minIn.Value()))
			maxNodes, err2 := strconv.Atoi(strings.TrimSpace(s.maxIn.Value()))
			if err1 != nil || err2 != nil || minNodes < 0 || maxNodes < max(minNodes, 1) {
				return nil
			}
			st := s.deps.Setup
			st.AutoscaleMin, st.AutoscaleMax = minNodes, maxNodes
			s.mode = "exec"
			s.comp = newExecComp(s.deps.Runner, s.deps.Builder.EnableAutoscaling(st), nil)
			return s.comp.start()
		}
		if s.focus == 0 {
			var cmd tea.Cmd
			s.minIn, cmd = s.minIn.Update(msg)
			return cmd
		}
		var cmd tea.Cmd
		s.maxIn, cmd = s.maxIn.Update(msg)
		return cmd

	case "exec":
		switch key.String() {
		case "enter":
			if s.comp.ok() {
				s.deps.Setup.AutoscaleEnabled = true
				return goNext
			}
		case "r":
			if s.comp.failed != nil {
				return s.comp.restart()
			}
		case "s":
			if s.comp.failed != nil {
				return goNext
			}
		}
	}
	return nil
}

func (s *autoscalingScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Configure autoscaling") + "\n")
	b.WriteString(theme.Subtle.Render("Optional: let GKE grow and shrink the Substrate node pool with demand.") + "\n\n")

	switch s.mode {
	case "loading":
		b.WriteString(theme.Accent.Render("  Loading node pools…"))

	case "choose":
		if s.err != nil {
			b.WriteString(theme.Warning.Render("  Could not list node pools: "+s.err.Error()) + "\n")
			b.WriteString(theme.Subtle.Render("  Press [enter] or [s] to skip this step.") + "\n")
			break
		}
		for i, p := range s.pools {
			status := ""
			if p.Autoscaled {
				status = theme.Good.Render(" (autoscaling already on)")
			}
			row := fmt.Sprintf("[%d] Enable autoscaling on %s  (%s)%s", i+1, p.Name, p.MachineType, status)
			if i == s.cursor {
				b.WriteString(theme.Selected.Render(" "+row+" ") + "\n")
			} else {
				b.WriteString(theme.Subtle.Render("  "+row) + "\n")
			}
		}
		skipRow := fmt.Sprintf("[%d] Skip — keep a fixed node count", len(s.pools)+1)
		if s.cursor == len(s.pools) {
			b.WriteString(theme.Selected.Render(" "+skipRow+" ") + "\n")
		} else {
			b.WriteString(theme.Subtle.Render("  "+skipRow) + "\n")
		}

	case "bounds":
		b.WriteString(theme.AccentPanel.Width(min(w-4, 50)).Render(
			theme.Title.Render("Node pool: "+s.deps.Setup.NodePool)+"\n\n"+
				"min nodes"+s.minIn.View()+"\n"+
				"max nodes"+s.maxIn.View()) + "\n")

	case "exec":
		b.WriteString(s.comp.view(w))
		if s.comp.ok() {
			b.WriteString("\n" + theme.Good.Render("Autoscaling enabled. Press [enter] to continue."))
		}
	}
	return b.String()
}

// ─── Deploy a demo workload ────────────────────────────────────────────────

type demoScreen struct {
	deps   *Deps
	cursor int
	comp   *execComp
}

func newDemoScreen(deps *Deps) *demoScreen { return &demoScreen{deps: deps} }

func (s *demoScreen) Init() tea.Cmd      { return nil }
func (s *demoScreen) CapturesText() bool { return false }

func (s *demoScreen) Hints() []Hint {
	if s.comp != nil {
		if s.comp.ok() {
			return []Hint{{"enter", "continue"}}
		}
		if s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"s", "skip"}}
		}
		return nil
	}
	return []Hint{{"1/2", "choose"}, {"enter", "confirm"}, {"b", "back"}}
}

func (s *demoScreen) Update(msg tea.Msg) tea.Cmd {
	if s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			return cmd
		}
		if key, ok := msg.(tea.KeyMsg); ok {
			switch key.String() {
			case "enter":
				if s.comp.ok() {
					s.deps.Setup.DemoDeployed = true
					return goNext
				}
			case "r":
				if s.comp.failed != nil {
					return s.comp.restart()
				}
			case "s":
				if s.comp.failed != nil {
					return goNext
				}
			}
		}
		return nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "1", "up", "k":
		s.cursor = 0
	case "2", "down", "j":
		s.cursor = 1
	case "b", "esc":
		return goBack
	case "enter":
		if s.cursor == 1 {
			return goNext
		}
		s.comp = newExecComp(s.deps.Runner, s.deps.Builder.DeployDemo(s.deps.Setup, "counter"), nil)
		return s.comp.start()
	}
	return nil
}

func (s *demoScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Deploy a demo workload") + "\n")
	b.WriteString(theme.Subtle.Render("The counter demo gives you a WorkerPool and an ActorTemplate to poke at.") + "\n\n")

	if s.comp != nil {
		b.WriteString(s.comp.view(w))
		if s.comp.ok() {
			b.WriteString("\n" + theme.Good.Render("Counter demo deployed. Press [enter] to continue."))
		}
		return b.String()
	}

	options := []string{
		"[1] Deploy the counter demo (WorkerPool + ActorTemplate)",
		"[2] Skip — I'll deploy my own workloads",
	}
	for i, opt := range options {
		if i == s.cursor {
			b.WriteString(theme.Selected.Render(" "+opt+" ") + "\n")
		} else {
			b.WriteString(theme.Subtle.Render("  "+opt) + "\n")
		}
	}
	return b.String()
}

// ─── Complete ──────────────────────────────────────────────────────────────

type completeScreen struct {
	deps *Deps
	comp *execComp
}

func newCompleteScreen(deps *Deps) *completeScreen { return &completeScreen{deps: deps} }

func (s *completeScreen) Init() tea.Cmd      { return nil }
func (s *completeScreen) CapturesText() bool { return false }

func (s *completeScreen) Hints() []Hint {
	if s.comp == nil && !s.deps.Setup.Upgrade {
		return []Hint{{"y", "verify the install"}, {"enter/q", "finish"}}
	}
	return []Hint{{"enter/q", "finish"}}
}

func (s *completeScreen) Update(msg tea.Msg) tea.Cmd {
	if s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			if s.comp.ok() {
				s.deps.Setup.Verified = true
			}
			return cmd
		}
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "y":
			if s.comp == nil && !s.deps.Setup.Upgrade {
				s.comp = newExecComp(s.deps.Runner, s.deps.Builder.Verify(s.deps.Setup), nil)
				return s.comp.start()
			}
		case "enter", "q":
			return doQuit
		}
	}
	return nil
}

func (s *completeScreen) View(w int) string {
	st := s.deps.Setup
	var b strings.Builder
	if st.Upgrade {
		installedDir, nextDir := s.deps.Builder.UpgradeTrees(s.deps.UpgradeDir, st)
		b.WriteString(theme.Good.Render("● UPGRADE PREPARED") + "\n\n")
		b.WriteString(theme.Panel.Width(w-4).Render(s.deps.Builder.UpgradeSummary(st, installedDir, nextDir)) + "\n")
		return b.String()
	}
	b.WriteString(theme.Good.Render("● SUBSTRATE IS ON") + "\n\n")

	summary := fmt.Sprintf(
		"project    %s\ncluster    %s (%s)%s\nbucket     gs://%s\nimages     %s\nfilestore  %s\nautoscale  %s\ndemo       %s",
		st.ProjectID,
		st.ClusterName, st.Zone, map[bool]string{true: "  · created by this run", false: ""}[st.ClusterIsNew],
		st.BucketName,
		st.ImageSummary(),
		map[bool]string{true: "installed", false: "skipped"}[st.FilestoreCSIDeployed],
		map[bool]string{true: fmt.Sprintf("on (%d–%d nodes, %s)", st.AutoscaleMin, st.AutoscaleMax, st.NodePool), false: "off"}[st.AutoscaleEnabled],
		map[bool]string{true: "counter demo deployed", false: "skipped"}[st.DemoDeployed],
	)
	b.WriteString(theme.Panel.Width(min(w-4, 76)).Render(summary) + "\n")

	if s.comp != nil {
		b.WriteString("\n" + s.comp.view(w) + "\n")
	} else {
		b.WriteString("\n" + theme.Subtle.Render("Press [y] to run `kubectl get pods -n ate-system` and see it live.") + "\n")
	}

	// A managed checkout is removed once the install succeeds, so its next
	// steps cannot ask the user to run anything inside it — offer the
	// self-contained install command instead.
	installAte := `go install ./cmd/kubectl-ate    # run inside your substrate checkout`
	if s.deps.Builder.Managed {
		installAte = s.deps.Builder.KubectlAteInstall()
	}
	next := theme.Title.Render("Next steps") + "\n" +
		theme.CommandLine.Render("kubectl port-forward -n ate-system svc/atenet-router 8000:80") + "\n" +
		theme.Subtle.Render("then, if you deployed the counter demo:") + "\n" +
		theme.CommandLine.Render(installAte) + "\n" +
		theme.CommandLine.Render("kubectl ate create atespace demo") + "\n" +
		theme.CommandLine.Render("kubectl ate create actor my-counter-1 -a demo --template=ate-demo-counter/counter") + "\n" +
		theme.CommandLine.Render(`curl -X POST -H "Host: my-counter-1.demo.actors.resources.substrate.ate.dev" http://localhost:8000/`)
	b.WriteString("\n" + theme.AccentPanel.Width(min(w-4, 92)).Render(next))
	return b.String()
}
