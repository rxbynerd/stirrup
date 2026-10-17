# Langfuse feedback and trace propagation: qualification plan

Status: pending, 2026-10-17. Companion to
[the implementation worksheet](langfuse-feedback-and-trace-propagation-implementation.md)
and [the research worksheet](langfuse-feedback-and-trace-propagation-research.md).

The worksheet's packages S1 to S6 and S8 were implemented without a
reachable Langfuse instance. Unit and integration tests pin what
Stirrup emits; nothing below repeats them, and each section names the
tests that already cover the Stirrup side. What remains is how Langfuse
(and the other party on each wire) treats what Stirrup sends. Every
such claim in the docs rests on Langfuse's documentation, its source at
the commit the research worksheet pins, or an OTLP-decoding stub. This
document lists the checks a future session runs against a live stack,
and is the place to record what each check found.

Record results inline: replace `[ ]` with `[x]` and append a one-line
observation to the same bullet (version, screenshot path, or the stored
IDs compared). A failed check becomes an issue; link it from the
bullet. One assertion per checkbox, so a partial result can be
recorded.

## Stack to pin

The work is spread over five pull requests stacked on the worksheets'
branch; S4 is on its own branch because it is independent of the rest.
Qualify a tree that contains all of them.

| Package | Pull request | Branch |
|---|---|---|
| S1 trace reference | #784 | `feat/langfuse-s1` |
| S2 correlation | #786 | `feat/langfuse-s2` |
| S3 `langfuse` profile | #787 | `feat/langfuse-s3` |
| S5, S6 trace context and upstream parent | #788 | `feat/langfuse-s5-s6` |
| S8 CLI flags and this plan | the PR from `feat/langfuse-s8` | `feat/langfuse-s8` |
| S4 MCP carrier | #785 | `feat/langfuse-s4` (not in the chain above) |

| Component | Pin | Notes |
|---|---|---|
| Langfuse | self-hosted v4.x; record the exact tag | an earlier form of the collector transform was verified live on v4.57.0; the current transform and the in-process profile have never run against Langfuse |
| Stirrup | the commit under test, containing every PR above | `traceEmitter.platform: langfuse`, `platformOptions.host`, `headers.Authorization` from a `secret://` reference, `captureContent: true` for the panel checks |
| Control-plane stub | a gRPC server built from this tree's proto that logs `run_started`, the `done` summary's trace reference and every `tool_result_request` trace context, and can set `trace_context` on `task_assignment` | required for S1, S5 and S6 whatever Hairpin supports; the fake control plane in `harness/cmd/stirrup/cmd/job_tracecontext_test.go` is a starting point |
| Hairpin | commit recorded with the results, proto synced to this tree | an unsynced Hairpin drops the new fields. Persisting the reference and forwarding correlation is hairpin#18; parenting proxied tool calls on the S5 carrier is hairpin#19. Hairpin is the functional-test control plane, not the feedback surface |
| MCP echo server | an MCP SDK server (Python or TypeScript) that logs `_meta` and request headers; record the SDK, its version and the negotiated protocol revision | for S4 |
| Browser | record which one | for every rendering claim |

Prerequisites:

- Read stored data back through Langfuse's v2 observations API. On v4
  the public `traces` and v1 `observations` query APIs return 404 (see
  `docs/observability-cloud.md`, Langfuse section).
- Assert span ancestry from stored parent IDs, never from span names.
- Field names below are the JSON and `protojson` forms
  (`traceReference`, `runSpanId`, `traceContext`). A stub that prints
  proto text shows `trace_reference`, `run_span_id`, `trace_context`.
- Supply the Langfuse credential through a `secret://` reference; never
  print it.

## S1. Trace reference

Covered by tests, not repeated: event ordering, the run ID on the
event, the hexadecimal shape, equality between `run_started`, `done`,
`RunTrace` and `RunResult`, a fresh reference per follow-up run, and
the absence of any reference under the JSONL emitter
(`core/livespans_otel_test.go`, `core/handoff_test.go`,
`cmd/root_test.go`, `trace/otel_test.go`).

- [ ] The trace with `traceId` exists in Langfuse.
- [ ] Its `invoke_agent` observation has ID `runSpanId`. OTLP/HTTP
      ingest must not re-key observations; if it does, the feedback
      design is broken and the worksheet needs revisiting.
- [ ] `POST /api/public/scores` with `traceId`, `observationId` set to
      `runSpanId`, `name`, `value` and `dataType` attaches a score to
      the run observation. Compare the request in
      `docs/observability-cloud.md` ("Scoring a run") with the live API
      reference, including whether the body is one object or an array,
      and correct the example if it differs.
- [ ] Posting a second score with the same `id` and `name` on the same
      day updates the first; posting it on a later day adds a second
      score. If so, qualify the sentence about a stable `id` in
      `docs/observability-cloud.md`.
