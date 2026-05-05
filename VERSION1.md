# VERSION1: Go Harness

This document describes what was built for Version 1 of Stirrup, a coding agent harness rewritten from Ruby to Go. It covers the architectural decisions, what shipped, what was deferred, and the security posture at merge time. Post-V1 hardening is tracked in GitHub Issues.

---

## Why Go

The original Ruby prototype (`stirrup.rb`, `server.rb`) validated the core patterns — agentic loop, tool abstraction, streaming events — but hit structural limits:

- **Concurrency**: MRI's GIL made true parallelism impossible. The Ruby prototype used two mutexes per connection to manage concurrent access; this was fragile and would only get worse.
- **Deployment**: Ruby's startup time, memory footprint, and native extension dependency management (Bundler) made it heavier than necessary for containerised workloads.
- **Supply chain**: The harness holds API keys and executes code. Every third-party dependency is an attack surface. Go's `go.sum` + `sum.golang.org` transparency log, absence of build-time code execution, and rich stdlib (HTTP, JSON, crypto, testing, regexp, compression) mean fewer dependencies and auditable ones.
- **Cloud-native fit**: Go produces a single static binary (~10-20MB on `scratch`/`distroless`), has first-class gRPC support (the reference implementation), and goroutines provide lightweight concurrency without coloured functions.

The lack of official AI SDKs for Go was acknowledged as a trade-off but turned out to be a strength. Each provider adapter is ~200-300 lines of HTTP+SSE parsing using stdlib (`net/http`, `encoding/json`, `bufio.Scanner`). Every line is owned and auditable.

---

## Architecture

The harness is a **short-lived job, not a server**. In cloud deployment, a control plane starts it as a K8s Job, passing the gRPC endpoint address as an environment variable. The harness connects outbound, receives its task assignment, streams events back, and exits on completion. For local development, a CLI entrypoint reads from stdin and writes to stdout.

The core design principle: **use the LLM only when judgement is needed; everything else is deterministic code**. The harness is a state machine with LLM calls at decision points, not an LLM with code bolted on.

### 12 swappable components

Every run is fully described by a `RunConfig` — a declarative specification of which implementation to use for each component. The core loop depends only on interfaces, never on concrete implementations. All dependencies are injected via the factory (`core.BuildLoop` / `core.BuildLoopWithTransport`).

| # | Component | Interface | Implementations shipped |
|---|---|---|---|
| 1 | Model provider | `ProviderAdapter` | Anthropic (direct SSE), AWS Bedrock Converse, OpenAI-compatible |
| 2 | Model router | `ModelRouter` | Static, per-mode, dynamic (complexity-based) |
| 3 | System prompt | `PromptBuilder` | Default per-mode templates, composed from fragments + dynamic context |
| 4 | Context strategy | `ContextStrategy` | Sliding window, LLM-summarise, offload-to-file |
| 5 | Tool registry | `ToolRegistry` | 7 built-in tools + MCP remote tools |
| 6 | Executor (sandbox) | `Executor` | Local (tier 1), container/Docker (tier 2), API/GitHub (tier 0), replay (eval) |
| 7 | Edit strategy | `EditStrategy` | Whole-file, search-replace, unified diff, multi-strategy with fallback |
| 8 | Verifier | `Verifier` | None, test-runner, LLM-as-judge, composite |
| 9 | Permission policy | `PermissionPolicy` | Allow-all, deny-side-effects (denies workspace-mutating tools only), ask-upstream (prompts on tools whose `RequiresApproval` flag is set) |
| 10 | Transport | `Transport` | Stdio (NDJSON), gRPC bidi streaming, null (sub-agents) |
| 11 | Git strategy | `GitStrategy` | None, deterministic |
| 12 | Trace emitter | `TraceEmitter` | JSONL, OpenTelemetry (OTLP/gRPC) |

### Modes

