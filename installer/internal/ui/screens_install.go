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
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gcp"
	"github.com/ai-on-gke/substrate-gke/installer/internal/snapshot"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
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
		comp: newExecComp(deps.Runner, deps.Builder.Bootstrap(deps.Setup), steps.Bootstrap(deps.Setup.ClusterIsNew, deps.Setup.EnableBetaAPIs), deps.LogPath),
	}
}

func (s *provisionScreen) Init() tea.Cmd      { return s.comp.start() }
func (s *provisionScreen) CapturesText() bool { return false }
func (s *provisionScreen) logComp() *execComp { return s.comp }

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
	switch {
	case st.ClusterIsNew:
		b.WriteString(theme.Subtle.Render(fmt.Sprintf(
			"Creating cluster %s in %s — expect 8–12 minutes. All steps are idempotent.", st.ClusterName, st.Zone)) + "\n\n")
	case st.EnableBetaAPIs:
		// The cluster screen has just told the user this update is coming;
		// the "only fills in the bucket" line below would contradict it
		// while a ten-minute control-plane update runs underneath.
		b.WriteString(theme.Subtle.Render(fmt.Sprintf(
			"Turning on the beta PodCertificate APIs for cluster %s — expect a control-plane update of roughly ten minutes, then the bucket, IAM, and dashboards.", st.ClusterName)) + "\n\n")
	default:
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
		comp: newExecComp(deps.Runner, deps.Builder.DeployAteSystem(deps.Setup), steps.Deploy(deps.Setup.Prebuilt()), deps.LogPath),
	}
}

func (s *controlPlaneScreen) Init() tea.Cmd      { return s.comp.start() }
func (s *controlPlaneScreen) CapturesText() bool { return false }
func (s *controlPlaneScreen) logComp() *execComp { return s.comp }

