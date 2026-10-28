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

# Deletes what `setup-gcp bootstrap` creates, in reverse order. Moved here
# from agent-substrate/substrate's hack/teardown.sh (upstream commit f69f41d3)
# together with tools/setup-gcp (agent-substrate/substrate#2304), so the two
# halves of the lifecycle change together.
#
# A resource that is already gone counts as deleted, so re-running after a
# partial teardown is safe. Any other failure is reported: the remaining steps
# still run, and the script exits 1 at the end naming every failed step.

# Source the environment variables. The file is optional: an installer (or a
# user pasting a one-liner) can pass the same variables through the
# environment instead, which also makes teardown possible on a machine that
# never had a dev-env file. NO_DEV_ENV=1 skips the file even when present, so
# a caller's values cannot be overridden by a .ate-dev-env.sh in the working
# directory that names another project or cluster.
if [ -z "${NO_DEV_ENV:-}" ] && [ -f .ate-dev-env.sh ]; then
  source .ate-dev-env.sh
fi
# No cluster-admin precheck here: every step below talks to GCP, not to the
# cluster, and requiring a live kubectl context would block tearing down a
# cluster that is already half-gone.

# require checks that each named variable is set, so a step fails up front
# with the variable's name rather than mid-deletion with a gcloud error. Each
# step declares only what it uses: deleting a bucket must not demand a node
# pool name. --all and --cluster-scoped check the union before their first
# step.
require() {
  for var in "$@"; do
    if [ -z "${!var:-}" ]; then
      echo "${var} is not set; export it (see tools/setup-gcp/README.md, Teardown)" >&2
      exit 1
    fi
  done
}

# failed lists the steps that hit a failure other than "already gone".
failed=()

scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT

# already_gone reports whether gcloud's error output means the resource or
# binding does not exist, which is the state teardown wants. These are the
# forms gcloud uses: GKE's code=404/Not found, Cloud Storage's "not found: 404",
# "HTTPError 404" and "matched no objects", IAM's "binding ... not found!", and
# Monitoring's NOT_FOUND. Only lines gcloud itself prints as "ERROR: ..."
# count, so other text in the log, such as the shell's "command not found",
# is not read as a deleted resource.
already_gone() {
  grep -qiE '^ERROR: .*(not found|NOT_FOUND|code=404|HTTPError 404|matched no objects)' "$1"
}

# bucket_gone BUCKET reports whether gs://BUCKET no longer exists. The bucket
# steps check this first rather than relying on each command's error: gcloud
# words a missing bucket differently by command and transport ("gs://B not
# found: 404." or "HTTPError 404: The specified bucket does not exist."). Only
# a 404 counts. Any other describe failure is shown and the bucket steps run
# anyway, so they report their own errors. The answer is cached per bucket for
# the run, in a scratch file.
bucket_gone() {
  local bucket="$1"
  local state_file="${scratch}/bucket-state-${bucket}"
  if [ ! -f "${state_file}" ]; then
    if gcloud storage buckets describe "gs://${bucket}" \
        --project="${PROJECT_ID}" --format="value(name)" \
        >/dev/null 2>"${scratch}/describe-err"; then
      echo "present" >"${state_file}"
    elif grep -qE '^ERROR: .*(not found: 404|HTTPError 404)' "${scratch}/describe-err"; then
      echo "gone" >"${state_file}"
    else
      cat "${scratch}/describe-err" >&2
      echo "unknown" >"${state_file}"
    fi
  fi
  [ "$(cat "${state_file}")" = "gone" ]
}

# gcloud_step STEP ARGS... runs `gcloud ARGS...` with its output shown as it
# runs, and sets last_status to "ok", "gone" (failed because the resource is
# already deleted), or "failed" (anything else; STEP is added to failed).
last_status=""
gcloud_step() {
  local step="$1"
  shift
  local log="${scratch}/last"
  if gcloud "$@" 2>&1 | tee "${log}"; then
    last_status="ok"
  elif already_gone "${log}"; then
    last_status="gone"
    echo "(already deleted)"
  else
    last_status="failed"
    failed+=("${step}")
  fi
}

# --- Helper Functions ---

# transfer_service_account prints the snapshot transfer service account's
# email, from SNAPSHOT_TRANSFER_GSA_NAME as setup-gcp and ate-setup build it.
transfer_service_account() {
  echo "${SNAPSHOT_TRANSFER_GSA_NAME:-ate-snapshot-transfer}@${PROJECT_ID}.iam.gserviceaccount.com"
}