Five modes shipped, each a partial `RunConfig` preset selecting the appropriate tools, permissions, and prompt templates:

| Mode | Tools | Permission policy | Output |
|---|---|---|---|
| Execution | Read, write, shell, search, web fetch, sub-agent | Allow-all | Code changes |
| Planning | Read-only | Deny-side-effects | Structured plan |
| Review | Read-only | Deny-side-effects | Structured review |
| Research | Read-only + web fetch | Deny-side-effects | Research brief |
| Toil | Read-only + web fetch | Deny-side-effects | Structured briefing |

Modes are not special — they are saved configurations. Any field can be overridden per-task via the `RunConfig`.

### Project structure

Four Go modules in a workspace (`go.work`):

```
stirrup/
  go.work                    # Go workspace: types, harness, eval, gen
  proto/harness/v1/          # Protobuf definitions for gRPC transport
  gen/                       # Generated Go code from proto (separate module)
  types/                     # Shared type definitions (zero dependencies)
  harness/                   # The harness binary
    cmd/stirrup/             # Unified CLI entrypoint (harness + job subcommands)
      main.go
      cmd/root.go
      cmd/harness.go
      cmd/job.go
    internal/
      core/                  # AgenticLoop, factory, token tracking, sub-agent, stall detection
      credential/            # Cross-cloud credential federation
      provider/              # Anthropic, Bedrock, OpenAI-compatible adapters
      router/                # Static, per-mode, dynamic model routing
      prompt/                # Per-mode templates, composed prompt builder
      context/               # Sliding window, summarise, offload-to-file
      tool/                  # ToolRegistry + 7 built-in tools + spawn_agent
      executor/              # Local, container (Docker/Podman), API (GitHub), replay
      edit/                  # Whole-file, search-replace, udiff, multi-strategy
      verifier/              # None, test-runner, LLM-as-judge, composite
      permission/            # Allow-all, deny-side-effects, ask-upstream
      git/                   # None, deterministic
      transport/             # Stdio, gRPC bidi streaming, null
      trace/                 # JSONL, OpenTelemetry
      observability/         # Structured logging (slog + ScrubHandler), OTel metrics
      health/                # File-based K8s liveness probes
      security/              # SecretStore, LogScrubber, input validation
      mcp/                   # MCP client (Streamable HTTP, JSON-RPC 2.0)
  eval/                      # Eval framework (separate module)
    cmd/eval/main.go         # CLI: run, compare, baseline, mine-failures, drift, compare-to-production
    judge/                   # test-command, file-exists, file-contains, composite
    runner/                  # Suite runner (live + replay)
    reporter/                # Comparison reporter
    lakehouse/               # TraceLakehouse adapters (file-based)
```

The `types` module has zero dependencies and defines the contract between harness, eval, and the (future) control plane. The `gen` module holds protobuf-generated code.

---

## Provider adapters

All three adapters use hand-rolled HTTP clients against documented REST APIs, consistent with the minimal-dependency philosophy.

**Anthropic** (`provider/anthropic.go`) — SSE streaming via `net/http` + `bufio.Scanner`. Credentials via `SecretStore` (API key in `x-api-key` header).

**Bedrock** (`provider/bedrock.go`) — AWS ConverseStream API via `aws-sdk-go-v2`. Translates between internal types and Bedrock's union-type wire format. Auth is IAM (SigV4); accepts optional `aws.CredentialsProvider` for cross-cloud credential federation.

**OpenAI-compatible** (`provider/openai.go`) — OpenAI chat completions streaming. Works with OpenAI, LiteLLM, Azure OpenAI, vLLM, Ollama via configurable `baseURL`.

All three providers have comprehensive streaming implementations with tool JSON accumulation across delta events and context cancellation. Anthropic and OpenAI-compatible error response bodies are bounded with `io.LimitReader` to avoid unbounded memory use.

### Credential federation

The `credential` package enables cross-cloud authentication. Two composable layers:

