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

// Package snapshot resolves the pinned agent-substrate/substrate checkout and
// builds the command invocations the wizard executes against it.
//
// The tree is fetched on demand rather than vendored into this repo, the same
// way DeployFilestoreCSI fetches the Filestore CSI driver overlay: a shallow
// git fetch of exactly the revision needed. Unlike that clone, this one is
// pinned to a commit and cached, because several wizard steps run against it.
package snapshot

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gkeoverlay"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

const (
	// ModulePath is the upstream Substrate Go module, for commands that
	// install from it directly rather than from a checkout.
	ModulePath = "github.com/agent-substrate/substrate"

	// RepoURL is the upstream Substrate repository. It is public, so the
	// fetch needs no credentials.
	RepoURL = "https://" + ModulePath + ".git"

	// Commit is the revision the installer starts at, and a default rather
	// than a floor: the images step can send the run to any commit in any
	// repository, and UseSource moves the builder off this one when it does.
	// What still holds either way is that there is a checkout — a pre-built
	// install skips the build, not the tree, because ate-setup reads the
	// manifests from source.
	//
	// The images step offers this on both tracks: as the revision a build
	// from source starts at, and as the manifest revision behind pre-built
	// images, which have to have been built from it.
	//
	// It is a release commit rather than a commit of main: the commit
	// upstream's v1.0.0-rc.1 tag names, the first release with published
	// images (ghcr.io/agent-substrate/substrate).
	//
	// Bump this to move to a newer Substrate, and update MinGoVersion to
	// match the `go` directive in that revision's go.mod.
	Commit = "f86fde9dfb5424c8d16f613de80469bdbef06541"

	// MinGoVersion mirrors the `go` directive in go.mod at Commit. The doctor
	// prefers the real go.mod once the tree is on disk and falls back to this
	// when checking Go before the first fetch.
	MinGoVersion = "1.27.0"
)

// There is no default image registry yet. Upstream publishes Commit's images
// to ghcr.io/agent-substrate/substrate at v1.0.0-rc.1, but the images step
// does not offer them, so a pre-built install names the registry and tag of
// images published for Commit, or for whatever commit it gives the images
// step. A registry is read-only to an install: images are pulled from it and
// never pushed to it, which is why installing pre-built images leaves
// KO_DOCKER_REPO out of the picture.

// TransferGSAName is the snapshot transfer service account's name: what
// setup-gcp creates (its DefaultTransferGSAName) and what ate-setup is given
// as SNAPSHOT_TRANSFER_GSA_NAME. ate-setup has no default for it, and from
// Substrate v0.6.0 refuses to deploy ate-api-server on GKE without it.
const TransferGSAName = "ate-snapshot-transfer"

// SnapshotLocation is ate-api-server's cluster-wide snapshot location: the
// root of the install's snapshot bucket, as upstream's
// hack/ate-dev-env.sh.example sets it. From Substrate v0.6.0, ate-setup
// refuses to deploy without one unless the PerTemplateSnapshotLocation
// preview gate is on (agent-substrate/substrate#2544); older trees ignore
// it. A caller's SNAPSHOT_LOCATION is not honored, for the same reason
// BUCKET_NAME is not: setup-gcp grants the transfer account access to this
// bucket alone, so any other location would fail at the first suspend.
func SnapshotLocation(st *state.Setup) string {
	return "gs://" + st.BucketName + "/"
}

// ShortCommit is Commit abbreviated for display.
func ShortCommit() string { return shorten(Commit) }

// shorten abbreviates a commit SHA for display and for cache directory names.
func shorten(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Root returns the directory holding the Substrate tree. An explicit path
// wins and must already be a checkout; otherwise the pinned tree lives in a
// per-commit directory under the user cache, so bumping Commit fetches into a
// fresh directory instead of mutating the old one.
//
// The returned path need not exist yet — the install steps fetch it on first
// use. Managed reports whether the installer owns that fetch.
func Root(explicit string) (path string, managed bool, err error) {
	if explicit != "" {
		if !isCheckout(explicit) {
			return "", false, fmt.Errorf("%s does not look like a substrate checkout (no go.mod)", explicit)
		}
		abs, err := filepath.Abs(explicit)
		return abs, false, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", false, fmt.Errorf("cannot locate a cache directory for the substrate checkout: %w", err)
	}
	return filepath.Join(cache, "substrate-gke", treePrefix+ShortCommit()), true, nil
}

func isCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// excludeMarker keeps CompleteMarker out of git's view of the tree: Go
// stamps a binary built from a tree with untracked files as dirty, and the
// marker would otherwise mark every image the installer builds that way.
func excludeMarker(dir string) string {
	return fmt.Sprintf(`echo %s >> %s/.git/info/exclude`, CompleteMarker, dir)
}

// CompleteMarker is written at the top of a managed checkout once its fetch
// has finished. Presence of the marker — not of go.mod — is what makes the
// cache trustworthy: git checks files out in path order, so an interrupted
// checkout can leave go.mod on disk without internal/ or vendor/, and keying
// the cache on go.mod would treat that half-tree as good forever.
const CompleteMarker = ".substrate-gke-complete"

// Fetched reports whether the tree at root is ready to build from. A managed
// checkout must carry its completion marker; a user-supplied one only has to
// look like a checkout, since the installer never writes to it.
func Fetched(root string, managed bool) bool {
	if !managed {
		return isCheckout(root)
	}
	_, err := os.Stat(filepath.Join(root, CompleteMarker))
	return err == nil
}

// treePrefix names the per-commit cache directories, and is what Cleanup uses
// to recognise trees this installer created. stageInfix joins a tree name to
// the mktemp suffix of the staging directories its fetch builds in, and
// lockSuffix names the flock file that marks a tree (and its stages) as
// belonging to a live run.
const (
	treePrefix = "substrate-"
	stageInfix = ".partial."
	lockSuffix = ".lock"
)

// Lock marks the managed tree as in use for the rest of the process, so a
// concurrent installer's Cleanup leaves it — and any stage its fetch is still
// building — alone. It also reclaims staging directories orphaned by a fetch
// that was killed outright: SIGKILL runs no traps, and nothing else ever
// learns a mktemp name, so without this sweep those directories would only go
// once some install on the machine succeeded.
//
// The lock is advisory and best-effort. On failure the run is merely as
// unprotected as it was before locks existed, which is not worth refusing to
// install over.
func (b *Builder) Lock() {
	if !b.Managed || b.lock != nil {
		return
	}
	base := filepath.Dir(b.Root)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return
	}
	// Sweep before taking our own lock: a stage orphaned by a killed run of
	// this same pin is guarded by the very lock name we are about to hold.
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), treePrefix) && strings.Contains(e.Name(), stageInfix) {
				removeUnlessLive(base, e.Name())
			}
		}
	}
	if f, err := sharedLock(b.Root + lockSuffix); err == nil {
		b.lock = f
	}
}

// Cleanup removes every tree this installer fetched, once an install has
// succeeded. A managed checkout is scratch space for one install, not a place
// to work: substrate development belongs in the developer's own clone, and a
// copy nobody is expected to edit is cheaper to re-fetch — a few seconds —
// than to keep around and reason about as a cache.
//
// Deleting is deferred to a successful finish rather than done at fetch time.
// While the install can still be retried, that tree is exactly what a retry
// needs; once it has worked, nothing will ask for it again.
//
// It never touches a user-supplied checkout — that tree, and its history,
// belong to the user — and it skips anything a concurrent installer run still
// holds a lock on: the first run to finish must not pull the tree out from
// under the second.
func (b *Builder) Cleanup() error {
	if !b.Managed {
		return nil
	}
	// Our own shared lock guards our tree from other runs' cleanups; held any
	// longer it would guard it from this one too.
	if b.lock != nil {
		b.lock.Close()
		b.lock = nil
	}
	base := filepath.Dir(b.Root)
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil // nothing was ever fetched
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		// Only ever remove what we would have created ourselves: this pin,
		// trees from earlier ones, and the staging directories a killed
		// fetch leaves behind.
		if !strings.HasPrefix(e.Name(), treePrefix) || strings.HasSuffix(e.Name(), lockSuffix) {
			continue
		}
		errs = append(errs, removeUnlessLive(base, e.Name()))
	}
	return errors.Join(errs...)
}

// removeUnlessLive deletes one cache entry unless the run that owns it is
// still alive. A tree and the stages fetched for it share one lock name, so
// failing to grab that lock exclusively means a live installer is using the
// tree — or is still staging it — and the entry must survive.
//
// The lock files themselves are left behind: they are empty, and unlinking
// one while another process holds or is acquiring it would split future runs
// across two inodes that cannot see each other's locks.
func removeUnlessLive(base, name string) error {
	tree := name
	if i := strings.Index(name, stageInfix); i >= 0 {
		tree = name[:i]
	}
	lock, err := exclusiveLock(filepath.Join(base, tree+lockSuffix))
	if err != nil {
		return nil // a live run holds it; not ours to reclaim
	}
	if lock != nil {
		defer lock.Close()
	}
	return os.RemoveAll(filepath.Join(base, name))
}

// gitFetchSteps is the recipe that materializes commit from repo inside an
// existing directory, as git argument lists. Fetching the URL directly
// instead of through a named remote keeps it usable in the fetch preamble,
// in the pasteable teardown command and in FetchTree, which all run it so the
// revision can never drift between them.
func gitFetchSteps(repo, commit string) [][]string {
	return [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", repo, commit},
		{"checkout", "-q", "FETCH_HEAD"},
	}
}

// fetchAt renders gitFetchSteps for a shell. dir is spliced into shell text
// as-is, so callers pass an already-quoted word; repo is a URL the user may
// have typed, so it is quoted here.
func fetchAt(dir, repo, commit string) []string {
	var lines []string
	for _, step := range gitFetchSteps(ShellQuote(repo), commit) {
		lines = append(lines, "git -C "+dir+" "+strings.Join(step, " "))
	}
	return lines
}

