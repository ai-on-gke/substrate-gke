#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit -o nounset -o pipefail

# Installs (or deletes) the cluster-wide micro-VM (kata + cloud-hypervisor)
# SandboxConfig on GKE, after staging its assets into the GCS asset bucket
# that `setup-gcp bootstrap` (or `create bucket`) creates, BUCKET_NAME-assets.
# It does what the GCS branch of agent-substrate/substrate's
# hack/install-microvm-deps.sh and hack/microvm-assets/stage-to-gcs.sh does,
# so that this repository's installer does not depend on a dev script in core
# for a bucket setup-gcp names (#82). The assets themselves are not specific to
# GKE: the parts that are not GCS-specific stay upstream and are read from a
# Substrate checkout:
#
#   * hack/microvm-assets/assemble.sh downloads the asset set for one arch.
#   * manifests/microvm/sandboxconfig-microvm.yaml.tmpl is the SandboxConfig.
#     It pins every asset's sha256, so the apply substitutes only the bucket's
#     scheme and name.
#
# On --install: assembles the asset set (skipped when OUT already holds it at
# the versions assemble.sh would download now), uploads it under kata-assets/
# in the asset bucket, and applies the `microvm` SandboxConfig pointing at it.
# atelet fetches the assets through the node's object-store plugin at boot. The
# plugin refuses anything under a snapshot location, and setup-gcp lets nodes
# read only the asset bucket, so the assets must not go in the snapshot bucket.
#
# On --delete: removes the SandboxConfig, unless a WorkerPool still uses it.
# Bucket contents are left alone: they are inert until a SandboxConfig points
# at them, and re-staging is cheap.
#
# Run from a Substrate checkout, or name one with SUBSTRATE_ROOT.
#
# Env:
#   BUCKET_NAME      (required) the snapshot bucket setup-gcp was given. The
#                    assets go to BUCKET_NAME-assets.
#   SUBSTRATE_ROOT   Substrate checkout (default: the git root of the working
#                    directory).
#   KUBECTL_CONTEXT  (optional) kube context; otherwise the current context.
#   PROJECT_ID       (optional) passed to gcloud as --project.
#   ARCH             node arch, amd64 or arm64 (default: from
#                    KO_DEFAULTPLATFORMS, else the workstation's GOARCH). The
#                    nodes run these binaries, so set it when the two differ.
#   OUT              asset dir (default: SUBSTRATE_ROOT/bin/microvm-assets/ARCH).
#   NO_DEV_ENV       set to skip sourcing .ate-dev-env.sh from the working
#                    directory.

if [ -z "${NO_DEV_ENV:-}" ] && [ -f .ate-dev-env.sh ]; then
  source .ate-dev-env.sh
fi

KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-}"

usage() {
  cat <<EOF
Usage: $0 (--install | --delete)

Options:
  --install   Assemble and stage the micro-VM assets in gs://BUCKET_NAME-assets
              and apply the cluster-wide microvm SandboxConfig.
  --delete    Remove the microvm SandboxConfig from the cluster.
              (Bucket contents are left alone.)
  -h, --help  Show this message.
EOF
}

action=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --install) action="install" ;;
    --delete)  action="delete"  ;;
    -h|--help) usage; exit 0    ;;
    *) echo "Error: unknown argument $1" >&2; usage; exit 1 ;;
  esac
  shift
done

if [[ -z "${action}" ]]; then
  usage
  exit 1
fi

if [[ -z "${BUCKET_NAME:-}" ]]; then
  echo "Error: BUCKET_NAME is not set; export the snapshot bucket name setup-gcp" >&2
  echo "       was given (see tools/setup-gcp/README.md). The assets go to" >&2
  echo "       BUCKET_NAME-assets." >&2
  exit 1
fi
ASSET_BUCKET="${BUCKET_NAME}-assets"

# kubectl falls back to localhost:8080 when neither --context nor a kubeconfig
# current-context is set, which would surface as "connection refused" from the
# apply, after the assets were assembled and uploaded. Resolve the target
# cluster up front instead.
if [[ -z "${KUBECTL_CONTEXT}" ]] && ! kubectl config current-context >/dev/null 2>&1; then
  echo "Error: no kube context to target: KUBECTL_CONTEXT is empty and the" >&2
  echo "       kubeconfig has no current-context. Set KUBECTL_CONTEXT or run:" >&2
  echo "         gcloud container clusters get-credentials <cluster> --location <location>" >&2
  exit 1
fi

COLOR_CYAN='\033[1;36m'
COLOR_RESET='\033[0m'
# The installer's checklist (installer/internal/steps.MicroVMDeps) follows
# these lines, so keep their prefix and wording.
log() {
  echo -e "${COLOR_CYAN}[install-microvm-deps]: $*${COLOR_RESET}"
}

run_kubectl() {
  kubectl ${KUBECTL_CONTEXT:+--context="${KUBECTL_CONTEXT}"} "$@"
}

run_gcloud() {
  gcloud ${PROJECT_ID:+--project="${PROJECT_ID}"} "$@"
}

