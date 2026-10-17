# Langfuse feedback and trace propagation: qualification plan

Status: pending, 2026-10-17. Companion to
[the implementation worksheet](langfuse-feedback-and-trace-propagation-implementation.md).
That worksheet's packages S1 to S6 and S8 were implemented without a
reachable Langfuse instance. Every claim below that names Langfuse
behaviour is therefore taken from Langfuse's documentation or from an
OTLP-decoding stub, not from a live instance. This document is the
list a future session works through once a self-hosted Langfuse v4.x
exists, and the place to record what each check found.

Record results inline: replace each `[ ]` with `[x]` and a one-line
observation (version, screenshot path, or the stored IDs compared).
A check that fails becomes an issue; link it here.

## Stack to pin

| Component | Pin | Notes |
|---|---|---|
| Langfuse | self-hosted v4.x (record the exact tag) | the collector transform was last verified live on v4.57.0; the in-process profile never has been |
| Stirrup | the merge commit of the final PR in the stack | `traceEmitter.platform: langfuse`, `platformOptions.host`, `headers.Authorization` from `secret://` |
| Hairpin | current `main` | functional-test control plane over gRPC; not the feedback surface |
| MCP echo server | any MCP SDK server (Python or TypeScript) that logs `_meta` and request headers | for S4 |
| Control-plane stub | records every `tool_result_request` it receives | for S5; Hairpin's log may suffice |

Unit and wire tests for each package already pass; nothing here
repeats them. Assert span ancestry from the stored observations, not
from span names.

## S1. Trace reference

- [ ] `run_started` arrives on the gRPC stream before the first
      `text_delta` or `tool_call`, with `id` equal to the run ID and a
      `traceReference` with 32-hex `traceId`, 16-hex `runSpanId`,
      `sampled: true`.
- [ ] The `done` summary and the `RunResult` carry the same
      `traceReference` as `run_started`.
- [ ] In Langfuse, the trace with that `traceId` exists and its
      `invoke_agent` observation has ID `runSpanId`. OTLP/HTTP ingest
      must not re-key observations; if it does, the feedback design is
      broken and the worksheet needs revisiting.
- [ ] `POST /api/public/scores` with `traceId`, `observationId =
      runSpanId`, `name`, `value`, `dataType` attaches to that
      observation. Confirm the body field names against the live API;
      the integration guide's example was written from Langfuse's
      docs.
- [ ] A score posted before the run's spans are flushed still links
      once they arrive (post immediately after `run_started`).
- [ ] A follow-up run (`user_response` in the same session) emits a
      second `run_started` with a different `traceId`.
- [ ] A run under the JSONL emitter reports no `traceReference` on any
      surface.

## S2. Correlation

Run with every `correlation` field set and two labels, for example
`{"agent_profile": "triage-v3", "execution_mode": "autonomous"}`.

- [ ] Langfuse's Users view lists `correlation.userId` and the Sessions
      view lists `correlation.sessionId`, with no collector in between.
      This is the claim that Langfuse reads `user.id` and `session.id`
      natively.
- [ ] A 199-byte session ID is kept; a 200-byte one is rejected by
      `ValidateRunConfig` (so it never reaches Langfuse). Confirm
      separately that Langfuse keeps a 199-byte session ID.
- [ ] Every stored observation of the run carries `session.id`,
      `user.id`, `stirrup.task.id`, `stirrup.attempt.id`,
      `stirrup.control_plane.url` and one `stirrup.label.<key>` per
      label. Without the S3 profile these land under
      `metadata.attributes` and are not filterable; that is expected.
- [ ] A URL with `?token=sk-ant-...` reaches Langfuse as
      `?token=[REDACTED]`.
- [ ] A follow-up run re-stamps the same correlation, including task
      and attempt IDs (follow-ups reuse the `RunConfig`).
- [ ] Separately, on Phoenix if one is available: does its user filter
      pick up `user.id`? The docs say this is unchecked.
- [ ] Post the owed comment on stirrup#775 saying its Phoenix `user.id`
      bullet is satisfied by this stamping.

## S3. `langfuse` profile

- [ ] With `platform: langfuse` and `platformOptions.host`, traces
      arrive at `<host>/api/public/otel/v1/traces` with
      `x-langfuse-ingestion-version: 4`; the operator's `Authorization`
      header passes through; no metrics requests are sent when only
      `host` is set.
- [ ] The trace is named `stirrup <mode>` when the run span is the
      root.
