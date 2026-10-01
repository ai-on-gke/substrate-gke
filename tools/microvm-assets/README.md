# Slim Kata MicroVM Assets Builder

This directory contains the build scripts to compile, prune, and stage slim **Kata Containers microVM guest assets** (`rootfs.img` and `kata-agent.slim`) for benchmarking and cluster assembly.

Built assets (`rootfs.img.zst`, `kata-agent.slim.zst`, and `SHA256SUMS`) can be uploaded directly to a private GCS bucket (`GCS_BUCKET=gs://...`) so benchmark runs and [`agent-substrate/substrate`](https://github.com/agent-substrate/substrate) (`hack/microvm-assets/assemble.sh`) can swap in the slimmed `rootfs.img` and `kata-agent` directly.

---

## What Optimization Does This Perform?

| Asset / Metric | Stock Kata | Slimmed Asset (`substrate-gke`) | Delta / Improvement |
| :--- | :--- | :--- | :--- |
| **`/usr/bin/kata-agent` binary** | `30.63 MB` (`30,626,088 B`) | **`18.59 MB` (`18,593,232 B`)** | **-12.04 MB (-39.3%)** |
| **Guest `rootfs.img` (ext4 raw)** | `256.0 MiB` (`268,435,456 B`) | **`35.0 MiB` (`36,700,160 B`)** | **-221.0 MiB (-86.3%)** |
| **Compressed `rootfs.img.zst` (`zstd -19`)** | `34.2 MiB` | **`10.2 MiB`** | **-70.2% network payload** |
| **Golden Snapshot RAM (`memory-ranges.zstd`)** | `27.7 MiB` | **`24.3 MiB`** | **-3.4 MiB (-12.3%)** |
| **End-to-End GKE Warm Restore Latency** | `285.2 ms` | **`248.6 ms`** (min `216.3 ms`) | **-36.6 ms (-12.8%)** |
| **Node Asset Prewarm (`atelet` GCS pull)** | `2.95 s` | **`0.71 s`** | **-2.24 s (-75.9%)** |

### Requirements

- **Cloud Hypervisor `>= v53`**: Because the slimmed outer VM rootfs omits `systemd` and `chronyd` and runs `/usr/bin/kata-agent` directly as PID 1, it requires Cloud Hypervisor `>= v53` so the VMM advances the guest clock on snapshot restore.

### Two-Stage Build Pipeline

1. **Stage 1: `slim-agent.sh` (Rust Compilation with Slim Feature Flags)**
   - Clones `kata-containers/kata-containers` at the version pinned in [`KATA_VERSION`](./KATA_VERSION) (`src/agent`).
   - Disables `AGENT_POLICY` (Open Policy Agent `regorus` engine) and `INIT_DATA` (Confidential Containers Trustee attestation) while keeping `SECCOMP=yes` (`LIBC=gnu` dynamic glibc ELF).
   - Applies `codegen-units = 1`, `lto = true`, `opt-level = s`, and `strip --strip-all`, saving `kata-agent.slim` and patching `/usr/bin/kata-agent` inside `rootfs.img` via `debugfs`.

2. **Stage 2: `slim-rootfs.sh` (Allowlist Rootfs Reconstruction & Partition Compaction)**
   - Extracts `/usr/bin/kata-agent` (already slimmed by `slim-agent.sh`), the 11 diagnostic and shell binaries (`sh`, `dash`, `bash`, `ip`, `ls`, `head`, `grep`, `cat`, `mount`, `umount`, `ps`), `iptables`/`xtables`, and all their `ldd`-resolved shared libraries into a minimal outer VM OS rootfs.
   - Formats a compact journal-less `ext4` partition (`mkfs.ext4 -O ^has_journal -d`, with `tune2fs -o journal_data_ordered` so `rootflags=data=ordered` mounts cleanly) inside a privileged `ubuntu:24.04` container and wraps it in a DOS/MBR disk image (`256 MiB` $\rightarrow$ `35 MiB`).

---

## Directory Structure

| File | Description |
| :--- | :--- |
| [`KATA_VERSION`](./KATA_VERSION) | Single source of truth for the pinned Kata Containers version (`4.1.0`). |
| [`slim-agent.sh`](./slim-agent.sh) | Builds the slim `kata-agent` (`AGENT_POLICY=no`, `INIT_DATA=no`), writes `kata-agent.slim`, and patches `/usr/bin/kata-agent` via `debugfs`. |
| [`slim-rootfs.sh`](./slim-rootfs.sh) | Rebuilds `rootfs.img` containing only `/usr/bin/kata-agent`, `DebugConsoleDump` utilities, `iptables`, and `ldd` shared libraries. |
| [`build-and-push.sh`](./build-and-push.sh) | End-to-end orchestrator that downloads upstream `kata-static`, runs Stages 1 & 2, compresses `rootfs.img` and `kata-agent.slim` with `zstd -19`, writes `SHA256SUMS`, and optionally uploads to a private GCS bucket. |

---

## Usage

### 1. Build Locally

```bash
make microvm-assets-build
```

Outputs are written to `_output/microvm-assets/<arch>/`:
- `rootfs.img` (`35 MiB` raw ext4) & `rootfs.img.zst` (`~10.2 MiB`)
- `kata-agent.slim` (`18.6 MB` dynamic glibc ELF) & `kata-agent.slim.zst`
- `SHA256SUMS`

### 2. Build & Upload to a Private GCS Bucket

```bash
make microvm-assets-push GCS_BUCKET=gs://your-private-benchmark-bucket
```


