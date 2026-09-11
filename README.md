# substrate-gke

GKE packaging for [Agent Substrate](https://github.com/agent-substrate/substrate): an
interactive installer that provisions the required GCP resources and installs the
Substrate control plane onto a GKE cluster.

![The installer's welcome screen](docs/screenshots/welcome.svg)

## Quickstart

Prerequisites: `gcloud` (authenticated, with application-default credentials), a Go
toolchain, `git`, and `kubectl`.

```bash
# One-line install and launch:
curl -sSL https://raw.githubusercontent.com/ai-on-gke/substrate-gke/main/install.sh | bash
```

Or from a local clone:

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

A terminal wizard walks nine steps, each running the real command it shows and
streaming its output:

1. **Check your setup** — probes for gcloud, credentials, Go, kubectl, network,
   and git, with copy-paste fixes.
2. **Choose your images** — pre-built release images (the default), or a build
   from source. See [Choosing images](#choosing-images).
3. **Choose your GCP project** — validated with `gcloud projects describe`.
4. **Connect your cluster** — lists your GKE clusters with their install state,
   or creates a new one. Substrate needs the PodCertificate Kubernetes beta
   APIs, which GKE only enables **at cluster creation**, so a fresh cluster is
   the recommended path. A cluster that already runs Substrate is blocked from
   reinstall (that would produce a broken, mixed-version cluster); the wizard
   offers an in-place teardown or points at the upgrade track instead.
5. **Provision GCP resources** — APIs, the cluster (if new), the snapshot
   bucket, IAM grants, and monitoring dashboards. Idempotent.
6. **Turn on Substrate** — installs CRDs, the API server, controller, atenet,
   and atelet.
7. **Install Filestore CSI driver** (optional).
8. **Configure autoscaling** (optional).
9. **Deploy a demo workload** (optional) — the upstream counter demo, with a
   live verification and next steps.

Exiting and re-running is safe; every step is idempotent.

| Connect your cluster | Blocked: already installed |
| --- | --- |
| ![Cluster list with install-state badges](docs/screenshots/clusters.svg) | ![The reinstall guard](docs/screenshots/guard.svg) |

| Provisioning | Complete |
| --- | --- |
| ![Provisioning GCP resources](docs/screenshots/provision.svg) | ![The completion screen](docs/screenshots/complete.svg) |

## Choosing images

Both options name a commit of `agent-substrate/substrate`, because the deploy
reads its manifests from a source tree either way:

- **Pre-built images** (default): the published release at
  `us-docker.pkg.dev/gke-substrate-release/substrate`, pinned to digests.
  Nothing is built or pushed, so your project needs no registry. Any registry
  and tag work — but move the commit with them: images from elsewhere need the
  commit they were built from, or they run behind mismatched manifests.
- **Build from source**: give a branch, tag, or commit; the images are built
  with [ko](https://ko.build) and pushed to your project's registry.

Either way the revision is resolved to an exact commit and verified against the
remote before the install starts.

## The Substrate checkout

The installer shallow-fetches the pinned Substrate tree into
`<user cache dir>/substrate-gke/substrate-<short commit>` on demand and deletes
it once the install succeeds. It is scratch space — to develop against
Substrate, point the installer at your own clone instead:

```bash
cd installer && go run . --substrate-root=/path/to/substrate
```

A checkout you supply is used as-is and never modified or deleted.

## Logs

Every command's full output is written to a timestamped log under
`<user cache dir>/substrate-gke/logs/`. Press `v` in the wizard for a
scrollable log viewer; failures show the extracted cause and the log path.

## Upgrading an installed cluster

Do not re-run the install track against a cluster that already runs Substrate
(the wizard blocks it). Run the installer and choose **Upgrade an installed
cluster**: it reads what the cluster runs, fetches the installed and new source
trees, and prints the hand-over for upstream's rolling upgrade runbook,
[`docs/upgrade.md`](https://github.com/agent-substrate/substrate/blob/main/docs/upgrade.md).
Nothing on the cluster changes until you follow the runbook.

## Tearing down

An install creates billable resources. Delete all of them — cluster, snapshot
bucket, IAM bindings, dashboards — with the values you gave the wizard (the
exit summary prints this exact invocation):

```bash
./tools/cleanup-gcp --project <project> --cluster <cluster> --location <zone> --bucket <bucket>
# or: make teardown PROJECT_ID=... CLUSTER_NAME=... CLUSTER_LOCATION=... BUCKET_NAME=...
```

It asks for confirmation and is safe to re-run after a partial failure. To
remove only the Substrate control plane while keeping the cluster, use the
`ate-setup delete ate-system` command the exit summary prints.

## Development

```bash
make test         # unit tests, including a scripted dry-run walk of the wizard
make verify       # gofmt + go vet
make screenshots  # regenerate the README screenshots from the dry-run wizard
```

**Bumping the pinned Substrate commit** (changes what fresh installs get): edit
`Commit` in `installer/internal/snapshot/snapshot.go` and update `MinGoVersion`
to match upstream's `go.mod` at that commit — `make substrate-pin-check`
verifies it. When a new release is published, bump `ReleaseVersion` and
`Commit` together: the commit must be what the release images were built from.
