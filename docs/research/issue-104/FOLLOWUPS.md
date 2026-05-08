# Follow-up issues from #104 research

Each entry below maps to a single follow-up GitHub issue, referencing #104 as its source. Items already covered by existing open issues (#8, #9, #89, #103, #105, #106) are noted at the bottom and **not** proposed as new issues — they are tracked where they belong.

---

## FU-1 — Align harness OTel attributes with the GenAI semantic conventions

**Area:** observability
**Linked to:** #104 (this research), #100 (native OTLP/HTTP), #98/#99 (OTel productisation)
**Priority:** P2

### Body

The OTel emitter (`harness/internal/trace/otel.go`) emits stirrup-specific span attribute names — `run.id`, `run.mode`, `run.provider`, `run.model`, `turn.tokens.input`, `tool.name`, etc. The OpenTelemetry GenAI semantic conventions ([https://opentelemetry.io/docs/specs/semconv/gen-ai/]) define a stable namespace (`gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`) plus tool/agent attributes. Off-the-shelf APM dashboards (Honeycomb, Datadog, Grafana, New Relic) key off the GenAI conventions; stirrup's stirrup-prefixed attributes work in custom dashboards but not in vendor-shipped ones.

The proposed change is ~30 lines of attribute renaming in `otel.go`, plus a transition policy decision: rename outright (breaks any existing operator dashboards keyed on `run.*`), dual-emit for one minor version (acceptable cost), or keep `run.*` and add `gen_ai.*` aliases (cheapest but creates a parallel naming universe).

The four-of-six convergence in the industry survey treats `gen_ai.*` as the cross-vendor lingua franca. Recommendation in #104 is to align where feasible. As of mid-2026, the GenAI semconv has cross-cutting stable attributes (e.g. `error.type`) but most GenAI-specific attributes remain in Development stability — design accordingly.

**Done looks like:** an ADR (in `docs/`) stating the alignment decision and a PR renaming attributes (with optional dual-emit window). No span format wire change at the OTLP layer.

---

## FU-2 — Resolve `HarnessEvent.trace` field and narrow `TraceLakehouse` interface

**Area:** proto / cleanup
**Linked to:** #104 (this research), parent #15
**Priority:** P2

### Body

Two related cleanups identified during #104 research:

1. **`HarnessEvent.trace`** (`proto/harness/v1/harness.proto:122`) is documented as carrying a `RunTrace` on `done` events, but `loop.go:294-298` emits `done` without populating it (`grpc_translate.go:26-28` would forward a non-nil trace; the loop never produces one). The wire field is dead code today.

2. **`TraceLakehouse.StoreTrace` and `StoreRecording`** (`types/lakehouse.go:9-27`) are never called outside test code. The harness has no production writer; eval/runner does not call them; the eval CLI only reads.

Two options for #1:
- **Populate it.** The trace would carry the simplified 7-field proto shape (run_id, turns, tokens, cost, duration, stop_reason). Requires the loop to wire `Trace.Finish` output into `event.Trace` before `Emit`. Useful as a CP "I have all the metrics now" signal even when OTel is the canonical wire.
- **Drop it in proto v2.** Rely on OTel + the new `UploadRecording` to carry trace data. Cleanest, but requires a proto major version cut.

For #2: drop `StoreTrace` / `StoreRecording` from the interface; they are vestigial. Keep `QueryTraces` / `QueryRecordings` / `Metrics` / `Close` — that is the actual eval CLI surface.

**Done looks like:** a PR removing `StoreTrace` / `StoreRecording` from the interface and deleting their `FileStore` implementations (or moving them to a CP-side adapter); a separate decision (ADR or direct PR) on whether to populate or remove `HarnessEvent.trace`. Should land before the CP recording path ships so the proto contract is clean.

---

## FU-3 — Introduce `tenant_id` wire format and CP-stamping invariant

**Area:** security / multi-tenancy
**Linked to:** #104 (this research), #8 (observability security)
**Priority:** P1

### Body

The proto, `RunConfig`, `RunTrace`, and `RunRecording` carry **no `tenant_id` field** today. Adding multi-tenant isolation requires deciding the wire shape and the trust boundary. The recommendation in #104 is the **bridge model** (per-tenant dataset/database, shared engine, per-tenant CMEK) with **CP-stamped tenant identity** — the harness authenticates with a verified credential (mTLS + SPIFFE for service-to-service; OIDC bearer for OSS-self-host-into-our-CP) and the CP derives `tenant_id` from the auth context, never trusting a body field.

Required changes:

- **Auth path:** wire mTLS (with SPIFFE IDs `spiffe://stirrup.cp/tenant/<id>/harness/<name>`) into the existing `GRPCTransport` (`harness/internal/transport/grpc.go`) on the harness side; matching JWT/SPIFFE verification on the CP side.
- **No body field:** explicitly do *not* add `tenant_id` to `types.RunConfig`. Adding it would let a misconfigured harness assert spoofed tenancy. The field belongs in a CP-internal struct.
- **Stamp invariant:** on receipt of any harness event (including the proposed `UploadRecording` chunks), the CP resolves the authenticated identity → `tenant_id`, rejects/strips any client-asserted tenant field, and stamps `tenant_id` on every record before passing to the writer. Sub-agent runs inherit the parent's stamp implicitly.
- **Wire shape for `OSS-self-host`:** OIDC client credentials flow → bearer JWT with `tenant` claim → `Authorization: Bearer ...` header on the gRPC connection.

Reject the Mimir / Tempo / Loki `X-Scope-OrgID` self-asserted-header pattern — it is only safe behind a trusted reverse proxy, and the CP is itself the trust boundary.

This issue blocks first-tenant onboarding for #15, even before BigQuery / ClickHouse Cloud is selected (#105/#106) — the auth and stamping contract is engine-independent.

**Done looks like:** an ADR on tenant-identity flow, a proto change (or proto-side documentation that no body fields exist), an auth implementation in the gRPC transport, and a CP-side reference implementation of the stamping invariant.

---

## FU-4 — Implement `UploadRecording` gRPC method and recording assembly seam

**Area:** ingestion / impl
**Linked to:** #104 (this research), parent #15
**Priority:** P1

### Body

The recommended ingestion contract from #104 introduces a server-streaming gRPC method `UploadRecording(stream RecordingChunk) returns (RecordingReceipt)` for `RunRecording` payloads. Today the harness does not produce recordings — `grep -rn "RunRecording{" harness/` is empty. The implementation work is:

1. **Wire the recording producer in the harness.** The agentic loop must accumulate `TurnRecord` shape data per turn (currently only `replay.go` constructs these). Decide: capture during the run (memory cost; bounded by max_turns × payload size) vs. assemble at end-of-run from the trace emitter's accumulated state. Capture-during is more flexible (multimodal #103, partial-run forensics); assemble-at-end is cheaper but couples to the trace emitter shape.

2. **Add the proto method and message types** to `proto/harness/v1/harness.proto`. Sketch in #104 §3.2 of the synthesis comment. Run `buf generate`.

3. **Implement the chunked upload client** in `harness/internal/transport/grpc.go` (or a sibling file). Server-streaming RPC; manifest-then-turn-then-final-outcome ordering; backpressure: warn-and-drop on failure (do not spool to disk).

4. **Recording-side scrubbing.** Wire `security.Scrub` over `TurnRecord.ToolCalls[].Input/Output` and `ModelInput.Messages[].Content` strings before each chunk is emitted. This is the new scrubbing site referenced in #104 §3.5.

5. **Idempotency.** Compute `content_sha256` over the assembled `RunRecording` proto bytes; pass in the manifest; CP verifies after final chunk; CP returns `RecordingReceipt.already_exists = true` on retry collision.

6. **Presigned-URL escape hatch.** Optional flag `--recording-upload-mode=stream|presigned`. When `presigned`, the CP returns an `upload_url` in a `ControlEvent` and the harness does a direct HTTP PUT. Conflicts with `NetworkMode: none` on the container executor — document the deployment constraint.

**Open question (was OQ-3):** the container executor's `NetworkMode: none` posture means the harness inside the container has no network egress. The gRPC connection to the CP is established by the host process before the container starts (`cmd/job.go`). Streaming through that gRPC connection is fine; the presigned-URL path may not be feasible inside a hardened container without reopening egress. Resolve this before settling on the escape-hatch design.

**Done looks like:** PR(s) implementing the proto method, the harness producer, the harness streaming client, scrubbing-at-chunk-emission, and an end-to-end test against a minimal CP. Recording assembly seam documented in CLAUDE.md.

---

## FU-5 — Formalise the schema-evolution policy

**Area:** storage / governance
**Linked to:** #104 (this research)
**Priority:** P2

### Body

#104 recommends:

- Default: additive-with-omitempty for the JSON wire shape; `optional` for proto fields.
- Add a `schema_version INT64` column (or proto field) to every `RunTrace` row, bumped on changes consumers might notice.
- Hot fields projected into typed columns (BigQuery materialised views; ClickHouse projections) — initial set: `run_id`, `tenant_id`, `started_at`, `outcome`, `mode`, `model`, `turns`, `tokens_input`, `tokens_output`, `parent_run_id`.
- Reserved for major breaks: versioned tables (`runs_v1`, `runs_v2`) with a cutover timestamp; never the default.
- Skip schema registry until external consumers exist.

Required to formalise:

1. **Add `schema_version`** to `types.RunTrace` and either the proto `RunTrace` (more rigour) or only the persisted shape (less ceremony). Default to the latter unless wire-side versioning is needed.
2. **Document the policy** in `docs/schema-evolution.md` (or in `VERSION1.md`). Cover: when to bump `schema_version`, what counts as additive vs. semantic change, the dangerous class (`OutputSize` units silently changing), and the `runs_v1 → runs_v2` cutover playbook.
3. **CI gate:** `buf breaking` on the proto `RunTrace` shape (already in toolchain via `buf.yaml`); add a unit test that pins the JSON shape of `RunTrace` and `RunRecording` so accidental field renames break CI.
4. **Bound `VerificationResult.Details`** (`types/runtrace.go:56`, currently `map[string]any`). Either cap the byte size at the schema layer or move it to a dedicated typed substructure. Otherwise it is a silent escape hatch for unbounded payloads.

This intersects FU-1 (any rename of `run.*` → `gen_ai.*` is a `schema_version` bump on the OTel side too).

**Done looks like:** ADR + `schema_version` column added; lint or test rules enforcing additive-only changes; the policy committed to `docs/`.

---

## FU-6 — Eval CLI `--lakehouse local:|cp:` URL scheme and ADC-style auth

**Area:** CLI / DX
**Linked to:** #104 (this research), parent #15
**Priority:** P2

### Body

#104 recommends extending the `--lakehouse` flag to accept a URL scheme:

- `local:./path` (or bare path for backwards compatibility) → `eval/lakehouse/filestore.go::FileStore`
- `cp:https://insights.example.com` → new `eval/lakehouse/cp.go`, gRPC client of CP `Insights` service
- `cp:` (no URL) → reads `~/.config/stirrup/insights-url`

Required changes:

1. **URL parser** in `eval/cmd/eval/main.go` and a small dispatcher constructing the right `TraceLakehouse` implementation by scheme.
2. **CP Insights gRPC client** (`eval/lakehouse/cp.go`) implementing `Metrics` and `MineFailures` against a typed gRPC API on the CP. Reject SQL passthrough — leaks schema, bypasses RLS.
3. **Auth flow:** `stirrup-eval auth login --insights-url https://insights.example.com` (new subcommand) that performs an OIDC PKCE browser flow and writes a refresh-token-bearing credentials file to `~/.config/stirrup/credentials.json`. Subcommands resolve credentials at call time.
4. **CI / non-interactive auth:** workload identity (GCP / GitHub OIDC → CP token exchange), reusing the pattern from `harness/internal/credential/web_identity_aws.go`.
5. **`mine-failures` returns `QuarantinedSuite`** (see FU-8). The CLI honours quarantine flags: refuses to execute without `--accept-quarantine`, prints the flag list.

OSS path stays cost-free: `--lakehouse local:./eval/baselines` works with no auth, no CP.

**Done looks like:** the URL scheme implemented, `cp:` backend talking to a CP reference implementation, `auth login` working, OIDC token storage handled, integration tests covering both backends.

---

## FU-7 — `LogScrubber` pattern coverage gaps and documentation reconciliation

**Area:** security / docs
**Linked to:** #104 (this research), #8 (observability security), #9 (eval framework security)
**Priority:** P2

### Body

Two related findings from #104 research:

1. **Documentation drift.** `CLAUDE.md` and `VERSION1.md` both state "7-pattern" `LogScrubber`. The actual implementation has **9 patterns** (`harness/internal/security/logscrubber.go:15-32`):

   1. `anthropic_api_key`
   2. `openai_api_key`
   3. `github_pat`
   4. `github_app_token`
   5. `aws_access_key_id` (the `AKIA...` ID, not the secret access key)
   6. `bearer_token` (case-insensitive `Bearer …`, JWTs included)
   7. `pem_private_key`
   8. `secret_ref` (`secret://...`)
   9. `api_key_header` (literal `api-key:` / `x-api-key:` / `Ocp-Apim-Subscription-Key:` lines, added for Azure)

2. **Coverage gaps.** The pack does not catch:
   - **AWS secret access keys** (40-char base64-y, no anchor) when they appear standalone in tool output (e.g. `cat ~/.aws/credentials` → `aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`). Pattern 6 catches them only after a `Bearer ` prefix.
   - **Slack tokens** (`xoxb-...`)
   - **Stripe live keys** (`sk_live_...`)
   - **GCP API keys** (`AIza...`)
   - **Azure storage account keys** (44-char base64, no anchor)
   - **Generic 32-character hex** strings

Each is a real risk in tool outputs (`run_command` reading a `.env`, `read_file` against a Terraform state).

3. **`RunConfig.Redact()` gaps.** Today `Redact()` strips secret references but does not strip:
   - `Provider.BaseURL` (could leak internal infrastructure topology, e.g. `https://llm-gateway.internal.corp/openai/v1`)
   - `Provider.APIKeyHeader`
   - `Provider.QueryParams`
   - `Tools.MCPServers[i].URL`

   Recommendation: extend `Redact()` to normalise `BaseURL` to its origin (strip path) and to redact non-allowlisted query params.

**Done looks like:** PR(s) adding the missing patterns (with an explicit key/value heuristic for AWS-secret-shaped data), extending `Redact()`, and updating `CLAUDE.md` and `VERSION1.md` to reflect the actual pattern count and behaviour. Should be coordinated with #8.

---

## FU-8 — Quarantine envelope for `mine-failures`

**Area:** eval / security
**Linked to:** #104 (this research), #9 (eval framework security)
**Priority:** P2

### Body

The `stirrup-eval mine-failures` command reads `RunRecording` payloads — full conversation content, including potentially-unscrubbed tool outputs and prompts — and writes them into an `EvalSuite` JSON file that may be checked into a repo and run on contributors' machines. This is a clear interaction with #9 (eval framework security).

#104 recommends a `QuarantinedSuite` envelope:

```go
type QuarantinedSuite struct {
    Suite           EvalSuite
    QuarantineFlags []string  // e.g. "unscrubbed_secret_event", "large_payload", "pii_classification"
}
```

Server-side rules (CP-applied by `MineFailures`):

- Flag `unscrubbed_secret_event` if any recording in the source dataset triggered `SecretRedactedInOutput`.
- Flag `pii_classification` if the recording is classified `restricted` (see #104 §8).
- Flag `large_payload` if any task input/output exceeds a configurable byte limit.
- Future flags driven by operator policy (e.g. `cross_tenant_data` — impossible under the FU-3 stamping invariant but checked defensively).

Client-side rules (eval runner-applied):

- The runner refuses to execute a `QuarantinedSuite` without `--accept-quarantine`.
- The runner prints the full flag list before execution (when accepted).
- Committing a quarantined suite to a repo is a code-review smell; CI lint blocks it.

This puts the CP in charge of declaring "this content is safe to exfiltrate"; the CLI is just a courier.

**Open question (was OQ-12):** does a flagged suite get redacted automatically (lossy — the LLM context that triggered the failure may be exactly what is needed to reproduce) or refused entirely (operator decides explicitly)? Default behaviour needs an opinion. Discuss in the issue.

**Done looks like:** the `QuarantinedSuite` shape defined in `types/eval.go`, the server-side `MineFailures` (or its CP equivalent) emitting flags, the runner enforcing them, an ADR on the redact-vs-refuse default. Coordinate with #9.

---

## Items intersecting existing issues — no new follow-ups proposed

The following surfaced during research but are already in scope of existing open issues. Notes are added to the relevant issues rather than creating duplicates.

- **AWS secret access key regex coverage.** Already #8 (observability security). Will be folded into FU-7 work.
- **`RunConfig.Redact()` not stripping `BaseURL` / `APIKeyHeader` / `QueryParams` / MCP URL.** Already #8. Folded into FU-7.
- **CMEK key revocation latency for GDPR.** Already #8 (right-to-erasure scope). Track when GDPR scope is confirmed.
- **BigQuery managed-Iceberg row-size limit pinning.** Covered by #105 (BigQuery deep dive).
- **ClickHouse Cloud GCS Iceberg support.** Covered by #106 (ClickHouse deep dive).
- **Recording compression ratio measurement.** Covered by #105/#106 — needs a representative-corpus benchmark before pinning the cost model.
- **`#89` OTel parent-context plumbing for `tool.spawn_agent` nesting.** Already tracked as #89.
- **`json.RawMessage` policy generalisation to multimodal payloads.** Covered by #103 (multimodal).
- **`VerificationResult.Details` unbounded `map[string]any`.** Folded into FU-5 (schema evolution).
- **Tenant onboarding automation (provisioning job for the bridge model).** CP infrastructure work, not a stirrup-repo issue. Track at the CP repo level.
- **OSS contributor identity and CP free-tier quota / abuse model.** Out of scope for this research; a CP product decision.
