# Langfuse feedback and trace propagation: implementation worksheet

Status: proposal, 2026-10-17. Companion to
[the research worksheet](langfuse-feedback-and-trace-propagation-research.md),
which holds the evidence and the reasoning. This document holds only the
*how*: types, fields, call sites, tests and the order of the pull requests.
Every file and line reference was re-checked against Stirrup
`d65c91bd` and the sibling checkouts pinned in the research worksheet
(Hairpin `5421cde9`, Billet `d4fb0e15`, haybale `e7941a5b`).

The target control plane is Hairpin. Feedback is collected and submitted
by Hairpin; Stirrup only reports identity and propagates context.

## Direction choices taken here

Each of these changes the shape of the work. The worksheet proceeds on
the recommended column; the alternative is kept so the choice can be
reversed before implementation starts.

| Choice | Recommended | Alternative | Why |
|---|---|---|---|
| Where Langfuse-specific attributes are produced | A `langfuse` platform profile in `harness/internal/observability/platform/` | The collector `transform` recipe already in `docs/observability-cloud.md#langfuse` | The platform package is the one place allowed to name a vendor; a profile removes the collector from the feedback pilot and makes the OTLP/HTTP requirement a validation error instead of a silent drop |
| Carrier shape on the wire | One shared proto message `TraceContext { traceparent, tracestate }` reused on `HarnessEvent` and `ControlEvent` | Two bare string fields per message | One type, one translate helper, one validator; `baggage` is excluded on purpose |
| Who may set an upstream parent | The gRPC task assignment and the Go embedding caller's `ctx`; the CLI gets a `--traceparent` flag that sets the context, not the config | A `traceContext` block inside `RunConfig` | A replayed `RunConfig` must not re-parent a new run onto an old trace |
| Feedback subject in Langfuse | Observation score on the Stirrup run span (`traceId` + `observationId`) | Trace score | The run span is the execution; a trace may later contain Hairpin or other work |
| Order of delivery | S1 → S2 → S3 (feedback pilot), then S4/S5 (downstream), then S6 (upstream), S7 last | Upstream parenting first | Feedback needs only S1; nothing downstream or upstream blocks it |

## Work packages

| ID | Repository | Title | Depends on |
|---|---|---|---|
| S1 | Stirrup | Trace reference on `run_started`, `done`, `RunTrace`, `RunResult` | none |
| S2 | Stirrup | Typed application correlation block on `RunConfig`, stamped on spans | none |
| S3 | Stirrup | `langfuse` platform profile | S2 for the user/session/URL mapping |
| S4 | Stirrup | W3C context in MCP `tools/call` `_meta` and HTTP headers | none |
| S5 | Stirrup | W3C context on `tool_result_request` | shared `TraceContext` proto message (also used by S6) |
| S6 | Stirrup | Context-aware trace start: upstream parent from the task assignment, the embedding `ctx`, or `--traceparent` | shared `TraceContext` proto message |
| S7 | Stirrup + haybale | Per-command Git trace headers through the executor env overlay | agreed executor overlay contract; haybale boundary policy |
| H1 | Hairpin | Persist the reference, expose it, collect feedback, deliver scores | S1 (and S2 for links) |
| H2 | Hairpin | Extract the tool carrier, trace the Billet RPC client | S5, B1 |
| B1 | Billet | OTel setup, Connect interceptor, MCP `_meta` extraction | none (useful with S4) |
| Y1 | haybale | Strip trace headers at external upstreams by default | none (required before S7 is enabled) |

S1, S2, S4 and S6 touch `proto/harness/v1/harness.proto`. Land the
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
CLI: `--correlation-session-id`, `--correlation-task-id`,
`--correlation-attempt-id`, `--correlation-user-id`,
`--correlation-url` in `harness/cmd/stirrup/cmd/runconfigflags.go`.

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

New file `harness/internal/observability/platform/langfuse.go`,
registered in `registry` (`platform.go:80`) and in `validTraceEmitterPlatforms`
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

Goal: a valid W3C parent supplied by the control plane, the embedding
caller, or the CLI becomes the parent of the run span, without
inheriting the supplier's cancellation.

### Wire and CLI

`ControlEvent` gains `TraceContext *TraceContext` (proto field 10, read in
`controlEventFromProto` at `grpc_translate.go:42`). It is meaningful on
`task_assignment` only; other events ignore it. The CLI adds
`--traceparent` and `--tracestate` in `harness.go` (harness-only
behaviour, not `runconfigflags.go`), which set the run context and never
touch `RunConfig`; `stirrup run-config` does not know them.

### Extraction

One helper, `observability.ExtractTraceContext(ctx, traceparent, tracestate) context.Context`,
wraps `propagation.TraceContext{}.Extract` over a `MapCarrier`. An
invalid carrier returns `ctx` unchanged (the propagator already
guarantees this; the test pins it).

Call sites:

- `harness/cmd/stirrup/cmd/job.go:134` on `task_assignment`, before
  `BuildLoop`, producing the `runCtx` handed to `loop.Run`.
- `harness.go`, from the two flags, on the CLI path.
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
that case, which is how Hairpin learns that feedback on this run has no
trace to land on. Document the choice in `docs/observability-cloud.md`
and add an integration test with an unsampled parent that asserts zero
exported spans and `sampled == false` on `run_started`.

### Tests