- [ ] A score posted immediately after `run_started`, before the run's
      spans are flushed, still links once they arrive.
- [ ] With a wrong `Authorization` value, or Langfuse stopped, the run
      still completes, `run_started` still reports `sampled: true`, and
      nothing is stored. This is the case the outer application must
      handle when a score targets a trace that never arrived.

## S2. Correlation

Covered by tests, not repeated: every validation bound, the span
attribute set and its precedence, scrubbing of a query-string token,
exclusion from prompts and metric labels
(`types/correlation_test.go`, `trace/otel_scope_test.go`,
`core/correlation_test.go`).

Run with every `correlation` field set and two labels, for example
`{"agent_profile": "triage-v3", "execution_mode": "autonomous"}`.

- [ ] With no `platform` set (plain OTLP/HTTP to Langfuse, no
      collector), the Users view lists `correlation.userId`.
- [ ] Under the same configuration the Sessions view lists
      `correlation.sessionId`. Repeat both with `platform: langfuse`
      and confirm nothing changes.
- [ ] Langfuse keeps a 199-byte session ID. Optionally record what it
      does with exactly 200, since the docs state "200 or more" as the
      drop threshold.
- [ ] Every stored observation of the run carries `session.id`,
      `user.id`, `stirrup.task.id`, `stirrup.attempt.id`,
      `stirrup.control_plane.url` and one `stirrup.label.<key>` per
      label. Without the profile these land under
      `metadata.attributes` and are not filterable; that is expected.
- [ ] A follow-up run in the same session appears as a second trace in
      the same Langfuse session, with the same task and attempt IDs
      (follow-ups reuse the `RunConfig`).
- [ ] On Phoenix, if one is available: its user filter picks up
      `user.id`. The docs say this is unchecked.

## S3. `langfuse` profile

Covered by tests, not repeated: endpoint path, ingestion header,
Authorization pass-through, no metric export with only `host`, every
attribute mapping including a real emitter run, `protocol: grpc`
rejected by validation, no endpoint at all failing at start and in
`--dry-run` (`platform/langfuse_test.go`, `types/runconfig_test.go`,
`core/preflight_test.go`).

- [ ] Langfuse accepts the export and the trace appears without the
      ingestion delay Langfuse documents for requests lacking
      `x-langfuse-ingestion-version`.
- [ ] The trace is named `stirrup <mode>`. (The profile sets the name
      on the run span whether or not it is the root; the non-root case
      is checked under S6.)
- [ ] The Traces table's metadata filter with key `run_mode` returns
      only the matching runs.
- [ ] The same filter works for `task_id`, `attempt_id` and
      `control_plane_url`.
- [ ] The same filter works for each label key (`agent_profile`,
      `execution_mode`), and the Observations table filters on the
      same label keys.
- [ ] `control_plane_url` renders as a clickable link in the formatted
      metadata table of the trace view (the raw JSON view differs).
      This renderer claim is source-only.
- [ ] A 1499-byte URL links and a 1500-byte URL does not, because
      Langfuse's renderer skips link detection at 1500 characters or
      more. If confirmed, lower the `correlation.url` cap from 1500 to
      1499 bytes in `types/correlation.go` and its tests, and correct
      the constant's comment and `docs/configuration.md`.
- [ ] Tags show `stirrup` and the mode.
- [ ] `release` shows the harness version.
- [ ] `guard.*` observations show type `guardrail` and
      `verifier.verify` shows `evaluator`.
- [ ] `context.prepare`, `hooks.*`, `git.*` and `workspace.*` show
      `chain`; agent, generation and tool types stay as Langfuse
      derives them from the GenAI conventions.
- [ ] With `captureContent: true`, the generation observations render
      the semconv message parts legibly in the browser. The docs say
      this was not viewed in a browser.
