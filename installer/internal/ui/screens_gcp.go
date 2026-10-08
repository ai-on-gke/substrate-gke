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
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ai-on-gke/substrate-gke/installer/internal/doctor"
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
	// billingOff and apiOff are set when the project provably has no
	// billing account or has GKEService disabled; probeErr means one of
	// those probes could not run, which proves neither.
	billingOff bool
	apiOff     bool
	probeErr   error
	// missing are bootstrap permissions the credentials provably lack;
	// permErr means the permission probe itself could not run.
	missing []gcp.RequiredPermission
	permErr error
	// docker are the docker checks that failed, when the install builds an
	// image with docker; see dockerProblem.
	docker []failedCheck
}

// failedCheck is a doctor check that did not pass, with what it found.
type failedCheck struct {
	name string
	res  doctor.Result
}

type projectScreen struct {
	deps       *Deps
	fields     []field
	focus      int
	validating bool
	// checkingDocker is set while validation also runs the docker checks,
	// so a slow docker is not taken for a slow gcloud.
	checkingDocker bool
	errText        string
	// permAcked is set once a permission problem has been shown, so the next
	// enter proceeds anyway: the probe is advisory (a role might be granted
	// minutes from now), but failing here beats failing mid-bootstrap.
	permAcked bool
	// probeAcked does the same for a billing or API probe that could not
	// run. A project provably without billing or the API is never waved
	// through: the very next screen would fail on it.
	//
	// Both acks hold only for the fields as they were: any edit resets
	// them, so changing the project shows its problems afresh.
	probeAcked bool
	// enableFor is the project [e] would enable GKEService on, set while
	// that offer is on screen; "" when there is no offer. Any edit to the
	// fields withdraws it, so e types normally again.
	enableFor string
	// enabling is the running `gcloud services enable`, nil otherwise.
	enabling *execComp
	// checkingEnable is set while a failed enable's cause is looked into;
	// see enableChecked.
	checkingEnable bool
	// billingCheck asks whether a project has billing. It is the gcp
	// client's BillingEnabled; tests swap in fixed answers.
	billingCheck func(ctx context.Context, projectID string) (bool, error)
}