// inEphemeralTree wraps command in a pasteable subshell that fetches the
// tree into a temporary directory, runs the command inside it, and reclaims
// the directory when the paste finishes — nothing else ever learns the mktemp
// name.
func (b *Builder) inEphemeralTree(command string) string {
	return `(d=$(mktemp -d) && trap 'rm -rf "$d"' EXIT && ` +
		strings.Join(fetchAt(`"$d"`, b.repo, b.commit), " && ") + ` && cd "$d" && ` + command + ")"
}

// TeardownCommand returns a self-contained shell command that deletes the
// control plane. It re-fetches the tree rather than pointing at the managed
// checkout, which Cleanup removes once the install succeeds. A user-supplied
// checkout is still there, so callers pass their own root for that case
// instead.
//
// ate-setup reads its target from the environment, so the command carries the
// same project/cluster answers the install used — pasted into a fresh shell
// weeks later, it must not depend on whatever kubectl context is ambient.
// KUBECTL_CONTEXT is blanked for that reason too: ate-setup would otherwise
// take an exported one over CLUSTER_NAME and delete from that cluster. It
// carries no image flags: deleting removes what the manifests name and never
// resolves an image reference, so a pre-built install tears down identically.
//
// KUBECONFIG names the installer's kubeconfig for the cluster (see
// KubeconfigPath): ate-setup fetches the credentials it needs into it, as it
// did for the install, instead of into the user's ~/.kube/config. Set on the
// command rather than exported, it leaves the pasting shell as it was. A
// delete that succeeds removes the file: the install left it, and nothing
// reads it once the control plane is gone (a later install fetches anew).
func (b *Builder) TeardownCommand(st *state.Setup, root string) string {
	kubeconfig := ShellQuote(KubeconfigPath(st.ProjectID, st.ClusterName, st.Zone))
	env := fmt.Sprintf("KUBECONFIG=%s PROJECT_ID=%s CLUSTER_NAME=%s CLUSTER_LOCATION=%s KUBECTL_CONTEXT= NO_DEV_ENV=1",
		kubeconfig, ShellQuote(st.ProjectID), ShellQuote(st.ClusterName), ShellQuote(st.Zone))
	del := env + " go run ./cmd/ate-setup delete ate-system && rm -f " + kubeconfig
	if root != "" {
		return fmt.Sprintf("(cd %s && %s)", ShellQuote(root), del)
	}
	return b.inEphemeralTree(del)
}

// DeleteAteSystem removes the Substrate control plane from the named cluster
// with upstream's ate-setup, keeping the cluster and its snapshot bucket. The
// wizard offers it from the install guard's blocked panel, so like
// CheckInstalled it takes the cluster by name: the selection is not committed
// to Setup yet. The env rides inside the command like TeardownCommand's, so
// the delete can only hit the cluster it names, never an ambient context.
//
// ate-setup returns while the namespace is still deleting, so the command
// waits for it to actually go: the guard re-probes the moment this finishes,
// and a probe of a half-deleted install would report — and cache — the very
// state the teardown just removed. Last, it removes the installer's
// kubeconfig for the cluster, as TeardownCommand does: the install left it,
// and an install that follows fetches a fresh one.
func (b *Builder) DeleteAteSystem(projectID, cluster, location string) execx.Spec {
	env := fmt.Sprintf("PROJECT_ID=%s CLUSTER_NAME=%s CLUSTER_LOCATION=%s KUBECTL_CONTEXT= NO_DEV_ENV=1",
		ShellQuote(projectID), ShellQuote(cluster), ShellQuote(location))
	lines := kubeconfigLines(projectID, cluster, location)
	lines = append(lines,
		env+" go run ./cmd/ate-setup delete ate-system",
		"kubectl wait --for=delete namespace/ate-system --timeout=180s",
		`rm -f "$KUBECONFIG"`,
	)
	return execx.Spec{
		Label:   "ate-setup delete ate-system",
		Display: "go run ./cmd/ate-setup delete ate-system && kubectl wait --for=delete namespace/ate-system",
		Argv:    b.inTree(strings.Join(lines, "\n")),
		SimLines: append(b.fetchSimLines(),
			"[step]: delete_ate_system",
			`namespace "ate-system" deleted`,
		),
	}
}

// KubectlAteInstall returns the command that installs the kubectl-ate plugin
// at the revision this install used, for machines where the managed checkout
// is already gone. It has to build from a checkout: `go install
// <module>@<commit>` is refused for this module, whose go.mod replaces
// k8s.io/apimachinery with a local third_party path.
func (b *Builder) KubectlAteInstall() string {
	return b.inEphemeralTree("go install ./cmd/kubectl-ate")
}

// NextSteps returns the commands the completion screen's "Next steps" panel
// walks through: the router port-forward every install can use, then the
// counter-demo tour. The exit summary reprints the same commands — the alt
// screen erases the panel the moment the wizard closes.
//
// A managed checkout is removed once the install succeeds, so its steps
// cannot ask the user to run anything inside it — they get the
// self-contained kubectl-ate install instead.
//
// These strings are coupled to Commit: the demo names come from the counter
// demo's registration, the --template flag from kubectl-ate, and the
// ate-target-actor header from atenet's router, which picks the actor from
// that header rather than from the Host. All three have changed upstream
// before, so bumping Commit means re-checking this function. Nothing here is
// executed by the wizard, so no test will catch the drift — the user is the
// one who runs it.
//
// The Envoy router serves only TLS and admits only clients that present an
// allowlisted pod certificate (see RouterNote), so a curl from the user's
// machine is refused. A tree with the counter demo's driver
// (agent-substrate/substrate#3178) runs such a client in the demo namespace,
// and the tour reads its logs; without one, the tour stops at creating the
// actor. lead is "" when there is nothing to run before the demo; it is kept
// for the callers, which print it when set.
func (b *Builder) NextSteps(st *state.Setup) (lead string, demo []string) {
	// The atespace and ActorTemplate the counter demo creates. `ate-setup
	// deploy demo counter`, `ate-setup deploy demo counter-microvm`, and
	// `hack/install-ate.sh --deploy-demo-counter-microvm` all create the
	// atespace, so the tour does not need a `kubectl ate create atespace` step.
	demoAtespace := "ate-demo-counter"
	demoTemplate := "counter"
	if st.MicroVMActive() {
		demoAtespace = "ate-demo-counter-microvm"
		demoTemplate = "counter-microvm"
	}

	// The install never touched the user's kubeconfig, so the kubectl
	// commands below need pointing at the cluster first: the lead is the
	// line that does it.
	lead = KubeconfigExport(st)
	if b.hasCounterDriver() {
		return lead, []string{"kubectl logs -n " + demoAtespace + " -l app=counter-driver -f"}
	}

	installAte := `go install ./cmd/kubectl-ate    # run inside your substrate checkout`
	if b.Managed {
		installAte = b.KubectlAteInstall()
	}
	return lead, []string{installAte, "kubectl ate create actor my-counter-1 -a " + demoAtespace + " --template " + demoTemplate}
}

// CounterDriverDir is the counter demo's driver, an in-cluster client of the
// Envoy router (agent-substrate/substrate#3178).
const CounterDriverDir = "demos/counter/driver"

// hasCounterDriver reports whether the fetched tree has CounterDriverDir. By
// the time next steps are shown, the demo step has fetched the tree.
func (b *Builder) hasCounterDriver() bool {
	info, err := os.Stat(filepath.Join(b.Root, CounterDriverDir))
	return err == nil && info.IsDir()
}

// RouterNote explains how to reach actors through the Envoy router, which
// admits only allowlisted clients.
func (b *Builder) RouterNote(st *state.Setup) string {
	return routerNoteBody(st) + b.RouterAuthURL() + " and the substrate-gke README."
}

// RouterNoteLead is RouterNote up to the link, for a screen that shows the
// link on a line of its own: wrapped into a panel, the URL broke at every
// slash and hyphen and could not be clicked.
func (b *Builder) RouterNoteLead(st *state.Setup) string {
	return routerNoteBody(st) + "the substrate-gke README and upstream's router documentation:"
}

// routerNoteBody is what RouterNote and RouterNoteLead share, up to where
// one names the URL and the other says where to find it.
func routerNoteBody(st *state.Setup) string {
	return "The ingress router admits only clients that present a pod certificate on its allowlist (" +
		IngressAllowlistSummary(st) + "). To call actors from your own workloads, see "
}

// RouterAuthURL is upstream's documentation of ingress client authentication
// at the commit this install runs.
func (b *Builder) RouterAuthURL() string {
	return "https://github.com/agent-substrate/substrate/blob/" + b.commit +
		"/cmd/atenet/internal/router/README.md#ingress-client-authentication-and-authorization"
}

// ShellQuote renders s as a single-quoted POSIX shell word. Go's %q produces a
// double-quoted string, which bash still expands — a `$` or a backtick in the
// path would be interpreted rather than taken literally.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Builder assembles execx.Specs rooted at the Substrate checkout.
type Builder struct {
	Root string
	// Managed is true when Root is the pinned checkout the installer fetches
	// itself, false when the user supplied their own tree.
	Managed bool
	// Version stamps the images ko builds (the checkout is detached at an
	// exact commit, so `git describe` has no tag to report).
	Version string
	// SetupGCP is the absolute path of this repository's setup-gcp module,
	// which Bootstrap runs. See FindSetupGCP.
	SetupGCP string
	// GKEOverlay is the absolute path of this repository's deploy/gke, and
	// Self the installer's own executable, which the deploy and upgrade
	// steps re-run with --add-gke-overlay (or --check-gke-overlay) to add it
	// to the fetched tree. See gkeoverlay.Find. With either empty, as in
	// tests that do not set them, the steps skip that line.
	GKEOverlay, Self string
	// repo and commit are the tree to fetch. They start at the pin and move
	// only when the wizard's images step picks something else.
	repo, commit string
	// envoy records whether the chosen revision builds envoy-dataplane.
	envoy bool
	// lock, while open, is the shared flock marking Root as in use by this
	// process. Taken by Lock, released by Cleanup (or process exit).
	lock *os.File
}