function usage() {
  echo "Usage: $0 [options]"
  echo "Options:"
  echo "  --delete-iam-policy-bindings          Delete bucket IAM policy bindings for atelet, ate-api-server and the transfer account"
  echo "  --delete-snapshot-bucket              Delete the snapshot bucket and the microVM asset bucket"
  echo "  --delete-gvisor-node-pool             Delete gVisor node pool"
  echo "  --delete-cluster                      Delete GKE cluster"
  echo "  --cluster-scoped                      Run the steps above (reverse order of setup)"
  echo "Project-wide options. These affect every Substrate cluster in the project:"
  echo "  --delete-dashboards                   Delete the Substrate monitoring dashboards"
  echo "  --delete-transfer-service-account     Delete the snapshot transfer service account"
  echo "  --revoke-atelet-permissions           Revoke atelet's project-level IAM bindings"
  echo "  --revoke-gke-node-permissions         Revoke GKE nodes permission to pull images, on every cluster that uses the default compute service account"
  echo "  --all                                 Run every step above (reverse order of setup)"
  echo "  --delete-repository                   Delete the Artifact Registry repository and its images (not part of --all)"
  exit 1
}

# --- Teardown Functions ---

# Revoke GKE Node Permissions (Reverse of grant_gke_node_permissions)
revoke_gke_node_permissions() {
  require PROJECT_ID PROJECT_NUMBER
  echo "Revoking GKE node permissions..."
  local role
  for role in roles/storage.objectViewer roles/artifactregistry.reader; do
    gcloud_step "revoke GKE node permissions (${role})" \
      projects remove-iam-policy-binding "${PROJECT_ID}" \
      --member="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com" \
      --role="${role}" \
      --condition=None \
      --quiet
  done
}

# Revoke Atelet's project-level bindings (Reverse of grant_atelet_permissions)
revoke_atelet_permissions() {
  require PROJECT_ID PROJECT_NUMBER
  echo "Revoking atelet project-level permissions..."
  local member="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/ate-system/sa/atelet"
  local role
  for role in roles/storage.objectAdmin roles/artifactregistry.reader; do
    gcloud_step "revoke atelet permissions (${role})" \
      projects remove-iam-policy-binding "${PROJECT_ID}" \
      --member="${member}" \
      --role="${role}" \
      --condition=None \
      --quiet
  done
}

# Delete Monitoring Dashboards (Reverse of create_monitoring_dashboards)
delete_dashboards() {
  require PROJECT_ID
  echo "Deleting Substrate monitoring dashboards..."
  # Matched by display name, since setup only records names in its JSON.
  local names=(
    "Substrate Snapshot Size & QPS"
    "Substrate Routing & E2E Latency"
    "Substrate gRPC Server — latency / QPS / errors"
  )
  local display_name dashboard found
  for display_name in "${names[@]}"; do
    # A failed list must not read as "no dashboards to delete".
    if ! found=$(gcloud monitoring dashboards list \
        --project="${PROJECT_ID}" \
        --filter="displayName=\"${display_name}\"" \
        --format="value(name)" 2>"${scratch}/list-err"); then
      cat "${scratch}/list-err" >&2
      failed+=("delete dashboards (listing \"${display_name}\")")
      continue
    fi
    for dashboard in ${found}; do
      gcloud_step "delete dashboard \"${display_name}\"" \
        monitoring dashboards delete "${dashboard}" \
        --project="${PROJECT_ID}" \
        --quiet
    done
  done
}