`trace/otel_test.go`: parent IDs under `StartWithContext`; an invalid or
absent parent yields a new root; cancelling the caller's context does
not cancel `RootContext()`. `core` tests: top-level run adopts the
parent; sub-agent root unchanged; follow-up starts a new trace.
`cmd/job_test.go`: a `task_assignment` with `trace_context` produces a
run whose exported root has that parent.

## S7. Git headers through the executor (deferred)

Prototype only after Y1 lands and the executor env-overlay contract is
agreed. Shape: for each `run_command`, compose `GIT_CONFIG_*` entries
(`harness/internal/sandboxidentity/env.go:67` is the existing
composition to extend) adding
`http.<haybale-url>.extraHeader=traceparent: ...` from the command's tool
span, into that command's environment only. Harness-owned git work
(`git.setup`, `git.checkpoint` spans in `loop.go`) uses its own span.
Never mutate the container's process-global environment. Acceptance:
haybale's HTTP spans nest under the originating command's span, and two
concurrent commands never share a parent.

## H1. Hairpin: persist, expose, collect, deliver

1. `just sync-proto` after S1 merges; regenerate. Hairpin's unknown-event
   fallback (`internal/controlplane/pump.go:167`) stores events with
   `protojson`, so without the regenerated types a `trace_reference`
   field is dropped on the floor.
2. `eventPump`: handle `run_started` explicitly: persist
   `TraceID`, `RunSpanID`, `Sampled` on `job.Job`
   (`internal/job/job.go:51`, beside the existing `TraceParent` which
   is Hairpin's own submission context and must stay separate) together
   with the configured backend name, then `appendProto`. On `done`,
   if `trace.trace_reference` is present and the job has none, persist
   it (belt and braces for a dropped `run_started`).
3. Expose the reference on the JobService `Get` response and the
   `/jobs/{id}` page (`internal/web/web.go:77`), with a Langfuse deep
   link built from `LANGFUSE_HOST` and `LANGFUSE_PROJECT_ID`:
   `{host}/project/{projectId}/traces/{traceId}`.
4. Populate `RunConfig.correlation` when assembling the task:
   `sessionId` = the job's session or job ID, `attemptId` = job ID,
   `url` = `PUBLIC_ORIGIN + /jobs/{id}`, `userId` = submitter when the
   request carries one.
5. Feedback: `POST /jobs/{id}/feedback {value, comment}` authenticated
   as today's job endpoints. Persist a row keyed `jobID + reviewer`
   first; a feedback ID derived from that key is the Langfuse score `id`
   so retries and edits upsert. An outbox worker delivers
   `POST {host}/api/public/scores` with `traceId`, `observationId` =
   run span ID, `name: "helpfulness"`, `value`, `comment`, Basic auth
   from the project key pair. A job with `sampled == false` or no
   reference stores the feedback and marks delivery "no trace".
6. Tests: event replay after restart keeps the reference; rating the
   second of two attempts scores the second job's span; retry/edit
   upserts; delivery failure leaves the row and retries.

## H2. Hairpin: tool carrier to Billet

In `fulfilToolResult` (`pump.go:324`) extract the event's
`trace_context` into the parent of `hairpin.memory.call` instead of the
harness-stream context. Add the `otelconnect` interceptor to the Billet
client at `internal/memory/memory.go:83`. Acceptance: a proxied call's
Billet spans nest under the Stirrup `execute_tool` span.

## B1. Billet: instrumentation

Billet has no OTel dependency. Add an opt-in OTLP setup (same env
variables as haybale's `internal/observability/setup.go` for
consistency), the `otelconnect` interceptor on the RPC server
(`internal/rpcserver/server.go`), and in the MCP server
(`internal/mcpserver/server.go`) read `params._meta.traceparent`
(and the HTTP headers) to start a server span per `tools/call`. Record
operation, counts, duration and outcome only; no memory content.

## Y1. haybale: external boundary

haybale preserves caller headers in its proxy rewrite
(`internal/proxy/proxy.go:445`). Strip `traceparent`, `tracestate` and
`baggage` before forwarding to the external Git host by default, with a
config flag to forward them. Required before S7 is enabled anywhere.

## Verification before calling the pilot complete

- Unit and wire tests listed per package above.
- A pinned local stack: Langfuse (self-hosted v4.x), Stirrup with the
  `langfuse` profile, Hairpin, Billet. Assert span ancestry from the
  stored observations, not from span names.
- Product path: submit two jobs, rate the second, edit the rating,
  restart Hairpin, confirm the score sits on the second job's run span.
- Browser path: job page → Langfuse trace; trace metadata
  `control_plane_url` → job page.
- Negative paths: JSONL-only emitter (no reference anywhere), unsampled
  parent (`sampled: false`, no spans), feedback submitted after export
  failure (row retained, delivery marked).

## Open questions for the maintainer

1. Is Hairpin the right first control plane, and is its `/jobs/{id}`
   page the surface users rate from, or does an outer application own
   that page and the `correlation.url`?
2. Profile versus collector for the Langfuse mapping (table above).
3. Should `correlation.userId` be accepted at all in the first cut, or
   deferred until a pseudonymisation policy exists?
4. Should S6 land in the first wave, or wait until H1's feedback flow
   has run live without it?
5. Is a `--traceparent` CLI flag wanted, or is the gRPC path enough?
