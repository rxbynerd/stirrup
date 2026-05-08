# Research Packet B — Architecture & Ingestion Contract (Issue #104)

Researcher: B (architecture decision and ingestion contract)
Sister tracks: A (industry survey), C (storage shape & schema evolution), D (security/multi-tenancy/eval CLI/migration)
Date: 2026-05-08

---

## 1. Current state

The `TraceLakehouse` interface (`types/lakehouse.go:9-27`) defines six methods: `StoreTrace`, `StoreRecording`, `QueryTraces`, `QueryRecordings`, `Metrics`, `Close`. Only one implementation exists — `eval/lakehouse/filestore.go::FileStore`, which marshals `RunTrace` and `RunRecording` to indented JSON files under `<root>/traces/<id>.json` and `<root>/recordings/<runId>.json`.

**Crucial finding: no production code path writes to the lakehouse.** A grep over the entire repo for `StoreTrace` / `StoreRecording` returns:

- `types/lakehouse.go` (interface)
- `eval/lakehouse/filestore.go` (impl)
- `eval/lakehouse/filestore_test.go` (the only callers — all in tests)

The eval CLI (`eval/cmd/eval/main.go:236, 294, 367, 530`) opens a `FileStore` only for read paths (`Metrics`, `QueryRecordings`). The eval **runner** (`eval/runner/runner.go:165, 200-227`) parses JSONL trace files written by the harness directly — it does *not* call `StoreTrace`. Tasks land in `SuiteResult.Tasks[].Trace`, never in the lakehouse.

Likewise, `RunRecording` is constructed nowhere outside tests. The harness records `TurnRecord` only for replay-test fixtures (`harness/internal/provider/replay.go`, `executor/replay.go`); there is no production recorder. `ProductionTrace` (`types/metrics.go:42-48`) is a wholly unused type.

**Where `RunTrace` actually goes today:**

1. The `TraceEmitter` (JSONL or OTel) accumulates turn / tool-call events during the run (`harness/internal/trace/jsonl.go:49-60`; `harness/internal/trace/otel.go:119-177`).
2. At end-of-run, `loop.go:307` calls `Trace.Finish(ctx, outcome)`. The JSONL emitter writes the trace as a single JSON line to the configured file; the OTel emitter ends the root span and flushes the OTLP exporter (`otel.go:181-233`). Both return `*RunTrace` to the caller in-process.
3. `loop.go:294-298` emits a `done` `HarnessEvent` over the `Transport`. Critically, **this event does NOT set `event.Trace`** — the wire field is nil. The proto comment at `harness.proto:74` ("trace: RunTrace with execution metrics") is aspirational. The translator `grpc_translate.go:26-28` would forward a non-nil `e.Trace`, but the loop never populates one.
4. `runTraceToProto` (`grpc_translate.go:64-73`) intentionally projects the full `RunTrace` (9 fields) onto a 7-field proto (`harness.proto:533-556`). Missing on the wire: `Config`, `ToolCalls[]`, `VerificationResults[]`, `StartedAt`, `CompletedAt`. This is not a bug *per se* — it is a deliberate "lightweight summary" wire shape — but the asymmetry matters for #104.

**OTel emitter coverage (vs. RunTrace):** the OTel emitter creates a root `run` span with attributes `run.id`, `harness.version`, `run.mode`, `run.provider`, `run.model`, `run.session_name` (`otel.go:91-110`), per-turn child spans with `turn.number`, `turn.tokens.{input,output}`, `turn.tool_calls`, `turn.stop_reason`, `turn.duration_ms` (`otel.go:140-150`), and per-tool-call spans with `tool.name`, `tool.success`, `tool.duration_ms` (`otel.go:168-176`). Sub-agent forwarding tags entries with `RunID` / `ParentRunID` for parent-only filter correctness (`types/runtrace.go:42-49`, `eval/lakehouse/filestore.go:239-254`).

