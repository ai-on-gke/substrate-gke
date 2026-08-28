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

# Vendors a snapshot of agent-substrate/substrate into ./substrate.
#
# The upstream installer (cmd/ate-setup) builds the control-plane images from
# source with ko, so the snapshot must carry the Go source tree and manifests,
# not just YAML. Only committed content is copied (via git archive), so a dirty
# working tree in the source checkout never leaks in.
#
# Usage: hack/vendor-substrate.sh <substrate-checkout> [ref]
set -euo pipefail

SRC="${1:?usage: $0 <substrate-checkout> [ref]}"
REF="${2:-HEAD}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${REPO_ROOT}/substrate"

# The paths ate-setup and setup-gcp need at build or run time. Everything else
# upstream (docs, e2e, benchmarking, CI) is deliberately left out.
PATHS=(
  LICENSE
  go.mod
  go.sum
  .ko.yaml
  _LICENSES
  vendor
  cmd
  internal
  pkg
  manifests
  demos
  tools/setup-gcp
  hack/tools/ko
  hack/ate-dev-env.sh.example
)

COMMIT="$(git -C "${SRC}" rev-parse "${REF}")"

rm -rf "${DEST}"
mkdir -p "${DEST}"
git -C "${SRC}" archive "${COMMIT}" -- "${PATHS[@]}" | tar -x -C "${DEST}"

cat > "${DEST}/VENDOR.md" <<EOF
# Vendored snapshot of agent-substrate/substrate

- Source: https://github.com/agent-substrate/substrate
- Commit: ${COMMIT}
- Vendored with: hack/vendor-substrate.sh

Do not edit files in this directory by hand. To update, run:

    hack/vendor-substrate.sh <substrate-checkout> <ref>

Only the paths needed to install Substrate on GKE are included (installer,
GCP bootstrap tool, manifests, and the Go source that ko builds the
control-plane images from). Upstream docs, e2e tests, and benchmarking are
intentionally excluded.
EOF

printf '%s\n' "${COMMIT}" > "${DEST}/VENDORED_COMMIT"

echo "Vendored substrate@${COMMIT} into ${DEST}"
