# Cloud Monitoring dashboards

Google Cloud Monitoring dashboard definitions for ATE. They turn the raw
`prometheus.googleapis.com/...` metrics that ATE emits into readable
per-method / per-stage latency / throughput / error views.

| File | Shows |
|------|-------|
| `ate-grpc-dashboard.json` | ateapi & atelet gRPC latency (p50/p95/p99), request rate, and error rate, by method |
| `ate-e2e-latency-dashboard.json` | "Substrate Routing & E2E Latency". Substrate routing P50/P95/P99, routing P99 by stage (routing / ateapi ResumeActor / atelet Restore), routing P99 by ActorTemplate, routing QPS by status, plus the E2E full round-trip from Envoy (ms — includes actor compute + response, so it's context, not our overhead): P99 and QPS by response class. Needs the `atenet-router-envoy` PodMonitoring for the round-trip lines. |
| `ate-snapshot-dashboard.json` | Substrate snapshot image size and throughput ("Substrate Snapshot Size & QPS"): snapshot (`pages`) image size P99 by ActorTemplate, P50/P95/P99 by image kind, and snapshot QPS. Checkpoint/Restore *latency* is not here — it's the atelet gRPC `Checkpoint`/`Restore` methods in `ate-grpc-dashboard.json`. |

The ResumeActor line in `ate-e2e-latency-dashboard.json` matches both
`ateapi.Control/ResumeActor` (Substrate v0.6.0 and earlier) and
`substrate.ateapi.v1.Control/ResumeActor` (after agent-substrate/substrate#2996
moved the public APIs under `api/<name>/v1`), so one dashboard serves clusters
on either side of the rename.

## Applying

Dashboards are created/updated (idempotently) by setup:

```sh
go -C tools/setup-gcp run . create dashboards   # also part of: bootstrap
```

Or apply any single file by hand:

```sh
gcloud monitoring dashboards create --config-from-file=tools/setup-gcp/dashboards/<file>.json
```
