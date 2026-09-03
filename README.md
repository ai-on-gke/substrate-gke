# substrate-gke

GKE packaging for [Agent Substrate](https://github.com/agent-substrate/substrate): an
interactive installer that provisions the required GCP resources and installs the
Substrate control plane onto a GKE cluster.

## Quickstart

Prerequisites: `gcloud` (authenticated, with application-default credentials), a Go
toolchain, `git`, and `kubectl`.

```bash
git clone https://github.com/ai-on-gke/substrate-gke.git
cd substrate-gke
gcloud auth application-default login
make run          # launch the interactive installer
```

Useful variants:

```bash
make doctor       # preflight checks only
make dry-run      # walk the full wizard without touching GCP
```

## What the installer does

A terminal wizard (ported from upstream PR #1171's onboarding UX) walks multiple steps,
each running the real command it shows and streaming its output:

1. **Check your setup** — probes for gcloud, application-default credentials, Go,
   kubectl, network reachability, and git, with copy-paste fixes. It runs first
   because the next step is the first to reach the network.
2. **Choose your images** — install pre-built images, or build them yourself from a
   source tree you name — see
   [Where the images come from](#where-the-images-come-from). It comes before the
   project step because the answer decides what that step needs: a pre-built
   install pushes nothing, so it is never asked for a registry.
3. **Choose your GCP project** — validated with `gcloud projects describe`.
4. **Connect your cluster** — lists your GKE clusters and whether each can run
   Substrate, or lets you create a new one. Substrate needs the PodCertificate
   Kubernetes beta APIs, which GKE only enables **at cluster creation** — clusters
   created without them cannot be fixed afterward, which is why creating a fresh
   cluster is the recommended path.
5. **Provision GCP resources** — `setup-gcp bootstrap`: APIs, the cluster (if new),
   the snapshot bucket, IAM grants, and monitoring dashboards. Idempotent.
6. **Turn on Substrate** — `ate-setup deploy ate-system`: installs CRDs, the API
   server, controller, atenet, and atelet, from the images chosen in step 2.
7. **Install Filestore CSI driver** (optional) — deploys the GCP Filestore CSI Driver
   configured for substrate. Note that after the driver is installed, additional
   steps are needed to configure a Filestore VolumePool.
8. **Configure autoscaling** (optional) — node-pool autoscaling via gcloud.
9. **Deploy a demo workload** (optional) — the upstream counter demo, then a live
   verification and next steps.

Exiting and re-running is always safe; every step is idempotent.

## Repository layout

```
installer/   The wizard (Go, bubbletea). `go run .` from this directory works too.
```

## Where the images come from

The images step chooses between two ways of getting the Substrate control-plane
images. Both end up naming a git commit, because `ate-setup` reads the deployment
manifests from a source tree either way.

**Pre-built images** — the default, and four fields: the image registry, the image
tag, the manifest repository, and the manifest commit id. All four are offered
pre-filled and all four can be overridden. The defaults are the published release at
`us-docker.pkg.dev/gke-substrate-release/substrate`, which is where we will host the
release images — it is coming soon, and the step says so — and the commit pinned below,
which is what they were built from. `ate-setup` pins every image to the digest its tag
resolves to. Nothing is built and nothing is pushed, so your project needs no image
registry of its own; the release registry in particular is pull-only.

Any registry and tag work, so a team that publishes its own builds — a staging
registry, or a private rebuild of a release — installs them by typing them here rather
than by building from source. **Move the manifest fields with them.** Only the release
registry is published alongside a tree known to match its tags; images from anywhere
else need the repository and commit they were built from, or they run behind manifests
from a different Substrate. The wizard warns about this as soon as the registry is
changed. (Inferring the commit from the images themselves is a later change.)

The tag doubles as the Substrate version — it names the atelet DaemonSet and sets the
`ate.dev/substrate-version` node label — so it has to be a valid Kubernetes label
value, and the wizard says so at the prompt rather than letting the install discover it.
A tag that carries its digest (`v0.1.0@sha256:…`) is fine; the version is the tag alone.

**A build from source** — for a fork, a branch, or a private hotfix. You give a
repository (defaulting to `agent-substrate/substrate`) and a revision in it: a branch, a
tag, or a full commit SHA. The revision box is pre-filled with that repository's current
HEAD, resolved with `git ls-remote` and shown as a commit id, so accepting it builds
exactly what is on the default branch right now; editing the repository clears it. The
images are built with [ko](https://ko.build) and pushed to your project's registry.

Either way, what you name is resolved to an exact commit before the install starts, and
that commit is checked against the remote with a filtered shallow fetch — naming a commit and
being served it are different things, and the difference is better found at the prompt
than ten minutes into an install. Repositories are read over https only.

## How Substrate itself is obtained

`ate-setup` reads the deployment manifests from a Substrate source tree, so one has to
be on disk at install time whichever images you chose — a pre-built install skips the
build, not the checkout. That is why both paths of the images step ask for a revision.

Rather than vendoring it, the installer fetches it with a shallow `git` fetch pinned to
an exact commit: whatever you named there, resolved to a full SHA before the install
starts. The first install step that needs the tree downloads it (a few seconds) into

```
<user cache dir>/substrate-gke/substrate-<short commit>
```

and later steps reuse it — one directory per commit, so two revisions never collide.
Nothing is written into this repo, and `agent-substrate/substrate` is public, so the
fetch of the default pin needs no credentials.

That tree is scratch space for one install, not somewhere to work: **it is deleted once
the install succeeds**, and re-running the installer fetches it again. If you want to
develop against Substrate, use your own clone (see `--substrate-root` below) — a copy
here would be removed out from under you.

Nothing is deleted while an install could still be retried, so a failed run leaves the
tree in place and retrying it costs no download. The tree is staged under a temporary
name and moved into place only once complete, so interrupting a fetch costs you the
download and nothing else.

To point at your own checkout instead — handy when testing an unmerged change:

```bash
cd installer && go run . --substrate-root=/path/to/substrate
```

A checkout you supply this way is used as-is and never modified or deleted.

### Moving to a newer Substrate

Edit `Commit` in `installer/internal/snapshot/snapshot.go`, and update `MinGoVersion`
next to it to match the `go` directive in that revision's `go.mod`. `make substrate-pin`
prints the current values, and `make substrate-pin-check` verifies `MinGoVersion`
against upstream's `go.mod` at that commit — run it after every bump, since a stale
value lets the preflight doctor pass and the install then fail mid-bootstrap.

`ReleaseRepo` and `ReleaseVersion` next to it are the registry and tag the images step
offers by default; bump `ReleaseVersion` and `Commit` together when a newer release is
published, since `Commit` is the manifest revision offered behind those images and has
to be what they were built from.

All three are defaults, not limits. The wizard accepts any registry, tag, and revision,
and the build-from-source track never uses `Commit` at all — it offers its repository's
live HEAD — so none of them has to change for someone installing their own build.

## Tearing down

An install creates billable resources: the GKE cluster, the snapshot bucket, IAM
bindings, and monitoring dashboards. Delete all of them with the values you gave the
wizard (the exit summary prints this exact invocation for your install):

```bash
./tools/cleanup-gcp --project <project> --cluster <cluster> --location <zone> --bucket <bucket>
# or: make teardown PROJECT_ID=<project> CLUSTER_NAME=<cluster> CLUSTER_LOCATION=<zone> BUCKET_NAME=<bucket>
```

The script asks for confirmation, then delegates the deletions to upstream's
`hack/teardown.sh` fetched at the same pinned commit the installer built from — the
teardown that matches what that bootstrap created — so it needs `gcloud` and `git`. It
is safe to re-run after a partial failure. To remove only the Substrate control plane
while keeping the cluster, use the `ate-setup delete ate-system` command the exit
summary prints instead. APIs enabled by the install are left enabled; they cost
nothing while unused.

## Development

```bash
make test      # unit tests, including a scripted dry-run walk of the whole wizard
make verify    # gofmt + go vet
```
