# Stirrup #104 — Consolidated Research: Lakehouse Architecture, Ownership, and Ingestion Contract

_Synthesised from four research packets (A: industry survey; B: architecture & ingestion contract; C: storage shape & schema evolution; D: security / multi-tenancy / eval CLI / migration). Date: 2026-05-08._

---

## 1. TL;DR

**Recommended architecture: hybrid of architectures 2 and 3.** OTel/OTLP becomes the canonical wire for `RunTrace`-equivalent telemetry (spans and metrics). A new server-streaming `UploadRecording` gRPC method carries full `RunRecording` payloads post-run, with the control plane (CP) as the persistence authority. The harness never holds lakehouse or object-store credentials.

**Headline ingestion-contract decision:** OTLP for spans and metrics; new `UploadRecording` server-stream for recordings; presigned-URL escape hatch for recordings that exceed the gRPC max message size. Backpressure: drop OTel spans (SDK default), block on `RunTask` state events, warn-and-drop on recording upload.

**Storage pattern:** pattern (b) — structured `RunTrace` rows in a columnar OLAP store (BigQuery / ClickHouse), full `RunRecording` blobs in object storage (GCS / S3), with `recording_uri` pointer in the trace row. Defer Iceberg for hot trace tables; adopt Parquet (no Iceberg) for recording blobs. Sub-agent recordings are separate rows linked via `parent_run_id`.

**Multi-tenancy model:** bridge — shared engine, per-tenant dataset (BigQuery) or database (ClickHouse), per-tenant CMEK, tenant identity stamped by the CP from authenticated identity (mTLS / SPIFFE or OIDC bearer) and never asserted by the harness.

**CLI plan:** `--lakehouse local:./path` (FileStore, retained indefinitely as the OSS escape hatch) and `--lakehouse cp:https://insights.example.com` (new CP Insights API, typed gRPC). `mine-failures` routes through the #9 quarantine; raw recordings never leave the CP boundary without operator review.

**Migration stance:** there is nothing to migrate. `eval/baselines/` is empty; no production recording path exists. FileStore is declared dev-only forever; the CP path is authoritative day one of #15. Seven follow-up issues are proposed from open questions across all four packets.

---

## 2. Comparative summary of the three architectures

### Scoring table

5 = strongly aligned with stirrup constraints; 1 = strongly misaligned. Rubric from `CLAUDE.md` ("minimal-dependency", "harness is short-lived job, not a server", "control plane is the long-running operator", "API keys never enter the container") and issue #8 security items.

