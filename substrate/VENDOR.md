# Vendored snapshot of agent-substrate/substrate

- Source: https://github.com/agent-substrate/substrate
- Commit: 785a3bdfb5c22b896416f7a875bc5d0da7767f0d
- Vendored with: hack/vendor-substrate.sh

Do not edit files in this directory by hand. To update, run:

    hack/vendor-substrate.sh <substrate-checkout> <ref>

Only the paths needed to install Substrate on GKE are included (installer,
GCP bootstrap tool, manifests, and the Go source that ko builds the
control-plane images from). Upstream docs, e2e tests, and benchmarking are
intentionally excluded.
