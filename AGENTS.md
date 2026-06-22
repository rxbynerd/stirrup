# stirrup

A coding agent harness. Go monorepo with 13 swappable components composed via `RunConfig`. See [`docs/architecture.md`](docs/architecture.md) for the architectural overview; this file is a per-package map for AI agents working on the codebase.

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
      core/                  # AgenticLoop, factory, token tracking, sub-agent spawning, parallel tool dispatch, stall detection
      credential/            # Cross-cloud credential federation
      provider/              # ProviderAdapter: Anthropic, Bedrock, OpenAI-compatible, OpenAI Responses, Gemini (Vertex AI)
      router/                # ModelRouter: static, per-mode, dynamic
      prompt/                # PromptBuilder: per-mode templates, composed fragments, overrides
      context/               # ContextStrategy: sliding window, summarise, offload-to-file
      tool/                  # ToolRegistry + built-in tools, including spawn_agent
      tool/toolname/         # Per-provider tool-name normalization + collision detection (#223)
      executor/              # Executor: local, container (Docker/Podman), API (GitHub), replay
      edit/                  # EditStrategy: whole-file, search-replace, udiff, multi-strategy
      verifier/              # Verifier: none, test-runner, composite, llm-judge
      permission/            # PermissionPolicy: allow-all, deny-side-effects, ask-upstream, policy-engine (Cedar)
      git/                   # GitStrategy: none, deterministic
      transport/             # Transport: stdio, gRPC bidi streaming, null (sub-agents)
      guard/                 # GuardRail: none, granite-guardian, cloud-judge, composite, phase-gated
      trace/                 # TraceEmitter: JSONL, OpenTelemetry (OTLP/gRPC or OTLP/HTTP)
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
    suites/                  # Eval suite definitions (HCLv2)
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
| `--mode`, `-m` | `planning` | Run mode: planning (default, read-only), execution, review, research, toil |
| `--model` | `claude-sonnet-4-6` | Model to use |
| `--provider` | `anthropic` | Provider type: anthropic, bedrock, openai-compatible (Chat Completions), openai-responses (Responses API), gemini (Vertex AI) |
| `--api-key-ref` | `secret://ANTHROPIC_API_KEY` | Secret reference for API key |
| `--workspace`, `-w` | current directory | Workspace directory |
| `--max-turns` | `20` | Maximum agentic loop turns |
| `--timeout` | `600` | Wall-clock timeout in seconds |
| `--trace` | (none) | Path to JSONL trace file |
| `--transport` | `stdio` | Transport type: stdio, grpc |
| `--transport-addr` | (none) | gRPC target address, required when transport is grpc |
| `--followup-grace` | `0` | Seconds to keep gRPC open for follow-ups (env: `STIRRUP_FOLLOWUP_GRACE`) |
| `--log-level` | `info` | Log level: debug, info, warn, error |
| `--config` | (none) | JSON `RunConfig` path. Use `-` for stdin; auto-detected on a non-TTY pipe. |
| `--output-runconfig` | (none) | Write the resolved `RunConfig` JSON to `<path>` (or `-` for stdout) and exit without running. Dry-run capture for replay or post-mortem. |

### CLI Flags (`stirrup run-config`)