- [ ] The trace table filters on `metadata.run_mode`, `task_id`,
      `attempt_id`, `control_plane_url` and each label key
      (`agent_profile`, `execution_mode`), each returning only the
      matching runs. The observation table filters on the same label
      keys.
- [ ] `control_plane_url` renders as a clickable link in the trace
      view. This renderer claim is source-only.
- [ ] Tags show `stirrup` and the mode; `release` shows the harness
      version.
- [ ] `guard.*` observations show type `guardrail`, `verifier.verify`
      shows `evaluator`, and `context.prepare`, `hooks.*`, `git.*`,
      `workspace.*` show `chain`. Agent, generation and tool types stay
      as Langfuse derives them from the GenAI conventions.
- [ ] `protocol: grpc` with `platform: langfuse` fails
      `ValidateRunConfig` and `stirrup harness --dry-run`. A config
      with neither `host` nor `traceEmitter.endpoint` fails at start
      rather than exporting to the gRPC port.
- [ ] The collector recipe in `docs/observability-cloud.md#langfuse`,
      run instead of the profile, produces the same trace name,
      metadata and tags (it was re-verified only through
      `otelcol-contrib` with a debug exporter).
- [ ] Langfuse Cloud has not been tested at all; test once if it is a
      target.

## S4. MCP `_meta` carrier

- [ ] A real MCP SDK server accepts `tools/call` with the extra
      `_meta.traceparent` and `_meta.tracestate` keys and does not
      reject the request. The unit tests used a stub that accepts
      anything.
- [ ] The server logs the same `traceparent` in `_meta` and in the
      request header, and its span ID equals the stored `execute_tool`
      observation for that call.
- [ ] If the server exports to Langfuse, its spans nest under that
      `execute_tool` observation, judged by stored parent IDs.
- [ ] Through any proxy in front of the server, the headers arrive
      unmodified.
- [ ] A JSONL-only `stirrup harness` run sends no trace context;
      a `stirrup job` run with a `trace_context` on the task assignment
      forwards the control plane's span on `tools/list` (see S6).

## S5. `tool_result_request` carrier

- [ ] For one turn with two concurrent control-plane tool calls, the
      stub records two different `traceContext.traceparent` values.
- [ ] Each value's span ID equals the stored `execute_tool` observation
      for its `toolUseId`.
- [ ] If Hairpin parents its proxied call on that value (hairpin#19),
      its span nests under the tool observation.
- [ ] Under the JSONL emitter with a `trace_context` on the task
      assignment, the carrier forwards the control plane's traceparent.

## S6. Upstream parent

- [ ] A `task_assignment` with a sampled `trace_context`: the run span
      stored in Langfuse has that trace ID and that parent span ID;
      `run_started`, `done` and `RunResult` report the same trace ID.
- [ ] An unsampled parent (trace flags `00`): Langfuse stores no
      observations for the run and `run_started` reports
      `sampled: false`.
- [ ] An invalid `traceparent`: the run starts a new trace and the task
      is not rejected.
- [ ] A follow-up run in the same session gets a new trace ID and no
      parent.
- [ ] Cancelling or timing out the control plane's own span context
      does not cancel the run (the #770 property).
- [ ] **Both Langfuse outcomes, recorded for the deployment to choose.**
      First, Hairpin's root span exported to the same Langfuse project:
      record the trace name (the profile's `langfuse.trace.name` must
      win) and what the trace-level input and output panels show.
      Second, Hairpin's root not exported: record how the orphaned run
      span renders and whether the trace-name and panel fallbacks still
      apply. Langfuse does not document the orphan case. Write the
      outcome into `docs/integration-guide.md`.
- [ ] Under the JSONL emitter with a `trace_context`, stderr log
      records carry the control plane's `trace_id` and `span_id`.
- [ ] Go embedding: `harnessapi.Loop.Run(ctx, …)` with a span on `ctx`
      parents the run on it; a `ctx` without a span starts a new root.

## S8. CLI flags

- [ ] `stirrup harness --traceparent <sampled> ...` exports a root
      whose parent matches; `--tracestate` arrives on the run span's
      context; an invalid value starts a new root.
- [ ] `stirrup run-config --correlation-* --correlation-label k=v`
      emits the block; `stirrup run-config --traceparent` is rejected.

## Deferred

- S7 (per-command Git trace headers through the executor overlay):
  not implemented; gated on haybale#16 and the executor overlay
  contract.
- The end-to-end feedback check (rate an attempt, restart, confirm the
  score lands on that run span) belongs to the outer-application
  worksheet.
- `baggage` on the wire: `TraceContext` field 3 is reserved; see the
  worksheet's carrier decision.
