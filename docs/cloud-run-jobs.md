# Stirrup on Cloud Run jobs

[Cloud Run jobs](https://cloud.google.com/run/docs/create-jobs) are a
one-shot batch surface: the container runs to completion, the exit code
*is* the result, and no HTTP listener is required. That matches
stirrup's existing exit semantics exactly — the harness binary runs
unmodified, with no Cloud-Run-specific code path in the loop, the
provider adapters, or the executor.

This document is the operator walkthrough. The architectural shape and
the result-collection design sit in
[`docs/architecture.md`](architecture.md). For the broader cross-cloud
credential federation story (which Cloud Run does **not** need — see
*Auth* below), see
[`docs/credential-federation.md`](credential-federation.md).

A complete fixture lives at
[`examples/runconfig/cloud-run-vertex-gemini.json`](../examples/runconfig/cloud-run-vertex-gemini.json).
The smoke workflow at
[`.github/workflows/smoke-cloud-run-job.yml`](../.github/workflows/smoke-cloud-run-job.yml)
deploys, executes, and verifies a Cloud Run job end-to-end on every
manual dispatch.

## Why Cloud Run jobs, not services or functions

The three Cloud Run product surfaces have materially different
container contracts, and only one fits an exit-on-completion binary
like stirrup:

| Surface | Container contract | Fit |
|---|---|---|
| Cloud Run **service** | Must listen on `0.0.0.0:$PORT` for HTTP. Long-running, autoscaled. | Bad fit. Wrapping the harness in an HTTP front is busywork. |
| Cloud Run **functions** (2nd-gen Cloud Functions) | Cloud Run service plus the Functions Framework wrapper. Event-driven. | Bad fit. Same HTTP problem with extra packaging overhead. |
| Cloud Run **jobs** | One-shot batch; container runs to completion, exit code is the result. No port. | Right fit. Matches the existing exit semantics. |

The job contract (no port, exit code, signal handling, env injection)
is documented at the Cloud Run [container runtime
contract](https://cloud.google.com/run/docs/container-contract).
Stirrup respects every constraint there without code changes — the
distroless image from
[`docs/container-publishing.md`](container-publishing.md) is the same
image used here.

## Auth

A Cloud Run job that runs in GCP does **not** need Workload Identity
Federation. The job inherits the attached service account identity via
the same GCE metadata server (`metadata.google.internal`) that GKE
Workload Identity exposes, and stirrup's `gcp-workload-identity`
credential source reads it transparently:

- `credential.type: "gcp-workload-identity"` resolves OAuth access
  tokens against the metadata server. The closure refreshes on
  expiry; no static credential ever enters the container.
- `gcp-default` (Application Default Credentials) would also work, but
  ADC also walks `GOOGLE_APPLICATION_CREDENTIALS` and user-mode
  `gcloud auth application-default login` files. `gcp-workload-identity`
  is the explicit, fail-closed variant — if the metadata server is
  unreachable, the run errors at boot rather than silently picking
  up a developer's local credentials. That is the correct semantics
  for a batch job.

The attached service account holds every Google-side grant
(`roles/aiplatform.user` for Vertex AI calls, `roles/storage.objectCreator`
on the results bucket). The harness itself holds nothing.

## Prerequisites

The walkthrough assumes a target GCP project, with these environment
variables set in the operator's shell for paste-readability:

```sh
PROJECT_ID=<gcp-project-id>
REGION=europe-west4
RESULTS_BUCKET=stirrup-results-${PROJECT_ID}
```

## 1. Enable the required APIs

```sh
gcloud services enable \
  run.googleapis.com \
  aiplatform.googleapis.com \
  artifactregistry.googleapis.com \
  storage.googleapis.com \
  secretmanager.googleapis.com \
  iamcredentials.googleapis.com \
  --project="$PROJECT_ID"
```

`iamcredentials.googleapis.com` is required because the deployer
identity needs to attach a service account to the Cloud Run job
(`iam.serviceAccountUser` on the target SA) — see
[*Service identity for jobs*](https://cloud.google.com/run/docs/securing/service-identity).

## 2. Create the results bucket

The bucket holds two artefacts: JSONL traces uploaded by the
`gcs` trace emitter, and workspace tarballs uploaded by
`executor.workspaceExportTo`. Uniform bucket-level access is the
recommended posture; per-object ACLs are an attack surface the
harness never needs.

```sh
gcloud storage buckets create "gs://${RESULTS_BUCKET}" \
  --location="$REGION" \
  --uniform-bucket-level-access \
  --project="$PROJECT_ID"
```

Apply a retention policy appropriate to the use case. For an eval
suite that produces several GiB per run, 30 days of object lifecycle
with `Delete` action is a reasonable default — Cloud Storage charges
storage for the full retention window even after the run completes.
The lifecycle file lives operator-side; an example:

```json
{
  "lifecycle": {
    "rule": [
      { "action": { "type": "Delete" }, "condition": { "age": 30 } }
    ]
  }
}
```

```sh
gcloud storage buckets update "gs://${RESULTS_BUCKET}" \
  --lifecycle-file=retention-30d.json \
  --project="$PROJECT_ID"
```

## 3. Create a user-managed service account for the job

Google explicitly recommends a user-managed service account over the
default Compute Engine SA for Cloud Run workloads — the default SA
holds `roles/editor` project-wide, which is far too broad for a batch
job that only needs Vertex AI and bucket-write access. The
recommendation is documented at [*Cloud Run service
identity*](https://cloud.google.com/run/docs/securing/service-identity).

```sh
gcloud iam service-accounts create stirrup-job \
  --display-name="Stirrup Cloud Run job" \
  --project="$PROJECT_ID"

JOB_SA="stirrup-job@${PROJECT_ID}.iam.gserviceaccount.com"
```

## 4. Grant the minimal IAM

Two role bindings — Vertex AI usage at the project level, and bucket
writes scoped to the results bucket. `roles/storage.objectCreator` is
the smallest grant that allows writes; `objectAdmin` is overkill (it
permits delete and ACL modification, neither of which the harness
ever performs).

```sh
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${JOB_SA}" \
  --role="roles/aiplatform.user"

gcloud storage buckets add-iam-policy-binding "gs://${RESULTS_BUCKET}" \
  --member="serviceAccount:${JOB_SA}" \
  --role="roles/storage.objectCreator"
```

## 5. Stage the RunConfig in Secret Manager

The Cloud Run job mounts the RunConfig as a file at
`/etc/stirrup/runconfig.json`. File-mounted secrets fit RunConfigs up
to ~64 KiB comfortably, rotate without redeploy, and avoid the
problem of structured JSON inside an env var (escape ambiguity, log
leakage). The approach is documented at [*Using secrets with Cloud
Run jobs*](https://cloud.google.com/run/docs/configuring/jobs/secrets).

The secret content is the JSON from
[`examples/runconfig/cloud-run-vertex-gemini.json`](../examples/runconfig/cloud-run-vertex-gemini.json),
with the bucket name and project ID swapped in for the placeholders.
The relevant fields:

```jsonc
{
  "provider": {
    "type": "gemini",
    "gcpProject": "<project-id>",
    "gcpLocation": "global",
    "credential": { "type": "gcp-workload-identity" }
  },
  "executor": {
    "type": "local",
    "workspace": "/tmp/stirrup-workspace",
    "workspaceExportTo": "gs://stirrup-results-<project-id>/cloud-run-vertex-gemini-example/workspace.tar.gz"
  },
  "traceEmitter": {
    "type": "gcs",
    "bucket": "stirrup-results-<project-id>",
    "objectPrefix": "traces/"
  },
  "resultSink": { "type": "stdout-json" }
  // …
}
```

```sh
gcloud secrets create stirrup-runconfig \
  --replication-policy=automatic \
  --project="$PROJECT_ID"

gcloud secrets versions add stirrup-runconfig \
  --data-file=runconfig.json \
  --project="$PROJECT_ID"

gcloud secrets add-iam-policy-binding stirrup-runconfig \
  --member="serviceAccount:${JOB_SA}" \
  --role="roles/secretmanager.secretAccessor" \
  --project="$PROJECT_ID"
```

## 6. Deploy the Cloud Run job

The image reference points at the same dual-published artefact the
GHCR/GAR publish workflow produces — see
[`docs/container-publishing.md`](container-publishing.md).
`--max-retries=0` is intentional: a stirrup run is not idempotent
(side effects via `run_command` and `edit_file` would re-execute on
retry), and Cloud Run's default of 3 retries silently amplifies
behaviour the operator did not opt into.

```sh
gcloud run jobs deploy stirrup-vertex-gemini \
  --image=europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup:latest \
  --region="$REGION" \
  --service-account="$JOB_SA" \
  --task-timeout=600 \
  --max-retries=0 \
  --memory=2Gi \
  --cpu=1 \
  --set-secrets=/etc/stirrup/runconfig.json=stirrup-runconfig:latest \
  --command=/usr/local/bin/stirrup \
  --args=harness,--config,/etc/stirrup/runconfig.json,--prompt,'Write hello.txt with the string smoke test passed' \
  --project="$PROJECT_ID"
```

The container contract Cloud Run applies is documented at the [Cloud
Run container runtime
contract](https://cloud.google.com/run/docs/container-contract).
Salient points for stirrup:

- **Filesystem** is writable but backed by in-memory tmpfs; tmpfs
  bytes consume the instance's memory budget. The 2 GiB above is
  comfortable for the default `gemini-2.5-flash` workload; raise it
  for large workspace or large-context runs.
- **SIGTERM grace** is 10 seconds before SIGKILL. Stirrup's signal
  handler at `harness/cmd/stirrup/cmd/root.go::setupSignalHandler`
  cancels the run context on SIGTERM, which lets the trace emitter
  flush and `workspaceExportTo` upload before the kill.
- **Max task timeout** is 7 days (1 hour with GPU); see [Cloud Run
  quotas](https://cloud.google.com/run/quotas). The fixture's
  `timeout: 600` is well inside that envelope.

## 7. Execute on demand

```sh
gcloud run jobs execute stirrup-vertex-gemini \
  --region="$REGION" \
  --wait \
  --project="$PROJECT_ID"
```

`--wait` blocks the local command until the execution terminates and
exits non-zero if the job did. Without `--wait`, the command returns
as soon as the execution is queued, which is the right shape for
async dispatch (a GHA workflow that fires-and-forgets) but the wrong
shape for an interactive smoke test.

## 8. Collect results

Both result-collection paths run independently — Shape A (the
structured `RunResult` from Cloud Logging) and Shape B (the workspace
tarball from GCS). The fixture configures both; real fixtures often
configure only one.

### Shape A: structured result from Cloud Logging

The `stdout-json` result sink writes a single line to stdout at
end-of-run, prefixed by the literal sentinel `STIRRUP_RESULT `:

```
STIRRUP_RESULT {"schemaVersion":1,"runId":"…","outcome":"success","turns":4,"tokenUsage":{"input":1234,"output":567},"finalAssistantText":"…"}
```

Cloud Run pipes stdout to Cloud Logging without re-serialisation, so
the line surfaces verbatim in the execution's logs. The sentinel
distinguishes the structured-result line from incidental tool-call
output. Extraction:

```sh
EXEC=$(gcloud run jobs executions list \
  --job=stirrup-vertex-gemini \
  --region="$REGION" \
  --limit=1 \
  --format='value(name)' \
  --project="$PROJECT_ID")

gcloud logging read \
  "resource.type=cloud_run_job AND labels.\"run.googleapis.com/execution_name\"=$EXEC AND textPayload:\"STIRRUP_RESULT \"" \
  --limit=1 \
  --format='value(textPayload)' \
  --project="$PROJECT_ID" \
  | sed 's/^STIRRUP_RESULT //' \
  | jq .
```

The `RunResult` schema is stable and versioned (`schemaVersion: 1`).
The fields most useful to a caller are `outcome` (`success`, `error`,
`stalled`, `tool_failures`, `timeout`), `turns`, `tokenUsage`, and
`finalAssistantText` (present when the loop produced one — callers
that framed the prompt for structured output parse it here).

Cloud Logging retention costs apply
(see [Cloud Logging pricing](https://cloud.google.com/stackdriver/pricing#logging-costs);
≈ $0.50/GiB at time of writing). For low-volume classification
workloads, the cost is negligible; higher-volume callers should
prefer the reserved `pubsub` result sink (interface reserved, adapter
deferred — see *Out of scope* below).

### Shape B: workspace tarball from GCS

`executor.workspaceExportTo` tarballs the workspace directory at
end-of-run and uploads it to the configured `gs://` URI. The upload
runs even when the harness exited non-zero, so the workspace state
at the failure point is recoverable.

```sh
gcloud storage cp \
  "gs://${RESULTS_BUCKET}/cloud-run-vertex-gemini-example/workspace.tar.gz" \
  workspace.tar.gz

tar -xzf workspace.tar.gz
```

Upload failures are logged but do **not** change the run's exit code
unless `--export-workspace-required` is set. The default is a soft
failure because a partial GCS outage during the upload window is
operationally less disruptive than masking the underlying run's
success — flip the required flag on for eval / regression pipelines
where the artefact is the deliverable.

### Trace JSONL from GCS

The `gcs` trace emitter writes a single JSONL file at
`gs://{bucket}/{objectPrefix}{runId}.jsonl`. Trailing slash on
`objectPrefix` is treated as implicit; an empty prefix puts the
trace at the bucket root. Same auth path as the workspace export.

```sh
gcloud storage cp \
  "gs://${RESULTS_BUCKET}/traces/cloud-run-vertex-gemini-example.jsonl" \
  trace.jsonl
```

## 9. Schedule with Cloud Scheduler (optional)

Cloud Scheduler invokes the Cloud Run jobs `:run` endpoint on a
cron-format trigger. The pattern is documented at [*Execute jobs on a
schedule*](https://cloud.google.com/run/docs/execute/jobs-on-schedule).
The scheduler job needs `roles/run.invoker` on the Cloud Run job and
its own service account; same pattern as any other authenticated
Cloud Scheduler target.

The harness has no scheduling state of its own — every execution is
independent. Run-to-run state (caches, model fine-tuning data) must
go through GCS or another external store.

## Gotchas

- **The default Compute Engine service account is not recommended.**
  It holds `roles/editor` project-wide. Cloud Run jobs deployed
  without an explicit `--service-account` inherit it, which means a
  prompt-injection failure could touch every project resource. The
  user-managed `stirrup-job@…` SA above is the minimum-blast-radius
  posture. Check whether a project is on the default SA with
  `gcloud iam service-accounts list --filter='email:*-compute@*' --project="$PROJECT_ID"`.
- **`--region` must match the GAR repo's region** (or the image must
  exist as a same-image multi-arch entry in a region close to the
  Cloud Run region; the dual-publish workflow pushes
  `linux/amd64` + `linux/arm64`). A `latest` tag in a different
  region surfaces as `Image not found` at deploy time, not at
  execution time.
- **Vertex `gcpLocation` does not have to match the Cloud Run
  region.** `global` is the right default for Gemini availability;
  regional pins (`europe-west4`, `us-central1`) are useful for
  data-residency requirements but trade higher per-region quota
  pressure. The two regions are independent decisions.
- **VPC connector / Direct VPC egress is deferred.** Default Cloud
  Run egress reaches Vertex AI's public endpoint over the
  Google-internal backbone; no VPC plumbing is needed for v1.
  Operators with Private Google Access requirements should track the
  follow-up issue rather than retrofitting a connector into this
  walkthrough.
- **Workspace persistence is tmpfs.** The workspace lives in
  in-memory storage that vanishes when the container exits. Anything
  the operator needs to keep must exit via
  `executor.workspaceExportTo` (the workspace tarball), the trace
  emitter, or the result sink.
- **`STIRRUP_RESULT` is the literal sentinel.** Anything else on
  stdout — tool-call output, log statements that escape to stdout —
  is *not* the result. The sentinel exists precisely so a noisy run
  cannot poison the structured-result extraction.

## See also

- [`docs/container-publishing.md`](container-publishing.md) — the
  dual-publish workflow that produces the GAR image referenced by
  `--image=`.
- [`docs/credential-federation.md`](credential-federation.md) —
  cross-cloud federation primitives. (Cloud Run does not need WIF;
  the in-GCP-runtime metadata-server path is simpler.)
- [`harness/internal/credential/google.go`](../harness/internal/credential/google.go)
  — `GoogleWorkloadIdentitySource` implementation; identical behaviour on
  GKE Workload Identity, GCE instances, and Cloud Run jobs.
- [`harness/internal/trace/gcs.go`](../harness/internal/trace/gcs.go)
  — the GCS trace emitter.
- [`harness/internal/workspaceexport/`](../harness/internal/workspaceexport)
  — the `gs://` workspace tarball uploader.
- Cloud Run [container runtime
  contract](https://cloud.google.com/run/docs/container-contract),
  [quotas and limits](https://cloud.google.com/run/quotas), [service
  identity](https://cloud.google.com/run/docs/securing/service-identity),
  [secrets](https://cloud.google.com/run/docs/configuring/jobs/secrets),
  [scheduled
  execution](https://cloud.google.com/run/docs/execute/jobs-on-schedule).