if [[ "${action}" == "delete" ]]; then
  # A WorkerPool that uses the config holds it with a protection finalizer.
  # The delete would only mark it Terminating, and `kubectl delete` would wait
  # for it without end; a Terminating config also cannot be recreated under
  # its name. So no delete is issued at all: the config stays and the pools
  # are named. A cluster without the WorkerPool CRD has no users.
  users="$(run_kubectl get workerpools --all-namespaces \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.status.sandboxClasses[*].configRef.name}{"\n"}{end}' 2>/dev/null \
    | awk '{ for (i = 2; i <= NF; i++) if ($i == "microvm") { print $1; break } }' || true)"
  if [[ -n "${users}" ]]; then
    echo "Warning: the microvm SandboxConfig is left in place; these WorkerPools" >&2
    echo "         still use it:" >&2
    while IFS= read -r pool; do echo "           ${pool}" >&2; done <<<"${users}"
    echo "         Delete them, or point their spec.sandboxClasses[].configRef at" >&2
    echo "         another microvm SandboxConfig, and run this again." >&2
    exit 0
  fi
  log "Deleting microvm SandboxConfig..."
  # By name rather than by manifest, so this needs no Substrate checkout. The
  # timeout turns a missing atecontroller, which releases the finalizer, into
  # an error instead of a hang.
  if ! run_kubectl delete --ignore-not-found --timeout=60s sandboxconfig microvm; then
    echo "Error: the microvm SandboxConfig did not finish deleting. It stays" >&2
    echo "       Terminating until atecontroller releases its protection" >&2
    echo "       finalizer; check that atecontroller is running, then run this" >&2
    echo "       again." >&2
    exit 1
  fi
  log "Done. (Bucket assets at gs://${ASSET_BUCKET}/kata-assets/ left in place.)"
  exit 0
fi

# --- install ----------------------------------------------------------------

SUBSTRATE_ROOT="${SUBSTRATE_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || true)}"
ASSEMBLE="hack/microvm-assets/assemble.sh"
MANIFEST_TEMPLATE="manifests/microvm/sandboxconfig-microvm.yaml.tmpl"
if [[ -z "${SUBSTRATE_ROOT}" || ! -f "${SUBSTRATE_ROOT}/${ASSEMBLE}" || ! -f "${SUBSTRATE_ROOT}/${MANIFEST_TEMPLATE}" ]]; then
  echo "Error: ${SUBSTRATE_ROOT:-the working directory} is not an agent-substrate/substrate" >&2
  echo "       checkout with ${ASSEMBLE} and ${MANIFEST_TEMPLATE}." >&2
  echo "       Run this from one, or set SUBSTRATE_ROOT." >&2
  exit 1
fi
SUBSTRATE_ROOT="$(cd "${SUBSTRATE_ROOT}" && pwd)"

# Target arch: match the images' platform when KO_DEFAULTPLATFORMS names one;
# fall back to the workstation's.
if [[ -z "${ARCH:-}" ]]; then
  if [[ -n "${KO_DEFAULTPLATFORMS:-}" ]]; then
    ARCH="${KO_DEFAULTPLATFORMS##*/}"
  else
    ARCH="$(go env GOARCH)"
  fi
fi
OUT="${OUT:-${SUBSTRATE_ROOT}/bin/microvm-assets/${ARCH}}"
ASSETS=(cloud-hypervisor virtiofsd vmlinux rootfs.img)

# assemble.sh resolves its paths from the git root, so it runs from the tree.
assemble() {
  (cd "${SUBSTRATE_ROOT}" && ARCH="${ARCH}" OUT="${OUT}" "./${ASSEMBLE}" "$@")
}

# Fail before the slow assemble if the asset bucket is missing: setup-gcp
# creates it, and nothing here should.
if ! run_gcloud storage buckets describe "gs://${ASSET_BUCKET}" --format="value(name)" >/dev/null; then
  echo "Error: cannot read gs://${ASSET_BUCKET}. setup-gcp creates it beside the" >&2
  echo "       snapshot bucket: run \`go run ./tools/setup-gcp create bucket\` from" >&2
  echo "       substrate-gke, or check BUCKET_NAME and PROJECT_ID." >&2
  exit 1
fi

# --- 1. assemble (if missing or stale) --------------------------------------
need_assemble=false
for f in "${ASSETS[@]}"; do
  if [[ ! -f "${OUT}/${f}" ]]; then
    need_assemble=true
    break
  fi
done
# Presence alone is not enough. The filenames do not change when a version pin
# moves, so a dir assembled before a bump looks complete while holding the old
# bytes, and atelet would reject them against the new shas only at runtime.
# Compare the stamp assemble.sh left with the one it would write now; a dir
# without one re-assembles.
if [[ "${need_assemble}" == "false" ]]; then
  want_stamp="$(assemble --print-stamp)"
  if [[ "$(cat "${OUT}/.asset-versions" 2>/dev/null)" != "${want_stamp}" ]]; then
    log "Asset set in ${OUT} is stale (version stamp mismatch); re-assembling."
    need_assemble=true
  fi
fi
if [[ "${need_assemble}" == "true" ]]; then
  log "Assembling micro-VM assets into ${OUT} (ARCH=${ARCH})..."
  assemble
else
  log "Assets already present in ${OUT}; skipping assemble."
fi

# --- 2. stage to the GCS asset bucket ---------------------------------------
log "Uploading assets to gs://${ASSET_BUCKET}/kata-assets/ ..."
for f in "${ASSETS[@]}"; do
  echo "   ${f}"
  run_gcloud storage cp "${OUT}/${f}" "gs://${ASSET_BUCKET}/kata-assets/${f}"
done

# --- 3. apply the cluster-wide microvm SandboxConfig ------------------------
log "Applying microvm SandboxConfig from ${MANIFEST_TEMPLATE}..."
sed -e "s|\${BUCKET_SCHEME}|gs|g" \
    -e "s|\${ASSET_BUCKET_NAME}|${ASSET_BUCKET}|g" \
    "${SUBSTRATE_ROOT}/${MANIFEST_TEMPLATE}" \
  | run_kubectl apply -f -

log "Done. Installed cluster-wide microvm SandboxConfig."
