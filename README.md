# stirrup

A coding agent harness for building composable AI agents. Built in Go with 12 swappable components that can be configured via `RunConfig`.

## Features

- **Modular architecture**: 12 interface-based components for LLM providers, routing, prompts, context, tools, execution, editing, verification, permissions, transport, git, and tracing
- **Unified CLI**: `stirrup harness` for local runs and `stirrup job` for Kubernetes control-plane jobs
- **Streaming support**: Anthropic SSE, AWS Bedrock ConverseStream, OpenAI-compatible chat completions, OpenAI Responses API, stdio transport, and gRPC bidi streaming
- **Security-first**: secret redaction, full JSON Schema validation, restrictive read-only modes, environment filtering, SSRF protection, path containment, HTTP timeouts, and loop stall detection
- **Multi-mode operation**: execution, planning, review, research, and toil modes
- **Token accounting**: input/output token tracking with configurable budget limits
- **Sub-agent spawning**: delegate subtasks to fresh agent instances with isolated context
- **Multi-strategy editing**: unified edit tool with fallback across unified diff, search-replace, and whole-file strategies
- **Eval framework**: live and replay suite execution, judges, comparison reporting, and file-backed production trace metrics

## Quick Start

### Prerequisites

- Go 1.26.1+
- `ANTHROPIC_API_KEY` environment variable set for the default Anthropic provider

### Installation

```bash
git clone <repository>
cd stirrup
go build -o stirrup ./harness/cmd/stirrup
```

### Usage

```bash
./stirrup harness --prompt "Your task here"
```

Or run directly:

```bash
go run ./harness/cmd/stirrup harness --prompt "Your task here"
```

### CLI Flags (`stirrup harness`)

| Flag | Default | Description |
|---|---|---|
| `--config` | (none) | Path to a JSON `RunConfig` file (mirrors `proto/harness/v1/harness.proto`). When set, explicitly-set flags override individual fields; unset flags do not. |
| `--prompt` | (required) | User prompt, also accepted as a positional argument |
| `--mode`, `-m` | `execution` | Run mode: execution, planning, review, research, toil |
| `--model` | `claude-sonnet-4-6` | Model to use |
| `--provider` | `anthropic` | Provider type: anthropic, bedrock, openai-compatible (Chat Completions), openai-responses (Responses API) |
| `--api-key-ref` | `secret://ANTHROPIC_API_KEY` | Secret reference for API key |
| `--workspace`, `-w` | current directory | Workspace directory |
| `--max-turns` | `20` | Maximum agentic loop turns |
| `--timeout` | `600` | Wall-clock timeout in seconds |
| `--trace` | (none) | Path to JSONL trace file |
| `--transport` | `stdio` | Transport type: stdio, grpc |
| `--transport-addr` | (none) | gRPC target address, required when transport is grpc |
| `--followup-grace` | `0` | Seconds to keep gRPC open for follow-ups, also configurable via `STIRRUP_FOLLOWUP_GRACE` |
| `--log-level` | `info` | Log level: debug, info, warn, error |
| `--executor` | `local` | Executor: local, container, api |
| `--edit-strategy` | `multi` | Edit strategy: whole-file, search-replace, udiff, multi (composite available only via `--config`) |
| `--verifier` | `none` | Verifier: none, test-runner, llm-judge (composite available only via `--config`) |
| `--git-strategy` | `none` | Git strategy: none, deterministic |
| `--trace-emitter` | `jsonl` | Trace emitter: jsonl, otel |
| `--otel-endpoint` | (none) | OTLP endpoint for the otel trace emitter (default: localhost:4317) |

#### Configuration precedence

When `--config <path>` is set, the file populates the full `RunConfig`.
Explicitly-set flags then override individual fields; flags left at their
default value do **not** override the file. When `--config` is not
provided, flags + their defaults build the `RunConfig` directly.

The default edit strategy is `multi` — the unified `edit_file` tool with
fallback across udiff, search-replace, and whole-file. Callers that
configure `write_file`, `search_replace`, or `apply_diff` in `tools.builtIn`
are aliased to the multi-strategy's `edit_file` tool, so the behavioural
contract is preserved.

See [`examples/runconfig/`](examples/runconfig/) for a fully-populated
config that exercises the container executor, OTel emitter, deterministic
git, dynamic router, and an MCP server.

### Example

```bash
ANTHROPIC_API_KEY=sk-ant-... ./stirrup harness \
  --prompt "Fix the failing test in main_test.go" \
  --mode execution \
  --max-turns 10 \
  --trace trace.jsonl

# Or load a full RunConfig from a file:
./stirrup harness --config examples/runconfig/full.json \
  --prompt "Fix the failing test in main_test.go"
```

## Eval CLI

```bash
go build -o stirrup-eval ./eval/cmd/eval

./stirrup-eval run --suite path/to/suite.json --output results/ --harness ./stirrup
./stirrup-eval compare --current results/result.json --baseline baseline/result.json
./stirrup-eval baseline --lakehouse path/to/lakehouse --output metrics.json
./stirrup-eval mine-failures --lakehouse path/to/lakehouse --limit 20 --output suite.json
./stirrup-eval drift --lakehouse path/to/lakehouse --window 7d
./stirrup-eval compare-to-production --results results/result.json --lakehouse path/to/lakehouse
```

## Architecture

The harness is composed of 12 swappable, interface-based components:

