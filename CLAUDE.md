# stirrup

A coding agent harness. Go monorepo with 12 swappable components that can be composed via RunConfig. See VERSION1.md for the full design document.

## Project Structure

```
stirrup/
  go.work                    # Go workspace: types, harness, eval, gen modules
  buf.yaml                   # Buf v2 config for proto linting/breaking
  buf.gen.yaml               # Buf code generation config (protobuf + gRPC)
  proto/harness/v1/          # Protobuf definitions for gRPC transport
  gen/                       # Generated Go code from proto (separate module)
  types/                     # Shared type definitions (zero dependencies)
  harness/                   # The harness binary
    cmd/harness/main.go      # CLI entrypoint
    cmd/job/main.go          # K8s job entrypoint (gRPC to control plane)
    internal/
      core/                  # AgenticLoop, factory, token tracking, sub-agent spawning, stall detection
      provider/              # ProviderAdapter: Anthropic, Bedrock, OpenAI-compatible
      router/                # ModelRouter: static, per-mode, dynamic
      prompt/                # PromptBuilder: per-mode templates
      context/               # ContextStrategy: sliding window, summarise, offload
      tool/                  # ToolRegistry + built-in tools (incl. spawn_agent)
      executor/              # Executor: local, container (Docker/Podman), API (GitHub)
      edit/                  # EditStrategy: whole-file, search-replace, udiff, multi-strategy
      verifier/              # Verifier: none, test-runner, composite, llm-judge
      permission/            # PermissionPolicy: allow-all, deny-side-effects, ask-upstream
      git/                   # GitStrategy: none, deterministic
      transport/             # Transport: stdio, gRPC bidi streaming, null (sub-agents)
      trace/                 # TraceEmitter: JSONL, OpenTelemetry (OTLP/gRPC)
      observability/         # Structured logging (slog + ScrubHandler), OTel metrics
      health/                # File-based K8s liveness probes
      security/              # SecretStore (env, file, AWS SSM), LogScrubber, input validation
      mcp/                   # MCP client: remote tool discovery via Streamable HTTP
  eval/                      # Eval framework
    cmd/eval/main.go         # CLI entrypoint (run, compare, baseline, mine-failures, drift, compare-to-production)
    judge/                   # Judge system: test-command, file-exists, file-contains, composite
    runner/                  # Suite runner (live + replay) and replay evaluator
    reporter/                # Comparison reporter: diffs two SuiteResults, text formatting
    lakehouse/               # TraceLakehouse adapters: file-based (FileStore)
    suites/                  # Eval suite definitions (JSON)
    baselines/               # Stored baseline results for CI comparison
```

## Running

```sh
go build -o stirrup-harness ./harness/cmd/harness
./stirrup-harness -prompt "Your task here"
```

Or directly:
```sh
go run ./harness/cmd/harness -prompt "Your task here"
```

Requires `ANTHROPIC_API_KEY` environment variable.

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-prompt` | (required) | User prompt |
| `-mode` | `execution` | Run mode: execution, planning, review, research, toil |
| `-model` | `claude-sonnet-4-6` | Model to use |
| `-provider` | `anthropic` | Provider type |
| `-api-key-ref` | `secret://ANTHROPIC_API_KEY` | Secret reference for API key |
| `-workspace` | current directory | Workspace directory |
| `-max-turns` | `20` | Maximum agentic loop turns |
| `-timeout` | `600` | Wall-clock timeout in seconds |
| `-trace` | (none) | Path to JSONL trace file |
| `-log-level` | `info` | Log level: debug, info, warn, error |
| `-transport` | `stdio` | Transport type: stdio, grpc |
| `-transport-addr` | (none) | gRPC target address (required when transport=grpc) |

### Eval CLI

```sh
go build -o stirrup-eval ./eval/cmd/eval

# Run an eval suite
./stirrup-eval run --suite path/to/suite.json --output results/ [--harness path/to/harness] [--dry-run]

# Compare two eval results
./stirrup-eval compare --current results/result.json --baseline baseline/result.json

# Pull production metrics as a baseline
./stirrup-eval baseline --lakehouse path/to/lakehouse [--after 2026-03-01] [--mode execution] [--output metrics.json]

# Mine failures into eval tasks
./stirrup-eval mine-failures --lakehouse path/to/lakehouse [--after 2026-03-01] [--limit 20] [--output suite.json]

# Detect metric drift between time windows
./stirrup-eval drift --lakehouse path/to/lakehouse --window 7d [--compare-window 7d] [--mode execution]

# Compare eval results against production metrics
./stirrup-eval compare-to-production --results results/result.json --lakehouse path/to/lakehouse [--after 2026-03-01] [--experiment-id exp1]
```

