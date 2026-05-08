# Research packet — Issue #104, Researcher D

**Scope:** Q5 (eval CLI evolution), Q7 (PII / scrubbing), Q8 (multi-tenancy), Q10 (migration).
**Sister researchers:** A (industry survey), B (architecture & ingestion contract), C (storage shape & schema evolution). Where this packet abuts theirs, the boundary is called out — *I describe what my area requires of theirs and let them recommend.*
**Date:** 2026-05-08.

## Executive summary

- **Scrubbing.** Today the harness scrubs in three places: trace emitters call `RunConfig.Redact()` before writing `RunTrace` ([`harness/internal/trace/jsonl.go:86`](file:///Users/rubynerd/Developer/stirrup/harness/internal/trace/jsonl.go), [`otel.go:218`](file:///Users/rubynerd/Developer/stirrup/harness/internal/trace/otel.go), [`nested_jsonl.go:132`](file:///Users/rubynerd/Developer/stirrup/harness/internal/trace/nested_jsonl.go)); the gRPC transport regex-scrubs `event.Text/Content/Message` on every outbound event ([`transport/grpc.go:101-103`](file:///Users/rubynerd/Developer/stirrup/harness/internal/transport/grpc.go)); the `slog` ScrubHandler scrubs all string log attributes. There is a structural gap: `RunRecording` (full conversation history with tool I/O) is never produced by the harness today — `grep -rn "RunRecording" harness/` returns nothing — but the type and `StoreRecording` lakehouse method exist for future use. **Recommendation: layered scrubbing — harness as the source of truth, CP as a backstop.**
- **Multi-tenancy.** Proto, `RunConfig`, `RunTrace`, and `RunRecording` carry **no `tenant_id` field today**. Adding one is unavoidable. Recommendation for early-stage scale (≤ ~50 tenants in year 1): **bridge model** — shared engine, per-tenant dataset (BigQuery) or database (ClickHouse), per-tenant CMEK / KMS keys, tenant identity carried as a gRPC auth-context attribute (not a body field) on the ingestion path. Pool model is a regression; silo is premature.
- **CLI.** The four subcommands collapse to two read methods (`Metrics`, `QueryRecordings`). Recommendation: **introduce a `Lakehouse` URL scheme — `local:./path` (FileStore, OSS escape hatch) and `cp:https://insights.example.com` (CP Insights API, typed gRPC, ADC-style auth)**. SQL passthrough is rejected: it leaks schema and bypasses RLS at the wire. `mine-failures` must route through the #9 quarantine — recommend a `QuarantinedSuite` variant that the runner refuses to execute without an `--accept-quarantine` flag.
- **Migration.** `eval/baselines/` contains only `.gitkeep`; no production lakehouse adapter ships. **There is nothing to migrate.** Recommendation: ship the new lakehouse cleanly, declare `FileStore` "dev-only forever," cap its supported schema at the current `RunTrace`/`RunRecording` shape, and let it accumulate dev recordings without backwards-compatibility burden on the CP path.

---

## 1. Q7 — Scrubbing and PII

### 1.1 Current state — where scrubbing happens

The codebase has **three scrubbing call sites** and one **deliberate gap**:

| Location | What it scrubs | When | Scope |
|---|---|---|---|
| `RunConfig.Redact()` ([`types/runconfig.go:281`](file:///Users/rubynerd/Developer/stirrup/types/runconfig.go)) | `Provider.APIKeyRef`, every `Providers[k].APIKeyRef`, `Executor.VcsBackend.APIKeyRef`, every `Tools.MCPServers[i].APIKeyRef` | Called by all three trace emitters before serialising `RunTrace.Config` | Whole-config redaction of *secret references only* — preserves diagnostic fields like `BaseURL`, `APIKeyHeader`, `QueryParams`, `CredentialConfig.RoleARN` |
| `security.Scrub` regex pack ([`harness/internal/security/logscrubber.go:15`](file:///Users/rubynerd/Developer/stirrup/harness/internal/security/logscrubber.go)) | 9 patterns (see §1.2) | gRPC transport on every `Emit` ([`transport/grpc.go:101-103`](file:///Users/rubynerd/Developer/stirrup/harness/internal/transport/grpc.go)); `ScrubHandler` on every `slog` string attribute | Wire and logs |
| `SecretRedactedInOutput` event ([`security/securityevent.go:124`](file:///Users/rubynerd/Developer/stirrup/harness/internal/security/securityevent.go)) | n/a — alert | Fires from `scrubAndReport` whenever a regex matches | Auditability of detected redactions |
| **GAP** — `RunRecording.Turns[].ToolCalls[].Input/Output` | nothing | n/a | The full conversation payload (tool inputs, tool outputs, model text) is never created, scrubbed, or persisted today. `grep -rn "RunRecording{" harness/` returns no results. The replay path constructs `TurnRecord`s for tests only. |

The gap is the dangerous piece. Once anyone wires `StoreRecording` for production traffic — which #15 / #104 explicitly contemplates — every megabyte-scale conversation, including raw user file contents, env-shaped strings, and tool outputs that may include secrets the regex pack does not catch, lands in the lakehouse unscrubbed unless we wire it deliberately.

### 1.2 The actual pattern list — CLAUDE.md is stale

CLAUDE.md and `VERSION1.md` both say "7-pattern" `LogScrubber`. The current list has **9 patterns** ([`logscrubber.go:15-32`](file:///Users/rubynerd/Developer/stirrup/harness/internal/security/logscrubber.go)):

| # | Name | Catches | Notes |
|---|---|---|---|
| 1 | `anthropic_api_key` | `sk-ant-…` | |
| 2 | `openai_api_key` | `sk-[A-Za-z0-9_-]{16,}` | Permissive — also matches some other vendor tokens with the `sk-` prefix |
| 3 | `github_pat` | `ghp_…` | |
| 4 | `github_app_token` | `ghs_…` | |
| 5 | `aws_access_key_id` | `AKIA[A-Z0-9]{16}` | Catches the **ID** only; SECRET access keys (40 chars, base64-y) are not pattern-detectable and rely on `bearer_token` |
| 6 | `bearer_token` | case-insensitive `Bearer …` | JWTs included |
| 7 | `pem_private_key` | `-----BEGIN…KEY-----` | |
| 8 | `secret_ref` | `secret://…` | |
| 9 | `api_key_header` | literal `api-key:` / `x-api-key:` / `Ocp-Apim-Subscription-Key:` lines | Added for Azure OpenAI compatibility |

**Things the regex pack does not catch:** Slack tokens (`xoxb-…`), Stripe live keys (`sk_live_…`), Google service-account JSON blobs, GCP API keys (`AIza…`), Azure storage account keys (44-char base64, no anchor), generic 32-character hex strings, and — critically — **AWS secret access keys** when they appear standalone rather than after `Bearer`. Each of these is a real risk in tool outputs (`run_command` reading a `.env` file, `read_file` against a Terraform state).

Update CLAUDE.md (out of scope for this packet).

### 1.3 Where should scrubbing run with a CP-owned writer?

Three options:

1. **Harness-only.** Wire never carries unscrubbed bytes; CP trusts the harness to do it.
2. **CP-only.** Harness sends raw events; CP scrubs immediately before write.
3. **Both (defence-in-depth).**

Tradeoffs:

- **Single source of truth.** One scrubbing implementation is easier to audit and update. Pure-CP wins this.
- **Failure mode if CP is compromised or misconfigured.** A CP whose scrubbing pipeline silently breaks (regex pack drift, dependency change) leaks every byte. Pure-harness fails closed; the worst the CP can do is fail-open against already-clean data.
- **Failure mode if harness is compromised.** A malicious harness can already exfiltrate secrets directly through the model — scrubbing is not a defence against an evil harness, only against accidental leaks.
- **OSS deployments.** Self-hosters running without a CP need scrubbing. Pure-CP excludes them.
- **Already-deployed mechanisms.** The gRPC transport already scrubs ([`transport/grpc.go:101-103`](file:///Users/rubynerd/Developer/stirrup/harness/internal/transport/grpc.go)). Removing that to centralise on the CP is a regression.

**Recommendation: layered.** Harness scrubs at the wire (we already do this); CP rescrubs immediately before write to cover (a) any new patterns added on the CP side without having to redeploy harnesses and (b) any data that arrives via paths other than the gRPC transport (e.g. a future blob-upload path for large recordings, see B's territory).

### 1.4 A worked example

A user asks the harness to "summarise the changes in `~/.aws/credentials`." The `read_file` tool reads the file. The output contains:

```
aws_access_key_id = AKIAIOSFODNN7EXAMPLE
aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

Trace this under the recommended model:

1. **Tool execution** — the file contents return as the `ToolResult.Content` string in the loop.
2. **gRPC `Emit` (tool_result event)** — `scrubAndReport` runs on `event.Content`. The first line matches `aws_access_key_id` and is redacted. The second line is **not redacted** by any current pattern — `wJalrXUtnFEMI/...` looks like base64 garbage to the regex pack. `SecretRedactedInOutput` fires once (for the AKIA pattern).
3. **CP receives the event over gRPC.** CP runs its own scrubber — same pack today, but with the freedom to add a Stripe pattern or a tightened "credentials-shaped key/value" heuristic without redeploying harnesses.
4. **CP buffers events into a `RunRecording` for the run.** Before persisting to the lakehouse, CP runs a third scrub pass over the assembled `Turns[].ToolCalls[].Output` strings as a final backstop.
5. **Persistence.** The recording lands in `recordings.payload` (a JSON-typed column or object-store blob — C's call) under a `restricted` classification (§1.5).

The unscrubbed secret access key still leaks in this scenario. That's a regex coverage gap, not an architectural one — fix by **adding patterns** and by making `mine-failures` route through quarantine (#9). The architecture has done its job: the leak is in one place, classified as restricted, and quarantine prevents it from being mined into a public eval suite.

### 1.5 Schema-level enforcement

`ProductionTrace` ([`types/metrics.go:42`](file:///Users/rubynerd/Developer/stirrup/types/metrics.go)) does *not* have a `Classification` field today, despite #8 alluding to one. Add it. Three values: `public` (e.g. an OSS contributor's eval recording explicitly opted-in), `internal` (default for any prod run), `restricted` (any recording known to have triggered `SecretRedactedInOutput`, any run touching a tenant tagged `pii`, any output longer than a configurable threshold).

Map this to engine features:

- **BigQuery.** Apply Data Catalog policy tags on the high-risk columns (`recording_payload`, `tool_call_output`) and require an IAM grant on the policy tag to read. Row-level security policies via `CREATE ROW ACCESS POLICY filter_by_tenant ON dataset.recordings GRANT TO ("group:tenant-X@…") FILTER USING (tenant_id = …)` are GA per [Google Cloud docs](https://cloud.google.com/bigquery/docs/row-level-security-intro). Combine the two: column-level for *what fields* and row-level for *which tenant's rows*. Dynamic data masking can substitute hashes for the few fields that need to be queryable but not readable.
- **ClickHouse.** `CREATE ROW POLICY` and column-level grants are first-class ([ClickHouse access-rights docs](https://clickhouse.com/docs/en/operations/access-rights)). The Cloud product layers RBAC on top; ClickHouse Cloud also supports CMK for encryption at rest on Enterprise plans.

The "high-risk" tier is the recording payload column; the "low-risk" tier is everything in `RunTrace` except `Config` (which is already `Redact`-ed — see §1.6). Splitting these tiers across two tables (or two columns with separate access) gives operators a one-knob "let analysts query metrics without unlocking conversation contents" control.

### 1.6 Encryption — at-rest beyond engine default

Engine-default encryption (Google-managed keys for BigQuery, AES-256 at-rest for ClickHouse Cloud) is sufficient for `RunTrace`. For `RunRecording` payloads, **per-tenant CMEK / customer-managed keys** are the right knob: it lets a tenant revoke their key on departure and have the lakehouse cryptographically forget them without a heavy compaction step. BigQuery supports CMEK at the dataset level — that aligns naturally with the bridge model recommendation in §2. Envelope encryption with per-tenant DEKs is overkill for a column-store target; revisit if regulated tenants demand it.

### 1.7 Gaps in `RunConfig.Redact()`

`Redact` strips only secret *references*. It does **not** strip:

- `Provider.BaseURL` — could leak internal infrastructure (e.g. `https://llm-gateway.internal.corp/openai/v1`). Low risk but not zero.
- `Provider.APIKeyHeader` — usually generic (`api-key`), but a custom value could leak vendor identity.
- `Provider.QueryParams` — typically just `api-version=preview`, but values are user-controlled.
- `Tools.MCPServers[i].URL` — same as `BaseURL`.

Recommendation: extend `Redact` to also normalise `BaseURL` to its origin (strip path) and to redact non-allowlisted query params. Out of scope to ship here; flag as follow-up.

---

## 2. Q8 — Multi-tenancy

### 2.1 Define our tenants

Three distinct tenant *populations*, each with different isolation requirements:

| Tenant population | Isolation requirement | Per-tenant volume | Cross-tenant analytics? |
|---|---|---|---|
| OSS self-hosters | None — they own their lakehouse | Single-digit runs/day → no scale concern | Never |
| Cloud product customers | Strong: legal exposure if A reads B | 1–10⁶ runs/day depending on plan | Operator-only, with audit trail |
| Internal teams running CI eval | Weak: they're all "us" | Bursty: 10–10³ runs per CI batch | Yes, freely |

The OSS case is solved by `FileStore` and the recommended `local:` URL scheme (§3). The internal case is solved by tagging with `tenant_id = "internal"` and a permissive grant. The hard case is **cloud product customers** — the rest of this section.

### 2.2 Three isolation models, scored

| Model | Setup cost / tenant | Per-tenant fixed cost | RLS-bug blast radius | Cross-tenant analytics | Per-tenant CMEK | Best for |
|---|---|---|---|---|---|---|
| **Pool** — single dataset/database, every row carries `tenant_id`, RLS filters | ≈ zero | ≈ zero | High — a single bad row policy leaks every tenant | Trivial (just don't filter) | Hard — engine-level encryption is per-table/dataset, not per-row | "1000 tiny tenants, no compliance pressure" |
| **Bridge** — shared engine, per-tenant dataset (BQ) / database (CH) | Provision-on-onboard, < 1 minute | Storage minimum only (BQ ≈ free for empty datasets; CH varies) | Bounded to the tenant-of-the-bug | Cross-dataset queries (BQ: unioned views with `_TABLE_SUFFIX`; CH: distributed table) | Yes — per-dataset CMEK on BQ, per-database key on CH | "Tens to hundreds of tenants, mixed compliance" |
| **Silo** — separate project / service per tenant | Significant — new project, IAM, billing, KMS keys | High — engine minimums × N | Effectively impossible | Painful (cross-project federation, slot/warehouse fan-out) | Yes, per-project | "Hyperscale or hard regulatory boundary" |

References: [AWS SaaS Lens — Tenant Isolation](https://docs.aws.amazon.com/wellarchitected/latest/saas-lens/tenant-isolation.html); [BigQuery RLS](https://cloud.google.com/bigquery/docs/row-level-security-intro); ClickHouse access docs above.

### 2.3 Recommendation

**Bridge model**, with these assumptions (called out, per the global instructions):

- ≤ ~50 tenants in the first 12 months.
- We are willing to write a tenant-onboarding job that provisions a dataset / database + KMS key + IAM grants. (One-time engineering cost, then ops automation.)
- Cross-tenant analytics ("does the eval gate failure rate predict the production failure rate, *across all tenants*?") is operator-only and goes through a service-account-level grant on a unioned view, not a customer-facing API.

Why not pool: the blast radius of a single bad RLS policy or a forgotten `WHERE tenant_id = …` on a query is unacceptable for the *recording* table, which contains user conversation content. Why not silo: provisioning a new BigQuery project or ClickHouse service per tenant is overkill at this scale and forecloses cheap cross-tenant analytics.

The bridge model is also the easiest to *upgrade from*: when a regulated tenant arrives, promote them to a silo (their own project) without touching the others. Pool → bridge is much harder (data has to move).

### 2.4 Schema implications

Add `tenant_id` to:

- `RunTrace` (top-level field, indexed/partitioning column)
- `RunRecording` (top-level)
- `RunConfig` *as a system-set field* — the harness should not be allowed to set it; the CP stamps it on receipt. (This avoids tenant spoofing by a misconfigured harness; see §2.6.)

Even in the bridge model, we want `tenant_id` on rows. Reasons: (a) it makes pool ↔ bridge migration cheap if we re-bridge later, (b) it lets the cross-tenant unioned view annotate rows correctly, and (c) it's a defence-in-depth marker — if a row ever ends up in the wrong dataset, the mismatch is detectable.

**Required of researcher C's storage-shape work:** any column-store schema for `RunTrace` and `RunRecording` must include a `tenant_id` STRING column with not-null and indexed/partitioned-by treatment. Engine specifics (BigQuery clustering vs. ClickHouse `ORDER BY (tenant_id, ...)`) are theirs.

### 2.5 Wire shape — proto changes

Currently the proto carries no tenant field anywhere. Two ways to add it:

- **In the body.** Add `RunConfig.tenant_id` and `RunTrace.tenant_id`. **Rejected** — a harness shouldn't be able to assert its tenancy in the body; that's spoofable.
- **In the auth context.** The CP authenticates the harness (mTLS / SPIFFE / OIDC bearer) and derives `tenant_id` from the verified identity. The harness never sets it. The CP stamps it on every record before write.

**Recommendation: auth context.** Specifically:

- mTLS with SPIFFE IDs (`spiffe://stirrup.cp/tenant/<id>/harness/<name>`). [SPIFFE](https://spiffe.io/docs/latest/spiffe-about/overview/) provides workload attestation across clouds.
- For the OSS / self-hosted case where a customer brings their own harness *into our* CP: bearer JWT with a `tenant` claim, signed by our identity provider. The harness uses an OIDC client credentials flow.
- The Mimir / Tempo / Loki convention of `X-Scope-OrgID` ([Mimir auth docs](https://grafana.com/docs/mimir/latest/manage/secure/authentication-and-authorization/)) is **not** the right pattern here — that header is *self-asserted* and only safe behind a trusted reverse proxy. The CP is itself the trust boundary, so we want a verified credential, not a header.

### 2.6 The CP's tenant-stamping invariant

The CP must, on receipt of a `done` event with embedded `RunTrace`:

1. Resolve the harness's authenticated identity → `tenant_id`.
2. Reject any `RunConfig.TenantID`, `RunTrace.TenantID`, or `RunRecording.TenantID` field set by the harness (or strip it). The body cannot assert tenancy.
3. Stamp `tenant_id` on the record before passing it to the writer.
4. Route the write to the per-tenant dataset / database.

This is the auth check researcher B's ingestion-contract sketch should formalise — the "scrubbing seam" issue #104 mentions includes this tenant-stamping seam.

### 2.7 Sub-agent runs

Sub-agent runs ([`harness/internal/core/...`](file:///Users/rubynerd/Developer/stirrup/harness/internal/core/) per #55, e.g. `tool/builtins/subagent.go`) execute in-process with a `NullTransport`. They can never cross a tenant boundary because they share the parent's process and config. The schema should reflect this: no `tenant_id` on `ToolCallSummary`/`ToolCallTrace` (#55's `ParentRunID` already chains them). The CP-stamped `tenant_id` on the parent `RunTrace` is authoritative; sub-agent telemetry inherits it implicitly via forwarding.

The constraint to encode: **the gRPC transport is the only place a `tenant_id` can appear on the wire, and it appears via authenticated identity, not a body field.** Forwarded sub-agent records ride the parent's stream and inherit the parent's stamp. Make this hard to violate by *not* putting `tenant_id` in `types.RunConfig` at all; the field belongs in a server-only struct in the CP repo.

---

## 3. Q5 — Eval CLI evolution

### 3.1 Today's surface — subcommand → method table

| Subcommand | `TraceLakehouse` methods | Fields read | Notes |
|---|---|---|---|
| `baseline` | `Metrics(ctx, filter)` ([main.go:264](file:///Users/rubynerd/Developer/stirrup/eval/cmd/eval/main.go)) | `RunTrace.Outcome, Turns, TokenUsage, StartedAt, CompletedAt, Config.Mode, Config.ModelRouter.Model` (only those used by `computeMetrics` / `matchesTraceFilter`) | Aggregate-only; never reads recordings |
| `mine-failures` | `QueryRecordings(ctx, filter)` ([main.go:312](file:///Users/rubynerd/Developer/stirrup/eval/cmd/eval/main.go)) | `RunRecording.RunID, FinalOutcome.Outcome, Config.Prompt, Config.Mode` | Reads full recordings — the only command that does |
| `drift` | `Metrics(ctx, filter)` ×2 | Same as `baseline` | |
| `compare-to-production` | `Metrics(ctx, filter)` | Same as `baseline` | |

That is the **entire** read API the CLI needs. `QueryTraces`, `StoreTrace`, `StoreRecording` are not called by `eval/cmd/eval/main.go` at all — they're interface methods reserved for ingestion (the harness side, future) and ad-hoc tools. The CP "Insights API" can be tiny.

### 3.2 The OSS-vs-customer split

`FileStore` is invaluable because:

1. CI eval gate (when populated) can run with no external dependency.
2. OSS contributors can run `stirrup-eval` against locally-recorded runs without touching the CP.
3. Replay tooling (`runner.ReplayRecording` at [`eval/runner/replay.go:18`](file:///Users/rubynerd/Developer/stirrup/eval/runner/replay.go)) needs *some* recording source.

**Keep `FileStore` as the OSS escape hatch indefinitely.** Don't deprecate it; don't try to maintain feature parity with the CP path. It's a development convenience, not a product.

### 3.3 What each subcommand becomes against a remote CP

For each, sketch three options and pick.

#### `baseline`

- **A. SQL passthrough.** `stirrup-eval baseline --connection-string bigquery://project.dataset --sql "SELECT AVG(turns)…"`. Rejected — leaks schema, bypasses RLS at the wire (RLS works only when the auth principal is the *user*; a service account with read-all defeats the model), creates a contract on the column names that we then can't evolve without breaking every CLI.
- **B. CP "Insights API".** `stirrup-eval baseline --lakehouse cp:https://insights.example.com --mode execution --after 2026-04-01`. CP exposes `GetMetrics(filter) -> TraceMetrics`. Auth flows below.
- **C. Pre-baked queries behind an auth-gated endpoint.** Same as B but less flexible.

**Pick B.** `Metrics` is already the right shape; expose it as a typed gRPC method.

#### `mine-failures`

This is the dangerous one. It reads full conversation content, including any unscrubbed user code, tool outputs, and prompts. The output gets written into an `EvalSuite` JSON file that may be checked into a repo and run on contributors' machines. **#9 quarantine is a hard prerequisite.**

- **A.** Reuse `QueryRecordings` over the CP API. Stream-paginate (recordings are large).
- **B.** A separate `MineFailures(filter, quarantine_policy) -> QuarantinedSuite` method on the CP that does the conversion server-side and returns a quarantine-tagged `EvalSuite`. The `QuarantinedSuite` carries a flag the runner inspects.

**Pick B.** Doing the conversion server-side keeps the rule "raw recordings never leave the CP boundary except for explicit operator review" enforceable. It also lets the CP apply quarantine policies (e.g. "drop any task whose prompt mentions a customer-confidential pattern" / "block tasks whose recordings tripped `SecretRedactedInOutput` more than N times") consistently.

#### `drift`

Two `Metrics` calls. Trivially the same as `baseline`. The CP method does not need a special drift API — the CLI can do the subtraction client-side as it does today.

#### `compare-to-production`

One `Metrics` call + a local `SuiteResult`. Same as `baseline`. No CP-side change.

### 3.4 Auth ergonomics — the OSS-friendly path

Three credential models for CLIs:

1. **Service account JSON.** Static file shipped to the developer. Easy to demo, terrible operationally — rotation is manual, leakage is permanent. ADC docs explicitly call this out: *"compromised service account keys can be used by a bad actor without any additional information."* ([Google ADC docs](https://docs.cloud.google.com/docs/authentication/application-default-credentials).) **Avoid.**
2. **ADC-style discovery.** Order of precedence: `STIRRUP_INSIGHTS_TOKEN` env var, then `~/.config/stirrup/credentials.json` written by `stirrup auth login` (browser flow, OIDC PKCE), then attached identity (e.g. workload identity in CI). This is what `gcloud`, `bq`, and `gh` do, and it's what new contributors expect.
3. **mTLS / SPIFFE.** Right for service-to-service (the harness ↔ CP path). Not for a human-driven CLI.

**Pick 2.** Specifically:

- New subcommand: `stirrup-eval auth login --insights-url https://insights.example.com` opens a browser, completes OIDC PKCE, writes a refresh-token-bearing credentials file to `~/.config/stirrup/credentials.json`.
- `stirrup-eval` subcommands accept `--lakehouse cp:https://insights.example.com` and resolve the bearer token from the env var or credentials file at call time.
- For CI / non-interactive: workload identity (GCP / GitHub OIDC → CP token exchange). Same pattern as the existing `WebIdentityAWSSource` in `harness/internal/credential/`.
- For OSS: `--lakehouse local:./eval/baselines` keeps working with no auth.

### 3.5 The `--lakehouse` URL scheme

Today: `--lakehouse <path>`. Tomorrow:

| Form | Meaning | Backing implementation |
|---|---|---|
| `local:./path` (or bare `./path` for backwards compat) | FileStore | `eval/lakehouse/filestore.go` |
| `cp:https://insights.example.com` | CP Insights API | New `eval/lakehouse/cp.go`, gRPC client of the CP `Insights` service |
| `cp:` (no URL) | Read from `~/.config/stirrup/insights-url` | Convenience |

This keeps the existing `--lakehouse` flag stable; the OSS flow needs no flag changes. Rejecting an unknown scheme is a hard error.

### 3.6 Quarantine model for `mine-failures`

Required interaction with #9 (eval framework security):

- Server-side: `MineFailures` returns `QuarantinedSuite { Suite EvalSuite; QuarantineFlags []string }`. Flags include `unscrubbed_secret_event` (recording tripped `SecretRedactedInOutput`), `cross_tenant_data` (impossible under §2 but checked), `large_payload` (over a configurable byte limit), `pii_classification`.
- Client-side: `stirrup-eval mine-failures --output suite.json` writes the suite *with* the quarantine envelope intact. The runner refuses to execute a quarantined suite without `--accept-quarantine` and prints the flag list. Committing a quarantined suite to a repo is a code-review smell; CI lint blocks it.

This puts the CP in charge of declaring "this content is safe to exfiltrate"; the CLI is just a courier.

---

## 4. Q10 — Migration story

### 4.1 What is actually at risk?

I checked the repo. **`eval/baselines/` contains only `.gitkeep`** (`find /Users/rubynerd/Developer/stirrup/eval/baselines -type f` returns one file). CI does not reference `FileStore` (`grep -rn "FileStore" .github/` returns nothing). `RunRecording` is **never produced by the harness** (`grep -rn "RunRecording{" harness/` is empty); the type exists for the eval replay path only. There is no live production data of any kind today.

The only possible "migration" concerns are:

1. **Local developer recordings.** A developer who has run the harness locally and pointed it at `--lakehouse ./mylakehouse` *might* have JSON files on disk. (None ship in the repo.) Their concern is "can I still query these locally?" — yes, FileStore stays.
2. **Future CI baselines.** If we land #15 / #104 *before* anyone fills `eval/baselines/`, this is moot. If baselines exist in `eval/baselines/` by the time the CP path lands, they need to be either re-recorded or imported.

### 4.2 The three options

| Path | Engineering cost | Failure mode if wrong |
|---|---|---|
| **"FileStore is dev-only; production starts fresh"** | ~ zero | Lost CI baselines for the cutover quarter (re-record from main branch) |
| **Bulk import** — write a one-shot `stirrup-eval import --from local:./baselines --to cp:https://…` | Low — the read side already loops over JSON files | Schema mismatch: if the new schema differs from the old `RunTrace`, the importer must transform. Manageable. |
| **Parallel write** — harness/CI writes to both `FileStore` and the new lakehouse for a quarter, then cuts over | Medium — requires teaching the harness about two writers, plus drift-detection between them | Over-engineered for our actual situation (no prod data) |

### 4.3 Recommendation

**Option 1 — "FileStore is dev-only forever."** Justified by:

- No production data exists today.
- `eval/baselines/` is empty; there is nothing to lose.
- `FileStore` keeps working for OSS and dev; we're not deleting it, just declaring its scope.
- The **CP path becomes authoritative on day one of #15**, with no transition window in which two stores diverge.

**Cutover criteria** (what makes the CP path "real"):

1. CP `Insights` API answers `Metrics` and `MineFailures` against a populated dataset for at least one tenant.
2. `stirrup-eval` with `cp:` scheme passes its own tests in CI.
3. A new `eval-gate` job (replacing the current TODO) runs against the CP path with a service-account credential.
4. The current `eval/baselines/.gitkeep` becomes a `eval/baselines/README.md` saying "Baselines now live in the CP lakehouse; this directory is for ad-hoc local testing only."

If at any point we discover developers have built up significant local recordings, write the importer (option 2) opportunistically — it's a 50-line tool and can sit on someone's branch unmerged until needed.

### 4.4 Schema-version-on-the-wire

This intersects researcher C's territory. My constraint on theirs: **whatever schema-evolution policy they recommend must have a `schemaVersion` field on every `RunTrace` and `RunRecording`** (or equivalent typed-column versioning), so that the FileStore can be tagged `schemaVersion: 1` permanently and the CP can move forward independently. The CLI dispatches reads based on the URL scheme (`local:` vs `cp:`), so the two stores never have to share a schema or a version space. Researcher C decides whether the CP-side version is monotonic, semver, or per-table.

---

## 5. Open questions

1. **Pattern-pack drift.** The `LogScrubber` pack is 9 patterns ([`logscrubber.go:15`](file:///Users/rubynerd/Developer/stirrup/harness/internal/security/logscrubber.go)) but CLAUDE.md / VERSION1.md still say 7. Either the docs need updating or there's a process gap (which?). Intersects #8.
2. **AWS secret access keys are not regex-detectable.** Patterns 5 (`AKIA…` ID) and 6 (`Bearer …`) catch most cases; `wJalrXUtnFEMI/…`-style 40-char base64-y secret-access-keys appearing standalone in a tool output are not caught. Add a key/value heuristic? Intersects #8 / #9.
3. **`RunConfig.Redact()` doesn't strip `BaseURL`, `APIKeyHeader`, `QueryParams`, `MCPServerConfig.URL`.** Could leak internal infrastructure topology. Out of scope here; flag for a follow-up issue. Intersects #8.
4. **`StoreRecording` is implemented in `FileStore` but the harness never calls it** ([`grep -rn "RunRecording{" harness/`](file:///Users/rubynerd/Developer/stirrup/harness/) is empty). What's the production path that produces a recording — the CP buffering events from the gRPC stream? That's researcher B's call. Required-of-B: define the recording assembly seam and where scrubbing runs in it.
5. **Quarantine UX for `mine-failures`.** Does a flagged suite get redacted automatically (lossy) or refused entirely (operator decides)? Intersects #9.
6. **Tenant-onboarding automation.** The bridge model is only cheap if dataset / DB / KMS / IAM provisioning is automated. If we never build that automation, the silo model becomes operationally simpler by attrition. Track as a CP infrastructure issue.
7. **`X-Scope-OrgID`-style header for OTel side.** Researcher B's territory but worth noting: if the OTel emitter (`harness/internal/trace/otel.go`) needs tenancy, the `OTLP-Header` / `X-Scope-OrgID` convention is the established pattern (per [Mimir auth docs](https://grafana.com/docs/mimir/latest/manage/secure/authentication-and-authorization/)). However, that pattern is *self-asserted* and only safe behind a trusted proxy — for our CP-owned collector path, prefer the verified-identity model.
8. **OSS contributor identity.** If we offer a "free tier" of the CP Insights API (e.g. for OSS evaluation), what's the quota and abuse model? Out of scope.
9. **CMEK key revocation latency.** Per-tenant CMEK on BigQuery is straightforward but key revocation takes hours to fully invalidate cached data. For a "right-to-erasure" claim with a tighter SLA, we may need explicit deletion as well. Track when GDPR comes into actual scope.
10. **`Provider.BaseURL` redaction.** Should `Redact()` normalise `https://llm.internal.corp/v1/openai` to `https://llm.internal.corp/`? Intersects #8.

---

## Sources

In-tree (file:line):

- [`types/lakehouse.go`](file:///Users/rubynerd/Developer/stirrup/types/lakehouse.go)
- [`types/runtrace.go`](file:///Users/rubynerd/Developer/stirrup/types/runtrace.go)
- [`types/metrics.go`](file:///Users/rubynerd/Developer/stirrup/types/metrics.go)
- [`types/runconfig.go:281`](file:///Users/rubynerd/Developer/stirrup/types/runconfig.go) (`Redact`)
- [`eval/lakehouse/filestore.go`](file:///Users/rubynerd/Developer/stirrup/eval/lakehouse/filestore.go)
- [`eval/cmd/eval/main.go`](file:///Users/rubynerd/Developer/stirrup/eval/cmd/eval/main.go)
- [`harness/internal/security/logscrubber.go:15`](file:///Users/rubynerd/Developer/stirrup/harness/internal/security/logscrubber.go) (9-pattern set)
- [`harness/internal/security/secretstore.go`](file:///Users/rubynerd/Developer/stirrup/harness/internal/security/secretstore.go)
- [`harness/internal/security/securityevent.go:124`](file:///Users/rubynerd/Developer/stirrup/harness/internal/security/securityevent.go) (`SecretRedactedInOutput`)
- [`harness/internal/transport/grpc.go:101-127`](file:///Users/rubynerd/Developer/stirrup/harness/internal/transport/grpc.go) (`scrubAndReport`)
- [`harness/internal/trace/jsonl.go:86`](file:///Users/rubynerd/Developer/stirrup/harness/internal/trace/jsonl.go), [`otel.go:218`](file:///Users/rubynerd/Developer/stirrup/harness/internal/trace/otel.go), [`nested_jsonl.go:132`](file:///Users/rubynerd/Developer/stirrup/harness/internal/trace/nested_jsonl.go) (`Redact()` call sites)
- [`proto/harness/v1/harness.proto`](file:///Users/rubynerd/Developer/stirrup/proto/harness/v1/harness.proto) (no tenant fields anywhere)
- `eval/baselines/.gitkeep` (only file present — nothing to migrate)

External:

- BigQuery row-level security (GA): https://cloud.google.com/bigquery/docs/row-level-security-intro
- BigQuery column-level security via Data Catalog policy tags (GA): https://cloud.google.com/bigquery/docs/column-level-security-intro
- ClickHouse access rights / row policies / column grants: https://clickhouse.com/docs/en/operations/access-rights
- AWS SaaS Lens — Tenant Isolation models (pool/silo/bridge): https://docs.aws.amazon.com/wellarchitected/latest/saas-lens/tenant-isolation.html
- Grafana Mimir auth (`X-Scope-OrgID`): https://grafana.com/docs/mimir/latest/manage/secure/authentication-and-authorization/
- Google Application Default Credentials (ADC precedence): https://docs.cloud.google.com/docs/authentication/application-default-credentials
- SPIFFE / SPIRE workload identity: https://spiffe.io/docs/latest/spiffe-about/overview/
- VERSION1.md (in-repo, security and lakehouse sections — note: the "7-pattern" claim is now stale; the codebase has 9)

*[Some claims, particularly around ClickHouse Cloud's specific multi-tenancy product features and per-tenant CMEK availability, are based on general knowledge of the product as of late 2024 / early 2026. Verify against the current ClickHouse Cloud pricing and security docs before architectural commitment.]*
