# Stirrup Lakehouse Research — Researcher C: Storage Shape, Schema Evolution, Open-Table Formats

Scope: questions Q3, Q6, and Q9 of issue #104. Sister researchers cover the
industry survey (A), the architecture & ingestion contract (B), and the
security/multi-tenancy/eval CLI/migration story (D). Where my analysis touches
their lanes (e.g. tenant_id columns, control-plane-vs-harness-write), I describe
the storage implication and defer the recommendation.

In-tree references use `path:line` (e.g. `types/runtrace.go:35` is
`ToolCallSummary`). External references are cited as URLs.

---

## 1. RunTrace and RunRecording size profile

### Field-by-field byte estimate

The `RunTrace` and `RunRecording` shapes are defined in `types/runtrace.go`.
Sizes below assume JSON-on-the-wire (the form `FileStore` writes today,
`eval/lakehouse/filestore.go:142`) and are the primary input to the
"keep them together vs split" decision.

**`RunTrace` (`types/runtrace.go:22`)** — bounded, kilobyte-scale.

| Field | Type | Typical bytes | Notes |
|---|---|---|---|
| `id` | string (UUID) | ~50 | bounded |
| `config` | `RunConfig` (whole struct) | 1–5 KB | dominated by `Provider`/`ModelRouter`/`Tools` config; `Prompt` is bounded by validation, but `DynamicContext` entries are capped at 50 KB each (`types/runconfig.go` per the validator) — so `Config` could climb to tens of KB if many context entries are attached |
| `startedAt` / `completedAt` | RFC 3339 strings | ~60 | bounded |
| `turns` | int | 4 | bounded |
| `tokenUsage` | `{input,output}` | ~30 | bounded |
| `toolCalls[]` | `[]ToolCallSummary` | N × ~150 B | each summary is ~120–200 B (name + ints + optional RunID/ParentRunID); for a 20-turn run with ≤5 tool calls/turn = ≤100 entries, ~15 KB worst case |
| `verificationResults[]` | feedback strings, may include eval-judge text | typically <2 KB; up to ~20 KB if `Feedback` is verbose | bounded by judge implementation — `Details map[string]any` is unbounded in principle |
| `outcome` | enum string | ~10 | bounded |

**RunTrace p50: ~5–10 KB. p95: ~30–50 KB.** The "kilobytes" claim from
the issue stands. The unbounded escape hatches are
`VerificationResult.Details` (`types/runtrace.go:56`), the entire
`Config` (specifically `RunConfig.DynamicContext` 50 KB × N entries),
and any future fields that escape the `omitempty` discipline.

**`RunRecording` (`types/runtrace.go:123`)** — unbounded, megabyte-scale.

The recording is a *strict superset* of the trace — it embeds
`FinalOutcome RunTrace` (line 127) plus the full per-turn
`ModelInput`/`ModelOutput` and `ToolCallRecord` payloads.

