# stirrup

A coding agent harness for building composable AI agents. Built in Go with 12 swappable components that can be configured via `RunConfig`.

## Features

- **Modular Architecture**: 12 interface-based components for LLM provider, tool execution, file editing, and more
- **Streaming Support**: SSE streaming from Claude API for real-time responses
- **Security-First**: Built-in secret redaction, input validation, permission policies, HTTP client timeouts, loop stall detection
- **Multi-Mode Operation**: execution, planning, review, research, toil modes
- **Token Accounting**: Input/output token tracking per turn with configurable budget limits
- **Sub-Agent Spawning**: Delegate subtasks to fresh agent instances with their own context
- **Multi-Strategy Editing**: Unified edit tool with automatic fallback (udiff → search-replace → whole-file)
- **Extensible**: All components can be swapped out and replaced

## Quick Start

### Prerequisites

- Go 1.26.1+
- `ANTHROPIC_API_KEY` environment variable set

### Installation

```bash
git clone <repository>
cd stirrup
go build -o stirrup-harness ./harness/cmd/harness
```

### Usage

```bash
./stirrup-harness -prompt "Your task here"
```

Or run directly:

```bash
go run ./harness/cmd/harness -prompt "Your task here"
```

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-prompt` | (required) | User prompt to execute |
| `-mode` | `execution` | Run mode: execution, planning, review, research, toil |
| `-model` | `claude-sonnet-4-6` | Claude model to use |
| `-provider` | `anthropic` | Provider type |
| `-api-key-ref` | `secret://ANTHROPIC_API_KEY` | Secret reference for API key |
| `-workspace` | current directory | Workspace directory |
| `-max-turns` | `20` | Maximum agentic loop turns |
| `-timeout` | `600` | Wall-clock timeout in seconds |
| `-trace` | (none) | Path to write JSONL trace file |

### Example

```bash
ANTHROPIC_API_KEY=sk-ant-... ./stirrup-harness \
  -prompt "Fix the failing test in main_test.go" \
  -mode execution \
  -max-turns 10 \
  -trace trace.jsonl
```

## Architecture

The harness is composed of 12 swappable, interface-based components:

1. **ProviderAdapter** — Streams completions from LLMs (Anthropic implemented)
2. **ModelRouter** — Selects provider and model per turn (static router implemented)
3. **PromptBuilder** — Assembles system prompt for each mode
4. **ContextStrategy** — Manages conversation history (sliding window implemented)
5. **ToolRegistry** — Resolves and dispatches tools
6. **Executor** — Executes commands and file operations (local executor)
7. **EditStrategy** — Applies file changes to workspace (whole-file strategy)
8. **Verifier** — Validates run output (none by default)
9. **PermissionPolicy** — Gates side-effecting operations (allow-all, deny-side-effects)
10. **Transport** — Streams events to/from control plane (stdio)
11. **GitStrategy** — Manages branches and commits (none by default)
12. **TraceEmitter** — Records telemetry in JSONL format

All components are injected via the core factory (`core.BuildLoop`), which constructs the agentic loop from a `RunConfig`.

## Project Structure

```
stirrup/
  go.work                    # Go workspace definition
  types/                     # Shared type definitions (zero dependencies)
  harness/                   # Main harness implementation
    cmd/harness/main.go      # CLI entrypoint
    internal/
      core/                  # AgenticLoop, factory, cost tracking, sub-agent spawning, stall detection
      provider/              # ProviderAdapter: Anthropic, Bedrock, OpenAI-compatible
      router/                # ModelRouter: static, per-mode, dynamic
      prompt/                # PromptBuilder: per-mode system prompts
      context/               # ContextStrategy: sliding window, summarise, offload
      tool/                  # ToolRegistry and built-in tools (incl. spawn_agent)
      executor/              # Executor: local, container, API
      edit/                  # EditStrategy: whole-file, search-replace, udiff, multi-strategy
      verifier/              # Verifier: none, test-runner, composite, llm-judge
      permission/            # PermissionPolicy: allow-all, deny-side-effects, ask-upstream
      git/                   # GitStrategy: none, deterministic
      transport/             # Transport: stdio, gRPC, null (for sub-agents)
      trace/                 # TraceEmitter: JSONL, OpenTelemetry
      security/              # SecretStore, LogScrubber, input validation
  eval/                      # Evaluation framework
    cmd/eval/main.go         # CLI entrypoint (run, compare, baseline, mine-failures, drift, compare-to-production)
    judge/                   # Judge system (test-command, file-exists, file-contains, composite)
    runner/                  # Suite runner and replay evaluator
    reporter/                # Comparison reporter with text formatting
    lakehouse/               # TraceLakehouse adapters (file-based)
    suites/                  # Eval suite definitions (JSON)
    baselines/               # Stored baseline results for CI comparison
```

## Security

stirrup includes security controls at every layer:

- **SecretStore**: Resolves `secret://` references (environment variables, files). API keys never stored in RunConfig.
- **LogScrubber**: Regex-based redaction of 7 secret patterns in all logs and trace output
- **Input Validation**: JSON Schema validation on all tool inputs with prototype pollution protection
- **RunConfig Validation**: Hard security invariants for read-only modes (restrictive permissions, bounded max-turns/timeout)
- **Untrusted Context**: Dynamic context wrapped in `<untrusted_context>` delimiters
- **Trace Redaction**: `RunConfig.Redact()` strips secrets before trace persistence

## Constraints

- **Max Turns**: 20 (configurable)
- **Default Model**: `claude-sonnet-4-6`
- **Max Tokens**: 64,000
- **Temperature**: 0.1
- **File Size Limit**: 10MB (read/write)
- **Command Output Cap**: 1MB
- **Command Timeout**: 30s default, 5 minutes max

## Development

### Running Tests

```bash
go test ./harness/...
```

### Building

```bash
go build ./harness/...
```

## Design Document

See [`VERSION1.md`](VERSION1.md) for the full architecture and design document.

## Legacy

The original Ruby prototype (`stirrup.rb`, `server.rb`) is preserved in the repo root for reference during the Go transition. The Go harness is the authoritative implementation.

## License

[Add license information as needed]
