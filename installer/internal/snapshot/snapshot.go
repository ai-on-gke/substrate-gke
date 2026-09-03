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
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
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
	// The step offers this as the manifest revision behind the pre-built
	// images, so it has to be the commit ReleaseVersion was built from. The
	// build-from-source track offers its repository's live HEAD instead and
	// only falls back here for --dry-run, which resolves nothing.
	//
	// Bump this to move to a newer Substrate, and update MinGoVersion to
	// match the `go` directive in that revision's go.mod.
	Commit = "cbae8250a5ff157e1ec69618804b07b825cdf52c"

	// MinGoVersion mirrors the `go` directive in go.mod at Commit. The doctor
	// prefers the real go.mod once the tree is on disk and falls back to this
	// when checking Go before the first fetch.
	MinGoVersion = "1.27.0"

	// ReleaseRepo publishes the GKE Substrate release images, and is the
	// registry the wizard offers by default. It is read-only to installs:
	// images are pulled from it and never pushed to it, which is why
	// installing pre-built images leaves KO_DOCKER_REPO out of the picture
	// entirely rather than pointing it here.
	ReleaseRepo = "us-docker.pkg.dev/gke-substrate-release/substrate"

	// ReleaseVersion is the tag the wizard offers by default, published from
	// Commit — the two move together, because the manifests read out of that
	// tree have to match the images this tag names.
	//
	// Both of these are defaults, not limits: the wizard takes any registry
	// and tag the user types, so a team installing its own published build
	// never has to fall back to building from source. It asks such a team for
	// a manifest revision as well, since only this registry is published
	// alongside a tree known to match.
	ReleaseVersion = "v0.1.0"
)

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
// weeks later, it must not depend on whatever kubectl context is ambient. It
// carries no image flags: deleting removes what the manifests name and never
// resolves an image reference, so a pre-built install tears down identically.
func (b *Builder) TeardownCommand(st *state.Setup, root string) string {
	env := fmt.Sprintf("PROJECT_ID=%s CLUSTER_NAME=%s CLUSTER_LOCATION=%s NO_DEV_ENV=1",
		ShellQuote(st.ProjectID), ShellQuote(st.ClusterName), ShellQuote(st.Zone))
	del := env + " go run ./cmd/ate-setup delete ate-system"
	if root != "" {
		return fmt.Sprintf("(cd %s && %s)", ShellQuote(root), del)
	}
	return b.inEphemeralTree(del)
}

