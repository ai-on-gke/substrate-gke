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

# Runs one Go module's tests in a network namespace with only loopback, so a
# test that reaches outside the machine fails. Needs sudo, which
# GitHub-hosted runners have.
#
# installer/internal/ui is left out: its wizard walks wait on real
# cursor-blink and spinner timers and take about 8.5 minutes (#103).
#
# Usage: .github/workflows/test-offline.sh <module>

set -euo pipefail

module="$1"
cd "${module}"

# Download modules and build the test binaries while the network is up.
go mod download
mapfile -t pkgs < <(go list ./... | grep -v '/installer/internal/ui$')
go test -run '^$' "${pkgs[@]}" >/dev/null

sudo unshare --net -- bash -c '
  set -euo pipefail
  ip link set lo up
  user="$1" path="$2" home="$3"
  shift 3
  exec sudo -u "${user}" env PATH="${path}" HOME="${home}" GOTOOLCHAIN=local GOPROXY=off \
    go test "$@"
' _ "$(id -un)" "${PATH}" "${HOME}" "${pkgs[@]}"