## Architecture

12 swappable components, all interface-based:

1. **ProviderAdapter** — streams completions from LLMs (Anthropic, Bedrock, OpenAI-compatible)
2. **ModelRouter** — selects provider+model per turn (static, per-mode, dynamic)
3. **PromptBuilder** — assembles system prompt (default per-mode templates, composed)
4. **ContextStrategy** — manages message history (sliding window, summarise, offload-to-file)
5. **ToolRegistry** — resolves and dispatches tools (7 built-in tools + MCP remote tools)
6. **Executor** — sandboxed file I/O and command execution (local, container, API)
7. **EditStrategy** — how file changes are applied (whole-file, search-replace, udiff, multi-strategy)
8. **Verifier** — validates run output (none, test-runner, composite, llm-judge)
9. **PermissionPolicy** — gates side-effecting tools (allow-all, deny-side-effects, ask-upstream)
10. **Transport** — streams events to/from control plane (stdio, gRPC bidi streaming, null for sub-agents)
11. **GitStrategy** — manages branches/commits (none, deterministic)
12. **TraceEmitter** — records telemetry (JSONL, OpenTelemetry)

The core loop is a pure function of its interfaces. All dependencies are injected via the factory (`core.BuildLoop` / `core.BuildLoopWithTransport`), which constructs components from a `RunConfig`.

### Provider adapters

- **Anthropic** (`provider/anthropic.go`) — SSE streaming via `net/http` + `bufio.Scanner`. Hand-rolled, no SDK dependency.
- **Bedrock** (`provider/bedrock.go`) — AWS ConverseStream API via `aws-sdk-go-v2`. Translates between internal types and Bedrock's union-type wire format. Auth is IAM (not API key); uses `config.LoadDefaultConfig()`.
- **OpenAI-compatible** (`provider/openai.go`) — OpenAI chat completions streaming. Works with OpenAI, LiteLLM, Azure OpenAI, vLLM, Ollama via configurable `baseURL`.

### Container executor

The container executor (`executor/container.go`, `executor/container_api.go`) uses the Docker Engine REST API directly over a Unix socket, with zero external dependencies. This is a deliberate design choice: the official Docker Go SDK (`github.com/docker/docker`) has a massive transitive dependency tree (moby, containerd, OCI specs, etc.) which conflicts with the project's minimal-dependency philosophy.

Both Docker and Podman implement the same Engine API, so the executor works transparently with either runtime. Socket auto-detection order: `DOCKER_HOST` env var, `/var/run/docker.sock`, `$XDG_RUNTIME_DIR/podman/podman.sock`, `/var/run/podman/podman.sock`.

Container lifecycle: created at executor init with `sleep infinity`, all operations go through exec or archive API, destroyed on `Close()`. Hardened with `CapDrop: ALL`, `no-new-privileges`, `NetworkMode: none` by default. API keys never enter the container.

### MCP client

The MCP client (`mcp/client.go`) connects to remote MCP servers via Streamable HTTP transport (JSON-RPC 2.0 over HTTP POST). Remote-only by design — no stdio subprocess management. Tool names are prefixed as `mcp_{serverName}_{toolName}` to avoid collisions.

### gRPC transport

The gRPC transport (`transport/grpc.go`) implements the Transport interface as an outbound bidi streaming client. Proto definitions are in `proto/harness/v1/harness.proto`, generated with Buf (`buf generate`). The harness connects to the control plane, not the other way around.

### API executor

The API executor (`executor/api.go`) implements the Executor interface for read-only modes backed by the GitHub REST API (Contents endpoint). `ReadFile` and `ListDirectory` work; `WriteFile` and `Exec` return errors. Uses stdlib `net/http` only, consistent with the minimal-dependency philosophy.

### LLM-as-Judge verifier

The LLM judge verifier (`verifier/llmjudge.go`) evaluates conversation output against natural-language criteria by calling a cheap model (default: Haiku). It streams a structured prompt, collects the response, and parses a JSON verdict `{"passed": bool, "feedback": string}`. Malformed responses are treated as failures with the raw response preserved in details.

### OpenTelemetry trace emitter

The OTel trace emitter (`trace/otel.go`) implements TraceEmitter using real OTel spans exported via OTLP/gRPC. Creates a root `run` span with child spans for turns, tool calls, provider streaming, context compaction, verification, permission checks, and git operations. Default endpoint: `localhost:4317`.

### Structured logging

The harness uses `log/slog` (stdlib) with a custom `ScrubHandler` (`observability/logger.go`) that wraps any `slog.Handler` and runs `security.Scrub()` on all string attribute values before delegation. This makes secret leakage through logs structurally impossible. JSON logs are written to stderr with a `runId` field on every line. Log level is configurable via `-log-level` flag or `RunConfig.LogLevel`.