| Field | Per-turn bytes | Notes |
|---|---|---|
| `runId` / `Config` / `FinalOutcome` | ~10–50 KB total | matches RunTrace |
| `turns[].modelInput.messages` | dominant | each turn carries the *entire* conversation up to that point; messages stack quadratically |
| `turns[].modelInput.tools` | ~5–20 KB per turn | tool definitions repeated per turn (every tool's `inputSchema` is `json.RawMessage`, `types/messages.go:30`) |
| `turns[].modelInput.model` | ~30 B | bounded |
| `turns[].modelOutput[]` | ~1–50 KB | assistant text + tool_use blocks |
| `turns[].toolCalls[].input` | `json.RawMessage` | a tool input — bounded only by JSON-Schema validation in `tool/`; typically <2 KB |
| `turns[].toolCalls[].output` | **string, no documented cap on the recording side** | the executor caps tool stdout at 1 MB (`harness/internal/executor/local.go:19` `maxOutputSize`), and `read_file` is bounded by 10 MB (`local.go:18`) but those caps are per call. A single `read_file` of a 10 MB file pushes a single turn over the BigQuery row limit on its own. |

**Quadratic growth is the dominant term.** The conversation is
re-serialised into `ModelInput.Messages` for every turn, so the
recording carries `turns × messages-up-to-this-turn` payload. For a
20-turn run with 5 KB/turn of new content, the cumulative payload is
20 × (1+2+...+20) × 5 KB ≈ 200 KB just for `Messages`. Add a single
`read_file` that returns 5 MB and the recording crosses 5 MB even
before output blocks.

**Estimated recording sizes:** p50: 200 KB – 1 MB. p95: 5–20 MB. p99:
20–80 MB (driven by full-file reads, large search outputs, or
multimodal payloads under #103). Multimodal payloads — even a single
1 MP PNG base64-encoded into a content block, ~1.4 MB — are an
additional cliff.

### The cliff for column-store rows

- **BigQuery streaming-insert row size limit: 10 MB**; legacy streaming
  inserts limit was 5 MB. Storage Write API sustains 10 MB per row.
  *[Based on general domain knowledge and BigQuery quota docs; verify
  before pinning any DDL]* — owners of #105 should confirm against the
  current quota page, which has moved hosts (`docs.cloud.google.com`)
  in the time the BigQuery docs were last updated.
- **ClickHouse MergeTree:** no documented hard row size limit, but
  practical behaviour degrades sharply once individual rows exceed a
  few MB (mark/granule sizing, compression block alignment, and
  `max_compress_block_size` interactions). Operators commonly cap
  large fields at 1–4 MB and store overflow in S3.

**Conclusion:** ~95% of recordings (p95 ≤ 20 MB by my estimate above)
fit fine for *file* storage but already exceed the BigQuery 10 MB row
limit and stress ClickHouse MergeTree. Multimodal (#103) makes this
strictly worse: a single image attachment moves a "typical" recording
above the column-store threshold. **Inlining the recording into a
column store is wrong by default; externalising the recording to
object storage and keeping a pointer in the row is the safe shape.**

`RunTrace`, by contrast, is comfortably under 100 KB for the entire
p99 and is a natural columnar payload.

---

## 2. Q3 — Recording vs trace separation

Three live patterns in the issue:

- **(a)** Both in column store, recording in a JSON-typed column.
- **(b)** Trace in column store, recording blob in object store, URL in trace row.
- **(c)** Both in column store, recording column aggressively partitioned and TTL'd.

### (a) Both in column store, recording as JSON column

**Pros**
- Single store, single backup, single auth boundary.
- Joins are trivial (`SELECT trace, recording FROM runs WHERE id = ?`).
- BigQuery `JSON` type encodes fields individually so projections like
  `recording.turns[5].toolCalls[0].name` are sargable
  (`https://cloud.google.com/bigquery/docs/json-data`). ClickHouse JSON
  type achieved production status in 25.3 (March 2025) and similarly
  decomposes paths into sub-columns.

**Cons**
- The 10 MB BigQuery row limit hits hard — already failing for ~5% of
  runs today, ~15-25% under multimodal #103.
- ClickHouse MergeTree degrades on 5+ MB rows.
- Storage cost: column-store storage is 5–10× more expensive than
  GCS/S3 standard for cold data; recordings are written-once,
  read-rarely, so the economics are hostile.
- Backup/restore drags every recording through BQ export → GCS, which
  defeats the purpose of using a managed store.
- GDPR-style deletion-for-compliance on a single recording rewrites
  the whole row, which is fine in BigQuery (DML support) but uncomfortable
  in ClickHouse MergeTree (mutations are async heavyweight).

### (b) Trace in column store, recording blob in object store

**Pros**
- Right tier per workload: structured analytical queries against the
  trace; cheap object storage for the recording payload.
- Recordings can use content-addressed naming (`gs://stirrup-recordings/<sha256>.json.zst`)
  for natural dedupe and immutability.
- Object storage gives free-tier-class lifecycle policies: GCS lifecycle
  rules and S3 lifecycle/Glacier tiering let you TTL recordings to
  cold-storage at 30 days, delete at 365 days, with no harness changes.
- Compliance deletion is a single object delete + tombstone in trace row.
- Multi-region replication is well-understood for both blobs (GCS
  multi-region, S3 cross-region replication) and column store.

**Cons**
- Two stores to operate, two auth domains, two failure modes. A
  partial write (trace row written, recording blob upload failed) can
  leave a dangling pointer; idempotency keys + a "retry from trace"
  workflow are needed (B's lane).
- Joins now span a SQL query plus a blob fetch — the `eval mine-failures`
  workflow has to either pre-fetch many blobs or accept latency.
- The blob is opaque to the SQL engine: you can't `SELECT
  recording.turns[*].toolCalls[*].name` against object storage. If a
  future eval workflow wants to mine over recordings *as data* (e.g.
  "find every run where tool X returned >100 KB"), you'd reach for
  Iceberg/Parquet (see Q9) or build a separate columnar index of the
  fields you care about.

**Illustrative DDL — BigQuery (concrete schemas are #105's lane)**:

```sql
-- Hot: structured trace row
CREATE TABLE runs (
  run_id          STRING NOT NULL,
  tenant_id       STRING NOT NULL,           -- D's lane
  parent_run_id   STRING,                    -- nested under parent for sub-agents
  started_at      TIMESTAMP NOT NULL,
  completed_at    TIMESTAMP,
  outcome         STRING,
  mode            STRING,
  model           STRING,
  turns           INT64,
  tokens_input    INT64,
  tokens_output   INT64,
  tool_calls      JSON,                       -- []ToolCallSummary, JSON-typed
  config          JSON,                       -- whole RunConfig, JSON-typed
  recording_uri   STRING,                     -- gs://stirrup-recordings/...
  recording_size  INT64,
  schema_version  INT64
)
PARTITION BY DATE(started_at)
CLUSTER BY tenant_id, mode, model;
```

```sql
-- ClickHouse equivalent, trace-only
CREATE TABLE runs (
  run_id String, tenant_id String, parent_run_id String,
  started_at DateTime64(3), completed_at DateTime64(3),
  outcome LowCardinality(String), mode LowCardinality(String),
  model LowCardinality(String),
  turns UInt32, tokens_input UInt32, tokens_output UInt32,
  tool_calls JSON, config JSON, recording_uri String,
  recording_size UInt64, schema_version UInt32
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(started_at)
ORDER BY (tenant_id, started_at, run_id)
TTL started_at + INTERVAL 2 YEAR;
```

### (c) Both in column store, partitioned + TTL'd recording column

**Pros**
- Same single-store auth/backup story as (a).
- Column-level TTL (BigQuery `OPTIONS(partition_expiration_days=30)` on
  a separate recording-partition table; ClickHouse `TTL recording TO DISK 'cold' / DELETE`)
  gets you automatic cold-data eviction without a second store.

**Cons**
- Doesn't solve the row-size-cliff. The BigQuery 10 MB row limit
  applies regardless of whether you TTL the column.
- ClickHouse `TTL ... DELETE` operates per-part, so deletion is
  asynchronous and operators can't promise "recording gone within X
  hours" under load — uncomfortable for compliance.
- Effectively hybridises (a) and (b) without committing — you still
  pay column-store $/GB for hot recordings, and you still need a
  separate cold tier; no clean win over (b).

### Migration story between patterns

(b) is the most *general* shape — every other pattern reduces to it
under a config change.

- **(a) → (b):** add a `recording_uri` column; for new runs, write to
  GCS first and fill the URI; for existing rows, keep the JSON column
  populated. Run a backfill job that uploads every existing recording
  blob to GCS, sets `recording_uri`, and (after a soak) sets the JSON
  column to `NULL` and drops it in a follow-up DDL. **No-downtime
  migration is feasible** because writes go to both, reads have a
  COALESCE shim.
- **(c) → (b):** trivial; (c) is (b) without a separate object
  store. Move the column to GCS by the same backfill.
- **(b) → (a):** counterproductive at any scale > a few MB/run, so
  not a real migration concern.

The migration cost is real (a backfill of 1M recordings × 5 MB
average = 5 TB of data movement) but it's a one-time cost and it
doesn't gate ingestion of new data. **Locking in (b) from day one
avoids this entirely.**

### Sub-agent recordings: parent-child shape

`#55` introduced forwarded sub-agent calls into `RunTrace.ToolCalls`
(`types/runtrace.go:17-21`). The forwarding model implies sub-agents
have their own `RunID` (`runtrace.go:46`) and a `ParentRunID` pointer.
For storage, two options:

1. **Nested under parent (single trace row).** Sub-agent calls already
   appear in the parent's `RunTrace.ToolCalls` as forwarded entries.
   Pros: one row per "user-visible run"; simple ergonomics for the
   eval CLI. Cons: parent rows grow unbounded under deep sub-agent
   trees; `parentOnlyToolCalls` (`eval/lakehouse/filestore.go:239`) is
   already needed to avoid double-counting and that cost compounds at
   scale.
2. **Separate rows per agent (FK on `parent_run_id`).** Each sub-agent
   gets its own row in the same `runs` table. Pros: bounded row size,
   cleaner aggregation (`SUM(turns) WHERE parent_run_id = ?`),
   recursive CTE / `COUNT(*)` queries are natural. Cons: every read
   that wants "the parent run plus its sub-agents" needs a join.

**Recommend (2): separate rows, with `parent_run_id` foreign key.**
The `ParentRunID` field is already on `ToolCallSummary` and `TurnTrace`
(`types/runtrace.go:46-49, 70-73`). Promoting it to a top-level
`runs.parent_run_id` column gives us:

- Sub-agent recordings are independently sized and TTL'd.
- The "tree of runs" is one self-join (`WITH RECURSIVE` in BigQuery, `arrayJoin` in ClickHouse).
- Forward-compatible with future "spawn many sub-agents" patterns.

The parent row's `tool_calls` still includes forwarded sub-agent
entries today — that's a wire-shape contract from #55 that callers
depend on; we don't break it. The sub-agent's *own* row is the
authoritative source. Make `parentOnlyToolCalls` a default view on the
`runs` table so consumers can't accidentally double-count.

### Default recommendation

**Pattern (b): structured trace in column store, recording blob in
GCS/S3, URL stored in trace row.**

- Trace row: ~10 KB, hot, partitioned by `DATE(started_at)`,
  clustered by `tenant_id, mode, model`. TTL: **2 years**, matching
  typical SaaS retention defaults; D will tune for compliance.
- Recording blob: content-addressed
  `gs://stirrup-recordings/<sha256>.json.zst` (zstd is ~5× ratio on
  trace-shaped JSON; commodity in both Go and the eval CLI). TTL via
  bucket lifecycle: **90 days hot, 180 days cold, delete at 365 days**
  unless an experiment tags it for retention. Sister #105/#106
  finalise the numbers.
- Sub-agent recordings: separate rows, `parent_run_id` FK; each row
  carries its own `recording_uri`; per-row delete handles tree-wide
  deletion in O(rows).
- `recording_uri` is **nullable**: small recordings (<256 KB) can stay
  inline in a `recording_inline JSON` column for development,
  cheap-to-query workflows, and CI fixtures. Use a single
  `recording_uri OR recording_inline` rule, never both.

---

## 3. Q6 — Schema evolution policy

`ToolCallSummary` (`types/runtrace.go:35`) is a working case study.
`RunID` and `ParentRunID` were added in #55 with `omitempty` JSON
tags and explicit comments at lines 42-49 explaining "absent on
parent-emitted events to preserve the existing wire shape." That is
the canonical *additive* change — same wire shape for old consumers,
new readers see the new field, no migration. The repo also already
uses `*bool` pointer fields (`RuleOfTwo.Enforce`, `SensitiveData`,
`Think`) so "unset" is wire-distinguishable from `false`
(`types/runconfig.go:108-112, 168-170`). This is "wire is the
contract" thinking, applied per-field.

The harder cases coming:

- **Multimodal (#103):** turn payloads will carry image/audio
  attachments. `ContentBlock` (`types/messages.go:15`) currently has
  text-shaped fields only — adding `ImageURL string` and `MediaType
  string` is additive and safe; carrying inline base64 bytes in a
  string field (a common vendor convention) breaks size assumptions
  downstream and pushes recording sizes past column-store row limits
  (re-derives the (b) decision in §2).
- **Sub-agent budgets:** likely shows up as `MaxTurns`, `TokenUsage`
  pointers on the `ToolCallSummary` for `spawn_agent` calls, or a
  parallel `SubAgentBudget` field on `RunTrace`. Both are additive.
- **More eval signals:** a `VerificationResult.Score float64` or a
  `qualityFlags []string` is additive.
- **Semantic drift:** `ToolCallSummary.OutputSize` is described as
  bytes today. If a future emitter measures it in characters (UTF-8
  rune count), the field name is unchanged, the wire shape is
  unchanged, but consumers silently get the wrong number. *This is
  the dangerous class of change that omitempty does not catch.*

### Four patterns

**(i) Wire-is-the-contract: protobuf-style additive evolution.**

How: every new field is optional with a sensible zero, parsers
tolerate unknown fields, removals/renames are forbidden. Go's JSON
unmarshaller already does the right thing for unknown fields; gRPC
proto3 default-zero semantics align. Buf categorises breaking changes
across `WIRE`, `WIRE_JSON`, `FILE`, `PACKAGE` — `WIRE_JSON` is the
recommended minimum and forbids renaming JSON field names, deleting
fields without reserving the number, and changing field types without
declared compatibility (`https://buf.build/docs/breaking/rules`).

When it works: 95% of cases, including all of `#55`, `#103`-flavour
additions, sub-agent budgets, and additional eval signals.

When it falls over: semantic drift (the `OutputSize` example);
deprecating a field without breaking anyone (you can mark deprecated
but readers that special-case it still work); type changes (string
→ structured object).

**(ii) JSON-typed columns + projection views.**

How: store the whole row as JSON; expose typed views over hot fields.
BigQuery `JSON` type encodes fields individually for direct field
access; ClickHouse JSON 25.3 production decomposes paths into
sub-columns. Projection views (`CREATE VIEW v AS SELECT JSON_VALUE(...)
AS run_id, ...`) give a stable typed surface even as the JSON
underneath changes.

When it works: rapid iteration; the trace shape isn't yet stable;
multiple consumers want different projections of the same data.

When it falls over: query cost (full-row scan vs columnar scan if
the projection view isn't materialised); type checking (typo in a
JSON path silently returns NULL); and when the JSON columns themselves
need TTL/partition/cluster pruning (you can't cluster by a JSON sub-field).

**(iii) Versioned tables (`runs_v1`, `runs_v2`) with read-time UNION.**

How: cut a new table when an incompatible change ships. Keep the old
one read-only. Read paths use a coalescing view
(`SELECT ... FROM runs_v1 UNION ALL SELECT ... FROM runs_v2`).
Cutover at a known timestamp.

When it works: rare, intentional, semantic breaks that no `omitempty`
discipline can cover. The example: if `outcome` enum values change
meaning ("success" used to include `cancelled`; now it doesn't), a
versioned cutover is the only honest answer.

When it falls over: routinely. Two tables doubles operational
overhead; queries get slower; the dual-write window is a known source
of bugs; consumers have to opt into the union or remember which table
they want.

**(iv) Schema registry.**

How: a Confluent / Buf BSR-style registry holds a typed schema per
version. Producers declare which version they emit; consumers declare
which versions they accept. Compatibility checks (`BACKWARD`,
`FORWARD`, `FULL`, `NONE` —
`https://docs.confluent.io/platform/current/schema-registry/fundamentals/schema-evolution.html`)
are enforced at registration time.

When it works: multi-team, multi-org producers/consumers; long-lived
streaming pipelines; you genuinely have producers in different
release trains and need explicit compat negotiation.

When it falls over: small teams (overhead exceeds benefit); when
producer and consumer ship in lockstep (what stirrup is today, with
the harness writing what its own eval CLI consumes); when the schema
is dataset-owned anyway (BigQuery `INFORMATION_SCHEMA.COLUMNS`,
ClickHouse `system.columns`) — you already have a schema registry
implicit in the engine.

### Recommendation

**Default: pattern (i) — additive-with-omitempty, plus a
`schema_version INT64` column on the row.**

- Every new field gets `,omitempty` (Go) and `optional` (proto).
- The `schema_version` is bumped on any change a *consumer might
  notice*. Bumps for additions are noted in changelog only; bumps for
  semantic changes (the `OutputSize` example) require explicit eval-CLI
  handling.
- This is what the repo is already doing — formalise it.

**Layer (ii) on top for hot field projection.**

- Define typed projection views in BigQuery and materialised views in
  ClickHouse for the hot fields the eval CLI queries today: `run_id`,
  `tenant_id`, `started_at`, `outcome`, `mode`, `model`, `turns`,
  `tokens_input`, `tokens_output`, `parent_run_id`. The
  `eval/lakehouse/filestore.go:172-175` filter already projects
  `Config.Mode` and `Config.ModelRouter.Model` — those are the
  authoritative "hot" set.
- Keep `tool_calls` and `config` as JSON columns. Field-by-field
  promotion to a typed column is then a reversible follow-up rather
  than a breaking change.

**Reserved for major breaks: pattern (iii) — versioned tables.**

- Default operating posture: never use it. Cost: a one-quarter dual-write
  window plus a UNION view.
- Trigger: a semantic change (outcome enum redefinition, field-meaning
  change, removal) that cannot be expressed as additive evolution.
- Before triggering: add the new field alongside the old (additive
  change), let consumers migrate, *then* remove the old field via a
  versioned cutover only if removal is genuinely needed. Most cases
  resolve without ever cutting v2.

**Skip pattern (iv) for now.** Schema registry overhead exceeds
benefit while harness and eval CLI ship in lockstep. Re-evaluate when
external consumers (a partner tool, a data-warehouse ingest job at a
customer) start consuming the lakehouse and we have actual cross-team
coordination cost. Buf BSR is already in our toolchain (`buf.yaml`,
`buf.gen.yaml`) for proto evolution checks — keep using it for the
gRPC wire contract; that's its lane.

**Concrete enforcement:**

- CI gate: `buf breaking` on the proto `RunTrace` shape; manual review on
  the JSON `RunTrace` shape (linter rules forbidding `,omitempty`-loss
  changes are doable as a `golangci-lint` custom rule but probably not
  worth building yet).
- `schema_version` is set centrally — single constant in `types/version`.
- Multimodal (#103) follows the additive path: new optional fields
  on `ContentBlock`, new optional fields on `RunRecording.Turns`. A
  size cliff *implication* hits the recording-blob discussion (§2)
  but not the schema (the recording is already JSON in object storage).

---

## 4. Q9 — Open-table formats

The candidates: Apache Iceberg, Delta Lake, Apache Hudi.

### What problem does the format solve?

All three add ACID-on-objects on top of Parquet/ORC files. Concretely:

- **Snapshot isolation** — each write produces a new manifest;
  readers see a consistent point-in-time view.
- **Schema evolution** — Iceberg tracks columns by ID, not name, so
  rename is non-destructive; type promotions follow rules
  (`https://iceberg.apache.org/docs/latest/evolution/`); add/drop are
  metadata-only.
- **Partition evolution** — Iceberg supports re-partitioning a live
  table without rewrites.
- **Time travel** — query at snapshot N or timestamp T; useful for
  reproducing a past eval run.
- **Branch / tag semantics** — Iceberg `WAP` (write-audit-publish)
  workflows let you stage changes on a branch.
- **Engine portability** — same files readable by Spark, Trino,
  Flink, BigQuery (BigLake / managed Iceberg), ClickHouse, DuckDB,
  Snowflake (read-only via external tables).

Engine-native tables give you the first two (snapshot, schema
evolution) at the cost of being engine-locked.

### BigLake's Iceberg story

BigQuery offers two distinct things, easily confused:

- **BigLake Iceberg tables (read-only).** External tables that point
  at Iceberg metadata in GCS. BigQuery reads, external engines own
  writes (Spark, Flink, Iceberg-aware loaders).
- **BigQuery managed tables for Apache Iceberg (read-write).**
  Announced 2024, GA 2024–2025. Native BigQuery DML + the Storage
  Write API write Iceberg-format data (Parquet + Iceberg manifests)
  to GCS. BigQuery handles file optimization (compaction), metadata,
  and clustering. Source:
  `https://cloud.google.com/blog/products/data-analytics/announcing-bigquery-tables-for-apache-iceberg`.

The cost of writing Iceberg from BigQuery (managed):

- You pay BigQuery storage and ingest costs *and* the GCS egress when
  external engines read. Net: 1.2–1.5× the cost of BigQuery-native
  storage at typical scale [*based on general knowledge of BQ
  pricing; verify with #105*].
- Some BigQuery features land later for managed Iceberg than for
  native — e.g. row-level security, fine-grained access — historically
  trail by 6–12 months.
- The read-side path: any Iceberg-aware engine can read the GCS
  files (Trino, Spark, DuckDB with the Iceberg extension), so engine
  portability is genuine.

### ClickHouse's Iceberg readers

`https://clickhouse.com/docs/en/engines/table-engines/integrations/iceberg`
states (as of the page contents above): **read-only**, supports S3,
Azure, HDFS, local. Notably **not GCS** at the engine level — for our
"BigQuery + ClickHouse" stack that's a real gap, since the Iceberg
files would naturally land on GCS if BigQuery is the writer. ClickHouse
Cloud has a separate object-storage abstraction; check for a GCS path
before banking on it. Read features: schema evolution, time travel,
position/equality deletes.

For the assumed scale (~10M traces/yr; ~30M sub-agent rows/yr;
recording blobs separate so ~10M trace rows on the lakehouse — this
is a back-of-envelope assumption from issue framing, not measured),
neither engine struggles to scan Iceberg vs native; the gap shows up
in *write* throughput, where ClickHouse can't write Iceberg at all
and BigQuery's managed Iceberg trails BigQuery-native by 1.5–3× at
high cardinality (anecdotal industry reports; verify with a benchmark
in #105/#106).

### Case for stirrup adopting Iceberg

- We commit to two engines (BigQuery, ClickHouse Cloud — issue #15
  context). Iceberg is the only credible portable format for both.
  If we're hedging engine choice, Iceberg is *the* hedge.
- Iceberg time travel + branch/tag give us free experiment
  reproducibility: "evaluate the eval CLI's `compare-to-production`
  against the production lakehouse at snapshot 0xabc" is one query
  with Iceberg, multiple snapshots and a coalescing view without it.
- Schema evolution rules are stricter than engine-native (column IDs
  immutable, types restricted to known-safe promotions) — this is a
  *feature*, not a cost, given the §3 concerns about silent semantic
  drift.

### Case against

- Adds a metastore/catalog dependency. Iceberg needs a catalog (REST
  catalog, AWS Glue, BigQuery's BigLake Metastore, Nessie, Hive). One
  more thing to operate, secure, back up.
- Both BigQuery managed Iceberg and ClickHouse Iceberg support are
  *behind* their respective native paths in feature richness as of
  2026. If we hit a feature gap (e.g. ClickHouse not yet supporting
  GCS Iceberg, or BigQuery managed Iceberg lagging on row-level
  security), we're stuck.
- For our access patterns — point lookups by `run_id`, range scans by
  `started_at` and `tenant_id`, aggregation by `mode`/`model` —
  engine-native MergeTree and BigQuery native tables both perform
  better than Iceberg because they own the file layout.
- We don't expect to swap engines often. The common reasons people
  swap (cost arbitrage, vendor lock-in panic) take years to play out;
  meanwhile every quarter without Iceberg is one quarter less
  operational complexity.

### Recommendation

**Defer Iceberg adoption for hot trace tables. Adopt Parquet (not
Iceberg) for recording blobs in the next storage iteration.**

Concretely:
- **Hot traces:** engine-native (BigQuery native, ClickHouse
  MergeTree). Standard column-store table per §2 pattern (b).
  Optimise for our access patterns; accept engine lock-in for now;
  re-evaluate annually.
- **Recording blobs:** today JSON+zstd. **Switch to Parquet sidecars**
  in the same storage iteration that splits trace from recording —
  Parquet alone (no Iceberg) gets you columnar compression, schema-on-write,
  and DuckDB/Spark can query GCS Parquet directly. This is the
  forcing function for "I want to mine recordings as data" without
  paying the Iceberg-catalog cost. If we later need ACID on the
  recording corpus (rewrites, partition evolution), promoting to
  Iceberg is a metadata-only migration on Parquet files.
- **Falsifiable trigger to revisit:** if (1) we need to swap an
  engine in the next 18 months, or (2) we want SQL-time-travel over
  the trace corpus for compliance/audit, or (3) a third engine
  (Snowflake, Databricks) becomes a real customer requirement. Until
  any of those fires, Iceberg is solving a problem we don't yet have.

---

## 5. Open questions

- **BigQuery row size pinning:** the 10 MB number above is widely
  cited but the docs page is mid-migration to a new host. #105 should
  pin the current limit and confirm the Storage Write API matches.
- **ClickHouse GCS Iceberg:** ClickHouse Cloud may have GCS
  abstractions that cover Iceberg-on-GCS even though the OSS engine
  docs only mention S3/Azure/HDFS. #106 should confirm.
- **Recording compression ratio measurement:** the ~5× zstd ratio on
  trace JSON is anecdotal. Run a measurement on a representative
  corpus before pinning the recording-storage cost model.
- **`VerificationResult.Details`** is `map[string]any` (`runtrace.go:56`)
  and unbounded. Should the schema cap it explicitly, or accept it as
  a variable-shape escape hatch and rely on per-judge convention?
- **Schema versioning placement:** I propose a `schema_version` row
  column. Should the proto-side `RunTrace` (`harness.proto:533`,
  7-field shape) carry it too, or is that decoupling intentional? The
  two-shape situation today is *defensible* (the proto is a "what
  the control plane sees on the done event" subset; the Go shape is
  the "what's persisted" superset) but that contract isn't documented.
- **Sub-agent recording dedup:** if a sub-agent is `spawn_agent`-ed
  multiple times with identical inputs, content-addressed blob
  storage dedupes for free. Worth measuring how often this happens
  before banking on it.
- **Parquet-vs-JSON cutover:** JSON recordings are debuggable and
  human-readable; Parquet recordings are columnar-queryable but
  opaque to `cat`. Pick one or support both; sister D's
  migration-story brief should land on this.
- **`json.RawMessage` policy:** `ToolCallRecord.Input` is
  `json.RawMessage` (`runtrace.go:116`) — opaque bytes. Generalising
  this principle ("the schema doesn't type-check tool inputs") to
  multimodal payloads would mean `ContentBlock.Image` could be
  `json.RawMessage` too, but that hides size cliffs from validation.
  Pin a position before #103 ships.

---

## Sources

- `types/runtrace.go`, `types/lakehouse.go`, `types/metrics.go`,
  `types/messages.go`, `types/runconfig.go`, `types/eval.go`
  (in-tree).
- `eval/lakehouse/filestore.go` (in-tree).
- `proto/harness/v1/harness.proto:530-556` (in-tree).
- `harness/internal/executor/local.go:18-22` (output caps).
- `VERSION1.md` "Lakehouse" / "Eval framework" sections.
- BigQuery JSON data type: `https://cloud.google.com/bigquery/docs/json-data`.
- BigQuery managed Iceberg tables: `https://cloud.google.com/blog/products/data-analytics/announcing-bigquery-tables-for-apache-iceberg`.
- ClickHouse JSON type (production-stable in 25.3):
  `https://clickhouse.com/docs/en/sql-reference/data-types/newjson`.
- ClickHouse Iceberg (read-only, S3/Azure/HDFS):
  `https://clickhouse.com/docs/en/engines/table-engines/integrations/iceberg`.
- Iceberg schema evolution overview:
  `https://iceberg.apache.org/docs/latest/evolution/`.
- Buf breaking-change rules:
  `https://buf.build/docs/breaking/rules` (categories: WIRE, WIRE_JSON, FILE, PACKAGE).
- Confluent Schema Registry compatibility modes:
  `https://docs.confluent.io/platform/current/schema-registry/fundamentals/schema-evolution.html`.
- BigQuery row size and quota numbers cited from general domain
  knowledge of BigQuery operational limits; the canonical quota docs
  page is in transit between hosts and could not be cleanly verified
  during this research — flagged as an open question for #105.
