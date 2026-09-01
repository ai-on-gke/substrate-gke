# substrate-gke

GKE packaging for [Agent Substrate](https://github.com/agent-substrate/substrate): an
interactive installer that provisions the required GCP resources and installs the
Substrate control plane onto a GKE cluster.

## Quickstart

Prerequisites: `gcloud` (authenticated, with application-default credentials), a Go
toolchain, `git`, and `kubectl`.

```bash
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
   kubectl, network reachability, and git, with copy-paste fixes.
2. **Choose your GCP project** — validated with `gcloud projects describe`.
3. **Connect your cluster** — lists your GKE clusters and whether each can run
   Substrate, or lets you create a new one. Substrate needs the PodCertificate
   Kubernetes beta APIs, which GKE only enables **at cluster creation** — clusters
   created without them cannot be fixed afterward, which is why creating a fresh
   cluster is the recommended path.
4. **Provision GCP resources** — `setup-gcp bootstrap`: APIs, the cluster (if new),
   the snapshot bucket, IAM grants, and monitoring dashboards. Idempotent.
5. **Turn on Substrate** — `ate-setup deploy ate-system`: builds the control-plane
   images from source with ko, pushes them to your project's registry, and installs
   CRDs, the API server, controller, atenet, and atelet.
6. **Install Filestore CSI driver** (optional) — deploys the GCP Filestore CSI Driver
   configured for substrate. Note that after the driver is installed, additional
   steps are needed to configure a Filestore VolumePool.
7. **Configure autoscaling** (optional) — node-pool autoscaling via gcloud.
8. **Deploy a demo workload** (optional) — the upstream counter demo, then a live
   verification and next steps.

Exiting and re-running is always safe; every step is idempotent.

## Repository layout

```
installer/   The wizard (Go, bubbletea). `go run .` from this directory works too.
```

## How Substrate itself is obtained

Upstream's installer builds the control-plane images from source with
[ko](https://ko.build), so a Substrate source tree has to be on disk at install time —
the manifests alone are not enough.

Rather than vendoring it, the installer fetches it with a shallow `git` fetch pinned to
an exact upstream commit. The first install step that needs the tree downloads it (a
few seconds) into

```
<user cache dir>/substrate-gke/substrate-<short commit>
```

and later steps reuse it. The directory is keyed by commit, so bumping the pin fetches
into a fresh directory instead of mutating the old one. Nothing is written into this
repo, and `agent-substrate/substrate` is public, so the fetch needs no credentials.

To point at your own checkout instead — handy when testing an unmerged change:

```bash
cd installer && go run . --substrate-root=/path/to/substrate
```

### Moving to a newer Substrate

Edit `Commit` in `installer/internal/snapshot/snapshot.go`, and update `MinGoVersion`
next to it to match the `go` directive in that revision's `go.mod`. `make substrate-pin`
prints the current values.

## Development

```bash
make test      # unit tests, including a scripted dry-run walk of the whole wizard
make verify    # gofmt + go vet
```
