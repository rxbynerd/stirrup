# stirrup

A coding agent harness. Go monorepo with 12 swappable components that can be composed via RunConfig. See VERSION1.md for the full design document.

## Project Structure

```
stirrup/
  go.work                    # Go workspace: types, harness, eval modules
  types/                     # Shared type definitions (zero dependencies)
  harness/                   # The harness binary
    cmd/harness/main.go      # CLI entrypoint
    internal/
      core/                  # AgenticLoop, factory, cost tracking
      provider/              # ProviderAdapter: Anthropic SSE streaming
      router/                # ModelRouter: static router
      prompt/                # PromptBuilder: per-mode templates
      context/               # ContextStrategy: sliding window
      tool/                  # ToolRegistry + built-in tools
      executor/              # Executor: local (workspace-scoped)
      edit/                  # EditStrategy: whole-file
      verifier/              # Verifier: none
      permission/            # PermissionPolicy: allow-all, deny-side-effects
      git/                   # GitStrategy: none
      transport/             # Transport: stdio (JSON lines)
      trace/                 # TraceEmitter: JSONL
      security/              # SecretStore, LogScrubber, input validation
  eval/                      # Eval framework (scaffolded, not yet implemented)
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

## Architecture

12 swappable components, all interface-based:

1. **ProviderAdapter** — streams completions from LLMs (Anthropic implemented)
2. **ModelRouter** — selects provider+model per turn (static implemented)
3. **PromptBuilder** — assembles system prompt (default per-mode templates)
4. **ContextStrategy** — manages message history (sliding window)
5. **ToolRegistry** — resolves and dispatches tools (6 built-in tools)
6. **Executor** — sandboxed file I/O and command execution (local)
7. **EditStrategy** — how file changes are applied (whole-file)
8. **Verifier** — validates run output (none)
9. **PermissionPolicy** — gates side-effecting tools (allow-all, deny-side-effects)
10. **Transport** — streams events to/from control plane (stdio)
11. **GitStrategy** — manages branches/commits (none)
12. **TraceEmitter** — records telemetry (JSONL)

The core loop is a pure function of its interfaces. All dependencies are injected via the factory (`core.BuildLoop`), which constructs components from a `RunConfig`.

## Security Foundations

- **SecretStore**: resolves `secret://` references (env vars, files). API keys never stored in RunConfig.
- **LogScrubber**: regex-based redaction of 7 secret patterns in all log/trace output.
- **Input validation**: JSON Schema validation on all tool inputs. Prototype pollution protection.
- **RunConfig validation**: hard security invariants (read-only modes must use restrictive permissions, bounded maxTurns/timeout).
- **Untrusted context delimiters**: dynamic context wrapped in `<untrusted_context>` tags.
- **RunConfig.Redact()**: strips secret references before trace persistence.

## Key Constants

- `MAX_TURNS = 20` (configurable via RunConfig/CLI)
- Default model: `claude-sonnet-4-6`
- `max_tokens: 64000`, `temperature: 0.1`
- File size limit: 10MB (read/write)
- Command output cap: 1MB
- Command timeout: 30s default, 5min max

## Development

```sh
go test ./harness/...    # Run all tests
go build ./harness/...   # Build all packages
```

## Legacy Ruby Code

The original Ruby prototype (`stirrup.rb`, `server.rb`) is still in the repo root. It is superseded by the Go harness but kept for reference during the transition.