// KubectlAteInstall returns the command that installs the kubectl-ate plugin
// at the revision this install used, for machines where the managed checkout
// is already gone. It has to build from a checkout: `go install
// <module>@<commit>` is refused for this module, whose go.mod replaces
// k8s.io/apimachinery with a local third_party path.
func (b *Builder) KubectlAteInstall() string {
	return b.inEphemeralTree("go install ./cmd/kubectl-ate")
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
	// repo and commit are the tree to fetch. They start at the pin and move
	// only when the wizard's images step picks something else.
	repo, commit string
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

// UseSource repoints the builder at another repository and commit — a fork, a
// branch, or a hotfix the user named in the images step.
//
// A managed tree is cached per commit, so the root and the ko version stamp
// move with it: two revisions never share a directory, and the images one
// build produces are never mistaken for another's. A tree the user supplied
// with --substrate-root is theirs, and is left exactly where it is.
func (b *Builder) UseSource(rev Revision) {
	b.repo, b.commit = rev.Repo, rev.Commit
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

// env builds the environment both upstream tools read, mirroring
// hack/ate-dev-env.sh.example. NO_DEV_ENV keeps ate-setup from sourcing a
// developer's .ate-dev-env.sh out from under the wizard's answers.
func (b *Builder) env(st *state.Setup) []string {
	env := []string{
		"PROJECT_ID=" + st.ProjectID,
		"PROJECT_NUMBER=" + st.ProjectNumber,
		"GCE_REGION=" + st.Region(),
		"CLUSTER_LOCATION=" + st.Zone,
		"CLUSTER_NAME=" + st.ClusterName,
		"NETWORK=" + st.Network,
		"SUBNETWORK=" + st.Subnetwork,
		"GVISOR_NODE_MACHINE_TYPE=" + st.MachineType,
		"BUCKET_NAME=" + st.BucketName,
		"NO_DEV_ENV=1",
	}
	if st.Prebuilt() {
		// Nothing is built and nothing is pushed, so a KO_DOCKER_REPO would
		// name a registry this install never writes to.
		//
		// VERSION is the image tag, not the checkout's commit. ate-setup
		// falls back to the tag on its own, but VERSION wins over it and
		// every spec inherits the caller's environment, so a developer who
		// sourced an ate-dev-env.sh would otherwise name the atelet DaemonSet
		// and label nodes after a version no installed image carries. Pinning
		// it here keeps the label matching the image, and makes an upgrade a
		// change of tag and nothing else.
		return append(env, "VERSION="+imageVersion(st.ImageTag))
	}
	return append(env,
		"KO_DOCKER_REPO="+st.KoDockerRepo,
		"KO_DEFAULTPLATFORMS=linux/amd64",
		"VERSION="+b.Version,
	)
}

// imageVersion is the Substrate version a pre-built image tag names. A tag may
// carry the digest it resolved to (v0.1.0@sha256:...); the version is the tag
// alone, the same cut ate-setup makes on its own fallback.
func imageVersion(tag string) string {
	v, _, _ := strings.Cut(tag, "@")
	return v
}

// labelValue is the Kubernetes label-value grammar, which is the whole
// constraint on an image tag here.
var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// labelValueMaxLength mirrors k8s.io/apimachinery's validation constant. It is
// restated rather than imported: the installer depends on no Kubernetes
// libraries, and one integer is not worth the module graph.
const labelValueMaxLength = 63

// CheckImageTag reports whether a pre-built image tag can serve as the
// Substrate version. That is the only thing constraining it: registries accept
// far more than this, but the version becomes the ate.dev/substrate-version
// node label and the atelet DaemonSet's name suffix, and ate-setup refuses a
// version that is not a valid label value rather than silently sanitizing one.
//
// Checking here turns that refusal into a correctable prompt, for the same
// reason the revision is resolved before the install starts rather than during
// it.
func CheckImageTag(tag string) error {
	v := imageVersion(tag)
	if len(v) > labelValueMaxLength {
		return fmt.Errorf("%q is %d characters; a version can be at most %d, because it becomes a node label",
			v, len(v), labelValueMaxLength)
	}
	if !labelValue.MatchString(v) {
		return fmt.Errorf("%q cannot be a Substrate version: it becomes the ate.dev/substrate-version node label, so it must be letters, digits, '.', '-', or '_', beginning and ending with a letter or digit", v)
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

// Bootstrap provisions GCP resources (APIs, cluster, bucket, IAM,
// dashboards) via the upstream tools/setup-gcp. All seven steps are
// idempotent, so it is safe to run against an existing cluster. This is the
// first step to touch the checkout, so it usually pays the fetch.
func (b *Builder) Bootstrap(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label:   "setup-gcp bootstrap",
		Display: "go run ./tools/setup-gcp bootstrap",
		Argv:    b.inTree("go run ./tools/setup-gcp bootstrap"),
		Env:     b.env(st),
		SimLines: append(b.fetchSimLines(),
			"Step 1/7: Enabling required APIs...",
			"Step 2/7: Creating GKE Cluster...",
			"Step 3/7: Creating GCS Bucket for snapshots...",
			"Step 4/7: Granting GKE Node permissions...",
			"Step 5/7: Granting Atelet permissions...",
			"Step 6/7: Creating IAM policy bindings for bucket...",
			"Step 7/7: Creating Monitoring Dashboards...",
			"Bootstrap completed successfully.",
		),
	}
}

// DeployAteSystem installs the Substrate control plane with the upstream
// cmd/ate-setup. ate-setup fetches cluster credentials itself from
// PROJECT_ID/CLUSTER_NAME/CLUSTER_LOCATION, and ko builds and pushes the
// control-plane images from the checkout's source.
func (b *Builder) DeployAteSystem(st *state.Setup) execx.Spec {
	display, argv := imageArgs(st)
	sim := append(b.fetchSimLines(),
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
			"Building github.com/agent-substrate/substrate/cmd/ate-api-server",
			"Building github.com/agent-substrate/substrate/cmd/atelet",
		)
	}
	return execx.Spec{
		Label:   "ate-setup deploy ate-system",
		Display: "go run ./cmd/ate-setup deploy ate-system" + display,
		Argv:    b.inTree("go run ./cmd/ate-setup deploy ate-system" + argv),
		Env:     b.env(st),
		SimLines: append(sim,
			"[step]: Waiting for ATE system components to be ready...",
			`deployment "ate-api-server" successfully rolled out`,
			`daemon set "atelet" successfully rolled out`,
		),
	}
}

// DeployDemo deploys one of the upstream demo applications (for the wizard,
// the counter demo). name is quoted like every other value inTree splices
// into a script: the only caller passes a literal today, but a demo name
// picked from a list or typed in would otherwise reach bash as source.
func (b *Builder) DeployDemo(st *state.Setup, name string) execx.Spec {
	display, argv := imageArgs(st)
	return execx.Spec{
		Label:   "ate-setup deploy demo " + name,
		Display: "go run ./cmd/ate-setup deploy demo " + name + display,
		Argv:    b.inTree("go run ./cmd/ate-setup deploy demo " + ShellQuote(name) + argv),
		Env:     b.env(st),
		SimLines: append(b.fetchSimLines(),
			"[step]: deploy_demo_"+name,
			"workerpool.ate.dev/ate-demo-"+name+" created",
			"actortemplate.ate.dev/"+name+" created",
		),
	}
}

// DeployFilestoreCSI clones the GCP Filestore CSI Driver repository and invokes
// its Substrate overlay deploy script (deploy/kubernetes/overlays/substrate/deploy.sh).
// It automatically disables the managed GKE Filestore CSI Driver addon if enabled
// to prevent conflicts with the Substrate overlay. It needs no Substrate
// checkout of its own.
func (b *Builder) DeployFilestoreCSI(st *state.Setup) execx.Spec {
	// Wizard answers reach this script as text, so they are quoted for the
	// same reason the fetch preamble quotes its paths.
	project, cluster, zone := ShellQuote(st.ProjectID), ShellQuote(st.ClusterName), ShellQuote(st.Zone)
	deployCmd := fmt.Sprintf("./deploy.sh --project-id %s", project)

	script := strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf(`FILESTORE_ADDON=$(gcloud container clusters describe %s --project=%s --location=%s --format="value(addonsConfig.gcpFilestoreCsiDriverConfig.enabled)" 2>/dev/null || true)`, cluster, project, zone),
		`if [[ "${FILESTORE_ADDON}" == "True" || "${FILESTORE_ADDON}" == "true" ]]; then`,
		`    echo "Disabling managed GKE Filestore CSI Driver addon..."`,
		fmt.Sprintf(`    gcloud container clusters update %s --project=%s --location=%s --update-addons=GcpFilestoreCsiDriver=DISABLED`, cluster, project, zone),
		`fi`,
		`TMP_DIR=$(mktemp -d)`,
		`trap 'rm -rf "${TMP_DIR}"' EXIT`,
		`git clone --depth 1 https://github.com/kubernetes-sigs/gcp-filestore-csi-driver.git "${TMP_DIR}/gcp-filestore-csi-driver"`,
		`cd "${TMP_DIR}/gcp-filestore-csi-driver/deploy/kubernetes/overlays/substrate"`,
		deployCmd,
	}, "\n")

	return execx.Spec{
		Label:   "deploy filestore csi driver",
		Display: deployCmd,
		Argv:    []string{"bash", "-c", script},
		Env:     b.env(st),
		SimLines: []string{
			"Disabling managed GKE Filestore CSI Driver addon...",
			"Updating " + st.ClusterName + "...",
			"Updated [" + st.ClusterName + "].",
			"Cloning into 'gcp-filestore-csi-driver'...",
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

// Verify lists the control-plane pods so the user sees Substrate running.
func (b *Builder) Verify(st *state.Setup) execx.Spec {
	return execx.Spec{
		Label:   "verify ate-system",
		Display: "kubectl get pods -n ate-system",
		Argv:    []string{"kubectl", "get", "pods", "-n", "ate-system", "-o", "wide"},
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