// enableCheckedMsg carries a failed enable and what a fresh billing probe
// then said about the project.
type enableCheckedMsg struct {
	owner     *projectScreen
	projectID string
	cause     string
	billingOn bool
	err       error
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
	scr := &projectScreen{deps: deps, fields: fields, billingCheck: deps.GCP.BillingEnabled}
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

// CapturesText is false while the enable runs: no field takes input then,
// and the app's own keys ([v] log, / commands, ? help) must get through.
func (s *projectScreen) CapturesText() bool { return s.enabling == nil }

func (s *projectScreen) Hints() []Hint {
	if s.enabling != nil {
		return []Hint{{"esc", "stop waiting"}}
	}
	if s.enableFor != "" {
		return []Hint{{"e", "enable the GKE API"}, {"enter", "check again"}, {"esc", "back"}}
	}
	return []Hint{{"tab/↓", "next field"}, {"enter", "validate & continue"}, {"esc", "back"}}
}

func (s *projectScreen) setFocus(i int) tea.Cmd {
	// Moving to another field withdraws the [e] offer, like an edit does:
	// otherwise the first e typed there (europe-west1-b, e2-standard-4)
	// would start the enable instead.
	s.enableFor = ""
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
	s.enableFor = ""
	s.validating = true
	acked := s.permAcked
	registry := s.dockerRegistry(pid)
	s.checkingDocker = registry != ""
	return func() tea.Msg {
		msg := projValidMsg{owner: s}
		msg.number, msg.err = s.deps.GCP.ProjectNumber(context.Background(), pid)
		// The cluster step lists clusters next, and that fails outright on
		// a project without billing or without the GKE API. Catch both
		// here, where the fix is still one command away.
		if msg.err == nil {
			msg.billingOff, msg.apiOff, msg.probeErr = projectServing(context.Background(), s.deps.GCP, pid)
		}
		// No billing or no API blocks the step, and Update then shows
		// that alone, so the checks below would be paid for and thrown
		// away, on every retry.
		blocked := msg.err != nil || msg.billingOff || msg.apiOff
		// Check the bootstrap permissions now rather than failing three
		// screens later, mid-provision. Skipped once acknowledged.
		if !blocked && !acked {
			msg.missing, msg.permErr = s.deps.GCP.MissingPermissions(context.Background(), pid)
		}
		if !blocked && registry != "" {
			for _, c := range doctor.DockerChecks(registry) {
				if res := c.Run(context.Background()); res.Status == doctor.Fail {
					msg.docker = append(msg.docker, failedCheck{c.Name, res})
				}
			}
		}
		return msg
	}
}

// dockerRegistry is the registry the install will push to with docker, or ""
// when it builds nothing with docker. The doctor already ran the docker
// checks, but against the default registry and before the images step, where
// a user who meant to install pre-built images may have skipped them. Now
// that both the track and the registry are known, they run for real.
//
// A dry run skips them, as it skips the doctor's own probes.
func (s *projectScreen) dockerRegistry(pid string) string {
	if s.deps.DryRun || s.deps.Builder == nil {
		return ""
	}
	// What the fields would make of the setup, without committing them
	// before the project has validated.
	st := *s.deps.Setup
	for _, f := range s.fields {
		f.set(&st, strings.TrimSpace(f.input.Value()))
	}
	st.ProjectID = pid
	if !s.deps.Builder.BuildsWithDocker(&st) {
		return ""
	}
	if st.KoDockerRepo == "" {
		return st.DefaultKoDockerRepo()
	}
	return st.KoDockerRepo
}

// dockerProblem renders the docker checks that failed. Unlike a permission
// problem it cannot be waved through: nothing outside this machine is going
// to fix it, and the build it blocks runs only after the control plane is
// applied.
func dockerProblem(failed []failedCheck) string {
	var b strings.Builder
	b.WriteString("This substrate builds its envoy-dataplane image with docker buildx, and docker is not ready:\n")
	for _, f := range failed {
		fmt.Fprintf(&b, "  %s: %s\n", f.name, f.res.Detail)
		if f.res.Fix != "" {
			fmt.Fprintf(&b, "    fix: %s\n", f.res.Fix)
		}
	}
	b.WriteString("Fix them, then press [enter] to check again. Or set ATE_ATENET_DATAPLANE=agentgateway before starting the installer, which builds nothing with docker.")
	return b.String()
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

// projectServing asks whether projectID has billing and the GKE API on.
// Each probe that could not run adds to probeErr instead of claiming either
// answer. The two probes run concurrently and share the Client's cached
// access token, so together they cost one round trip, not two.
func projectServing(ctx context.Context, gc *gcp.Client, projectID string) (billingOff, apiOff bool, probeErr error) {
	var (
		wg                 sync.WaitGroup
		billingOn, apiOn   bool
		billingErr, apiErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		billingOn, billingErr = gc.BillingEnabled(ctx, projectID)
	}()
	go func() {
		defer wg.Done()
		apiOn, apiErr = gc.ServiceEnabled(ctx, projectID, gcp.GKEService)
	}()
	wg.Wait()
	if billingErr == nil {
		billingOff = !billingOn
	}
	if apiErr == nil {
		apiOff = !apiOn
	}
	return billingOff, apiOff, errors.Join(billingErr, apiErr)
}

// billingProblem renders a project without billing. There is no offer to fix
// it here: linking an account needs the user to pick one, and often a
// billing admin to allow it.
func billingProblem(projectID string, apiOff bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Billing is not enabled on %s; GKE refuses every request without it.\n", projectID)
	b.WriteString(billingFix(projectID))
	if apiOff {
		fmt.Fprintf(&b, "%s is disabled too; once billing is linked, this screen can enable it for you.\n", gcp.GKEService)
	}
	b.WriteString(billingNext)
	return b.String()
}

// billingFix is the fix for a project without billing, shared by every
// panel that reports one so they cannot drift apart.
func billingFix(projectID string) string {
	return fmt.Sprintf("  fix: gcloud billing projects link %s --billing-account=ACCOUNT_ID\n"+
		"   or: https://console.cloud.google.com/billing/linkedaccount?project=%s\n", projectID, projectID)
}

// billingNext closes a billing panel: nothing here can link an account.
const billingNext = "Link an account, then press [enter] to check again."

// apiProblem renders a project with billing but without the GKE API, and
// offers to enable it.
func apiProblem(projectID string) string {
	return fmt.Sprintf("%s is disabled on %s; the cluster step cannot list or create clusters without it.\n"+
		"  fix: %s\n"+
		"Press [e] to enable it now, or [enter] to check again.",
		gcp.GKEService, projectID, gcp.EnableServiceCommand(projectID, gcp.GKEService))
}

// probeProblem renders a billing or API probe that could not run. Like a
// permission probe failure it proves nothing, so a second enter goes on.
func probeProblem(projectID string, err error) string {
	return fmt.Sprintf("Could not verify billing and the GKE API on %s:\n%v\n"+
		"Press [enter] again to continue anyway; the cluster step may fail.", projectID, err)
}

// enableProblem renders a failed `gcloud services enable`, with the fix for
// kind. A project provably without billing never gets here: it takes the
// billingProblem panel instead (see enableChecked).
func enableProblem(projectID, cause string, kind enableFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Could not enable %s on %s", gcp.GKEService, projectID)
	if cause != "" {
		b.WriteString(":\n" + cause)
	}
	b.WriteString("\n")
	switch kind {
	case enableFailedBilling:
		fmt.Fprintf(&b, "Billing must be enabled on %s before any API can be.\n", projectID)
		b.WriteString(billingFix(projectID))
		b.WriteString(billingNext)
		return b.String()
	case enableFailedPermission:
		b.WriteString("Enabling it needs serviceusage.services.enable (roles/serviceusage.serviceUsageAdmin).\n")
	}
	fmt.Fprintf(&b, "  fix: %s\n"+
		"Press [e] to try again, or [enter] to check again.", gcp.EnableServiceCommand(projectID, gcp.GKEService))
	return b.String()
}

type enableFailure int

const (
	enableFailedOther enableFailure = iota
	enableFailedPermission
	enableFailedBilling
)

// enableFailureKind sorts a `gcloud services enable` error by its cause.
// gcloud prints the API's status, e.g. "FAILED_PRECONDITION: Billing must
// be enabled for activation of service(s)" or "PERMISSION_DENIED:
// Permission denied to enable service". It matches status names, never
// bare codes like 403: the line often carries operation names and project
// numbers that contain those digits.
//
// Whether billing is the cause is asked of the billing API first; the
// text is consulted for billing only when that probe cannot answer.
func enableFailureKind(cause string) enableFailure {
	lower := strings.ToLower(cause)
	switch {
	case strings.Contains(lower, "billing"):
		return enableFailedBilling
	case strings.Contains(cause, "PERMISSION_DENIED"), strings.Contains(lower, "permission denied"):
		return enableFailedPermission
	}
	return enableFailedOther
}

// enable runs `gcloud services enable` for the offered project.
func (s *projectScreen) enable() tea.Cmd {
	s.errText = ""
	s.enabling = newExecComp(s.deps.Runner, gcp.EnableService(s.enableFor, gcp.GKEService), nil, s.deps.LogPath)
	return s.enabling.start()
}

// enableDone routes the finished enable: on success every check runs again,
// so the screen advances only once the whole project validates. On failure
// it asks the billing API whether billing is why, rather than guessing from
// gcloud's text; enableChecked then picks the panel.
func (s *projectScreen) enableDone() tea.Cmd {
	comp := s.enabling
	s.enabling = nil
	if comp.failed == nil {
		return s.submit()
	}
	cause := comp.cause
	if cause == "" {
		cause = comp.failed.Error()
	}
	pid := s.enableFor
	s.validating, s.checkingEnable = true, true
	check := s.billingCheck
	return func() tea.Msg {
		on, err := check(context.Background(), pid)
		return enableCheckedMsg{owner: s, projectID: pid, cause: cause, billingOn: on, err: err}
	}
}

// enableChecked shows a failed enable once the billing probe has answered.
//
//   - Billing provably off: the billing panel, as on submit. [e] is
//     withdrawn: no enable can work until an account is linked.
//   - Billing provably on: gcloud's text is read for the permission case
//     only; anything else gets the cause and the manual command.
//   - The probe could not answer: gcloud's text is the only evidence left,
//     so a billing precondition in it still gets the billing fix. This is
//     the case [e] is offered in without a billing answer (probeErr on
//     submit), so it is where a billing failure is most likely, and
//     dropping the fallback would show a generic panel for it.
func (s *projectScreen) enableChecked(m enableCheckedMsg) {
	s.validating, s.checkingEnable = false, false
	switch {
	case m.err == nil && !m.billingOn:
		s.enableFor = ""
		s.errText = billingProblem(m.projectID, true)
		return
	case m.err == nil:
		kind := enableFailureKind(m.cause)
		if kind == enableFailedBilling {
			// The API says billing is on; the text is wrong or stale.
			kind = enableFailedOther
		}
		s.errText = enableProblem(m.projectID, m.cause, kind)
	default:
		kind := enableFailureKind(m.cause)
		s.errText = enableProblem(m.projectID, m.cause, kind)
		if kind == enableFailedBilling {
			// The billing panel says to link an account and press
			// enter: another enable would fail the same way.
			s.enableFor = ""
		}
	}
}

// Stop cancels an enable still running when the wizard leaves the screen.
func (s *projectScreen) Stop() {
	if s.enabling != nil {
		s.enabling.stop()
	}
}

// logComp exposes the running enable, so [l]/[v] show its live output.
func (s *projectScreen) logComp() *execComp { return s.enabling }

func (s *projectScreen) Update(msg tea.Msg) tea.Cmd {
	if s.enabling != nil {
		if cmd, handled := s.enabling.update(msg); handled {
			if s.enabling.finished {
				return s.enableDone()
			}
			return cmd
		}
	}
	switch m := msg.(type) {
	case prefillMsg:
		if m.owner == s && s.fields[0].input.Value() == "" {
			s.fields[0].input.SetValue(m.project)
		}
		return nil

	case enableCheckedMsg:
		if m.owner == s {
			s.enableChecked(m)
		}
		return nil

	case projValidMsg:
		if m.owner != s {
			return nil
		}
		s.validating = false
		pid := strings.TrimSpace(s.fields[0].input.Value())
		if m.err != nil {
			s.errText = m.err.Error()
			return nil
		}
		// Billing and the API first: nothing after them works without
		// them, and neither can be waved through.
		if m.billingOff {
			s.errText = billingProblem(pid, m.apiOff)
			return nil
		}
		if m.apiOff {
			s.enableFor = pid
			s.errText = apiProblem(pid)
			return nil
		}
		if len(m.docker) > 0 {
			s.errText = dockerProblem(m.docker)
			return nil
		}
		if m.probeErr != nil && !s.probeAcked {
			s.probeAcked = true
			s.errText = probeProblem(pid, m.probeErr)
			return nil
		}
		if len(m.missing) > 0 || m.permErr != nil {
			s.permAcked = true
			s.errText = permProblem(pid, m.missing, m.permErr)
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
		if s.enabling != nil {
			// Only esc gets through: it abandons the wait, not the
			// operation, which gcloud may already have started server-side.
			if m.String() == "esc" {
				s.enabling.stop()
				s.enabling = nil
				s.errText = fmt.Sprintf("Stopped waiting for %s to enable. Press [enter] to check again.", gcp.GKEService)
			}
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
		case "e":
			if s.enableFor != "" {
				return s.enable()
			}
		}
		// Editing a field withdraws the offer: it was for the project as
		// validated, and e has to type again. It also re-arms the
		// advisory warnings: "continue anyway" was said about the project
		// as it was, and must not wave a different one through.
		s.enableFor = ""
		s.probeAcked = false
		s.permAcked = false
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
	case s.enabling != nil:
		b.WriteString(theme.Accent.Render(fmt.Sprintf("Enabling %s on %s… (this can take a minute or two)", gcp.GKEService, s.enableFor)) + "\n\n")
		b.WriteString(s.enabling.view(w))
	case s.checkingEnable:
		b.WriteString(theme.Accent.Render(fmt.Sprintf("Enabling %s failed; checking billing on %s…", gcp.GKEService, s.enableFor)))
	case s.validating && s.checkingDocker:
		b.WriteString(theme.Accent.Render("Validating project, billing and APIs with gcloud and checking Docker…"))
	case s.validating:
		b.WriteString(theme.Accent.Render("Validating project, billing and APIs with gcloud…"))
	case s.errText != "":
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 90)).Render(theme.Bad.Render(s.errText)))
	default:
		b.WriteString(theme.Subtle.Render("On submit the project is validated, and billing and the GKE API are checked."))
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
	st.ClusterKVMReady = c.KVMReady
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
				st.ClusterKVMReady = false
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