# Delete IAM Policy Bindings for the buckets (Reverse of create_iam_policy_bindings)
delete_iam_policy_bindings() {
  require PROJECT_ID PROJECT_NUMBER BUCKET_NAME
  echo "Deleting IAM policy bindings for the buckets..."
  local wi="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/ate-system/sa"
  local asset_bucket="${BUCKET_NAME}-assets"
  local subject role
  if bucket_gone "${BUCKET_NAME}"; then
    echo "(gs://${BUCKET_NAME} already deleted; its bindings went with it)"
  else
    # atelet's snapshot bucket roles exist only on installs made with
    # --atelet-snapshot-access; a missing binding counts as already deleted.
    for subject in atelet ate-api-server; do
      for role in roles/storage.objectAdmin roles/storage.bucketViewer; do
        gcloud_step "delete bucket IAM binding (${subject}, ${role})" \
          storage buckets remove-iam-policy-binding "gs://${BUCKET_NAME}" \
          --member="${wi}/${subject}" \
          --role="${role}" \
          --quiet
      done
    done
    local transfer
    transfer="serviceAccount:$(transfer_service_account)"
    for role in roles/storage.objectAdmin roles/storage.bucketViewer; do
      gcloud_step "delete bucket IAM binding (transfer account, ${role})" \
        storage buckets remove-iam-policy-binding "gs://${BUCKET_NAME}" \
        --member="${transfer}" \
        --role="${role}" \
        --quiet
    done
  fi
  if bucket_gone "${asset_bucket}"; then
    echo "(gs://${asset_bucket} already deleted; its bindings went with it)"
  else
    gcloud_step "delete asset bucket IAM binding (atelet, roles/storage.objectViewer)" \
      storage buckets remove-iam-policy-binding "gs://${asset_bucket}" \
      --member="${wi}/atelet" \
      --role=roles/storage.objectViewer \
      --quiet
  fi
}

# Delete the snapshot transfer service account. Deleting it drops the token
# creator grants on it.
delete_transfer_service_account() {
  require PROJECT_ID
  echo "Deleting the snapshot transfer service account..."
  gcloud_step "delete transfer service account" \
    iam service-accounts delete "$(transfer_service_account)" \
    --project="${PROJECT_ID}" --quiet
}

# Delete the snapshot and microVM asset buckets (Reverse of create_buckets)
delete_snapshot_bucket() {
  require PROJECT_ID BUCKET_NAME
  echo "Deleting snapshot and asset buckets..."
  local bucket kind
  for kind in snapshot asset; do
    bucket="${BUCKET_NAME}"
    if [ "${kind}" = "asset" ]; then
      bucket="${BUCKET_NAME}-assets"
    fi
    if bucket_gone "${bucket}"; then
      echo "(gs://${bucket} already deleted)"
      continue
    fi
    gcloud_step "empty ${kind} bucket" \
      storage rm --recursive "gs://${bucket}/**" --project="${PROJECT_ID}" --quiet
    gcloud_step "delete ${kind} bucket" \
      storage buckets delete "gs://${bucket}" --project="${PROJECT_ID}" --quiet
  done
}

# Delete gVisor Node Pool (Reverse of create_gvisor_node_pool)
delete_gvisor_node_pool() {
  require PROJECT_ID CLUSTER_NAME CLUSTER_LOCATION NODE_POOL_NAME
  echo "Deleting gVisor node pool..."
  gcloud_step "delete node pool" \
    container node-pools delete "${NODE_POOL_NAME}" \
    --cluster="${CLUSTER_NAME}" \
    --location="${CLUSTER_LOCATION}" \
    --project="${PROJECT_ID}" \
    --quiet
}

# Delete Cluster (Reverse of create_cluster)
delete_cluster() {
  require PROJECT_ID CLUSTER_NAME CLUSTER_LOCATION
  echo "Deleting GKE cluster..."
  gcloud_step "delete cluster" \
    container clusters delete "${CLUSTER_NAME}" \
    --location="${CLUSTER_LOCATION}" \
    --project="${PROJECT_ID}" \
    --quiet
  if [ "${last_status}" != "gone" ]; then
    return 0
  fi
  # GKE answers a wrong --location with the same NOT_FOUND as a deleted
  # cluster. Look for the name in every location before calling it deleted.
  local elsewhere
  if ! elsewhere=$(gcloud container clusters list \
      --project="${PROJECT_ID}" \
      --filter="name=${CLUSTER_NAME}" \
      --format="value(location)" 2>"${scratch}/list-err"); then
    cat "${scratch}/list-err" >&2
    # A caller may hold container.clusters.delete without
    # container.clusters.list. Failing then would fail every re-run for a
    # cluster that is gone, so warn instead; any other list error still fails.
    if grep -q 'PERMISSION_DENIED' "${scratch}/list-err"; then
      echo "WARNING: could not verify that ${CLUSTER_NAME} is absent from other locations (listing clusters was denied); treating it as deleted. If ${CLUSTER_LOCATION} is the wrong location, the cluster is still running." >&2
    else
      failed+=("delete cluster (could not check other locations for ${CLUSTER_NAME})")
    fi
    return 0
  fi
  if [ -n "${elsewhere}" ]; then
    elsewhere="$(echo ${elsewhere} | tr ' ' ',')"
    echo "Cluster ${CLUSTER_NAME} is not in ${CLUSTER_LOCATION} but exists in: ${elsewhere}" >&2
    failed+=("delete cluster (${CLUSTER_NAME} is in ${elsewhere}, not ${CLUSTER_LOCATION})")
  fi
}