1. **ProviderAdapter** - Streams completions from LLMs: Anthropic, Bedrock, OpenAI-compatible (Chat Completions), OpenAI Responses
2. **ModelRouter** - Selects provider and model per turn: static, per-mode, dynamic
3. **PromptBuilder** - Assembles system prompts: default per-mode templates, composed fragments, overrides
4. **ContextStrategy** - Manages conversation history: sliding window, summarise, offload-to-file
5. **ToolRegistry** - Resolves and dispatches built-in and MCP remote tools
6. **Executor** - Executes commands and file operations: local, container, API, replay
7. **EditStrategy** - Applies file changes: whole-file, search-replace, unified diff, multi-strategy
8. **Verifier** - Validates run output: none, test-runner, LLM-as-judge, composite
9. **PermissionPolicy** - Gates side-effecting operations: allow-all, deny-side-effects, ask-upstream
10. **Transport** - Streams events: stdio, gRPC bidi streaming, null
11. **GitStrategy** - Manages branches and commits: none, deterministic
12. **TraceEmitter** - Records telemetry: JSONL, OpenTelemetry

All components are injected via the core factory (`core.BuildLoop` / `core.BuildLoopWithTransport`), which constructs the agentic loop from a `RunConfig`.

## Project Structure

```text
stirrup/
  go.work                    # Go workspace: types, harness, eval, gen modules
  buf.yaml                   # Buf v2 config for proto linting/breaking
  buf.gen.yaml               # Buf code generation config
  proto/harness/v1/          # Protobuf definitions for gRPC transport
  gen/                       # Generated Go code from proto
  types/                     # Shared type definitions
  harness/                   # Harness binary and components
    cmd/stirrup/             # Unified CLI: harness and job subcommands
    harnessapi/              # Public embedding API
    internal/
      core/                  # AgenticLoop, factory, token tracking, sub-agents, stall detection
      credential/            # Cross-cloud credential federation
      provider/              # Anthropic, Bedrock, OpenAI-compatible, OpenAI Responses adapters
      router/                # Static, per-mode, dynamic model routers
      prompt/                # Prompt builders and system prompt templates
      context/               # Sliding window, summarise, offload-to-file
      tool/                  # Tool registry and built-in tools
      executor/              # Local, container, API, replay executors
      edit/                  # Whole-file, search-replace, udiff, multi-strategy
      verifier/              # None, test-runner, composite, llm-judge
      permission/            # Allow-all, deny-side-effects, ask-upstream
      git/                   # None and deterministic git strategies
      transport/             # Stdio, gRPC, null transports
      trace/                 # JSONL and OpenTelemetry emitters
      observability/         # Structured logging and OTel metrics
      health/                # File-based K8s liveness probes
      security/              # SecretStore, LogScrubber, input validation
      mcp/                   # Remote MCP client over Streamable HTTP
  eval/                      # Eval framework
    cmd/eval/                # Eval CLI
    judge/                   # test-command, file-exists, file-contains, composite
    runner/                  # Live suite runner and replay evaluator
    reporter/                # Result comparison and text formatting
    lakehouse/               # File-backed TraceLakehouse adapter
    suites/                  # Eval suite definitions
    baselines/               # Stored baseline results
```

## Security

Stirrup includes security controls at every layer:

- **SecretStore**: Resolves `secret://` references from environment variables, files, and AWS SSM. API keys are not stored in `RunConfig`.
- **LogScrubber**: Regex-based redaction of 7 secret patterns in logs and trace output.
- **Input validation**: JSON Schema Draft 2020-12 validation via `santhosh-tekuri/jsonschema`, with external schema loading disabled and prototype pollution keys stripped.
- **RunConfig validation**: Hard security invariants for read-only modes, bounded max turns, timeout, follow-up grace, cost budget, and token budget.
- **Executor containment**: Workspace path traversal prevention, command output caps, file size limits, filtered command environments, and container hardening.
- **Web fetch protection**: HTTP/HTTPS only, private host blocking, DNS validation, explicit timeout, and 100KB response cap.
- **Untrusted context**: Dynamic context wrapped in `<untrusted_context>` delimiters.
- **Trace redaction**: `RunConfig.Redact()` strips secret references before trace persistence.
- **Stall detection**: Repeated identical tool calls and consecutive tool failures terminate the loop.

## Key Constants

- `MaxTurns`: 20 by default, hard-capped at 100 by `ValidateRunConfig`
- Default model: `claude-sonnet-4-6`
- Provider request defaults: `max_tokens` 64000, `temperature` 0.1
- File size limit: 10MB for reads and writes
- Command output cap: 1MB
- Command timeout: 30 seconds default, 5 minutes maximum
- Follow-up grace cap: 3600 seconds
- Token budget cap: 50M tokens
- Cost budget cap: $100

## Development

A `Justfile` is provided for common tasks:

```bash
just              # build + test
just build        # build stirrup and stirrup-eval binaries
just test         # go test ./harness/... ./types/... ./eval/...
just lint         # golangci-lint run ./harness/... ./types/... ./eval/...
just proto        # buf generate
just buf-lint     # buf lint
just docker       # build stirrup Docker image
just clean        # remove built binaries
```

Or directly:

```bash
go test ./harness/... ./types/... ./eval/...
go build ./harness/... ./types/... ./eval/...
buf generate
buf lint
```

## Design Document

See [`VERSION1.md`](VERSION1.md) for the Version 1 architecture summary.

## License

Apache 2.0