// NewBuilder returns a Builder for the tree at root, fetching the pin.
func NewBuilder(root string, managed bool) *Builder {
	version := "substrate-local"
	if managed {
		version = "substrate-" + ShortCommit()
	}
	return &Builder{Root: root, Managed: managed, Version: version, repo: RepoURL, commit: Commit}
}

// UseSource repoints the builder at the commit the user named in the images
// step: a branch, a tag, or a commit of the upstream repository, which is the
// only one either track reads.
//
// A managed tree is cached per commit, so the root and the ko version stamp
// move with it: two revisions never share a directory, and the images one
// build produces are never mistaken for another's. A tree the user supplied
// with --substrate-root is theirs, and is left exactly where it is.
func (b *Builder) UseSource(rev Revision) {
	b.repo, b.commit, b.envoy = rev.Repo, rev.Commit, rev.EnvoyDataplane
	if !b.Managed {
		return
	}
	// The lock names the tree, so it has to follow the tree. Dropping it for
	// the instant in between is safe: nothing has fetched into the new path
	// yet, so there is nothing there for another run to reclaim.
	relock := b.lock != nil
	if relock {
		b.lock.Close()
		b.lock = nil
	}
	b.Root = filepath.Join(filepath.Dir(b.Root), treePrefix+shorten(rev.Commit))
	b.Version = "substrate-" + shorten(rev.Commit)
	if relock {
		b.Lock()
	}
}

// FetchLabel is the checklist label for the fetch preamble of the in-tree
// steps. It names the revision the run uses, so a run the images step moved
// off the pin does not claim to fetch the pinned checkout. A tree the user
// supplied is used as it is, with nothing fetched.
func (b *Builder) FetchLabel() string {
	switch {
	case !b.Managed:
		return "Use the substrate checkout at " + b.Root
	case b.repo == RepoURL && b.commit == Commit:
		return "Fetch the pinned substrate checkout (" + shorten(b.commit) + ")"
	default:
		return "Fetch the selected substrate checkout (" + shorten(b.commit) + ")"
	}
}

// CleanupCommand preserves the cluster and repository targets used by this
// install. clusterScoped adds --cluster-scoped, the form the exit summary
// leads with (see CleanupLeads).
func (b *Builder) CleanupCommand(st *state.Setup, clusterScoped bool) string {
	command := CleanupCommand(st.ProjectID, st.ClusterName, st.Zone, st.BucketName, clusterScoped)
	if st.CreatesArtifactRepository() {
		if st.RepositoryName() != "ate-images" {
			command += " --repository " + ShellQuote(st.RepositoryName())
		}
		command += " --delete-repository"
	}
	return command
}

// BuildsWithDocker reports whether deploying the control plane runs `docker
// buildx build --push` against st.KoDockerRepo, which ate-setup does for the
// envoy-dataplane image. Every install deploys the envoy router (see
// Dataplane), so that takes a tree that has the image.
//
// ko reads gcloud's credentials by itself; docker reads only its own config.
// So a host where every ko image pushes can still fail here, after the
// control-plane bundle is applied, leaving a half-installed cluster.
//
// A pre-built install builds nothing. ate-setup still builds envoy-dataplane
// there until agent-substrate/substrate#1964, but without a registry to push
// to it cannot succeed with or without docker, so docker is not asked for.
func (b *Builder) BuildsWithDocker(st *state.Setup) bool {
	if st.Prebuilt() {
		return false
	}
	if !b.Managed {
		// A tree the user supplied is built as it stands, whatever revision
		// the images step recorded.
		return HasEnvoyDockerfile(b.Root)
	}
	return b.envoy
}

// Dataplane is the atenet dataplane every install deploys, and the only one
// the GKE installer supports. ate-setup also offers agentgateway through
// ATE_ATENET_DATAPLANE, but it has no support policy for GKE installs, so the
// doctor refuses it (see UnsupportedDataplane) and the control-plane deploy
// names envoy explicitly.
const Dataplane = "envoy"

// UnsupportedDataplane returns the caller's ATE_ATENET_DATAPLANE when it names
// a dataplane other than Dataplane, and "" when it is unset or Dataplane.
func UnsupportedDataplane() string {
	if v := os.Getenv("ATE_ATENET_DATAPLANE"); v != "" && v != Dataplane {
		return v
	}
	return ""
}

// dataplaneEnv passes Dataplane to ate-setup, replacing any value the caller
// exported. The doctor has already refused another value; this keeps a run
// that skipped it on envoy too.
func dataplaneEnv() []string {
	return []string{"ATE_ATENET_DATAPLANE=" + Dataplane}
}

// HasEnvoyDockerfile reports whether the Substrate tree at root builds the
// envoy-dataplane image, which 0.2 and later do.
func HasEnvoyDockerfile(root string) bool {
	_, err := os.Stat(filepath.Join(root, EnvoyDockerfile))
	return err == nil
}

// env builds the environment both upstream tools read, mirroring
// hack/ate-dev-env.sh.example. NO_DEV_ENV keeps ate-setup from sourcing a
// developer's .ate-dev-env.sh out from under the wizard's answers.
//
// KUBECTL_CONTEXT is blanked because every spec inherits the caller's
// environment, and a developer who sourced .ate-dev-env.sh has it exported.
// ate-setup takes a non-empty context as "credentials already configured"
// and skips fetching them for CLUSTER_NAME, so the install would land in
// whatever cluster that context names instead of the one the wizard chose.
// The shell scripts pass it to kubectl/ko as --context, where it names a
// context the installer's KUBECONFIG (see kubeconfigLines) does not have.
// Empty means "use the current context" to both. exec.Cmd keeps the last
// duplicate, so this overrides the inherited value.
//
// NETWORK, SUBNETWORK and ENABLE_DATAPLANE_V2 are sent for every run, and for
// an existing cluster they are its own (see Setup.BootstrapNetwork):
// bootstrap compares an existing cluster's network, subnetwork and datapath
// with them and warns on each difference, and the defaults match few real
// clusters. ENABLE_DATAPLANE_V2 has to be explicit, because bootstrap
// registers no flag for it and inherits create cluster's default of true.
func (b *Builder) env(st *state.Setup) []string {
	network, subnetwork, dataplaneV2 := st.BootstrapNetwork()
	env := []string{
		"PROJECT_ID=" + st.ProjectID,
		"PROJECT_NUMBER=" + st.ProjectNumber,
		"GCE_REGION=" + st.Region(),
		"ARTIFACT_REGISTRY_REPOSITORY=" + st.RepositoryName(),
		"CREATE_ARTIFACT_REPOSITORY=" + fmt.Sprint(st.CreatesArtifactRepository()),
		"CLUSTER_LOCATION=" + st.Zone,
		"CLUSTER_NAME=" + st.ClusterName,
		// Read only when bootstrap creates the cluster, so it is sent for every
		// run and simply ignored by one that connects to an existing cluster —
		// the alternative, omitting it conditionally, would make the
		// environment differ between two runs that provision the same way.
		"CLUSTER_VERSION=" + st.ClusterVersion,
		// The two travel together: GKE serves a version only in the
		// channels that carry it, and an unset channel means Regular.
		"RELEASE_CHANNEL=" + st.ReleaseChannel,
		"NETWORK=" + network,
		"SUBNETWORK=" + subnetwork,
		"ENABLE_DATAPLANE_V2=" + strconv.FormatBool(dataplaneV2),
		"NODE_MACHINE_TYPE=" + st.MachineType,
		"BUCKET_NAME=" + st.BucketName,
		// Bootstrap creates the account under this name and ate-setup hands
		// it to ate-api-server's credential exchange, so both read it from
		// here. Trees that predate the account ignore it.
		"SNAPSHOT_TRANSFER_GSA_NAME=" + TransferGSAName,
		"SNAPSHOT_LOCATION=" + SnapshotLocation(st),
		// Read only when bootstrap creates the cluster; see
		// SeparateSystemNodesLine for how deploy uses the pools.
		"SEPARATE_SYSTEM_NODES=" + strconv.FormatBool(!st.SharedSystemNodes),
		"KUBECTL_CONTEXT=",
		"NO_DEV_ENV=1",
	}
	if st.Prebuilt() {
		// Nothing is built and nothing is pushed, so a KO_DOCKER_REPO would
		// name a registry this install never writes to.
		//
		// VERSION is the image tag, not the checkout's commit. ate-setup
		// falls back to the tag on its own, but VERSION wins over it and
		// every spec inherits the caller's environment, so a developer who
		// sourced an ate-dev-env.sh would otherwise have ate-setup record a
		// version no installed image carries. Pinning it here keeps the
		// record matching the image, and makes an upgrade a change of tag and
		// nothing else.
		return append(env, "VERSION="+imageVersion(st.ImageTag))
	}
	return append(env,
		"KO_DOCKER_REPO="+st.BuildRepository(),
		"KO_DEFAULTPLATFORMS="+targetPlatform,
		"VERSION="+b.Version,
	)
}

const (
	targetArch     = "amd64"
	targetPlatform = "linux/" + targetArch
)

// imageVersion is the Substrate version a pre-built image tag names. A tag may
// carry the digest it resolved to (v0.1.0@sha256:...); the version is the tag
// alone, the same cut ate-setup makes on its own fallback.
func imageVersion(tag string) string {
	v, _, _ := strings.Cut(tag, "@")
	return v
}

