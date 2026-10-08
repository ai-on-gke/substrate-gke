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
# pool name. --all checks the union before its first step.
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
# forms gcloud uses: GKE's code=404/Not found, Cloud Storage's "not found: 404"
# and "matched no objects", IAM's "binding ... not found!", and Monitoring's
# NOT_FOUND.
already_gone() {
  grep -qiE 'not found|NOT_FOUND|code=404|matched no objects' "$1"
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
function usage() {
  echo "Usage: $0 [options]"
  echo "Options:"
  echo "  --revoke-gke-node-permissions         Revoke GKE nodes permission to pull images"
  echo "  --revoke-atelet-permissions           Revoke atelet's project-level IAM bindings"
  echo "  --delete-iam-policy-bindings          Delete bucket IAM policy bindings for atelet and ate-api-server"
  echo "  --delete-snapshot-bucket              Delete snapshot bucket"
  echo "  --delete-gvisor-node-pool             Delete gVisor node pool"
  echo "  --delete-cluster                      Delete GKE cluster"
  echo "  --delete-dashboards                   Delete the Substrate monitoring dashboards"
  echo "  --all                                 Run all teardown steps (reverse order of setup)"
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

# Delete IAM Policy Bindings for Bucket (Reverse of create_iam_policy_bindings)
delete_iam_policy_bindings() {
  require PROJECT_ID PROJECT_NUMBER BUCKET_NAME
  echo "Deleting IAM policy bindings for bucket..."
  local wi="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/ate-system/sa"
  local subject role
  for subject in atelet ate-api-server; do
    for role in roles/storage.objectAdmin roles/storage.bucketViewer; do
      gcloud_step "delete bucket IAM binding (${subject}, ${role})" \
        storage buckets remove-iam-policy-binding "gs://${BUCKET_NAME}" \
        --member="${wi}/${subject}" \
        --role="${role}" \
        --quiet
    done
  done
}

# Delete Snapshot Bucket (Reverse of create_snapshot_bucket)
delete_snapshot_bucket() {
  require PROJECT_ID BUCKET_NAME
  echo "Deleting snapshot bucket..."
  gcloud_step "empty snapshot bucket" \
    storage rm --recursive "gs://${BUCKET_NAME}/**" --project="${PROJECT_ID}" --quiet
  gcloud_step "delete snapshot bucket" \
    storage buckets delete "gs://${BUCKET_NAME}" --project="${PROJECT_ID}" --quiet
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
    failed+=("delete cluster (could not check other locations for ${CLUSTER_NAME})")
    return 0
  fi
  if [ -n "${elsewhere}" ]; then
    elsewhere="$(echo ${elsewhere} | tr ' ' ',')"
    echo "Cluster ${CLUSTER_NAME} is not in ${CLUSTER_LOCATION} but exists in: ${elsewhere}" >&2
    failed+=("delete cluster (${CLUSTER_NAME} is in ${elsewhere}, not ${CLUSTER_LOCATION})")
  fi
}

# --- Main Logic ---
if [ "$#" -eq 0 ]; then
  usage
fi

while [[ "$#" -gt 0 ]]; do
  case $1 in
    --revoke-gke-node-permissions) revoke_gke_node_permissions ;;
    --revoke-atelet-permissions) revoke_atelet_permissions ;;
    --delete-iam-policy-bindings) delete_iam_policy_bindings ;;
    --delete-snapshot-bucket) delete_snapshot_bucket ;;
    --delete-gvisor-node-pool) delete_gvisor_node_pool ;;
    --delete-cluster) delete_cluster ;;
    --delete-dashboards) delete_dashboards ;;
    --all)
      # Check every variable the steps below use before any of them runs,
      # so a missing one cannot stop the teardown halfway through.
      require PROJECT_ID PROJECT_NUMBER BUCKET_NAME CLUSTER_NAME CLUSTER_LOCATION
      delete_dashboards
      delete_iam_policy_bindings
      revoke_atelet_permissions
      revoke_gke_node_permissions
      delete_snapshot_bucket
      # Deleting the cluster removes its node pools, so --all does not insist
      # on a pool name a caller (e.g. an installer) may not track.
      if [ -n "${NODE_POOL_NAME:-}" ]; then
        delete_gvisor_node_pool
      else
        echo "NODE_POOL_NAME not set; skipping node pool deletion (the cluster deletion removes its pools)"
      fi
      delete_cluster
      ;;
    *) usage ;;
  esac
  shift
done

if [ "${#failed[@]}" -gt 0 ]; then
  echo >&2
  echo "Teardown incomplete; see the errors above. Resources from these steps may still exist (and bill):" >&2
  for step in "${failed[@]}"; do
    echo "  FAILED: ${step}" >&2
  done
  exit 1
fi
