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
	"time"

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
	// check, when set, returns why the submitted value cannot be used, or ""
	// when it can. It runs before anything reaches the network.
	check func(v string) string
}

type prefillMsg struct {
	owner   *projectScreen
	project string
}

type projValidMsg struct {
	owner *projectScreen
	// seq is the submit this answers; see projectScreen.validateSeq.
	seq    int
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
	// channel is why GKE would refuse the new-cluster version on the chosen
	// channel, or ""; see channelProblem. versionWrong says the version is
	// the field to fix, and channelPair is the pair that was checked.
	channel      string
	versionWrong bool
	channelPair  string
}

// failedCheck is a doctor check that did not pass, with what it found.
type failedCheck struct {
	name string
	res  doctor.Result
}

type projectScreen struct {
	deps            *Deps
	fields          []field
	repositoryField int
	focus           int
	validating      bool
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
	// see enableChecked. checkCancel abandons that check (esc, or leaving
	// the screen), checkSeq tells its result from an abandoned one's, and
	// checkPending is the failure being looked into, shown from gcloud's
	// text alone if the check is abandoned.
	checkingEnable bool
	checkCancel    context.CancelFunc
	checkSeq       int
	checkPending   enableCheckedMsg
	// billingCheck asks whether a project has billing. It is the gcp
	// client's BillingEnabled; tests swap in fixed answers.
	billingCheck func(ctx context.Context, projectID string) (bool, error)
	// projectNumber resolves the project ID, the first call a submit makes.
	// It is the gcp client's ProjectNumber; tests swap in one that blocks.
	projectNumber func(ctx context.Context, projectID string) (string, error)
	// validateCancel abandons a running submit's checks (esc, or leaving
	// the screen), and validateSeq tells its result from an abandoned
	// one's, which must not advance the wizard.
	validateCancel context.CancelFunc
	validateSeq    int
	// channelAcked is the version/channel pair whose channel problem has
	// been shown, so the next submit with the same pair proceeds. Advisory
	// like the others, and one more reason: the pair only matters if the
	// next screen creates a cluster, and GKE rolls versions out per
	// location, so a user connecting an existing cluster must not be stopped
	// by it. Reset on any edit, like the others, so changing the zone (or
	// anything else) checks again.
	channelAcked string
}

// submitTimeout bounds a submit's checks as a whole. A healthy run takes a
// few seconds; with the network down each gcloud call would otherwise wait
// its own cmdTimeout, one after the other. A var only so tests can shorten
// it.
var submitTimeout = 30 * time.Second

// enableCheckTimeout bounds the billing check after a failed enable. The
// enable often failed because the network is down, and the panel it
// delays used to appear at once.
const enableCheckTimeout = 10 * time.Second

// enableCheckedMsg carries a failed enable and what a fresh billing probe
// then said about the project.
type enableCheckedMsg struct {
	owner     *projectScreen
	seq       int
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
			// New clusters only: an existing cluster is always given its own
			// network (see Setup.BootstrapNetwork).
			newField("VPC network (new clusters)", st.Network, "default", func(s *state.Setup, v string) { s.Network = v }),
			newField("VPC subnetwork (new clusters)", st.Subnetwork, "default", func(s *state.Setup, v string) { s.Subnetwork = v }),
			// Only consulted when the run creates a cluster. Offered anyway
			// rather than hidden behind that choice, which is made on the next
			// screen: the fields would have to appear after it, splitting one
			// form into two.
			clusterVersionField(st.ClusterVersion),
			releaseChannelField(st.ReleaseChannel),
		)
		// Only a build from source pushes images anywhere, so only it needs a
		// registry to push them to.
		if !st.Prebuilt() {
			placeholder := "<region>-docker.pkg.dev/<project>/ate-images"
			fields = append(fields,
				newField("Image registry (leave empty for default)", st.KoDockerRepo, placeholder, func(s *state.Setup, v string) { s.KoDockerRepo = v }),
			)
		}
	}
	repositoryField := -1
	if st.Track == state.TrackAdvanced && !st.Prebuilt() {
		repositoryField = len(fields)
		fields = append(fields, newField("Artifact Registry repository (leave empty for default)", st.ArtifactRegistryRepository, "ate-images", func(s *state.Setup, v string) { s.ArtifactRegistryRepository = v }))
	}
	scr := &projectScreen{deps: deps, fields: fields, repositoryField: repositoryField,
		billingCheck: deps.GCP.BillingEnabled, projectNumber: deps.GCP.ProjectNumber}
	scr.fields[0].input.Focus()
	return scr
}