// imageTag is the OCI distribution grammar for an image tag.
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// imageTagMaxLength is the longest tag that grammar allows.
const imageTagMaxLength = 128

// CheckImageTag reports whether a pre-built image tag is one a registry can
// hold. The tag doubles as the Substrate version ate-setup records, and a
// malformed one would otherwise fail only once the install is under way.
//
// Checking here turns that failure into a correctable prompt, for the same
// reason the revision is resolved before the install starts rather than during
// it.
func CheckImageTag(tag string) error {
	v := imageVersion(tag)
	if len(v) > imageTagMaxLength {
		return fmt.Errorf("%q is %d characters; an image tag can be at most %d", v, len(v), imageTagMaxLength)
	}
	if !imageTag.MatchString(v) {
		return fmt.Errorf("%q is not an image tag: it must be letters, digits, '_', '.', or '-', and cannot begin with '.' or '-'", v)
	}
	return nil
}

// imageArgs renders the ate-setup flags that select pre-built images, as the
// text to display and the text to run. They are flags rather than
// ATE_IMAGE_REPO/ATE_IMAGE_TAG so the command the wizard shows says which
// images it is installing.
func imageArgs(st *state.Setup) (display, argv string) {
	if !st.Prebuilt() {
		return "", ""
	}
	return fmt.Sprintf(" --image-repo %s --image-tag %s", st.ImageRepo, st.ImageTag),
		fmt.Sprintf(" --image-repo %s --image-tag %s", ShellQuote(st.ImageRepo), ShellQuote(st.ImageTag))
}

// Lines cordonLines prints, one for each outcome it deploys with.
const (
	SeparateSystemNodesLine = "Keeping Substrate's system components on the system node pools"
	SharedSystemNodesLine   = "No system node pools; Substrate's system components share the worker nodes"
)

// externalPostgresCheck is true in the shell when ate-setup will use an
// external database rather than the bundled PostgreSQL: the variables its
// planPostgres reads. The installer's environment reaches ate-setup, so the
// check sees what ate-setup will.
const externalPostgresCheck = `[ -n "${ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING:-}${ATE_API_POSTGRES_CLOUDSQL_INSTANCE:-}" ]`

// cordonLines sets ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS for the deploy:
// true when the cluster has nodes labeled for the system pools (see
// setup-gcp's systempools.go), so ate-setup pins the system components, and
// the bundled PostgreSQL, to them; false otherwise, including for a caller
// who exported it. ate-setup leaves the system components Pending without
// system nodes, and refuses to deploy the bundled PostgreSQL without an
// ate-postgres node, so the ate-postgres pool is required unless the
// database is external. A cluster the user brought has neither, unless they
// added them with the commands bootstrap printed.
//
// The check runs in a child shell so that its exit status can be read under
// set -e; it inherits the KUBECONFIG the step exported (see kubeconfigLines)
// and so reaches the cluster the step installs into. It exits
// systemPoolsAbsent when a pool is missing; any other failure (kubectl, the
// API server) stops the deploy rather than read as "no pools" and quietly
// install the system components onto the worker nodes.
func cordonLines(st *state.Setup) string {
	lines := []string{"export ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS=false"}
	if st.SharedSystemNodes {
		return strings.Join(append(lines, "echo "+ShellQuote(SharedSystemNodesLine)), "\n") + "\n"
	}
	check := []string{
		`system=$(kubectl get nodes -l ate.dev/workloadType=ate-system-infra -o name)`,
		fmt.Sprintf(`[ -n "$system" ] || exit %d`, systemPoolsAbsent),
		externalPostgresCheck + ` && exit 0`,
		`postgres=$(kubectl get nodes -l ate.dev/workloadType=ate-postgres -o name)`,
		fmt.Sprintf(`[ -n "$postgres" ] || exit %d`, systemPoolsAbsent),
	}
	return strings.Join(append(lines,
		"pools=0",
		"bash -euo pipefail -c "+ShellQuote(strings.Join(check, "\n"))+" || pools=$?",
		`case "$pools" in`,
		"  0) export ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS=true; echo "+ShellQuote(SeparateSystemNodesLine)+" ;;",
		fmt.Sprintf("  %d) echo %s ;;", systemPoolsAbsent, ShellQuote(SharedSystemNodesLine)),
		`  *) echo "Could not check the cluster for the system node pools" >&2; exit "$pools" ;;`,
		"esac"), "\n") + "\n"
}

// PlacementLine is what placementLines prints when the API server is where
// cordoning put it.
const PlacementLine = "ate-api-server is running on the ate-system-infra nodes"

// placementLines checks, after a deploy with
// ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS=true, that every ate-api-server
// pod runs on a node labeled ate.dev/workloadType=ate-system-infra, and fails
// the step naming the node when one does not, rather than report a
// separation that did not happen. Terminating pods from an earlier,
// uncordoned rollout are skipped. Like cordonLines' check, it runs in a child
// shell that inherits the step's KUBECONFIG.
func placementLines(st *state.Setup) string {
	if st.SharedSystemNodes {
		return ""
	}
	check := []string{
		`nodes=$(kubectl get pods -n ate-system -l app=ate-api-server -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.spec.nodeName}}{{"\n"}}{{end}}{{end}}')`,
		`[ -n "$nodes" ] || { echo "No scheduled ate-api-server pod to check the placement of" >&2; exit 1; }`,
		`for node in $nodes; do`,
		`  type=$(kubectl get node "$node" -o go-template='{{index .metadata.labels "ate.dev/workloadType"}}')`,
		`  if [ "$type" != ate-system-infra ]; then`,
		`    echo "ate-api-server is running on node $node, which is not labeled ate.dev/workloadType=ate-system-infra" >&2`,
		`    exit 1`,
		`  fi`,
		`done`,
		"echo " + ShellQuote(PlacementLine),
	}
	return "\n" + strings.Join([]string{
		`if [ "$ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS" = true ]; then`,
		"  bash -euo pipefail -c " + ShellQuote(strings.Join(check, "\n")),
		"fi",
	}, "\n")
}

// systemPoolsAbsent is the exit status of cordonLines' check when a system
// pool is missing, kept apart from the 1 that set -e gives any failure.
const systemPoolsAbsent = 3

// FetchLine and CachedLine are the two outcomes the fetch preamble reports.
// The install checklists key their "fetch" phase off both, so they are
// constants rather than literals: rewording an echo without updating the
// matcher would silently stop that checklist row from ever lighting up.
const (
	FetchLine  = "Fetching substrate"
	CachedLine = "Using cached substrate@"
)

// ensure returns the shell lines that guarantee the checkout exists and leave
// the shell inside it. Fetching only the pinned commit at depth 1 keeps this
// to a few seconds; a tree the user supplied is used as-is.
//
// The tree is staged in a sibling directory and renamed into place only once
// it is complete, so interrupting a fetch leaves no half-tree that later runs
// would mistake for a good cache. The stage is a mktemp name rather than a
// fixed ".partial": two wizards running at once would otherwise share one
// staging path, and the interleaving where the second re-inits the stage the
// first is about to mark complete publishes an empty tree that, marker and
// all, no later run would ever distrust.
//
// Paths are single-quoted because the shell expands `$` and backticks inside
// double quotes.
func (b *Builder) ensure() []string {
	if !b.Managed {
		return []string{"cd " + ShellQuote(b.Root)}
	}
	lines := []string{
		"SUBSTRATE_DIR=" + ShellQuote(b.Root),
		fmt.Sprintf(`if [ ! -e "${SUBSTRATE_DIR}/%s" ]; then`, CompleteMarker),
		// Single-quoted whole: the repository URL is user input, and inside
		// double quotes a `$` or backtick in it would be expanded.
		"    echo " + ShellQuote(fmt.Sprintf("%s@%s from %s...", FetchLine, shorten(b.commit), b.repo)),
		`    rm -rf "${SUBSTRATE_DIR}"`,
		`    mkdir -p "$(dirname "${SUBSTRATE_DIR}")"`,
		fmt.Sprintf(`    STAGE=$(mktemp -d "${SUBSTRATE_DIR}%sXXXXXX")`, stageInfix),
		// Nothing else knows this name, so a failure from here on has to
		// clean up after itself. Ctrl-C needs the second trap: bash dies on
		// an untrapped signal without ever running its EXIT handler, and
		// exiting from the handler is what gets us back to that path.
		`    trap 'rm -rf "${STAGE}"' EXIT`,
		`    trap 'exit 130' INT TERM`,
	}
	for _, cmd := range fetchAt(`"${STAGE}"`, b.repo, b.commit) {
		lines = append(lines, "    "+cmd)
	}
	lines = append(lines, "    "+excludeMarker(`"${STAGE}"`))
	return append(lines,
		fmt.Sprintf(`    touch "${STAGE}/%s"`, CompleteMarker),
		// If a concurrent run published first, its tree is as good as ours;
		// renaming onto it would only nest our stage inside it.
		fmt.Sprintf(`    if [ -e "${SUBSTRATE_DIR}/%s" ]; then`, CompleteMarker),
		`        rm -rf "${STAGE}"`,
		`    else`,
		`        mv "${STAGE}" "${SUBSTRATE_DIR}"`,
		// The check above cannot be atomic with the mv: a run that publishes
		// in between turns our rename into a move *into* its tree. mv leaves
		// the stage's own name behind in that case and only in that case, so
		// removing that path mops up the race loser and is a no-op for the
		// winner.
		`        rm -rf "${SUBSTRATE_DIR}/${STAGE##*/}"`,
		`    fi`,
		fmt.Sprintf(`    echo "Fetched substrate@%s"`, shorten(b.commit)),
		`else`,
		fmt.Sprintf(`    echo "%s%s"`, CachedLine, shorten(b.commit)),
		`fi`,
		`cd "${SUBSTRATE_DIR}"`,
	)
}

