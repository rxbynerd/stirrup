# stirrup

A coding agent harness. Go monorepo with 12 swappable components that can be composed via `RunConfig`. See `VERSION1.md` for the Version 1 architecture summary.

## Project Structure

```text
stirrup/
  go.work                    # Go workspace: types, harness, eval, gen modules
  buf.yaml                   # Buf v2 config for proto linting/breaking
  buf.gen.yaml               # Buf code generation config (protobuf + gRPC)
  proto/harness/v1/          # Protobuf definitions for gRPC transport
  gen/                       # Generated Go code from proto (separate module)
  types/                     # Shared type definitions
  harness/                   # The harness binary and components
    cmd/stirrup/             # Unified CLI entrypoint (cobra: harness + job subcommands)
      main.go
      cmd/root.go
      cmd/harness.go
      cmd/job.go
    harnessapi/              # Public embedding API
    internal/
      core/                  # AgenticLoop, factory, token tracking, sub-agent spawning, stall detection
      credential/            # Cross-cloud credential federation
      provider/              # ProviderAdapter: Anthropic, Bedrock, OpenAI-compatible
      router/                # ModelRouter: static, per-mode, dynamic
      prompt/                # PromptBuilder: per-mode templates, composed fragments, overrides
      context/               # ContextStrategy: sliding window, summarise, offload-to-file
      tool/                  # ToolRegistry + built-in tools, including spawn_agent
      executor/              # Executor: local, container (Docker/Podman), API (GitHub), replay
      edit/                  # EditStrategy: whole-file, search-replace, udiff, multi-strategy
      verifier/              # Verifier: none, test-runner, composite, llm-judge
      permission/            # PermissionPolicy: allow-all, deny-side-effects, ask-upstream
      git/                   # GitStrategy: none, deterministic
      transport/             # Transport: stdio, gRPC bidi streaming, null (sub-agents)
      trace/                 # TraceEmitter: JSONL, OpenTelemetry (OTLP/gRPC)
      observability/         # Structured logging (slog + ScrubHandler), OTel metrics
      health/                # File-based K8s liveness probes
      security/              # SecretStore, LogScrubber, input validation
      mcp/                   # MCP client: remote tool discovery via Streamable HTTP
  eval/                      # Eval framework
    cmd/eval/main.go         # CLI: run, compare, baseline, mine-failures, drift, compare-to-production
    judge/                   # test-command, file-exists, file-contains, composite
    runner/                  # Suite runner (live + replay) and replay evaluator
    reporter/                # Comparison reporter and text formatting
    lakehouse/               # TraceLakehouse adapters: file-based FileStore
    suites/                  # Eval suite definitions (JSON)
    baselines/               # Stored baseline results for CI comparison
```

## Running

```sh
go build -o stirrup ./harness/cmd/stirrup
./stirrup harness --prompt "Your task here"
```

Or directly:

```sh
go run ./harness/cmd/stirrup harness --prompt "Your task here"
```

Requires `ANTHROPIC_API_KEY` environment variable for the default Anthropic provider.

### CLI Flags (`stirrup harness`)

| Flag | Default | Description |
|---|---|---|
| `--prompt` | (required) | User prompt, also accepted as a positional argument |
| `--mode`, `-m` | `execution` | Run mode: execution, planning, review, research, toil |
| `--model` | `claude-sonnet-4-6` | Model to use |
| `--provider` | `anthropic` | Provider type: anthropic, bedrock, openai-compatible |
| `--api-key-ref` | `secret://ANTHROPIC_API_KEY` | Secret reference for API key |
| `--workspace`, `-w` | current directory | Workspace directory |
| `--max-turns` | `20` | Maximum agentic loop turns |
| `--timeout` | `600` | Wall-clock timeout in seconds |
| `--trace` | (none) | Path to JSONL trace file |
| `--transport` | `stdio` | Transport type: stdio, grpc |
| `--transport-addr` | (none) | gRPC target address, required when transport is grpc |
| `--followup-grace` | `0` | Seconds to keep gRPC open for follow-ups (env: `STIRRUP_FOLLOWUP_GRACE`) |
| `--log-level` | `info` | Log level: debug, info, warn, error |

## Architecture

12 swappable components, all interface-based:

1. **ProviderAdapter** - streams completions from LLMs (Anthropic, Bedrock, OpenAI-compatible)
2. **ModelRouter** - selects provider+model per turn (static, per-mode, dynamic)
3. **PromptBuilder** - assembles system prompt (default per-mode templates, composed fragments, overrides)
4. **ContextStrategy** - manages message history (sliding window, summarise, offload-to-file)
5. **ToolRegistry** - resolves and dispatches tools (7 built-in runtime tools + MCP remote tools)
6. **Executor** - sandboxed file I/O and command execution (local, container, API, replay)
7. **EditStrategy** - how file changes are applied (whole-file, search-replace, udiff, multi-strategy)
8. **Verifier** - validates run output (none, test-runner, composite, llm-judge)
9. **PermissionPolicy** - gates side-effecting tools (allow-all, deny-side-effects, ask-upstream)
10. **Transport** - streams events to/from control plane (stdio, gRPC bidi streaming, null)
11. **GitStrategy** - manages branches/commits (none, deterministic)
12. **TraceEmitter** - records telemetry (JSONL, OpenTelemetry)

The core loop is a pure function of its interfaces. All dependencies are injected via the factory (`core.BuildLoop` / `core.BuildLoopWithTransport`), which constructs components from a `RunConfig`.

### Provider Adapters