Emits a fully-resolved `RunConfig` JSON document without invoking the
loop. Accepts every `RunConfig`-producing flag from `stirrup harness`
plus a base via stdin or `--config <path>`. See
[`docs/configuration.md`](docs/configuration.md#building-runconfigs-interactively).

| Flag | Default | Description |
|---|---|---|
| `--validate` | `false` | Run `types.ValidateRunConfig` and exit non-zero on failure. |
| `--compact` | `false` | Single-line JSON instead of indented (2-space). |
| `--redact` | `false` | Apply `RunConfig.Redact()` before emit (rewrites `secret://` references). |

## Architecture

13 swappable components, all interface-based:

1. **ProviderAdapter** — streams completions from LLMs (Anthropic, Bedrock, OpenAI-compatible, OpenAI Responses, Gemini via Vertex AI)
2. **ModelRouter** — selects provider+model per turn (static, per-mode, dynamic)
3. **PromptBuilder** — assembles system prompt (default per-mode templates, composed fragments, overrides)
4. **ContextStrategy** — manages message history (sliding window, summarise, offload-to-file)
5. **ToolRegistry** — resolves and dispatches tools (7 built-in runtime tools + MCP remote tools)
6. **Executor** — sandboxed file I/O and command execution (local, container, API, replay)
7. **EditStrategy** — how file changes are applied (whole-file, search-replace, udiff, multi-strategy)
8. **Verifier** — validates run output (none, test-runner, composite, llm-judge)
9. **PermissionPolicy** — gates side-effecting tools (allow-all, deny-side-effects, ask-upstream, policy-engine/Cedar)
10. **Transport** — streams events to/from control plane (stdio, gRPC bidi streaming, null)
11. **GitStrategy** — manages branches/commits (none, deterministic)
12. **TraceEmitter** — records telemetry (JSONL, OpenTelemetry OTLP/gRPC or OTLP/HTTP)
13. **GuardRail** — LLM-based content safety classifier at pre-turn, pre-tool, and post-turn hooks (none, granite-guardian, cloud-judge, composite)

The core loop is a pure function of its interfaces. All dependencies are injected via the factory (`core.BuildLoop` / `core.BuildLoopWithTransport`), which constructs components from a `RunConfig`.

### Provider adapters

- **Anthropic** (`provider/anthropic.go`) — SSE streaming via `net/http` + `bufio.Scanner`. Hand-rolled, no SDK dependency.
- **Bedrock** (`provider/bedrock.go`) — AWS ConverseStream API via `aws-sdk-go-v2`. Auth is IAM, with optional credential federation.
- **OpenAI-compatible** (`provider/openai.go`) — OpenAI chat completions streaming. Works with OpenAI, LiteLLM, Azure OpenAI, vLLM, Ollama via configurable `baseURL`.
- **OpenAI Responses** (`provider/openai_responses.go`) — OpenAI Responses API (`POST /v1/responses`) streaming. Distinct wire format from Chat Completions: top-level `instructions`, typed `input[]` items, flat tool schema, `max_output_tokens`, explicit `store: false`, and named SSE events. Selected explicitly via `provider.type: "openai-responses"` — no auto-detection between the two OpenAI adapters.
- **Gemini via Vertex AI** (`provider/gemini.go`) — Vertex AI `:streamGenerateContent` with `?alt=sse`. SSE-framed, hand-rolled HTTP. Auth is GCP IAM via Application Default Credentials or a service account key. Synthesises tool-call IDs and remaps `finishReason: STOP` to `tool_use` when the stream emitted function calls.

See [`docs/providers.md`](docs/providers.md) for full per-adapter configuration details.

### Container executor

The container executor (`executor/container.go`, `executor/container_api.go`) uses the Docker Engine REST API directly over a Unix socket, with no Docker SDK dependency. Docker and Podman are both supported through the Engine API.

Container lifecycle: created at executor init with `sleep infinity`; operations go through the exec or archive API; destroyed on `Close()`. Hardened with `CapDrop: ALL`, `no-new-privileges`, and `NetworkMode: none` by default. API keys never enter the container.

### MCP client

The MCP client (`mcp/client.go`) connects to remote MCP servers via Streamable HTTP transport (JSON-RPC 2.0 over HTTP POST). Remote-only by design. Tool names are prefixed as `mcp_{serverName}_{toolName}` to avoid collisions.

### gRPC transport

The gRPC transport (`transport/grpc.go`) implements the `Transport` interface as an outbound bidi streaming client. Proto definitions are in `proto/harness/v1/harness.proto`, generated with Buf (`buf generate`). The harness connects to the control plane, not the other way around.

### API executor

The API executor (`executor/api.go`) implements the `Executor` interface for read-only modes, backed by the GitHub REST API Contents endpoint. `ReadFile` and `ListDirectory` work; `WriteFile` and `Exec` return errors.

### K8s job entrypoint

`stirrup job` dials the control plane at `CONTROL_PLANE_ADDR` via gRPC, emits a `ready` event, blocks until a `task_assignment` arrives, then runs the agentic loop over the pre-established transport using `BuildLoopWithTransport`.

### Eval framework

- **Judge** (`eval/judge/`) — evaluates `EvalJudge` criteria against workspace state. Supports `test-command`, `file-exists`, `file-contains`, and `composite`.
- **Runner** (`eval/runner/`) — loads `EvalSuite` HCL (`.hcl` extension required), creates temp workspaces, optionally clones repos, invokes the harness binary, parses JSONL traces, and applies judges. Supports bounded concurrency.
- **Replay evaluator** (`eval/runner/replay.go`) — re-evaluates recorded runs through judges without re-running the harness.
- **Reporter** (`eval/reporter/`) — diffs two `SuiteResult` sets and formats human-readable reports.
- **Lakehouse** (`eval/lakehouse/filestore.go`) — file-backed `TraceLakehouse` adapter for production trace metrics and recordings.

### Credential federation

The `credential` package (`credential/`) provides cross-cloud authentication through a two-tier abstraction:

- **TokenSource** — fetches identity tokens from the runtime environment (GKE Workload Identity metadata server, k8s projected volumes, environment variable, AWS IRSA, Azure IMDS, GitHub Actions OIDC).
- **credential.Source** — exchanges identity tokens (or resolves static secrets) into provider-specific credentials. Implementations include `StaticSource`, `WebIdentityAWSSource`, `AnthropicWIFSource`, `OpenAIWIFSource`, `AzureWorkloadIdentitySource`, and GCP-native paths. The Anthropic and OpenAI WIF sources share the JSON token-exchange skeleton in `oauth_exchange.go`.

The `BearerToken` closure returned by `Resolved.BearerToken` is called on every provider request — tokens are refreshed without restarting the run. See [`docs/credential-federation.md`](docs/credential-federation.md), [`docs/anthropic-wif.md`](docs/anthropic-wif.md), [`docs/openai-wif.md`](docs/openai-wif.md), and [`docs/azure-workload-identity.md`](docs/azure-workload-identity.md).

### GuardRail

The `guard` package (`guard/`) provides an LLM-based safety classifier called at three points in the agentic loop: pre-turn (before untrusted content enters context), pre-tool (before the model's proposed tool call is dispatched), and post-turn (after the assistant's response). Adapters: `none` (no-op), `granite-guardian` (IBM Granite Guardian via vLLM), `cloud-judge` (reuses a configured ProviderAdapter), `composite`. See [`docs/guardrails.md`](docs/guardrails.md).

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

## Agent Working Practices

Agents are encouraged to work autonomously through implementation,
verification, and review packaging. For bounded GitHub issues or Beads tasks,
the expected flow is:

1. Inspect the issue/task and relevant code.
2. Keep edits scoped to the selected issue.
3. Run targeted verification, broadening to `just test` when the blast radius
   warrants it.
4. Create a focused branch and commit the verified change.
5. Push the branch and open a draft pull request for review.

Use branch names like `fix/issue-291-batchpoll-flake` or
`feat/issue-293-eval-wif`. Commit subjects follow the existing convention:
`<area>: <imperative subject>`.

Draft PR bodies should include the linked issue, a concise summary, tests run,
and any residual risk or follow-up work. If verification cannot run locally,
the PR must say exactly what was not run and why.

Do not mix unrelated issues in one PR unless the issue bodies are explicitly
coupled. Do not stage or commit unrelated dirty worktree changes. If unrelated
local changes are present, leave them alone and stage only the files touched for
the current task.

## External Dependencies Rationale

The project follows a minimal-dependency philosophy. Provider adapters and the container executor use hand-rolled HTTP clients against documented REST APIs rather than pulling in large SDK dependency trees.

Exceptions where external deps are accepted:

- `github.com/spf13/cobra` for CLI framework
- `github.com/santhosh-tekuri/jsonschema/v6` for full JSON Schema validation
- `aws-sdk-go-v2` for Bedrock, STS, and SSM SecretStore
- `google.golang.org/grpc` + `google.golang.org/protobuf` for gRPC transport
- `go.opentelemetry.io/otel` + OTLP exporter for OpenTelemetry trace and metrics (gRPC and HTTP/protobuf)
- `golang.org/x/oauth2` for GCP ADC, GCP service-account JWT flow, and WIF token refresh
- `github.com/cedar-policy/cedar-go` for the Cedar policy engine (`policy-engine` PermissionPolicy)
- `github.com/hashicorp/hcl/v2` for eval suite HCL parsing (eval module only)
