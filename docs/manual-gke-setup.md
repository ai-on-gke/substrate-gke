# Setting up GKE by hand

The installer runs these steps for you. Follow this page to run them
yourself, for example against a Substrate checkout you are developing in.

It replaces the "GKE Quickstart (Development)" and "Tearing down resources
(GCP)" sections of the agent-substrate/substrate README, which ran that
repository's copies of `tools/setup-gcp` and `hack/teardown.sh`. Both tools
now live in this repository
([agent-substrate/substrate#2304](https://github.com/agent-substrate/substrate/issues/2304)).

You need two checkouts:

* this repository, for `tools/setup-gcp` and `tools/setup-gcp/teardown.sh`;
* [agent-substrate/substrate](https://github.com/agent-substrate/substrate) at
  the revision you want to install, for `cmd/ate-setup`.

## Provision and deploy

1. Create and configure your environment file. setup-gcp, `teardown.sh` and
   ate-setup all read the same variables:

   ```bash
   cp tools/setup-gcp/ate-dev-env.sh.example .ate-dev-env.sh
   # Edit .ate-dev-env.sh to match your project and preferences, then source it:
   source .ate-dev-env.sh
   ```

2. Enable application-default credentials for gcloud:

   ```bash
   gcloud auth application-default login --project=${PROJECT_ID}
   ```

3. From the root of this repository, provision the GKE cluster, the GCS
   buckets, the snapshot transfer and node service accounts, the IAM bindings and the
   monitoring dashboards:

   ```bash
   go -C tools/setup-gcp run . bootstrap --create-repository
   ```

   `--create-repository` creates the Artifact Registry repository that the
   environment file's `KO_DOCKER_REPO` names, and lets the nodes pull from
   it. Step 4 pushes there when it builds the images from source. Leave the
   flag off if you install published images.

   On a fresh project this also creates the Workload Identity IAM grants that
   ate-api-server and atelet depend on. See
   [what `create iam` actually grants](../tools/setup-gcp/README.md#what-create-iam-actually-grants)
   to audit them or apply them by hand. `--atelet-snapshot-access` (on by
   default) keeps atelet's snapshot bucket roles, which Substrate releases up
   to v0.5.0 need; turn it off for later releases.

   If you bring your own cluster, its control plane and every node pool must
   run GKE 1.37 or later, which serves the PodCertificateRequest and
   ClusterTrustBundle APIs by default. `bootstrap` stops on a cluster with
   either below 1.37. See the [Create Cluster warning](../tools/setup-gcp/README.md#2-create-cluster).

4. From the Substrate checkout, deploy the control plane:

   ```bash
   go run ./cmd/ate-setup deploy ate-system --credential-provider='{"name":"k8s.io"}' \
     --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-demo-counter/sa/default,spiffe://cluster.local/ns/ate-demo-counter-microvm/sa/default
   ```

   From Substrate v0.6.0, ate-setup refuses to deploy on GKE without
   `SNAPSHOT_LOCATION` and `SNAPSHOT_TRANSFER_GSA_NAME`; the environment file
   sets both. `--credential-provider` selects the egress credential provider.
   `--ingress-allowed-spiffe-ids` lists the clients the router lets reach
   actors, and each deploy replaces the list. The IDs above are the counter
   demos' driver pods, which call their actors through the router. Replace
   them with your own clients' IDs
   (`spiffe://cluster.local/ns/<namespace>/sa/<service-account>`) if you skip
   the demos. See upstream's
   [operator-install.md](https://github.com/agent-substrate/substrate/blob/main/docs/guides/operator-install.md)
   for both flags.

   **Sandbox classes.** From Substrate v1.0.0, `deploy ate-system` installs
   both the `gvisor` and `microvm` SandboxConfigs by default. For `microvm`
   it downloads the guest assets for amd64 and arm64 and uploads them with
   `gcloud` to `gs://${BUCKET_NAME}-assets`, which `bootstrap` creates with
   read access for atelet. If the cluster runs only gVisor actors, add
   `--sandbox-classes=gvisor` to skip that. To add micro-VM later, run
   `go run ./cmd/ate-setup deploy sandboxconfig --sandbox-classes=gvisor,microvm`
   with the same flags as above. Every ate-setup deploy replaces the install
   record that upgrades start from, so pass the settings the install used.
   Substrate v1.0.0-rc.1 and earlier have no `--sandbox-classes` flag; stage
   micro-VM there with `hack/install-microvm-deps.sh --install`.

   **Images.** ate-setup builds the images from the checkout with `ko` and
   pushes them to `KO_DOCKER_REPO`. To install a published release instead,
   add `--image-repo ghcr.io/agent-substrate/substrate --image-tag <release>`,
   with the checkout at that release's commit.

5. Deploy a sample application, also from the Substrate checkout:

   ```bash
   go run ./cmd/ate-setup deploy demo counter
   ```

   On a cluster with a KVM-capable node pool (the environment file's
   `ENABLE_NESTED_VIRTUALIZATION=true` gives a new cluster one) and the
   `microvm` SandboxConfig, `deploy demo counter-microvm` runs the same demo
   in a micro-VM. Releases don't publish demo images, so with
   `--image-repo ghcr.io/agent-substrate/substrate` the demos fail to pull
   theirs. Run `deploy demo` without `--image-repo`, so ate-setup builds the
   demo image with `ko`.

## Individual steps

`go -C tools/setup-gcp run . --help` lists the steps `bootstrap` runs, which
can also run one at a time:

```bash
go -C tools/setup-gcp run . create cluster
go -C tools/setup-gcp run . create bucket
```

To run the PostgreSQL store backend on Cloud SQL, with IAM database
authentication and no passwords, see
[tools/setup-gcp/cloud-sql.md](../tools/setup-gcp/cloud-sql.md).

## Tear down

To remove only the control plane and keep the cluster, run this from the
Substrate checkout:

```bash
go run ./cmd/ate-setup delete ate-system
```

To delete the GCP resources, run `teardown.sh` from the directory that holds
`.ate-dev-env.sh` (it sources that file unless `NO_DEV_ENV=1` is set), or
export the same variables first. It needs `PROJECT_NUMBER` as well, which
the environment file from step 1 sets:

```bash
<substrate-gke checkout>/tools/setup-gcp/teardown.sh [--all|--cluster-scoped]
```

* `--all` deletes everything, including the dashboards and the snapshot
  transfer and node service accounts, which every Substrate cluster in the
  project shares.
* `--cluster-scoped` deletes only this cluster, its buckets and the bindings on
  them. Use it while other Substrate clusters still use the project.

Run it with no arguments for the individual steps. See
[Teardown](../tools/setup-gcp/README.md#teardown) for how it handles resources
that are already gone and failed steps. [`tools/cleanup-gcp`](../tools/cleanup-gcp)
wraps it with flags and a confirmation prompt.
