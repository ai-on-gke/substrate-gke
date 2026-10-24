#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Orchestrates the end-to-end build, shrinking, compression, and optional GCS
# upload of slim Kata microVM assets (rootfs.img + kata-agent.slim) for benchmarking.
#
# Pipeline:
#   1. Download official Kata release tarball (kata-static-${KATA_VER}-${ARCH}.tar.zst)
#   2. Extract upstream rootfs.img (256 MiB)
#   3. Build slim kata-agent (slim-agent.sh: 30.6 MB -> 18.6 MB, saved as kata-agent.slim and patched into rootfs.img)
#   4. Slim rootfs.img to minimal outer VM OS (slim-rootfs.sh: 256 MiB -> 35 MiB)
#   5. Compress assets with zstd -19 and generate SHA256SUMS
#   6. Optionally upload compressed assets + SHA256SUMS to a private GCS bucket

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

KATA_VER="${KATA_VER:-$(tr -d '[:space:]' < "${SCRIPT_DIR}/KATA_VERSION")}"
ARCH="${ARCH:-amd64}"
ASSET_REV="${ASSET_REV:-1}"
OUT_DIR="${OUT_DIR:-$(pwd)/_output/microvm-assets/${ARCH}}"
GCS_BUCKET="${GCS_BUCKET:-}"
GCS_PREFIX="${GCS_PREFIX:-microvm-assets/${KATA_VER}-slim.${ASSET_REV}/${ARCH}}"
PUSH="${PUSH:-false}"

case "${ARCH}" in
  amd64|x86_64)
    ARCH="amd64"
    ;;
  arm64|aarch64)
    ARCH="arm64"
    ;;
  *)
    echo "Unsupported ARCH: ${ARCH}" >&2
    exit 1
    ;;
esac

mkdir -p "${OUT_DIR}"

TARBALL="kata-static-${KATA_VER}-${ARCH}.tar.zst"
KATA_URL="https://github.com/kata-containers/kata-containers/releases/download/${KATA_VER}/${TARBALL}"

echo "=== Step 1: Downloading Kata ${KATA_VER} static release (${ARCH}) ==="
if [[ ! -f "${OUT_DIR}/${TARBALL}" ]]; then
  curl -fSL "${KATA_URL}" -o "${OUT_DIR}/${TARBALL}"
fi

echo "=== Step 2: Extracting upstream rootfs.img ==="
EXTRACT_TMP="$(mktemp -d)"
trap 'rm -rf "${EXTRACT_TMP}"' EXIT

tar --use-compress-program="zstd -d" -xf "${OUT_DIR}/${TARBALL}" -C "${EXTRACT_TMP}" \
  --wildcards \
  --exclude='*nvidia*' \
  './opt/kata/share/kata-containers/kata-containers.img' \
  './opt/kata/share/kata-containers/kata-ubuntu-*.image'

KATA_SHARE="${EXTRACT_TMP}/opt/kata/share/kata-containers"
ROOTFS_LINK="$(readlink "${KATA_SHARE}/kata-containers.img")"

cp -f "${KATA_SHARE}/${ROOTFS_LINK}" "${OUT_DIR}/rootfs.img"

echo "=== Step 3: Building slim kata-agent and patching rootfs.img (${KATA_VER}, ${ARCH}) ==="
ARCH="${ARCH}" KATA_VER="${KATA_VER}" IMAGE="${OUT_DIR}/rootfs.img" \
  "${SCRIPT_DIR}/slim-agent.sh"

echo "=== Step 4: Slimming rootfs.img to minimal allowlisted outer VM OS ==="
ARCH="${ARCH}" "${SCRIPT_DIR}/slim-rootfs.sh" "${OUT_DIR}/rootfs.img"

echo "=== Step 5: Generating SHA256SUMS and compressing with zstd ==="
(
  cd "${OUT_DIR}"
  sha256sum kata-agent.slim rootfs.img > SHA256SUMS
  zstd -19 -T0 -f rootfs.img -o rootfs.img.zst
  zstd -19 -T0 -f kata-agent.slim -o kata-agent.slim.zst
  sha256sum kata-agent.slim.zst rootfs.img.zst >> SHA256SUMS
)

echo "=== Asset Summary (${OUT_DIR}) ==="
ls -lh "${OUT_DIR}/kata-agent.slim" "${OUT_DIR}/kata-agent.slim.zst" "${OUT_DIR}/rootfs.img" "${OUT_DIR}/rootfs.img.zst"
cat "${OUT_DIR}/SHA256SUMS"

if [[ "${PUSH}" == "true" || -n "${GCS_BUCKET}" ]]; then
  if [[ -z "${GCS_BUCKET}" ]]; then
    echo "error: GCS_BUCKET (e.g. gs://my-bucket) is required when PUSH=true" >&2
    exit 1
  fi
  BUCKET_URI="gs://${GCS_BUCKET#gs://}"
  DEST_URI="${BUCKET_URI%/}/${GCS_PREFIX#/}"
  echo "=== Step 6: Uploading slim assets to ${DEST_URI}/ ==="
  gcloud storage cp \
    "${OUT_DIR}/rootfs.img.zst" \
    "${OUT_DIR}/kata-agent.slim.zst" \
    "${OUT_DIR}/SHA256SUMS" \
    "${DEST_URI}/"
  echo "=== Successfully uploaded assets to ${DEST_URI}/ ==="
fi
