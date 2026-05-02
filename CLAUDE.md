# stirrup

A coding agent harness. Go monorepo with 12 swappable components that can be composed via RunConfig.

VERSION1.md contains the summary of what was implemented during "version 1" (PR #1).

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
    cmd/stirrup/             # Unified CLI entrypoint (cobra: harness + job subcommands)
      main.go
      cmd/root.go
      cmd/harness.go
      cmd/job.go
    harnessapi/              # Public embedding API
    internal/
      core/                  # AgenticLoop, factory, token tracking, sub-agent spawning, stall detection
      credential/            # Cross-cloud credential federation (token sources + credential sources)
      provider/              # ProviderAdapter: Anthropic, Bedrock, OpenAI-compatible, OpenAI Responses
      router/                # ModelRouter: static, per-mode, dynamic
      prompt/                # PromptBuilder: per-mode templates
      context/               # ContextStrategy: sliding window, summarise, offload-to-file
      tool/                  # ToolRegistry + built-in tools (incl. spawn_agent)
      executor/              # Executor: local, container (Docker/Podman), API (GitHub), replay
      executor/egressproxy/  # In-process HTTP/CONNECT forward proxy for container allowlist mode (B2)
      edit/                  # EditStrategy: whole-file, search-replace, udiff, multi-strategy, scanned
      verifier/              # Verifier: none, test-runner, composite, llm-judge
      permission/            # PermissionPolicy: allow-all, deny-side-effects, ask-upstream, policy-engine (Cedar)
      git/                   # GitStrategy: none, deterministic
      transport/             # Transport: stdio, gRPC bidi streaming, null (sub-agents)
      trace/                 # TraceEmitter: JSONL, OpenTelemetry (OTLP/gRPC)
      observability/         # Structured logging (slog + ScrubHandler), OTel metrics
      health/                # File-based K8s liveness probes
      security/              # SecretStore (env, file, AWS SSM), LogScrubber, input validation
      security/codescanner/  # Post-edit static analysis: patterns, semgrep, composite (B5)
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
go build -o stirrup ./harness/cmd/stirrup
./stirrup harness --prompt "Your task here"
```

Or directly:
```sh
go run ./harness/cmd/stirrup harness --prompt "Your task here"
```

Requires `ANTHROPIC_API_KEY` environment variable.

### CLI Flags (`stirrup harness`)

| Flag | Default | Description |
|---|---|---|
| `--config` | (none) | Path to a JSON RunConfig file (mirrors `proto/harness/v1/harness.proto`). Explicit flags still override individual fields; unset flags do not. |
| `--prompt` | (required) | User prompt (also accepted as positional arg) |
| `--mode`, `-m` | `execution` | Run mode: execution, planning, review, research, toil |
| `--model` | `claude-sonnet-4-6` | Model to use |
| `--provider` | `anthropic` | Provider type: anthropic, bedrock, openai-compatible (Chat Completions), openai-responses (Responses API). Both OpenAI variants accept `--base-url`, `--api-key-header`, and `--query-param` for Azure / gateway scenarios. |
| `--api-key-ref` | `secret://ANTHROPIC_API_KEY` | Secret reference for API key |
| `--base-url` | (none) | Provider base URL (openai-compatible, openai-responses). E.g. `https://<resource>.openai.azure.com/openai/v1`. |
| `--api-key-header` | (none) | Header name for sending the API key. Empty means `Authorization: Bearer` (default). Set to `api-key` for Azure OpenAI key auth, or `x-api-key` / `Ocp-Apim-Subscription-Key` for gateway variants. |
| `--query-param` | (none) | Repeatable `key=value`. Adds query parameters to every provider request URL — e.g. `--query-param api-version=preview` for Azure. Keys here override duplicates already encoded in `--base-url`. |
| `--workspace`, `-w` | current directory | Workspace directory |
| `--max-turns` | `20` | Maximum agentic loop turns |
| `--timeout` | `600` | Wall-clock timeout in seconds |
| `--trace` | (none) | Path to JSONL trace file |
| `--log-level` | `info` | Log level: debug, info, warn, error |
| `--transport` | `stdio` | Transport type: stdio, grpc |
| `--transport-addr` | (none) | gRPC target address (required when transport=grpc) |
| `--followup-grace` | `0` | Seconds to keep gRPC open for follow-ups (env: STIRRUP_FOLLOWUP_GRACE) |
| `--executor` | `local` | Executor: local, container, api |
| `--edit-strategy` | `multi` | Edit strategy: whole-file, search-replace, udiff, multi (composite via `--config` only) |
| `--verifier` | `none` | Verifier: none, test-runner, llm-judge (composite via `--config` only) |
| `--git-strategy` | `none` | Git strategy: none, deterministic |
| `--trace-emitter` | `jsonl` | Trace emitter: jsonl, otel |
| `--otel-endpoint` | (none) | OTLP endpoint for the otel trace emitter (default: localhost:4317) |
| `--container-runtime` | (none) | OCI runtime for the container executor: runc, runsc (gVisor), kata, kata-qemu, kata-fc. Empty = engine default. Requires the runtime to be registered with the host Docker/Podman daemon. See `docs/sandbox.md`. |
| `--permission-policy-file` | (none) | Path to a Cedar policy file for the policy-engine PermissionPolicy. When set and the policy type is unset elsewhere, also implies `permissionPolicy.type=policy-engine`. Starters live under `examples/policies/`. |
| `--code-scanner` | (none) | CodeScanner type: none, patterns, semgrep, composite. Composite requires `--config` (`codeScanner.scanners`). Empty defers to the mode-aware default (patterns for execution, none for read-only modes). |

Precedence: `--config` file → explicit flags → defaults. Flags left at
their default value do NOT override the file. The default edit strategy
is `multi`; legacy tool names (`write_file`, `search_replace`,
`apply_diff`) in `tools.builtIn` are aliased to the multi-strategy's
`edit_file` tool by `core/factory.go::editToolEnabled`.

A fully-populated example lives at `examples/runconfig/full.json`.

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

1. **ProviderAdapter** — streams completions from LLMs (Anthropic, Bedrock, OpenAI-compatible, OpenAI Responses)
2. **ModelRouter** — selects provider+model per turn (static, per-mode, dynamic)
3. **PromptBuilder** — assembles system prompt (default per-mode templates, composed)
4. **ContextStrategy** — manages message history (sliding window, summarise, offload-to-file)
5. **ToolRegistry** — resolves and dispatches tools (7 built-in tools + MCP remote tools)
6. **Executor** — sandboxed file I/O and command execution (local, container, API)
7. **EditStrategy** — how file changes are applied (whole-file, search-replace, udiff, multi-strategy)
8. **Verifier** — validates run output (none, test-runner, composite, llm-judge)
9. **PermissionPolicy** — gates tools that mutate the workspace or require operator approval (allow-all, deny-side-effects, ask-upstream, policy-engine). `deny-side-effects` rejects only workspace-mutating tools (write_file, run_command, edit_file); read-only-but-network/budget-touching tools like web_fetch and spawn_agent are still allowed and are gated separately by `ask-upstream`. `policy-engine` evaluates a Cedar policy file per tool call and falls back to one of the three other policies on no-decision.
10. **Transport** — streams events to/from control plane (stdio, gRPC bidi streaming, null for sub-agents)
11. **GitStrategy** — manages branches/commits (none, deterministic)
12. **TraceEmitter** — records telemetry (JSONL, OpenTelemetry)

The core loop is a pure function of its interfaces. All dependencies are injected via the factory (`core.BuildLoop` / `core.BuildLoopWithTransport`), which constructs components from a `RunConfig`.

### Provider adapters

- **Anthropic** (`provider/anthropic.go`) — SSE streaming via `net/http` + `bufio.Scanner`. Hand-rolled, no SDK dependency.
- **Bedrock** (`provider/bedrock.go`) — AWS ConverseStream API via `aws-sdk-go-v2`. Translates between internal types and Bedrock's union-type wire format. Auth is IAM (not API key); uses `config.LoadDefaultConfig()`. Accepts optional `aws.CredentialsProvider` for cross-cloud credential federation.
- **OpenAI-compatible** (`provider/openai.go`) — OpenAI chat completions streaming. Works with OpenAI, LiteLLM, Azure OpenAI, vLLM, Ollama via configurable `baseURL`. Azure OpenAI key auth is supported by setting `provider.apiKeyHeader: "api-key"` (Entra ID bearer tokens still work with the empty default), and required api-version pins ride through `provider.queryParams`.
- **OpenAI Responses** (`provider/openai_responses.go`) — OpenAI Responses API (`POST /v1/responses`) streaming. Distinct wire format from Chat Completions: top-level `instructions` field, typed `input[]` items (`message` / `function_call` / `function_call_output`), flat tool schema, `max_output_tokens`, explicit `store: false`, and named SSE events (`response.output_text.delta`, `response.function_call_arguments.delta`, `response.completed`, `response.incomplete`, `response.failed`). Selected explicitly via `provider.type: "openai-responses"` — there is no auto-detection between the two OpenAI adapters because silent fallback would mask configuration errors. Built-in OpenAI-side tools (`web_search`, `file_search`, `computer_use`, `code_interpreter`), server-side state via `previous_response_id`, and reasoning controls are intentionally not supported in this adapter; the harness manages its own conversation history. Azure Foundry's `/openai/v1/responses` endpoint is wire-compatible: point `provider.baseUrl` at the Azure resource, set `provider.apiKeyHeader: "api-key"` for key auth (or leave empty for Entra ID Bearer), and add `provider.queryParams: {"api-version": "preview"}` (or whatever the deployment expects). Azure-only Responses extensions ride the existing forward-compatible "unknown SSE event" path and are silently ignored. See `examples/runconfig/azure-openai.json`.

### Credential federation

The `credential` package (`credential/`) enables cross-cloud authentication for provider adapters. It has two composable layers:

- **TokenSource** — fetches identity tokens from the runtime environment. Implementations: `GKEMetadataTokenSource` (GKE Workload Identity metadata server), `FileTokenSource` (k8s projected volumes), `EnvTokenSource` (environment variable).
- **credential.Source** — exchanges identity tokens (or resolves static secrets) into provider-specific credentials. Implementations: `StaticSource` (wraps SecretStore for API keys), `AWSDefaultSource` (SDK default chain), `WebIdentityAWSSource` (OIDC token → STS AssumeRoleWithWebIdentity → AWS credentials).

Token sources are reusable across targets — the same GKE OIDC token can be exchanged for AWS or (future) Azure credentials. `WebIdentityAWSSource.Resolve()` is non-blocking: it sets up a lazy `aws.CredentialsCache` that calls STS on first use and automatically refreshes before expiry. Configured via `ProviderConfig.Credential` in RunConfig; when omitted, the source type is inferred from the provider type (backward compatible).

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

The harness uses `log/slog` (stdlib) with a custom `ScrubHandler` (`observability/logger.go`) that wraps any `slog.Handler` and runs `security.Scrub()` on all string attribute values before delegation. This makes secret leakage through logs structurally impossible. JSON logs are written to stderr with a `runId` field on every line. Log level is configurable via `--log-level` flag or `RunConfig.LogLevel`.

### OTel metrics

The `observability/metrics.go` package emits OTel metrics via OTLP/gRPC alongside tracing. Instruments: 12 counters (`stirrup.harness.runs`, `.turns`, `.tokens.input`, `.tokens.output`, `.tool_calls`, `.tool_errors`, `.provider.requests`, `.provider.errors`, `.context.compactions`, `.security.events`, `.verification.attempts`, `.stalls`), 5 histograms (run/turn/tool-call duration, provider latency, TTFB), and 1 UpDownCounter (context token estimate). All instruments use standard attributes (`run.mode`, `provider.type`, `tool.name`, etc.). `NewNoopMetrics()` provides a zero-cost no-op when metrics are disabled.

### Heartbeat and health probes

The agentic loop emits `heartbeat` events on the transport every 30 seconds during execution. For K8s jobs, a file-based liveness probe (`health/probe.go`) writes `/tmp/healthy` after the ready event and removes it on shutdown.

### K8s job entrypoint

`stirrup job` is the K8s job subcommand. It dials the control plane at `CONTROL_PLANE_ADDR` via gRPC, emits a "ready" event, blocks until a `task_assignment` arrives, then runs the agentic loop over the pre-established transport using `BuildLoopWithTransport`.

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

### Deterministic safety rings (issue #42)

Five layered controls compose at run construction:

- **Container runtimeClass** (`executor/container.go`) — optional `Runtime` field passed to the Docker Engine API selects `runc` (default), `runsc` (gVisor), or `kata-*` for kernel-isolation.
- **Egress proxy** (`executor/egressproxy/`) — when `network.mode == "allowlist"` the container executor starts an in-process forward proxy on the host network namespace; the container is wired with `HTTP_PROXY`/`HTTPS_PROXY` and only well-formed requests to allowlisted FQDNs are forwarded. v1 fails closed only for cooperating clients (the iptables drop is a documented follow-up).
- **Cedar policy engine** (`permission/policyengine.go`) — the fourth `PermissionPolicy` type. Backed by `github.com/cedar-policy/cedar-go`. Loads a `.cedar` file at boot, evaluates each tool call as `(principal=User::"<runId>", action=Action::"tool:<name>", resource=Tool::"<name>", context={input, workspace, dynamicContext})`, falls back to a configured non-policy-engine policy on no-decision.
- **Rule of Two** (`types/runconfig.go::validateRuleOfTwo`) — structural invariant rejecting any RunConfig that simultaneously holds untrusted input, sensitive data, and external communication unless gated by `ask-upstream`. `RuleOfTwo.Enforce: false` is the only override; the factory emits a `rule_of_two_disabled` security event when it is used and `rule_of_two_warning` when exactly two of three flags hold.
- **CodeScanner** (`security/codescanner/`, `edit/scanned.go`) — post-edit static analysis. Pure-Go pattern pack, optional shell-out to `semgrep`, or composite. Block findings roll back the write; warn findings emit `code_scan_warning`.

Operator-facing walkthrough: [`docs/sandbox.md`](docs/sandbox.md). Starter Cedar policies: [`examples/policies/`](examples/policies/).

#### RunConfig fields added in #42

| Field | Default | Notes |
|---|---|---|
| `executor.runtime` | `""` (engine default) | Closed set: `runc`, `runsc`, `kata`, `kata-qemu`, `kata-fc`. Only valid when `executor.type == "container"`; `ValidateRunConfig` rejects the field on `local` / `api` executors. |
| `permissionPolicy.type` (extended) | unchanged | Adds `policy-engine` alongside the existing three. Requires `policyFile`; rejects `..` traversal segments; `policyFile` set with any other type is a hard error. |
| `permissionPolicy.policyFile` | (none) | Filesystem path to the Cedar policy file. Absolute paths are operator-managed; workspace-relative paths are resolved against `executor.workspace`. |
| `permissionPolicy.fallback` | `deny-side-effects` (when `policy-engine`) | Closed set: `allow-all`, `deny-side-effects`, `ask-upstream`. Chained policy engines are rejected. |
| `ruleOfTwo.enforce` | `nil` (enforce) | `*bool` so unset is wire-distinguishable from `false`. The proto field is declared `optional` for the same reason. The factory emits `rule_of_two_disabled` when enforcement is overridden and `rule_of_two_warning` when two of three flags hold without override. |
| `codeScanner.type` | mode-aware (`patterns` for execution, `none` for read-only) | Closed set: `none`, `patterns`, `semgrep`, `composite`. Composite requires `codeScanner.scanners` (each entry from the non-composite set). |
| `codeScanner.blockOnWarn` | `false` | Promotes warn findings to block; useful for production pinning. |
| `codeScanner.semgrepConfigPath` | `""` (passes `--config auto`) | Local rules-bundle path. Set this for air-gapped deployments and supply-chain pinning — `auto` reaches out to `semgrep.dev` at scan time. See `docs/sandbox.md`. |

## Security Foundations

- **SecretStore**: resolves `secret://` references (env vars, files, AWS SSM via `secret://ssm:///param-name`). `AutoSecretStore` routes by scheme, only initialising SSM client when config refs require it. API keys never stored in RunConfig.
- **LogScrubber**: regex-based redaction of 7 secret patterns in all log/trace output.
- **Input validation**: JSON Schema Draft 2020-12 validation via `santhosh-tekuri/jsonschema`, with external schema loading disabled and prototype pollution keys stripped.
- **RunConfig validation**: hard security invariants (read-only modes must use restrictive permissions, bounded maxTurns/timeout, FollowUpGrace <= 3600s, MaxCostBudget <= $100, MaxTokenBudget <= 50M).
- **HTTP client timeouts**: all provider adapters (Anthropic, OpenAI, Bedrock) and MCP client use explicit HTTP clients with timeouts (120s streaming, 30s MCP) — never `http.DefaultClient`.
- **Environment filtering**: command execution allowlists 27 safe env vars; blocks all API keys and cloud credentials.
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

A `Justfile` is provided for common tasks (requires [just](https://github.com/casey/just)):
```sh
just              # build + test (default)
just build        # build stirrup + eval binaries
just test         # go test ./harness/... ./types/... ./eval/...
just lint         # golangci-lint
just proto        # buf generate
just buf-lint     # buf lint
just docker       # build stirrup Docker image
just clean        # remove built binaries
```

Or directly:
```sh
go test ./harness/... ./types/... ./eval/...    # Run all tests
go build ./harness/... ./types/... ./eval/...   # Build all packages
buf generate             # Regenerate proto code (after editing .proto files)
buf lint                 # Lint proto files
```

### CI

GitHub Actions at `.github/workflows/ci.yml`:
- **verify** job: delegates to the reusable `_verify.yml` workflow, which runs `go test` for types, harness, and eval modules and builds the stirrup and eval binaries with `-trimpath` and `types/version` ldflags (on every push)
- **eval-gate** job: builds binaries, runs eval suites from `eval/suites/`, compares against baselines in `eval/baselines/`, uploads results as artifacts (on main branch push, after verify passes)
- **publish-container** job: builds and pushes Docker image to `ghcr.io/rxbynerd/stirrup` (on main branch push only, after verify passes)

### Releases

Releases are produced by `.github/workflows/release.yml`, triggered by pushing a `v*.*.*` tag (or via `workflow_dispatch` against an existing tag for retries):

```sh
git tag -a v1.2.3 -m "Release notes"
git push origin v1.2.3
```

The workflow re-runs `_verify.yml`, then in parallel cross-compiles `stirrup` and `stirrup-eval` for linux/{amd64,arm64}, darwin/{amd64,arm64}, windows/amd64 (tar.gz on Unix, zip on Windows), generates SPDX + CycloneDX SBOMs via `anchore/sbom-action`, and renders a changelog from `git log` since the previous tag (capped at 100 lines). A `release` job aggregates all artifacts into a single `SHA256SUMS` file and publishes a GitHub Release. Tags containing `-` (e.g. `v1.2.3-rc1`) are marked as prereleases automatically.

Version-label conventions injected via `-X github.com/rxbynerd/stirrup/types/version.version` and `...commit`:

| Build origin | `Full()` output |
|---|---|
| `release.yml` on a tag | `v1.2.3 (ab74b75)` |
| `ci.yml` on `refs/heads/main` | `main (ab74b75)` |
| `ci.yml` on any other ref | `dev (ab74b75)` |
| `go build` / `go run` locally | `dev` |

Artifact signing (cosign / Sigstore) is intentionally out of scope; a commented-out signing seam sits in `release.yml` between the SHA256SUMS step and the release-create step.

### Known issue: gopls false positives

The LSP (gopls) frequently reports false positive diagnostics due to the `go.work` workspace module resolution. Errors referencing packages like `NewComposedPromptBuilder not declared by package prompt` or fields from other modules are almost always false. Always verify with `go build` and `go test` — if they pass, the LSP errors are spurious.

## External dependencies rationale

The project follows a minimal-dependency philosophy. Provider adapters and the container executor use hand-rolled HTTP clients against well-documented REST APIs rather than pulling in large SDK dependency trees. This is deliberate — for a security-sensitive harness that holds API keys and executes code, minimising the dependency surface is worth the cost of writing a few hundred lines of HTTP client code.

Exceptions where external deps are accepted:
- `github.com/spf13/cobra` for CLI framework (production-grade subcommand routing, help generation, flag parsing)
- `github.com/santhosh-tekuri/jsonschema/v6` for full JSON Schema validation
- `aws-sdk-go-v2` for Bedrock and SSM SecretStore (IAM SigV4 auth is complex enough to justify)
- `google.golang.org/grpc` + `google.golang.org/protobuf` for gRPC transport (the reference Go gRPC implementation)
- `go.opentelemetry.io/otel` + OTLP exporter for OpenTelemetry trace and metrics (the reference OTel SDK)

## Lint policy

golangci-lint v2 is configured via `.golangci.yml`. `just lint` runs the linter across all workspace modules.

When resolving lint findings, understand the code's intent before changing it:

- **Linter suggestions are not mandates.** Diagnostics prefixed `QF` (quick-fix), `S` (simplification), or `SA` (static analysis suggestion) may conflict with deliberate patterns like compile-time type assertions, intentional sentinel patterns, or defensive coding. If suppressing with `//nolint:<linter> // <reason>` preserves the original intent better than the suggested rewrite, prefer the nolint directive.
- **Never weaken a safety mechanism to satisfy a linter.** Compile-time type checks (`var _ T = expr`), interface satisfaction guards (`var _ Interface = (*Impl)(nil)`), and deliberate panics in unreachable branches exist for a reason. If a linter flags them, suppress with a comment explaining the intent.
- **Treat auto-fix output as a draft.** `golangci-lint fmt` and `--fix` can rewrite code mechanically. Review the diff for semantic changes beyond formatting, especially in test assertions and type-checked expressions.
- **Check for cascading breakage.** Removing an "unused" symbol may break a compile-time contract or a test helper reserved for future use in an in-progress branch. Grep for the symbol and read surrounding comments before deleting.
