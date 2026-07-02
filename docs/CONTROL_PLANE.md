# Control plane design

- **Status:** principal design document. Input to a future
  implementation-planning session, to be combined with additional
  specifications (see [§9](#9-related-documents)). Nothing here is
  a published-API commitment.
- **Date:** 2026-07-02, grounded at stirrup `adac2b2` (branch
  `control-plane`).
- **Scope:** requirements, integration contracts, deployment
  shapes, and design recommendations for a control plane that
  schedules, launches, supervises, and collects results from
  stirrup harness runs. The control plane is a separate project;
  this document lives in stirrup because stirrup defines the
  contract the control plane must implement.

## 1. Position and division of labour

Stirrup is the **agent plane**: a short-lived, single-run process
that executes one agentic task to completion and exits. It is
started by a control plane (or a developer) per task
(`docs/architecture.md`). Everything stateful across tasks is
deliberately out of scope for the harness and assigned to the
control plane by the existing docs and code comments:

- "No persistent state across tasks. The control plane is
  responsible for conversation history, **scheduling**, and any
  multi-task continuity." (`docs/architecture.md`)
- "Cost is a control-plane concern. The harness tracks tokens for
  budget enforcement but does not maintain pricing tables."
  (`docs/architecture.md`, `harness/internal/core/types.go`)
- Batch mode: "The control plane bundles concurrent runs into a
  single provider-side batch" and "is responsible for deciding
  whether to cancel an entire bundle when a single run drops out."
  (`docs/batch.md`)
- No queue, fleet, scheduler, or session-store construct exists
  anywhere in the repository — by design.

The control plane is therefore the long-running service that owns:
task intake and queueing, RunConfig authoring, workspace
materialisation, dispatch, live supervision (heartbeats,
permission brokering, cancellation), result and trace ingestion,
conversation continuity, retry policy, cost accounting, and fleet
management. [§4](#4-responsibilities-of-a-control-plane) catalogues
each responsibility.

### 1.1 Established decisions

These decisions are already on the project record and constrain
any control-plane design. They should be treated as settled unless
explicitly reopened:

1. **Single-tenant, orchestration-only.** The control plane
   focuses exclusively on orchestrating agent workloads for one
   tenant. Multi-tenancy is implemented *above* it — a management
   plane provisions one control plane per tenant — and no sense of
   tenancy is pushed down into the agent plane (issue #110,
   closed as "won't do" with this rationale). This supersedes the
   earlier per-record tenant-stamping design from the issue #104
   research; the stamping *invariant* (identity derives from the
   authenticated channel, never from a body field) remains good
   guidance for whatever identity the control plane does track.
2. **The harness connects outbound.** The control plane is a gRPC
   *server*; each harness dials `CONTROL_PLANE_ADDR` and opens one
   bidi stream per run. There is no inbound port on the harness,
   no service mesh hop into it, and no shared filesystem
   (`docs/deployment.md`).
3. **Secrets never travel in RunConfig.** The control plane
   provisions credential *bindings* (env vars, mounted token
   files, metadata-server identities) into the runtime; the
   RunConfig carries only `secret://` references and non-secret
   federation identifiers. `RunConfig.Redact()` strips references
   before anything is persisted (`types/runconfig.go`).
4. **Mutual partial trust.** The harness treats the control plane
   as partially trusted: control-plane-supplied tool results are
   size-capped and scrubbed for secret-shaped strings on entry
   (`harness/internal/core/types.go`). Symmetrically, the control
   plane must not treat harness-asserted fields as identity — see
   [§6.4](#64-transport-security).
5. **The process boundary is the sanctioned integration
   boundary.** The engine feasibility assessment
   (`docs/ENGINE.md`, branch `engine`) concludes that in-process
   embedding via `harness/harnessapi/` is viable but currently
   defective (four verified defects, an 86-module dependency
   closure) and recommends the subprocess/gRPC boundary — the one
   eval already uses — until the gated engine phases land. A
   control plane should exec the binary or drive `stirrup job`,
   not embed the loop.
6. **Safe-by-default posture is preserved end-to-end.** The CLI
   defaults to the read-only `planning` mode; read-only modes
   structurally exclude mutating tools and reject `allow-all`
   permission policies (`types/runconfig.go::ValidateRunConfig`).
   A control plane inherits the obligation: its own defaults must
   not be looser than the harness's.

## 2. The integration surface stirrup ships today

Stirrup already ships the harness side of a complete control-plane
protocol. This section inventories every surface a control plane
drives or consumes, with the contract facts an implementation plan
needs. Source-of-truth pointers are given per subsection; this
document summarises rather than duplicates them.

### 2.1 The gRPC contract

Source of truth: [`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto);
operator walkthrough with sequence diagram in
[`docs/deployment.md`](deployment.md). Generated Go stubs live in
the `gen/` module (`github.com/rxbynerd/stirrup/gen`) — a Go
control plane imports `gen` for the typed messages and server
stub, and `types` for RunConfig authoring and validation.

```proto
service HarnessService {
  rpc RunTask(stream HarnessEvent) returns (stream ControlEvent);
}
```

One bidi stream per run, for the life of the run. The harness is
the gRPC **client**; the control plane implements the **server**.

**Lifecycle** (`stirrup job`, `harness/cmd/stirrup/cmd/job.go`):

1. Harness dials `CONTROL_PLANE_ADDR`, opens `RunTask`.
2. Harness sends `HarnessEvent{type:"ready"}` carrying
   `harness_version` and, in the event `id` field, the
   `CONTROL_PLANE_SESSION_ID` env value — the correlation key the
   control plane set when it created the Job.
3. Control plane sends `ControlEvent{type:"task_assignment"}`
   with a full `RunConfig` in `task`. This must be the first
   ControlEvent; the harness waits **5 minutes** for it, then
   exits. A `cancel` sent before assignment causes a clean
   no-op exit.
4. Harness builds the loop and streams events; the control plane
   answers requests and may cancel.
5. Harness sends a terminal `done` event, optionally holds the
   stream open for a follow-up grace window, then exits.

**Harness → control plane events** (`HarnessEvent`):

| Type | Payload highlights | Control-plane obligation |
|---|---|---|
| `ready` | `harness_version`, session ID in `id` | Correlate stream to the dispatched task; gate on version compatibility before assigning. |
| `text_delta` | `text` | Optional: live output display. High volume. |
| `tool_call` | `id`, `name`, `input` (JSON) | Optional: live activity display, audit. |
| `tool_result` | `tool_use_id`, `content` | Optional: audit. |
| `permission_request` | `request_id`, `tool_name`, `input` | **Must answer** with `permission_response` echoing `request_id` before the `ask-upstream` policy timeout (default **60 s**, `permissionPolicy.timeout`; also reachable as a policy-engine fallback) or the call is auto-denied. |
| `tool_result_request` | `request_id`, `tool_use_id`, `tool_name`, `input` | **Must answer** with `tool_result_response`. This is the async-tool deferral channel (future transport-backed sub-agents, issue #54). |
| `heartbeat` | — | Emitted every **30 s** (hardcoded). Absence indicates a hang; drive the orphan-detection watchdog from it. |
| `warning` / `error` | `message` | Persist; `error` precedes an abnormal exit. |
| `batch_submission` | `request_id`, `input` (provider request body) | Batch mode only: submit to the provider batch API, reply with `batch_result` (see [§4.7](#47-batch-bundling)). |
| `batch_waiting` | `request_id` | Batch liveness, every 5 minutes. |
| `batch_cancel_request` | `request_id` | Best-effort: cancel the provider-side batch entry. |
| `done` | `stop_reason` | Terminal. See the trace caveat below. |

**Control plane → harness events** (`ControlEvent`):

| Type | Payload | Semantics |
|---|---|---|
| `task_assignment` | `task` (RunConfig) | Once, first. Duplicates are ignored. There is no mid-run config mutation. |
| `user_response` | `user_response` | Free-text user message. During a run it is injected on the next turn; during the follow-up grace window it triggers a follow-up run (see the continuity caveat in [§7](#7-known-gaps-the-control-plane-project-will-press-on)). |
| `permission_response` | `request_id`, `allowed`, `reason` | Answer to `permission_request`. `reason` on denial is passed to the model as context. |
| `tool_result_response` | `request_id`, `content`, `is_error` | Fulfils an async tool. Content is capped at 1 MiB (`maxAsyncToolResultBytes`, silently truncated) and secret-scrubbed on entry — the harness does not trust it blindly. |
| `batch_result` | `request_id`, `content`, `is_error` | Provider batch outcome for a `batch_submission`. |
| `cancel` | — | Graceful interrupt: the harness stops within one turn boundary, cancels in-flight provider/tool work via context, still runs git finalisation, and emits `done` with `stop_reason:"cancelled"`. |

**Contract facts that shape control-plane design:**

- **No reconnect.** `GRPCTransport`
  (`harness/internal/transport/grpc.go`) has no redial logic. A
  broken stream is terminal for that run attempt: emit failures
  surface as errors and the process exits. The control plane must
  treat stream loss as "run orphaned" and reschedule as a *new*
  run (fresh `runId`, fresh workspace) if policy allows.
- **`done` does not carry run metrics today.** The proto documents
  a `trace` field on `done`, but the loop emits
  `HarnessEvent{Type:"done", StopReason: outcome}` without
  populating it (`harness/internal/core/loop.go`; issue #104
  research follow-up FU-2). Until that lands, per-run metrics
  reach the control plane via OTel, the trace emitter, or the
  result sink — not the stream.
- **`done.stop_reason` carries the full outcome vocabulary**
  (`success`, `error`, `max_turns`, `verification_failed`,
  `verification_error`, `budget_exceeded`, `stalled`,
  `tool_failures`, `cancelled`, `timeout`, `max_tokens`) — the
  loop reports its `RunTrace.Outcome` value there, which is wider
  than the seven-value set the proto comment lists.
- **Transport security is not built in.** The dial path defaults
  to `insecure.NewCredentials()`; `stirrup job` currently wires
  no TLS options. See [§6.4](#64-transport-security) and issue #7.
- **Permission gating requires gRPC.** The `ask-upstream` policy
  and the Rule-of-Two `onDetect:"ask-upstream"` action both
  require `transport=grpc`; stdio has no upstream to answer
  (`types/runconfig.go`).

### 2.2 The `stirrup job` container contract

Source of truth: [`docs/deployment.md`](deployment.md).

- Image: `ghcr.io/rxbynerd/stirrup:<tag>` (releases) or `:main`;
  distroless static, no shell, uid 65532, multi-arch
  linux/amd64+arm64. `ENTRYPOINT ["/usr/local/bin/stirrup"]` with
  no default subcommand — the Job spec passes `job` (or
  `harness …`) as args.
- Env: `CONTROL_PLANE_ADDR` (required), `CONTROL_PLANE_SESSION_ID`
  (correlation, echoed in `ready`; the harness does not enforce
  presence or format — see [§6.4](#64-transport-security)),
  `STIRRUP_FOLLOWUP_GRACE` (seconds; the RunConfig
  `followUpGrace` field is validated ≤ 3600, the env fallback is
  parsed unvalidated).
- Liveness: the harness touches `/tmp/healthy` after connecting
  and removes it on exit. The image ships no shell, so exec-based
  probes need a debug sidecar; see the probe guidance in
  [`docs/deployment.md`](deployment.md).
- Signals: SIGTERM/SIGINT cancel the run context; the trace
  emitter flushes and the workspace export still runs (on the
  independent post-run timeout described in §2.5), provided the
  kill grace window allows. Set `Job.spec.activeDeadlineSeconds`
  slightly above `RunConfig.timeout` as the cluster-side
  backstop.
- Exit code signals *infrastructure* success only: `0` even when
  the run outcome is `max_turns` or `verification_failed`;
  non-zero for transport/build/runtime failure. Semantic outcome
  must be read from the event stream or result surfaces — never
  inferred from the exit code.

### 2.3 The CLI subprocess contract

For control planes that spawn the harness as a local child process
(`stirrup harness`), the relevant surface
(`harness/cmd/stirrup/cmd/harness.go`, `runconfigbuilder.go`):

- **Config delivery:** `--config <path>`, piped stdin (a JSON
  RunConfig is auto-detected; empty pipes are tolerated), or
  flags. Explicitly-changed flags override file fields; defaults
  do not. `stirrup run-config` emits a resolved config without
  running, enabling pipeline composition; `--output-runconfig`
  captures the exact config a flag invocation would use.
- **Live event stream for free:** the default `stdio` transport
  writes every `HarnessEvent` as a scrubbed JSON line to stdout
  during the run. An observe-only supervisor can consume progress
  without gRPC. Interactive control (permissions, follow-ups,
  cancel-by-event) still requires
  `--transport grpc --transport-addr <host:port>` pointed at a
  loopback control-plane server; in harness mode the config comes
  from flags/file and the transport is used for events and
  control only (no `task_assignment` wait).
- **Result line:** `resultSink.type:"stdout-json"` (or
  `--output json`) prints one final
  `STIRRUP_RESULT {json}` line — sentinel `"STIRRUP_RESULT "`,
  last line wins, `grep | tail -n1` to defeat prompt-injected
  fakes (`harness/internal/resultsink/resultsink.go`).
- **Exit codes** (`harness/cmd/stirrup/cmd/exitcode.go`): `0`
  success, `1` validation failure (and untyped errors), `2` config
  parse error, `3` I/O error, `4` usage error. Same caveat as
  §2.2: exit code ≠ run outcome.
- **Preflight:** `--dry-run` exercises every init step short of
  the first turn — validation, component construction, credential
  resolution, provider/MCP/trace/egress/executor reachability —
  without spending tokens. A control plane should run it when
  onboarding a new config shape or environment, not per dispatch.
- **CLI-only knobs:** `--output`, `--export-workspace-required`,
  and session flags are wrapper concerns not present on RunConfig;
  a pure-gRPC control plane cannot set them via `task_assignment`.

### 2.4 RunConfig authoring

Source of truth: `types/runconfig.go` (schema + validation),
[`docs/configuration.md`](configuration.md) (field reference).

The RunConfig is the composition root: one document fully
describes a run, and authoring it is the control plane's core
write-path. Facts an authoring layer must encode:

- **Validation-enforced fields:** `mode`, `provider`, `maxTurns`
  (1–100), `timeout` (1–3600 s). **Schema-expected but not
  validation-enforced:** `prompt` — an empty prompt passes
  `ValidateRunConfig` (by design, since `systemPromptOverride`
  and prompt builders can supply content), so the authoring layer
  needs its own non-empty check at intake. `runId` must match
  `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$` and should always be set by
  the control plane (it is the universal correlation key across
  events, traces, metrics, and artifact names).
- **Spend and time bounds** are per-run and ceiling-capped by
  validation: `maxTokenBudget` ≤ 50 M, `maxCostBudget` ≤ $100,
  `timeout` ≤ 1 h, `followUpGrace` ≤ 1 h, `toolDispatch.maxParallel`
  ≤ 16. Anything longer-lived than an hour is a control-plane
  construct built from multiple runs.
- **Mode defaulting is CLI-layer only.** `applyModeDefaults`
  (read-only tool list, `deny-side-effects` policy) runs in the
  CLI, not in `ValidateRunConfig`. A control plane authoring raw
  RunConfigs must populate `tools.builtIn` (e.g. from
  `types.DefaultReadOnlyBuiltInTools()`) and `permissionPolicy`
  itself for read-only modes, or validation rejects the config.
- **Validation is importable.** A Go control plane should call
  `types.ValidateRunConfig` at task-intake time — exactly the
  fail-fast pattern `eval/runner` uses — so a bad config is
  rejected at the API boundary instead of after a Pod was
  scheduled. Note it mutates (fills defaults) and the read-only
  invariants, Rule-of-Two check, and closed-set enums all fire
  here.
- **No schema version; strict parsing.** RunConfig has no version
  field, and both JSON entry points use `DisallowUnknownFields` —
  an older harness hard-rejects a config carrying newer fields
  (exit 2). A control plane that persists configs against a mixed
  fleet must pin the harness image/version per config and gate
  emitted fields on the target's `harness_version` (from `ready`).
  Removing fields is safe; adding fields is a break for older
  binaries.
- **Redact before persisting.** Store operator-facing copies via
  `RunConfig.Redact()`; store the authoritative config in a
  secret-grade store if exact reproduction is needed. Session
  headers in the sessions spec follow the same principle
  (`docs/sessions-spec-draft.md` §5.3).

### 2.5 Result collection surfaces

Stirrup deliberately splits results into independent surfaces
(`harness/internal/resultsink/resultsink.go`); a run wires any
combination:

| Surface | Config | Payload | Delivery semantics |
|---|---|---|---|
| **Result sink** (the answer) | `resultSink.type` | `types.RunResult`: `schemaVersion`, `runId`, `outcome`, `turns`, `tokenUsage`, `durationMs`, `finalAssistantText` (currently never populated — issue #164), `verifierVerdict`, `error` | Fire-once, best-effort, non-fatal on failure, no retry. Implemented: `none`, `stdout-json`. `gcp-pubsub` and `gcs` are reserved and rejected by validation. |
| **Trace emitter** (the evidence) | `traceEmitter.type` | Full JSONL transcript (`run_started`, `turn_record` with complete model I/O, `tool_call_record`, `run_finished`) or OTel spans | `jsonl`: streamed + flushed per event, crash-tolerant — but **a default empty `filePath` writes to a discarded buffer**; the control plane must set a path or use `gcs`. `gcs`: buffers in memory, single PUT at run end to `gs://bucket/prefix/runId.jsonl` — nothing lands if the process dies. `otel`: summary spans, content capture opt-in. |
| **Workspace export** (the artifacts) | `executor.workspaceExportTo` | tar.gz of the workspace (≤ 1 GiB compressed), uploaded even on failed runs | GCS only in v1. Fires after sink + trace on an independent timeout that survives signal cancellation. |
| **OTel** (live telemetry) | `traceEmitter` (otel) + `observability` | GenAI-semconv spans, ~40 live metrics (turns, tokens, tool calls, provider latency, guard/scanner/verifier durations), optional OTLP log export | Streamed live during the run. The cleanest passive progress channel: point every run at the control plane's collector. Correlation keys: `run.id` (spans/metrics) and `RunConfig.sessionName` → `gen_ai.conversation.id`. |

Critical implication: **the full transcript never flows over the
gRPC stream.** The `done` event is summary-only (and its trace
field is unpopulated today). A control plane that wants
transcripts must set the trace emitter per run (`gcs` for remote
runs) or wait for the `UploadRecording` ingestion contract
proposed in the issue #104 research (FU-4).

### 2.6 Execution topologies and infrastructure obligations

The executor decides where *tools* run relative to the harness
process (`harness/internal/executor/`, `docs/executors/k8s.md`,
`docs/executors/k8s-agent-sandbox.md`). The control plane decides
where the *harness* runs. Combined topology matrix:

| Executor | Tools run | Infrastructure the control plane must provide |
|---|---|---|
| `local` | Same host/process as harness | A disposable, isolated host per run (the Pod/VM *is* the sandbox). Distinct workspace per run — `local` defaults to the process cwd, so two co-located runs collide. Cannot enforce an egress allowlist (validation rejects it). |
| `container` | Docker/Podman container on the harness host | Docker socket per harness host; image registry allowlist; the egress proxy runs in-process. Note the documented fail-open caveat: raw-TCP from the container bypasses the HTTP proxy unless host-level iptables is added. |
| `k8s` | Dedicated Pod per run, remote from harness | Kube API access + RBAC (`pods`, `pods/exec`, `networkpolicies`), an **enforcing CNI** (Dataplane V2 / Cilium / Calico — kindnet silently does not enforce), a sandbox image with `sh`+`tar`, and in allowlist mode a **shared `stirrup egress-proxy` Deployment per namespace** (label `app=stirrup-egress-proxy`, port 8080 — a hard contract with the generated NetworkPolicy). |
| `k8s-sandbox` | Pod via Agent Sandbox CRD (`agents.x-k8s.io/v1alpha1`) | Everything `k8s` needs plus the CRD + controller; gVisor is forced. Warm-pool adoption is refused by design (per-pod NetworkPolicy binding). |
| `api` | Nowhere (read-only VCS API) | Nothing; pure HTTP from the harness process. |

Notes for the design:

- Pod naming is `stirrup-<48-bit-random>`; collision risk across
  concurrent runs is negligible, and each run's NetworkPolicy is
  derived from its Pod name. The per-namespace egress proxy is
  the only shared mutable resource — its allowlist scope and
  capacity are control-plane policy.
- The harness is strictly single-run-per-process. Fan-out is
  achieved by launching more processes/Jobs, never by reusing one.
- `stirrup egress-proxy` is a first-class subcommand intended to
  be operated as shared infrastructure: fail-closed on an empty
  allowlist, never logs full URLs, emits `egress_allowed`/
  `egress_blocked` events. The control plane owns its deployment,
  allowlist distribution, and audit-log collection.

### 2.7 Credential provisioning

The RunConfig names a credential *strategy*; the control plane
provisions the runtime so that strategy can succeed
(`harness/internal/credential/source.go`,
[`docs/credential-federation.md`](credential-federation.md)):

| Credential type | Control plane provisions |
|---|---|
| `static` | The secret value behind the `secret://` ref: env var, mounted file, or SSM parameter (`secret://ENV`, `secret://file:///path`, `secret://ssm:///param`). |
| `aws-default` / `gcp-default` / `gcp-workload-identity` | A runtime identity (IRSA, instance profile, ADC, GKE WI metadata server) — no secret material in transit. |
| `web-identity`, `gcp-workload-identity-federation`, `anthropic-wif`, `azure-workload-identity`, `openai-wif` | An OIDC token source in the runtime (projected SA token file, env var, metadata endpoint, GHA OIDC) plus the non-secret federation identifiers in the config. The bearer is minted in-process at request time. |

Preferred posture: federation over static keys everywhere the
runtime supports it. The control plane's secret store then holds
almost nothing — it grants identities, not values.

### 2.8 Prior art in-repo

- **`eval/runner/runner.go` is a working miniature control
  plane:** bounded worker-pool fan-out with order-preserving
  results, per-task `os.MkdirTemp` workspaces, the trace file
  deliberately placed *outside* the workspace (hermeticity — an
  agent once summarised its own leaked in-progress trace),
  `ValidateRunConfig` before spawn, subprocess invocation with
  `--config`, redacted config artifacts, JSONL trace parse-back,
  and judge-based verdicts. No retries by design. Reuse these
  patterns.
- **`harness/internal/transport/grpc_test.go`** contains a
  minimal in-process `HarnessService` server — the seed of a
  control-plane conformance test.
- **The Cloud Run jobs walkthrough**
  ([`docs/cloud-run-jobs.md`](cloud-run-jobs.md)) is the
  serverless "no control plane" pattern: config via secret-mounted
  file, results via `stdout-json` + GCS trace + workspace export,
  scheduling via Cloud Scheduler, `--max-retries=0` because runs
  are not idempotent. A control plane generalises exactly these
  responsibilities.

## 3. Definition: what a control plane is not

Boundaries, to prevent scope creep in the implementation plan:

- **Not multi-tenant.** One control plane serves one tenant
  (§1.1). A management plane — separate project — provisions and
  federates control planes.
- **Not an agent framework.** Conversation logic, prompts, tool
  semantics, and safety enforcement live in the harness. The
  control plane never rewrites model messages; it routes,
  supervises, and records.
- **Not a general CI system.** DAG orchestration across dependent
  tasks, artifact build graphs, and cron infrastructure are out of
  scope for the MVP; simple chains (run B after run A with A's
  branch) can be modelled as follow-up tasks.
- **Not a secrets manager.** It integrates with one (or with
  cloud identity); it does not become one.
- **Not inside the trust boundary of the agent.** Prompt-injected
  agents will try to reach the control plane's API; its network
  position must assume sandbox egress is hostile
  ([§6.4](#64-transport-security)).

## 4. Responsibilities of a control plane

The requirements catalogue. Each item names the stirrup surface it
binds to.

### 4.1 Task intake and queueing

Accept task submissions (API/CLI/webhook/schedule), assign a
stable task ID (idempotency key), persist a durable queue with
priorities and per-queue concurrency limits. A task is not a run:
one task may produce several runs (retries, follow-ups); the task
is the user-facing unit, the run is the harness-facing unit
(`runId`).

### 4.2 RunConfig authoring and policy layering

Translate a task + operator policy into a validated RunConfig
(§2.4). Recommended layering, deterministic precedence, lowest to
highest:

1. Control-plane global defaults (safe-by-default: read-only mode,
   `deny-side-effects`, container/k8s executor, trace emitter
   always set).
2. Named profiles ("research", "pr-review", "toil-batch",
   "execution-gated") — curated bundles of mode, tools, executor,
   verifier, budgets, safety-ring posture.
3. Per-task request fields (prompt, dynamicContext, workspace
   source, budget within profile ceilings).
4. Hard fleet policy (ceilings the request cannot exceed; the
   Rule-of-Two override and `ruleOfTwo.enforce:false` reserved to
   operators).

The authoring layer must set what the CLI would otherwise default:
read-only tool lists, permission policy, trace emitter file
path/bucket, `runId`, `sessionName`.

### 4.3 Workspace materialisation

The harness assumes a workspace exists; nothing in stirrup clones
repositories. The control plane must define how code reaches the
sandbox: init-container git clone, snapshot/tarball restore,
PVC/volume mount, or image-baked sources. Requirements: each run
gets a fresh, isolated workspace (runs are not idempotent and may
leave side effects); materialisation credentials (deploy keys)
stay out of the RunConfig and out of the agent's reach; for
retries, always re-materialise. The `git_strategy: deterministic`
component handles only branch/commit management *inside* an
existing checkout.

### 4.4 Scheduling and dispatch

Match queued tasks to capacity and launch runs:

- **K8s path:** create a `Job` running `stirrup job` with
  `CONTROL_PLANE_ADDR` + `CONTROL_PLANE_SESSION_ID`, node
  selectors/taints per trust tier, `activeDeadlineSeconds` >
  `timeout`, `backoffLimit: 0` (the control plane owns retries,
  not the kubelet).
- **Local path:** spawn `stirrup harness` subprocesses (§2.3).
- **Serverless path:** trigger Cloud Run job executions with
  config mounted from Secret Manager (§2.8).

Assignment protocol: on `ready`, verify `harness_version` and the
echoed session ID, then send `task_assignment` well inside the
5-minute window. Track "dispatched but never connected" with a
timeout keyed to Pod scheduling latency.

### 4.5 Live supervision

- **Liveness:** expect `heartbeat` every 30 s; mark the run
  suspect after 2 missed intervals and orphaned after ~90 s of
  silence combined with Pod-status checks.
- **Permission brokering:** answer `permission_request` within the
  policy timeout (default 60 s) — from policy tables
  (auto-allow/deny lists) or by paging a human (UI/chat approval).
  Unanswered requests auto-deny, which fails safe but degrades the
  run; design the human path for latency.
- **Async tools:** answer `tool_result_request` (the channel
  transport-backed sub-agent dispatch will use, issue #54).
- **Cancellation:** expose task-level cancel; send `cancel`, then
  enforce with Pod deletion if `done` does not arrive within a
  turn-length grace.
- **Steering:** `user_response` mid-run injects a user message on
  the next turn.

### 4.6 Result and trace ingestion

Consume, per run: the terminal `done` event (outcome), the trace
(JSONL file collected from a bucket, or OTel spans in the
collector), the `RunResult` (once sinks or FU-2/#164 land), and
workspace artifacts (tarball from GCS). Persist an event timeline
per run — the stream events are the natural source of truth for
"what happened", and the trace is the evidence for "what the model
saw/did". Apply retention policy; traces contain full transcripts
and must be treated as sensitive even though the harness scrubs
secret-shaped strings before emission.

The lakehouse research (issue #104 and its follow-ups) is the
reference design for the analytical store: additive-with-
`omitempty` schema evolution, a `schema_version` column, hot
fields projected into typed columns (`run_id`, `started_at`,
`outcome`, `mode`, `model`, `turns`, token counts,
`parent_run_id`).

### 4.7 Batch bundling

When runs opt into batch mode (`provider.batch.enabled`,
`transport=grpc`), the control plane owns the provider-side batch
lifecycle (`docs/batch.md`): collect `batch_submission` events
across concurrent runs, bundle them into provider batch API calls
(Anthropic `/v1/messages/batches`, OpenAI `/v1/batches`), poll,
and deliver each `batch_result` correlated by `request_id`. It
also owns bundle-cancellation policy when a member run cancels
(`cancel_bundle_on_run_cancel` merely signals the preference).
This is a distinct scheduler with a 24-hour tail; treat it as an
optional module, not MVP.

### 4.8 Conversation and session continuity

Conversation history across runs is a control-plane
responsibility by charter (§1). Today the primitive is weak:
follow-ups via `user_response` in the grace window are *new runs*
whose prompt is replaced — `RunFollowUpLoop` rebuilds message
history rather than continuing it
(`docs/sessions-spec-draft.md` §1). The sessions spec (append-only
session log, `RunWithMessages`, teleporting) is the planned fix
and is the single most important stirrup-side dependency for a
conversational control plane. Design session state around that
spec's shape: session ID = first run ID, per-run new run IDs,
secretless header + externally supplied credential bindings on
resume.

### 4.9 Cost accounting and quotas

The harness enforces per-run `maxTokenBudget`/`maxCostBudget` from
internal estimates and reports `cost_usd` on `RunTrace` —
advisory. Authoritative pricing tables, per-task/queue/day
aggregation, and quota enforcement (refuse dispatch when a budget
pool is exhausted) are control-plane features. Token counts come
from OTel metrics live and the trace at completion. Note the batch
caveat: budget checks run post-turn, so one batch turn can
overshoot by a single response's worth.

### 4.10 Retry policy

Stirrup runs are **not idempotent** (`docs/cloud-run-jobs.md`
mandates `--max-retries=0`). Retry means: new `runId`, fresh
workspace materialisation, same task ID, bounded attempts with
backoff, and an audit trail linking attempts to the task.
Automatic retry is safe only for runs that never reached
side-effecting state (failed before assignment, infra exit ≠ 0
with zero tool calls); anything that executed tools needs
mode-aware policy (read-only runs: retry freely; execution runs:
require re-materialisation and, by default, human confirmation).

### 4.11 Fleet and version management

Track harness image/version per environment; gate assignment on
`ready.harness_version`; pin persisted RunConfigs to compatible
versions (§2.4 strict parsing); roll out new images
queue-by-queue. The control plane should record the version in
the run record — it is the reproducibility key alongside the
redacted config.

Be aware there is no mechanism behind "gate on version" beyond
string comparison: `harness_version` is an opaque build label
(`v1.2.3`, `main (<sha>)`, `dev`), and the proto has no
capability or minimum-version negotiation. Until upstream adds
one ([§7](#7-known-gaps-the-control-plane-project-will-press-on)
item 11), the control plane must maintain its own
version→supported-fields matrix, derived from release notes, and
dispatch only configs whose fields the target version parses.
The `dev`/`main` labels make canary fleets ambiguous — pin
production queues to tagged releases.

### 4.12 Security posture ownership

The five safety rings ([`docs/safety-rings.md`](safety-rings.md))
are operator policy, and the control plane is the operator:

| Ring | Control-plane lever |
|---|---|
| 1 — kernel isolation | Mandate `executor.runtime` (`runsc`/`kata*`) per trust tier; `k8s-sandbox` forces gVisor. |
| 2 — egress allowlist | Own `network.mode` + allowlists per profile; operate the shared egress-proxy Deployment; collect its audit events; guarantee an enforcing CNI. |
| 3 — Cedar policy engine | Distribute pinned `.cedar` policy files to runtimes; choose fallbacks; version policies alongside profiles. |
| 4 — Rule of Two | The control plane constructs every input to the invariant (dynamicContext sensitivity, tool list, network, permission policy). Reserve `ruleOfTwo.enforce:false` to break-glass with audit. |
| 5 — code scanner | Choose scanner type/strictness per profile; distribute semgrep rule bundles for air-gapped runtimes. |

Plus: credential provisioning (§2.7), transport security (§6.4),
trace retention, and the audit log of permission decisions and
overrides.

## 5. Deployment tiers

Control-plane scale varies from a laptop supervisor to an
engineering-org fleet. The recommendation is one architecture with
swappable infrastructure, not three products: the same state
machine, RunConfig authoring library, and ingestion schema at
every tier, so a team can start at Tier 0 and grow without a
rewrite.

### Tier 0 — local supervisor

The "background agents" shape: a single binary on a developer
machine (or a small always-on box) supervising a handful of
concurrent runs.

- Dispatch: subprocess `stirrup harness` per run; worktree or
  temp-dir workspaces (the eval-runner recipe).
- Control channel: a loopback gRPC server + `--transport grpc
  --transport-addr localhost:<port>` when permission brokering or
  follow-ups are wanted; plain stdout JSONL consumption for
  observe-only runs.
- State: SQLite (tasks, runs, events); traces as local JSONL
  files (`traceEmitter.filePath` outside the workspace).
- Executors: `local` (trusted work), `container` (untrusted).
- Interface: CLI + optional minimal web page; approvals via
  terminal prompt or desktop notification.

### Tier 1 — team service

A long-running service for one team: tens of concurrent runs,
shared infrastructure, humans in the loop.

- Dispatch: K8s Jobs running `stirrup job` (§4.4); executor `k8s`
  or `k8s-sandbox` for untrusted work, or `local`-in-Pod where
  the Pod is the sandbox.
- Control channel: the gRPC server behind TLS (§6.4), one
  Deployment, streams are affine to a replica — start
  single-replica-per-shard rather than solving stream handoff.
- State: Postgres (task/run state machine + event timeline);
  object store for traces + workspace exports; OTLP collector for
  live metrics; Grafana/Langfuse dashboards ride the existing
  OTel semconv work.
- Interface: HTTP API (`POST /tasks`, `GET /tasks/{id}`,
  `POST /tasks/{id}/cancel`, `GET /runs/{id}/events` with
  SSE/websocket tail), chat-ops approvals for
  `permission_request`.
- Shared infra: egress-proxy Deployment per namespace, image/
  policy/profile registries.

### Tier 2 — fleet

Thousands of engineers; multiple clusters; platform team operates.

- Everything in Tier 1, plus: horizontally scaled gRPC ingress
  (streams pinned per replica; run-affinity via
  `CONTROL_PLANE_SESSION_ID`), a durable queue (per-org
  priorities, fair-share), the batch-bundling module (§4.7) for
  research/toil fleets, lakehouse ingestion (issue #104 design)
  for analytics, org-wide quota/budget pools, and policy-as-code
  review flows for profiles and Cedar bundles.
- Multi-tenancy arrives here as a **management plane**: an
  umbrella service that provisions a control plane (and its
  namespace, proxy, buckets, dashboards) per tenant, in line with
  the issue #110 decision. Identity between planes is
  service-to-service (mTLS/SPIFFE); nothing tenant-shaped leaks
  into RunConfig or the agent plane.

## 6. Design recommendations

### 6.1 State machine

Durable, validated, auditable states; recommended minimum:

```
task:  submitted → queued → dispatching → running
         → awaiting_approval (overlay) → completed | failed
         | cancelled | orphaned → (requeue per policy)
run:   created → connected → assigned → running
         → done(outcome) | orphaned | infra_failed
```

Rules: no illegal jumps; every transition timestamped and
attributed; `orphaned` (stream lost / heartbeats missed / Pod
gone) is distinct from `failed` (clean `done` with a bad outcome)
because their retry semantics differ (§4.10). Reconciliation loop:
periodically compare desired state against Pod/process reality —
never trust the stream alone.

### 6.2 Delivery semantics

Design for at-least-once everywhere: assignment can race a dying
Pod, events can be replayed by a resumed consumer, `Emit` is
fire-once on the harness side. Idempotency keys: task ID
(intake), `runId` (everything run-scoped), `request_id`
(permission/tool/batch correlation). Since the harness never
reconnects a broken stream (§2.1), exactly-once *within* a stream
is trivially true and resumption is always a new run — this
simplifies the server considerably.

### 6.3 Event handling and backpressure

`text_delta` dominates volume. Persist selectively: store
tool_call/tool_result/permission/warning/error/done rows;
aggregate text deltas per turn (or drop them, relying on the trace
for full text). Apply per-stream buffer caps in the server;
gRPC's flow control provides natural backpressure to the harness,
whose `Emit` blocks — a stalled consumer slows the run rather
than dropping events.

Wire limits to design around: the harness configures no
`MaxRecvMsgSize`/`MaxSendMsgSize`, so gRPC's default **4 MB**
frame limit governs both directions — a `task_assignment` whose
RunConfig carries large `dynamicContext` entries can exceed it
and kill the stream at assignment time. Inbound
`tool_result_response` content is separately capped at 1 MiB by
the harness (§2.1). Budget config sizes accordingly and validate
serialized size before dispatch.

### 6.4 Transport security

The gap: the harness dials with insecure credentials by default
and `stirrup job` wires no TLS today; the proto has no
application-level auth field (issue #7 tracks `session_token`,
sequence numbers, and cancel rate-limiting; the transport already
exposes `WithTLSCredentials`/`WithDialOptions` for an embedding
layer). Until stirrup-side auth lands:

- Terminate TLS in front of the control plane; run harness↔CP
  traffic inside a mesh or private network; use NetworkPolicy so
  only harness Pods reach the ingress.
- Derive identity from the channel, not the body: match streams
  to expected `CONTROL_PLANE_SESSION_ID`s the control plane itself
  issued at dispatch, and reject unknown or reused IDs. Treat the
  session ID as a one-time bearer capability: unguessable
  (≥128-bit random), single-use, expiring with the dispatch
  window. Note the harness side provides no enforcement — an
  unset env var yields an empty `ready.id` — so the entire scheme
  rests on the control plane refusing streams whose ID it did not
  issue.
- Assume a compromised sandbox will try to reach the control
  plane: the API surface reachable from run networks must be
  exactly the gRPC ingress, nothing else (no metrics, no admin
  API on the same listener).
- Prioritise upstream issue #7 work early in the control-plane
  project; mutual auth is required before any deployment where
  runs and control plane cross a trust boundary.

### 6.5 Config, policy, and validation parity

Implement the authoring layer in Go and import `types` — the
control plane then validates with the *same* code the harness
runs (`ValidateRunConfig`), eliminating config-drift bugs between
planes. Wire-side, import `gen` for the proto types. Non-Go
control planes must treat `docs/configuration.md` + the proto as
schema and accept a validation gap (mitigate with `stirrup
run-config --validate` as a sidecar check).

### 6.6 Observability of the control plane itself

Emit control-plane metrics adjacent to the harness's: queue depth,
dispatch latency (submit→ready→assigned), permission-response
latency (the human-in-the-loop SLO), orphan rate, outcome mix,
token/cost per queue. Reuse the harness's resource-attribute
scheme (`service.namespace`, `deployment.environment`) so one
Grafana/Langfuse view spans both planes; propagate `runId` and
`sessionName` everywhere.

### 6.7 Testing strategy

- Conformance: an in-process gRPC server against the real binary
  (pattern: `harness/internal/transport/grpc_test.go`), driving
  ready→assign→events→done, permission round-trips, cancel, and
  assignment-timeout paths.
- Determinism: eval's replay providers/executors run loop-shaped
  tests without paid API calls; the control plane can reuse suite
  fixtures for end-to-end rehearsal.
- Chaos: kill Pods mid-run, drop streams, delay permission
  responses past timeout, deliver duplicate `batch_result`s —
  the orphan/retry machinery is the part that will actually be
  exercised in production.

## 7. Known gaps the control-plane project will press on

Stirrup-side work items that a control plane implementation will
surface immediately. Each should be tracked as an upstream issue
and sequenced with the control-plane milestones rather than
worked around permanently:

1. **gRPC channel security** — issue #7 (session token, replay
   protection) plus wiring TLS options into `runJob`. Required
   before any cross-trust-boundary deployment (§6.4).
2. **Run metrics on the stream** — populate `done.trace`
   (issue #104 follow-up FU-2) so the control plane gets
   turns/tokens/cost without a bucket round-trip.
3. **`RunResult.finalAssistantText`** — never populated
   (issue #164; ENGINE Phase 0). Until fixed, the run's textual
   answer exists only in the trace or the `text_delta` stream.
4. **Result sinks `gcp-pubsub` / `gcs`** — reserved, validation-
   rejected. Either implement upstream or rely on stream + trace.
5. **Transcript ingestion over the stream** — `UploadRecording`
   (FU-4) if pulling JSONL from buckets proves operationally
   clumsy; includes the recording-scrub seam.
6. **True conversational continuity** — the sessions spec
   (`docs/sessions-spec-draft.md`): `RunWithMessages`, session
   logs, teleporting. Prerequisite for follow-ups that actually
   continue a conversation (§4.8).
7. **Transport-backed sub-agent dispatch** — issue #54: the
   harness-side contract for the control plane to run sub-agents
   as sibling runs (fan-out becomes a control-plane scheduling
   concern).
8. **Config-only parity for CLI-only knobs** — `--output`,
   `--export-workspace-required` have no RunConfig fields, so the
   gRPC path cannot express them.
9. **Observability translation gaps** — `grpc_translate.go`
   notes fields that do not round-trip the wire (e.g. some
   observability resource attributes); audit before relying on
   task_assignment for full config fidelity.
10. **Heartbeat cadence is hardcoded** (30 s) — acceptable for
    v1 watchdogs; revisit only if fleet-scale tuning demands it.
11. **No version/capability negotiation on the wire** —
    `ready.harness_version` is an opaque build label and the
    proto carries no min-version or capability set, while strict
    config parsing makes field mismatches fatal (§2.4). Either
    add negotiation upstream or accept the control-plane-side
    version→fields matrix (§4.11) as a permanent maintenance
    cost.

## 8. Open questions for the implementation plan

Deliberately left to the implementation-planning session, with
leanings where a default is clear:

1. **Language and repo shape.** Leaning: Go, separate repository,
   importing `types` + `gen` (§6.5). The ENGINE assessment's
   "stable control plane's local provisioner" naming suggests the
   same expectation.
2. **Workspace materialisation v1.** Git clone via init container
   vs snapshot restore. Leaning: shallow clone + optional seed
   overlay (eval's recipe), with snapshots later.
3. **State store v1.** Leaning: SQLite at Tier 0 behind a
   storage interface, Postgres from Tier 1; event timeline as an
   append-only table either way.
4. **API surface v1.** Task CRUD + event tail + approval
   endpoints; is a UI in scope for the first milestone or is
   chat-ops enough?
5. **Which upstream gaps block MVP.** Leaning: #7 (auth) and
   FU-2 (`done.trace`) are pre-MVP; sessions and batch modules
   are post-MVP.
6. **Follow-up semantics before sessions land.** Expose
   follow-ups as "new run with shared task context" honestly, or
   hide them until continuity is real? Leaning: expose honestly,
   label clearly.
7. **Trace retention and privacy tiers.** Defaults per mode
   (read-only vs execution), encryption-at-rest requirements, and
   operator-facing redaction guarantees.
8. **Scheduling fairness.** FIFO + per-queue concurrency for MVP;
   when (if ever) does priority/fair-share arrive?

## 9. Related documents

- [`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto)
  — the wire contract (source of truth).
- [`docs/deployment.md`](deployment.md) — `stirrup job`,
  container contract, operator checklist.
- [`docs/configuration.md`](configuration.md) — RunConfig field
  reference.
- [`docs/safety-rings.md`](safety-rings.md) /
  [`docs/security.md`](security.md) — the posture the control
  plane operates.
- [`docs/batch.md`](batch.md) — batch-mode control-plane
  obligations.
- [`docs/sessions-spec-draft.md`](sessions-spec-draft.md) —
  session continuity (draft; gates §4.8).
- [`docs/cloud-run-jobs.md`](cloud-run-jobs.md) — the
  control-plane-less serverless pattern.
- `docs/ENGINE.md` (branch `engine`) — embed-vs-exec assessment;
  process boundary recommendation.
- `docs/control-plane-spec.md` (branch `control-plane-spec`) —
  an earlier externally drafted requirements sketch; its task
  model, state-machine, and MVP-milestone material is absorbed
  into §4/§6 here. Supersede on merge.
- Issues #7 (transport security), #54 (transport-backed
  sub-agents), #104 + follow-ups (lakehouse/ingestion), #110
  (single-tenant decision), #164 (final assistant text), #245
  (`--dry-run`).

## Grounding

Verified at `adac2b2`: `proto/harness/v1/harness.proto`
(HarnessService, event vocabulary, RunConfig wire schema);
`harness/cmd/stirrup/cmd/job.go` (job lifecycle, env vars,
5-minute assignment timeout, result/export post-run hooks);
`harness/cmd/stirrup/cmd/{harness,root,exitcode,runconfigbuilder}.go`
(CLI contract, exit codes, config precedence, `--dry-run`,
`--output-runconfig`); `harness/internal/transport/grpc.go`
(insecure default, no reconnect, scrub-on-emit);
`harness/internal/core/loop.go` (heartbeat, outcome vocabulary,
`done` emitted without trace payload);
`types/runconfig.go` (schema, `ValidateRunConfig` invariants,
`Redact`, limit ceilings, strict parsing);
`types/result.go` + `harness/internal/resultsink/resultsink.go`
(RunResult, sink semantics, `STIRRUP_RESULT` sentinel);
`harness/internal/trace/{jsonl,gcs,otel}.go` and
`harness/internal/observability/` (trace/metrics surfaces);
`harness/internal/executor/{local,container,k8s,agentsandbox,api}.go`
+ `egressproxy/` (topologies, proxy contract);
`harness/internal/credential/source.go` (credential matrix);
`eval/runner/runner.go` (orchestration prior art);
`docs/{architecture,deployment,batch,cloud-run-jobs,safety-rings}.md`,
`docs/sessions-spec-draft.md`; `docs/ENGINE.md` at `engine`
branch commit `bf71fe5`; GitHub issues #7, #54, #104 (+
`research/issue-104` FOLLOWUPS), #110, #164.