| Dimension | Arch 1 (harness writes direct) | Arch 2 (CP writes) | Arch 3 (OTel-derived) |
|---|:---:|:---:|:---:|
| Harness dependency surface | 1 | 5 | 4 |
| Coupling harness ↔ CP | 5 | 2 | 4 |
| OTel duplication | 1 | 3 | 5 |
| Eval CLI ergonomics | 4 | 2 | 3 |
| Security model alignment (#8) | 2 | 5 | 3 |
| Schema-evolution churn | 2 | 4 | 3 |
| Failure mode if lakehouse down | 2 | 4 | 5 |
| Ops complexity | 2 | 3 | 3 |
| **Sum** | **19** | **28** | **30** |

_Source: research-2-architecture.md, §3._

### Architecture 1 — Harness writes directly to BQ / ClickHouse

Transitively pulls massive SDK dependency trees (BigQuery Storage Write API ≈ 80 MB of generated proto; `clickhouse-go/v2` is lighter but still violates the minimal-dependency philosophy). Worst security posture: the harness needs lakehouse write credentials, violating "API keys never enter the container," and doubles the credential-federation surface. Schema migrations must land in lockstep with harness rollouts. Best coupling score (5) but scores 1 on dep surface, 1 on OTel duplication, 2 on security, and 2 on failure mode. **Not recommended.**

### Architecture 2 — CP as the sole writer

Harness gains no new dependencies; it ships a richer `done` event over the existing gRPC stream. Best security posture: CP is the natural place to do encryption-at-rest, RBAC, and PII classification. The cost is coupling: if the CP is down, the harness has no persistence path. OSS users without a CP keep `FileStore`. The eval CLI needs a CP read API for production queries. **Good for security; constrained on coupling and eval ergonomics.**

### Architecture 3 — OTel-derived lakehouse (SQL view over span store)

No additional harness dependencies; commits harder to OTel. `Metrics()`-style queries become SQL views over the span store. Cleanest failure mode (OTel `BatchSpanProcessor` drops on overflow rather than blocking). Real problem: recordings (1 MB+) have no good home in a span store; a second store is still needed. So arch 3 alone does not satisfy `mine-failures`. **Good for spans and metrics; incomplete on recordings.**

### Why pure arch 3 scores higher but the hybrid wins

Arch 3 scores 30 vs arch 2's 28 on points, but the score is misleading for recording-heavy workloads: it effectively defers the recording problem and requires a side store anyway. The hybrid combines the best of both — arch 3 for the lightweight telemetry path (no new coupling, no duplication), arch 2 for the recording path (security, RBAC, CMEK, quarantine). For full packet details, see `research-2-architecture.md`.

---

## 3. Recommended architecture with reasoning

**Hybrid (2 + 3): OTel for spans/metrics, CP-as-writer for recordings.**

### The in-tree finding that dissolves the "OTel duplication" framing

**No production code path today writes to the lakehouse.** A grep over the entire repo for `StoreTrace` / `StoreRecording` returns only the interface definition (`types/lakehouse.go`), one implementation (`eval/lakehouse/filestore.go`), and test callers. Critically:

- `loop.go:294-298` emits a `done` `HarnessEvent` but **does not set `event.Trace`** — the wire field is nil. The proto comment at `harness.proto:74` ("trace: RunTrace with execution metrics") is aspirational, not implemented.
- `RunRecording` is constructed nowhere outside tests. `grep -rn "RunRecording{" harness/` returns no results. (`research-2-architecture.md` §1; `research-4-security-cli.md` §1.1)

This means there is **no existing production pipe to displace**. The OTel emitter is already the de-facto trace path; the only question is what additional structure the CP receives alongside spans. There is no duplication problem to solve — there is only a recording-upload problem to add.

### The hybrid seam

- **OTel emitter promotes to canonical telemetry pipe.** Existing `OTelTraceEmitter` (`trace/otel.go`) continues as-is. Attribute names should align with OTel GenAI semconv where feasible (`gen_ai.usage.input_tokens`, etc.) — ~30 lines of renaming — to enable off-the-shelf APM dashboards. The eval CLI's `Metrics()` / `baseline` / `drift` subcommands target a SQL view over the span store.
- **CP-as-writer for recordings.** Post-`Finish`, the harness opens a new `UploadRecording` server-streaming RPC, sends turn-sized `RecordingChunk` messages, and receives a `RecordingReceipt` with the lakehouse object URI. The CP assembles, scrubs (second pass), encrypts-at-rest, and persists to object storage. Harness never holds object-store or CMEK credentials.
- **`TraceLakehouse` interface narrowed.** Drop `StoreTrace` / `StoreRecording` from the interface (never called outside tests). Keep `QueryTraces` / `QueryRecordings` / `Metrics` / `Close`. The `--lakehouse local:|cp:` URL scheme selects the implementation. (`research-2-architecture.md` §3)
- **Sub-agent recording.** Separate rows per sub-agent in the `runs` table, linked via `parent_run_id`. Each sub-agent emits its own `UploadRecording` call; the CP stamps `parent_run_id` from the manifest. This eliminates the `parentOnlyToolCalls` workaround (`eval/lakehouse/filestore.go:239`) and makes sub-agent analytics first-class. (`research-2-architecture.md` §4.6; `research-3-storage.md` §2)

### Why arch 2's coupling penalty is acceptable here

The coupling cost (score 2) is borne only for recordings. OTel spans are emitted to any OTLP endpoint — operators without a CP can forward to Tempo, Honeycomb, or Datadog. The CP is only mandatory for `mine-failures`-style workflows that require full recording access. OSS users who never use those workflows pay no penalty.

---

## 4. Ingestion contract sketch

### 4.1 Wire format per data type

**RunTrace (aggregate metrics) → OTLP/gRPC spans.** No new wire. The existing `OTelTraceEmitter` already emits OTLP. The OTel SDK's `BatchSpanProcessor` handles retry and back-off; overflow drops the span and increments `otel.sdk.processor.spans.dropped`. The eval CLI's `Metrics()` reads a SQL view over the span store.

**OTel metrics → OTLP/gRPC metrics.** Already wired via `observability/metrics.go`. No change.

**RunRecording (megabyte-scale) → new server-streaming gRPC method.** The proto IDL sketch (verbatim from `research-2-architecture.md` §4.1):

```proto
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

**Escape hatch for outsized recordings:** for recordings that exceed the gRPC max message size (default 4 MB, easily raised; recordings >100 MB would need this), the CP returns a presigned object-store URL in a control event (`upload_url`) and the harness uploads directly. This trades CP load for transient HTTP egress from the harness, which conflicts with `NetworkMode: none` on the container executor — document as the deployment constraint. Default is Option A (streaming through the CP gRPC connection). (`research-2-architecture.md` §4.1)

### 4.2 Idempotency

Minimum viable idempotency token: `(run_id, content_sha256)` computed by the harness over the full `RunRecording` proto bytes. The CP's lakehouse is the dedupe authority — `UploadRecording` is at-least-once; persistence is exactly-once via primary-key collision on `run_id`. `RecordingReceipt.already_exists = true` is the success-on-retry signal. OTel spans use `BatchSpanProcessor` idempotency transparently. (`research-2-architecture.md` §4.2)

### 4.3 Backpressure per data class

| Data class | Mechanism | Rationale |
|---|---|---|
| OTel spans | Drop (SDK `BatchSpanProcessor`, queue 2048 spans) | Best-effort observability; never block the loop |
| `RunTask` bidi events | Block (existing `stream.Send`) | State-bearing; drops corrupt loop state |
| `UploadRecording` | Warn-and-drop post-`Finish` | Not on the critical path; run already terminated successfully |

The harness never spools to disk by default. Spool-to-disk is an opt-in flag; it conflicts with the ephemeral-job posture. (`research-2-architecture.md` §4.3)

### 4.4 Scrubbing seam

**Harness-side scrubbing is authoritative.** Three existing call sites: `RunConfig.Redact()` in all three trace emitters (`jsonl.go:86`, `otel.go:218`, `nested_jsonl.go:132`); `security.Scrub` regex on every gRPC `Emit` (`transport/grpc.go:101-103`); `ScrubHandler` on every `slog` string attribute. These are not removed.

**CP-side scrubbing runs as a second pass** before the lakehouse write: (a) to apply operator-policy patterns that can be updated without redeploying harnesses, and (b) to cover data arriving via future paths (e.g. the blob-upload escape hatch) that bypass the gRPC scrub.

Note: CLAUDE.md/VERSION1.md state "7-pattern" LogScrubber. The codebase has **9 patterns** (`logscrubber.go:15-32`), including patterns for Azure API headers added for Azure OpenAI compatibility. AWS secret access keys (40-char base64-y strings appearing standalone in tool output) are not caught by any current pattern — a known gap. (`research-4-security-cli.md` §1.1–1.3)

### 4.5 Sub-agent fan-in story (#55)

`core/subagent.go:110-126` wraps the parent emitter in a `NestedJSONLEmitter` so child `Turn` / `ToolCall` events are forwarded to the parent trace tagged with `RunID` / `ParentRunID`. On the ingestion side, each sub-agent emits its own `UploadRecording` call; the `RecordingManifest.parent_run_id` field carries the link. The CP stamps this into a `parent_run_id` column on the `runs` table row. Parent-only metrics use a `parentOnlyToolCalls`-style view rather than an in-process workaround. The outstanding `#89` turn[N] OTel nesting bug means sub-agent spans don't yet nest correctly under `tool.spawn_agent` — fixing that is a prerequisite for full span-tree fidelity in APM tools, but does not block the ingestion contract. (`research-2-architecture.md` §4.6)

### 4.6 `HarnessEvent.trace` field

`loop.go:294-298` emits `done` without setting `event.Trace`. The proto field is aspirational. Proposal (from research-2): either populate it and version-pin, or drop it in proto v2 and rely on OTel + `UploadRecording` to carry trace data. The "lightweight summary on done" is reconstructable from a span query at the CP. Track as a schema decision follow-up.

---

## 5. Eval CLI evolution plan

### Subcommand × backend table

| Subcommand | Methods called | `local:` (FileStore) | `cp:` (CP Insights API) |
|---|---|---|---|
| `baseline` | `Metrics(filter)` | Works today | `GetMetrics(filter)` gRPC call |
| `drift` | `Metrics(filter)` × 2 | Works today | Same as baseline × 2; subtraction client-side |
| `compare-to-production` | `Metrics(filter)` | Works today | Same as baseline |
| `mine-failures` | `QueryRecordings(filter)` | Works locally | `MineFailures(filter, policy) → QuarantinedSuite` (server-side conversion) |

The entire read API reduces to two methods: `Metrics` and (for `mine-failures`) a recording access path. `QueryTraces`, `StoreTrace`, and `StoreRecording` are not called by `eval/cmd/eval/main.go` today. (`research-4-security-cli.md` §3.1)

### `--lakehouse` URL scheme

| Form | Meaning | Implementation |
|---|---|---|
| `local:./path` (or bare path for backwards compat) | FileStore | `eval/lakehouse/filestore.go` |
| `cp:https://insights.example.com` | CP Insights API | New `eval/lakehouse/cp.go`, gRPC client |
| `cp:` (no URL) | Read from `~/.config/stirrup/insights-url` | Convenience |

Unknown schemes are a hard error. Auth for `cp:`: ADC-style discovery — `STIRRUP_INSIGHTS_TOKEN` env var → `~/.config/stirrup/credentials.json` (written by `stirrup-eval auth login`, OIDC PKCE browser flow) → attached workload identity for CI. mTLS / SPIFFE is the service-to-service path (harness ↔ CP); it is not the human-CLI path. (`research-4-security-cli.md` §3.4–3.5)

### FileStore retained indefinitely

FileStore stays as the OSS escape hatch. It supports CI eval gates without external dependencies, OSS contributors without a CP, and replay tooling (`eval/runner/replay.go:18`). It is capped at the current `RunTrace`/`RunRecording` schema and carries no backwards-compatibility burden on the CP path. No deprecation path planned. (`research-4-security-cli.md` §3.2)

### `mine-failures` quarantine interaction with #9

`mine-failures` against `cp:` returns `QuarantinedSuite { Suite EvalSuite; QuarantineFlags []string }`. Flags include `unscrubbed_secret_event`, `large_payload`, `pii_classification`. The runner refuses to execute a quarantined suite without `--accept-quarantine`. Committing a quarantined suite is a code-review smell; CI lint blocks it. Conversion is done server-side on the CP so raw recordings never leave the CP boundary without explicit operator review. (`research-4-security-cli.md` §3.6)

---

## 6. Schema-evolution policy

**Default: additive-with-omitempty + `schema_version` column.** Every new field gets `,omitempty` (Go) and `optional` (proto3). `schema_version INT64` on every row is bumped on any change a consumer might notice. The repo already does this for sub-agent fields (`ToolCallSummary.RunID`, `ToolCallSummary.ParentRunID` at `types/runtrace.go:42-49` with explicit `omitempty` comments). Formalise the pattern.

**Hot-field projection views.** Define typed projection views in BigQuery and materialised views in ClickHouse over the fields the eval CLI queries today: `run_id`, `tenant_id`, `started_at`, `outcome`, `mode`, `model`, `turns`, `tokens_input`, `tokens_output`, `parent_run_id` (the authoritative "hot" set from `filestore.go:172-175`). Keep `tool_calls` and `config` as JSON-typed columns; promote fields to typed columns as a reversible follow-up.

**Reserved for major breaks only: versioned tables (`runs_v1`, `runs_v2`).** Trigger only for semantic changes that cannot be expressed additively (outcome enum redefinition, field-meaning change). Before triggering: add the new field alongside the old, let consumers migrate, then remove the old field via a versioned cutover only if removal is genuinely needed. Most cases resolve without ever cutting v2.

**Skip schema registry (pattern iv) for now.** Overhead exceeds benefit while harness and eval CLI ship in lockstep. Buf BSR (`buf.yaml`, `buf.gen.yaml`) already gates proto evolution; keep using it for the gRPC wire contract. Revisit when external consumers start consuming the lakehouse independently.

**Dangerous class of change (note):** silent semantic drift — e.g. `ToolCallSummary.OutputSize` changing from bytes to characters without a field rename. `omitempty` does not catch this. Mitigate with `schema_version` bumps, changelog entries, and CI-level tests that assert expected units. (`research-3-storage.md` §3)

---

## 7. Recording vs trace separation

**Pattern (b): structured trace in column store; recording blob in object storage; `recording_uri` pointer in trace row.**

### Size profile

- **`RunTrace`**: p50 5–10 KB; p95 30–50 KB. Comfortable in any columnar store. Dominant unbounded escape hatches: `VerificationResult.Details` (`types/runtrace.go:56`, `map[string]any`) and `RunConfig.DynamicContext` (50 KB × N entries).
- **`RunRecording`**: p50 200 KB – 1 MB; p95 5–20 MB; p99 20–80 MB. Quadratic growth is the dominant term (conversation is re-serialised into `ModelInput.Messages` for every turn). A single `read_file` of a 10 MB file pushes one turn past the BigQuery 10 MB row limit. Multimodal (#103) worsens this: one 1 MP PNG base64-encoded ≈ 1.4 MB per content block. (`research-3-storage.md` §1)

**Inlining recordings into a column store is wrong by default at p95 and above.** Pattern (b) avoids the BigQuery 10 MB row limit cliff and ClickHouse MergeTree degradation on 5+ MB rows.

### Default TTLs

- **Trace rows:** 2-year TTL; hot data for the full period.
- **Recording blobs:** content-addressed `gs://stirrup-recordings/<sha256>.json.zst` (zstd ≈ 5× ratio on trace-shaped JSON); 90-day hot, 180-day cold via bucket lifecycle, delete at 365 days by default. Runs tagged for experiment retention are exempt. `recording_uri` is nullable: recordings under 256 KB can stay inline in a `recording_inline JSON` column for CI and dev workflows.

### Sub-agent recordings

Separate rows per sub-agent, `parent_run_id` FK on the `runs` table. Each sub-agent carries its own `recording_uri`. Per-row delete handles tree-wide deletion in O(rows). The existing `parentOnlyToolCalls` workaround (`filestore.go:239`) is replaced by a SQL view. (`research-3-storage.md` §2; `research-2-architecture.md` §4.6)

### Migration path from pattern (a) to pattern (b)

Add `recording_uri` column; write to GCS first for new runs; backfill existing rows with a job that uploads each recording blob and sets the URI; after soak, null out the JSON column and drop it in a follow-up DDL. No-downtime migration is feasible because writes go to both paths during transition. **Starting with (b) from day one avoids this entirely.** (`research-3-storage.md` §2)

---

## 8. Multi-tenancy model

**Bridge model** (shared engine, per-tenant dataset/database, per-tenant CMEK), assuming ≤ 50 tenants in year one.

| Model | RLS-bug blast radius | Per-tenant CMEK | Cross-tenant analytics | Best for |
|---|---|---|---|---|
| Pool | High — leaks all tenants | Hard | Trivial | 1000 tiny tenants, no compliance pressure |
| **Bridge** | Bounded to one tenant | Yes — per-dataset (BQ) / per-database (CH) | Cross-dataset views (operator-only) | Tens to hundreds of tenants, mixed compliance |
| Silo | Effectively impossible | Yes | Painful | Hyperscale or hard regulatory boundary |

_Source: [AWS SaaS Lens](https://docs.aws.amazon.com/wellarchitected/latest/saas-lens/tenant-isolation.html)_

### `tenant_id` placement

`tenant_id` belongs on every row in the column store — both trace rows and recording index rows. The CP stamps it from the authenticated harness identity; the harness body never asserts tenancy. Proto carries no `tenant_id` field; it lives only in the CP's server-side struct. Sub-agent calls share the parent's `tenant_id` implicitly via the forwarding model. (`research-4-security-cli.md` §2.4–2.7; `research-3-storage.md` §2 DDL already includes `tenant_id STRING NOT NULL`)

The bridge model is the easiest to upgrade: promote a regulated tenant to a silo (their own project) without touching others. Pool → bridge is much harder (data must move).

### Wire shape for tenant identity

mTLS with SPIFFE IDs (`spiffe://stirrup.cp/tenant/<id>/harness/<name>`) for service-to-service. For OSS / self-hosted harnesses connecting to a cloud CP: bearer JWT with `tenant` claim, OIDC client credentials flow. The `X-Scope-OrgID` self-asserted header pattern ([Grafana Mimir docs](https://grafana.com/docs/mimir/latest/manage/secure/authentication-and-authorization/)) is explicitly rejected — it is only safe behind a trusted reverse proxy, and the CP is itself the trust boundary. (`research-4-security-cli.md` §2.5)

---

## 9. PII / scrubbing layered model

### Three harness call sites (existing)

| Location | What it scrubs | When |
|---|---|---|
| `RunConfig.Redact()` (`types/runconfig.go:281`) | `APIKeyRef` fields on provider, executor, MCP servers | In all three trace emitters before serialising `RunTrace.Config` |
| `security.Scrub` 9-pattern regex (`logscrubber.go:15-32`) | 9 patterns including API keys, GitHub PATs, AWS access key IDs, Bearer tokens, PEM keys, secret refs, Azure API key headers | gRPC `Emit` (`transport/grpc.go:101-103`) and `slog` `ScrubHandler` |
| `SecretRedactedInOutput` event (`securityevent.go:124`) | Audit alert only | On every regex match |

**Fourth site (CP):** CP rescrubs before lakehouse write, applying operator-policy patterns and covering data arriving via the presigned-URL blob upload path.

### Classification column

Add `Classification` to `ProductionTrace` (`types/metrics.go:42`). Three values: `public` (opted-in OSS recording), `internal` (default for production runs), `restricted` (any run that triggered `SecretRedactedInOutput`, any tenant tagged `pii`, any recording over a configurable byte threshold). Engine enforcement:

- **BigQuery:** Data Catalog policy tags on `recording_payload` and `tool_call_output` columns; row-level security via `CREATE ROW ACCESS POLICY` ([BigQuery RLS docs](https://cloud.google.com/bigquery/docs/row-level-security-intro)).
- **ClickHouse:** `CREATE ROW POLICY` and column-level grants ([ClickHouse access-rights docs](https://clickhouse.com/docs/en/operations/access-rights)).

### Worked example

A run reads `~/.aws/credentials`. The output includes `AKIAIOSFODNN7EXAMPLE` (caught by pattern 5) and `wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY` (not caught — 40-char base64-y string, no regex anchor). `SecretRedactedInOutput` fires once. The CP receives the partially-scrubbed event, applies its own pack (opportunity to add a key/value heuristic), and classifies the recording as `restricted`. The `mine-failures` quarantine flags it with `unscrubbed_secret_event`; it cannot be mined into an eval suite without explicit operator review. **The unscrubbed secret access key is an existing regex coverage gap, not an architectural gap.** The architecture localises the leak and prevents it from propagating to eval suites or contributors. (`research-4-security-cli.md` §1.4)

### Gaps in `RunConfig.Redact()`

`Redact()` strips secret references but does not strip `Provider.BaseURL`, `Provider.APIKeyHeader`, `Provider.QueryParams`, or `Tools.MCPServers[i].URL`. These could leak internal infrastructure topology. Flagged as a follow-up intersecting #8; out of scope for this research. (`research-4-security-cli.md` §1.7)

---

## 10. Open-table formats

**Defer Iceberg for hot trace tables. Adopt Parquet (not Iceberg) for recording blobs in the first storage iteration.**

### Why defer Iceberg

- Iceberg requires a metastore/catalog (REST catalog, AWS Glue, BigQuery BigLake Metastore, Nessie). One more system to operate, secure, and back up.
- Both BigQuery managed Iceberg and ClickHouse Iceberg support trail their respective native paths in feature richness as of 2026 (ClickHouse Iceberg is read-only and does not support GCS as a storage back-end at the OSS engine level — a real gap for a BQ + GCS stack).
- For stirrup's access patterns — point lookups by `run_id`, range scans by `started_at` / `tenant_id`, aggregation by `mode` / `model` — engine-native MergeTree and BigQuery native tables perform better than Iceberg because they own the file layout.

### Why Parquet for recording blobs

Parquet sidecar files (no Iceberg catalog) give columnar compression, schema-on-write, and direct queryability from DuckDB/Spark without a catalog service. If ACID on the recording corpus is needed later (rewrites, partition evolution), promoting from Parquet to Iceberg is a metadata-only migration. The survey (research-1) found **no vendor in the six surveyed offers Iceberg-native recording export** — Braintrust returns Parquet from BTQL; Langfuse exports to blob without an open-table format. Stirrup using Parquet is already at or ahead of industry. ([Braintrust BTQL docs](https://www.braintrust.dev/docs/reference/btql); [BigQuery managed Iceberg](https://cloud.google.com/blog/products/data-analytics/announcing-bigquery-tables-for-apache-iceberg)) (`research-3-storage.md` §4)

### Falsifiable triggers to revisit

If (1) we need to swap a storage engine within 18 months, or (2) we need SQL time-travel over the trace corpus for compliance/audit, or (3) a third engine (Snowflake, Databricks) becomes a real customer requirement. Until then, Iceberg solves a problem we do not have.

---

## 11. Migration story

**There is nothing to migrate. The CP path is authoritative on day one of #15.**

Confirmed in-tree:
- `eval/baselines/` contains only `.gitkeep`. (`research-4-security-cli.md` §4.1)
- `grep -rn "RunRecording{" harness/` returns no results — no production recorder exists.
- `grep -rn "StoreTrace\|StoreRecording" .` returns only the interface, one implementation, and test callers.

**FileStore is declared dev-only forever.** It retains its current schema permanently; it accumulates dev recordings without backwards-compatibility burden on the CP path. CI eval gates reference `FileStore` for local runs; production eval gates use `cp:`. The eval CLI's `--lakehouse local:` scheme is the OSS escape hatch.

**Cutover criteria for the CP path to be "real":** (1) CP Insights API answers `Metrics` and `MineFailures` against a populated dataset for at least one tenant; (2) `stirrup-eval` with `cp:` scheme passes its own tests in CI; (3) the new eval-gate job runs against the CP path with a workload-identity credential. (`research-4-security-cli.md` §4.3)

If developers accumulate significant local recordings before the CP path lands, a 50-line importer (`stirrup-eval import --from local:./baselines --to cp:https://...`) can be written opportunistically — it is not a day-one requirement.

---

## 12. Industry survey condensed

**Strongest signal: four of six surveyed products converge on the same storage triple — columnar OLAP store + object storage for blobs + Postgres for transactional state.**

| Product | OLAP store | Blob store | Transactional |
|---|---|---|---|
| Langfuse v3 | ClickHouse | S3-compatible | Postgres + Redis |
| LangSmith | ClickHouse | S3 / Azure Blob / GCS | Postgres + Redis |
| Helicone | ClickHouse | MinIO / S3 | Supabase (Postgres) |
| Braintrust | Brainstore (custom, object-storage-primary) | Same tier | Postgres + Redis |

_Sources: [Langfuse self-hosting](https://langfuse.com/self-hosting); [LangSmith architectural overview](https://docs.langchain.com/langsmith/architectural-overview); [Helicone GitHub README](https://github.com/Helicone/helicone); [Braintrust Brainstore](https://www.braintrust.dev/blog/how-brainstore-works)_

Phoenix (SQLite/Postgres with full payloads as span attributes) and Honeycomb (wide-event spans) are the outliers — both work for kilobyte-scale LangChain spans and break at stirrup's megabyte-scale recordings. The four-vendor consensus validates pattern (b).

**OTel posture spectrum:** none of the six products re-emits OTel. The industry divides into OTel-only wire (Honeycomb, Phoenix), OTel co-equal with native SDK (LangSmith, Langfuse v4), and native SDK primary (Braintrust, Helicone). Stirrup's hybrid adopts the "OTel co-equal" position for spans and adds a CP-specific recording channel. The OTel GenAI semconv attributes that stirrup needs (`gen_ai.usage.input_tokens`, `gen_ai.tool.*`, `gen_ai.agent.*`) are in Development stability as of mid-2026 — all GenAI conventions remain experimental; only cross-cutting attributes like `error.type` are stable. ([OTel GenAI semconv](https://opentelemetry.io/docs/specs/semconv/gen-ai/))

**Iceberg/Delta absence:** not one of the six surveyed vendors ships Iceberg-native recording export. Braintrust returns Parquet from BTQL; Langfuse exports blobs on a schedule without an open-table format. Stirrup's intent to eventually support Iceberg puts it ahead of the industry on this dimension — but the delta between "intent" and "adopted" should not be closed prematurely.

For the full survey including product-level steal/avoid notes, see `research-1-survey.md`.

---

## 13. Open questions

The following open questions are candidates for follow-up issues. Where a question is already covered by an open issue, that is noted and no new issue is proposed.

| # | Short title | Area | Blocks #15? |
|---|---|---|---|
| OQ-1 | OTel attribute naming alignment (`run.*` → `gen_ai.*`) | observability | No, but affects APM dashboard compatibility |
| OQ-2 | `HarnessEvent.trace` field: populate or deprecate | ingestion / architecture | Should be resolved before CP recording path ships |
| OQ-3 | Recording transport under `NetworkMode: none` for large blobs | architecture | Yes — must resolve before `UploadRecording` is designed |
| OQ-4 | AWS secret access key regex coverage gap | security | No, but a live security gap — intersects #8 |
| OQ-5 | `RunConfig.Redact()` gaps (BaseURL, QueryParams, MCPServerConfig.URL) | security | No — intersects #8 |
| OQ-6 | `VerificationResult.Details` unbounded `map[string]any` | storage / schema | No |
| OQ-7 | BigQuery managed-Iceberg row limit and ClickHouse GCS Iceberg gap | storage | Depends on #105/#106 engine decisions |
| OQ-8 | Recording compression ratio measurement | storage | No |
| OQ-9 | CMEK key revocation latency for right-to-erasure compliance | security | No — track when GDPR scope confirmed |
| OQ-10 | `#89` OTel parent-context plumbing (turn[N] nesting under `tool.spawn_agent`) | observability | No, but affects span-tree fidelity; already tracked as #89 |
| OQ-11 | Tenant onboarding automation (bridge model provisioning job) | architecture / infrastructure | Yes — bridge model requires this before first external tenant |
| OQ-12 | Quarantine UX for `mine-failures`: redact-then-return vs refuse | CLI / security | Intersects #9 |

OQ-10 is already #89. OQ-12 is already a concern of #9. Both are noted and not proposed as new issues. The remaining 10 are candidates for follow-up issues — seven have been promoted to `FOLLOWUPS.md` based on whether they are specific enough to file and whether they are not superseded by existing open issues.
