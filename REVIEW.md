# Stirrup Code Review — 2026-03-22 (updated)

Comprehensive review of the Go harness codebase on the `golang` branch. All 12 VERSION1.md components are fully implemented, all 7 phases complete. 630+ tests pass across 22 packages at 100% package-level coverage. Zero known vulnerabilities.

## Overall Assessment

This is a strong V1. The architecture is clean — pure dependency injection, interface-driven, minimal external dependencies — and the security posture is thorough at every layer. The codebase is ready to run real workloads. What it lacks is the infrastructure to *measure* those workloads (eval framework) and a handful of hardening items that will matter at scale.

---

## What's Strong

### Architecture
The 12-component interface-based design with factory composition is textbook. All dependencies are injected via `core.BuildLoop`, the agentic loop is a pure function of its interfaces, and resource cleanup follows LIFO order. The factory in `core/factory.go` is the clearest composition root in the project.

### Provider Adapters
Hand-rolled SSE/HTTP clients for Anthropic and OpenAI-compatible, with AWS SDK only where IAM SigV4 demands it. All three providers (Anthropic, Bedrock, OpenAI) have comprehensive streaming implementations with proper tool JSON accumulation across delta events, context cancellation, and 40+ tests between them. No SDK bloat.

### Security — Defense in Depth
- **Path traversal**: Blocked with symlink-aware containment checks in all 3 executors (local, container, API). Tested with realistic attack scenarios (`../../../`, symlink escapes, absolute paths outside workspace).
- **Command injection**: `shellQuote()` in search_files with explicit tests. No unbounded user input in shell commands.
- **Container hardening**: `CapDrop: ALL`, `no-new-privileges`, `NetworkMode: none` by default. API keys never enter the container.
- **Log scrubbing**: 7-pattern regex scrubber (Anthropic keys, OpenAI keys, GitHub PATs/app tokens, AWS access keys, Bearer tokens, PEM keys, secret:// refs) applied before all trace and transport output.
- **Web fetch**: Private IP blocking (RFC 1918, loopback, link-local, multicast), scheme whitelisting (http/https only), DNS resolution validation, 100KB response cap.
- **Prototype pollution**: `__proto__` and `constructor` keys stripped from all tool inputs.
- **Environment filtering**: Command execution allowlists 27 safe env vars; blocks all AWS/GCP credentials.
- **Untrusted context**: Dynamic context wrapped in `<untrusted_context>` tags with explicit instructions to treat as data.

### Test Quality
Not just present but thoughtful:
- Mock Docker Engine API over Unix socket for container executor
- `bufconn` for in-process gRPC testing
- `httptest` servers for provider SSE streams
- Real git repos via `TempDir` for git strategy
- Table-driven tests throughout
- Edge cases: malformed JSON, context cancellation, symlink escapes, multi-hunk diffs with fuzzy matching

### Transport
Both stdio (NDJSON) and gRPC (bidi streaming) are production-ready with proper secret scrubbing and complex nested `RunConfig` proto translation. The K8s job entrypoint (`cmd/job/main.go`) correctly implements the dial-wait-run lifecycle.

### CI
GitHub Actions at `.github/workflows/ci.yml` covers `go test` for types, harness, and eval modules, `go build` for the harness and eval binaries, container image publish to GHCR on main branch pushes, and a tier 3 eval gate that runs eval suites when available.

---

## Issues Found

### P1 — Fix before production use

~~**Token estimation uses crude `/4` heuristic**~~ **RESOLVED**
Token estimation now accounts for per-message overhead (4 tokens), per-block overhead (3 tokens), tool-related metadata (IDs, names, ToolUseIDs), system prompt tokens, and tool definition tokens. Both call sites in the loop (context preparation and cost tracking) include all three sources. Tests cover the overhead constants.

~~**Budget not re-checked after tool results**~~ **RESOLVED**
Budget is now re-checked after tool results are appended (before git checkpoint), returning `budget_exceeded` immediately if breached.

~~**Read-only mode validation is incomplete**~~ **RESOLVED**
`ValidateRunConfig` now requires read-only modes to provide an explicit `tools.builtIn` list that excludes `write_file` and `run_command`. Four new test cases cover all branches.

~~**Silent error suppression in the loop**~~ **RESOLVED**
All previously-suppressed errors are now logged via `log.Printf`. Git checkpoint/finalise errors also emit best-effort warning events via transport.

### P2 — Address soon

~~**FollowUpLoop is untested**~~ **RESOLVED**
Four tests added in `followup_test.go`: zero grace period, follow-up arrival, grace expiry, and context cancellation.

**JSON Schema validator is simplified** (`security/inputvalidator.go`)
The Phase 1 validator supports `type`, `required`, `additionalProperties`, and `properties` — but not `$ref`, `oneOf`, `anyOf`, `allOf`, or `format`. Noted with a TODO suggesting `santhosh-tekuri/jsonschema`. MCP tools with complex schemas could pass invalid input through validation.

~~**MCP connection failure is fatal**~~ **RESOLVED** (`core/factory.go`)
MCP connection failures now log a warning and skip the unavailable server's tools, rather than failing the entire `BuildLoop`.

~~**Magic numbers in core logic**~~ **RESOLVED**
Extracted to named constants: `defaultMaxContextTokens`, `defaultReserveForResponse`, `tokenEstimationDivisor`, `absoluteMaxTurns`, `messageOverheadTokens`, `blockOverheadTokens`, `toolDefinitionOverheadTokens`.

~~**Pricing table hardcoded**~~ **RESOLVED**
Cost estimation removed from Stirrup entirely. Pricing is a control plane concern — see CONTROL_PLANE.md "Concerns delegated from Stirrup" section. The harness retains a `TokenTracker` for token budget enforcement only.

### P3 — Low priority

- ~~No rate limiting on tool execution — model could call tools in a tight loop~~ **RESOLVED** — stall detection (`core/stall.go`) terminates after 3 repeated identical calls or 5 consecutive failures
- ~~HTTP error response bodies from providers are not size-limited~~ **RESOLVED** — Anthropic and OpenAI provider adapters now cap error body reads to 1MB via `io.LimitReader`
- ~~Fuzzy diff threshold hardcoded at 0.80 in udiff strategy — not configurable~~ **RESOLVED** — threshold is now configurable via `UdiffStrategy.FuzzyThreshold` (default 0.80)
- ~~Container executor `putArchive` path parameter not URL-encoded — would fail on special characters~~ **RESOLVED** — `putArchive` and `getArchive` now use `url.PathEscape` on the path parameter
- ~~Web fetch User-Agent `stirrup-harness/1.0` is minor information disclosure~~ **DEFERRED** — Web Fetch tool will move to the control plane for fleet-wide caching. See CONTROL_PLANE.md "Concerns delegated from Stirrup" section.
- ~~`AskUpstreamPolicy` has no timeout on upstream response~~ **RESOLVED** — `AskUpstreamPolicy` now enforces a configurable timeout (default 5 minutes) with context deadline

---

## What's Missing

### ~~Eval Framework (Phase 5)~~ **RESOLVED** (2026-03-22)

The eval framework is now implemented with the following components:

- **ReplayProvider** (`harness/internal/provider/replay.go`) — deterministic provider replaying recorded `TurnRecord.ModelOutput` as stream events. Thread-safe atomic turn counter. 6 tests.
- **ReplayExecutor** (`harness/internal/executor/replay.go`) — deterministic executor replaying tool call recordings keyed by `(toolName, canonicalInput)`. Tracks writes for verification. 12 tests.
- **Judge system** (`eval/judge/`) — evaluates `EvalJudge` criteria against workspace state. Supports `test-command`, `file-exists`, `file-contains`, `composite` (with `all`/`any` require), and `diff-review` (stub). Path traversal prevention. 19 tests.
- **Eval runner** (`eval/runner/`) — orchestrates suite execution: validates suite, creates temp workspaces, clones repos, invokes harness binary, parses JSONL traces, applies judges. Supports `DryRun` mode.
- **Replay evaluator** (`eval/runner/replay.go`) — re-evaluates recorded runs through judges without re-running the harness.
- **Comparison reporter** (`eval/reporter/`) — diffs two `SuiteResult` sets, flags regressions (pass→fail) and improvements (fail→pass), computes turn deltas and aggregate metrics. Human-readable text formatter. 8 tests.
- **CLI** (`eval/cmd/eval/`) — `eval run --suite <path>` and `eval compare --current <path> --baseline <path>` subcommands.

**Still missing** (deferred to later phases):
- First eval suite mined from real repo PRs (10-20 tasks)
- `diff-review` judge implementation (requires LLM judge integration)
- Postgres/BigQuery lakehouse adapter (file-based adapter implemented; production adapter depends on control plane choices)

### ~~Lakehouse Integration (Phase 6)~~ **RESOLVED** (2026-03-22)

Production feedback loop infrastructure is now implemented:

- **TraceLakehouse interface** (`types/lakehouse.go`) — storage and querying abstraction for production run data. Extended `TraceFilter` with time bounds, `TraceMetrics` with percentile durations, `DriftReport` with delta tracking.
- **FileStore adapter** (`eval/lakehouse/filestore.go`) — file-based TraceLakehouse implementation. JSON files in `traces/` and `recordings/` directories. Supports all filter fields, aggregate metrics with p50/p95 percentiles. 20 tests.
- **`eval baseline`** command — pulls production metrics from a lakehouse, outputs TraceMetrics as JSON or human-readable summary.
- **`eval mine-failures`** command — queries failed recordings, constructs EvalTasks with default test-command judges, outputs an EvalSuite JSON.
- **`eval drift`** command — compares metrics between two adjacent time windows, flags significant changes (pass rate drop >5pp, cost/turns increase >20%). Exit code 1 on drift.
- **Tier 3 eval CI gate** — `eval-gate` job in CI runs eval suites on main branch pushes, compares against stored baselines, uploads results as artifacts.

### ~~Phase 7 Features~~ **RESOLVED** (2026-03-22)

Phase 7 is now complete:

- **Multi-strategy edit fallback** (`edit/multi.go`) — unified `edit_file` tool that accepts udiff, search-replace, or whole-file input and routes to the appropriate strategy with automatic fallback. 11 tests.
- **Sub-agent spawning** (`core/subagent.go`, `tool/builtins/subagent.go`, `transport/null.go`) — `spawn_agent` tool creates a fresh `AgenticLoop` with a subset of context, no recursion (spawn_agent excluded from child tools), synchronous execution with `captureTransport` for output extraction. 8 tests.
- **`eval compare-to-production`** (`eval/cmd/eval/main.go`) — loads eval results + production metrics from lakehouse, builds `LabVsProductionReport`, prints comparison table. 5 tests.
- **Security hardening** (SECURITY_HARDENING.md immediate fixes):
  - HTTP client timeouts on all provider adapters and MCP client
  - RunConfig validation bounds (FollowUpGrace ≤ 3600s, MaxCostBudget ≤ $100, MaxTokenBudget ≤ 50M)
  - Loop stall detection (`core/stall.go`) — repeated identical calls (3x) and consecutive failures (5x). 6 tests.

### Other gaps

- **No end-to-end smoke test** with a real provider (even a single recorded interaction would catch wire-format regressions)

---

## Recommended Focus Areas (in order)

### ~~1. Harden the loop~~ DONE (2026-03-22)

### ~~2. Token estimation improvement~~ DONE (2026-03-22)

### ~~3. Eval framework~~ DONE (2026-03-22)

### ~~4. Lakehouse integration~~ DONE (2026-03-22)

### 5. Mine first eval suite
Mine 10-20 eval tasks from a real repo's closed PRs using `eval mine-failures`. Add tier 1 (replay-based unit tests) and tier 2 (smoke eval) as CI gates. The CI infrastructure is in place — it just needs suite files.

### 6. Full JSON Schema validation
Replace the simplified input validator with `santhosh-tekuri/jsonschema` or equivalent. Becomes important as MCP tool schemas grow more complex (nested objects, `oneOf` unions, format constraints).

### ~~7. Graceful MCP degradation~~ DONE (2026-03-22)

---

## By the Numbers

| Metric | Value |
|--------|-------|
| Internal packages | 17 (harness) + 5 (eval) = 22 |
| Packages with tests | 22/22 (100%) |
| Test functions | ~630 |
| All passing | Yes |
| External dep families | 5 (AWS SDK, gRPC, protobuf, OTel, OTel exporter) |
| Known vulnerabilities | 0 |
| TODOs in production code | 1 (JSON Schema input validator — acknowledged) |
| VERSION1.md components | 12/12 implemented |
| VERSION1.md phases | 7/7 complete |
| CI | GitHub Actions (build, test, eval gate, container publish) |
