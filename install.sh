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

set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/ai-on-gke/substrate-gke.git}"
REPO_BRANCH="${REPO_BRANCH:-main}"

main() {
    # If standard input is piped (e.g. curl ... | bash), the interactive
    # Bubble Tea TUI still needs keyboard input — but the shell's own stdin
    # must be left alone: bash reads this script from that pipe, so an
    # `exec < /dev/tty` would make bash read the REST of the script from the
    # terminal once main returns, hanging the session and executing whatever
    # is typed. The tty is handed to the installer process alone, below.
    # `-r /dev/tty` is not enough to know the tty is usable (the node exists
    # without a controlling terminal in CI and containers), so actually try
    # opening it.
    local tty_in=""
    if [ ! -t 0 ] && (exec < /dev/tty) 2>/dev/null; then
        tty_in="/dev/tty"
    fi

    # Check for basic required prerequisites
    if ! command -v git >/dev/null 2>&1; then
        echo "error: 'git' is required to install substrate-gke." >&2
        echo "Please install git and try again." >&2
        exit 1
    fi

    if ! command -v go >/dev/null 2>&1; then
        echo "error: 'go' (Go toolchain) is required to run the substrate-gke installer." >&2
        echo "Please install Go (https://go.dev/doc/install) and try again." >&2
        exit 1
    fi

    # Determine target repository directory
    local script_dir=""
    if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    fi

    local target_dir
    if [ -n "${script_dir}" ] && [ -f "${script_dir}/installer/main.go" ]; then
        target_dir="${script_dir}"
    elif [ -f "./installer/main.go" ] && [ -f "./Makefile" ]; then
        target_dir="$(pwd)"
    else
        target_dir="${SUBSTRATE_DIR:-$HOME/.substrate-gke}"
        if [ -d "${target_dir}/.git" ]; then
            echo "==> Existing installation found at ${target_dir}. Updating..."
            git -C "${target_dir}" fetch --quiet origin "${REPO_BRANCH}"
            git -C "${target_dir}" checkout --quiet "${REPO_BRANCH}"
            git -C "${target_dir}" pull --quiet --ff-only origin "${REPO_BRANCH}" || true
        else
            echo "==> Cloning substrate-gke into ${target_dir}..."
            mkdir -p "$(dirname "${target_dir}")"
            git clone --quiet --depth 1 --branch "${REPO_BRANCH}" "${REPO_URL}" "${target_dir}"
        fi
    fi

    echo "==> Launching substrate-gke installer..."
    launch() {
        if command -v make >/dev/null 2>&1 && [ "$#" -eq 0 ]; then
            make -C "${target_dir}" run
        else
            (cd "${target_dir}/installer" && go run . "$@")
        fi
    }
    if [ -n "${tty_in}" ]; then
        launch "$@" < "${tty_in}"
    else
        launch "$@"
    fi
}

main "$@"