**Sub-agent fan-in:** `core/subagent.go:110-126` wraps the parent emitter in a `NestedJSONLEmitter` (despite the name, it works for OTel parents too via the `TraceEmitter` interface). Every child Turn / ToolCall is forwarded to the parent emitter tagged with the child's `RunID` and the parent's `ParentRunID`. Aggregation that consumes `RunTrace.ToolCalls` *must* filter via `parentOnlyToolCalls` (`filestore.go:239`); otherwise sub-agent activity double-counts against parent aggregates. This is load-bearing for #55 and informs the ingestion-contract sketch below.

**VERSION1.md alignment:** "A file-based adapter (`eval/lakehouse/filestore.go`) is implemented for development and CI. Production adapters (Postgres, BigQuery) depend on control plane choices and are deferred" (`VERSION1.md:293`). #104 is the issue that finally closes that deferral.

---

## 2. The OTel-vs-lakehouse question (Q2)

### Field-by-field overlap (RunTrace ↔ OTel spans)

| RunTrace field | OTel span attribute (today) | Notes |
|---|---|---|
| `ID` | `run.id` (root span) | 1:1. |
| `Config.Mode` | `run.mode` | 1:1. |
| `Config.Provider.Type` | `run.provider` | 1:1. |
| `Config.ModelRouter.Model` | `run.model` | 1:1. |
| `Config.SessionName` | `run.session_name` | 1:1, omitted when empty. |
| `Config.*` (full RunConfig) | **missing** | Redact() runs in `Finish()` then is dropped on the OTel path. The full redacted config is *only* kept in the in-memory aggregate that `Finish` returns (`otel.go:216-230`). |
| `StartedAt` / `CompletedAt` | span start/end timestamps | derived, not as discrete attrs. |
| `Turns` (count) | `run.turns` (set in `Finish`) | 1:1. |
| `TokenUsage.{Input,Output}` | derived from sum of `turn.tokens.*` child spans | aggregable via SQL but not present as a root attr. |
| `Outcome` | `run.outcome` (root, set in `Finish`) | 1:1. |
| `ToolCalls[].{Name,Success,DurationMs}` | per-tool-call child span attrs | 1:1, except `InputSize` / `OutputSize` / `ErrorReason` are not recorded as span attrs (only in the in-memory aggregate). |
| `ToolCalls[].{RunID,ParentRunID}` | **missing** as span attrs | The forwarding emitter tags the in-memory aggregate but does not set `run.parent_id` on tool-call spans. The TODO at `otel.go:133-139` documents that turn[N] spans don't even nest under the parent's `tool.spawn_agent` span (#89). For BigQuery / ClickHouse OTel exporters this means parent-only filtering is harder than the in-process `parentOnlyToolCalls` filter. |
| `VerificationResults[]` | the loop creates a `verification` span (per `core/factory.go` and `loop.go`), but `Passed` / `Feedback` / `Details` are not consistently span attrs. | Audit needed; survey track A may already have. |

### Disjoint OTel-only attributes

- `harness.version` (root span) — useful for cohort analysis; not in `RunTrace`.
- `service.name`, `service.version`, etc. via `observability.Resource()` — process-level metadata never round-tripped into `RunTrace`.
- Future GenAI semconv attributes (`gen_ai.system`, `gen_ai.request.model`, `gen_ai.response.id`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`) — emerging stable spec [OpenTelemetry GenAI semconv, https://opentelemetry.io/docs/specs/semconv/gen-ai/]. Today the emitter uses stirrup-specific attribute names; aligning would cost ~20 lines and make off-the-shelf APM dashboards work.
- Trace context (`trace_id`, `span_id`) for cross-system correlation — entirely absent from `RunTrace`.

### Access-pattern differences (APM vs eval)

APM-style queries (Tempo, Honeycomb, Cloud Trace, Jaeger) optimise for:

- Low-latency single-trace retrieval by `trace_id` (UI: "show me this run's span tree").
- Recent-only retention (typical default 7-30 days; older spans tiered out or sampled).
- Span-tree traversal (parent-child on `span_id` references).
- Service-graph aggregation (count of inter-service calls).
- Tail-based sampling (1-100% of error/slow traces, 1-10% of OK).

Eval-style queries (the four `eval` subcommands at `eval/cmd/eval/main.go`):

- `baseline`: aggregate `Metrics(filter)` over a multi-month window — pass rate, mean turns, P50/P95 — per `(mode, model)` cohort.
- `mine-failures`: full-payload retrieval of `RunRecording` (the 1MB+ one with full prompts/tool I/O) for non-success outcomes, projected into eval tasks.
- `drift`: window-over-window `Metrics()` comparison.
- `compare-to-production`: full table-scan join of lab variant results vs production aggregates.

These are batch / OLAP / ad-hoc-SQL workloads. Sampling is hostile (you'd skew your pass rate). Retention horizons are months-to-years (you want to compare today's release to last quarter's baseline). Access pattern is "table scan with predicate" not "trace-by-id".

### Is `TraceLakehouse` redundant under each architecture?

- **Architecture 1 (harness writes BQ/ClickHouse direct):** `TraceLakehouse` is the natural seam. Not redundant.
- **Architecture 2 (CP writes):** `TraceLakehouse` lives in the CP. The harness module no longer needs the interface — the eval CLI does. Either move it to a shared module, or have the eval CLI call the CP's read API. Not redundant within the eval CLI; *redundant within the harness module*.
- **Architecture 3 (OTel-derived):** `TraceLakehouse` becomes a SQL view layer. `Metrics()` is `SELECT count(*), avg(turns), ... FROM stirrup_runs WHERE ...` against a span store (BigQuery via OTel GCP exporter; ClickHouse via the official ClickHouse OTel exporter [https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter]; Tempo via TraceQL with metrics-generator). This is operationally tenable — Honeycomb, Datadog, and Grafana Tempo all expose SQL/metrics-generator surfaces over span stores — but with two real frictions:
  1. OTel-shaped span tables are wide-and-sparse with attributes-as-bags. Eval's structured fields (`Outcome`, `Mode`, `TokenUsage`) become attribute-bag lookups (`SpanAttributes['run.outcome']`). ClickHouse handles this fine; BigQuery is workable but verbose. [ClickHouse OTel schema docs, https://clickhouse.com/docs/en/observability/integrating-opentelemetry]
  2. Recordings (1MB+) have no good home in a span store. Spans cap event payloads (Tempo: 1MB hard limit per span; OTLP: 4MB default gRPC max message size). You can't shove `RunRecording.Turns[].ModelInput.Messages[]` into a span attribute. Architecture 3 needs a *second* store for recordings or it doesn't satisfy `mine-failures`. So under arch 3, `TraceLakehouse` survives — but only for recordings. Aggregate metrics move to a SQL-over-spans view.

**Bottom line on Q2:** the OTel path *almost* covers `RunTrace.Metrics()`-style queries today, modulo (a) some attribute renaming and (b) the missing full `Config` and `VerificationResults`. It does *not* cover `RunRecording`. So a second store is genuinely needed under any architecture that wants `mine-failures` to keep working — but that store is recording-shaped, not metric-shaped.

---

## 3. Three architectures, scored (Q1)

### Scoring rubric

5 = strongly aligned with stirrup constraints; 1 = strongly misaligned. Rubric tied to CLAUDE.md's stated philosophy ("minimal-dependency", "harness is short-lived job, not a server", "control plane is the long-running operator", "API keys never enter the container") and issue #8's items 6.1-6.3.

| Dimension | Arch 1 (harness writes) | Arch 2 (CP writes) | Arch 3 (OTel-derived) |
|---|---:|---:|---:|
| Harness dep surface | 1 | 5 | 4 |
| Coupling harness↔CP | 5 | 2 | 4 |
| OTel duplication | 1 | 3 | 5 |
| Eval CLI ergonomics | 4 | 2 | 3 |
| Security model alignment (#8) | 2 | 5 | 3 |
| Schema-evolution churn | 2 | 4 | 3 |
| Failure mode if lakehouse down | 2 | 4 | 5 |
| Cost / op complexity | 2 | 3 | 3 |
| **Sum** | **19** | **28** | **30** |

### Architecture 1: Harness writes directly to BQ / ClickHouse

**Harness dep surface: 1.** A BigQuery Storage Write API client transitively pulls `cloud.google.com/go/bigquery`, `cloud.google.com/go/iam`, the GAX gRPC machinery, and ~80MB of generated Cloud APIs proto. ClickHouse via `github.com/ClickHouse/clickhouse-go/v2` is lighter (~5MB of deps including chproto), but adds a second wire format the harness must maintain. A `kafka-go` or `pubsub` shim is comparably heavy. Either choice violates "the harness is a short-lived job with hand-rolled HTTP" (CLAUDE.md, "External dependencies rationale"). Stirrup's existing precedent (Anthropic, OpenAI, GitHub Contents API) is to avoid SDKs even when they exist.

**Coupling: 5.** Harness ships completely standalone. CP can be any/none.

**OTel duplication: 1.** This is the architecture that creates the most parallel pipes. The OTel emitter still ships its OTLP/gRPC export, the new BQ/ClickHouse adapter ships its own, and every byte of trace data is encoded twice on the harness's outbound link.

**Eval CLI: 4.** OSS contributor experience is fine — `FileStore` keeps working. CI is fine — the file adapter is the test path. Production adapter just plugs in.

**Security alignment (#8): 2.** This architecture is the worst for #8. The harness needs BQ/ClickHouse write credentials, which violates "API keys never enter the container" and re-introduces credential federation in a new dimension (now we federate to two clouds: provider creds + lakehouse creds). The container executor explicitly drops capabilities and `NetworkMode: none` (CLAUDE.md, "Container executor"); a write path to BigQuery breaks that posture unless we run the writer in a sidecar. RunRecording encryption-at-rest (#8 item 6.1) becomes the harness's job.

**Schema evolution: 2.** Schema migrations now span the harness binary. A field added to `RunTrace` requires harness rollout *and* schema migration in lockstep. The `harness_version` column becomes load-bearing for backward-compat selects.

**Failure mode if lakehouse down: 2.** Harness blocks (worst), drops (loses data), or spools to disk (re-introduces local state in a job that's supposed to be ephemeral). All three are bad.

**Cost / op complexity: 2.** Each harness instance hits BQ/ClickHouse independently — N concurrent runs = N inserts/sec. BQ Storage Write API has per-stream throughput limits; you'll need batching. ClickHouse handles concurrent inserts but the part-merge load is real.

### Architecture 2: CP writes; harness emits over gRPC only

**Harness dep surface: 5.** No new deps. Harness ships a richer `done` event payload over the existing gRPC stream.

**Coupling: 2.** This is the only score where Arch 2 takes a hit. The harness now *requires* a CP for production trace persistence. The local-CLI fallback (`stdout` JSONL) keeps working for OSS. But "I want to run the harness against my own BQ without standing up a CP" becomes "you must operate two services" — a regression for the federated multi-org use case.

**OTel duplication: 3.** OTLP and the gRPC trace path coexist. They overlap heavily for spans, but we now have a *richer* event channel that carries `Config`, `VerificationResults`, etc. Some operators will dual-source; others will turn off OTel. Net: not great, not terrible.

**Eval CLI: 2.** The eval CLI today reads `FileStore` directly. Under Arch 2, production eval queries hit the CP's read API (gRPC or REST). OSS users without a CP lose `mine-failures` against a real production lakehouse, but they still have `FileStore` for their own runs. The break: the CP's read API needs to mirror `TraceLakehouse` faithfully, which means versioning the read API alongside the wire format. That's not free.

**Security alignment (#8): 5.** Best of the three. The CP is the long-running operator; it is the natural place to do encryption-at-rest, classification routing (#8.6.2), and RBAC (#8.6.3). The harness retains the redact-at-source posture and never holds lakehouse creds.

**Schema evolution: 4.** Wire-format evolution lives in proto buf rules; lakehouse schema lives in the CP. Decouples nicely. The proto's existing Buf v2 lint/breaking config (`buf.yaml`) is the obvious gating mechanism.

**Failure mode if lakehouse down: 4.** CP buffers; harness is unaware. If the CP itself is down, the harness's gRPC stream breaks — but that was already a fatal condition (the CP delivers the task assignment). No new failure surface.

**Cost / op complexity: 3.** Adds a writer in the CP (sidecar or service). Standard ops; no new exotic moving parts.

### Architecture 3: OTel-derived lakehouse (SQL view over span store)

**Harness dep surface: 4.** OTel SDK is already in the dep tree. We commit to it harder. If we standardise on OTel, we *gain* a fully-typed metrics path for free (already wired via `observability/metrics.go`). The cost is one extra collector hop in production.

**Coupling: 4.** Harness only needs an OTLP endpoint. CP can be the collector, or operators can run their own Otel collector independent of the CP. Best decoupling of the three.

**OTel duplication: 5.** None — by definition the OTel path *is* the canonical path.

**Eval CLI: 3.** Concretely, `Metrics()` becomes a SQL query against the span store. A BigQuery view: `CREATE VIEW stirrup_runs AS SELECT trace_id AS run_id, attributes['run.outcome'] AS outcome, ... FROM otel_traces WHERE name='run' AND parent_span_id IS NULL`. This is fine for `baseline` / `drift`. For `mine-failures` we need recordings — which span stores are bad at — so we keep `RunRecording` on a side store. The eval CLI now has *two* read backends to coordinate.

**Security alignment (#8): 3.** The OTel collector model is opinionated: spans are tagged with attributes including potentially-sensitive content. Off-the-shelf collectors don't do per-row encryption. We rely on `Redact()` at emit time — same as today. RBAC over a span store is ecosystem-dependent: BigQuery dataset IAM is fine; Tempo's per-tenant model is fine; vanilla ClickHouse needs row-level policies you have to configure. Workable but not turnkey.

**Schema evolution: 3.** OTel attributes are bags, so adding fields is "free" — but consumers who rely on a column-shaped projection still need view migrations. The tradeoff: never breaks at the wire level, often breaks at the analytic-layer level.

**Failure mode if lakehouse down: 5.** OTel collectors are designed for batching, retry, and back-off. The OTel SDK's `BatchSpanProcessor` drops on overflow rather than blocking the harness — the documented behaviour [OTel SDK spec, https://opentelemetry.io/docs/specs/otel/trace/sdk/#batching-processor]. Best behaviour of the three.

**Cost / op complexity: 3.** Operators must run a collector. For OSS users with no CP, "spin up a local Tempo" is a much higher bar than "use the file emitter" — but the file emitter still ships, so OSS doesn't pay this cost.

### Recommendation: hybrid, with a precise seam

**Recommended architecture: hybrid (2 + 3).**

- **OTel for spans / metrics / aggregates** (arch 3 path). Promote the OTel emitter to the canonical telemetry pipe. Align attribute names with the GenAI semconv where feasible (`gen_ai.usage.input_tokens` etc.) to make off-the-shelf APM dashboards work. The eval CLI's `Metrics()` / `baseline` / `drift` subcommands gain a backend that talks to the span store via SQL view (BigQuery / ClickHouse). This kills duplication for the "lightweight" path.

- **CP-as-writer for recordings** (arch 2 path). The harness emits a `RecordingChunk` server-stream alongside the existing `RunTask` bidi, keyed by `run_id`. The CP collects chunks, persists `RunRecording` to object storage (encrypted-at-rest per #8.6.1), and writes a `recordings` index row to whatever read store the eval CLI uses. The harness never holds lakehouse / object-store creds.

- **`TraceLakehouse` interface stays, narrowed to two methods that matter at the eval boundary.** Drop `StoreTrace` / `StoreRecording` from the interface (they're never called outside tests). Keep `QueryTraces` / `QueryRecordings` / `Metrics` / `Close`. Add `LakehouseURL` (or similar) so the eval CLI can target either FileStore (OSS / CI) or a CP-backed read API (production).

The seam: **harness emits spans (canonical) + recording chunks (best-effort, optional). CP indexes both into the lakehouse. Eval CLI queries the lakehouse via the narrowed read interface.**

---

## 4. Ingestion contract sketch (Q4)

For the recommended hybrid architecture.

### 4.1 Wire formats

**Lightweight RunTrace (per-run aggregate):** OTLP/gRPC spans. The existing `OTelTraceEmitter` already does this. Sub-agent forwarding via `NestedJSONLEmitter` continues to work; the #89 turn[N] parenting bug is the one remaining gap for full span-tree fidelity. No new wire.

**OTel metrics:** OTLP/gRPC metrics, already wired via `observability/metrics.go`. No new wire.

**RunRecording (megabyte-scale):** new server-streaming gRPC method. Two reasonable shapes:

```proto
// Option A: stream chunks of TurnRecord over a dedicated RPC.
service HarnessService {
  // ... existing RunTask ...

  // UploadRecording streams a RunRecording in turn-sized chunks.
  // Called by the harness post-Finish, before stream close.
  // The CP responds with the lakehouse object URI (for audit).
  rpc UploadRecording(stream RecordingChunk) returns (RecordingReceipt);
}

message RecordingChunk {
  // First chunk only: the manifest.
  RecordingManifest manifest = 1;

  // Subsequent chunks: one TurnRecord per chunk.
  TurnRecord turn = 2;

  // Last chunk only: the final outcome.
  RunTrace final_outcome = 3;
}

message RecordingManifest {
  string run_id = 1;
  string parent_run_id = 2;     // empty for top-level runs; set for sub-agents
  RunConfig redacted_config = 3;
  uint32 turn_count = 4;
  uint64 sequence_token = 5;    // for idempotency; see 4.2
  string harness_version = 6;
  bytes content_sha256 = 7;     // tail-computed; CP verifies after final chunk
}

message RecordingReceipt {
  string lakehouse_uri = 1;     // e.g. gs://stirrup-recordings/run-id.json.enc
  string content_sha256 = 2;    // CP echoes back what it computed
  bool already_exists = 3;      // idempotency hit
}
```

**Option B (escape hatch for very large recordings):** the CP returns a presigned object-store URL in a control event (`upload_url`); harness uploads directly. Trades CP load for the harness needing transient HTTP egress, which conflicts with the container executor's `NetworkMode: none` posture. *Not recommended unless recordings exceed the gRPC max message size on the way through the CP.* For reference: gRPC default max message size is 4MB, easily raised; full conversation-history recordings >100MB would need this path.

I recommend **Option A as default**, with Option B as a configurable fallback for operators who run agents with very long histories. The proto can simply add a `string upload_url` to `RecordingReceipt`'s sibling control event and document the chosen path per-deployment.

### 4.2 Idempotency

Today the harness has no retry semantics for `done`. It emits once and exits. But once the recording upload is on the wire, we need to handle:

- Network partition mid-stream → harness reconnects → must not double-write the recording.
- CP crashes mid-write → harness re-issues `UploadRecording` → CP must dedupe.

The minimum viable idempotency token is `(run_id, content_sha256)` computed by the harness over the full `RunRecording` proto bytes. The CP's lakehouse should be the dedupe authority — `UploadRecording` is at-least-once, persistence is exactly-once via primary-key collision on `run_id`. The `RecordingReceipt.already_exists=true` path is the success-on-retry signal.

For OTel spans, the OTel SDK's `BatchSpanProcessor` already handles retry transparently; trace-id collisions are spec-handled.

The simplified `RunTrace` over the `done` event is an interesting case: the proto says it carries the trace, but the loop today doesn't populate it. If we **stop pretending** — drop the `RunTrace trace = 10` field from `HarnessEvent` in the next major proto version, and let the OTel + recording paths carry trace data — we eliminate one source of confusion. The "lightweight summary on done" should be reconstructable from a span query at the CP, not duplicated on the wire.

### 4.3 Backpressure

Today: `gRPCTransport.Emit` blocks on `g.stream.Send(pe)` (`grpc.go:110`). `Send` is documented as blocking when the underlying flow-control window is full. The mutex on `g.mu` further serialises emits. There is no drop / spool / metric for slow CPs.

Under the proposed hybrid:

- **OTel spans:** rely on `BatchSpanProcessor` semantics. The processor has a bounded queue (default 2048 spans); on overflow, *the new span is dropped* and a metric (`otel.sdk.processor.spans.dropped`) increments. This is the textbook behaviour and matches "the harness is a short-lived job that should not block on telemetry". Operators can raise the queue size via standard env vars.
- **RunTask bidi (existing events):** keep blocking semantics. These events drive the agentic loop — drops would corrupt state. Status quo.
- **UploadRecording:** also blocking from the harness perspective, but the call is post-`Finish` and out of the critical path. If it fails the run still terminated successfully — the harness logs a warning, exits, and the CP can re-pull from a debug-only local spool if configured. Default: drop on failure with a `recording_upload_failed` security event. Spool-to-disk is opt-in via a flag; it conflicts with the ephemeral-job posture and re-introduces local state.

In summary: **block on RunTask events (state-bearing), drop on OTel spans (best-effort observability), warn-and-drop on UploadRecording (post-run forensics).** The harness never spools by default.

### 4.4 Recording transport choice

A 1MB-ish recording over the existing `RunTask` bidi by stuffing turns into `HarnessEvent.text` or similar is structurally wrong: the gRPC server's max message size is configurable but per-message; flow-control would interleave recording chunks with live events; and the existing `done` semantics ("then the stream closes") would race with chunk delivery.

A separate `UploadRecording` server-streaming RPC keeps the abstractions clean: one stream per concern, predictable lifecycle, individual flow-control windows. For deployments where the harness can't reach the CP for the size of recording (say, a behind-NAT agent uploading 200MB of conversation history), the presigned-URL escape hatch is the documented Option B above.

### 4.5 Scrubbing seam

**Harness-side scrubbing must remain authoritative — it is defence in depth.** `LogScrubber` already runs on the gRPC `Emit` path (`grpc.go:119-127`), and `RunConfig.Redact()` already runs in both trace emitters (`jsonl.go:84-87`, `otel.go:216-220`). Three reasons to keep this:

1. The CP may be operated by a different trust boundary than the workload (multi-tenant SaaS, cross-org deployments).
2. A compromised CP that has access to redacted data must not also have access to un-redacted secrets.
3. The OTel collector path may be operator-owned, not CP-owned. Spans go to a third-party APM. We cannot delegate scrubbing to the CP for that path.

**CP-side scrubbing should run as a second pass** (bracketing, not replacement). Its purpose is operator-policy redaction — content the *operator* (not the harness) considers sensitive (customer PII regexes, internal hostnames, tenant IDs). This happens before the lakehouse write. If the CP is compromised, the harness-side scrub still kept secrets out of spans / recordings; if the harness is compromised, the CP scrub still keeps customer data out of the lakehouse. Layered defences.

For `RunRecording` specifically: harness scrubs at chunk-emission time over the same patterns it applies to `HarnessEvent`. CP applies operator policy before persisting. Encryption-at-rest (#8.6.1) happens after both passes and is a CP responsibility — the harness does not hold the encryption key, consistent with current SecretStore boundaries.

### 4.6 Sub-agent specifics

Today `core/subagent.go:110` wraps the parent emitter so child Turn / ToolCall events are forwarded to the parent's trace stream tagged with `RunID` / `ParentRunID`. The eval-side filter (`parentOnlyToolCalls`, `filestore.go:239`) untangles them when computing parent-only aggregates. This works for the JSONL emitter but is partly broken for OTel (#89: turn[N] spans don't nest under `tool.spawn_agent`).

Three options for the lakehouse:

1. **Single trace, parent-tagged:** keep the current behaviour. The CP receives one `RunTrace` for the parent (with sub-agent calls flagged via `RunID`). Sub-agents do not get separate `RunTrace` rows. *Simplest, but loses sub-agent-as-first-class-citizen analytics.*

2. **Separate child traces, parent_run_id linked:** emit a separate `RunTrace` row for each sub-agent. The lakehouse schema gets a `parent_run_id` column; queries that want "everything in this conversation" join. *Best for analytics, requires schema change.*

3. **Both:** emit both. Ingestion is more expensive; queries can pick.

**Recommendation: option 2.** It matches what OTel already wants to do (turn[N] under `tool.spawn_agent` per #89), it makes the existing `parentOnlyToolCalls` workaround unnecessary, and it makes it possible to ask "how many sub-agent calls did execution-mode runs spawn last week" — which is a strictly more useful question than "the parent run had this many turns".

The proto sketch above already accommodates this: `RecordingManifest.parent_run_id` carries the link. Span-side, the OTel emitter's own `parentCtx` should be passed down (#89 fix) so sub-agent spans nest correctly.

---

## 5. Open questions

These are the things I could not close from in-tree reading; each is a candidate follow-up issue.

- **OTel attribute naming alignment.** Should the harness rename `run.*`, `turn.*`, `tool.*` attributes to match the GenAI semconv (`gen_ai.*`, `gen_ai.operation.*`)? The semconv was Stable as of OTel ~1.30; off-the-shelf Honeycomb/Datadog/New Relic dashboards key off it. Cost: ~30 lines of attribute renaming. Risk: existing operator dashboards keyed on `run.outcome` break.

- **`HarnessEvent.trace` field deprecation.** The wire field is documented as carrying the trace on `done` but the loop never populates it (`loop.go:294-298`). Proposal: either populate it and version-pin, or remove it in proto v2 and rely on OTel + UploadRecording. Confirm with researchers C/D — schema evolution may want an opinion.

- **Transport choice for recording chunks under the container executor's `NetworkMode: none`.** The harness in container mode has no network egress; the gRPC connection to the CP is established by the host process before the container starts (the K8s job entrypoint, `cmd/job.go`). For very large recordings, does the harness escape `NetworkMode: none` cleanly, or do we need a recording-via-gRPC-only constraint?

- **`RunRecording` size budget and retention.** Real-world recordings could be 10MB-100MB+ at high turn counts. Does the v1 lakehouse cap, sample, or rotate? #8.6.1 mentions 30-day retention as a default; researcher D should own the policy detail.

- **Eval CLI compatibility under arch 3 + view layer.** If `Metrics()` becomes a SQL view, the eval CLI needs a SQL driver dep tree. Should the lakehouse expose a thin gRPC read API (CP-mediated) instead, so eval keeps its minimal-deps posture?

- **#89 OTel parent-context plumbing.** Already tracked as a follow-up; flagging here because it materially affects whether the CP can answer "what's the full trace of this run including sub-agents" via span-tree queries alone.

---

## 6. Summary

**Recommended architecture: hybrid (2 + 3).** OTel/OTLP becomes the canonical pipe for `RunTrace`-equivalent telemetry (spans + metrics). A new server-streaming `UploadRecording` gRPC method is added for full `RunRecording` payloads, with the CP as the persistence authority and an optional presigned-URL escape hatch for outsized recordings.

**Headline ingestion-contract decision: OTLP for spans + new `UploadRecording` gRPC server-stream + presigned-URL escape hatch.** Backpressure: drop spans (OTel default), block on RunTask state events, warn-and-drop on recording upload. Idempotency: `(run_id, content_sha256)` with `RecordingReceipt.already_exists` as the retry-success signal. Scrubbing: harness-side authoritative, CP-side as a second pass for operator policy. Sub-agents: separate `RunTrace` rows linked via `parent_run_id`, eliminating today's `parentOnlyToolCalls` workaround.

**Open questions count: 6.**