// clusterVersionField is the advanced track's version for a cluster the run
// creates. It is validated rather than taken as typed because nothing later
// would catch a bad one: the new-cluster path never passes the cluster
// screen's readiness check, so a release below the floor would be created and
// installed onto without a word. Cleared, it means the default rather than
// "no version", which would hand the choice to GKE's Regular channel, whose
// own default is below the floor.
func clusterVersionField(current string) field {
	f := newField(clusterVersionLabel, current, state.DefaultClusterVersion, func(s *state.Setup, v string) {
		if v == "" {
			v = state.DefaultClusterVersion
		}
		s.ClusterVersion = v
	})
	f.check = clusterVersionProblem
	return f
}

// clusterVersionProblem rejects a version below the supported floor. An empty
// value is the default, and an unreadable one is left for GKE to judge: it
// knows aliases like "latest" that this check does not, and it rejects a
// version that does not exist with a clearer error than ours.
func clusterVersionProblem(v string) string {
	if v == "" || (gcp.Cluster{MasterVersion: v}).SupportedRelease() {
		return ""
	}
	return fmt.Sprintf("Cluster version %s is below %s, the oldest release Substrate is supported on.", v, gcp.MinSupportedRelease)
}

// Labels of the new-cluster fields, which submit reads back to ask GKE
// whether the channel carries the version.
const (
	clusterVersionLabel = "Cluster version (new clusters)"
	releaseChannelLabel = "Release channel (new clusters)"
)

// releaseChannelField is the advanced track's channel for a cluster the run
// creates. Cleared, it means the default, as the version field does: an unset
// channel lands the cluster on Regular, which does not carry the default
// version.
func releaseChannelField(current string) field {
	f := newField(releaseChannelLabel, current, state.DefaultReleaseChannel, func(s *state.Setup, v string) {
		if v == "" {
			v = state.DefaultReleaseChannel
		}
		s.ReleaseChannel = strings.ToLower(v)
	})
	f.check = releaseChannelProblem
	return f
}

// releaseChannelProblem rejects what can never work, and leaves the rest to
// GKE's own list of what each channel carries (see channelProblem). Extended
// is refused outright: it does not allow beta APIs, and setup-gcp asks for
// them on every cluster it creates, so it refuses extended itself. "none"
// is not offered either; GKE no longer lets new accounts create a cluster
// outside a channel.
func releaseChannelProblem(v string) string {
	switch strings.ToLower(v) {
	case "", "rapid", "regular", "stable":
		return ""
	case "extended":
		return "The extended channel does not allow the beta APIs setup-gcp turns on for every cluster. Use rapid, regular, or stable."
	}
	return fmt.Sprintf("Release channel %q is not one of rapid, regular, or stable.", v)
}

// channelProblem asks GKE whether the channel carries the version in this
// location, and says so if it does not. Asked rather than worked out, because
// it changes every few weeks: 1.37 is in Rapid alone today and will reach
// Regular in a couple of months. If GKE cannot be asked, nothing is reported;
// the create itself will fail fast with GKE's own message. versionWrong
// reports that no channel offers the version, so the version is the field
// to change rather than the channel.
func channelProblem(ctx context.Context, gc *gcp.Client, project, location, version, channel string) (problem string, versionWrong bool) {
	if version == "" || channel == "" || version == "latest" {
		return "", false
	}
	carried, err := gc.ChannelVersions(ctx, project, location)
	if err != nil {
		// Silent, except when the lookup ran out the submit's deadline:
		// then it says so like every other check (#59), and submitting again
		// goes on as for any other channel warning.
		if timedOut := deadlineHit(ctx, err, "asking GKE which channels offer "+version); timedOut != err {
			return timedOut.Error() + ".", false
		}
		return "", false
	}
	if gcp.ChannelCarries(carried[channel], version) {
		return "", false
	}
	var with []string
	for _, ch := range []string{"rapid", "regular", "stable"} {
		if gcp.ChannelCarries(carried[ch], version) {
			with = append(with, ch)
		}
	}
	if len(with) == 0 {
		return fmt.Sprintf("GKE has no channel that offers version %s in %s.", version, location), true
	}
	return fmt.Sprintf("The %s channel does not offer version %s in %s; %s does.", channel, version, location, strings.Join(with, " and ")), false
}