- [ ] With both `platformOptions.host` and a different
      `traceEmitter.endpoint` set, the ingestion header is not applied
      (the profile's headers follow its own endpoint). Decide whether
      to document or reject that combination.
- [ ] The collector recipe in `docs/observability-cloud.md` (the
      collector alternative under Langfuse), run instead of the
      profile with the exporter spelled `otlp_http/langfuse` and the
      contrib version recorded, produces the same trace name, tags,
      release, run-mode, task, attempt, control-plane URL and label
      metadata, and the same guardrail, evaluator and chain
      observation types, as the profile.
- [ ] If Langfuse Cloud is a target: repeat the first S1 check, the
      first S2 check and the first three S3 checks against Cloud, and
      record ingestion latency with and without the ingestion header.

## S4. MCP `_meta` carrier

Covered by tests, not repeated: `_meta` and header equal the exported
`execute_tool` span, distinct parents for concurrent calls, nothing
sent without a span, no baggage, coexistence with the bearer and
session headers (`mcp/tracecontext_test.go`,
`core/dispatch_parallel_test.go` on `feat/langfuse-s4`).

- [ ] A real MCP SDK server accepts `tools/call` with the extra
      `_meta.traceparent` and `_meta.tracestate` keys and does not
      reject the request. The unit tests used a stub that accepts
      anything.
- [ ] The server logs the same `traceparent` in `_meta` and in the
      request header.
- [ ] With the server extracting from both the header and `_meta`, one
      tool call produces one logical server span, not two.
- [ ] If the server exports to Langfuse, its span's stored parent ID is
      the `execute_tool` observation for that call.
- [ ] Through any proxy in front of the server, the headers arrive
      unmodified.
- [ ] A `stirrup job` run with a `trace_context` on the task assignment
      forwards the control plane's span on `tools/list` (see the
      upstream-parent checks under S6).

## S5. `tool_result_request` carrier

Covered by tests, not repeated: distinct carriers per concurrent call
equal to the recorded `execute_tool` span, forwarding of the upstream
parent under the JSONL emitter (`core/async_tracecontext_test.go`,
`core/upstream_parent_test.go`).

- [ ] For one turn with two concurrent control-plane tool calls, the
      stub records two different `traceContext.traceparent` values and
      each span ID equals the stored `execute_tool` observation for its
      `toolUseId`.
- [ ] If Hairpin parents its proxied call on that value (hairpin#19),
      its span's stored parent ID is that tool observation.

## S6. Upstream parent

Covered by tests, not repeated: a sampled parent is adopted and an
unsampled one exports nothing through `stirrup job`, an invalid parent
starts a new root, sub-agents and follow-ups are unaffected, the
caller's cancellation and deadline are not inherited by the run's root
context, JSONL log records correlate with the upstream parent
(`cmd/job_tracecontext_test.go`, `core/upstream_parent_test.go`,
`trace/otel_parent_test.go`, `observability/tracecontext_test.go`).

- [ ] For a `task_assignment` with a sampled `trace_context`, the run
      span stored in Langfuse has that trace ID and that parent span
      ID, and `run_started` reports the same trace ID.
- [ ] With trace flags `00`, Langfuse stores no observations for the
      run while `run_started` reports `sampled: false`.
- [ ] With an unsampled parent and `OTEL_TRACES_SAMPLER=always_on` in
      the harness's environment, the run exports and `run_started`
      reports `sampled: true`; with `parentbased_always_on` it exports
      nothing. This needs only an OTLP stub; no test pins it.
- [ ] With `captureContent: true`, run the sampled-parent case with the
      control plane's root span exported to the same Langfuse project.
      Record the trace name (the profile's `langfuse.trace.name` should
      win) and what the trace-level input and output panels show.
- [ ] Repeat with the control plane's root span not exported. Record
      whether the trace appears in the Traces table at all, how the
      run span renders, and whether the trace name and the panels
      still apply. Langfuse documents that a root span is required and
      says nothing about orphans. Correct the sentences in
      `docs/integration-guide.md` ("Joining a control-plane trace")
      that state the orphan rendering as fact and that credit only the
      collector recipe with setting the trace name.
- [ ] Go embedding: cancel the `ctx` passed to `harnessapi.Loop.Run`
      mid-run, with a span on it. The run ends as cancelled and its run
      span still arrives in Langfuse, ended and carrying
      `run.cancelled_by`. Repeat over gRPC with a `cancel` control
      event.

## S8. CLI flags

Covered by tests, not repeated: flag-to-config precedence and label
parsing, `run-config` rejecting the parent flags, `--traceparent`
parenting the exported root, an invalid or unsampled value
(`cmd/correlation_test.go`, `cmd/traceparent_test.go`).

- [ ] `stirrup harness --traceparent <sampled> --tracestate k=v`
      against the live Langfuse produces a run observation whose stored
      parent is the given span, and the `STIRRUP_RESULT` line's
      `traceReference` carries the given trace ID.

## Owed actions

- After the Phoenix check under S2, comment on stirrup#775 that its
  Phoenix `user.id` bullet is met by span stamping, stating whether
  Phoenix's user filter was confirmed.
- Write the chosen S6 outcome (control-plane root exported or not) into
  `docs/integration-guide.md`.

## Deferred

- S7 (per-command Git trace headers through the executor overlay): not
  implemented. It waits on haybale stripping trace headers at its
  external boundary (haybale#16) and an agreed executor env-overlay
  contract.
- The end-to-end feedback check (rate an attempt, restart, confirm the
  score lands on that run span) belongs to the outer-application
  worksheet.
- `baggage` on the wire: `TraceContext` field 3 is reserved; see the
  worksheet's carrier decision.