- **TokenSource** — fetches identity tokens from the runtime environment. Implementations: `GKEMetadataTokenSource` (GKE Workload Identity), `FileTokenSource` (k8s projected volumes), `EnvTokenSource`.
- **credential.Source** — exchanges identity tokens into provider-specific credentials. Implementations: `StaticSource` (wraps SecretStore for API keys), `AWSDefaultSource` (SDK default chain), `WebIdentityAWSSource` (OIDC token -> STS AssumeRoleWithWebIdentity -> AWS credentials).

`WebIdentityAWSSource.Resolve()` is non-blocking: it sets up a lazy `aws.CredentialsCache` that calls STS on first use and automatically refreshes before expiry.

---

## Executor tiers

The `Executor` interface abstracts where commands run and how files are accessed. Three tiers shipped, selectable per-task via `RunConfig`:

**Tier 0 — API-backed** (`executor/api.go`): Read-only, backed by GitHub REST API. No clone, no filesystem, no sandbox. `WriteFile` and `Exec` return errors. Uses stdlib `net/http` only. URL paths are properly escaped via `url.PathEscape`.

**Tier 1 — Local process** (`executor/local.go`): Workspace-scoped with symlink-aware path containment (`filepath.EvalSymlinks`). Suitable for trusted environments (local dev, internal CI). Environment variables are filtered to an allowlist of 27 safe vars; all API keys and cloud credentials are blocked.

**Tier 2 — Container** (`executor/container.go`, `executor/container_api.go`): Uses Docker Engine REST API directly over a Unix socket, with zero external dependencies. Both Docker and Podman are supported (same Engine API). Container lifecycle: created at executor init with `sleep infinity`, all operations go through exec or archive API, destroyed on `Close()`. Hardened with `--cap-drop ALL`, `--security-opt no-new-privileges`, `--network none` by default. API keys never enter the container.

Filesystem executors enforce: 10MB file size limits, 1MB command output cap, 30s default command timeout (5min max), and symlink-aware workspace containment. The API executor is read-only and validates workspace-relative paths before using the GitHub Contents API.

**Tier 3 — MicroVM** (Firecracker/E2B/Kata) was designed but not implemented. The `Executor` interface means it can be added as a drop-in without touching the loop, tools, or anything else.

---

## Edit strategies

Research data showed edit format is one of the highest-leverage harness decisions (disabling fuzzy patching increases errors 9x per Aider benchmarks; edit format choice affects model accuracy by 30-50%). Four strategies shipped:

- **Whole-file** — model writes entire file content. Simple, used for new files and small files.
- **Search-replace** — model specifies `old_string`/`new_string` pairs. This is what Claude Code uses.
- **Unified diff** — model produces a unified diff. Applied with multi-strategy fallback: exact match -> whitespace-insensitive -> fuzzy Levenshtein (configurable threshold, default 0.80).
- **Multi-strategy** (`edit/multi.go`) — unified `edit_file` tool that accepts fields from all three strategies, routes based on which fields are present, and automatically falls back to the next applicable strategy on failure.

---

## Core loop features

### Verification

The `Verifier` component adds an outer loop around the agentic loop: run until the model says done, verify, and if verification fails, feed feedback back as a user message. Three verifiers shipped:

- **TestRunner**: runs a shell command, parses exit code + output.
- **LLM-as-judge** (`verifier/llmjudge.go`): calls a cheap model (default: Haiku) with a structured prompt, parses a JSON verdict `{"passed": bool, "feedback": string}`.
- **Composite**: chains multiple verifiers.

### Context management

Three strategies for managing message history as it approaches token limits:

- **Sliding window**: drops oldest messages beyond token budget.
- **LLM-summarise**: summarises old turns into a condensed message using a separate (cheaper) model call.
- **Offload-to-file**: writes full tool results to workspace files, replaces in-context with a pointer.