- **Anthropic** (`provider/anthropic.go`) - SSE streaming via `net/http` + `bufio.Scanner`. Hand-rolled, no SDK dependency.
- **Bedrock** (`provider/bedrock.go`) - AWS ConverseStream API via `aws-sdk-go-v2`. Auth is IAM, with optional credential federation.
- **OpenAI-compatible** (`provider/openai.go`) - OpenAI chat completions streaming. Works with OpenAI, LiteLLM, Azure OpenAI, vLLM, Ollama via configurable `baseURL`.

### Container Executor

The container executor (`executor/container.go`, `executor/container_api.go`) uses the Docker Engine REST API directly over a Unix socket, with zero Docker SDK dependency. Docker and Podman are both supported through the Engine API.

Container lifecycle: created at executor init with `sleep infinity`, all operations go through exec or archive API, destroyed on `Close()`. Hardened with `CapDrop: ALL`, `no-new-privileges`, and `NetworkMode: none` by default. API keys never enter the container.

### MCP Client

The MCP client (`mcp/client.go`) connects to remote MCP servers via Streamable HTTP transport (JSON-RPC 2.0 over HTTP POST). Remote-only by design. Tool names are prefixed as `mcp_{serverName}_{toolName}` to avoid collisions.

### gRPC Transport

The gRPC transport (`transport/grpc.go`) implements the `Transport` interface as an outbound bidi streaming client. Proto definitions are in `proto/harness/v1/harness.proto`, generated with Buf (`buf generate`). The harness connects to the control plane, not the other way around.

### API Executor

The API executor (`executor/api.go`) implements the `Executor` interface for read-only modes backed by the GitHub REST API Contents endpoint. `ReadFile` and `ListDirectory` work; `WriteFile` and `Exec` return errors.

### K8s Job Entrypoint

`stirrup job` dials the control plane at `CONTROL_PLANE_ADDR` via gRPC, emits a `ready` event, blocks until a `task_assignment` arrives, then runs the agentic loop over the pre-established transport using `BuildLoopWithTransport`.

### Eval Framework

- **Judge** (`eval/judge/`) - evaluates `EvalJudge` criteria against workspace state. Supports `test-command`, `file-exists`, `file-contains`, and `composite`.
- **Runner** (`eval/runner/`) - loads `EvalSuite` JSON, creates temp workspaces, optionally clones repos, invokes the harness binary, parses JSONL traces, and applies judges. Task execution is currently sequential.
- **Replay evaluator** (`eval/runner/replay.go`) - re-evaluates recorded runs through judges without re-running the harness.
- **Reporter** (`eval/reporter/`) - diffs two `SuiteResult` sets and formats human-readable reports.
- **Lakehouse** (`eval/lakehouse/filestore.go`) - file-backed `TraceLakehouse` adapter for production trace metrics and recordings.

## Security Foundations

- **SecretStore**: resolves `secret://` references from env vars, files, and AWS SSM. API keys are not stored in `RunConfig`.
- **LogScrubber**: regex-based redaction of 7 secret patterns in log/trace output.
- **Input validation**: JSON Schema Draft 2020-12 validation via `santhosh-tekuri/jsonschema`, with external schema loading disabled and prototype pollution keys stripped.
- **RunConfig validation**: hard security invariants for read-only modes, bounded max turns/timeout, `FollowUpGrace <= 3600s`, `MaxCostBudget <= $100`, and `MaxTokenBudget <= 50M`.
- **HTTP client timeouts**: provider adapters and MCP/web fetch clients use explicit HTTP clients with timeouts.
- **Environment filtering**: command execution allowlists safe env vars and blocks API keys/cloud credentials.
- **Untrusted context delimiters**: dynamic context wrapped in `<untrusted_context>` tags.
- **RunConfig.Redact()**: strips secret references before trace persistence.
- **Stall detection**: repeated identical tool calls (3x) and consecutive failures (5x) terminate the loop.

## Key Constants

- `MaxTurns`: 20 by default, hard-capped at 100 by `ValidateRunConfig`
- Default model: `claude-sonnet-4-6`
- `max_tokens: 64000`, `temperature: 0.1`
- File size limit: 10MB (read/write)
- Command output cap: 1MB
- Command timeout: 30s default, 5min max
- Follow-up grace cap: 3600s
- Token budget cap: 50M
- Cost budget cap: $100

## Development

A `Justfile` is provided for common tasks:

```sh
just              # build + test
just build        # build stirrup + eval binaries
just test         # go test ./harness/... ./types/... ./eval/...
just lint         # golangci-lint run ./harness/... ./types/... ./eval/...
just proto        # buf generate
just buf-lint     # buf lint
just docker       # build stirrup Docker image
just clean        # remove built binaries
```

Or directly:

```sh
go test ./harness/... ./types/... ./eval/...
go build ./harness/... ./types/... ./eval/...
buf generate
buf lint
```

### Known Issue: gopls False Positives

The LSP (gopls) frequently reports false positive diagnostics due to the `go.work` workspace module resolution. Always verify with `go build` and `go test`; if they pass, LSP errors referencing workspace packages are likely spurious.

## External Dependencies Rationale

The project follows a minimal-dependency philosophy. Provider adapters and the container executor use hand-rolled HTTP clients against documented REST APIs rather than pulling in large SDK dependency trees.

Exceptions where external deps are accepted:

- `github.com/spf13/cobra` for CLI framework
- `github.com/santhosh-tekuri/jsonschema/v6` for full JSON Schema validation
- `aws-sdk-go-v2` for Bedrock, STS, and SSM SecretStore
- `google.golang.org/grpc` + `google.golang.org/protobuf` for gRPC transport
- `go.opentelemetry.io/otel` + OTLP exporter for OpenTelemetry trace and metrics