// inTree wraps a command so it runs inside the Substrate checkout, fetching
// the tree first if it is not there yet.
func (b *Builder) inTree(command string) []string {
	lines := append([]string{"set -euo pipefail"}, b.ensure()...)
	lines = append(lines, command)
	return []string{"bash", "-c", strings.Join(lines, "\n")}
}

// fetchSimLines is what --dry-run replays for the fetch preamble.
func (b *Builder) fetchSimLines() []string {
	if !b.Managed {
		return nil
	}
	return []string{CachedLine + shorten(b.commit)}
}

// Bootstrap provisions GCP resources through this repository's setup-gcp,
// after fetching the selected Substrate checkout.
func (b *Builder) Bootstrap(st *state.Setup) execx.Spec {
	phases := []string{"Enabling required APIs..."}
	if st.CreatesArtifactRepository() {
		phases = append(phases, "Creating Artifact Registry repository...")
	}
	phases = append(phases,
		"Creating the GKE node service account...",
		"Creating GKE Cluster...",
		"Creating GCS buckets for snapshots and micro-VM assets...",
		"Granting GKE Node permissions...",
		"Granting Atelet permissions...",
		"Creating the snapshot transfer service account...",
		"Creating IAM policy bindings for the buckets...",
		"Creating Monitoring Dashboards...",
	)
	lines := b.fetchSimLines()
	for i, phase := range phases {
		lines = append(lines, fmt.Sprintf("Step %d/%d: %s", i+1, len(phases), phase))
	}
	run := "go -C " + ShellQuote(b.SetupGCP) + " run . bootstrap"
	env := b.env(st)
	if st.Prebuilt() {
		// Bootstrap grants or prints the nodes' image pull access for the
		// registry the images come from: KO_DOCKER_REPO for a source build
		// (see env), and this for pre-built images. Only bootstrap gets it;
		// ate-setup takes the registry from --image-repo (see imageArgs).
		env = append(env, "ATE_IMAGE_REPO="+st.ImageRepo)
	}
	return execx.Spec{
		Label:    "setup-gcp bootstrap",
		Display:  run,
		Argv:     b.inTree(run),
		Env:      env,
		SimLines: append(lines, "Bootstrap completed successfully."),
	}
}

// overlayCommand is the shell line that runs this installer on the
// Substrate tree at dir (a shell word, already quoted) to add deploy/gke to
// it when the tree no longer ships upstream's GKE manifests. With add false
// it only reports, for a checkout the user supplied: the installer never
// modifies those. It is empty when the Builder has no GKEOverlay or Self.
//
// TODO: Replace with `--component <GKEOverlay>/components/gke` on the
// ate-setup deploy once upstream ships `ate-setup deploy --component`
// (agent-substrate/substrate PR 3179) or `ate-setup render`
// (agent-substrate/substrate#3240). See gkeoverlay.
func (b *Builder) overlayCommand(dir string, add bool) string {
	if b.GKEOverlay == "" || b.Self == "" {
		return ""
	}
	flag := " --check-gke-overlay "
	if add {
		flag = " --add-gke-overlay "
	}
	return ShellQuote(b.Self) + " --gke-overlay " + ShellQuote(b.GKEOverlay) + flag + dir
}

// withOverlay prefixes command with overlayCommand for the current
// directory, which inTree has made the Substrate tree.
func (b *Builder) withOverlay(command string) string {
	if line := b.overlayCommand(".", b.Managed); line != "" {
		return line + "\n" + command
	}
	return command
}

// DeployAteSystem installs the Substrate control plane with the upstream
// cmd/ate-setup. ate-setup fetches cluster credentials itself from
// PROJECT_ID/CLUSTER_NAME/CLUSTER_LOCATION — into the installer's own
// kubeconfig, which the step exports first (see kubeconfigLines) — and ko
// builds and pushes the control-plane images from the checkout's source.
// Before ate-setup runs, deploy/gke is added to the tree if the tree needs
// it (see overlayCommand).
func (b *Builder) DeployAteSystem(st *state.Setup) execx.Spec {
	display, argv := imageArgs(st)
	sim := append(b.fetchSimLines(),
		gkeoverlay.NotNeededLine,
		"[step]: deploy_ate_system",
		"[step]: deploy_crds",
		"[step]: ensure_apiserver_prerequisites",
		"[step]: create_jwt_authority_pool_secret",
		"[step]: create_actor_id_ca_pool_secret",
		"[step]: create_podcertificate_controller_cas",
		"[step]: create_api_server_env_vars",
		"[step]: create_api_authentication_config",
	)
	if !st.Prebuilt() {
		// ko's build progress is the loudest part of a source install and the
		// only part a pre-built one does not have, so a dry run of one must
		// not print it. Nothing replaces it: pulling a digest is silent.
		sim = append(sim,
			"Building go.ate.dev/substrate/cmd/ate-api-server",
			"Building go.ate.dev/substrate/cmd/atelet",
		)
	}
	return execx.Spec{
		Label:   "ate-setup deploy ate-system",
		Display: "go run ./cmd/ate-setup deploy ate-system" + display,
		Argv: b.inTree(strings.Join(kubeconfigLines(st.ProjectID, st.ClusterName, st.Zone), "\n") + "\n" +
			b.withOverlay(cordonLines(st)+"go run ./cmd/ate-setup deploy ate-system"+argv+placementLines(st))),
		Env: append(b.controlPlaneEnv(st), sandboxClassesEnv(state.SandboxGVisor)),
		SimLines: append(sim,
			"[step]: deploy_k8s_credential_provider",
			"[step]: Waiting for ATE system components to be ready...",
			`deployment "ate-api-server" successfully rolled out`,
			`daemon set "atelet" successfully rolled out`,
		),
	}
}

// controlPlaneEnv is the environment of every ate-setup deploy. Each deploy
// replaces ate-setup's install record, which the upgrade flow reads, so a
// partial deploy must pass the same settings as the full one.
func (b *Builder) controlPlaneEnv(st *state.Setup) []string {
	return slices.Concat(b.env(st), credentialProviderEnv(), ingressAllowlistEnv(st), dataplaneEnv())
}

// sandboxClassesEnv selects the default SandboxConfigs ate-setup deploys.
//
// From agent-substrate/substrate#2092, ate-setup defaults to gvisor,microvm
// and stages the micro-VM assets on every deploy, which needs gcloud and the
// asset bucket even on a cluster that never runs a micro-VM. DeployAteSystem
// passes gvisor alone, and InstallMicroVMDeps adds microvm once the user picks
// it and the KVM check has run. Older trees ignore the variable, as they
// ignore ATE_CREDENTIAL_PROVIDER.
func sandboxClassesEnv(classes ...string) string {
	return "ATE_INSTALL_SANDBOX_CLASSES=" + strings.Join(classes, ",")
}

// defaultCredentialProvider is the egress credential provider the control
// plane is deployed with: the bundled Kubernetes Secrets provider, as in
// upstream's quickstart. `ate-setup deploy ate-system` requires one from
// v0.4.0 on; older trees ignore the variable.
const defaultCredentialProvider = `{"name":"k8s.io"}`

// CredentialProvider is the provider the control plane is deployed with: the
// caller's ATE_CREDENTIAL_PROVIDER, else defaultCredentialProvider.
func CredentialProvider() string {
	if v := os.Getenv("ATE_CREDENTIAL_PROVIDER"); v != "" {
		return v
	}
	return defaultCredentialProvider
}

// credentialProviderEnv passes defaultCredentialProvider, unless the caller
// already exports ATE_CREDENTIAL_PROVIDER.
func credentialProviderEnv() []string {
	if os.Getenv("ATE_CREDENTIAL_PROVIDER") != "" {
		return nil
	}
	return []string{"ATE_CREDENTIAL_PROVIDER=" + defaultCredentialProvider}
}

// DemoClientSPIFFEIDs are the identities of the counter demos' driver pods,
// which call their actors through the ingress router
// (agent-substrate/substrate#3178). The pod identity signer issues
// spiffe://cluster.local/ns/<namespace>/sa/<service-account>, with the trust
// domain fixed; ate-setup fixes the demo namespaces, and the drivers run as
// their namespace's default service account. A demo run lists both, because
// the allowlist is set when the control plane is deployed, before the
// sandbox step decides which demo runs.
var DemoClientSPIFFEIDs = []string{
	"spiffe://cluster.local/ns/ate-demo-counter/sa/default",
	"spiffe://cluster.local/ns/ate-demo-counter-microvm/sa/default",
}

// ParseSPIFFEIDs splits a comma-separated list of SPIFFE IDs, trimming each
// and dropping empty entries, and checks every one is spiffe://<trust
// domain>[/path]. The router matches IDs exactly, so a malformed one would
// only ever refuse its client. At least one is required: from Substrate
// v0.6.0, ate-setup refuses to deploy the Envoy router without an allowlist.
func ParseSPIFFEIDs(value string) ([]string, error) {
	var ids []string
	for _, id := range strings.Split(value, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		u, err := url.Parse(id)
		if err != nil || u.Scheme != "spiffe" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%q is not a SPIFFE ID; want spiffe://<trust domain>/<path>, e.g. spiffe://cluster.local/ns/<namespace>/sa/<service-account>", id)
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("enter at least one client SPIFFE ID: ate-setup will not deploy the ingress router with an empty allowlist")
	}
	return ids, nil
}

// IngressAllowlistSummary describes st's allowlist for the control-plane
// screen and the router note.
func IngressAllowlistSummary(st *state.Setup) string {
	if st.DemoMode {
		return "the counter demos' driver pods"
	}
	if n := len(st.IngressAllowedSPIFFEIDs); n != 1 {
		return fmt.Sprintf("the %d client identities you entered", n)
	}
	return "the client identity you entered"
}

