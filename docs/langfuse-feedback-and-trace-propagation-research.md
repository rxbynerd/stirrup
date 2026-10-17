# Langfuse feedback and distributed tracing worksheet

Research date: 2026-10-17. Status: proposal; no runtime changes or live integration tests.

## Recommendation

Implement **both trace-reference reporting and W3C context propagation**. They solve
different problems. Reporting lets a control plane attach feedback to a particular
execution. Propagation lets services record their work in the same causal trace.
Neither requires deterministic trace IDs or a Langfuse SDK inside Stirrup.

Ship trace-reference reporting first. It enables feedback without changing who owns
the trace, and works through Hairpin if Hairpin persists and exposes the reference.
Then propagate each tool call's context to MCP servers and control-plane tools.
Accept upstream run context as a separate, optional capability. Keep feedback
collection and Langfuse score submission in the application/control plane.

The observability stack ending at [#778](https://github.com/rxbynerd/stirrup/pull/778)
improved the telemetry inside Stirrup and its export paths. The remaining work is
chiefly at service boundaries, application identity, and durable result delivery.

## Evidence and scope

| Source | Reviewed revision | Evidence |
|---|---|---|
| Stirrup | `d65c91bd833cc877a35c86d92ff91be1fdcb2824` | Current clean checkout; includes #778; code and PR metadata inspected |
| Hairpin | `5421cde927477b58ce03ede2624901ac210cd9c7` | Clean local checkout; remote HEAD not verified |
| haybale | `e7941a5b317c49803c1671008aab244dd0094a06` | Clean local checkout; remote HEAD not verified |
| Billet | `d4fb0e1536a3a4e811242944999c96c54bcc9f04` | Clean local checkout; remote HEAD not verified |
| Langfuse, OTel, W3C, MCP | Linked documentation retrieved on research date | Documentation/source evidence; deployed versions may differ |

The merged stack was #768 → #769 → #770 → #776 → #777 → #778. Its prior live
observations remain in [the platform assessment](../research/observability-platforms-2026-10.md).
This worksheet does not repeat those experiments or claim hosted acceptance. No
production control plane was inspected. Integration designs below are proposals.

## What works today

| Capability | Assessment | Source |
|---|---|---|
| Export Stirrup spans to Langfuse | Supported through OTLP/HTTP | [Existing recipe](observability-cloud.md#langfuse) |
| Agent, generation, tool and sub-agent spans | Supported internally, with live per-call contexts | [Live spans](../harness/internal/trace/otel_live.go), [dispatch](../harness/internal/core/livespans.go) |
| Group runs into a session | Partial: `SessionName` becomes `session.id` and `gen_ai.conversation.id` on every span | [Scope processor](../harness/internal/trace/otel_scope.go) |
| Return OTel identity to an integrator | Missing from events, run summaries and result sinks | [Events](../types/events.go), [RunTrace](../types/runtrace.go), [RunResult](../types/result.go) |
| Continue an upstream trace | Missing, including Go embedding | [Emitter start](../harness/internal/trace/otel.go), [loop start](../harness/internal/core/loop.go), [embedding API](../harness/harnessapi/harnessapi.go) |
| Propagate into MCP calls | Missing at the HTTP and JSON-RPC boundaries | [MCP client](../harness/internal/mcp/client.go) |
| Propagate into control-plane tools | Missing from `tool_result_request` | [Async dispatch](../harness/internal/core/types.go) |
| Supply per-run user identity or a session URL | Missing as a caller-configured surface | [RunConfig](../types/runconfig.go) |
| Add Langfuse-specific attributes | Supported through collector transforms | [Enrichment recipe](observability-cloud.md#langfuse) |

`RunTrace.ID` is the application `RunID`, **not** the OTel trace ID. The emitter's
`Start` creates its span from `context.Background()`; the loop then installs that
span on its execution context. Passing a traced context to `Loop.Run` does not
currently continue that trace. The internal `RootContext()` already exposes the
real span, so reporting its identity requires no ID-generation workaround.
[Pinned emitter source](https://github.com/rxbynerd/stirrup/blob/d65c91bd833cc877a35c86d92ff91be1fdcb2824/harness/internal/trace/otel.go#L234).

## The three identity options

| Option | Assessment | Recommendation |
|---|---|---|
| A. Return the actual trace ID | Ordinary correlation; does not change tracing semantics | Implement first; return the run span ID too |
| B. Receive upstream context | Standard distributed tracing when it names a real causal parent | Accept W3C `traceparent` and optional `tracestate`, not a bare trace ID |
| C. Derive a deterministic trace ID | Possible, but needs uniqueness, attempt and sampling rules | Do not make it the normal Stirrup contract |

OTel exposes trace/span IDs through `SpanContext`, including after a span ends. A
child inherits its parent's trace ID; a new root gets a new trace ID. There is no
specification problem with returning those IDs in an application event. Inbound
context also needs the parent span ID and flags, which a trace ID alone omits.
[OTel tracing API](https://opentelemetry.io/docs/specs/otel/trace/api/).

Use an OTel propagator to inject/extract the W3C carrier. Invalid propagation data
must not replace an existing valid context. A protobuf message is a suitable
carrier; propagation need not be an HTTP header.
[Propagators specification](https://opentelemetry.io/docs/specs/otel/context/api-propagators/).

Langfuse supports seeded deterministic IDs. That does not make them necessary for
feedback. W3C recommends globally unique, random trace IDs; deterministic generation
is not categorically forbidden. The practical problem is reusing a ticket or
session seed across independent attempts, collapsing them into one trace. Avoid
fabricating a parent span solely to force a caller-selected trace ID. Store task,
attempt and session identity separately.
[Langfuse IDs](https://langfuse.com/docs/observability/features/trace-ids-and-distributed-tracing),
[W3C ID generation](https://www.w3.org/TR/trace-context/#considerations-for-trace-id-field-generation).

The UI does not strictly need the raw trace ID. It can submit feedback against its
own response/run identifier, and its backend can resolve the persisted trace
reference. Langfuse documents both browser and server-side score submission; its
browser example uses a dedicated scores-ingestion public key. For Equestrianism,
server-side submission is the simpler ownership boundary: the application already
knows who may rate which execution.
[User feedback](https://langfuse.com/docs/observability/features/user-feedback).

## Proposed trace-reference contract

Add an optional vendor-neutral `TraceReference` to the public types and protobuf
schema. Illustrative JSON, not an implemented API:

```json
{
  "type": "run_started",
  "runId": "run-123",
  "traceReference": {
    "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
    "runSpanId": "00f067aa0ba902b7",
    "sampled": true
  }
}
```

Emit it immediately after the run span exists, before model/tool work. Repeat the
same reference in final `RunTrace`, the `done` summary and `RunResult`. This covers
streaming clients, Go embedding and clients that only consume result sinks. Do not
put it on `ready`: that event precedes assignment and span creation. Identity should
remain available on cancellation and failures after span creation; setup failures
before a span exists should leave it absent. With no OTel run span, omit the reference
rather than presenting an application run ID as one.

The implementation must update explicit copy/translation paths, especially
[`doneSummary`](../harness/internal/core/handoff.go),
[`runTraceToProto`](../harness/internal/transport/grpc_translate.go), and result
construction. Current code does attach a summary to ordinary `done` events;
the statement to the contrary in [the integration guide](integration-guide.md)
is stale. Early construction failures still have no run summary.

Treat the reference as correlation, not an ingestion receipt. Keep backend host,
project and URL construction in integration configuration. The control plane should
persist the backend destination alongside the reference when routing can vary by
tenant or over time; an old run must not silently acquire today's project URL.

Persist a mapping like:

```text
application response/task + execution attempt
    -> Stirrup run ID
    -> OTel trace ID + Stirrup run span ID
    -> backend destination + application session ID
```

One task can have several attempts. A follow-up can produce another run. Preserve
each reference and associate feedback with the response/attempt actually shown to
the user. The existing follow-up loop generates fresh RunIDs and resets its trace
context; new fields must preserve that behavior.
[Follow-up implementation](../harness/internal/core/loop.go).

## Feedback flow and ownership

```mermaid
sequenceDiagram
    participant UI as User surface
    participant CP as Control plane
    participant H as Hairpin (optional)
    participant S as Stirrup
    participant L as Langfuse
    CP->>H: Submit attempt and application correlation
    H->>S: Task assignment; optional W3C parent context
    S-->>H: run_started with actual trace/run-span IDs
    H-->>CP: Persisted trace reference
    S->>L: OTLP spans, directly or through collector
    CP-->>UI: Response tied to attempt
    UI->>CP: Rating/comment for response
    CP->>CP: Authorize and persist feedback
    CP->>L: Score using persisted trace/span reference
```

The direct path omits Hairpin. Feedback does not need to travel back through a
completed Stirrup process, and does not require keeping its span open.

Langfuse scores can target a whole trace or a specific observation. Observation
scores require both `traceId` and `observationId`; for an OTLP span the observation
ID is its span ID. Use the Stirrup run span for “how did this execution perform?”
when the enclosing trace also contains orchestration or other runs. Use a trace
score when the intended subject really is the whole trace. Scores may arrive before
the trace and link later. Stable score IDs support idempotent updates.
[Score API/SDK](https://langfuse.com/docs/evaluation/evaluation-methods/scores-via-sdk),
[OTLP observation identity example](https://langfuse.com/integrations/native/opentelemetry/experiments).

Proposed application responsibilities:

1. Resolve a response/attempt to its stored reference; do not trust an arbitrary
   browser-supplied trace ID as authorization.
2. Persist the rating and optional comment before attempting backend delivery.
3. Use a stable feedback ID, scoped to the intended rating policy, for retries and
   edits. Multiple reviewers must not accidentally overwrite one another.
4. Retry delivery through an outbox and retain feedback when tracing is unavailable.
5. Store the configuration revision/model identity needed to compare agent setups.
   A rating alone does not identify which configuration deserves credit or blame.

Keep human feedback distinct from verifier pass/fail and inferred behavioral scores.
They answer different questions. Start with execution helpfulness plus an optional
reason, then decide which dimensions are useful from real operator feedback.

## Upstream context and trace lifetime

Add a small `TraceContext` carrier (`traceparent`, `tracestate`) to task assignment
and CLI JSON configuration. Treat it as invocation context, not reusable agent
configuration. Replaying a saved RunConfig should clear it unless continuation is
explicitly intended. Go embedding should use the caller's context directly. Define
precedence so a supplied assignment carrier is extracted at the boundary, once;
the core receives the resulting context rather than competing parent sources.

Make trace start context-aware: `Start(ctx, ...)` or a compatible optional interface.
The parent must reach span creation, not merely exporter construction. Preserve
cancellation/deadlines while changing span ancestry; this was a substantive fix in
[#770](https://github.com/rxbynerd/stirrup/pull/770).

For the long-lived bidi stream, use **per-message context** on task assignments and
tool requests. Stirrup opens the RPC before the control plane assigns work; its
initial gRPC metadata cannot carry a later task's parent or distinguish concurrent
tool calls. Follow-up runs need their own context decision, rather than automatically
reusing the first assignment's parent. Input added during an active run can be linked
to that run without trying to change its already-established parent.

Recommended lifetime policy:

| Work | Proposed relationship |
|---|---|
| One execution attempt, its model calls, tools, and nested sub-agents | One trace with normal parent/child spans |
| A synchronous upstream execution span | Parent of Stirrup's run span when explicitly supplied |
| Queued submission, later attempts, long-lived sessions | Separate traces where useful; link causality and group by stable business/session IDs |
| Later feedback | Independent application operation referring to the completed observation |

Hairpin already separates submission, launch and harness-stream traces, with links
back to persisted submission context. Retain that operational design initially.
Reporting Stirrup's reference is enough for feedback. Later, Hairpin can pass a
real execution/dispatch span as parent or intentionally start a separate execution
trace with a link. The optional upstream feature should not force one multi-hour
trace for an entire job/session.
[Hairpin observability](https://github.com/rxbynerd/hairpin/blob/5421cde927477b58ce03ede2624901ac210cd9c7/docs/observability.md),
[recorder](https://github.com/rxbynerd/hairpin/blob/5421cde927477b58ce03ede2624901ac210cd9c7/internal/telemetry/recorder.go#L304).

OTel links express relationships across traces, but should not be relied on for
Langfuse navigation: its [span-links request](https://github.com/langfuse/langfuse/issues/12337)
remained open at review, and the inspected ingestion path did not map them. A shared
trace is the practical way to show ordinary remote tool work in one Langfuse tree.
Separate attempts can still share a Langfuse session.
[Inspected ingestion source](https://github.com/langfuse/langfuse/blob/e3a2bee2a2c048adf29b8d8a18b29bd4a8bf8ac3/packages/shared/src/server/otel/OtelIngestionProcessor.ts).

## Navigation and application metadata

Application → Langfuse is documented through trace-URL helpers. Build links in the
control-plane adapter using its configured Langfuse host/project; ordinary private
trace links require the viewer's Langfuse access.
[Trace URLs](https://langfuse.com/docs/observability/features/url).

Langfuse → application has a practical source-backed option: emit a short (under 1,500 characters)
`control_plane_url` metadata string. Langfuse's formatted metadata table uses a
renderer that turns URL strings into external links. This was verified in source
at `e3a2bee2a2c048adf29b8d8a18b29bd4a8bf8ac3`, not in a running deployment; raw/advanced
JSON views differ. Test click-through on the deployed version before calling this
accepted. [URL renderer](https://github.com/langfuse/langfuse/blob/e3a2bee2a2c048adf29b8d8a18b29bd4a8bf8ac3/web/src/components/table/ValueCell.tsx#L49).

[Web Callouts](https://langfuse.com/docs/observability/features/web-callouts) execute
backend POST requests. They are useful for actions, but are not a documented browser
navigation feature. No callout is needed merely to provide a session link.

There are two implementation sizes:

| Option | Benefit | Limit |
|---|---|---|
| Collector constructs a URL from existing root `run.id` | Deployment-only bridge when URL mapping is predictable | Stirrup run ID may differ from the application's session/job ID; children lack that attribute |
| Add bounded application correlation to RunConfig | Works for arbitrary control planes and supplies URL, session, user and attempt identity | Needs schema, validation, span stamping and integration changes |

Prefer a small typed application-correlation block over an unrestricted attribute
bag for the first implementation: opaque session ID, display name, task/attempt ID,
optional user reference, and canonical session/run URL. Names are design proposals.
Keep the existing `SessionName` fallback for compatibility; an explicit session ID
should take precedence for grouping. Reusing a display label such as “nightly-triage”
can otherwise combine unrelated sessions.

Stamp approved correlation attributes on all relevant spans. Map those neutral keys
to `langfuse.trace.metadata.*`, observation metadata and user/session fields in the
collector, or later in a Langfuse profile inside the existing platform package.
Langfuse v4 needs the attributes on relevant observations for filtering; generic
unmapped attributes do not provide the same explicit metadata mapping.
[OTLP attribute mapping](https://langfuse.com/integrations/native/opentelemetry).

Langfuse's own guidance for getting trace-level attributes onto every span is
OpenTelemetry baggage plus a `BaggageSpanProcessor` in the emitting process. The
OTLP endpoint reads span attributes only, never baggage. Stirrup's run-scope
processor already stamps every span at start, so the recommendation is met
in-process without baggage. Across service boundaries, baggage would only let
downstream spans match observation-level session and user filters, and the same
page warns that baggage propagates to third-party APIs and must carry no personal
data, which is the haybale concern above. Langfuse also reads `user.id` and
`session.id` natively beside its `langfuse.*` keys, filters only top-level
`langfuse.trace.metadata.*` keys, drops session IDs of 200 characters or more,
and names a trace after its root span unless `langfuse.trace.name` is set.
[Propagation guidance](https://langfuse.com/integrations/native/opentelemetry),
[session limits](https://langfuse.com/docs/observability/features/sessions).

The outermost control plane should supply its canonical URL. Hairpin's
[`/jobs/{id}` route](https://github.com/rxbynerd/hairpin/blob/5421cde927477b58ce03ede2624901ac210cd9c7/internal/web/web.go#L77)
is a useful secondary operator link, but may not be the user's session surface.
Use a configured public origin and ordinary authenticated URLs, not signed tokens.
Keep these fields out of prompts and service resource attributes. Per-run values
also do not belong on metric labels. Propagate application metadata explicitly;
W3C trace context does not carry span attributes automatically.

## Downstream propagation

Stirrup already passes a distinct tool-span context through each dispatch. Extend
the boundaries using that context; do not reuse the run span for every remote call.
[Per-call dispatch](https://github.com/rxbynerd/stirrup/blob/d65c91bd833cc877a35c86d92ff91be1fdcb2824/harness/internal/core/dispatch.go#L101).

### MCP and Billet

Inject W3C context into `tools/call.params._meta`, outside model-facing `arguments`.
The versioned [MCP 2026-07-28 specification](https://modelcontextprotocol.io/specification/2026-07-28/basic#_meta)
reserves `traceparent`, `tracestate` and `baggage`; Langfuse also documents this
[MCP tracing pattern](https://langfuse.com/docs/observability/features/mcp-tracing).
Start with the first two fields. Adding propagation does not constitute support for
the whole 2026 protocol revision; test against the actual peer SDK/protocol versions.

For HTTP transport spans, W3C headers can accompany message metadata. Define the
relationship: transport context parents HTTP work; valid message metadata is the
parent for logical MCP work, with a documented fallback when absent. Test conflicting
carriers and avoid creating duplicate logical server spans. Never put per-call
headers in a shared static header map.

Billet currently has no OTel instrumentation/dependency. Its MCP handlers discard
the request argument, and its Connect RPC server lacks tracing middleware. Add
extraction, spans and opt-in OTLP export for both entrances, then shared service and
backend spans. A propagated ID without recording/export creates no visible Billet
work. Keep memory content out of telemetry; operation, counts, duration and outcome
are sufficient for an initial integration.
[Billet module](https://github.com/rxbynerd/billet/blob/d4fb0e1536a3a4e811242944999c96c54bcc9f04/go.mod),
[MCP server](https://github.com/rxbynerd/billet/blob/d4fb0e1536a3a4e811242944999c96c54bcc9f04/internal/mcpserver/server.go),
[RPC server](https://github.com/rxbynerd/billet/blob/d4fb0e1536a3a4e811242944999c96c54bcc9f04/internal/rpcserver/server.go).

### Control-plane tools through Hairpin

Add the active tool's carrier to `tool_result_request`. Hairpin should extract it
when starting `hairpin.memory.call`, then inject into its Billet RPC client. Today
that span uses the harness-stream context, and the client has no tracing interceptor.
[Hairpin tool dispatch](https://github.com/rxbynerd/hairpin/blob/5421cde927477b58ce03ede2624901ac210cd9c7/internal/controlplane/pump.go#L324),
[Billet client construction](https://github.com/rxbynerd/hairpin/blob/5421cde927477b58ce03ede2624901ac210cd9c7/internal/memory/memory.go#L83).

Preserve existing request IDs and tool-use IDs: they correlate replies, whereas trace
context describes causality. Timeout, retry and async completion must retain the
correct call reference without changing a shared stream context. This work does not
enable control-plane tools inside Stirrup sub-agents, which currently have
`NullTransport` and reject that path.

Hairpin vendors Stirrup's proto. Its unknown-event fallback uses `protojson` on the
decoded message, so new unknown protobuf fields do not automatically survive into
JSON event storage. Coordinate `sync-proto`, generated types, explicit persisted
run/attempt references, JobService output and event replay. A new event name alone
is insufficient.
[Schema sync](https://github.com/rxbynerd/hairpin/blob/5421cde927477b58ce03ede2624901ac210cd9c7/Justfile#L21),
[event handling](https://github.com/rxbynerd/hairpin/blob/5421cde927477b58ce03ede2624901ac210cd9c7/internal/controlplane/pump.go#L167).

### Sandbox Git through haybale

haybale is a Git HTTP proxy, not an MCP server. It already extracts incoming W3C
headers and records HTTP/upstream work when telemetry is configured. The missing
caller-side step is getting context from a Stirrup operation into Git's HTTP calls.
[HTTP tracing setup](https://github.com/rxbynerd/haybale/blob/e7941a5b317c49803c1671008aab244dd0094a06/internal/observability/setup.go#L189).

Prototype URL-scoped Git `http.<haybale-url>.extraHeader` values in a **per-execution**
environment overlay. Compose with existing sandbox identity `GIT_CONFIG_*` entries;
do not mutate a process-global environment or persist one span ID for the container's
lifetime. For `run_command`, requests from that command can descend from its tool
span; haybale supplies individual HTTP spans. Harness-owned checkout/checkpoint work
should use its own operation span. Full tracing of arbitrary shell descendants is
a separate capability, not an automatic consequence of setting an environment variable.
[Existing Git environment composition](../harness/internal/sandboxidentity/env.go).

Decide the proxy's outbound boundary before enabling this. haybale suppresses
automatic injection of its own context, but preserves caller headers in its proxy
rewrite. Newly supplied tracing headers can therefore reach the external Git host.
Default to stripping them at external upstreams unless that continuation is intended;
do not forward baggage by default.
[Proxy behavior](https://github.com/rxbynerd/haybale/blob/e7941a5b317c49803c1671008aab244dd0094a06/internal/proxy/proxy.go#L445).

All participating services must export to the same Langfuse project for the joined
tree. Keep distinct `service.name` values. A collector can route the execution subtree
to Langfuse and operational telemetry elsewhere, but must not rewrite trace/span IDs
or silently drop the parents needed for the view being promised.

## Sampling and delivery

Stirrup constructs its provider without an explicit sampler. The SDK default is
parent-based sampling with sampled roots; environment configuration can override it.
Accepting an unsampled upstream parent can therefore suppress the run's exported
subtree. This needs an explicit integration test and operator guidance.
[Provider construction](../harness/internal/trace/otel.go),
[OTel sampling specification](https://opentelemetry.io/docs/specs/otel/trace/sdk/).

A valid trace ID and even `sampled: true` do not prove storage. Stirrup currently logs
`ForceFlush` failures without failing the run. Collectors can also sample or drop data,
and retention can expire it. Never equate successful execution with “available in
Langfuse.” Store feedback independently and distinguish pending/unavailable telemetry
in the application. Late feedback cannot resurrect spans that were never recorded.

For an initial feedback pilot, retain all execution traces intended to receive
feedback, subject to the deployment's content policy. Keep payload capture optional;
IDs, timings and ratings do not require `captureContent`. If the upstream request is
aggressively sampled, choose an explicitly separate sampled execution trace with a
causal link, or adjust sampling at the execution boundary. Do not silently override
incoming flags just to make the UI appear complete.

Use the existing OTLP/HTTP recipe and `x-langfuse-ingestion-version: 4` header.
Current Langfuse docs warn of delayed visibility without it. Validate score and
observation reads against the deployed API version rather than old trace-query
examples. [Langfuse OTLP integration](https://langfuse.com/integrations/native/opentelemetry).

## Delivery plan

These are proposed work packages, not newly created issues. One control-plane
integration owner should own the end-to-end acceptance; repository changes alone
cannot establish it.

| Step | Owner and change | Depends on | Acceptance |
|---|---|---|---|
| 1 | Stirrup: trace-reference type, start event, final summaries/results, proto and docs | None | Exported IDs equal reported IDs; absence and failure semantics are explicit |
| 2 | Hairpin + outer control plane: schema sync, durable attempt mapping, response association, feedback outbox | 1 | Restart/replay preserves reference; a user rating lands on the intended run observation |
| 3 | Stirrup + control plane: typed application correlation and collector mapping | 1 for full UI flow | Session grouping uses stable IDs; clickable application backlink works on deployed Langfuse |
| 4 | Stirrup + Billet: MCP carrier and Billet instrumentation | Existing per-tool spans | One trace contains the tool and its Billet descendants; parallel calls have distinct parents |
| 5 | Stirrup + Hairpin + Billet: per-tool event carrier and RPC instrumentation | 4's Billet telemetry; coordinated proto update | Proxied calls nest beneath their originating Stirrup tool span |
| 6 | Stirrup + upstream integrators: context-aware run start and assignment/follow-up carriers | Agreed lifetime/sampling policy | Valid parent is continued; standalone runs still work; cancellation and follow-ups remain correct |
| 7 | Stirrup executor + haybale: per-execution Git headers and external boundary policy | Agreed executor overlay contract | Git spans join the right command; concurrent commands do not leak context |

Implementation of steps 3–6 can overlap after agreeing the small shared carrier/reference
types; integration acceptance follows the dependencies in the table. Feedback should
not wait for all downstream services or an upstream-parenting
redesign. The user/session enrichment part overlaps
[#775](https://github.com/rxbynerd/stirrup/issues/775), which remains open; extend or
split that work deliberately rather than creating a competing identity design.

Minimum verification before calling the integration complete:

- In-memory span tests: reported IDs, parent IDs, sampled/unsampled contexts, invalid
  carriers, no exporter, cancellation, sub-agents, retries and follow-up isolation.
- Wire tests: JSON/proto translation, `doneSummary`, result sinks, old/new peer
  behavior, Hairpin persistence/replay and parallel tool requests.
- A pinned local Langfuse/collector integration: direct Stirrup → Billet MCP,
  Stirrup → Hairpin → Billet RPC, and Git → haybale. Assert ancestry from exported
  spans and stored observations, not merely matching names.
- Product test: rate the second of two attempts, retry/edit that rating, restart the
  control plane, and verify the score still belongs to the selected run. Exercise
  export delay, dropped telemetry and feedback after execution finishes.
- Browser check: click from the application to the correct private Langfuse trace,
  then from metadata back to the correct application session. Inspect session filters
  and the downstream span tree on the deployed version.

Existing targeted test homes include `core/livespans_otel_test.go`,
`core/subagent_otel_test.go`, `core/dispatch_parallel_test.go`,
`trace/otel_scope_test.go`, `transport/grpc_translate_test.go` and the MCP client tests.
This worksheet was checked for local links and Markdown whitespace; runtime tests
are reserved for the implementation work above.