Token estimation accounts for per-message overhead (4 tokens), per-block overhead (3 tokens), tool-related metadata, system prompt tokens, and tool definition tokens.

### Sub-agent spawning

The `spawn_agent` built-in tool creates a fresh `AgenticLoop` with its own message history, running synchronously as a tool call. The sub-agent reuses the parent's provider, executor, and tools (except `spawn_agent` itself, preventing infinite recursion). Uses a `NullTransport` and `captureTransport` for output extraction. Max turns capped at 20, defaults to 10.

### Stall detection

The `stallDetector` (`core/stall.go`) tracks consecutive identical tool calls and consecutive failures. The loop terminates with `"stalled"` after 3 repeated identical calls (same name + same input) or `"tool_failures"` after 5 consecutive failures.

### Budget enforcement

The loop checks token budgets before each turn and again after tool results are appended. `TokenTracker` enforces token budget limits configured in `RunConfig`. Cost estimation (dollar amounts, pricing tables) was deliberately excluded from the harness — pricing is a control plane concern. The harness retains token counting for budget enforcement only.

### Follow-up loop

After the main agentic loop completes, an optional follow-up grace period allows the control plane to send additional user messages. Configurable via `RunConfig.FollowUpGrace` (bounded to <= 3600s).

---

## Transport

**Stdio** (`transport/stdio.go`): NDJSON to stdout, reads stdin. Used for local/interactive development.

**gRPC** (`transport/grpc.go`): Bidi streaming client implementing the `Transport` interface. Proto definitions in `proto/harness/v1/harness.proto`, generated with Buf. The harness connects to the control plane, not the other way around. Complex nested `RunConfig` proto translation with proper secret scrubbing.

**Null** (`transport/null.go`): No-op transport used by sub-agents.

The K8s job subcommand (`cmd/stirrup/cmd/job.go`, invoked as `stirrup job`) dials the control plane at `CONTROL_PLANE_ADDR` via gRPC, emits a "ready" event, blocks until a `task_assignment` arrives, then runs the loop over the pre-established transport.

### MCP client

The MCP client (`mcp/client.go`) connects to remote MCP servers via Streamable HTTP transport (JSON-RPC 2.0 over HTTP POST). Remote-only by design — no stdio subprocess management. Tool names are prefixed as `mcp_{serverName}_{toolName}`. MCP tools default to `sideEffects: true`. Connection failures log a warning and skip the unavailable server's tools rather than failing the entire run.

---

## Security

### Foundations shipped in V1