// fieldValue returns the trimmed value of the field labeled label, or "" when
// the form has no such field (the quickstart track).
func (s *projectScreen) fieldValue(label string) (string, int) {
	for i, f := range s.fields {
		if f.label == label {
			return strings.TrimSpace(f.input.Value()), i
		}
	}
	return "", -1
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

// CapturesText is false while the enable, the billing check after a
// failed one, or a submit's checks run: no field takes input then, and the
// app's own keys ([v] log, / commands, ? help) must get through.
func (s *projectScreen) CapturesText() bool {
	return s.enabling == nil && !s.checkingEnable && !s.validating
}

func (s *projectScreen) Hints() []Hint {
	if s.enabling != nil {
		return []Hint{{"esc", "stop waiting"}}
	}
	// The bar only advertises keys Update acts on: during the billing check
	// or a submit's checks that is esc alone.
	if s.checkingEnable || s.validating {
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
	for i, f := range s.fields {
		if f.check == nil {
			continue
		}
		if problem := f.check(strings.TrimSpace(f.input.Value())); problem != "" {
			s.errText = problem
			return s.setFocus(i)
		}
	}
	if s.repositoryField >= 0 {
		name := strings.TrimSpace(s.fields[s.repositoryField].input.Value())
		if err := state.ValidateRepositoryName(name); err != nil {
			message := err.Error()
			s.errText = strings.ToUpper(message[:1]) + message[1:] + "."
			return s.setFocus(s.repositoryField)
		}
	}
	s.errText = ""
	s.enableFor = ""
	s.validating = true
	acked := s.permAcked
	st := s.inputSetup(pid)
	var repositoryPermissions []gcp.RequiredPermission
	if st.CreatesArtifactRepository() {
		repositoryPermissions = gcp.RepositoryPermissions
	}
	registry := s.dockerRegistry(&st)
	s.checkingDocker = registry != ""
	// Only the advanced track can change the version or channel, so only it
	// pays for asking GKE whether they go together. Empty fields mean the
	// defaults, which is what is checked.
	zone, _ := s.fieldValue("Cluster location (zone)")
	version, versionAt := s.fieldValue(clusterVersionLabel)
	channel, _ := s.fieldValue(releaseChannelLabel)
	if version == "" {
		version = state.DefaultClusterVersion
	}
	if channel == "" {
		channel = state.DefaultReleaseChannel
	}
	channel = strings.ToLower(channel)
	pair := version + "/" + channel
	checkChannel := versionAt >= 0 && s.channelAcked != pair
	// One deadline for the whole submit, cancellable with esc: with the
	// network down, each call below would otherwise wait its own
	// cmdTimeout in turn while every key is swallowed.
	s.validateSeq++
	seq := s.validateSeq
	ctx, cancel := context.WithTimeout(context.Background(), submitTimeout)
	s.validateCancel = cancel
	projectNumber := s.projectNumber
	return func() tea.Msg {
		defer cancel()
		msg := projValidMsg{owner: s, seq: seq, channelPair: pair}
		// Each submit re-reads ADC: after a PERMISSION_DENIED panel the
		// user may have run `gcloud auth application-default login` as
		// someone else. The probes in this submit still share one fetch.
		// This runs here in the Cmd, never on the Update goroutine.
		s.deps.GCP.ResetToken()
		// The token does not depend on the project: fetch it while
		// `projects describe` runs, so the probes below find it cached
		// instead of paying a second cold gcloud spawn after it.
		go s.deps.GCP.WarmToken(context.Background())
		msg.number, msg.err = projectNumber(ctx, pid)
		msg.err = deadlineHit(ctx, msg.err, "looking up project "+pid)
		// The cluster step lists clusters next, and that fails outright on
		// a project without billing or without the GKE API. Catch both
		// here, where the fix is still one command away.
		if msg.err == nil {
			msg.billingOff, msg.apiOff, msg.probeErr = projectServing(ctx, s.deps.GCP, pid)
			msg.probeErr = deadlineHit(ctx, msg.probeErr, "")
		}
		// No billing or no API blocks the step, and Update then shows
		// that alone, so the checks below would be paid for and thrown
		// away, on every retry.
		blocked := msg.err != nil || msg.billingOff || msg.apiOff
		// Under the same deadline and esc as the rest, and skipped when the
		// project is blocked: get-server-config would fail there too.
		if !blocked && checkChannel {
			msg.channel, msg.versionWrong = channelProblem(ctx, s.deps.GCP, pid, zone, version, channel)
		}
		// Check the bootstrap permissions now rather than failing three
		// screens later, mid-provision. Skipped once acknowledged.
		if !blocked && !acked {
			msg.missing, msg.permErr = s.deps.GCP.MissingPermissions(ctx, pid, repositoryPermissions...)
			msg.permErr = deadlineHit(ctx, msg.permErr, "")
		}
		// Not once the deadline has passed (or esc has cancelled): every
		// docker check would fail at once on the dead context, and Update
		// shows a docker failure ahead of probeErr/permErr, hiding the
		// "timed out" that is the real story.
		if !blocked && registry != "" && ctx.Err() == nil {
			for _, c := range doctor.DockerChecks(registry) {
				if res := c.Run(ctx); res.Status == doctor.Fail {
					msg.docker = append(msg.docker, failedCheck{c.Name, res})
				}
			}
		}
		return msg
	}
}

// deadlineHit says so when err came from the submit's own deadline, so a
// check cut off by submitTimeout reads as that rather than as an API fault
// ("…: context deadline exceeded"). doing names the step when the panel
// showing err does not already (the project lookup's raw error has no
// context of its own); "" leaves just the time. Any other err is returned
// unchanged.
func deadlineHit(ctx context.Context, err error, doing string) error {
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	if doing == "" {
		return fmt.Errorf("timed out after %v: %w", submitTimeout, err)
	}
	return fmt.Errorf("timed out after %v %s: %w", submitTimeout, doing, err)
}

// abandonValidation stops waiting for a submit's checks (esc): they are
// cancelled, their late result is dropped, and the fields are live again.
// esc again then goes back, as usual.
func (s *projectScreen) abandonValidation() {
	if s.validateCancel != nil {
		s.validateCancel()
		s.validateCancel = nil
	}
	s.validateSeq++
	s.validating, s.checkingDocker = false, false
	pid := strings.TrimSpace(s.fields[0].input.Value())
	s.errText = fmt.Sprintf("Stopped validating %s. Press [enter] to check again, or [esc] to go back.", pid)
}

// inputSetup reads the form without committing it before validation succeeds.
func (s *projectScreen) inputSetup(pid string) state.Setup {
	st := *s.deps.Setup
	for _, f := range s.fields {
		f.set(&st, strings.TrimSpace(f.input.Value()))
	}
	st.ProjectID = pid
	return st
}

// dockerRegistry selects checks for a known build destination. The default AR
// destination waits for cluster selection, which may change its region.
func (s *projectScreen) dockerRegistry(st *state.Setup) string {
	if s.deps.DryRun || s.deps.Builder == nil {
		return ""
	}
	if !s.deps.Builder.BuildsWithDocker(st) {
		return ""
	}
	if st.CreatesArtifactRepository() {
		return ""
	}
	return st.BuildRepository()
}

// dockerProblem renders the docker checks that failed. Unlike a permission
// problem it cannot be waved through: nothing outside this machine is going
// to fix it, and the build it blocks runs only after the control plane is
// applied.
func dockerProblem(failed []failedCheck, retryKey string) string {
	var b strings.Builder
	b.WriteString("This substrate builds its envoy-dataplane image with docker buildx, and docker is not ready:\n")
	for _, f := range failed {
		fmt.Fprintf(&b, "  %s: %s\n", f.name, f.res.Detail)
		if f.res.Fix != "" {
			fmt.Fprintf(&b, "    fix: %s\n", f.res.Fix)
		}
	}
	fmt.Fprintf(&b, "Fix them, then press [%s] to check again. Or set ATE_ATENET_DATAPLANE=agentgateway before starting the installer, which builds nothing with docker.", retryKey)
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
// billingOn is set when the billing API has confirmed billing is enabled;
// the text is then never read as a billing failure, so a line such as
// "PERMISSION_DENIED: … billing …" is the permission failure it says it is.
func enableFailureKind(cause string, billingOn bool) enableFailure {
	lower := strings.ToLower(cause)
	switch {
	case !billingOn && strings.Contains(lower, "billing"):
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
	s.checkSeq++
	ctx, cancel := context.WithTimeout(context.Background(), enableCheckTimeout)
	s.checkCancel = cancel
	pending := enableCheckedMsg{owner: s, seq: s.checkSeq, projectID: pid, cause: cause}
	s.checkPending = pending
	check := s.billingCheck
	return func() tea.Msg {
		defer cancel()
		m := pending
		m.billingOn, m.err = check(ctx, pid)
		return m
	}
}

// abandonCheck stops waiting for the billing check (esc): the failure is
// shown at once from gcloud's text alone, as when the probe cannot answer,
// and the check's late result is dropped.
func (s *projectScreen) abandonCheck() {
	m := s.checkPending
	m.err = context.Canceled
	s.enableChecked(m)
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
	if s.checkCancel != nil {
		s.checkCancel()
		s.checkCancel = nil
	}
	if m.err == nil && !m.billingOn {
		s.enableFor = ""
		s.errText = billingProblem(m.projectID, true)
		return
	}
	kind := enableFailureKind(m.cause, m.err == nil)
	s.errText = enableProblem(m.projectID, m.cause, kind)
	if kind == enableFailedBilling {
		// Only when the probe could not answer. The billing panel says
		// to link an account and press enter: another enable would
		// fail the same way.
		s.enableFor = ""
	}
}

// Stop cancels an enable, the billing check after one, or a submit's
// checks, still running when the wizard leaves the screen.
func (s *projectScreen) Stop() {
	if s.enabling != nil {
		s.enabling.stop()
	}
	if s.checkCancel != nil {
		s.checkCancel()
	}
	if s.validateCancel != nil {
		s.validateCancel()
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
		// A result for a check that was abandoned (esc) comes too late.
		if m.owner == s && s.checkingEnable && m.seq == s.checkSeq {
			s.enableChecked(m)
		}
		return nil

	case projValidMsg:
		// A result for a submit that was abandoned (esc) or superseded
		// must not advance the wizard or replace the panel.
		if m.owner != s || m.seq != s.validateSeq {
			return nil
		}
		if s.validateCancel != nil {
			s.validateCancel()
			s.validateCancel = nil
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
			s.errText = dockerProblem(m.docker, "enter")
			return nil
		}
		if m.probeErr != nil && !s.probeAcked {
			s.probeAcked = true
			s.errText = probeProblem(pid, m.probeErr)
			return nil
		}
		if m.channel != "" {
			s.channelAcked = m.channelPair
			s.errText = m.channel + " That only matters if the next step creates a cluster; submit again to continue anyway."
			label := releaseChannelLabel
			if m.versionWrong {
				label = clusterVersionLabel
			}
			_, at := s.fieldValue(label)
			return s.setFocus(at)
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
		return goNext

	case tea.KeyMsg:
		if s.checkingEnable {
			// esc stops waiting for the billing check and shows the
			// failure from gcloud's text; nothing else is live.
			if m.String() == "esc" {
				s.abandonCheck()
			}
			return nil
		}
		if s.validating {
			// esc stops waiting for the checks; a second esc goes back.
			if m.String() == "esc" {
				s.abandonValidation()
			}
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
		s.channelAcked = ""
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
		// Below the floor 'y' leads somewhere nobody supports, and often to a
		// bootstrap that asks GKE for an API that release has never had and
		// stops there. Keep the key — a canned fixture or a misread version
		// should not trap anyone — but stop advertising it as a way forward.
		if s.cursor < len(s.clusters) && !s.clusters[s.cursor].SupportedRelease() {
			return []Hint{{"y", "use it anyway (unsupported)"}, {"esc", "choose another"}}
		}
		return []Hint{{"y", "enable and continue"}, {"esc", "choose another"}}
	}
	return []Hint{{"↑/↓", "select"}, {"enter", "confirm"}, {"r", "reload"}, {"b", "back"}}
}

// selected is the cluster under the cursor, and false on the create-new row.
func (s *clusterScreen) selected() (gcp.Cluster, bool) {
	if s.cursor < 0 || s.cursor >= len(s.clusters) {
		return gcp.Cluster{}, false
	}
	return s.clusters[s.cursor], true
}

// choose is the one place a selection reaches Setup. Everything before it —
// probing included — works off the gcp.Cluster alone, so an aborted
// selection leaves no zone, name, or derived bucket behind to leak into the
// create-new path or a later pick.
func (s *clusterScreen) choose(c gcp.Cluster) tea.Cmd {
	st := s.deps.Setup
	st.SelectCluster(c)
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
		// Every mode but the list and the name prompt is about one cluster,
		// read through s.cursor, and a reload can land in any of them (two
		// [r]s in flight). Which reloads are allowed to change the screen
		// depends on what it is doing with that cluster.
		if s.mode != "list" && s.mode != "name" {
			// A probe or teardown is running against the selection, and its
			// completion reads s.clusters[s.cursor]. Swapping the list under
			// it would act on the wrong cluster, or index past the end; and
			// a failed reload is no reason to stop an ate-setup delete
			// mid-flight. The list stays as it was; [r] after it finishes
			// picks up anything new.
			if s.mode == "probing" || s.mode == "teardown" || m.err != nil {
				return nil
			}
			prev, hadSelection := s.selected()
			s.clusters = m.clusters
			s.cursor = len(s.clusters)
			kept := false
			if hadSelection {
				for i, c := range s.clusters {
					if c.Name == prev.Name && c.Location == prev.Location {
						s.cursor, kept = i, true
						break
					}
				}
			}
			switch {
			case !kept:
				s.mode = "list"
			case s.mode == "confirm" && s.clusters[s.cursor].SubstrateReady():
				// The APIs were turned on out of band between the reloads.
				// The confirmation is about a problem the cluster no longer
				// has, and 'y' through it would record nothing to enable.
				// Back to the list, where the row now badges ready.
				s.mode = "list"
			}
			return s.bgProbes()
		}
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
				st.NewCluster(name)
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
			// Not while the list failed to load: the create row would be
			// the only one left, the name prompt is hidden behind the error
			// panel, and its prefilled name may be a cluster that exists —
			// one the list would have caught. [r] reloads.
			if s.loading || s.err != nil {
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
		// "substrate-ready" is capability (the beta PodCertificate APIs), not
		// install state: what the probe learned about an actual install is its
		// own badge, so a teardown visibly clears it while readiness rightly
		// stays. Name the missing capability rather than only saying something
		// is — the confirm panel then explains how to get it.
		badge := theme.Good.Render(theme.GlyphDone + " substrate-ready")
		switch {
		case c.SubstrateReady():
		case !c.SupportedRelease():
			// Distinct from "missing" because the remedy differs: no
			// enablement helps this one until the control plane moves.
			badge = theme.Bad.Render(theme.GlyphFail + " needs " + gcp.MinSupportedRelease.String() + "+")
		default:
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
		// The confirmation needs every row the terminal has: at 80×24 the
		// full list leaves it a handful, and the remedy was being clamped
		// away. While it is open the list shrinks to the row it is about.
		if s.mode == "confirm" && i != s.cursor {
			continue
		}
		row := fmt.Sprintf("[%d] %-24s %-14s %-18s %2d nodes  %s", i+1, c.Name, c.Location, c.MasterVersion, c.NodeCount, badge)
		if i == s.cursor {
			b.WriteString(theme.Selected.Render(" "+row+" ") + "\n")
		} else {
			b.WriteString(theme.Subtle.Render("  "+row) + "\n")
		}
	}
	createRow := fmt.Sprintf("[%d] ＋ Create a new cluster (recommended)", len(s.clusters)+1)
	switch {
	case s.mode == "confirm":
	case s.cursor == len(s.clusters):
		b.WriteString(theme.Selected.Render(" "+createRow+" ") + "\n")
	default:
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
		sel := s.clusters[s.cursor]
		// Two outcomes, and the release picks which: below the floor nothing
		// that can be enabled helps; above it provision turns the APIs on in
		// place, and the pools below 1.37 that made them necessary have to be
		// replaced afterward. (A cluster with no such pool serves the APIs as
		// GA and never reaches this panel.)
		//
		// Written as paragraphs for the panel to wrap, with no token longer
		// than the narrowest panel (about 40 columns at 80×24, beside the
		// sidebar): hard-wrapped lines and full API names broke mid-word
		// there, and the panel was taller than the window. The keys are not
		// repeated here; the bottom bar always shows them, outside the clamp.
		// The replacement commands are not here either — they are needed
		// after provision, so that is where provision shows them.
		version := s.deps.Setup.ClusterVersion
		var title string
		var paras []string
		switch {
		case !sel.SupportedRelease():
			title = sel.Name + " is below " + gcp.MinSupportedRelease.String() + ", the oldest release Substrate supports."
			lede := "Its control plane runs " + sel.MasterVersion + "."
			// Only worth saying where it is true. From 1.35 GKE accepts the
			// enablement happily; such a cluster is unsupported, not broken,
			// and quoting an error it would never return sends the user
			// looking for a problem that is not there.
			if !sel.BetaAPIsAvailable() {
				lede += " GKE will not even enable the beta PodCertificate APIs there."
			}
			paras = []string{
				lede,
				"Upgrade its control plane to " + gcp.MinSupportedRelease.String() + " or newer and come back, or [esc] and create a new cluster, made at " + version + " with the APIs on.",
			}
		default:
			title = sel.Name + ": the beta PodCertificate APIs are off."
			paras = []string{
				"[y] has provision turn them on in place, a control-plane update of about ten minutes.",
			}
			// Usually some pool is below 1.37 here, or the cluster would serve
			// the APIs as GA and be ready. The exception is a control plane
			// whose version could not be read over pools that are all fine:
			// not ready, since the control plane might not serve v1, but
			// there is no pool to replace and none is recorded, so the panel
			// must not promise provision will list any.
			switch r := sel.PoolReplacement(); {
			case len(r.Names) > 0:
				phrase := "1 node pool runs"
				if len(r.Names) > 1 {
					phrase = fmt.Sprintf("%d node pools run", len(r.Names))
				}
				paras = append(paras, fmt.Sprintf("%s below %s and must be replaced before Substrate starts; provision shows how.", phrase, gcp.PodCertificateGARelease))
			case !r.Needed:
				paras = append(paras, "Its node pools all run "+gcp.PodCertificateGARelease.String()+" or newer and need nothing; only the control plane's version ("+sel.MasterVersion+") could not be read.")
			default:
				paras = append(paras, "Its nodes run below "+gcp.PodCertificateGARelease.String()+", so its node pools must be replaced before Substrate starts; provision shows how.")
			}
			paras = append(paras, "Or [esc] and create a new cluster, made at "+version+" with the APIs on.")
		}
		// No spacer line above it: at 80×24 that line is the panel's bottom
		// border.
		b.WriteString(theme.ErrorPanel.Width(min(w-4, 78)).Render(
			theme.Warning.Render(title) + "\n\n" + strings.Join(paras, "\n\n")))
	default:
		// Wrapped to the content width rather than broken by hand: at 80
		// columns the hand-broken lines ran past the edge and were cut.
		b.WriteString("\n" + theme.Subtle.Width(max(w-2, 20)).Render(
			"Substrate needs "+gcp.MinSupportedRelease.String()+" or newer. Below "+gcp.PodCertificateGARelease.String()+", in the control plane or any node pool, it also needs the beta PodCertificate APIs, "+
				"which GKE serves only for clusters that opted in; provision turns them on for a cluster without them, "+
				"and its pools below "+gcp.PodCertificateGARelease.String()+" must then be replaced. New clusters are created at "+s.deps.Setup.ClusterVersion+"."))
	}
	return b.String()
}