// ingressAllowlistEnv passes st's allowlist to ate-setup, replacing any
// ATE_INGRESS_ALLOWED_SPIFFE_IDS the caller exported: the IngressClients step
// prefilled from it, and what the user confirmed there is what counts.
func ingressAllowlistEnv(st *state.Setup) []string {
	return []string{"ATE_INGRESS_ALLOWED_SPIFFE_IDS=" + strings.Join(st.IngressAllowedSPIFFEIDs, ",")}
}

// craneDigest prints an image reference's registry digest. It must stay in
// step with the go-containerregistry version in the pinned checkout's go.mod,
// so bump it alongside Commit.
//
// crane authenticates with the docker keychain only (~/.docker/config.json and
// its credential helpers), whereas ate-setup's resolver also tries gcloud/ADC
// credentials. The default release repo is anonymous-readable, so this only
// matters for a private mirror: there the micro-VM pre-built demo on a tree
// without counterMicroVMDemoDir fails at `crane digest` with UNAUTHORIZED
// unless `gcloud auth configure-docker HOST` has been run for that registry
// host, even though the gVisor path works.
const craneDigest = "go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 digest"

// counterMicroVMDemoDir is where ate-setup registers its counter-microvm demo.
// Its presence in the checkout means `ate-setup deploy demo counter-microvm`
// exists there: upstream added it in v0.2.0 (agent-substrate/substrate#1785),
// so main and the pinned Commit have it and older trees such as v0.1.0 do not.
const counterMicroVMDemoDir = "cmd/ate-setup/internal/demos/countermicrovm"

// The counter-microvm templates a pre-built micro-VM demo rewrites in place:
// install-ate.sh reads them from these fixed paths, so they cannot be
// rendered to a copy instead.
const (
	demoPoolTemplate  = "demos/counter/counter-microvm.yaml.tmpl"
	demoActorTemplate = "demos/counter/counter-microvm-template.yaml.tmpl"
)

// restoreDemoTemplates defines, then runs, a function that puts back any
// template a previous pre-built run left rewritten, from the .bak copy
// `sed -i.bak` made. It runs before anything else on every micro-VM demo
// path, ate-setup's included, since it reads the same templates: a run that
// was SIGKILLed never reached its EXIT trap, and its leftover
// ${REF}@${DIGEST} would otherwise make the next sed a no-op and silently
// deploy the old digest — or, on a source build, a pre-built one.
var restoreDemoTemplates = []string{
	`restore_demo_templates() {`,
	`  for f in ` + demoPoolTemplate + ` ` + demoActorTemplate + `; do`,
	`    if [ -f "${f}.bak" ]; then mv -f "${f}.bak" "${f}"; fi`,
	`  done`,
	`}`,
	`restore_demo_templates`,
}

// DeployDemo deploys one of the upstream demo applications (for the wizard,
// the counter demo). When the Micro-VM sandbox runtime is selected and staged,
// it runs `ate-setup deploy demo counter-microvm` if the checkout has it (see
// counterMicroVMDemoDir), exactly as the gVisor demo runs ate-setup. Older
// trees, such as v0.1.0, fall back to hack/install-ate.sh
// --deploy-demo-counter-microvm (the demo step from hack/run-microvm-demo.sh),
// which those trees keep for good.
//
// TODO: Drop the install-ate.sh fallback (deprecated upstream), the template
// rewrite, and the jq/make entries in doctor.MicroVMTools. Commit is past
// v0.2.0, so they only serve older commits typed into the images step.
func (b *Builder) DeployDemo(st *state.Setup, name string) execx.Spec {
	display, argv := imageArgs(st)
	if st.MicroVMActive() {
		const ateSetup = "go run ./cmd/ate-setup deploy demo counter-microvm"
		fallback := []string{"./hack/install-ate.sh --deploy-demo-counter-microvm"}
		if st.Prebuilt() {
			// On a pre-built install, pin ateom-microvm and counter to their
			// per-image registry digests (ActorTemplate admission requires an
			// '@' digest reference) and substitute them into the demo templates
			// inside a subshell so ko resolve/apply passes them through without
			// building or pushing anything. ate-setup's prebuilt resolver does
			// all of this itself, so only the fallback needs it.
			//
			// ImageRepo is free text, so the digest lookup has to work against
			// any registry, as ate-setup's own prebuilt resolver does on the
			// gVisor path. crane is that resolver's library
			// (go-containerregistry) at the version upstream pins; it is run
			// by version because the checkout vendors the library but not the
			// crane command.
			tag := imageVersion(st.ImageTag)
			ateomRef := ShellQuote(st.ImageRepo + "/ateom-microvm:" + tag)
			counterRef := ShellQuote(st.ImageRepo + "/counter:" + tag)
			fallback = []string{
				"(",
				// The edited templates are restored from the .bak copies sed
				// leaves, never from git: with --substrate-root the tree is
				// the user's, and uncommitted edits to these files are theirs.
				"  trap restore_demo_templates EXIT",
				"  trap 'exit 130' INT TERM",
				// Exported in here, not in the step's env, so it never
				// reaches ate-setup, whose prebuilt path leaves ko alone.
				"  export KO_DOCKER_REPO=ko.local",
				"  ATEOM_REF=" + ateomRef,
				"  COUNTER_REF=" + counterRef,
				`  ATEOM_DIGEST=$(` + craneDigest + ` "${ATEOM_REF}")`,
				`  COUNTER_DIGEST=$(` + craneDigest + ` "${COUNTER_REF}")`,
				`  sed -i.bak "s|ko://github.com/agent-substrate/substrate/cmd/ateom-microvm|${ATEOM_REF}@${ATEOM_DIGEST}|g" ` + demoPoolTemplate,
				`  sed -i.bak "s|ko://github.com/agent-substrate/substrate/demos/counter|${COUNTER_REF}@${COUNTER_DIGEST}|g" ` + demoActorTemplate,
				"  ./hack/install-ate.sh --deploy-demo-counter-microvm",
				")",
			}
		}
		lines := append(kubeconfigLines(st.ProjectID, st.ClusterName, st.Zone), restoreDemoTemplates...)
		// Display is fixed before the tree is inspected, so each branch says
		// which one ran: that line is how the log tells them apart.
		lines = append(lines,
			"if [ -d "+counterMicroVMDemoDir+" ]; then",
			"  echo "+ShellQuote("Deploying with: "+ateSetup),
			"  "+ateSetup+argv,
			"else",
			"  echo "+ShellQuote("Deploying with: hack/install-ate.sh --deploy-demo-counter-microvm"),
		)
		for _, l := range fallback {
			lines = append(lines, "  "+l)
		}
		lines = append(lines, "fi")
		return execx.Spec{
			Label: "deploy demo counter-microvm",
			// Which command runs is decided in the script, so the title
			// names the step and the script prints "Deploying with: …".
			Display:          "deploy demo counter-microvm",
			DisplayIsSummary: true,
			Argv:             b.inTree(strings.Join(lines, "\n")),
			Env:              microVMEnv(b.env(st)),
			SimLines: append(b.fetchSimLines(),
				"[step]: deploy_demo_counter_microvm",
				"workerpool.ate.dev/counter-microvm created",
				"actortemplate.ate.dev/counter-microvm created",
			),
		}
	}
	lines := append(kubeconfigLines(st.ProjectID, st.ClusterName, st.Zone),
		"go run ./cmd/ate-setup deploy demo "+ShellQuote(name)+argv)
	return execx.Spec{
		Label:   "ate-setup deploy demo " + name,
		Display: "go run ./cmd/ate-setup deploy demo " + name + display,
		Argv:    b.inTree(strings.Join(lines, "\n")),
		Env:     b.env(st),
		SimLines: append(b.fetchSimLines(),
			"[step]: deploy_demo_"+name,
			"workerpool.ate.dev/ate-demo-"+name+" created",
			"actortemplate.ate.dev/"+name+" created",
		),
	}
}

// microVMEnv is base (env or controlPlaneEnv) plus the settings upstream's
// micro-VM tooling reads that no other step needs: two the script itself
// reads, and one for the gcloud it uploads with. ate-setup's port of the
// script (agent-substrate/substrate#2092) ignores ARCH, since it stages both
// amd64 and arm64 on GKE, and reads ATE_INSTALL_KIND the same way.
//
// ARCH is pinned rather than left to the script's own resolution, which reads
// KO_DEFAULTPLATFORMS and falls back to the workstation's `go env GOARCH`.
// env only sets KO_DEFAULTPLATFORMS when the install builds from source, so a
// pre-built install leaves the script on that fallback — and the assets are
// executed by the cluster's nodes, not by the workstation. From an arm64
// laptop that stages arm64 binaries into the bucket, and the failure surfaces
// much later, as a cold boot that cannot exec them.
//
// ATE_INSTALL_KIND=false selects the GCS staging path over the in-cluster
// rustfs one. It is already the script's default; naming it here keeps a
// developer's exported value from redirecting a GKE install's assets into a
// kind bucket that does not exist.
//
// The KUBECTL_CONTEXT that env blanks matters most here: with one set,
// install-microvm-deps.sh skips its up-front context check and install-ate.sh
// skips its own get-credentials, so an inherited dev-env context (absent from
// the installer's KUBECONFIG, see kubeconfigLines) would fail only at
// `kubectl apply`, after the assets were assembled and uploaded. Blank, both
// use that KUBECONFIG's current context — install-ate.sh re-fetches the same
// credentials into it first, which is redundant but harmless.
//
// CLOUDSDK_STORAGE_PARALLEL_COMPOSITE_UPLOAD_ENABLED=False is for the `gcloud
// storage cp` both paths stage the assets with (the script's
// hack/microvm-assets/stage-to-gcs.sh; ate-setup shells out to the same
// command). The assets are large enough for gcloud to split each upload into
// components sent by worker processes, and on a workstation with
// context-aware access enabled (context_aware/use_client_certificate = true)
// those workers fail with `[SSL] PEM lib` and `Temporary components were not
// uploaded correctly`: each reads the per-invocation client certificate file
// that the parent gcloud writes and removes. The step then fails after the
// download, with half the assets and the components left in the bucket and
// the SandboxConfig never applied (#111). gcloud storage cp has no flag for
// this; the property is the only switch, and the sequential upload it
// selects costs nothing noticeable for four objects.
func microVMEnv(base []string) []string {
	return append(base,
		"ARCH="+targetArch,
		"ATE_INSTALL_KIND=false",
		"CLOUDSDK_STORAGE_PARALLEL_COMPOSITE_UPLOAD_ENABLED=False",
	)
}