- **SecretStore**: resolves `secret://` references. Backends: environment variables, files, AWS SSM (`secret://ssm:///param-name`). `AutoSecretStore` routes by scheme, only initialising SSM client when config refs require it. API keys never stored in `RunConfig`.
- **RunConfig validation**: hard invariants enforced before any component is constructed. Read-only modes must provide an explicit tool list excluding `write_file` and `run_command`. `maxTurns` bounded [1, 100], `timeout` bounded [1, 3600], `FollowUpGrace` <= 3600, `MaxCostBudget` <= $100, `MaxTokenBudget` <= 50M.
- **Path traversal prevention**: symlink-aware containment in all 3 executors. Search tool calls `ResolvePath` before constructing commands. Tested with `../../../`, symlink escapes, absolute paths.
- **Command injection**: `shellQuote()` in search_files with explicit tests.
- **Web fetch SSRF protection**: private IP blocking (RFC 1918, loopback, link-local, multicast), scheme whitelisting (http/https only), DNS resolution validation, 100KB response cap.
- **Environment filtering**: command execution allowlists 27 safe env vars; blocks all API keys and cloud credentials.
- **Log scrubbing**: 7-pattern regex scrubber (Anthropic keys, OpenAI keys, GitHub PATs/app tokens, AWS access keys, Bearer tokens including JWTs, PEM keys, secret:// refs). Applied via `ScrubHandler` wrapper around `slog.Handler` — makes secret leakage through logs structurally impossible.
- **Input validation**: JSON Schema validation on all tool inputs, with prototype pollution protection (`__proto__`/`constructor` keys stripped). The current implementation uses `santhosh-tekuri/jsonschema` for Draft 2020-12 support with external schema loading disabled.
- **Untrusted context**: dynamic context wrapped in `<untrusted_context>` tags with model instructions to treat as data.
- **RunConfig.Redact()**: strips secret references before trace/recording persistence.
- **HTTP client timeouts**: all provider adapters and MCP client use explicit HTTP clients (120s streaming, 30s MCP). Never `http.DefaultClient`.
- **Container hardening**: `--cap-drop ALL`, `--security-opt no-new-privileges`, `--network none` by default.

### V1 security fixes applied

These were identified during code review and fixed before merge:

1. **Search tool path traversal** (0.1) — `ResolvePath` called before constructing grep commands.
2. **Web fetch SSRF** (0.2) — private IP/reserved range blocking with DNS resolution pinning.
3. **HTTP client timeouts** (0.3) — explicit timeouts on all HTTP clients.
4. **Log scrubber regex gaps** (0.4) — broadened patterns for JWTs, base64 tokens, OpenAI keys.
5. **Environment variable leakage** (0.5) — 27-key allowlist in `filteredCommandEnv()`.
6. **API executor URL encoding** (0.6) — `url.PathEscape` on path parameters.
7. **RunConfig validation bounds** (0.7) — upper bounds on grace period, cost budget, and token budget.

### JSON Schema validator

The input validator (`security/inputvalidator.go`) uses `santhosh-tekuri/jsonschema` v6 for JSON Schema Draft 2020-12 support, including inline `$ref`/`$defs`, `oneOf`/`anyOf`/`allOf`, `format`, `enum`, `pattern`, numeric bounds, and array item validation. External schema loading is disabled to prevent local file reads or SSRF through untrusted MCP schemas, and dangerous keys such as `__proto__` and `constructor` are stripped before validation.

### Post-V1 hardening

Post-V1 security hardening is tracked in GitHub Issues, prioritised by deployment milestone. See issues labelled `security` for the full roadmap, covering: container sandbox hardening, MCP server trust model, prompt injection defense in depth, network egress hardening, gRPC transport security, observability security, eval framework security, dependency supply chain, and DoS resilience.

---

## Observability

### Structured logging

The harness uses `log/slog` (stdlib) with a custom `ScrubHandler` that wraps any `slog.Handler` and runs `security.Scrub()` on all string attribute values before delegation. JSON logs to stderr with `runId` on every line.

### Distributed tracing (OpenTelemetry)

The OTel trace emitter (`trace/otel.go`) creates a root `run` span with child spans for turns, tool calls, provider streaming, context compaction, verification, permission checks, and git operations. Exported via OTLP/gRPC (default endpoint: `localhost:4317`). The JSONL emitter is used for local development.

### Metrics

12 OTel counters (`stirrup.harness.runs`, `.turns`, `.tokens.input`, `.tokens.output`, `.tool_calls`, `.tool_errors`, `.provider.requests`, `.provider.errors`, `.context.compactions`, `.security.events`, `.verification.attempts`, `.stalls`), 5 histograms (run/turn/tool-call duration, provider latency, TTFB), and 1 UpDownCounter (context token estimate). `NewNoopMetrics()` provides a zero-cost no-op when metrics are disabled.

### Health probes

The agentic loop emits `heartbeat` events every 30 seconds during execution. For K8s jobs, a file-based liveness probe writes `/tmp/healthy` after the ready event and removes it on shutdown.

---

## Eval framework

### Components

- **ReplayProvider** (`provider/replay.go`) — replays recorded model outputs as stream events. Thread-safe atomic turn counter. No API calls.
- **ReplayExecutor** (`executor/replay.go`) — replays recorded tool call outputs keyed by `(toolName, canonicalInput)`. Tracks writes for assertion.
- **Judge system** (`eval/judge/`) — evaluates criteria against workspace state. Supports `test-command` (shell exit code), `file-exists`, `file-contains` (regex), `composite` (`all`/`any` require). Path traversal prevention. `diff-review` (LLM judge) is stubbed.
- **Suite runner** (`eval/runner/`) — orchestrates execution: validates suite, creates temp workspaces, clones repos, invokes harness binary, parses JSONL traces, applies judges. Supports `DryRun` mode.
- **Replay evaluator** (`eval/runner/replay.go`) — re-evaluates recorded runs through judges without re-running the harness.
- **Comparison reporter** (`eval/reporter/`) — diffs two `SuiteResult` sets, flags regressions (pass->fail) and improvements (fail->pass), computes turn deltas and aggregate metrics.

### CLI

```
eval run        --suite <path>                     # Run an eval suite
eval compare    --current <path> --baseline <path> # Compare two results
eval baseline   --lakehouse <path>                 # Pull production metrics
eval mine-failures --lakehouse <path>              # Mine failures into eval tasks
eval drift      --lakehouse <path> --window 7d     # Detect metric drift
eval compare-to-production --results <path> --lakehouse <path>
```

### Lakehouse

The `TraceLakehouse` interface (`types/lakehouse.go`) abstracts storage and querying of production run data. A file-based adapter (`eval/lakehouse/filestore.go`) is implemented for development and CI. Production adapters (Postgres, BigQuery) depend on control plane choices and are deferred.

### CI integration

GitHub Actions at `.github/workflows/ci.yml`:

- **verify** job: `go test` for types, harness, and eval modules; builds harness and eval binaries (every push and PR).
- **eval-gate** job: builds binaries, runs eval suites from `eval/suites/`, compares against baselines in `eval/baselines/`, uploads results as artifacts (main branch push, after verify passes).
- **publish-container** job: builds and pushes Docker image to `ghcr.io/rxbynerd/stirrup` (main branch push, after verify passes).

---

## What was deferred

| Item | Reason |
|---|---|
| Tier 3 microVM executor (Firecracker/E2B) | Not needed until multi-tenant SaaS deployment. `Executor` interface means drop-in addition. |
| `diff-review` judge (LLM judge for eval) | Requires LLM judge integration; stubbed in eval framework. |
| First mined eval suite (10-20 tasks from real PRs) | CI infrastructure is in place; needs suite files mined from a real repo. |
| Postgres/BigQuery lakehouse adapter | File-based adapter covers dev/CI. Production adapter depends on control plane choices. |
| End-to-end smoke test with real provider | Would catch wire-format regressions but requires API key in CI. |
| Cost estimation / pricing tables | Deliberately excluded. Pricing is a control plane concern. Harness retains `TokenTracker` for budget enforcement only. |
| Scheduling / toil triggering | Control plane responsibility. Harness supports the toil mode; the control plane decides when to dispatch jobs. |
| A2A (Agent-to-Agent Protocol) adapter | Control plane concern. Harness speaks gRPC; control plane translates for external interop. |
| Container image supply chain, MCP sandboxing, gRPC mutual auth | Post-V1 hardening. Tracked in GitHub Issues. |

---

## External dependencies

External dependency families are deliberately small and justified:

| Dependency | Rationale |
|---|---|
| `github.com/spf13/cobra` | CLI framework for subcommands, flag parsing, and help generation. |
| `github.com/santhosh-tekuri/jsonschema/v6` | Full JSON Schema validation for built-in and MCP tool inputs. |
| `aws-sdk-go-v2` | Bedrock provider (IAM SigV4) and SSM SecretStore. SigV4 auth is too complex to hand-roll. |
| `google.golang.org/grpc` + `google.golang.org/protobuf` | gRPC transport. The reference Go gRPC implementation. |
| `go.opentelemetry.io/otel` + OTLP exporter | OpenTelemetry trace emitter. The reference OTel SDK. |

The container executor uses the Docker Engine REST API directly over a Unix socket rather than the official Docker Go SDK, avoiding its massive transitive dependency tree (moby, containerd, OCI specs).

---

## By the numbers

| Metric | Value |
|---|---|
| Go modules | 4 (types, harness, eval, gen) |
| Go packages | 30 |
| Packages with tests | 27 |
| Test functions | 701 |
| All passing | Yes |
| External dep families | 5 |
| TODOs in production code | 1 (eval runner concurrency is currently sequential) |
| Components | 12/12 implemented |

---

## Key design decisions

1. **The core loop is a pure function of its interfaces.** No imports from concrete implementations, no environment variable reads, no direct filesystem access. This is what makes every component independently swappable and testable.

2. **Edit strategy is a first-class component.** Research showed edit format is one of the highest-leverage harness decisions. The multi-strategy approach with automatic fallback proved the architecture — different models produce different quality diffs, and fallback handles graceful degradation.

3. **Verification closes the feedback loop.** The model doesn't know if code compiles or tests pass. The `Verifier` component adds an outer loop that feeds failure feedback back into the agentic loop.

4. **The sandbox tier is a RunConfig property.** Different modes and trust levels get fundamentally different isolation. The tools call `executor.ReadFile()` without knowing whether they're hitting the GitHub API, a local filesystem, or a Docker container.

5. **The harness is a job, not a server.** Started by a control plane, connects outbound via gRPC, streams events, exits on completion. This replaced the Ruby prototype's Sinatra/Puma/Faye server stack.

6. **Cost estimation is a control plane concern.** The harness tracks tokens for budget enforcement but does not maintain pricing tables or compute dollar costs. Pricing changes frequently and is a fleet-wide concern.

7. **Minimal dependencies are worth hand-rolling HTTP clients.** For a security-sensitive harness that holds API keys and executes code, owning the HTTP client code (a few hundred lines per provider) is preferable to importing SDK dependency trees.

---

## Resolved design decisions

- **State persistence**: none in the harness. Conversation history is ephemeral. Persistence is the control plane's responsibility.
- **Git integration**: deterministic strategy shipped (harness manages branches/commits, not the model). Model-driven git was considered and rejected for V1.
- **Communication model**: gRPC bidi streaming between harness and control plane. Protobuf contract replaces the WebSocket JSON protocol from the Ruby prototype.
- **Interoperability**: the harness speaks gRPC. A2A compatibility for external agents is the control plane's responsibility via adapter pattern.
- **MCP**: supported from the start. Remote-only (Streamable HTTP), no stdio subprocess management.

---

## Issue #43 — GuardRail

The 13th swappable component, `GuardRail`, adds an LLM-based safety classifier at three points in the agentic loop: **PreTurn** (untrusted text — tool outputs, dynamic context, the initial prompt — before it enters context), **PreTool** (model-proposed tool calls before dispatch), and **PostTurn** (assistant text before it leaves the loop). Two adapters ship: `granite-guardian` (IBM Granite Guardian 4.1-8B served via vLLM over OpenAI-compatible chat completions) and `cloud-judge` (reuses an existing `ProviderAdapter` such as Anthropic Haiku for deployments without GPU access). A `composite` primitive lets operators layer additional classifiers (e.g. a fast-path Llama Prompt Guard 2 in front of Granite Guardian) without modifying the harness; no fast-path adapter ships natively. The component is opt-in: the default is `none`, the call sites are no-ops, and zero behaviour changes from the pre-#43 path. Operator walkthrough: [`docs/guardrails.md`](docs/guardrails.md). The classifier is a probability reducer, not an authoriser — a `VerdictAllow` from the guard does **not** override a deny from the [Cedar policy engine](docs/safety-rings.md); the two questions are different and both must agree to allow.