### OTel metrics

The `observability/metrics.go` package emits OTel metrics via OTLP/gRPC alongside tracing. Instruments: 12 counters (`stirrup.harness.runs`, `.turns`, `.tokens.input`, `.tokens.output`, `.tool_calls`, `.tool_errors`, `.provider.requests`, `.provider.errors`, `.context.compactions`, `.security.events`, `.verification.attempts`, `.stalls`), 5 histograms (run/turn/tool-call duration, provider latency, TTFB), and 1 UpDownCounter (context token estimate). All instruments use standard attributes (`run.mode`, `provider.type`, `tool.name`, etc.). `NewNoopMetrics()` provides a zero-cost no-op when metrics are disabled.

### Heartbeat and health probes

The agentic loop emits `heartbeat` events on the transport every 30 seconds during execution. For K8s jobs, a file-based liveness probe (`health/probe.go`) writes `/tmp/healthy` after the ready event and removes it on shutdown.

### K8s job entrypoint

`cmd/job/main.go` is the K8s job binary. It dials the control plane at `CONTROL_PLANE_ADDR` via gRPC, emits a "ready" event, blocks until a `task_assignment` arrives, then runs the agentic loop over the pre-established transport using `BuildLoopWithTransport`.

### Protobuf / Buf toolchain

Proto files are managed with [Buf](https://buf.build). To regenerate after proto changes:
```sh
buf generate
```
Generated code lives in `gen/` (a separate Go module in the workspace). Buf config: `buf.yaml` (lint/breaking rules) and `buf.gen.yaml` (code generation with `buf.build/protocolbuffers/go` and `buf.build/grpc/go` remote plugins).

### Replay doubles (for deterministic eval testing)

- **ReplayProvider** (`provider/replay.go`) — implements `ProviderAdapter` by replaying recorded `TurnRecord.ModelOutput` as stream events. Atomic turn counter, no API calls.
- **ReplayExecutor** (`executor/replay.go`) — implements `Executor` by replaying recorded tool call outputs indexed by `(toolName, canonicalInput)`. Tracks writes for assertion via `Writes()`.

### Eval framework

- **Judge** (`eval/judge/`) — evaluates `EvalJudge` criteria against workspace state. Supports `test-command` (shell exit code), `file-exists`, `file-contains` (regex), `composite` (`all`/`any`), and `diff-review` (stub). Path traversal prevention on all workspace-relative paths.
- **Runner** (`eval/runner/`) — orchestrates suite execution: loads `EvalSuite` from JSON, creates temp workspaces, optionally clones repos at specific refs, invokes the harness binary, parses JSONL traces, applies judges. Sequential task execution. Errors per-task are captured without halting the suite.
- **Replay evaluator** (`eval/runner/replay.go`) — re-evaluates recorded runs through judges without re-running the harness. Useful for testing new judge criteria against existing recordings.
- **Reporter** (`eval/reporter/`) — diffs two `SuiteResult` sets. Detects regressions (pass→fail/error) and improvements (fail/error→pass). Computes turn deltas from `RunTrace`. Text formatter for human-readable output.
- **CLI** (`eval/cmd/eval/`) — `run`, `compare`, `baseline`, `mine-failures`, `drift`, `compare-to-production` subcommands.

### Lakehouse (production feedback loop)

- **TraceLakehouse interface** (`types/lakehouse.go`) — abstracts storage and querying of production run data. Any backing store (files, Postgres, BigQuery) can implement this interface.
- **FileStore adapter** (`eval/lakehouse/filestore.go`) — file-based TraceLakehouse implementation. Stores traces and recordings as JSON files. Supports filtering by time range, outcome, mode, model. Computes aggregate metrics with p50/p95 duration percentiles.
- **`eval baseline`** — pulls aggregate metrics from a lakehouse for use as experiment baselines.
- **`eval mine-failures`** — queries non-success recordings and generates EvalSuite JSON with test-command judges.
- **`eval drift`** — compares metrics between two adjacent time windows, flags significant changes (pass rate >5pp drop, turns >20% increase), exits 1 on drift.
- **`eval compare-to-production`** — loads eval results and production metrics from lakehouse, builds `LabVsProductionReport`, prints comparison table.

### Sub-agent spawning

The `spawn_agent` built-in tool (`tool/builtins/subagent.go`) creates a fresh `AgenticLoop` with its own message history, running synchronously as a tool call. The sub-agent reuses the parent's provider, executor, and tools (except `spawn_agent` itself — preventing infinite recursion). It uses a `NullTransport` (no streaming to control plane), `NoneVerifier`, `NoneGitStrategy`, and a `captureTransport` that records text deltas for output extraction. Max turns capped at 20, defaults to 10.

The `SubAgentSpawner` function type in `builtins` decouples the tool from the `core` package, avoiding circular imports. The factory provides the concrete closure.

### Multi-strategy edit fallback

The `MultiStrategy` (`edit/multi.go`) presents a unified `edit_file` tool that accepts fields from all three edit strategies. It routes based on which fields are present (diff → udiff, old_string → search-replace, content → whole-file) and automatically falls back to the next applicable strategy if the primary one fails.

### Loop stall detection

The `stallDetector` (`core/stall.go`) tracks consecutive identical tool calls and consecutive failures. The loop terminates with `"stalled"` after 3 repeated identical calls (same name + same input) or `"tool_failures"` after 5 consecutive failures.

## Security Foundations

- **SecretStore**: resolves `secret://` references (env vars, files, AWS SSM via `secret://ssm:///param-name`). `AutoSecretStore` routes by scheme, only initialising SSM client when config refs require it. API keys never stored in RunConfig.
- **LogScrubber**: regex-based redaction of 7 secret patterns in all log/trace output.
- **Input validation**: JSON Schema validation on all tool inputs. Prototype pollution protection.
- **RunConfig validation**: hard security invariants (read-only modes must use restrictive permissions, bounded maxTurns/timeout, FollowUpGrace ≤ 3600s, MaxTokenBudget ≤ 50M).
- **HTTP client timeouts**: all provider adapters (Anthropic, OpenAI, Bedrock) and MCP client use explicit HTTP clients with timeouts (120s streaming, 30s MCP) — never `http.DefaultClient`.
- **Environment filtering**: command execution allowlists 27 safe env vars; blocks all API keys and cloud credentials.
- **Untrusted context delimiters**: dynamic context wrapped in `<untrusted_context>` tags.
- **RunConfig.Redact()**: strips secret references before trace persistence.
- **Stall detection**: repeated identical tool calls (3x) and consecutive failures (5x) terminate the loop.

## Key Constants

- `MAX_TURNS = 20` (configurable via RunConfig/CLI)
- Default model: `claude-sonnet-4-6`
- `max_tokens: 64000`, `temperature: 0.1`
- File size limit: 10MB (read/write)
- Command output cap: 1MB
- Command timeout: 30s default, 5min max

## Development

A `Justfile` is provided for common tasks (requires [just](https://github.com/casey/just)):
```sh
just              # build + test (default)
just build        # build harness + job + eval binaries
just test         # go test ./harness/... ./types/... ./eval/...
just lint         # golangci-lint
just proto        # buf generate
just buf-lint     # buf lint
just docker       # build harness Docker image
just docker-job   # build job Docker image
just clean        # remove built binaries
```

Or directly:
```sh
go test ./harness/... ./eval/...    # Run all tests
go build ./harness/...   # Build all packages
buf generate             # Regenerate proto code (after editing .proto files)
buf lint                 # Lint proto files
```

### CI

GitHub Actions at `.github/workflows/ci.yml`:
- **verify** job: runs `go test` for types, harness, and eval modules, builds the harness and eval binaries (on every push and PR)
- **eval-gate** job: builds binaries, runs eval suites from `eval/suites/`, compares against baselines in `eval/baselines/`, uploads results as artifacts (on main branch push, after verify passes)
- **publish-container** job: builds and pushes Docker image to `ghcr.io/rxbynerd/stirrup` (on main branch push only, after verify passes)

### Known issue: gopls false positives

The LSP (gopls) frequently reports false positive diagnostics due to the `go.work` workspace module resolution. Errors referencing packages like `NewComposedPromptBuilder not declared by package prompt` or fields from other modules are almost always false. Always verify with `go build` and `go test` — if they pass, the LSP errors are spurious.

## External dependencies rationale

The project follows a minimal-dependency philosophy. Provider adapters and the container executor use hand-rolled HTTP clients against well-documented REST APIs rather than pulling in large SDK dependency trees. This is deliberate — for a security-sensitive harness that holds API keys and executes code, minimising the dependency surface is worth the cost of writing a few hundred lines of HTTP client code.

Exceptions where external deps are accepted:
- `aws-sdk-go-v2` for Bedrock and SSM SecretStore (IAM SigV4 auth is complex enough to justify)
- `google.golang.org/grpc` + `google.golang.org/protobuf` for gRPC transport (the reference Go gRPC implementation)
- `go.opentelemetry.io/otel` + OTLP exporter for OpenTelemetry trace emitter (the reference OTel SDK)

## Legacy Ruby Code

The original Ruby prototype (`stirrup.rb`, `server.rb`) is still in the repo root. It is superseded by the Go harness but kept for reference during the transition.
