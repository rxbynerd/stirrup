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
    internal/
      core/                  # AgenticLoop, factory, cost tracking
      provider/              # ProviderAdapter: Anthropic, Bedrock, OpenAI-compatible
      router/                # ModelRouter: static, per-mode, dynamic
      prompt/                # PromptBuilder: per-mode templates
      context/               # ContextStrategy: sliding window, summarise, offload
      tool/                  # ToolRegistry + built-in tools
      executor/              # Executor: local, container (Docker/Podman)
      edit/                  # EditStrategy: whole-file, search-replace, udiff
      verifier/              # Verifier: none, test-runner, composite
      permission/            # PermissionPolicy: allow-all, deny-side-effects, ask-upstream
      git/                   # GitStrategy: none, deterministic
      transport/             # Transport: stdio, gRPC bidi streaming
      trace/                 # TraceEmitter: JSONL
      security/              # SecretStore, LogScrubber, input validation
      mcp/                   # MCP client: remote tool discovery via Streamable HTTP
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
| `-transport` | `stdio` | Transport type: stdio, grpc |
| `-transport-addr` | (none) | gRPC target address (required when transport=grpc) |

## Architecture

12 swappable components, all interface-based:

1. **ProviderAdapter** — streams completions from LLMs (Anthropic, Bedrock, OpenAI-compatible)
2. **ModelRouter** — selects provider+model per turn (static, per-mode, dynamic)
3. **PromptBuilder** — assembles system prompt (default per-mode templates, composed)
4. **ContextStrategy** — manages message history (sliding window, summarise, offload-to-file)
5. **ToolRegistry** — resolves and dispatches tools (6 built-in tools + MCP remote tools)
6. **Executor** — sandboxed file I/O and command execution (local, container)
7. **EditStrategy** — how file changes are applied (whole-file, search-replace, udiff)
8. **Verifier** — validates run output (none, test-runner, composite)
9. **PermissionPolicy** — gates side-effecting tools (allow-all, deny-side-effects, ask-upstream)
10. **Transport** — streams events to/from control plane (stdio, gRPC bidi streaming)
11. **GitStrategy** — manages branches/commits (none, deterministic)
12. **TraceEmitter** — records telemetry (JSONL)

The core loop is a pure function of its interfaces. All dependencies are injected via the factory (`core.BuildLoop`), which constructs components from a `RunConfig`.

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

### Protobuf / Buf toolchain

Proto files are managed with [Buf](https://buf.build). To regenerate after proto changes:
```sh
buf generate
```
Generated code lives in `gen/` (a separate Go module in the workspace). Buf config: `buf.yaml` (lint/breaking rules) and `buf.gen.yaml` (code generation with `buf.build/protocolbuffers/go` and `buf.build/grpc/go` remote plugins).

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
buf generate             # Regenerate proto code (after editing .proto files)
buf lint                 # Lint proto files
```

### Known issue: gopls false positives

The LSP (gopls) frequently reports false positive diagnostics due to the `go.work` workspace module resolution. Errors referencing packages like `NewComposedPromptBuilder not declared by package prompt` or fields from other modules are almost always false. Always verify with `go build` and `go test` — if they pass, the LSP errors are spurious.

## External dependencies rationale

The project follows a minimal-dependency philosophy. Provider adapters and the container executor use hand-rolled HTTP clients against well-documented REST APIs rather than pulling in large SDK dependency trees. This is deliberate — for a security-sensitive harness that holds API keys and executes code, minimising the dependency surface is worth the cost of writing a few hundred lines of HTTP client code.

Exceptions where external deps are accepted:
- `aws-sdk-go-v2` for Bedrock (IAM SigV4 auth is complex enough to justify)
- `google.golang.org/grpc` + `google.golang.org/protobuf` for gRPC transport (the reference Go gRPC implementation)

## Legacy Ruby Code

The original Ruby prototype (`stirrup.rb`, `server.rb`) is still in the repo root. It is superseded by the Go harness but kept for reference during the transition.