// legacyMicroVMScript is upstream's shell micro-VM installer. Its presence in
// the checkout selects how InstallMicroVMDeps stages the assets:
// agent-substrate/substrate#2092 ported it into ate-setup (`deploy
// sandboxconfig --sandbox-classes`) and deleted it in the same commit, so the
// script and the flag never coexist. The pinned Commit (v1.0.0-rc.1) predates
// #2092 and still has the script; upstream main and release-1.0 do not.
const legacyMicroVMScript = "hack/install-microvm-deps.sh"

// microVMATESetup stages the micro-VM assets and applies the `microvm`
// SandboxConfig on trees from agent-substrate/substrate#2092 on. It stages
// both amd64 and arm64 into gs://<BUCKET_NAME>-assets under
// microvm-assets/<arch>/<sha256> with gcloud. The sandbox classes come from
// sandboxClassesEnv.
const microVMATESetup = "go run ./cmd/ate-setup deploy sandboxconfig"

// InstallMicroVMDeps stages the micro-VM sandbox assets and applies the
// cluster-wide `microvm` SandboxConfig from the pinned checkout: with
// ate-setup where the tree has it (see legacyMicroVMScript), else with
// upstream's hack/install-microvm-deps.sh.
//
// This is a separate step because micro-VM is opt-in: deploy ate-system is
// kept to gVisor (see sandboxClassesEnv), and the micro-VM SandboxConfig
// needs its sandbox binaries — cloud-hypervisor, virtiofsd, the guest kernel,
// the guest rootfs and the base kata config — staged in the asset bucket
// first, because the runtime fetches them from the bucket at boot rather than
// carrying them in the worker image.
//
// Upstream's tooling is driven rather than reimplemented in Go: it pins the
// staged assets' sha256 digests into the SandboxConfig at apply time.
//
// Credentials are fetched into the installer's KUBECONFIG the same way the
// control-plane step does. Both paths refuse to run without a resolvable
// context — deliberately, since they would otherwise discover the problem
// only after assembling and uploading everything — and an install must
// target the cluster the wizard chose, never whatever context happens to be
// ambient.
//
// Every ate-setup deploy replaces the install record the upgrade flow reads,
// so the ate-setup branch passes the control-plane step's settings and image
// flags, and records both sandbox classes. The script ignores them.
//
// TODO(#99): Drop the script branch, ARCH in microVMEnv, and the
// sha256sum/zstd/unzip entries in doctor.MicroVMTools once Commit is past
// agent-substrate/substrate#2092 and older commits typed into the images step
// no longer need them.
func (b *Builder) InstallMicroVMDeps(st *state.Setup) execx.Spec {
	display, argv := imageArgs(st)
	lines := kubeconfigLines(st.ProjectID, st.ClusterName, st.Zone)
	// Display is fixed before the tree is inspected, so each branch says
	// which one ran: that line is how the log tells them apart.
	lines = append(lines,
		"if [ -f "+legacyMicroVMScript+" ]; then",
		"  echo "+ShellQuote("Staging with: "+legacyMicroVMScript+" --install"),
		"  "+legacyMicroVMScript+" --install",
		"else",
		"  echo "+ShellQuote("Staging with: "+microVMATESetup+display),
		"  "+microVMATESetup+argv,
		"fi",
	)
	env := microVMEnv(b.controlPlaneEnv(st))
	return execx.Spec{
		Label: "stage micro-VM assets",
		// Which command runs is decided in the script, so the title names
		// the step and the script prints "Staging with: …".
		Display:          "stage micro-VM assets and apply the microvm SandboxConfig",
		DisplayIsSummary: true,
		Argv:             b.inTree(strings.Join(lines, "\n")),
		Env:              append(env, sandboxClassesEnv(state.SandboxGVisor, state.SandboxMicroVM)),
		// The dry run follows the pinned Commit, which still has the script.
		SimLines: append(b.fetchSimLines(),
			"Staging with: "+legacyMicroVMScript+" --install",
			"[install-microvm-deps]: Assembling micro-VM assets into bin/microvm-assets/amd64 (ARCH=amd64)...",
			"[install-microvm-deps]: Uploading assets to gs://"+st.BucketName+"-assets/kata-assets/ ...",
			"[install-microvm-deps]: Applying microvm SandboxConfig from manifests/microvm/sandboxconfig-microvm.yaml.tmpl...",
			"sandboxconfig.ate.dev/microvm created",
			"[install-microvm-deps]: Done. ActorTemplates must reference this SandboxConfig by name (sandboxConfig.configName: microvm).",
		),
	}
}

// FilestoreOverlayRepo is the upstream Filestore CSI driver repository, whose
// deploy/kubernetes/overlays/substrate is what the Filestore step installs.
const FilestoreOverlayRepo = "https://github.com/kubernetes-sigs/gcp-filestore-csi-driver.git"

// FilestoreAPICheckLine opens the check that the cluster serves the API
// version the overlay's CSIDriverConfig is written against. The checklist
// keys its row off it.
const FilestoreAPICheckLine = "Checking the cluster serves the overlay's CSIDriverConfig API"

// FilestoreAddonOffLine is printed in place of the addon update when the
// managed driver is already off, so the checklist row lights either way.
const FilestoreAddonOffLine = "The managed GKE Filestore CSI Driver addon is off; nothing to disable."

// filestoreAPICheck runs in the overlay directory, before anything is
// changed, and fails the step when the cluster does not serve the apiVersion
// of the overlay's CSIDriverConfig. The overlay is written against
// ate.dev/v1alpha1, which agent-substrate/substrate#2996 stopped serving
// before v1.0.0-rc.1 (kubernetes-sigs/gcp-filestore-csi-driver#1416); left
// to deploy.sh, that was found at the Kustomize apply, after it had granted
// roles/file.editor and the step had turned the managed addon off, and the
// apply itself had already created the namespace, Deployment and DaemonSet
// (#114). The version is read from the clone rather than assumed, so the
// check follows the overlay when it moves.
const filestoreAPICheck = `want=$(sed -n 's|^apiVersion:[[:space:]]*\(ate\.dev/[^[:space:]]*\).*|\1|p' csi_driver_config.yaml | head -n 1)
if [[ -z "${want}" ]]; then
  echo "error: could not read the CSIDriverConfig apiVersion from the overlay's csi_driver_config.yaml; its layout has changed" >&2
  exit 1
fi
echo "` + FilestoreAPICheckLine + ` (${want})..."
served=$(kubectl get crd csidriverconfigs.ate.dev -o jsonpath='{range .spec.versions[?(@.served==true)]}{.name}{" "}{end}' 2>/dev/null || true)
if [[ -z "${served}" ]]; then
  echo "error: the cluster has no csidriverconfigs.ate.dev CRD; Substrate's control plane has to be on before its Filestore driver" >&2
  exit 1
fi
if [[ " ${served} " != *" ${want#ate.dev/} "* ]]; then
  echo "error: the Filestore overlay's CSIDriverConfig is written against ${want}, but this cluster serves csidriverconfigs.ate.dev as: ${served% }" >&2
  echo "Nothing was changed: no IAM binding was granted and the managed Filestore addon was left as it is." >&2
  echo "The overlay has to move to a served version upstream (kubernetes-sigs/gcp-filestore-csi-driver#1416); skip this step until it has." >&2
  exit 1
fi
echo "The cluster serves csidriverconfigs.ate.dev as: ${served% }"`

// FilestoreServiceAccount is the name of the service account upstream's
// deploy.sh creates when run without --service-account, as the Filestore
// step runs it; teardown.sh deletes the same account.
const FilestoreServiceAccount = "substrate-filestore-csi"

// FilestoreGrantCheckLine opens the check that the project grants
// roles/file.editor to the Filestore CSI service account. The checklist
// keys its row off it.
const FilestoreGrantCheckLine = "Checking roles/file.editor is granted to the Filestore CSI service account"

// filestoreGrantAttempts bounds the re-grant in filestoreGrantCheck.
const filestoreGrantAttempts = 6