validate_repository_config() {
  require PROJECT_ID GCE_REGION
  ARTIFACT_REGISTRY_REPOSITORY="${ARTIFACT_REGISTRY_REPOSITORY:-ate-images}"
  if [[ ! "${ARTIFACT_REGISTRY_REPOSITORY}" =~ ^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
    echo "ARTIFACT_REGISTRY_REPOSITORY must be 1-63 lowercase letters, digits or hyphens, starting with a letter and ending with a letter or digit" >&2
    exit 1
  fi
}

delete_repository() {
  local repository="projects/${PROJECT_ID}/locations/${GCE_REGION}/repositories/${ARTIFACT_REGISTRY_REPOSITORY}"
  echo "Deleting Artifact Registry repository ${repository} and its images..."
  gcloud_step "delete Artifact Registry repository" \
    artifacts repositories delete "${ARTIFACT_REGISTRY_REPOSITORY}" \
    --project="${PROJECT_ID}" \
    --location="${GCE_REGION}" \
    --quiet
}

# --- Main Logic ---
if [ "$#" -eq 0 ]; then
  usage
fi

# Every step shells out to gcloud. Without it each call would fail with the
# shell's "command not found", so stop before the first step instead.
if ! command -v gcloud >/dev/null 2>&1; then
  echo "gcloud not found on PATH; teardown needs the Google Cloud SDK (https://cloud.google.com/sdk/docs/install)" >&2
  exit 1
fi

# buckets_and_cluster deletes the buckets, the node pool and the cluster, for
# --cluster-scoped and --all. Deleting the cluster removes its node pools, so
# neither insists on a pool name a caller (e.g. an installer) may not track.
buckets_and_cluster=(delete_snapshot_bucket)
if [ -n "${NODE_POOL_NAME:-}" ]; then
  buckets_and_cluster+=(delete_gvisor_node_pool)
fi
buckets_and_cluster+=(delete_cluster)

steps=()
delete_repository_requested=false
all_requested=false
cluster_scoped_requested=false
for arg in "$@"; do
  case "${arg}" in
    --revoke-gke-node-permissions) steps+=(revoke_gke_node_permissions) ;;
    --revoke-atelet-permissions) steps+=(revoke_atelet_permissions) ;;
    --delete-iam-policy-bindings) steps+=(delete_iam_policy_bindings) ;;
    --delete-transfer-service-account) steps+=(delete_transfer_service_account) ;;
    --delete-snapshot-bucket) steps+=(delete_snapshot_bucket) ;;
    --delete-gvisor-node-pool) steps+=(delete_gvisor_node_pool) ;;
    --delete-cluster) steps+=(delete_cluster) ;;
    --delete-dashboards) steps+=(delete_dashboards) ;;
    --delete-repository)
      steps+=(delete_repository)
      delete_repository_requested=true
      ;;
    --cluster-scoped)
      cluster_scoped_requested=true
      steps+=(delete_iam_policy_bindings "${buckets_and_cluster[@]}")
      ;;
    --all)
      all_requested=true
      steps+=(delete_dashboards delete_iam_policy_bindings delete_transfer_service_account revoke_atelet_permissions revoke_gke_node_permissions "${buckets_and_cluster[@]}")
      ;;
    *) usage ;;
  esac
done
if ${delete_repository_requested}; then
  validate_repository_config
fi
if ${all_requested} || ${cluster_scoped_requested}; then
  require PROJECT_ID PROJECT_NUMBER BUCKET_NAME CLUSTER_NAME CLUSTER_LOCATION
fi
for step in "${steps[@]}"; do
  "${step}"
done

if [ "${#failed[@]}" -gt 0 ]; then
  echo >&2
  echo "Teardown incomplete; see the errors above. Resources from these steps may still exist (and bill):" >&2
  for step in "${failed[@]}"; do
    echo "  FAILED: ${step}" >&2
  done
  exit 1
fi
