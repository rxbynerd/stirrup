# Stirrup Code Review — 2026-03-22

Comprehensive review of the Go harness codebase on the `golang` branch. All 12 VERSION1.md components are fully implemented. 400+ tests pass at 100% package-level coverage. Zero known vulnerabilities.

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
GitHub Actions at `.github/workflows/ci.yml` covers `go test` for types and harness modules, `go build` for the harness binary, and container image publish to GHCR on main branch pushes.

---

## Issues Found

### P1 — Fix before production use

**Token estimation uses crude `/4` heuristic** (`core/loop.go:297`)
The `estimateCurrentTokens` function divides character count by 4. It doesn't account for system prompt size, tool definitions, JSON structural overhead, or the difference between code and natural language tokenization. Impact: budget underreporting on runs with many tools or large context. Could cause context window overflows that surface as opaque provider errors rather than clean budget breaches.

**Budget not re-checked after tool results** (`core/loop.go:304`)
Tool results are appended to message history but budget enforcement only runs at the *start* of each turn. A tool returning massive output (e.g., reading a large file) won't trigger a budget check before the next provider call. The provider may reject the request with a context-length error instead of the harness producing a clean budget-exceeded outcome.

**Read-only mode validation is incomplete** (`types/runconfig.go:310-315`)
`ValidateRunConfig` checks that non-execution modes use restrictive permission policies, but doesn't validate that write-capable tools (`write_file`, `run_command`) are disabled or that the executor type is read-only compatible. A misconfigured read-only mode could still modify files.

**Silent error suppression in the loop** (`core/loop.go` various locations)
`Transport.Emit()`, `Git.Setup()`/`Git.Finalise()`, and `Trace.RecordTurn()` errors are discarded with `_`. Git errors are the most concerning — if a checkpoint commit silently fails, the agent believes its changes are persisted when they aren't. These should at minimum be logged, even if non-fatal.

### P2 — Address soon

**FollowUpLoop is untested** (`core/loop.go:322-369`)
The follow-up request handling code path has zero test coverage. It's a separate function from the main loop with its own control flow.

**JSON Schema validator is simplified** (`security/inputvalidator.go`)
The Phase 1 validator supports `type`, `required`, `additionalProperties`, and `properties` — but not `$ref`, `oneOf`, `anyOf`, `allOf`, or `format`. Noted with a TODO suggesting `santhosh-tekuri/jsonschema`. MCP tools with complex schemas could pass invalid input through validation.

**MCP connection failure is fatal** (`core/factory.go:93-101`)
If any configured MCP server is unavailable at startup, `BuildLoop` fails entirely. The harness cannot start even if the MCP server is optional for the task. Could degrade gracefully: log a warning, skip the server's tools, continue.

**Magic numbers in core logic** (`core/loop.go`, `core/factory.go`)
`200000` (context window default), `64000` (response token reserve), `3` (verification retries), `100` (max turns cap) are hardcoded inline. Should be named constants for clarity and to prevent accidental inconsistency.

**Pricing table hardcoded** (`core/types.go:281-289`)
Model pricing lives in a function body. Will need manual updating as new models release — easy to forget. Consider externalising or at least centralising with the model name constants.

### P3 — Low priority

- No rate limiting on tool execution — model could call tools in a tight loop
- HTTP error response bodies from providers are not size-limited
- Fuzzy diff threshold hardcoded at 0.80 in udiff strategy — not configurable
- Container executor `putArchive` path parameter not URL-encoded — would fail on special characters
- Web fetch User-Agent `stirrup-harness/1.0` is minor information disclosure
- `AskUpstreamPolicy` has no timeout on upstream response (mitigated by caller's context timeout, but could be more explicit)

---

## What's Missing

### Eval Framework (Phase 5 — the critical gap)

Types are defined in `types/eval.go` (`EvalSuite`, `EvalTask`, `EvalJudge`, `Experiment`, `RunConfigOverrides`) but the `eval/` module contains only a `go.mod`. Nothing is implemented:

- **ReplayProvider** — deterministic provider replaying recorded stream events from an `EvalTask`
- **ReplayExecutor** — deterministic executor replaying file reads and command outputs from a baseline run
- **Eval runner** — orchestrator for local runs with trace collection
- **Comparison reporter** — diffs two trace sets, flags regressions in cost/turns/outcome
- **CLI commands** — `eval run`, `eval compare`, `eval mine-failures`, `eval drift`
- **First eval suite** — mined from real repo PR history (10-20 tasks)

Without eval, there is no way to measure whether changes to prompts, context strategies, model routing, or model versions improve or regress quality. This blocks Phases 6-7 (lakehouse integration, drift detection, sub-agent spawning).

### Other gaps

- **No end-to-end smoke test** with a real provider (even a single recorded interaction would catch wire-format regressions)
- **Sub-agent spawning** (Phase 7 per VERSION1.md)
- **Lakehouse integration** (Phase 6 — production feedback loops)

---

## Recommended Focus Areas (in order)

### 1. Harden the loop (half day)
- Extract magic numbers into named constants
- Re-check budget after tool results before the next provider call
- Log (don't discard) transport/git/trace errors — make them observable even if non-fatal
- Add tests for `FollowUpLoop`
- Tighten read-only mode validation to check tool enablement and executor type

### 2. Token estimation improvement (half day)
Replace the `/4` heuristic with something that accounts for system prompt size and tool definitions. Doesn't need a real tokenizer — even a calibrated multiplier based on message role and content type would be materially better than dividing everything by 4.

### 3. Eval framework (primary remaining work)
Suggested implementation order:
1. `ReplayProvider` + `ReplayExecutor` (deterministic test doubles that replay recorded events)
2. Minimal eval runner: takes a suite JSON, runs each task against replay doubles, collects traces
3. Comparison reporter: diffs two trace sets, flags regressions in outcome/cost/turns
4. Mine 10-20 eval tasks from a real repo's closed PRs to populate the first suite
5. CI integration: run the eval suite as a gate on harness changes

### 4. Full JSON Schema validation
Replace the simplified input validator with `santhosh-tekuri/jsonschema` or equivalent. Becomes important as MCP tool schemas grow more complex (nested objects, `oneOf` unions, format constraints).

### 5. Graceful MCP degradation
Change `BuildLoop` to warn and continue when an MCP server is unreachable, rather than failing the entire loop construction. The harness should be usable without optional remote tool servers.

---

## By the Numbers

| Metric | Value |
|--------|-------|
| Internal packages | 15 |
| Packages with tests | 15/15 (100%) |
| Test functions | ~400+ |
| All passing | Yes |
| External dep families | 5 (AWS SDK, gRPC, protobuf, OTel, OTel exporter) |
| Known vulnerabilities | 0 |
| TODOs in production code | 1 (input validator — acknowledged) |
| VERSION1.md components | 12/12 implemented |
| VERSION1.md phases | 1-4 of 7 complete |
| CI | GitHub Actions (build, test, container publish) |