// filestoreGrantCheck runs after deploy.sh, with FILESTORE_PROJECT and
// FILESTORE_GSA set, and fails the step when the project policy does not
// grant roles/file.editor to the account. On a fresh project deploy.sh
// creates the account and grants the role in the next breath; IAM has not
// propagated the account yet, so the grant fails with "Service account ...
// does not exist", and deploy.sh swallows that (`|| true`), so the step
// "succeeds" without the role and the first CreateVolume fails with a
// permission error nothing points back here
// (kubernetes-sigs/gcp-filestore-csi-driver#1417, #143). Until that lands
// upstream, the installer reads the policy back and re-grants, a few
// seconds apart, for a bounded number of attempts; it says what to run when
// the role is still missing. A policy that already holds the role is left
// alone: no add-iam-policy-binding, no policy write.
var filestoreGrantCheck = `echo "` + FilestoreGrantCheckLine + ` (${FILESTORE_GSA})..."
filestore_role_granted() {
  local roles
  roles=$(gcloud projects get-iam-policy "${FILESTORE_PROJECT}" \
    --flatten="bindings[].members" \
    --filter="bindings.role=roles/file.editor AND bindings.members=serviceAccount:${FILESTORE_GSA}" \
    --format="value(bindings.role)") || return 1
  [[ "${roles}" == *roles/file.editor* ]]
}
attempt=0
until filestore_role_granted; do
  attempt=$((attempt + 1))
  if [[ "${attempt}" -gt ` + strconv.Itoa(filestoreGrantAttempts) + ` ]]; then
    echo "error: roles/file.editor is not granted to ${FILESTORE_GSA} on project ${FILESTORE_PROJECT} after ` + strconv.Itoa(filestoreGrantAttempts) + ` attempts; without it the Filestore CSI driver cannot create volumes." >&2
    echo "deploy.sh grants the role right after creating the account, before IAM knows the account, and ignores the failure (kubernetes-sigs/gcp-filestore-csi-driver#1417). Grant it and re-run this step:" >&2
    echo "  gcloud projects add-iam-policy-binding ${FILESTORE_PROJECT} --member=serviceAccount:${FILESTORE_GSA} --role=roles/file.editor --condition=None" >&2
    exit 1
  fi
  echo "roles/file.editor is not granted to ${FILESTORE_GSA} yet (deploy.sh grants it before IAM knows a new account; kubernetes-sigs/gcp-filestore-csi-driver#1417). Granting it, attempt ${attempt} of ` + strconv.Itoa(filestoreGrantAttempts) + `..."
  gcloud projects add-iam-policy-binding "${FILESTORE_PROJECT}" \
    --member="serviceAccount:${FILESTORE_GSA}" \
    --role=roles/file.editor \
    --condition=None \
    --format=none || true
  sleep 5
done
echo "roles/file.editor is granted to ${FILESTORE_GSA}."`

// DeployFilestoreCSI clones the GCP Filestore CSI Driver repository and invokes
// its Substrate overlay deploy script (deploy/kubernetes/overlays/substrate/deploy.sh).
// It automatically disables the managed GKE Filestore CSI Driver addon if enabled
// to prevent conflicts with the Substrate overlay. It needs no Substrate
// checkout of its own. The check and deploy.sh run kubectl against the
// current context, so the step points them at the installer's kubeconfig
// for the cluster first (see kubeconfigLines).
//
// Past that export, the script does nothing it cannot take back until
// filestoreAPICheck has passed: the clone and the check come first, the
// addon update and deploy.sh (which grants IAM before it applies) after.
// The clone is of HEAD, not a pin: no revision of the overlay serves
// ate.dev/v1 yet, so a pin would only fix the failure in place, while HEAD
// picks up the upstream fix when it lands and the check keeps the step
// honest until then. After deploy.sh, filestoreGrantCheck makes sure the
// role it may have failed to grant is there.
func (b *Builder) DeployFilestoreCSI(st *state.Setup) execx.Spec {
	// Wizard answers reach this script as text, so they are quoted for the
	// same reason the fetch preamble quotes its paths.
	project, cluster, zone := ShellQuote(st.ProjectID), ShellQuote(st.ClusterName), ShellQuote(st.Zone)
	deployCmd := fmt.Sprintf("./deploy.sh --project-id %s", project)

	script := strings.Join(append(append([]string{"set -euo pipefail"}, kubeconfigLines(st.ProjectID, st.ClusterName, st.Zone)...),
		`TMP_DIR=$(mktemp -d)`,
		`trap 'rm -rf "${TMP_DIR}"' EXIT`,
		`git clone --depth 1 `+FilestoreOverlayRepo+` "${TMP_DIR}/gcp-filestore-csi-driver"`,
		`cd "${TMP_DIR}/gcp-filestore-csi-driver/deploy/kubernetes/overlays/substrate"`,
		filestoreAPICheck,
		fmt.Sprintf(`FILESTORE_ADDON=$(gcloud container clusters describe %s --project=%s --location=%s --format="value(addonsConfig.gcpFilestoreCsiDriverConfig.enabled)" 2>/dev/null || true)`, cluster, project, zone),
		`if [[ "${FILESTORE_ADDON}" == "True" || "${FILESTORE_ADDON}" == "true" ]]; then`,
		`    echo "Disabling managed GKE Filestore CSI Driver addon..."`,
		fmt.Sprintf(`    gcloud container clusters update %s --project=%s --location=%s --update-addons=GcpFilestoreCsiDriver=DISABLED`, cluster, project, zone),
		`else`,
		`    echo "`+FilestoreAddonOffLine+`"`,
		`fi`,
		deployCmd,
		fmt.Sprintf(`FILESTORE_PROJECT=%s`, project),
		fmt.Sprintf(`FILESTORE_GSA=%s@%s.iam.gserviceaccount.com`, FilestoreServiceAccount, project),
		filestoreGrantCheck,
	), "\n")

	return execx.Spec{
		Label:   "deploy filestore csi driver",
		Display: deployCmd,
		Argv:    []string{"bash", "-c", script},
		Env:     b.env(st),
		SimLines: []string{
			"Cloning into 'gcp-filestore-csi-driver'...",
			FilestoreAPICheckLine + " (ate.dev/v1)...",
			"The cluster serves csidriverconfigs.ate.dev as: v1",
			"Disabling managed GKE Filestore CSI Driver addon...",
			"Updating " + st.ClusterName + "...",
			"Updated [" + st.ClusterName + "].",
			"=================================================================",
			"  🚀 GKE Substrate Filestore CSI Driver Overlay Deployment",
			"=================================================================",
			"📋 Configuration:",
			"   - GCP Project ID     : " + st.ProjectID,
			"   - GCP Service Account: substrate-filestore-csi@" + st.ProjectID + ".iam.gserviceaccount.com",
			"=================================================================",
			"🔐 Configuring Service Account & IAM Bindings...",
			"   - Verifying default Service Account existence...",
			"   - Service Account already exists (Skipping creation).",
			"   - Granting 'roles/file.editor' to the default Service Account on project " + st.ProjectID + "...",
			"   - Binding Workload Identity User role to the Kubernetes ServiceAccount...",
			"⚙️  Generating serviceaccount_patch.yaml with service account 'substrate-filestore-csi@" + st.ProjectID + ".iam.gserviceaccount.com'...",
			"📦 Applying Substrate Filestore CSI Driver Overlay via Kustomize...",
			"serviceaccount/gcp-filestore-csi-controller-sa configured",
			"service/csi-filestore-controller created",
			"csidriverconfig.ate.dev/filestore.csi.storage.gke.io created",
			"deployment.apps/gcp-filestore-csi-controller configured",
			"daemonset.apps/gcp-filestore-csi-node configured",
			"=================================================================",
			"✅ Deployment complete!",
			"   - CSI Driver & Substrate CSIDriverConfig applied.",
			"   - Check controller status : kubectl get pods -n gcp-filestore-csi-driver",
			"   - Check Substrate driver  : kubectl get csidriverconfig",
			"=================================================================",
			FilestoreGrantCheckLine + " (" + FilestoreServiceAccount + "@" + st.ProjectID + ".iam.gserviceaccount.com)...",
			"roles/file.editor is granted to " + FilestoreServiceAccount + "@" + st.ProjectID + ".iam.gserviceaccount.com.",
		},
	}
}

// EnableAutoscaling turns on cluster autoscaling for a node pool.
func (b *Builder) EnableAutoscaling(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label: "enable node-pool autoscaling",
		Display: fmt.Sprintf(
			"gcloud container node-pools update %s --cluster=%s --location=%s --enable-autoscaling --min-nodes=%d --max-nodes=%d",
			st.NodePool, st.ClusterName, st.Zone, st.AutoscaleMin, st.AutoscaleMax),
		Argv: []string{
			"gcloud", "container", "node-pools", "update", st.NodePool,
			"--project=" + st.ProjectID,
			"--cluster=" + st.ClusterName,
			"--location=" + st.Zone,
			"--enable-autoscaling",
			fmt.Sprintf("--min-nodes=%d", st.AutoscaleMin),
			fmt.Sprintf("--max-nodes=%d", st.AutoscaleMax),
		},
		SimLines: []string{
			"Updating node pool " + st.NodePool + "...",
			"Updated [" + st.ClusterName + "/" + st.NodePool + "].",
		},
	}
}

// Verify lists the control-plane pods so the user sees Substrate running. It
// reads the cluster the wizard installed into, through the installer's
// kubeconfig, not whatever context the user's shell has current.
// The default columns, not -o wide: the output lands in the completion
// screen's log panel, which wraps lines at its width, and the wide form's
// node names and IPs made every row wrap mid-field even at 110 columns.
func (b *Builder) Verify(st *state.Setup) execx.Spec {
	lines := append([]string{"set -euo pipefail"}, kubeconfigLines(st.ProjectID, st.ClusterName, st.Zone)...)
	lines = append(lines, "kubectl get pods -n ate-system")
	return execx.Spec{
		Label:   "verify ate-system",
		Display: "kubectl get pods -n ate-system",
		Argv:    []string{"bash", "-c", strings.Join(lines, "\n")},
		SimLines: []string{
			"NAME                              READY   STATUS    RESTARTS   AGE",
			"ate-api-server-6f4d9c-x2lkq       1/1     Running   0          2m",
			"ate-controller-58fd7b-mm4tw       1/1     Running   0          2m",
			"atelet-9kzql                      1/1     Running   0          2m",
			"atenet-router-7f66d8-qq2vr        1/1     Running   0          2m",
			"postgres-0                        1/1     Running   0          3m",
		},
	}
}
