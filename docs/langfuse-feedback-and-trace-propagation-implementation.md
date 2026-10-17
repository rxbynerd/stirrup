# Langfuse feedback and trace propagation: implementation worksheet

Status: proposal, 2026-10-17. Companion to
[the research worksheet](langfuse-feedback-and-trace-propagation-research.md),
which holds the evidence and the reasoning. This document holds only the
*how*: types, fields, call sites, tests and the order of the pull requests.
Every file and line reference was re-checked against Stirrup
`d65c91bd` and the sibling checkouts pinned in the research worksheet
(Hairpin `5421cde9`, Billet `d4fb0e15`, haybale `e7941a5b`).

Scope: this worksheet covers Stirrup only. Stirrup reports identity and
propagates context; it collects no feedback and submits no scores. The
work in the other Equestrianism components is tracked where it will be
done:

| Component | Where the work is tracked |
|---|---|
| Outer application (owns the rating page, feedback store and score delivery) | [`langfuse-feedback-outer-application-worksheet.md`](langfuse-feedback-outer-application-worksheet.md) |
| Hairpin (pass-through: persist and expose the reference, forward correlation) | [hairpin#18](https://github.com/rxbynerd/hairpin/issues/18) |
| Hairpin (parent proxied tool calls on the harness tool span, trace the Billet client) | [hairpin#19](https://github.com/rxbynerd/hairpin/issues/19) |
| Billet (OTel export, Connect interceptor, MCP `_meta` extraction) | [billet#2](https://github.com/rxbynerd/billet/issues/2) |
| haybale (strip trace headers at the external Git boundary) | [haybale#16](https://github.com/rxbynerd/haybale/issues/16) |

Hairpin is the functional-test control plane for the Stirrup packages
below; nothing here makes it the feedback surface.

## Decisions

These were settled with the maintainer on 2026-10-17 and are not open.
The rejected alternative is recorded so the reasoning is not lost.

| Decision | Taken | Rejected | Why |
|---|---|---|---|
| Who owns feedback | An outer application owns the rating page, the feedback store, the outbox and the score submission (its own worksheet). Hairpin persists and exposes the reference and forwards correlation (hairpin#18) | Hairpin as the feedback surface | Under Hairpin's model an outer application owns the user-facing page; Hairpin stays usable for functional testing |
| Where non-Stirrup work is specified | Each component's work lives in its own worksheet or as issues on its repository (table above) | One cross-repository worksheet here | A Stirrup document cannot be the source of truth for another repository's backlog |
| Where Langfuse-specific attributes are produced | A `langfuse` platform profile in `harness/internal/observability/platform/`, the vendor-mapping mechanism introduced by [#777](https://github.com/rxbynerd/stirrup/pull/777) | The collector `transform` recipe already in `docs/observability-cloud.md#langfuse` | The platform package is the one place allowed to name a vendor; a profile removes the collector from the feedback pilot and makes the OTLP/HTTP requirement a validation error instead of a silent drop |
| `correlation.userId` | Accepted in the first cut as an opaque value | Deferred until a pseudonymisation policy exists | Pseudonymisation is the upstream application's responsibility; Stirrup treats the value as opaque |
| Carrier shape on the wire | One shared proto message `TraceContext { traceparent, tracestate }` reused on `HarnessEvent` and `ControlEvent` | Two bare string fields per message | One type, one translate helper, one validator; `baggage` is excluded on purpose |
| Who may set an upstream parent | The gRPC task assignment and the Go embedding caller's `ctx` (S6); CLI flags that set the context, not the config (S8) | A `traceContext` block inside `RunConfig` | A replayed `RunConfig` must not re-parent a new run onto an old trace |
| Feedback subject in Langfuse | Observation score on the Stirrup run span (`traceId` + `observationId`) | Trace score | The run span is the execution; a trace may later contain Hairpin or other work |
| Delivery waves | Wave 1: S1, S2, S3 (what the feedback pilot needs from Stirrup). Wave 2: S6 and S4/S5, independent of the pilot's outcome. S8 as its own follow-up PR. S7 last | Upstream parenting first, or upstream parenting gated on the pilot | Feedback needs only S1; upstream parenting is wanted regardless of how the pilot goes, so it is sequenced, not conditional |

## Work packages

| ID | Title | Depends on | Consumed by |
|---|---|---|---|
| S1 | Trace reference on `run_started`, `done`, `RunTrace`, `RunResult` | none | hairpin#18, outer application |
| S2 | Typed application correlation block on `RunConfig`, stamped on spans | none | hairpin#18, outer application |
| S3 | `langfuse` platform profile | S2 for the user/session/URL mapping | operators |
| S4 | W3C context in MCP `tools/call` `_meta` and HTTP headers | none | billet#2 |
| S5 | W3C context on `tool_result_request` | shared `TraceContext` proto message (also used by S6) | hairpin#19 |
| S6 | Context-aware trace start: upstream parent from the task assignment or the embedding `ctx` | shared `TraceContext` proto message | control planes, embedders |
| S7 | Per-command Git trace headers through the executor env overlay | haybale#16 landed; agreed executor overlay contract | haybale |
| S8 | CLI flags for correlation and the upstream parent (follow-up PR) | S2, S6 | CLI operators |

S1, S2, S5 and S6 touch `proto/harness/v1/harness.proto`. Land the
shared `TraceContext` message with whichever of S5/S6 goes first and
keep field numbers reserved as listed below so the PRs do not collide:

| Message | Next free field | Reserved for |
|---|---|---|
| `HarnessEvent` | 15 | `TraceReference trace_reference = 15` (S1), `TraceContext trace_context = 16` (S5) |
| `ControlEvent` | 10 | `TraceContext trace_context = 10` (S6) |
| `RunTrace` | 37 | `TraceReference trace_reference = 37` (S1) |
| `RunConfig` | 35 | `CorrelationConfig correlation = 35` (S2) |

Every proto change ships with its copy in
`harness/internal/transport/grpc_translate.go`; the CLAUDE.md invariant
applies to all four.

## S1. Trace reference

Goal: the control plane learns the OTel trace ID and run-span ID of a
run as soon as the run span exists, and again on every terminal surface.

### Types

New file `types/tracereference.go`:

```go
// TraceReference identifies the run's OTel root span so a control plane
// can attach later operations, such as a feedback score, to the trace
// an observability backend stored. Absent when the run records no OTel
// span.
type TraceReference struct {
    TraceID string `json:"traceId"`           // 32 lowercase hex chars
    SpanID  string `json:"runSpanId"`         // 16 lowercase hex chars
    Sampled bool   `json:"sampled"`           // trace flags sampled bit
}
```

Add `TraceReference *TraceReference `json:"traceReference,omitempty"``
to `types.HarnessEvent` (`types/events.go:303`), `types.RunTrace`
(`types/runtrace.go:82`) and `types.RunResult` (`types/result.go:44`).
The field is harness-produced, so `ValidateRunConfig` is untouched and
`RunConfig.Redact()` is not involved.

### Emitter

In `harness/internal/trace/trace.go` add an optional capability next to
`RootContextProvider`:

```go
// TraceReferenceProvider is implemented by emitters that record an OTel
// run span. TraceReference returns nil before Start and for emitters
// without a span.
type TraceReferenceProvider interface {
    TraceReference() *types.TraceReference
}
```

`OTelTraceEmitter` implements it from `e.rootSpan.SpanContext()` under
`e.mu` (`harness/internal/trace/otel.go:235` creates the span;
`:603` is the sibling accessor to copy). `Finish` sets
`RunTrace.TraceReference` from the same span context. The JSONL, GCS and
nested emitters do not implement the interface; a run under them reports
no reference, which is the documented absence case.

### Loop

In `AgenticLoop.Run` (`harness/internal/core/loop.go`), directly after
the `ready` block at `:316`, emit:

```go
if p, ok := l.Trace.(trace.TraceReferenceProvider); ok {
    if ref := p.TraceReference(); ref != nil {
        if err := l.Transport.Emit(types.HarnessEvent{
            Type: "run_started", ID: config.RunID, TraceReference: ref,
        }); err != nil {
            l.logEmitFailure("run_started", err)
        }
    }
}
```

Ordering on the stream becomes `ready` → `run_started` → first
`text_delta`/`tool_call`. The event is placed after git setup so a setup
failure that ends the run before any span-bearing work never produces
it; `done` still carries the reference when `Trace.Start` ran. Sub-agent
loops use `NullTransport`, whose `Emit` discards the event, so no gate is
needed. Follow-up runs call `Trace.Start` again
(`loop.go:177` via `RunFollowUpLoop`, `:1309` mints the new `RunID`), so
each follow-up emits its own `run_started` with a fresh reference.

### Copy paths

- `doneSummary` (`harness/internal/core/handoff.go:171`): add
  `TraceReference: rt.TraceReference`.
- `runTraceToProto` (`grpc_translate.go:77`) and `harnessEventToProto`
  (`:16`): map the new message.
- `RunResult` construction (`harness/cmd/stirrup/cmd/root.go:191`): add
  `TraceReference: rt.TraceReference`. The result sink's truncation
  search (`resultsink.go:228`) only shrinks `FinalAssistantText`, so the
  reference survives truncation.
- Proto: `message TraceReference { string trace_id = 1; string run_span_id = 2; bool sampled = 3; }`,
  plus the two field additions in the table above. Document
  `run_started` in the `HarnessEvent.type` comment and the `id` field
  comment (it carries the run ID on this event).

### Tests

- `trace/otel_test.go`: reference is nil before `Start`; after `Start`
  equals the in-memory exporter's root span IDs; `Finish` copies it onto
  the `RunTrace`; a new `Start` yields a new trace ID.
- `core/livespans_otel_test.go`: `run_started` is emitted once, after
  `ready`, before the first model event, with `ID == RunConfig.RunID` and
  IDs equal to the exported root span; a follow-up run emits a second,
  different reference; the sub-agent test asserts nothing reaches the
  parent transport.
- `transport/grpc_translate_test.go`: round trip of the event and the
  `RunTrace` field.
- `cmd/root_test.go` and `core/handoff_test.go`: `RunResult` and the
  `done` summary carry it; a run whose emitter is JSONL has it absent
  everywhere.

### Docs

`docs/integration-guide.md`: add `run_started` to the event table
(`:847`) and correct the stale sentence near `:84` that says ordinary
`done` events carry no summary. `docs/deployment.md`: gRPC sequence.
`docs/configuration.md`: `RunResult` schema. `docs/observability-cloud.md#langfuse`:
a short "Scoring a run" paragraph showing `traceId` + `runSpanId` as the
score target.

## S2. Application correlation

Goal: a control plane can tell Stirrup the stable identifiers its own
UI uses, and those identifiers reach every span.

### RunConfig

```go
// Correlation carries control-plane identity for the run's telemetry.
// Values are stamped on every span and never enter the model's context.
Correlation *CorrelationConfig `json:"correlation,omitempty"`

type CorrelationConfig struct {
    SessionID string `json:"sessionId,omitempty"` // stable grouping key; takes precedence over SessionName for session.id
    TaskID    string `json:"taskId,omitempty"`
    AttemptID string `json:"attemptId,omitempty"`
    UserID    string `json:"userId,omitempty"`    // opaque; operators choose pseudonymous values
    URL       string `json:"url,omitempty"`       // canonical control-plane page for this run
}
```

Validation (`types/runconfig.go`, beside `validateSessionName` at
`:3636`): each ID ≤ 255 bytes, printable, no control characters; `URL`
absolute, scheme `http` or `https`, no userinfo, ≤ 1500 bytes (the
Langfuse metadata renderer limit the research cites). The block survives
`Redact()`; it holds no secrets.

Proto: `message CorrelationConfig` with the five strings; field 35 on
`RunConfig`; copy in `runConfigFromProto` (`grpc_translate.go:170`).
CLI flags are S8. `UserID` is accepted as-is: the upstream application
pseudonymises before it sets the field, and Stirrup never interprets
the value.

### Span stamping

`runScopeProcessor.setRun` (`harness/internal/trace/otel_scope.go:45`)
takes a small struct instead of one string. `OnStart` (`:51`) stamps on
every span:

| Attribute | Source |
|---|---|
| `session.id`, `gen_ai.conversation.id` | `Correlation.SessionID`, else `SessionName` (unchanged fallback) |
| `user.id` | `Correlation.UserID` |
| `stirrup.task.id`, `stirrup.attempt.id` | the matching fields |
| `stirrup.control_plane.url` | `Correlation.URL` |

Not resource attributes, not metric labels (`metricAttrs` stays as it
is), not the prompt. `OTelTraceEmitter.Start` (`otel.go:262`) passes the
struct in.

### Tests

- `trace/otel_scope_test.go`: every span carries the five attributes;
  `SessionID` wins over `SessionName`; neither set yields no
  `session.id`.
- `types/runconfig_test.go`: validation table, including a `javascript:`
  URL, a URL with userinfo, and an over-long ID.
- `prompt` package: render each mode's system prompt with a fully
  populated `Correlation` and assert none of the values appear.
- `observability` metrics test: the attribute set used for metrics
  excludes the new keys.
- `transport/grpc_translate_test.go`: round trip.

### Docs

`docs/configuration.md`: new "Correlation" subsection under the
observability settings. `docs/observability-cloud.md`: the Sessions
bullet under Langfuse now names `correlation.sessionId`.
`docs/integration-guide.md`: cookbook entry "Linking a run to the
control plane's own page". Note on
[#775](https://github.com/rxbynerd/stirrup/issues/775): its Phoenix
`user.id` bullet is satisfied by this stamping; comment there rather than
opening a competing identity design.

## S3. `langfuse` platform profile

Goal: the attribute mapping the collector recipe performs today happens
in-process, and the OTLP/HTTP requirement is enforced.

This is the vendor-specific mapping mechanism that
[#777](https://github.com/rxbynerd/stirrup/pull/777) introduced for
CloudWatch and Phoenix, applied to Langfuse. New file
`harness/internal/observability/platform/langfuse.go`, registered in
`registry` (`platform.go:80`) and in `validTraceEmitterPlatforms`
(`types/runconfig.go:1929`).

```go
var langfuseDefinition = definition{
    options: map[string]bool{"host": false},
    build:   buildLangfuse,
}
```

`buildLangfuse` returns a `Profile` with:

- `RequiredProtocol: "http/protobuf"` (Langfuse drops gRPC silently).
- `Signals[traces]`: `Endpoint: host + "/api/public/otel"` when `host`
  is set; `Headers: {"x-langfuse-ingestion-version": "4"}`.
  Authorization stays with the operator's `traceEmitter.headers`
  `secret://` entry; the profile does not own it.
- `WrapSpanExporter` adding, per span at export time:

| Set | From |
|---|---|
| `langfuse.user.id` | `user.id` |
| `langfuse.trace.metadata.control_plane_url` | `stirrup.control_plane.url` |
| `langfuse.trace.metadata.task_id`, `...attempt_id` | the `stirrup.*` keys |
| `langfuse.trace.metadata.run_mode` | `run.mode` |
| `langfuse.release` | resource `service.version` |
| `langfuse.trace.tags` | `["stirrup", <run.mode>]` |
| `langfuse.observation.type` | `guardrail` for `guard.*`; `evaluator` for `verifier.verify`; `chain` for `context.prepare`, `hooks.*`, `git.*`, `workspace.*` |

That is the collector transform in `docs/observability-cloud.md`
(verified live on v4.57.0) moved into Go. `session.id` is read natively
by Langfuse and needs no mapping. Metrics: Langfuse discards them; the
profile leaves metric export alone, as the doc already notes.

Tests follow `phoenix_test.go`: a recorded span through the wrapped
exporter carries the mapped keys; a config with `protocol: grpc` and
`platform: langfuse` fails validation; `vendorscan_test.go` passes
because the vendor name lives only in this package. Live acceptance:
pinned self-hosted Langfuse, then click the `control_plane_url` metadata
value in the trace view; that renderer claim is source-only until
checked in a browser.

Docs: `docs/observability-cloud.md#langfuse` gains "### The `langfuse`
profile" mirroring the Phoenix section; the collector YAML stays as the
alternative for operators who already run one.

## S4. MCP `_meta` carrier

Goal: an MCP server sees the calling tool span as its parent.

No propagator is configured anywhere in the harness today (no
`propagation.` references outside tests). Use an explicit
`propagation.TraceContext{}` value; do not rely on the global.

In `callToolInner` (`harness/internal/mcp/client.go:513`), extend the
params struct with `Meta map[string]string `json:"_meta,omitempty"`` and
fill it when `oteltrace.SpanContextFromContext(ctx).IsValid()`:

```go
carrier := propagation.MapCarrier{}
propagation.TraceContext{}.Inject(ctx, carrier)
params.Meta = carrier // traceparent, tracestate only
```

`ctx` here is the per-call tool span context: `dispatch.go:221` passes
`plan[idx].spanCtx`, and `registerMCPTool` forwards the handler `ctx`
into `callTool` (`client.go:798`). `arguments` is untouched.

In `call` (`client.go:690`), inject the same two headers on the
`*http.Request` for every method, from the method's `ctx`. The static
header map and `sess` carry nothing per-call. Transport headers and
message `_meta` are injected from the same context, so a server that
reads either sees the same parent.

Tests (`mcp/client_test.go` plus `core/dispatch_parallel_test.go`): an
`httptest` server asserts `_meta.traceparent` and the request header
decode to the span the in-memory exporter recorded for that tool call;
two parallel calls receive distinct parents; no `_meta` is sent when the
context carries no span; `initialize` and `tools/list` carry the run
span when one exists.

## S5. Tool carrier on `tool_result_request`

Goal: a control-plane-fulfilled tool call carries its tool span.

Proto: `message TraceContext { string traceparent = 1; string tracestate = 2; }`;
`TraceContext trace_context = 16` on `HarnessEvent`. Go:
`TraceContext *TraceContext `json:"traceContext,omitempty"`` on
`types.HarnessEvent` with the same two fields; map in
`harnessEventToProto`.

In `dispatchAsyncToolCall` (`harness/internal/core/types.go:530`) the
`ctx` is already the tool span context (same path as S4). Inject with
`propagation.TraceContext{}` into the event alongside `RequestID`,
`ToolName`, `ToolUseID`. Request and tool-use IDs keep their meaning
(reply correlation); the carrier only describes causality.

Tests: `core/livespans_otel_test.go` asserts the event's carrier matches
the recorded `execute_tool` span; the parallel-dispatch test asserts two
concurrent async tools carry different span IDs; translate round trip.

## S6. Upstream parent

Goal: a valid W3C parent supplied by the control plane or the embedding
caller becomes the parent of the run span, without inheriting the
supplier's cancellation. Wave 2, sequenced after the pilot's Stirrup
packages but not conditional on the pilot's result. CLI flags are S8.

### Wire

`ControlEvent` gains `TraceContext *TraceContext` (proto field 10, read in
`controlEventFromProto` at `grpc_translate.go:42`). It is meaningful on
`task_assignment` only; other events ignore it. The carrier never
touches `RunConfig`, so a replayed configuration cannot re-parent a run.

### Extraction

One helper, `observability.ExtractTraceContext(ctx, traceparent, tracestate) context.Context`,
wraps `propagation.TraceContext{}.Extract` over a `MapCarrier`. An
invalid carrier returns `ctx` unchanged (the propagator already
guarantees this; the test pins it).

Call sites:

- `harness/cmd/stirrup/cmd/job.go:134` on `task_assignment`, before
  `BuildLoop`, producing the `runCtx` handed to `loop.Run`.
- Go embedding needs no call: `harnessapi.Loop.Run(ctx, config)` passes
  the caller's `ctx` through, and a span on it is the parent. This is a
  behaviour change for embedders who already pass a traced `ctx`;
  document it as the intended semantics and the way to opt out (pass a
  `ctx` without a span).

### Emitter

Add to `trace.go`:

```go
// ContextStarter is implemented by emitters whose run span can be
// parented on a span carried by ctx. Only the span context is taken from
// ctx; its cancellation and deadline are not.
type ContextStarter interface {
    StartWithContext(ctx context.Context, runID string, config *types.RunConfig)
}
```

`OTelTraceEmitter.StartWithContext` shares the body of `Start` and
replaces `context.Background()` at `otel.go:269` with
`oteltrace.ContextWithSpanContext(context.Background(), oteltrace.SpanContextFromContext(ctx))`.
Copying only the span context is what keeps the #770 fix intact: the
root context still has no deadline.

### Loop

At `loop.go:177`, when `l.TraceContext == nil` (a top-level run), the
incoming `ctx` carries a valid span context, and the emitter implements
`ContextStarter`, call `StartWithContext(ctx, ...)`; otherwise `Start`.
Sub-agent loops are built with `TraceContext` set by the parent and
their root span comes from `StartSubAgentSpan` (`otel_live.go:372`), so
nothing changes for them. Follow-up runs derive their context from the
session context in `RunFollowUpLoop` (`loop.go:1312`), which carries no
remote parent, so each follow-up starts a new root; a later change may
let `user_response` carry its own `trace_context`.

### Sampling

The provider has no explicit sampler (`otel.go:190`), so the SDK's
parent-based default applies: an unsampled parent suppresses the run's
export. Do not override. The S1 reference reports `sampled: false` in
that case, which is how the outer application learns that feedback on
this run has no trace to land on. Document the choice in `docs/observability-cloud.md`
and add an integration test with an unsampled parent that asserts zero
exported spans and `sampled == false` on `run_started`.

### Tests

`trace/otel_test.go`: parent IDs under `StartWithContext`; an invalid or
absent parent yields a new root; cancelling the caller's context does
not cancel `RootContext()`. `core` tests: top-level run adopts the
parent; sub-agent root unchanged; follow-up starts a new trace.
`cmd/job_test.go`: a `task_assignment` with `trace_context` produces a
run whose exported root has that parent.

## S8. CLI flags (follow-up PR)

One PR after S2 and S6 have merged, so each flag maps onto a surface
that already exists on the wire.

- Correlation: `--correlation-session-id`, `--correlation-task-id`,
  `--correlation-attempt-id`, `--correlation-user-id`,
  `--correlation-url` in `harness/cmd/stirrup/cmd/runconfigflags.go`,
  shared by `stirrup harness` and `stirrup run-config`, mapped onto
  `RunConfig.Correlation` in `runconfigbuilder.go`. They are config, so
  `run-config` emits them.
- Upstream parent: `--traceparent` and `--tracestate` in `harness.go`
  only (harness-only behaviour, beside `--export-workspace-required`).
  They feed `observability.ExtractTraceContext` on the CLI path and
  never reach `RunConfig`; `stirrup run-config` does not know them.
- Tests: flag-to-config table for the correlation flags; a harness
  invocation with `--traceparent` exports a root whose parent matches;
  `run-config` rejects the parent flags.
- Docs: `docs/configuration.md` flag reference for both groups.

## S7. Git headers through the executor (deferred)

Prototype only after haybale strips trace headers at its external
boundary ([haybale#16](https://github.com/rxbynerd/haybale/issues/16))
and the executor env-overlay contract is agreed. Shape: for each
`run_command`, compose `GIT_CONFIG_*` entries
(`harness/internal/sandboxidentity/env.go:67` is the existing
composition to extend) adding
`http.<haybale-url>.extraHeader=traceparent: ...` from the command's tool
span, into that command's environment only. Harness-owned git work
(`git.setup`, `git.checkpoint` spans in `loop.go`) uses its own span.
Never mutate the container's process-global environment. Acceptance:
haybale's HTTP spans nest under the originating command's span, and two
concurrent commands never share a parent.

## Verification of the Stirrup packages

- Unit and wire tests listed per package above.
- A pinned local stack: Langfuse (self-hosted v4.x) and Stirrup with
  the `langfuse` profile, driven over gRPC by Hairpin as the
  functional-test control plane. For S4, a local MCP server that echoes
  `_meta` and headers; for S5, a control-plane stub that records the
  `tool_result_request` carrier. Assert span ancestry from the stored
  observations, not from span names.
- Identity: the `traceId` and `runSpanId` on `run_started`, `done` and
  `RunResult` equal the root span Langfuse stored for that run.
- Correlation: every stored observation of a run carries `session.id`,
  `user.id` and the `stirrup.*` keys, and the profile's `langfuse.*`
  mappings appear in the trace's metadata and user fields. Click the
  `control_plane_url` metadata value in the trace view; that renderer
  claim is source-only until checked in a browser.
- Negative paths: JSONL-only emitter (no reference anywhere); unsampled
  parent (`sampled: false`, zero exported spans); invalid `correlation`
  rejected by `ValidateRunConfig`; invalid carriers ignored at every
  extraction point.

The end-to-end feedback check (rate an attempt, restart, confirm the
score lands on that run span) belongs to the outer-application
worksheet.