func (s *controlPlaneScreen) Hints() []Hint {
	hints := []Hint{{"m", "command drawer"}}
	switch {
	case s.comp.ok():
		hints = append(hints, Hint{"enter", "continue"})
	case s.comp.failed != nil:
		hints = append(hints, Hint{"r", "retry"}, Hint{"b", "back"})
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
	subtitle += "\nEgress credential provider: " + snapshot.CredentialProvider() + " (set ATE_CREDENTIAL_PROVIDER to change it)."
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

// ─── Choose your sandbox runtime ───────────────────────────────────────────

// clusterKVMMsg is the answer to the KVM probe Init starts. It names the
// cluster it asked about: screens are rebuilt on every visit, so an answer
// can arrive at a later sandboxScreen than the one that asked.
type clusterKVMMsg struct {
	cluster string
	ready   bool
	err     error
}

type sandboxScreen struct {
	deps            *Deps
	cursor          int
	confirmingNoKVM bool
	// kvmPending is true while the probe Init started is in flight. Until
	// it answers, a cluster not already known to be KVM-ready reads as
	// "checking" rather than as having no KVM-capable pool, and Micro-VM
	// cannot be confirmed.
	kvmPending   bool
	kvmErr       error
	missingTools []string
	comp         *execComp
}

func newSandboxScreen(deps *Deps) *sandboxScreen {
	var missing []string
	if !deps.DryRun {
		missing = doctor.MissingMicroVMTools()
	}
	cur := 0
	if deps.Setup.MicroVM() && len(missing) == 0 {
		cur = 1
	}
	return &sandboxScreen{deps: deps, cursor: cur, missingTools: missing}
}

func (s *sandboxScreen) Init() tea.Cmd {
	if s.deps.GCP == nil || s.deps.GCP.DryRun {
		return nil
	}
	s.kvmPending = true
	st := s.deps.Setup
	project, cluster, zone := st.ProjectID, st.ClusterName, st.Zone
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ready, err := s.deps.GCP.ClusterKVMReady(ctx, project, cluster, zone)
		return clusterKVMMsg{cluster: cluster, ready: ready, err: err}
	}
}

// awaitingKVM reports whether Micro-VM has to wait for the probe: the
// cluster is not already known to be KVM-ready and the answer is not in.
func (s *sandboxScreen) awaitingKVM() bool {
	return s.kvmPending && !s.deps.Setup.ClusterKVMReady
}

func (s *sandboxScreen) CapturesText() bool { return false }
func (s *sandboxScreen) logComp() *execComp { return s.comp }

func (s *sandboxScreen) Hints() []Hint {
	if s.comp != nil {
		if s.comp.ok() {
			return []Hint{{"enter", "continue"}}
		}
		if s.comp.failed != nil {
			return []Hint{{"r", "retry"}, {"s", "skip"}}
		}
		return nil
	}
	if s.confirmingNoKVM {
		return []Hint{{"y", "stage anyway"}, {"n", "keep gVisor"}}
	}
	if len(s.missingTools) > 0 {
		return []Hint{{"enter", "confirm"}, {"b", "back"}}
	}
	if s.cursor == 1 && s.awaitingKVM() {
		return []Hint{{"1/2", "choose"}, {"b", "back"}}
	}
	return []Hint{{"1/2", "choose"}, {"enter", "confirm"}, {"b", "back"}}
}

func (s *sandboxScreen) startMicroVMStage() tea.Cmd {
	s.confirmingNoKVM = false
	s.deps.Setup.SandboxClass = state.SandboxMicroVM
	s.comp = newExecComp(s.deps.Runner, s.deps.Builder.InstallMicroVMDeps(s.deps.Setup), steps.MicroVMDeps(), s.deps.LogPath)
	return s.comp.start()
}

func (s *sandboxScreen) Update(msg tea.Msg) tea.Cmd {
	if kvm, ok := msg.(clusterKVMMsg); ok {
		if kvm.cluster != s.deps.Setup.ClusterName {
			return nil
		}
		s.kvmPending = false
		s.kvmErr = kvm.err
		if kvm.err == nil {
			s.deps.Setup.ClusterKVMReady = kvm.ready
			if kvm.ready {
				// A no-KVM prompt raised on an earlier answer (a second
				// probe from a revisit can land later) no longer applies;
				// drop it rather than leave 1/2/enter swallowed behind a
				// hidden y/n.
				s.confirmingNoKVM = false
			}
		}
		return nil
	}
	if s.comp != nil {
		if cmd, handled := s.comp.update(msg); handled {
			if s.comp.ok() {
				s.deps.Setup.MicroVMDeployed = true
			}
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
			case "s":
				if s.comp.failed != nil {
					// The cluster keeps the gVisor config ate-setup already
					// applied, so a skipped micro-VM install leaves a working
					// install rather than a broken one. Record the class the
					// cluster actually ended up with.
					s.deps.Setup.SandboxClass = state.SandboxGVisor
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
	if s.confirmingNoKVM {
		switch key.String() {
		case "y", "Y":
			return s.startMicroVMStage()
		case "n", "N", "esc", "1":
			s.confirmingNoKVM = false
			s.cursor = 0
		}
		return nil
	}
	switch key.String() {
	case "1", "up", "k":
		s.cursor = 0
	case "2", "down", "j":
		if len(s.missingTools) == 0 {
			s.cursor = 1
		}
	case "b", "esc":
		return goBack
	case "enter":
		if s.cursor == 0 || len(s.missingTools) > 0 {
			s.deps.Setup.SandboxClass = state.SandboxGVisor
			return goNext
		}
		if s.awaitingKVM() {
			// Asking "stage anyway?" now would warn about a missing
			// KVM pool nobody has checked for yet. The view says the
			// check is running, and it answers within its timeout.
			return nil
		}
		if !s.deps.Setup.ClusterKVMReady {
			s.confirmingNoKVM = true
			return nil
		}
		return s.startMicroVMStage()
	}
	return nil
}

func (s *sandboxScreen) View(w int) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose your sandbox runtime") + "\n")
	b.WriteString(theme.Subtle.Render(
		"gVisor is installed with the control plane and needs nothing here.\n"+
			"Micro-VM is opt-in: its sandbox binaries are staged to the snapshot\nbucket and a cluster-wide SandboxConfig is applied.") + "\n\n")

	if s.comp != nil {
		b.WriteString(s.comp.view(w))
		if s.comp.ok() {
			b.WriteString("\n" + theme.Good.Render("Micro-VM SandboxConfig applied. Press [enter] to continue."))
		}
		return b.String()
	}

	opt2 := "[2] Micro-VM (kata + cloud-hypervisor) — stage assets and apply the SandboxConfig"
	if len(s.missingTools) > 0 {
		opt2 = "[2] Micro-VM (kata + cloud-hypervisor) — unavailable on this host"
	}
	options := []string{
		"[1] gVisor — already installed, nothing more to do",
		opt2,
	}
	for i, opt := range options {
		if i == s.cursor {
			b.WriteString(theme.Selected.Render(" "+opt+" ") + "\n")
		} else {
			b.WriteString(theme.Subtle.Render("  "+opt) + "\n")
		}
	}

	if len(s.missingTools) > 0 {
		b.WriteString("\n" + theme.Warning.Render(
			fmt.Sprintf("Note: Micro-VM staging is unavailable on this workstation (missing %s on PATH;\n"+
				"the Micro-VM sandbox and demo require %s).",
				strings.Join(s.missingTools, ", "), strings.Join(doctor.MicroVMTools, ", "))))
		return b.String()
	}

	st := s.deps.Setup
	switch {
	case st.ClusterKVMReady:
		b.WriteString("\n" + theme.Good.Render(
			fmt.Sprintf("%s Cluster %q has a KVM-capable node pool (nested virtualization enabled).",
				theme.GlyphDone, st.ClusterName)))
	case s.kvmPending:
		msg := fmt.Sprintf("Checking whether cluster %q has a KVM-capable node pool…", st.ClusterName)
		if s.cursor == 1 {
			msg += "\nWait for the check to finish before selecting Micro-VM."
		}
		b.WriteString("\n" + theme.Subtle.Render(msg))
	case s.kvmErr != nil:
		b.WriteString("\n" + theme.Warning.Render(
			fmt.Sprintf("Note: could not check whether cluster %q has a KVM-capable node pool (%v).\n"+
				"Micro-VM workers need /dev/kvm (enableNestedVirtualization or a -metal machine type),\n"+
				"or they will stay Pending and the demo will time out.",
				st.ClusterName, s.kvmErr)))
	default:
		b.WriteString("\n" + theme.Warning.Render(
			fmt.Sprintf("Note: cluster %q has no KVM-capable node pool yet. Micro-VM workers need /dev/kvm\n"+
				"(enableNestedVirtualization or a -metal machine type),\n"+
				"or they will stay Pending and the demo will time out.",
				st.ClusterName)))
	}
	if !st.ClusterKVMReady && s.confirmingNoKVM {
		// After a failed probe nothing is known about the pools, so the
		// prompt must not assert the outcome the note above could not check.
		warning := "Micro-VM workers will stay Pending on this cluster and the demo will time out.\n"
		if s.kvmErr != nil {
			warning = "Could not verify a KVM-capable node pool; Micro-VM workers may stay Pending and the demo may time out.\n"
		}
		b.WriteString("\n\n" + theme.Warning.Render(warning+
			"Press [y] to stage anyway, or [n] to keep gVisor."))
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
func (s *filestoreScreen) logComp() *execComp { return s.comp }

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
		s.comp = newExecComp(s.deps.Runner, s.deps.Builder.DeployFilestoreCSI(s.deps.Setup), steps.FilestoreCSI(), s.deps.LogPath)
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
func (s *autoscalingScreen) logComp() *execComp { return s.comp }

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
			s.comp = newExecComp(s.deps.Runner, s.deps.Builder.EnableAutoscaling(st), nil, s.deps.LogPath)
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
func (s *demoScreen) logComp() *execComp { return s.comp }

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
		s.comp = newExecComp(s.deps.Runner, s.deps.Builder.DeployDemo(s.deps.Setup, "counter"), nil, s.deps.LogPath)
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

	deployOpt := "[1] Deploy the counter demo (gVisor · WorkerPool + ActorTemplate)"
	if s.deps.Setup.MicroVMActive() {
		deployOpt = "[1] Deploy the counter-microvm demo (micro-VM · WorkerPool + ActorTemplate)"
	}
	options := []string{
		deployOpt,
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

// SandboxSummary describes the runtime this install ended up with. The choice
// and the staging are deliberately separate facts: the wizard has to ask which
// runtime you want before it can act on the answer, and the step that acts on
// it can still be skipped or fail, so a Setup can name micro-VM without having
// staged anything. Reporting only SandboxClass would claim an install that did
// not happen.
func SandboxSummary(st *state.Setup) string {
	switch {
	case !st.MicroVM():
		return "gVisor"
	case st.MicroVMDeployed:
		return "micro-VM  · assets staged, SandboxConfig applied"
	default:
		return "micro-VM  · chosen, but assets were not staged"
	}
}

// DemoName returns the counter demo variant that applies to this install.
func DemoName(st *state.Setup) string {
	if st.MicroVMActive() {
		return "counter-microvm"
	}
	return "counter"
}

// DemoSummary names the counter demo variant that was deployed.
func DemoSummary(st *state.Setup) string {
	if !st.DemoDeployed {
		return "skipped"
	}
	return DemoName(st) + " demo deployed"
}

type completeScreen struct {
	deps *Deps
	comp *execComp
}

func newCompleteScreen(deps *Deps) *completeScreen { return &completeScreen{deps: deps} }

func (s *completeScreen) Init() tea.Cmd      { return nil }
func (s *completeScreen) CapturesText() bool { return false }
func (s *completeScreen) logComp() *execComp { return s.comp }

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
				s.comp = newExecComp(s.deps.Runner, s.deps.Builder.Verify(s.deps.Setup), nil, s.deps.LogPath)
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
		"project    %s\ncluster    %s (%s)%s\nsandbox    %s\nbucket     gs://%s\nimages     %s\nfilestore  %s\nautoscale  %s\ndemo       %s",
		st.ProjectID,
		st.ClusterName, st.Zone, map[bool]string{true: "  · created by this run", false: ""}[st.ClusterIsNew],
		SandboxSummary(st),
		st.BucketName,
		st.ImageSummary(),
		map[bool]string{true: "installed", false: "skipped"}[st.FilestoreCSIDeployed],
		map[bool]string{true: fmt.Sprintf("on (%d–%d nodes, %s)", st.AutoscaleMin, st.AutoscaleMax, st.NodePool), false: "off"}[st.AutoscaleEnabled],
		DemoSummary(st),
	)
	b.WriteString(theme.Panel.Width(min(w-4, 76)).Render(summary) + "\n")

	if s.comp != nil {
		b.WriteString("\n" + s.comp.view(w) + "\n")
	} else {
		b.WriteString("\n" + theme.Subtle.Render("Press [y] to run `kubectl get pods -n ate-system` and see it live.") + "\n")
	}

	portForward, demo := s.deps.Builder.NextSteps(st)
	next := theme.Title.Render("Next steps") + "\n" +
		theme.CommandLine.Render(portForward)
	if st.DemoDeployed {
		next += "\n" + theme.Subtle.Render("then, to try the "+DemoName(st)+" demo:")
		for _, cmd := range demo {
			next += "\n" + theme.CommandLine.Render(cmd)
		}
	}
	b.WriteString("\n" + theme.AccentPanel.Width(min(w-4, 92)).Render(next))
	return b.String()
}
