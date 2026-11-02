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
  the revision you want to install, for `cmd/ate-setup`. It must be
  [v0.6.0](https://github.com/agent-substrate/substrate/releases/tag/v0.6.0) or later: since [#92](https://github.com/ai-on-gke/substrate-gke/pull/92), setup-gcp's IAM grants support only
  Substrate releases with per-actor snapshot credentials.

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
   buckets, the snapshot transfer service account, the IAM bindings and the
   monitoring dashboards:

   ```bash
   go -C tools/setup-gcp run . bootstrap
   ```

   On a fresh project this also creates the Workload Identity IAM grants that
   ate-api-server and atelet depend on. See
   [what `create iam` actually grants](../tools/setup-gcp/README.md#what-create-iam-actually-grants)
   to audit them or apply them by hand. atelet gets no storage role on the
   project or the snapshot bucket; see that section for how to remove grants
   an older setup left behind.

   If you bring your own cluster, its control plane and every node pool must
   run GKE 1.37 or later, which serves the PodCertificateRequest and
   ClusterTrustBundle APIs by default. `bootstrap` stops on a cluster with
   either below 1.37. See the [Create Cluster warning](../tools/setup-gcp/README.md#2-create-cluster).

4. From the Substrate checkout, deploy the control plane:

   ```bash
   go run ./cmd/ate-setup deploy ate-system --credential-provider='{"name":"k8s.io"}' \
     --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/atenet-client
   ```

   From Substrate v0.6.0, ate-setup refuses to deploy on GKE without
   `SNAPSHOT_LOCATION` and `SNAPSHOT_TRANSFER_GSA_NAME`; the environment file
   sets both. `--ingress-allowed-spiffe-ids` lists the clients the router lets
   reach actors, and `--credential-provider` selects the egress credential
   provider. See upstream's
   [operator-install.md](https://github.com/agent-substrate/substrate/blob/main/docs/operator-install.md)
   for both.

5. Deploy a sample application, also from the Substrate checkout:

   ```bash
   go run ./cmd/ate-setup deploy demo counter
   ```

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
export the same variables first:

```bash
<substrate-gke checkout>/tools/setup-gcp/teardown.sh [--all|--cluster-scoped]
```

* `--all` deletes everything, including the dashboards, the snapshot transfer
  service account and the project-level IAM bindings, which every Substrate
  cluster in the project shares.
* `--cluster-scoped` deletes only this cluster, its buckets and the bindings on
  them. Use it while other Substrate clusters still use the project.

Run it with no arguments for the individual steps. See
[Teardown](../tools/setup-gcp/README.md#teardown) for how it handles resources
that are already gone and failed steps. [`tools/cleanup-gcp`](../tools/cleanup-gcp)
wraps it with flags and a confirmation prompt.
