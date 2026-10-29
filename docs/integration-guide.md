# Integration guide

This guide is for engineers integrating stirrup as the agentic
workload runner behind a control plane, orchestrator, or automation
pipeline. It covers the integration surfaces, the gRPC wire contract,
and cookbook-style recipes for the major features. The single
touchstone for the wire contract is
[`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto);
every message and event referenced here is defined there.

Deep dives live in the per-topic docs — this guide links to them
rather than repeating them. For the component model behind the
`RunConfig`, see [`architecture.md`](architecture.md); for the full
field reference, [`configuration.md`](configuration.md).

## Integration surfaces at a glance

| Surface | Shape | Reach for it when |
|---|---|---|
| **gRPC control plane** (`stirrup job`) | The harness dials out and opens one `RunTask` bidi stream; the control plane sends a `RunConfig` and consumes events. | Multi-run orchestration, human-in-the-loop approvals, follow-up turns, sandbox identity tokens, provider batch bundling. The primary integration surface. |
| **CLI composition** (`stirrup harness`, `stirrup run-config`) | `RunConfig` via flags, file, or stdin; results via exit code, `STIRRUP_RESULT` stdout line, traces, and sinks. | CI pipelines, cron jobs, shell-driven automation, and any feature not yet carried on the proto (see [Features not on the wire](#features-not-on-the-wire)). |
| **Serverless job** (Cloud Run) | The unmodified container runs `stirrup harness --config` with a secret-mounted `RunConfig`; results leave via stdout logging, GCS traces, and workspace export. | Elastic burst capacity with no cluster to run. Walkthrough: [`cloud-run-jobs.md`](cloud-run-jobs.md). |
| **Go embedding** (`harness/harnessapi`) | `BuildLoopWithTransport(ctx, config, transport)` constructs an in-process loop; `Loop.Run` executes it. | Single-binary tools bundling their own control-plane logic. Everything under `harness/internal/*` is not public API. |

The four surfaces use the same `RunConfig` concepts, but they are not
perfectly interchangeable. The CLI applies convenience defaults that
the gRPC path does not, and a few JSON fields have no proto mirror (see
[Features not on the wire](#features-not-on-the-wire)). For equivalent
fields, protobuf JSON uses lower-camel-case names such as `runId` and
`maxTurns`, matching the file-config form.

## The wire contract

### One RPC

```proto
service HarnessService {
  rpc RunTask(stream HarnessEvent) returns (stream ControlEvent);
}
```

The **control plane implements the server**; the harness is the
client and connects outbound. There is no inbound port on the harness
and no shared filesystem. One stream carries the whole run: task
assignment, streaming output, permission round-trips, and the terminal
status.

### Getting the schema

- Source of truth: [`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto).
  Generate stubs with `buf generate` or protoc.
- Every CI and release run publishes a compiled
  `google.protobuf.FileDescriptorSet` as the `proto-descriptor-set`
  workflow artifact (`stirrup-<label>.fds.binpb` + `.sha256`) for
  clients that need the schema without a checkout — `grpcurl`, Envoy
  gRPC-JSON transcoding, reflection-less generators.

### Stream lifecycle

1. The harness (launched as `stirrup job` with `CONTROL_PLANE_ADDR`
   set) dials the control plane and opens the `RunTask` stream.
2. The harness sends
   `HarnessEvent{type:"ready", harness_version, capabilities}`.
   When `CONTROL_PLANE_SESSION_ID` is set in the Pod environment,
   the `id` field echoes it so the stream can be correlated to the
   session that launched the Pod.
3. The control plane sends
   `ControlEvent{type:"task_assignment", task: RunConfig}` — this
   must be the first `ControlEvent`. The harness waits at most
   **5 minutes** for it, then exits. A `cancel` sent before
   assignment makes the harness exit cleanly without running. An
   optional `trace_context` on the assignment makes the run's trace
   a child of the control plane's span; see
   [Joining a control-plane trace](#joining-a-control-plane-trace-trace_context).
4. If the `RunConfig` opts in, loop construction sends a
   `sandbox_token_request` and blocks (fail-closed, 60 s) for the
   `sandbox_token_response` before creating the sandbox. When the
   response carries `expires_at`, further requests follow ahead of
   each expiry — at most eight per run.
5. Once the run's setup is done, the harness sends
   `HarnessEvent{type:"run_started", id, trace_reference}`, followed by
   a `user_message` carrying the run's prompt. `id` is the run ID.
   `trace_reference` is set only when the run records an OTel span
   (`traceEmitter.type: "otel"`) and holds the OTel trace ID and
   run-span ID that a feedback score or trace link targets. A `warning`
   raised during setup (a Rule-of-Two detection in the prompt, dynamic
   context or repository guidance, or a `continueOnError` `preRun` hook
   failure) is held and sent right after `run_started`. A run that
   fails setup (a fatal `preRun` hook, git setup) sends no
   `run_started`; its held warnings precede its `error`.
6. During execution the harness streams a live transcript of the run:
   `turn_started` and then `turn_committed` or `turn_abandoned` around
   each model turn, the
   model's `text_delta` and `thinking_delta` fragments, each
   `tool_call` and its `tool_result`, and a `user_message` for every
   user-role message the harness appends to the history. Occasional
   `warning` events and a `heartbeat` (every **30 seconds** by default;
   see [Detecting a stalled or orphaned run](#detecting-a-stalled-or-orphaned-run))
   are interleaved. Depending on configuration it may also emit
   `permission_request`, `tool_result_request`, `batch_submission`,
   `batch_waiting`, and `batch_cancel_request`. See
   [Watching a run live](#watching-a-run-live) for the ordering and
   delivery rules.
7. A built loop ends by emitting `HarnessEvent{type:"done",
   stop_reason, trace}`. `trace` is the run summary: outcome, token
   usage, stop details, git handoff, workspace export receipt, error,
   structured output, degraded capabilities, and the `trace_reference`
   when the run recorded an OTel span. Some early
   run failures emit a human-readable `error` event first. A task
   rejected before its run started (an invalid `RunConfig`, a loop
   build failure) gets a `done` with `stop_reason: "error"` and no
   `trace`.
8. If a follow-up grace window is configured, the stream stays open
   for `user_response` events that trigger fresh runs in the same
   process and workspace, each sending its own `run_started` with a new
   run ID (and a new trace reference when the run records an OTel
   span); otherwise the stream closes and the process exits.

### Events from the harness

| `type` | Fields | Control-plane obligation |
|---|---|---|
| `ready` | `harness_version`, `capabilities`, `id` (session echo) | Gate assignment on `capabilities` when the task depends on a contract feature; see [Compatibility](#compatibility). `harness_version` is informational. |
| `run_started` | `id` (run ID), `trace_reference` (`trace_id`, `run_span_id`, `sampled`) | Key the run's transcript on `id`. Sent once per run, primary and follow-up, after setup and before the run's first `user_message`. `trace_reference` is present only when the run records an OTel span; persist it against the run to target feedback scores or link to the trace. `sampled: false` means the span was not exported, so the backend holds no trace for it. The `done` summary repeats the reference. |
| `user_message` | `text`, `source`, `request_id` | Informational. A user-role message the harness appended to the history, after sanitisation, in append order. `text` is the appended text only (capped at 256 KiB), even when the harness merged it into a trailing user message. `source` names its origin; see [Watching a run live](#watching-a-run-live) for the vocabulary, which is open. `request_id` echoes the `user_response` the text came from, which marks that input as accepted into the history. Tool results are reported by `tool_result`, not by `user_message`. |
| `turn_started` | `turn`, `model`, `provider` | Informational. A model turn began. `turn` counts from 0 within each run. `provider` is the adapter type (for example `anthropic` or `openai-responses`), never the operator's `providers` map key. Sent once per turn, before the first provider request. |
| `text_delta` | `text` | Render / accumulate. Fragments are incremental model output, re-chunked by the harness's secret carry; see [Secrets](#secrets-on-the-stream). |
| `thinking_delta` | `text` | Render / accumulate. Incremental reasoning text, where the provider exposes it, capped at 256 KiB per event and re-chunked like `text_delta`. Opaque (redacted) reasoning is never sent, and none is sent when `reasoningSummary` is `"off"`. |
| `tool_call` | `id`, `name`, `input` (JSON bytes) | Informational. Not guaranteed to be followed by a matching `tool_result`: a cancelled, stream-faulted, stalled, or `max_tokens`-truncated turn can orphan one, so treat `done` as the terminal close for any pending state keyed on `id`. `input` is the model's raw output — scrubbed of secrets but not yet schema-validated, guardrail-checked, or stripped of prototype-pollution keys — and must be treated as untrusted, unlike `permission_request.input`. When the model's arguments are not a JSON object, `input` is `{}`, a `warning` event names the call, and its `tool_result` carries the parse error. `name` is the presented (aliased) tool name, which can differ from `permission_request.tool_name` (the internal tool ID) under a non-default toolset profile. |
| `tool_result` | `tool_use_id`, `name`, `content`, `is_error`, `duration_ms` | Informational. `name` is the same presented name as the matching `tool_call`. `is_error` is true when the call failed. `duration_ms` is the call's run time, excluding any wait for a parallel dispatch slot. `content` is capped at 256 KiB. |
| `tools_loaded` | `tools`, `source`, `name`, `input`, `turn` | Informational. Under lazy tool loading, a tool search makes deferred tools visible to the model for the rest of the run. `tools` lists their internal tool IDs, `source` says which search ran (`native` or `harness`; an open vocabulary, so preserve unknown values), `name` is the search tool's name, `input` is its input, and `turn` is the turn it ran in. `name` is informational only: OpenAI's hosted search is also named `tool_search`, so only `source` tells it from the harness search. Sent once per search that loaded a tool not already loaded: for a harness search, right after that search's `tool_result`; for a native search, when the response is accepted, after the turn's `tool_call` events and before its `tool_result` events, so a `tool_call` for a newly loaded tool can arrive before the `tools_loaded` that explains it. Either way it precedes the turn's `turn_committed` or `turn_abandoned`, and a `tools_loaded` followed by `turn_abandoned` still leaves its tools loaded. Sent by harnesses advertising the `lazy_tool_loading` capability; see [Lazy tool loading](configuration.md#lazy-tool-loading). |
| `turn_committed` | `turn`, `stop_reason`, `usage`, `tool_result_count` | Informational. The turn's assistant message, and a result for every tool call in it, are in the history. `stop_reason` is the model's stop reason for the turn, not the run outcome. A `pause_turn` stop means the next turn continues the same assistant message; see [Watching a run live](#watching-a-run-live). `usage` is the turn's token usage. |
| `turn_abandoned` | `turn`, `abandon_reason` | Informational. The turn left nothing valid in the history; discard what was buffered for it. `abandon_reason` says why; see [Watching a run live](#watching-a-run-live) for the vocabulary, which is open. Every `turn_started` is followed by exactly one `turn_committed` or `turn_abandoned`. Sent by harnesses advertising the `turn_abandoned` capability. |
| `permission_request` | `request_id`, `tool_name`, `input` | **Must respond** with `permission_response` echoing `request_id` before the policy timeout (default 60 s) or the call is auto-denied. |
| `tool_result_request` | `request_id`, `tool_use_id`, `tool_name`, `input`, `trace_context` | **Must respond** with `tool_result_response` echoing `request_id`; the loop blocks on it under a per-call timeout. `trace_context` (`traceparent`, `tracestate`), when present, is the W3C context of the call's `execute_tool` span; see [Control-plane-fulfilled tools](#control-plane-fulfilled-tools-tool_result_request). |
| `heartbeat` | — | Liveness signal every 30 s by default; `RunConfig.heartbeat_seconds` sets 5-600 s. Sent only while a run is in progress. Treat sustained absence during a run as a hang and reap the Pod; see [Detecting a stalled or orphaned run](#detecting-a-stalled-or-orphaned-run) for when none are sent. |
| `warning` | `message`, `request_id` (about a `user_response`) | Log; non-fatal. A `warning` carrying `request_id` is about that `user_response`: a message starting `user_response dropped:` means it was not accepted (empty text, the queue of 16 was full, the session was cancelled, or it was still queued when the session ended) — retry after the next turn where that applies; one starting `user_response sanitized:` means it was accepted after tag stripping or truncation to 50,000 bytes. |
| `error` | `message` | Optional diagnostic for some early run failures; when emitted by a built loop, a `done` follows. |
| `done` | `stop_reason`, `trace` | Terminal status for that run. `trace` is the run summary, including `trace_reference` when the run recorded an OTel span; it is absent on a `done` for a task rejected before its run started. |
| `batch_submission` | `request_id`, `input` (BatchSubmission JSON) | Batch mode only — see [Batch mode](#batch-mode-amortised-token-pricing). |
| `batch_waiting` | `request_id` | Batch keep-alive (roughly every 5 minutes) distinguishing "provider batch still pending" from a stall. |
| `batch_cancel_request` | `request_id` | Best-effort: cancel the provider-side batch. The harness does not wait for a reply. |
| `sandbox_token_request` | `request_id`, `audience` | See [Sandbox identity tokens](#sandbox-identity-tokens-authenticated-git-egress). |

The control-stream `run_started` event is distinct from the
`run_started` record that opens every JSONL trace file
([`trace-inspection.md`](trace-inspection.md)). The JSONL record is
always written and carries the redacted `RunConfig`; the event carries
the run ID and, when the run records an OTel span, the trace reference
instead.

### Events to the harness

| `type` | Fields | Semantics |
|---|---|---|
| `task_assignment` | `task` (RunConfig), `trace_context` (optional) | First event on the stream. Duplicate assignments are ignored. A valid `trace_context` (`traceparent`, `tracestate`) parents the primary run's span on the control plane's span; see [Joining a control-plane trace](#joining-a-control-plane-trace-trace_context). |
| `user_response` | `user_response`, `request_id` (optional) | During an active run: queued (bounded FIFO of 16) and injected as a user message at the run's next turn boundary — after pending tool results, before the next model call — in arrival order; the run continues past `end_turn` while input is queued. During the follow-up grace window: the oldest queued event starts a new run with its text as the prompt (from the previous run's history under `followUpHistory: "continue"`, see [Follow-up turns](#follow-up-turns)), and anything queued behind it joins that run at its first turn boundary. The text is sanitised like `dynamicContext` (XML/HTML tags stripped, capped at 50,000 bytes), each alteration reported by a `warning` echoing `request_id`. Empty text, a full queue, or arrival after `cancel` drops the event with a `warning`; nothing is dropped silently, including input still queued when the session ends. |
| `permission_response` | `request_id`, `allowed`, `reason` | Decision for a `permission_request`. `reason` on a denial is passed to the model as context. |
| `tool_result_response` | `request_id`, `content`, `is_error` | Result payload for a `tool_result_request`. `is_error: true` surfaces to the model as a tool failure. |
| `batch_result` | `request_id`, `content` (BatchResult JSON) | Completes a `batch_submission`. Encode failures in `content.err` — `content` is the canonical discriminator. `is_error` is optional and must agree with it. |
| `sandbox_token_response` | `request_id`, `token`, `expires_at`, `is_error`, `reason` | The signed sandbox identity JWT, or an explicit refusal. |
| `cancel` | — | End the session. An active run aborts within one turn boundary (git finalisation still runs; its `done` carries `stop_reason:"cancelled"`), queued `user_response` input is discarded with a `warning` each, and no follow-up window opens. With no run active the stream closes without another `done`. One `cancel` is always enough. |

Correlation rule: every `*_request` event carries a `request_id`
that the response **must echo verbatim**. Requests may interleave;
responses are matched by ID, not order.

### Terminal semantics

For the current `stirrup job` implementation, read
**`done.stop_reason`** as the run outcome. Values include `success`,
`error`, `max_turns`, `verification_failed`, `verification_error`,
`budget_exceeded`, `stalled`, `tool_failures`, `cancelled`, `timeout`,
`max_tokens`, and feature-specific outcomes such as `setup_failed`,
`hook_failed`, `guardrail_blocked`, and `rule_of_two_violation`.
Provider stop reasons also pass through verbatim, such as `refusal`
(the model declined to respond) and `pause_turn` (a paused server tool
turn the harness stopped continuing; see
[Paused turns](providers.md#paused-turns)).
Consumers should preserve unknown values so outcomes can be added
without breaking the protocol.

`done.trace` carries the run summary: the harness sends `done` only
after the final git checkpoint, the workspace export and the trace
upload have finished, so the summary includes their results and the
run's `error`. Obtain verifier details and final assistant text from a
configured `resultSink`, process stdout, or a trace emitter. `cost_usd`
is not calculated by the harness.
A refusal's category and explanation reach operators through the trace
emitters (the JSONL and GCS `RunTrace`, and the `stop.category`
attribute on the OTel `provider.stream` span) and the `provider refused
to respond` Warn log line. `RunTrace.stop_details` mirrors them on the
proto for when `done.trace` is populated; `RunResult` does not carry
them.

A `done` is emitted for every failure that happens after the task
assignment arrives, including a `RunConfig` rejected on arrival: the
harness emits `error` carrying the cause — config validation, secret
resolution, policy loading, sandbox-token exchange, or any other
component-construction failure — then `done{stop_reason:"error"}`,
before exiting non-zero. A pre-loop `done` carries no `trace` (no run
started) and no `RunResult` reaches the `resultSink`, so a consumer
that reads `done.trace` unconditionally must tolerate its absence.
Sending the pair is best-effort if the transport is failing, and
failures before the stream exists (dialling the control plane) have
nowhere to send it, so treat stream closure without a `done` as a
crashed or killed harness. With follow-ups enabled, each run has its
own `done`; it is terminal for the run, not necessarily for the
stream.

On an orderly exit the harness half-closes the stream and waits up to
2 seconds (not configurable) for the control plane to end the RPC before
closing the connection. Closing a gRPC connection discards frames the
writer has queued but not yet flushed, so without that wait a terminal
`done` emitted immediately before exit can be lost. A control plane that
returns from `RunTask` once it has the terminal `done` releases the
harness immediately; one that holds the stream open — serving
follow-ups, say — delays the harness's exit by that window and no
longer.

The wait is best-effort and does not upgrade `done` into a delivery
guarantee. It does not apply when the harness is terminated by
SIGTERM/SIGINT: the signal cancels the stream, so the RPC is already
dead and any terminal event still queued is discarded. A control plane
that stops reading also defeats it, since HTTP/2 flow control stalls the
writer until the window expires. Treating stream closure without a
`done` as a crashed or killed harness therefore remains correct.

Validating control-plane-side before dispatch is still worthwhile: it
turns a pod launch into an immediate API error. Mirror the harness with
`stirrup run-config --validate`, or call `types.ValidateRunConfig` from
Go.

### Transport security posture (v0.1)

The gRPC transport is **plaintext and unauthenticated** by default — the
harness dials with insecure credentials and `TransportConfig`
exposes only `{type, address}`. Acceptable deployment shapes are
same-host loopback, a private network, or mesh-provided mTLS
(Istio, Linkerd). API keys never transit the stream (they travel as
`secret://` references and outbound events are scrubbed for
secret-shaped strings), but prompts, tool results, and especially
the raw sandbox identity JWT do. Do not point a harness at a control
plane across an untrusted network without TLS, which `stirrup job` enables through `CONTROL_PLANE_TLS_*` environment variables and `stirrup harness` through `--transport-tls-*` flags (never `RunConfig` fields). The public embedding API accepts a caller-provided `Transport`,
so an embedder can supply its own authenticated transport; the built-in
TLS constructor is internal. Full discussion:
[`deployment.md`](deployment.md#transport-security-posture-v01).

## A minimal control plane

The smallest correct `RunTask` implementation:

1. Accept the stream; wait for `ready`.
2. Send `task_assignment` with a valid `RunConfig`.
3. Consume events. Log `text_delta` / `tool_call` / `tool_result`;
   track `heartbeat` arrival times.
4. Answer every `permission_request` (even a blanket deny keeps the
   contract; an unanswered request stalls the tool call until the
   timeout auto-denies it).
5. On `done`, record `stop_reason` and close. (A server that supports
   follow-up runs keeps the stream open instead.)

[`examples/controlplane/`](../examples/controlplane) is a runnable
implementation of these steps built on the generated stubs. It assigns a
`RunConfig` from a file, answers permission requests by flag (allow, deny, or
ask on the terminal), prints events and exits with a status that reflects the
outcome. Its `smoke.sh` runs one task through `stirrup job` against a scripted
provider with no API key. The example is plaintext and unauthenticated; it
supports one run and does not opt into sandbox identity, batching, async
results, or follow-ups. Add handlers for those events before enabling the
corresponding config.

Launch the harness as a Kubernetes Job running `stirrup job` with
`CONTROL_PLANE_ADDR` pointing at this server. Set
`Job.spec.activeDeadlineSeconds` above `RunConfig.timeout`, allowing
additional time for the pre-assignment wait (up to five minutes),
component construction, post-run hooks, result/export flushing, and —
with a follow-up grace window — each follow-up run's own `timeout`
plus the idle windows between runs.
The process writes `/tmp/healthy`, but the published distroless image
contains no `test` or shell binary; inspect that file from a sidecar
sharing the volume, or use a custom image/probe helper. The wire
heartbeat is the control plane's direct run-liveness signal.

### Detecting a stalled or orphaned run

The harness emits a `heartbeat` event every 30 seconds while a run is in
progress. `RunConfig.heartbeat_seconds` (JSON `heartbeatSeconds`,
`--heartbeat-seconds` on the CLI) sets another interval: `0` or unset
keeps 30 s, and any other value must be between 5 and 600. The interval
applies to the primary run and to every follow-up run in the session.
A harness whose `ready.capabilities` lacks `heartbeat_interval` ignores
the field and keeps 30 s (see [Compatibility](#compatibility)).

Heartbeats cover the run itself. They start when the loop begins the
run, after component construction, and stop when the run's context ends
(its `timeout` or a `cancel`) or just before `done`, whichever comes
first. None are sent while the harness waits for the assignment, while
it builds the loop, between runs (including the follow-up grace window),
or during `postRun` hooks that outlive the run's deadline (see
[`security.md`](security.md#lifecycle-hooks)).

A control plane that reaps a Pod after missing heartbeats should pick a
deadline of several intervals, not one, so a single delayed event does
not trigger a reap. The heartbeat is emitted by a background goroutine
rather than by the turn loop, so it continues through a long model call
or tool execution. A shorter interval raises event volume on
the stream accordingly. `batch_waiting` events are a separate liveness
signal during batch waits (see [Batch mode](#batch-mode-amortised-token-pricing)).

## Composing a RunConfig

The `RunConfig` in `task_assignment` is the composition root — it
selects every component of the run. Crucially, **a wire-delivered
`RunConfig` skips the CLI's convenience defaulting**
(`applyModeDefaults` runs only in the CLI path). The harness
validates it (`ValidateRunConfig`) but a control plane must be
explicit about fields the CLI would have filled:

| Field | Wire requirement |
|---|---|
| `run_id` | Set it. Used in traces, sandbox token `sub` claims, and GCS object names. No path separators, `..`, or control bytes. |
| `mode` | Required: `execution`, or a read-only mode (`planning`, `review`, `research`, `toil`). |
| `prompt` | Required. |
| `provider.type` | Required: `anthropic`, `bedrock`, `openai-compatible`, `openai-responses`, `gemini`. |
| `max_turns` | Required, 1–100. The CLI default of 20 is **not** applied on the wire. |
| `timeout` | Required, 1–3600 seconds, per run: the primary run (component construction included) and, afresh, each follow-up. The CLI default of 600 is **not** applied on the wire. |
| `permission_policy.type` | Required for `execution`; validation rejects an empty type rather than inferring the CLI's `allow-all`. Read-only modes default to `deny-side-effects`. |
| `tools.built_in` | Required (non-empty) for read-only modes, and must exclude the mutating tools. Optional for `execution` (empty enables all built-ins). |

Validation fills sensible defaults for the rest: `editStrategy.type`
→ `multi`, `codeScanner` → `patterns` in execution mode / `none` in
read-only modes, `permissionPolicy.type` → `deny-side-effects` in
read-only modes, provider retry policy → 3 attempts with capped
exponential backoff. `executor.type` defaults to `local`. Defaults
that only ever loosen a run are left to the caller: `allow-all` is
what the CLI picks for `--mode execution`, but the harness will not
infer it from an omitted field.

Built-in tool names accepted in `tools.built_in`:

```
read_file  write_file  edit_file  search_replace  apply_diff
list_directory  grep_files  find_files
git_status  git_changed_files  git_diff  git_show
run_command  read_command_output  web_fetch  spawn_agent
```

The mutating set rejected in read-only modes is `write_file`,
`run_command`, `edit_file`, `search_replace`, `apply_diff`.
`read_command_output` additionally requires
`tools.commandOutput.enabled`; that command-output config is not yet
available on the proto.

Secret **values** must never appear in a `RunConfig`; references and
credential-federation settings do. When an `apiKeyRef` is used, it must
be a `secret://` reference — `secret://NAME` (environment variable on
the harness), `secret://file:///path`, or
`secret://ssm:///param-name` (AWS SSM) — resolved harness-side at
runtime. A literal key is rejected by validation. Production trace
emitters persist `RunConfig.Redact()` rather than the live config;
debug builds can explicitly relax trace redaction. Cross-cloud
alternatives (IRSA, GKE Workload Identity,
Azure/Anthropic/OpenAI WIF) are configured via
`provider.credential`; see
[`credential-federation.md`](credential-federation.md).

## Cookbook

Each recipe uses protobuf JSON's lower-camel-case field names, which
also work in CLI config files. Fields covered by an earlier recipe are
elided with `…`; those fragments are explanatory, not complete
standalone JSON documents.

### Read-only triage run (safe default posture)

Goal: investigate, review, or plan without any write surface.

```json
{
  "runId": "triage-8f2e",
  "mode": "review",
  "prompt": "Review the diff between main and release-1.2 for regressions.",
  "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "modelRouter": { "type": "static", "model": "claude-sonnet-4-6" },
  "permissionPolicy": { "type": "deny-side-effects" },
  "tools": { "builtIn": ["read_file", "list_directory", "grep_files", "find_files", "git_status", "git_diff", "git_show"] },
  "maxTurns": 20,
  "timeout": 600
}
```

- Read-only modes structurally cannot hold `allow-all` (nor a
  Cedar fallback of `allow-all`) or mutating tools —
  `ValidateRunConfig` rejects the config, not just the call.
- `deny-side-effects` still permits `web_fetch` and `spawn_agent`
  when listed; gate them with `ask-upstream` or Cedar if that
  matters.
- The `api` executor (`executor.type: "api"` + `vcsBackend`) can read
  a GitHub or GitLab repository without a local workspace. Content
  tools operate through the remote API, but the `git_*` tools require a
  real checkout, so omit them for an API-executor run.

### Editable run in a container sandbox

Goal: let the agent write code and run commands inside a hardened
container instead of the harness host.

```json
{
  "runId": "fix-1234",
  "mode": "execution",
  "prompt": "Fix the failing test in main_test.go.",
  "provider": { "…": "…" },
  "executor": {
    "type": "container",
    "image": "ghcr.io/rxbynerd/stirrup-sandbox:latest",
    "runtime": "runsc",
    "network": { "mode": "none" },
    "resources": { "cpus": 2.0, "memoryMb": 2048, "diskMb": 8192, "pids": 256 }
  },
  "permissionPolicy": { "type": "ask-upstream" },
  "gitStrategy": { "type": "deterministic" },
  "maxTurns": 40,
  "timeout": 1800
}
```

- The container is hardened unconditionally: `CapDrop ALL`,
  `no-new-privileges`, a read-only rootfs, and the nobody uid.
  `runtime: "runsc"` (gVisor) adds a kernel boundary when that OCI
  runtime is installed on the host.
- Sandbox executors require an explicit `network` block. Use
  `network.mode: "none"` for no network. `mode: "allowlist"` plus
  `network.allowlist` starts an in-process egress proxy enforcing an
  FQDN allowlist.
- `gitStrategy: "deterministic"` creates a branch from the run ID
  and commits the result — the natural handoff artifact for a
  control plane that opens PRs.
- On Kubernetes, use `executor.type: "k8s"` (or `k8s-sandbox` for
  the Agent Sandbox CRD) with `k8sNamespace` etc.; egress is
  enforced by a per-Pod NetworkPolicy plus a shared proxy
  Deployment (`stirrup egress-proxy`), wired via
  `k8sEgressProxyUrl`. See [`executors/k8s.md`](executors/k8s.md).

### Human-in-the-loop approvals (`ask-upstream`)

Goal: a human (or policy service) approves each side-effecting tool
call.

```json
"permissionPolicy": { "type": "ask-upstream", "timeout": 120 }
```

- Approval-required tools (all mutating tools, plus `web_fetch` and
  `spawn_agent`) emit `permission_request`; everything else is
  auto-allowed.
- Respond within `timeout` seconds (0 = default 60) or the call is
  denied fail-closed. A timeout surfaces to the model as a tool
  error; an explicit deny surfaces as `Permission denied: <reason>`.
- Denials with a good `reason` steer the model; a bare deny often
  just gets retried differently.
- MCP-prefixed tools (`mcp_*`) are always treated as
  approval-required under this policy.

### Cedar policy engine (rules, not round-trips)

Goal: express the approval decision as reviewable policy instead of
answering every request interactively.

```json
"permissionPolicy": {
  "type": "policy-engine",
  "policyFile": "/etc/stirrup/policy.cedar",
  "fallback": "deny-all"
}
```

- `policyFile` is read by the **harness process** during loop
  construction. Stage it via a ConfigMap mount or bake it into the
  image. Policy text cannot be shipped inside the `RunConfig`.
- Cedar answers allow/deny/no-decision; `fallback` decides
  no-decisions. A permit-based allowlist is only meaningful with
  `fallback: "deny-all"` — the default `deny-side-effects` would
  still allow non-mutating gated tools.
- Policies are linted at load; error-severity findings (dead
  clauses, unanchored wildcard hosts) abort the run before any turn.
- Starter policies: [`examples/policies/`](../examples/policies/).
  Semantics: [`safety-rings.md`](safety-rings.md#ring-3--cedar-policy-engine-per-call-authorization).

### Injecting task context

Goal: give the agent issue bodies, retrieved documents, or customer
records while marking the content as untrusted.

```json
"dynamicContext": {
  "issue_body": { "value": "…external issue text…" },
  "customer_record": { "value": "…", "sensitive": true }
}
```

- Every entry is sanitised (tag stripping, 50 KB cap) and wrapped in
  `<untrusted_context>` blocks. The system prompt tells the model to
  treat it as data rather than instructions; the delimiter is a prompt
  defence, not a hard security boundary.
- `sensitive: true` per entry — or `sensitiveData: true` at the top
  level — declares the "sensitive data" leg of the Rule of Two.
  Declare honestly: the harness deliberately does not infer
  sensitivity from credentials it manages itself.

### Rule of Two: the posture invariant

A run should not simultaneously hold **untrusted input**, **sensitive
data**, and **external communication**. `ValidateRunConfig` rejects
the all-three combination unless the policy is `ask-upstream` or the
operator explicitly sets `ruleOfTwo.enforce: false`; two legs produce
a `rule_of_two_warning` security event.

Control-plane levers:

- Declare sensitivity via `sensitiveData` / per-entry `sensitive`
  (above).
- When untrusted input + external communication hold and no
  sensitivity was declared, a runtime classifier auto-arms and
  watches conversation content; on detection the default action
  revokes external-communication tools for the rest of the run
  (`block-external`). `ruleOfTwo.runtime.onDetect` selects
  `block-external` | `ask-upstream` (gRPC transport only) |
  `redact` | `abort` | `warn`.
- `ruleOfTwo: { "enforce": false }` bypasses the invariant and
  emits an auditable `rule_of_two_disabled` security event. There
  is deliberately no CLI flag for this.

Full design: [`safety-rings.md`](safety-rings.md#ring-4--rule-of-two-pre-flight-invariant--runtime-classifier).

### Custom system prompts

Goal: control the system prompt from the control plane.

```json
"systemPromptOverride": "You are the refund-triage agent. …"
```

- `systemPromptOverride` replaces the shipped mode prompt verbatim —
  never template-parsed, so prompts compiled by an external
  prompt-management system (e.g. Langfuse) pass through untouched.
  Structural sections (workspace path, turn budget, dynamic
  context) are still appended.
- Alternatively `promptBuilder.template` supplies a Go
  `text/template` rendered against `.Model` / `.Mode` / `.Tier` /
  `.ModelIs` for model-conditional prompts, and
  `promptBuilder.promptModel` pins which model identity the shipped
  templates render for (prompt/model comparison runs). The override
  is mutually exclusive with both.
- Preview offline with `stirrup prompt render --mode execution`.
  Reference: [`configuration.md`](configuration.md#system-prompt-templating).

### Model routing and multi-provider

Goal: cheap model for easy turns, expensive model when the run gets
hard — or different providers per mode.

```json
"providers": {
  "anthropic": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "local":     { "type": "openai-compatible", "baseUrl": "http://vllm:8000/v1", "apiKeyRef": "secret://VLLM_KEY" }
},
"modelRouter": {
  "type": "dynamic",
  "provider": "local",    "model": "qwen-3.6",
  "cheapProvider": "local",    "cheapModel": "qwen-3.6",
  "expensiveProvider": "anthropic", "expensiveModel": "claude-sonnet-4-6",
  "expensiveTurnThreshold": 5,
  "expensiveTokenThreshold": 50000,
  "cheapStopReasons": ["end_turn"]
}
```

- Router types: `static` (one model), `per-mode` (`modeModels` map),
  and `dynamic`. The dynamic router chooses the cheap model when the
  previous stop reason is in `cheapStopReasons`, the expensive model
  after a configured turn/output-token threshold, and the default
  model otherwise. `expensiveTokenThreshold` compares the run's
  cumulative output tokens, which include reasoning tokens (Gemini
  thought tokens included), so thinking-heavy runs reach it sooner.
- Behaviour knobs are provider-neutral: `temperature`,
  `reasoningEffort`, and `reasoningSummary` sit at the top level of
  `RunConfig`. Adapters with a supported native control project them;
  other adapters may ignore `reasoningEffort`. `reasoningSummary: "off"`
  stops reasoning text reaching the stream as `thinking_delta` events
  on every provider; see
  [Reasoning summaries](configuration.md#reasoning-summaries).
- Provider-specific wire quirks (Azure headers/query params, Z.ai
  compat profile, the OpenAI-wire endpoint profile, Gemini safety
  settings) live on `ProviderConfig`.
  See [`providers.md`](providers.md).

### Budgets and limits

```json
"maxTurns": 40,
"timeout": 1800,
"maxTokenBudget": 2000000,
"contextStrategy": { "type": "summarise", "maxTokens": 150000 }
```

- The token budget is enforced between provider calls and terminates
  the run with `outcome: "budget_exceeded"`; wall-clock expiry yields
  `timeout`. Ceilings are 50 M tokens and 3600 s.
- `maxCostBudget` is accepted and capped at $100, but the harness does
  not currently calculate cost or enforce that budget; setting it logs a
  WARN at config validation. `RunTrace.cost_usd` is unpopulated for the
  same reason. Enforce money limits in the control plane using provider
  billing/pricing data.
- `contextStrategy` bounds the conversation itself:
  `sliding-window` (default), `summarise`, `offload-to-file`, or `clear-tool-results`.
- Pair `timeout` with an infrastructure-level deadline
  (`activeDeadlineSeconds`) that also allows for assignment, setup,
  and teardown — and, with follow-ups enabled, for every additional
  run's own `timeout` plus the grace windows between runs, since
  `timeout` is per run rather than per session.

### Verification: use the terminal outcome

```json
"verifier": {
  "type": "composite",
  "verifiers": [
    { "type": "test-runner", "command": "go test ./...", "timeout": 300 },
    { "type": "llm-judge", "criteria": "The diff addresses the issue without unrelated changes." }
  ]
}
```

A model turn that ends `end_turn` but fails verification produces
`done.stop_reason: "verification_failed"`; the control plane can retry,
escalate to a stronger model, or route to a human without parsing model
output.

### Follow-up turns

Goal: run another prompt in the same process and workspace without
re-provisioning the sandbox.

- Set a positive `followUpGrace` (seconds, ≤ 3600) in the `RunConfig`,
  or set `STIRRUP_FOLLOWUP_GRACE` in the Pod environment. A positive
  config value wins; zero or unset falls back to the environment.
- After `done`, the stream stays open for the grace window. A
  `user_response` starts a new run with a new run ID and the response
  text as its prompt. What that run starts from is set by
  `followUpHistory`:
  - `"reset"` (the default; an empty value selects it) starts each
    follow-up from its prompt alone. The previous run's conversation is
    not visible to the model.
  - `"continue"` starts each follow-up from the previous run's
    committed history with the prompt appended, so a session of
    follow-ups is one conversation. A turn is committed once its
    assistant message is in history and every tool call in it has a
    result. A turn cut off mid-dispatch is dropped together with
    everything after it. A turn that ends the run without dispatching
    its tool calls is kept without them when anything else remains: any
    text or reasoning for most stops, and text alone for a turn cut off
    at the output limit. Otherwise it is discarded. When the history
    ends in a user message (tool results, or verifier feedback), the
    prompt joins that message rather than following it.
    Tool results the previous run had not yet screened, because it
    ended before its next turn boundary, are screened before the
    follow-up's first model call, with the Rule-of-Two and guard
    handling a turn boundary applies. A run that ended
    `rule_of_two_violation` or `guardrail_blocked` passes no history
    on: the next follow-up starts from its prompt alone and the harness
    sends a `warning` saying so. The system prompt, dynamic context and
    repository guidance are rebuilt for every run and are not part of
    the carried history. Anthropic rejects a signed thinking block
    replayed under a system prompt or tool list other than the one it
    was produced under, and the rebuilt system prompt often differs
    because it lists the workspace's files and git status. A follow-up
    whose system prompt or tools differ from those the carried history
    was last sent with therefore drops the carried thinking blocks;
    its own thinking is replayed as usual. Each run re-sends the carried history, so its
    input tokens count against that run's `maxTokenBudget`, and the
    configured context strategy compacts a long session the same way it
    compacts a long run. The Rule-of-Two latch spans the whole session
    in both modes: once tripped, it stays tripped for every later run.
    Every run that continues the history sends the prompt-cache key of
    the run that produced it, normally the primary run's for the whole
    session, so providers that route on the key keep the shared prefix
    cached. A follow-up that starts from its prompt alone gets a key of
    its own, and later continuing follow-ups reuse that key. Tool calls
    in the carried history count as tool use for tool-choice escalation
    (`toolChoiceEscalation`): a continued follow-up that answers without
    a tool is not retried when that history holds at least one tool
    call. The tool-call count and the retry cap restart in every run,
    and again after a verification retry or output-schema correction
    within it, so the count then starts from the carried history's tool
    calls alone.

  The primary run is the same in both modes, and the field does nothing
  without a follow-up window. It takes effect only through the
  follow-up loop that serves that window: an embedder that calls
  `harnessapi.Loop.Run` repeatedly gets no carried history and no
  warning.
- On the stream, every follow-up run in either mode opens with its own
  `run_started` and a `user_message` with source `followup_prompt`
  whose `text` is the new prompt alone, even when `"continue"` merges it
  into the carried history's last message, and whose `request_id`
  echoes the `user_response` that started the run. Its turns count from
  0 again. The carried history is not re-sent as events: a control
  plane rendering the session as one conversation joins the runs' event
  sequences in order. The `warning` for history that is not continued
  arrives after that run's `run_started`. See
  [Watching a run live](#watching-a-run-live).
- A `user_response` sent during an active run — primary or follow-up —
  is not a follow-up: it is queued (up to 16) and injected as a user
  message at that run's next turn boundary, after any pending tool
  results and before the next model call, in arrival order. A run does
  not end while input is queued: an `end_turn` with input waiting
  becomes another turn instead of `done`, so a control plane that keeps
  talking keeps the run alive, bounded by `maxTurns` and `timeout`. Mid-run
  input gets the `dynamicContext` treatment on arrival — XML/HTML tags
  stripped, capped at 50,000 bytes, each alteration reported by a
  `warning` — and is then screened like the initial prompt: Rule-of-Two
  observation, and the pre-turn guard, which classifies it regardless of
  its length (the `MinChunkChars` skip does not apply). Empty text, or
  input beyond the queue bound, is rejected with a `warning` echoing the
  event's `request_id`. Each accepted follow-up ends with its own `done`.
  A control plane relaying end-user chat into `user_response` is placing
  untrusted text on the operator channel; the guard screens a scrubbed
  copy of it, but the model treats it as the operator's instruction and
  reads token-shaped text the guard could not see unfenced (see
  [Guard input](guardrails.md#guard-input)).
- Each follow-up is a run in its own right: it receives a fresh `timeout`
  budget when its `user_response` is taken up, and the grace timer
  restarts after it completes, so the window measures idle time and a
  follow-up that outlives the grace period does not close it. `stirrup
  job` has no session-wide cap of its own; bound the Pod with
  `activeDeadlineSeconds`. (`stirrup harness` bounds its own session at
  10 × (`timeout` + `followUpGrace`) after the primary run.) The
  precedence of every limit is tabulated in
  [`deployment.md`](deployment.md#run-budgets-and-precedence).
- Every run — primary and each follow-up — is finalised the same way:
  its `RunResult` is emitted on the configured `resultSink` exactly once,
  and, when `executor.workspaceExportTo` is set, its workspace is
  exported once the run completes. The primary run exports to the
  configured URI verbatim; each follow-up exports to that URI with its
  own run ID inserted as the path segment before the object name
  (`gs://bucket/runs/r1/workspace.tar.gz` →
  `gs://bucket/runs/r1/<followUpRunId>/workspace.tar.gz`), so successive
  tarballs never overwrite each other. The follow-up's run ID is the
  `runId` on its `RunResult`. Export failures follow the same
  `executor.workspaceExportRequired` policy as the primary run.
- A `cancel` ends the session wherever it lands: during a run it
  cancels that run and no grace window opens afterwards; during the
  grace window it closes the stream promptly without an extra `done`.
- A follow-up whose run fails outright (the loop returns an error, as
  opposed to a non-success outcome such as `timeout`) ends the session;
  the failure is on that run's `done` and `RunResult`.

### Watching a run live

Goal: render a run as it happens, from the `RunTask` stream alone,
including the model's text and reasoning, every tool call with its
result, every user-role message the harness appends, and each turn's
token usage.

No configuration is needed. The transcript events are sent for every
run regardless of `traceEmitter.type`, and the trace emitters keep
recording as before; the stream is the live view, the trace is the
durable record.

**Design.** The transcript is a series of incremental events on the
existing `RunTask` stream, not a new RPC and not an upload of the
whole transcript at the end of the run. A terminal upload would arrive
only after the run and, on a long run, would exceed grpc-go's 4 MiB
default message limit; each event stays far below it, because
`tool_result` content, `tool_call` input, `user_message` text and
`thinking_delta` text are each capped at 256 KiB and deltas are small.
The new event types and fields are additive, so a control plane that
does not know them ignores them and the RPC is unchanged. The stream
and the trace emitters are independent of each other, as
[`philosophy.md`](philosophy.md) sets out: the stream is for real-time
control, telemetry for long-term analysis. Delivery and keys are
described below.

**Delivery.** The events of a run arrive on one ordered stream, from
its `run_started` to its `done`. Delivery is at most once: the harness
does not acknowledge, retry or replay events, and a stream that breaks
cannot be resumed, since losing the control stream ends the run with
`control_stream_lost`. A consumer that needs a complete record reads
it from a trace emitter or the `resultSink` after the run, not from
the stream.

**Keys.** The run ID from `run_started.id` identifies everything up to
that run's `done`. Within a run, `turn` numbers the model turns from 0;
it restarts at 0 for each follow-up run. `tool_use_id` joins a
`tool_result` to its `tool_call`, and `request_id` joins a
`user_message` to the `user_response` it delivered.

**Shape of a run.**

```text
run_started            id
user_message           source: initial_prompt (or followup_prompt)
turn_started           turn 0, model, provider (adapter type)
  thinking_delta …     reasoning text, where exposed
  text_delta …         answer text
  tool_call …          one per call the model made
  tools_loaded …       one per native tool search that loaded tools
  tool_result …        one per call, in call order; each harness
                       tool_search result is followed by its tools_loaded
turn_committed         turn 0, stop_reason, usage, tool_result_count
                       (or turn_abandoned: turn 0, abandon_reason)
user_message …         anything the harness appends before the next turn
turn_started           turn 1 …
…
done                   stop_reason (the run outcome), trace
```

Deltas and tool events between a `turn_started` and the turn's
`turn_committed` or `turn_abandoned` belong to that turn. A
`user_message` never belongs to a turn: it follows the turn's
`turn_committed` or `turn_abandoned` and precedes the next
`turn_started`. `warning` and `heartbeat` events can arrive anywhere,
except that a Rule-of-Two or `continueOnError` `preRun` hook warning
raised during the run's setup is held until `run_started` is sent.

**Committed and uncommitted turns.** `turn_committed` means the
turn's assistant message, and a result for every tool call in it, are
in the run's history, which later turns build on. A turn that ends the
run without dispatching its tool calls commits without them, so a
`tool_call` event streamed in such a turn has no `tool_result` and is
not part of the history. A turn that does not commit is closed by
`turn_abandoned` instead, before the next `turn_started` or `done`.
Its deltas were shown live but are not part of the conversation the
model continues from. Tools that did run still had their effects on
the workspace. `abandon_reason` says why, from an open vocabulary;
preserve values that are not recognised:

| `abandon_reason` | Meaning |
|---|---|
| `no_response` | The turn ended before a usable response arrived: the provider request failed, the run was cancelled or timed out, or the context window or token budget was exhausted. |
| `empty_response` | The response held nothing that may enter the history: an empty message, or one cut off at the output limit with no text. A turn of tool calls alone that is not dispatched lands here too. |
| `missing_tool_calls` | The response stopped for tool use but carried no tool calls. |
| `unanswered_tool_calls` | A stall or the consecutive-failure limit stopped dispatch before every tool call in the turn had a result. |
| `guardrail_blocked` | The post-turn guard rejected the answer. |

A harness without the `turn_abandoned` capability sends nothing in
its place: there, a `turn_started` that reaches the next
`turn_started` or `done` without a `turn_committed` is a turn that did
not commit.

**Paused turns.** A turn committed with `stop_reason` `pause_turn` was
paused by Anthropic's server tool loop, typically after about ten
native tool searches. The harness continues it in the next turn,
sending no `user_message` in between and keeping the paused turn's
model. The continuation's content extends the paused turn's assistant
message in the history rather than starting a new one, so a consumer
that keeps one entry per assistant message appends the next turn's
content to the paused entry. On the run's last allowed turn, a due
budget wind-down notice is sent instead, as a `user_message` with
source `wind_down`, and that turn's content starts a new assistant
message. After five consecutive continuations, a further pause ends
the run with outcome `pause_turn` and a `warning`, as does a paused
turn with no content, which is reported as `turn_abandoned`; see
[Paused turns](providers.md#paused-turns).

Within one turn, a provider failure that the harness retries, or a
request it compacts and resends after a context-window overflow,
sends a `warning` before the next attempt; any deltas and `tool_call`
events the failed attempt streamed are not part of the turn. The retry
keeps the turn's provider and model and sends no second
`turn_started`.

**User messages.** `source` names where a `user_message` came from.
The vocabulary is open: preserve values that are not recognised.
Every source other than `initial_prompt`, `followup_prompt` and
`user_response` is text the harness wrote, and new values will
normally be harness-written too, so a consumer can render an
unrecognised source in its harness-message style. `text` holds the
message's text blocks only. A consumer that joins the `text` of every
event regardless of type also picks up `user_message` and
`thinking_delta` text, so it filters on `type`.

| `source` | Appended when |
|---|---|
| `initial_prompt` | The primary run starts; the text is the prompt. |
| `followup_prompt` | A follow-up run starts; the text is the `user_response` that started it, whose `request_id` it echoes. |
| `user_response` | A `user_response` sent during a run is injected at a turn boundary. Several queued inputs join the history as one message but are reported as one event each. |
| `verifier_feedback` | The verifier failed and its feedback is sent back to the model. |
| `structured_output_correction` | The final answer failed `outputSchema` and the model is asked to correct it. |
| `tool_choice_escalation` | A turn that should have called a tool did not, and the escalation policy prompts the model. |
| `stall_nudge` | The model repeated the same tool-call pattern and is told to change approach. |
| `wind_down` | A budget is nearly exhausted and the model is told to finish up. |
| `output_limit_recovery` | A turn stopped at the output limit and the model is told how to continue. |

A `user_message` reports text as it was appended to the history, not
that the model will act on it:

- `user_response` is reported only once the input passes screening. An
  input the Rule-of-Two monitor or the pre-turn guard stops is never
  reported, so its `request_id` is not echoed; the run ends and `done`
  carries `rule_of_two_violation` or `guardrail_blocked`.
- `initial_prompt`, `followup_prompt` and `verifier_feedback` are
  reported before the screening of the turn that delivers them, so a
  run can still end `rule_of_two_violation` or `guardrail_blocked`
  after one; the outcome on `done` supersedes it. That includes the
  prompt of a follow-up whose carried history ends the run when it is
  screened.
- The other sources are harness-authored text, reported exactly as
  appended.

When a run's last turn leaves no assistant message, for example an
empty `end_turn`, the history ends on a user message. Text the harness
appends next joins that message rather than following it, so the
next request never holds two user messages in a row. The output-schema
check treats the missing answer as a final message with no text and
asks for a correction, and the verifier runs as after any other
answer; an `llm-judge` verifier then sees no final answer. Verifier
feedback merged this way is screened when it is appended, not at the
next turn, and stays a text block of its own marked with the
`verifier_feedback` origin, so the `llm-judge` verifier leaves it out
and the compaction summariser labels it harness text, as they do for a
separate feedback message.

A Rule-of-Two redaction or guardrail replacement applied to the content
afterwards, as with tool results, is not re-announced. `onDetect:
redact` protects the model's context, not the control stream:
`verifier_feedback` text and `tool_result` content reach the stream
with only the transport's secret scrubbing applied, so latch-tier spans
the scrubber does not recognise (card numbers, IBANs, anchored SSNs)
are sent as they are.

**Reasoning.** `thinking_delta` carries reasoning text only where the
provider returns it in readable form; a model that returns only an
opaque signature, such as `claude-sonnet-5-5` by default, sends none.
Reasoning is model output and gets the same handling as `text_delta`.
`reasoningSummary: "off"` stops every `thinking_delta` without changing
what the model is replayed; see
[Reasoning summaries](configuration.md#reasoning-summaries) for what
each provider returns under `"auto"` and when the field is unset.

**Field caps.** Tool payloads, `user_message` text, `thinking_delta`
text and `message` are capped at 256 KiB with a truncation marker.
The name and ID fields (`id`, `name`, `tool_use_id`, `request_id`,
`tool_name`) and `stop_reason` are capped at 1 KiB on every event, cut
without a marker so equal values stay equal: a capped `tool_call.id`
still matches its `tool_result.tool_use_id`, but a `request_id` longer
than 1 KiB comes back shortened and does not match the one sent.
`source` is capped like a name field. `tools_loaded.tools` holds at
most 256 entries, each capped like a name field, and
`tools_loaded.input` is capped like a `tool_call` input.

<a id="secrets-on-the-stream"></a>**Secrets.** Every text field is scrubbed of recognised secret formats
before it is sent. `text_delta` and `thinking_delta` fragments are not
the provider's deltas and have no stable size or count: concatenated
in order they give the model's text with recognised secrets redacted,
including a secret split between two provider deltas. Text a secret
pattern could still match, such as a partly written token or an open
key block, is held back and sent no later than the next event of the
model's response (`tool_call`, `turn_committed`, `turn_abandoned`,
`done`) or the end of the stream; ordinary prose is sent as it arrives. `heartbeat` and
`warning` events raised outside the response, such as one for a dropped
`user_response`, are not ordered against held text and can arrive
first. There is no timer, so delta silence is not a stall signal: use
`heartbeat`. A possible match longer than 8 KiB is sent as one
`[REDACTED]` from its start, and nothing more of it is sent; one still
open after 1 MiB also brings a `warning` saying the stream is withheld
until it ends. See [`security.md`](security.md#control-stream-scrubbing)
for both bounds.
The `done` trace's final assistant text is scrubbed whole.

**Capability gating.** A harness that predates these events sends none
of `user_message`, `turn_started`, `turn_committed` or
`thinking_delta`, leaves `name`, `is_error` and `duration_ms` unset on
`tool_result`, and sends `run_started` only for runs that record an
OTel span. A control plane that depends on the transcript gates task
assignment on `live_transcript` in `ready.capabilities`, and ignores
event types it does not recognise so later additions do not break it.
One that relies on an explicit close for every turn gates on
`turn_abandoned`.

### Cancelling a run

Send `ControlEvent{type:"cancel"}` at any point after assignment; it
ends the session. During an active run the harness cancels in-flight
provider streams and tool calls via context, runs git finalisation, and
emits `done` with `stop_reason:"cancelled"`; any `user_response` input
queued for that run is discarded, each reported by a `warning`. No
follow-up window opens after a cancelled run. With no run active —
between a run's `done` and the next run, or inside the grace window —
the stream closes without another `done`. One `cancel` is always
enough; a control plane never needs to send it twice. Before
assignment, `cancel` exits cleanly without `done`. Cancellation during synchronous
component construction is not a reliable boundary, so retain an
infrastructure deadline/SIGTERM fallback. On process shutdown, the job
uses bounded contexts to flush traces and the result sink.

### Control-plane-fulfilled tools (`tool_result_request`)

Goal: give the model a tool whose *result* the control plane
computes — a memory store, an internal search index, a ticket
system — without the harness holding credentials for it or the
sandbox reaching it over the network.

```json
"tools": {
  "builtIn": ["read_file", "run_command"],
  "controlPlane": [
    {
      "name": "search_memory",
      "description": "Search long-term memory for prior events and facts.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "query": {"type": "string"},
          "limit": {"type": "integer"}
        },
        "required": ["query"]
      }
    },
    {
      "name": "save_memory",
      "description": "Persist a memory for future runs.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "content": {"type": "string"},
          "kind": {"type": "string", "enum": ["event", "fact"]}
        },
        "required": ["content"]
      },
      "timeoutSeconds": 30,
      "requiresApproval": true
    }
  ]
}
```

Each entry is registered as an async tool. The model sees `name`,
`description`, and `inputSchema` exactly as it would a built-in. A
call validates the input against the schema harness-side, then emits
`tool_result_request` (`request_id`, `tool_use_id`,
`tool_name` = `name`, `input` = the validated JSON) and blocks on the
matching `tool_result_response`. Concurrent calls in one turn fan out
under the `toolDispatch.maxParallel` semaphore (default 4, ceiling
16), so the control plane may receive several requests before it has
answered the first.

Control-plane responsibilities:

- Answer every request with `tool_result_response` echoing
  `request_id`. `content` reaches the model verbatim (truncated with
  a marker beyond 1 MiB); choose a shape the description tells the
  model to expect, JSON or prose. `is_error: true` surfaces `content`
  to the model as a failed tool call, after secret scrubbing; the
  model may retry or move on, and the run continues.
- Answer within the per-entry `timeoutSeconds` (0–3600; `0` selects
  the harness default of 60 s). Expiry is a tool failure the model
  sees, not a run failure. A response arriving after expiry is
  dropped.
- Expect no request after `cancel` or the run timeout: dispatch
  aborts under the run context, the model sees a cancelled tool
  failure, and the run winds down as usual. A request that could not
  be written to the stream (transport disconnect) fails the call
  the same way.
- Optionally parent the work for a call on `trace_context`, the W3C
  `traceparent` and `tracestate` of the call's `execute_tool` span.
  A span started from it, exported to the same backend, nests under
  the tool call in the run's trace. Each concurrent call carries its
  own span. The field is absent when the call has no valid span
  context. Under a trace emitter that records no spans, such as
  `jsonl`, a run with an upstream parent opens no tool spans, so every
  `tool_result_request` in the run carries that same upstream parent
  (see [Joining a control-plane trace](#joining-a-control-plane-trace-trace_context)).
  It carries causality only: correlate the response by
  `request_id`, never by trace or span ID. See
  [`observability-cloud.md`](observability-cloud.md#trace-context-exchanged-with-a-control-plane).

Validation (`ValidateRunConfig`) rejects the block unless
`transport.type` is `grpc` — a config arriving in `task_assignment`
satisfies that by construction, the CLI and Cloud Run topologies
cannot use it — and rejects a `name` that collides with a built-in
tool, carries the `mcp_` prefix, repeats within the list, or fails
the provider-portable grammar (`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`);
`description` and an object-typed `inputSchema` are required.

Posture notes:

- Control-plane tools never mutate the workspace, so read-only
  modes (`planning`, `review`, `research`, `toil`) admit them beside
  an explicit `tools.builtIn` list.
- `requiresApproval: true` sends each call through the permission
  policy first — a Cedar `forbid` denies it, `ask-upstream` raises a
  `permission_request` — on the same footing as `web_fetch`. Unset,
  calls dispatch without a permission check, like read-only
  built-ins.
- The Rule-of-Two runtime gate does not revoke these tools: the
  control plane already receives every `tool_call` input on the
  stream, so a control-plane tool opens no channel it did not have.
- Sub-agents spawned by `spawn_agent` have no live control-plane
  transport; a control-plane tool called from a sub-agent fails
  fast with a tool error rather than waiting out the timeout.

Field reference: [`configuration.md`](configuration.md#control-plane-tools).
Wire contract for the event pair: [`deployment.md`](deployment.md).

### Joining a control-plane trace (`trace_context`)

Goal: show a run inside the control plane's own trace, under the span
that dispatched it, instead of as a separate trace.

```json
{
  "type": "task_assignment",
  "task": { "…": "RunConfig" },
  "trace_context": {
    "traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
    "tracestate": "vendor=opaque"
  }
}
```

`trace_context` is a W3C Trace Context carrier. With the `otel` trace
emitter, a valid `traceparent` becomes the parent of the run's
`invoke_agent stirrup` span, so the run's spans share the control
plane's trace ID, and `run_started` and the `done` summary report that
trace ID in `trace_reference`. Behaviour to plan for:

- The carrier is read on `task_assignment` only. It applies to the
  primary run; follow-up runs started by `user_response` begin new
  traces.
- An invalid `traceparent` is ignored and the run starts a new trace;
  the task is not rejected. A malformed `tracestate` is dropped and
  the parent kept.
- The run follows the parent's sampling decision. An unsampled
  parent (trace flags `00`) means the harness exports no spans for
  the run, and `trace_reference.sampled` is `false`. See
  [`observability-cloud.md`](observability-cloud.md#trace-context-exchanged-with-a-control-plane).
- The carrier is not part of `RunConfig`, so storing and replaying a
  `RunConfig` never re-parents a new run onto an old trace. Send the
  carrier for the task being dispatched, not one saved with the
  config.
- Baggage is not accepted. `TraceContext` field 3 is reserved for it.
- Under a trace emitter that records no spans, such as `jsonl`, the
  run opens no spans of its own but still carries the parent: every
  `tool_result_request` in the primary run sends the same upstream
  parent as its `trace_context`, with `request_id` remaining the reply
  key, and the harness's log records carry the control plane's trace
  and span IDs.
- Control-plane-fulfilled tool calls send the reverse carrier: each
  `tool_result_request` carries its `execute_tool` span's context
  (see [Control-plane-fulfilled tools](#control-plane-fulfilled-tools-tool_result_request)).

The Go embedding surface follows the same rule without a carrier:
`harnessapi.Loop.Run(ctx, config)` parents the run on a valid span
context in `ctx`. To start a new trace, pass a `ctx` with no span,
such as one derived from `context.Background()`.

**Langfuse under an upstream parent.** Langfuse groups spans into a
trace by trace ID and takes the trace name and the trace-level input
and output from the trace's root observation. A run parented on a
control-plane span is not that root, so the integration chooses one
of two outcomes and records which one the deployment uses:

- **The control plane exports its root span to the same Langfuse
  project.** The run appears inside the control plane's trace. The
  trace is named after the control plane's root span unless a span
  carries the `langfuse.trace.name` attribute (the collector recipe in
  [`observability-cloud.md`](observability-cloud.md#langfuse) sets
  it), and the trace-level input and output
  panels show the control plane's root observation, which is empty
  unless the control plane records content on it. The per-generation
  and per-tool panels are unaffected.
- **The control plane does not export its root span to Langfuse.**
  The run span arrives with a parent Langfuse never receives, and
  Langfuse shows it as an orphan whose parent is missing. Whether
  the trace name and trace-level panels still fall back to it is not
  documented by Langfuse, so verify the rendering on the deployed
  Langfuse version before relying on it.

Trace-level metadata, user, session and tags are unaffected either
way, because Langfuse reads them from any span in the trace. Feedback
scores keep targeting the run span through `trace_reference`
(`traceId` plus `runSpanId` as the observation ID) in both outcomes;
see [`observability-cloud.md`](observability-cloud.md#langfuse).

### Sandbox identity tokens (authenticated git egress)

Goal: give the sandbox a short-lived, per-run credential for a git
proxy such as [Haybale](https://github.com/rxbynerd/haybale) —
without the harness ever holding a signing key.

```json
"executor": {
  "type": "k8s",
  "…": "…",
  "sandboxIdentity": { "source": "control-plane", "audience": "https://haybale.internal" },
  "gitProxy": { "url": "https://haybale.internal", "hosts": ["github.com"], "rewriteSsh": true }
}
```

The control plane must:

1. Answer every `sandbox_token_request` (the first after
   `task_assignment`, before sandbox creation; up to seven more as
   refreshes) with a signed JWT in `sandbox_token_response.token`
   within 60 s — or refuse explicitly with `is_error: true`. Silence
   on the first request aborts the run fail-closed; on a refresh it
   stops refreshing and surfaces a `warning` event.
2. Derive the run's identity from the stream, never from the
   request body; validate `audience` against its own allowlist
   before minting.
3. Scope `sub` to `run-<runId>` and provision the per-run proxy
   policy (repos, verbs) alongside issuance.
4. Set `expires_at` so the harness refreshes ahead of expiry —
   short-lived (≤15 min) tokens then work for runs of any permitted
   length. Without it the token as issued must outlive the run.

Inside the sandbox the token lives in two places: the `envVar`
environment variable (`HAYBALE_TOKEN` by default) holds the token as
issued at creation and is never updated, so it is expired after the
first refresh; the file named by `<envVar>_FILE`
(`HAYBALE_TOKEN_FILE`) is what the composed git credential helper
reads and what every refresh rewrites. Lifecycle hooks and any other
consumer must read the file, not the variable.

The token is the one intentionally raw credential on the stream —
plaintext by default — so this flow requires transport TLS or the trusted-network posture.
The feature requires a pre-established gRPC transport before the
executor is built; `stirrup job` is the normal topology, while an
embedder may supply an equivalent transport. Both `executor` blocks
ride on the proto, so a control plane can send them in the
`task_assignment` that starts the run. A timeout or refusal fails loop
construction and closes the stream without `done`. Full contract:
[`deployment.md`](deployment.md#sandbox-identity-token-issuance-control-plane-implementers).

### Batch mode (amortised token pricing)

Goal: run high-volume, latency-tolerant workloads (`research`,
`toil`) at provider batch pricing by letting the control plane
bundle many runs' turns into provider-side batches.

```json
"provider": {
  "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY",
  "batch": { "enabled": true, "maxWaitSeconds": 3600 }
}
```

Per turn the harness emits `batch_submission` whose `input` is:

```json
{ "schema_version": 1, "provider_type": "anthropic",
  "custom_id": "stirrup-<runId>-turn-<n>", "body": { …provider request… } }
```

Control-plane responsibilities:

- `body` is the fully-formed provider request the streaming adapter
  would have POSTed. Wrap it as one entry (with `custom_id`
  preserved verbatim) in a provider batch — Anthropic
  `/v1/messages/batches`, OpenAI `/v1/batches` — and own the whole
  lifecycle: polling or webhooks, results fetch, bundling entries
  from concurrent runs into shared batches.
- Reply with `batch_result` echoing `request_id`; `content` is
  `{"response": …}` on success or
  `{"err": {"type": "batch_expired" | "batch_cancelled" | "invalid_request_error" | "server_error", …}}`.
  `content` is the canonical discriminator and must set exactly one of
  the two fields. The ControlEvent's `is_error` is optional and
  redundant: when present it must be `true` if and only if `content`
  carries `err`. The harness turns each of the following into an
  `invalid_request_error` naming what it saw, rather than guessing:
  missing `content`, `content` over 4 MiB, malformed JSON, a payload
  setting neither or both fields, and an `is_error` that contradicts the
  payload.
- `batch_waiting` heartbeats mark the wait as healthy. The batch
  client gives up after `maxWaitSeconds`, optionally falling back to
  streaming (`fallbackOnTimeout`). `maxWaitSeconds` must lie in
  `(0, timeout]` and defaults to the task's `timeout`; `timeout`
  itself is capped at 3600 s on both the `stirrup harness` and
  `stirrup job` paths, so 3600 s is the longest batch wait a control
  plane can ask for. `fallbackOnTimeout` additionally requires a
  `maxWaitSeconds` strictly below `timeout` — a wait equal to the run
  deadline can never expire first — and is rejected at validation
  otherwise. The headroom has to cover every preceding batch turn as
  well, since each turn's wait is charged against the same deadline.
  See [`batch.md`](batch.md#the-wait-budget).

Mode gating: `execution` never batches; `research`/`toil` batch
freely; `planning`/`review` need `allowInteractiveModes: true`.
Policy and cost analysis: [`batch.md`](batch.md).

### Collecting results

Four independent channels; use the ones the topology supports:

| Channel | Carries | Topology |
|---|---|---|
| `done` event | Outcome in `stop_reason`; run summary, including the trace reference, in `trace` | gRPC. The primary terminal signal for a control plane, sent after the git handoff, workspace export and trace upload. |
| `STIRRUP_RESULT` stdout line / `resultSink` | `RunResult` JSON: outcome, token usage (with cache and reasoning breakdown, see [`trace-inspection.md`](trace-inspection.md#token-usage)), duration, `finalAssistantText` (capped at 128 KiB by default), verifier verdict, command-output archive pointer, `traceReference` | Any (`resultSink.type: "stdout-json"`); `--output json` is the CLI equivalent. The line lands on process stdout, so a job control plane must collect Pod logs if it needs this detail. Parse the **last** matching line — the sentinel is defence against a model echoing a fake one. |
| Trace emitter | Full event-by-event record (`jsonl` file, `gcs` object `gs://bucket/prefix/<runId>.jsonl`, or `otel` spans/metrics) | Any. JSONL schema: [`trace-inspection.md`](trace-inspection.md). Production builds embed a `Redact()`-ed `RunConfig`; debug builds can explicitly relax trace redaction. |
| Workspace export | `tar.gz` of the sandbox workspace to `gs://…` (`executor.workspaceExportTo`) | Any. Required by default: a failed export turns a successful run into `handoff_failed`; `executor.workspaceExportRequired: false` only records it. |

For OTel specifically: `sessionName` labels the run in logs and
carries on every span as `gen_ai.conversation.id` and `session.id`
unless `correlation.sessionId` is set (see the next recipe).
`observability.serviceName` / `environment` / `serviceNamespace` ride
the OTel Resource, and `serviceName` is how backends tell one agent
deployment from another. `traceEmitter.captureContent` opts spans into
GenAI message-content attributes (off by default, PII), and
`captureContentMaxBytes` caps each content attribute.
`traceEmitter.platform` and `platformOptions` select a backend profile
that fills unset exporter settings; option keys are checked when the
harness builds its exporters, so an unknown key fails the run at start
rather than at `ValidateRunConfig`. Backends and protocol matrix:
[`observability-cloud.md`](observability-cloud.md).

### Linking a run to the control plane's own page

Goal: find a run's spans in the observability backend by the
identifiers the control plane already uses, and get from a span back
to the control plane's page for the run.

```json
"correlation": {
  "sessionId": "conversation-8841",
  "taskId": "task-2207",
  "attemptId": "attempt-2",
  "userId": "u-5c1f0e",
  "url": "https://cp.example.com/tasks/2207/attempts/2",
  "labels": { "agent_profile": "reviewer", "execution_mode": "batch" }
}
```

- Every span of the run carries `session.id` and
  `gen_ai.conversation.id` (from `sessionId`, which takes precedence
  over `sessionName`), `user.id`, `stirrup.task.id`,
  `stirrup.attempt.id`, `stirrup.control_plane.url`, and one
  `stirrup.label.<key>` per label. Langfuse reads `user.id` and
  `session.id` natively, so users and sessions populate without a
  collector.
- The values never reach the model. They are not resource attributes
  or metric labels either, so per-run values cannot inflate metric
  cardinality.
- Every value is opaque to the harness. `userId` belongs to the
  control plane to pseudonymise before it is sent; the harness never
  interprets it.
- Derive every identifier server-side, never from end-user input: a
  forged session ID places runs inside another user's session.
- `url` is the control plane's canonical page for the run: an absolute
  `http` or `https` URL without userinfo or a fragment. It must not
  carry credentials. A configured public origin with an ordinary
  authenticated route works; a signed or token-bearing URL does not
  belong here, because traces persist it. The harness scrubs
  secret-shaped values from every field before stamping or persisting
  them, as a backstop only.
- `labels` carries the control plane's own slicing dimensions. Keys
  are lower-case identifiers, so they stay valid metadata path
  segments in backends that index them.
- The block is recorded with the rest of the config in JSONL and GCS
  trace files and in recordings. Bounds:
  [Correlation](configuration.md#correlation).

### Safety posture quick reference

The five operator-configurable rings, from
[`safety-rings.md`](safety-rings.md):

| Ring | Surface | Default |
|---|---|---|
| 1 — Sandbox runtime class | `executor.runtime` (gVisor/Kata) | Engine default (`runc`); hardening flags always on |
| 2 — Egress allowlist | `executor.network` + proxy / NetworkPolicy | Must be explicit for sandbox executors; `none` gives no network |
| 3 — Cedar policy engine | `permissionPolicy.type: "policy-engine"` | Off; select a non-Cedar permission policy explicitly |
| 4 — Rule of Two | `ruleOfTwo`, `sensitiveData` | Enforced; classifier auto-arms |
| 5 — Code scanner | `codeScanner` | `patterns` in execution mode |

A production-recommended starting posture: container executor with
`runsc`, `network.mode: "allowlist"`, Cedar policy with `deny-all`
fallback, `codeScanner: "patterns"`, Rule of Two enforcement on.
Add `guardRail` (Granite Guardian via vLLM, or `cloud-judge`) for
LLM-based content classification at pre-turn/pre-tool/post-turn —
see [`guardrails.md`](guardrails.md).

## Features not on the wire

The proto mirrors `RunConfig` field-for-field, with a small set of
exceptions. These fields exist on the JSON `RunConfig` (file/stdin
topologies) but **cannot be expressed in a `task_assignment`**. Apart
from `transport`, which is harness-local by design, an integration
that needs one supplies the config file itself (CLI or Cloud Run
topology):

| Surface | Feature | Why |
|---|---|---|
| `transport` | Transport selection. | Deliberate. It describes how the harness was launched, not the work assigned: a config arriving in a `task_assignment` came over the gRPC stream by construction, so the harness sets `grpc` itself. Accepting a wire value would let a control plane declare a transport contradicting the stream carrying the declaration. |
| `traceEmitter.credential`, `traceEmitter.archive` | Credential override for the `gcs` emitter; durable storage for command-output sidecars. | Not yet mapped into the existing proto credential types and translation layer. |
| `tools.profile`, `tools.commandOutput` | Tool presentation profile; command-output capture bounds. | Not yet mirrored. |

Everything else on `RunConfig` is expressible in a
`task_assignment`, including the executor's
`registryAllowlist`, `workspaceExportTo`, `k8sEgressProxyUrl`,
`sandboxIdentity`, and `gitProxy` blocks, plus `resultSink`,
`toolChoiceEscalation`, `correlation`, `observability.logsExport`,
`observability.serviceName`, and the `traceEmitter` fields
`metricsTemporality`, `captureContentMaxBytes`, `platform`, and
`platformOptions`.

When a field in this table becomes load-bearing for a control plane,
the project invariant is that it ships with its proto mirror and
translation — treat an entry here as "open an issue", not "work
around silently".

## Compatibility

- `ready.capabilities` lists the contract features the harness
  implements. Gate task assignment on the presence of a capability; a
  `ready` without the field comes from a harness that predates every
  listed feature, and unrecognised values are ignored. The values are:
  - `live_transcript`: the `user_message`, `turn_started`,
    `turn_committed` and `thinking_delta` events, `name`, `is_error`
    and `duration_ms` on `tool_result`, and `run_started` for every
    run.
  - `reasoning_summary`: `RunConfig.reasoningSummary` (field 59) is
    honoured.
  - `follow_up_history`: `RunConfig.followUpHistory` (field 60) is
    honoured.
  - `turn_abandoned`: the `turn_abandoned` event and its
    `abandon_reason` (field 26); every `turn_started` is followed by
    exactly one `turn_committed` or `turn_abandoned`.
  - `lazy_tool_loading`: `RunConfig.tools.lazyLoading`
    (`ToolsConfig.lazy_loading`, field 11) is honoured, and the
    `tools_loaded` event and its `tools` (field 27) report every tool
    search that loaded tools.
  - `heartbeat_interval`: `RunConfig.heartbeatSeconds` (field 61) is
    honoured. A harness without it keeps the 30 s interval and
    ignores the field, so a control plane that shortens its liveness
    deadline should gate on this capability.
- `ready.harness_version` carries the build label
  (`v1.2.3 (abc1234)` for releases) and is informational: `dev` and
  `main` builds cannot be ordered against a release, so it is not a
  basis for feature gating.
- For mirrored fields, file-config JSON and protobuf JSON use the same
  lower-camel-case names. The MCP host allowlist spelling is
  `tools.mcpServers[].allowedMcpHosts`; the pre-#554 file spelling
  `allowedMCPHosts` is no longer accepted.
- File-config JSON rejects unknown fields (`DisallowUnknownFields`).
  Protobuf binary compatibility is different: an older harness ignores
  unknown fields. Gate assignment on `ready.capabilities` when a task
  depends on a newly added wire field; do not assume it will fail
  loudly.
- New `HarnessEvent` types are added without a version bump of the
  RPC. Ignore unrecognised types; gate on `ready.capabilities` when a
  feature depends on one, as for the live transcript events in
  [Watching a run live](#watching-a-run-live).
- The container image is distroless (`nonroot`, no shell):
  `ghcr.io/rxbynerd/stirrup:<tag>` per release, `:main` per merge.
