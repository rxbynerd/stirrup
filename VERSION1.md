# VERSION1: Coding Harness Redesign

## Why not Ruby?

Ruby is a poor fit for a cloud-deployed autonomous coding harness:

1. **GIL** — MRI's Global Interpreter Lock makes true parallelism impossible within a single process. A harness that needs to run multiple tool calls concurrently, stream from model APIs, and serve multiple sessions is fighting the runtime.
2. **AI SDK ecosystem** — Anthropic and OpenAI both treat Python and TypeScript as first-class SDK targets. Ruby has no official SDKs; stirrup currently does raw HTTP/SSE parsing against the Anthropic API. Every model provider added means more hand-rolled client code.
3. **Cloud deployment** — Ruby's startup time, memory footprint, and dependency management (Bundler + native extensions) make it heavier than necessary for containerised workloads. No meaningful serverless story.
4. **Concurrency model** — Faye/WebSocket + Puma threads is workable but fragile. The current stirrup code already has two mutexes per connection to manage concurrent access. This gets worse, not better, as the system grows.

## Language recommendation: TypeScript (Node.js / Bun)

**TypeScript** is the strongest fit for this project. Rationale:

| Factor | TypeScript | Go | Python |
|---|---|---|---|
| AI SDKs | Official Anthropic + OpenAI SDKs, both excellent | No official SDKs | Official SDKs, best ecosystem |
| Streaming | Native async iterators, ReadableStream | Goroutines, channels | asyncio, less ergonomic |
| JSON handling | Native — no serialisation layer | Verbose struct tags | Good (dicts) |
| Cloud deployment | Excellent (containers, Lambda, Cloud Run, Fly) | Best (single binary) | Good but dependency-heavy |
| MCP support | Official MCP TypeScript SDK | Community only | Official MCP Python SDK |
| Iteration speed | Fast — harness code is impermanent by nature | Slower (compile, verbose) | Fast |
| Type safety | Good enough (structural typing) | Excellent | Optional (mypy) |
| Process management | child_process, execa — adequate | Excellent (os/exec) | subprocess — adequate |

**Go** is the runner-up if we later need extreme concurrency or want single-binary deployment, but the AI SDK gap is significant — you'd be writing raw HTTP clients for every provider, which is exactly the problem stirrup has in Ruby today.

**Python** is the ecosystem leader for AI but has worse deployment ergonomics (virtualenvs, dependency conflicts, startup time) and its async story (asyncio) is more complex than Node's event loop for this kind of I/O-bound work.

**Runtime**: Start with Node.js (LTS). Bun is faster but less battle-tested for long-running server processes with WebSockets. Easy to switch later since the code is the same.

## Architecture

### Core principle: hybrid deterministic-agentic flows

The research is clear (Stripe's "blueprints" pattern, Princeton's findings): **use the LLM only when judgement is needed; everything else should be deterministic code**. The harness is a state machine with LLM calls at decision points, not an LLM with code bolted on.

### System layers

```
┌──────────────────────────────────────────────────┐
│  Entrypoints                                      │
│  CLI (local/interactive)  │  gRPC client (K8s job) │
└──────────┬───────────────────────────────────────-┘
           │
┌──────────▼───────────────────────────────────────-┐
│  Mode Router                                       │
│  Selects system prompt, tools, stop conditions,    │
│  output format based on task assignment             │
└──────────┬───────────────────────────────────────-┘
           │
┌──────────▼───────────────────────────────────────-┐
│  Core Loop (the "harness")                         │
│  - Message history + context management            │
│  - Stream model response                           │
│  - Dispatch tool calls (built-in + MCP)            │
│  - Append results                                  │
│  - Check termination conditions                    │
│  - Emit events upstream (gRPC stream / stdout)     │
└──────────┬───────────────────────────────────────-┘
           │
┌──────────▼───────────────────────────────────────-┐
│  Provider Adapters                                 │
│  Anthropic │ Bedrock Converse │ OpenAI-compatible   │
│  Common interface: stream(messages, tools) →        │
│    AsyncIterable<StreamEvent>                       │
└──────────┬───────────────────────────────────────-┘
           │
┌──────────▼───────────────────────────────────────-┐
│  Tool Registry                                     │
│  Built-in tools + MCP server connections           │
│  Sandboxed execution (workspace-scoped)            │
└──────────────────────────────────────────────────-┘
```

The harness is a **short-lived job, not a server**. In cloud deployment, the control plane (a separate service) starts it as a K8s Job, passing the gRPC endpoint address as an environment variable. The harness connects outbound, receives its task assignment, streams events back, and exits on completion. For local development, a CLI entrypoint reads from stdin and writes to stdout.

### What each mode does

| Mode | System prompt focus | Tools available | Stop condition | Output |
|---|---|---|---|---|
| **Execution** | "You are a coding agent. Make changes, run tests, iterate until done." | All (read, write, shell, git, search) | `end_turn` or max turns | Code changes (committed to branch) |
| **Planning** | "Analyze the codebase and produce a step-by-step implementation plan." | Read-only (read, search, list, shell for `git log`/`git diff` etc.) | `end_turn` or max turns | Structured plan (markdown) |
| **Review** | "Review the following changes. Identify bugs, style issues, missed edge cases, and opportunities." | Read-only + git diff | `end_turn` | Structured review (issues + suggestions) |
| **Research** | "Research the following topic. Explore the codebase, read documentation, synthesize findings." | Read-only + web fetch | `end_turn` | Research brief (markdown) |
| **Toil** | "Check for {trigger}. Prepare a briefing for the engineer." | Git/API tools (PR list, diff, status) | `end_turn` | Structured briefing |

Each mode is just a partial `RunConfig` preset — it sets defaults for the components that vary by mode, while inheriting everything else from the base config:

```typescript
// A mode is a named set of RunConfig overrides, not a separate type.
// The control plane (or CLI) merges: baseConfig + modePreset + taskOverrides → RunConfig
interface ModePreset {
  name: string;
  promptBuilder: PromptBuilderConfig;
  modelRouter: ModelRouterConfig;
  tools: ToolsConfig;
  editStrategy: EditStrategyConfig;
  verifier: VerifierConfig;
  permissionPolicy: PermissionPolicyConfig;
  maxTurns: number;
}
```

This means modes are not special — they're just saved configurations. You can create new modes, fork existing ones, or override any field per-task.

## Swappable components

The harness is a composition of pluggable components. Every component below has an interface; the core loop depends only on interfaces, never on concrete implementations. This is what makes experimentation possible: change one component, keep everything else the same, compare results.

### Component map

| # | Component | What it does | Interface | Implementations |
|---|---|---|---|---|
| 1 | **Model provider** | Streams completions from an LLM | `ProviderAdapter` | Anthropic (direct), AWS Bedrock Converse, OpenAI-compatible (covers OpenAI, LiteLLM, Azure OpenAI, vLLM, Ollama) |
| 2 | **Model router** | Selects which provider + model to use per turn | `ModelRouter` | Static (one model), per-mode, dynamic (complexity-based) |
| 3 | **System prompt** | Assembles the system prompt from templates + context | `PromptBuilder` | Static string, templated, composed from fragments |
| 4 | **Context strategy** | Manages message history and compaction | `ContextStrategy` | Sliding window, LLM-summarise, offload-to-file |
| 5 | **Tool registry** | Resolves tool definitions and dispatches calls | `ToolRegistry` | Built-in, MCP, hybrid |
| 6 | **Executor (sandbox)** | Runs shell commands and file I/O | `Executor` | Local process, Docker container, Firecracker/E2B |
| 7 | **Edit strategy** | How the model applies file changes | `EditStrategy` | Whole-file rewrite, search-replace, unified diff, patch |
| 8 | **Verifier** | Validates the run's output before completion | `Verifier` | None, test-runner, linter, LLM-as-judge, composite |
| 9 | **Permission policy** | Decides what to do when a side-effecting tool is called | `PermissionPolicy` | Allow-all, deny-side-effects, ask-upstream |
| 10 | **Transport** | Streams events to/from the control plane | `Transport` | gRPC bidi, stdio, (A2A via control plane) |
| 11 | **Git strategy** | Manages branches, commits, push lifecycle | `GitStrategy` | Model-driven, deterministic, hybrid (TBD — see resolved decisions) |
| 12 | **Trace emitter** | Records structured telemetry for each run | `TraceEmitter` | JSONL file, OpenTelemetry, in-memory (tests) |

### Interface definitions

```typescript
// --- 1. Model provider ---
//
// Three concrete adapters cover the required API surfaces:
//
// AnthropicAdapter — Anthropic Messages API (direct). Uses @anthropic-ai/sdk.
//   For: Claude models via api.anthropic.com.
//   Auth: API key (x-api-key header).
//
// BedrockConverseAdapter — AWS Bedrock Converse API. Uses @aws-sdk/client-bedrock-runtime.
//   For: Claude, Llama, Mistral, etc. via Bedrock. Many enterprises require Bedrock
//   for compliance/governance (IAM policies, CloudTrail audit, VPC endpoints).
//   Auth: AWS IAM (SigV4) — typically via instance role, IRSA, or env credentials.
//   Note: Bedrock uses different model IDs (e.g. "anthropic.claude-sonnet-4-6-v1")
//   and the Converse API has its own message/tool format. The adapter translates
//   between our internal Message/ToolDefinition types and Bedrock's wire format.
//
// OpenAICompatibleAdapter — OpenAI Chat Completions API. Uses openai SDK.
//   For: OpenAI GPT models (native), plus any OpenAI-compatible endpoint:
//   - LiteLLM (proxy for 100+ providers behind a single OpenAI-compatible API)
//   - Azure OpenAI (different base URL + API version header)
//   - vLLM, Ollama, llama.cpp server (local inference with OpenAI-compatible API)
//   Auth: API key (Authorization: Bearer). Base URL is configurable.
//
interface ProviderAdapter {
  stream(params: {
    model: string;
    system: string;
    messages: Message[];
    tools: ToolDefinition[];
    maxTokens: number;
    temperature: number;
  }): AsyncIterable<StreamEvent>;
}

type StreamEvent =
  | { type: "text_delta"; text: string }
  | { type: "tool_call"; id: string; name: string; input: Record<string, unknown> }
  | { type: "message_complete"; stopReason: string; content: ContentBlock[] }
  | { type: "error"; error: Error };

// --- 2. Model router ---
// Selects provider + model for each turn based on context.
// Simplest implementation returns a static value. Advanced implementations
// could route cheap turns (simple tool calls) to Haiku and complex reasoning
// to Opus, or A/B test models.
interface ModelRouter {
  select(context: {
    mode: string;
    turn: number;
    lastStopReason?: string;
    tokenUsage: { input: number; output: number };
  }): { provider: string; model: string };
}

// --- 3. System prompt ---
// Assembles the final system prompt. Allows composition from reusable
// fragments (role preamble, tool usage instructions, output format,
// workspace description) and injection of dynamic context (file tree,
// git status, plan progress).
interface PromptBuilder {
  build(context: {
    mode: string;
    workspace: string;
    dynamicContext?: Record<string, string>;  // injected by control plane or previous phases
  }): string;
}

// --- 4. Context strategy ---
// Controls how message history is managed as it approaches token limits.
// The loop calls `prepare()` before each model call, giving the strategy
// a chance to compact, summarise, or offload messages.
interface ContextStrategy {
  prepare(messages: Message[], budget: {
    maxTokens: number;
    currentTokens: number;
    reserveForResponse: number;
  }): Promise<Message[]>;
}

// --- 5. Tool registry ---
// (Already defined — built-in Tool interface + MCP client)
interface Tool {
  name: string;
  description: string;
  inputSchema: JSONSchema;
  sideEffects: boolean;
  handler: (input: unknown) => Promise<string>;
}

interface ToolRegistry {
  list(): ToolDefinition[];
  resolve(name: string): Tool | undefined;
}

// --- 6. Executor (sandbox) ---
// Abstracts where commands run and how files are accessed.
// The built-in filesystem and shell tools delegate to this interface
// rather than calling fs/child_process directly.
interface Executor {
  readFile(path: string): Promise<string>;
  writeFile(path: string, content: string): Promise<void>;
  listDirectory(path: string): Promise<string[]>;
  exec(command: string, opts?: { timeout?: number }): Promise<{
    exitCode: number;
    stdout: string;
    stderr: string;
  }>;
  resolvePath(relativePath: string): string;  // workspace-scoped path resolution
}

// --- 7. Edit strategy ---
// Controls how the model's intent to modify a file is translated into
// actual file changes. Research shows this is critical: fuzzy patching
// reduces errors 9x (Aider), and edit format choice affects model accuracy
// by 30-50%.
//
// The edit strategy is both a tool definition (what the model sees) and
// an applicator (how the harness applies it).
interface EditStrategy {
  toolDefinition(): ToolDefinition;          // the edit tool exposed to the model
  apply(input: unknown, executor: Executor): Promise<EditResult>;
}

type EditResult = {
  path: string;
  applied: boolean;
  diff?: string;       // unified diff of what changed
  error?: string;      // if applied is false
};

// --- 8. Verifier ---
// Runs after the agentic loop completes (or between iterations) to validate
// the output. Can trigger re-entry into the loop with verification feedback.
interface Verifier {
  verify(context: {
    mode: string;
    executor: Executor;
    messages: Message[];
    artifacts: Artifact[];       // files changed, plans produced, etc.
  }): Promise<VerificationResult>;
}

type VerificationResult = {
  passed: boolean;
  feedback?: string;             // fed back into the loop if not passed
  details?: Record<string, unknown>;
};

// --- 9. Permission policy ---
// Called before executing any tool with sideEffects: true.
// "ask-upstream" sends a request to the control plane via the transport
// and waits for approval — this is how interactive confirmation works.
interface PermissionPolicy {
  check(tool: ToolDefinition, input: unknown): Promise<
    | { allowed: true }
    | { allowed: false; reason: string }
  >;
}

// --- 10. Transport ---
interface Transport {
  emit(event: HarnessEvent): void;
  onControl(handler: (event: ControlEvent) => void): void;
  close(): Promise<void>;
}

// --- 11. Git strategy ---
// (Interface reserved — implementation deferred pending research)
interface GitStrategy {
  setup(workspace: string, taskId: string): Promise<void>;    // e.g. create branch
  checkpoint(message: string): Promise<void>;                  // e.g. commit
  finalise(): Promise<{ branch: string; sha: string }>;        // e.g. push
}

// --- 12. Trace emitter ---
interface TraceEmitter {
  start(runId: string, config: RunConfig): void;
  recordTurn(turn: TurnTrace): void;
  recordToolCall(call: ToolCallTrace): void;
  finish(outcome: "success" | "error" | "max_turns"): Promise<RunTrace>;
}
```

### RunConfig: the composition root

Every run is fully described by a `RunConfig` — a declarative specification of which implementation to use for each component. This is what the control plane sends (via the `TaskAssignment` in the gRPC contract) and what the CLI builds from flags/env.

```typescript
interface RunConfig {
  // Identity
  runId: string;
  mode: string;                            // "execution" | "planning" | "review" | "research" | "toil"

  // What to do
  prompt: string;
  dynamicContext?: Record<string, string>; // injected context (e.g. PR diff, codebase summary)

  // Component selections — each is a discriminated union of implementation + config
  provider: ProviderConfig;                // { type: "anthropic", apiKeyRef: "secret://anthropic-key" } | { type: "bedrock", region, profile? } | { type: "openai-compatible", apiKeyRef: "secret://openai-key", baseUrl? }
  modelRouter: ModelRouterConfig;          // { type: "static", provider: "anthropic", model: "claude-sonnet-4-6" }
  promptBuilder: PromptBuilderConfig;      // { type: "default" } | { type: "custom", fragments: [...] }
  contextStrategy: ContextStrategyConfig;  // { type: "sliding-window", maxTokens: 180000 }
  executor: ExecutorConfig;                // { type: "local", workspace: "/path" } | { type: "docker", image: "..." }
  editStrategy: EditStrategyConfig;        // { type: "whole-file" } | { type: "search-replace" } | { type: "udiff" }
  verifier: VerifierConfig;                // { type: "none" } | { type: "test-runner", command: "npm test" }
  permissionPolicy: PermissionPolicyConfig;// { type: "allow-all" } | { type: "deny-side-effects" }
  gitStrategy: GitStrategyConfig;          // { type: "none" } | { type: "deterministic", baseBranch: "main" }
  traceEmitter: TraceEmitterConfig;        // { type: "jsonl", path: "/traces" } | { type: "otel", endpoint: "..." }
  tools: ToolsConfig;                      // { builtins: [...], mcp: [{ uri: "..." }] }

  // Limits
  maxTurns: number;
  maxTokenBudget?: number;                 // hard cap on total tokens (input + output)
  maxCostBudget?: number;                  // hard cap on estimated cost ($)
  timeout?: number;                        // wall-clock timeout in seconds
}
```

The harness entrypoint (CLI or K8s job) deserialises a `RunConfig`, constructs the concrete implementations via a factory/registry, and hands them to the core loop. The loop only ever sees interfaces.

**Why this matters for experimentation:** to compare Opus vs Sonnet on planning tasks, or search-replace vs whole-file editing on execution tasks, you change one field in the RunConfig. The traces from each run are directly comparable because everything else is held constant.

### Updated project structure

```
harness/
  package.json
  tsconfig.json
  src/
    core/
      loop.ts             # The ReAct agentic loop — depends only on interfaces
      types.ts            # Shared types: Message, ToolCall, ToolResult, ContentBlock, etc.
      factory.ts          # Constructs concrete components from RunConfig
    providers/
      interface.ts        # ProviderAdapter interface
      anthropic.ts        # Anthropic Messages API (uses @anthropic-ai/sdk)
      bedrock.ts          # AWS Bedrock Converse API (uses @aws-sdk/client-bedrock-runtime)
      openai-compatible.ts # OpenAI Chat Completions API (uses openai SDK, configurable baseUrl)
    router/
      interface.ts        # ModelRouter interface
      static.ts           # Always returns one provider+model
      per-mode.ts         # Maps mode -> provider+model
    prompts/
      interface.ts        # PromptBuilder interface
      default.ts          # Default prompt templates per mode
      composed.ts         # Assembles from fragments + dynamic context
    context/
      interface.ts        # ContextStrategy interface
      sliding-window.ts   # Drop oldest messages beyond token budget
      summarise.ts        # LLM-summarise old turns (uses provider adapter)
    tools/
      interface.ts        # Tool, ToolRegistry interfaces
      registry.ts         # Concrete registry (built-in + MCP)
      builtins/
        filesystem.ts     # read_file, write_file, list_directory — delegates to Executor
        search.ts         # grep, glob — delegates to Executor
        shell.ts          # run_command — delegates to Executor
        web-fetch.ts      # HTTP fetch -> markdown
    executor/
      interface.ts        # Executor interface
      local.ts            # Direct fs + child_process (workspace-scoped)
      docker.ts           # Docker container exec
    edit/
      interface.ts        # EditStrategy interface
      whole-file.ts       # Model writes entire file content
      search-replace.ts   # Model specifies old_string/new_string pairs
      udiff.ts            # Model produces unified diff, harness applies
    verifier/
      interface.ts        # Verifier interface
      none.ts             # No verification (default)
      test-runner.ts      # Run a test command, parse exit code + output
      composite.ts        # Chain multiple verifiers
    permissions/
      interface.ts        # PermissionPolicy interface
      allow-all.ts        # No restrictions
      deny-side-effects.ts # Block all side-effecting tools (for planning/review)
      ask-upstream.ts     # Send approval request via transport, wait for response
    git/
      interface.ts        # GitStrategy interface
      none.ts             # No git management
      # (implementations deferred pending research)
    transport/
      interface.ts        # Transport interface
      grpc.ts             # gRPC bidi streaming client
      stdio.ts            # JSON lines to stdout, reads stdin
    trace/
      interface.ts        # TraceEmitter interface
      jsonl.ts            # Append to JSONL file
      otel.ts             # OpenTelemetry export
    security/
      secret-store.ts     # SecretStore interface + env-var backend
      log-scrubber.ts     # Regex-based secret redaction for logs and recordings
      input-validator.ts  # JSON Schema validation + prototype key stripping for tool inputs
      config-validator.ts # RunConfig security invariant checks
    mcp/
      client.ts           # MCP client: connects to MCP servers, registers tools
    proto/
      harness.proto       # Protobuf service + message definitions
    cli/
      index.ts            # CLI entrypoint — builds RunConfig from flags/env
    job.ts                # K8s job entrypoint — builds RunConfig from TaskAssignment
    config.ts             # RunConfig type + deserialisation + validation
    index.ts              # Library entrypoint
  Dockerfile
```

Note: this shows the `harness` package structure only. See "Project structure: monorepo with packages" under the Experimentation section for the full monorepo layout including `types` and `eval` packages.

## Key design decisions

### 1. The core loop is a pure function of its interfaces

The loop receives all its dependencies as constructor arguments. It has no imports from concrete implementations, no environment variable reads, no direct file system access. This is what makes every component independently swappable.

```typescript
class AgenticLoop {
  constructor(
    private provider: ProviderAdapter,
    private router: ModelRouter,
    private promptBuilder: PromptBuilder,
    private context: ContextStrategy,
    private tools: ToolRegistry,
    private executor: Executor,
    private verifier: Verifier,
    private permissions: PermissionPolicy,
    private git: GitStrategy,
    private transport: Transport,
    private trace: TraceEmitter,
  ) {}

  async run(config: RunConfig): Promise<RunTrace> { ... }
}
```

### 2. Edit strategy is a first-class component

Research is emphatic that edit format is one of the highest-leverage harness decisions (source: Aider benchmark data — disabling fuzzy patching increases errors 9x; "high-level diffs" reduce errors 30-50% vs line-level). Yet the current stirrup codebase has no edit abstraction at all — the model just calls `write_file` with the entire file content.

Three strategies to implement, in order of priority:

| Strategy | How it works | When to use |
|---|---|---|
| **Whole-file** | Model writes the entire file. Simple, no ambiguity, but wasteful on large files and prone to the model "forgetting" parts. | Small files, new file creation |
| **Search-replace** | Model specifies `old_string` / `new_string` pairs with surrounding context for anchoring. This is what Claude Code uses. | Surgical edits to existing files |
| **Unified diff** | Model produces a unified diff. Harness applies with multi-strategy fallback (exact -> whitespace-insensitive -> fuzzy Levenshtein). | Large multi-hunk edits |

The `EditStrategy` interface means we can A/B test these per model (some models produce better diffs than others) and fall back gracefully.

### 3. Verification closes the feedback loop

The current proposal has the loop running until `end_turn` or `max_turns` — the model decides when it's done. But the model doesn't know if the code compiles, tests pass, or the output meets requirements. The `Verifier` component adds an outer loop:

```
repeat {
  run agentic loop until model says "done"
  run verifier
  if verifier passes → done
  if retries exhausted → done (with failure)
  else → feed verifier feedback back into the loop as a user message
}
```

This is the pattern Spotify's Honk, Stripe's Minions, and Codex all use. The verifier is pluggable:

- **TestRunner**: `npm test`, `pytest`, `bundle exec rspec` — parse exit code and output
- **LLMJudge**: a separate (cheap) model reviews the diff against the original prompt for scope creep and correctness
- **Composite**: chain both — tests must pass AND judge must approve

### 4. Context strategy prevents degradation

The research data is stark: correctness degrades around 32K tokens even in models with much larger windows (Databricks study on Llama 3.1 405B), and Spotify's agent "forgot the original task after a few turns." Context management is not optional.

The `ContextStrategy` interface lets us experiment with different approaches:

| Strategy | How it works | Trade-off |
|---|---|---|
| **Sliding window** | Drop oldest messages when budget exceeded | Simple, but loses early context (including the task description) |
| **LLM-summarise** | Summarise old turns into a condensed message | Preserves meaning, but costs an extra LLM call and can lose detail |
| **Offload-to-file** | Write full tool results to workspace files, replace in-context with "see file X" | Preserves detail if the model reads the file, but adds a tool-call round-trip |

All strategies should preserve the system prompt and the most recent N turns in full (recency bias is real and useful). The Manus pattern (rewriting a `progress.md` file each turn) can be layered on top of any strategy.

### 5. Tiered sandbox model

Stirrup currently has no real sandbox. The path traversal guard (`workspace_path`) and command blocklist in `server.rb` are best-effort filters running in the same process, same user, same filesystem and network as the harness itself. A prompt injection that gets past the regex blocklist — trivial with `python -c "import os; ..."` — has full access to the API key, the network, and the host filesystem.

The VERSION1 `Executor` interface already abstracts where tools run. The key design addition: **the sandbox tier is a property of the RunConfig, selected per-task based on risk**. Different modes and trust levels get fundamentally different isolation.

#### Sandbox tiers

**Tier 0: API-backed virtual executor (no sandbox, no filesystem)**

For read-only modes (research, review, planning) against repos the harness doesn't need to clone. The model never touches a real filesystem — `read_file`, `search_files`, and `list_directory` are backed by API calls (GitHub/GitLab API, or any VCS provider).

```typescript
interface VcsBackend {
  readFile(repo: string, ref: string, path: string): Promise<string>;
  listDirectory(repo: string, ref: string, path: string): Promise<string[]>;
  searchCode(repo: string, query: string): Promise<SearchResult[]>;
  getDiff(repo: string, base: string, head: string): Promise<string>;
}
```

Properties:
- No clone required (saves 30s+ of startup for large repos)
- No sandbox required (nothing to escape from — there's no local process, no filesystem)
- `write_file` and `exec` are not available (the tool registry simply doesn't include them)
- `sideEffects: false` on all tools, enforced by the `deny-side-effects` permission policy
- Rate limits are the only constraint (GitHub API: 5000 req/hr authenticated)
- Prompt injection risk: **minimal** — the model can read code but can't execute anything, write anything, or reach the network

This is appropriate for: research mode, review mode, planning mode against known repos, toil mode (PR briefings).

**Tier 1: Workspace-scoped local process (current stirrup model)**

For trusted environments: local development, internal CI, tasks against your own repos where the prompt source is trusted (you wrote it).

```typescript
class LocalExecutor implements Executor {
  // Path traversal guard (same as stirrup's workspace_path)
  // Command blocklist + metacharacter rejection
  // Timeout enforcement
  // Output capping
}
```

Properties:
- Same process, same user, same network
- Path guard prevents accidental escapes but is not a security boundary
- Good enough for your own laptop or a trusted CI runner
- **Not appropriate for untrusted inputs** (external issues, open-source contributions, user-submitted prompts)

This is appropriate for: local interactive use, internal CI smoke evals.

**Tier 2: Container sandbox (Docker + gVisor or seccomp)**

For execution/debugging modes where the prompt or target repo might contain adversarial content (issues filed by external users, repos with untrusted `.github` configs, dependency code the model might read and be influenced by).

The harness process runs **outside** the container. Only tool execution happens inside. This is the critical architectural boundary:

```
┌─────────────────────────────────────────────┐
│  Harness process (trusted)                   │
│  - Holds API keys                            │
│  - Streams from model API                    │
│  - Makes tool dispatch decisions             │
│  - Emits events to control plane             │
│                                              │
│  ┌─────────────────────────────────────────┐ │
│  │  Container (untrusted)                   │ │
│  │  - Workspace mounted as volume           │ │
│  │  - Tool execution happens here           │ │
│  │  - No access to API keys                 │ │
│  │  - Network: egress deny-all or allowlist │ │
│  │  - Resource limits: CPU, mem, disk, PIDs │ │
│  │  - Read-only root filesystem             │ │
│  │  - No privileged capabilities            │ │
│  └─────────────────────────────────────────┘ │
└─────────────────────────────────────────────┘
```

```typescript
class ContainerExecutor implements Executor {
  constructor(private config: {
    image: string;                        // base image with language runtimes
    workspace: string;                    // host path, mounted as /workspace
    network: "none" | { allowlist: string[] };  // egress control
    resources: {
      cpus: number;                       // e.g. 2
      memoryMb: number;                   // e.g. 4096
      diskMb: number;                     // e.g. 10240
      pids: number;                       // e.g. 256
    };
    timeout: number;                      // container-level kill after N seconds
  }) {}

  // All methods execute via `docker exec` against the running container.
  // The container is started once at the beginning of the run and killed at the end.
  async exec(command: string, opts?: { timeout?: number }): Promise<ExecResult> {
    // No blocklist needed — the container IS the boundary.
    // Network isolation, filesystem isolation, and resource limits are
    // enforced by the container runtime, not by regex filters.
  }
}
```

Properties:
- Filesystem isolation: the container only sees /workspace (mounted volume) + read-only root
- Network isolation: `--network none` or a custom network with egress allowlist (e.g. allow npm registry, deny everything else)
- Resource isolation: CPU, memory, disk, PID limits prevent resource exhaustion
- **API keys are never inside the container.** The harness calls the model API from outside; the container only runs tool commands.
- The command blocklist becomes unnecessary — the container runtime enforces boundaries, not regex patterns
- Startup cost: ~1-3s for a warm container (pre-pulled image, reused between turns within a run)

This is appropriate for: execution mode, debugging mode, any task where the prompt source or repo content isn't fully trusted.

**Tier 3: MicroVM (Firecracker / E2B / Kata Containers)**

For maximum isolation: untrusted inputs from the public internet, multi-tenant scenarios, or compliance requirements that demand hardware-level isolation.

Same architecture as tier 2 (harness outside, execution inside) but with a VM boundary instead of a container boundary. Properties:
- Hardware-level isolation (separate kernel)
- Snapshot/restore for sub-second startup (Firecracker specialty)
- Complete network isolation (only a proxy for model API calls if needed)
- Higher overhead (~100-500MB memory per VM vs ~10-50MB per container)

This is appropriate for: multi-tenant SaaS, processing untrusted public inputs, regulated environments.

Not designed in detail here — when we need it, the `Executor` interface means we add a new implementation without touching the loop, tools, or anything else. The tier 2 container executor proves the architecture; tier 3 is a drop-in upgrade.

#### Network egress: the critical control

Prompt injection's most dangerous outcome isn't corrupting the workspace (that's recoverable via git reset). It's **exfiltration**: sending secrets, code, or data to an attacker-controlled server. Every sandbox tier above 0 must control network egress.

| Tier | Network posture | Rationale |
|---|---|---|
| 0 (API-backed) | N/A — no local process | Model reads via VCS API only; harness controls what API calls are made |
| 1 (local) | **Unrestricted** — same as host | This is why tier 1 is only for trusted environments |
| 2 (container) | **Deny-all or allowlist** | Default: `--network none`. If the task needs package installs: allowlist specific registries (npm, PyPI, RubyGems). Never allow arbitrary egress. |
| 3 (microVM) | **Deny-all + proxy** | All external access goes through a logging proxy that enforces allowlists and records every request |

The allowlist is part of the `ExecutorConfig` in the RunConfig, so the control plane can set it per-task:

```typescript
type ExecutorConfig =
  | { type: "api"; vcsBackend: VcsBackendConfig }
  | { type: "local"; workspace: string }
  | { type: "container"; image: string; workspace: string;
      network: "none" | { allowlist: string[] };
      resources: ResourceLimits }
  | { type: "microvm"; image: string; workspace: string;
      network: { proxy: string; allowlist: string[] };
      resources: ResourceLimits };
```

#### Secret isolation

API keys (model provider, VCS, MCP servers) must **never** be accessible inside the sandbox. The architecture enforces this:

- The harness process holds all secrets and makes all external API calls (model API, VCS API, MCP connections)
- Tool execution inside the container/VM has no access to environment variables, mounted secret files, or network paths that could reach secret stores
- If a tool needs authenticated access (e.g. `git push` with a token), the harness injects a short-lived, narrowly-scoped credential into the container at runtime, and revokes it when the run ends
- The `GitStrategy` component (not the model) manages credential injection, so the model never sees the token in its context

#### How tiers map to modes

The default tier per mode, overridable in the RunConfig:

| Mode | Default tier | Rationale |
|---|---|---|
| Research | 0 (API-backed) | Read-only, no execution needed |
| Review | 0 (API-backed) | Read-only, inspects diffs |
| Planning | 0 (API-backed) or 1 (local) | Usually read-only; tier 1 if the model needs to run `git log` or explore build output |
| Toil | 0 (API-backed) | Read-only VCS/API operations |
| Execution | 2 (container) | Writes files, runs commands — needs real isolation |
| Debugging | 2 (container) | Same risk profile as execution |

The control plane can override this per-task. A research task against an untrusted repo might use tier 1 instead of tier 0 (if the VCS API doesn't support the needed search operations). An execution task against a fully-trusted internal repo in a locked-down CI environment might use tier 1 for speed.

#### Updated Executor interface

The interface is unchanged — that's the point. The tools call `executor.readFile()`, `executor.exec()`, etc., without knowing whether they're hitting the GitHub API, a local filesystem, or a Docker container. The sandbox tier is invisible to the model and to the tool implementations.

One addition: a `capabilities` method so the tool registry can adapt what tools are offered based on what the executor supports:

```typescript
interface Executor {
  // ... existing methods (readFile, writeFile, listDirectory, exec, resolvePath) ...

  // What this executor supports. The tool registry uses this to filter
  // which tools to offer the model. An API-backed executor reports
  // canWrite: false and canExec: false, so write_file and run_command
  // are never offered.
  capabilities(): {
    canRead: boolean;
    canWrite: boolean;
    canExec: boolean;
    canNetwork: boolean;       // can the executed process reach the network?
    maxTimeout: number;        // maximum command timeout in seconds
  };
}
```

This closes the loop: the executor determines what's possible, the tool registry determines what's offered, and the permission policy determines what's allowed. Three independent layers of control, all driven by the RunConfig.

### 6. Scheduling and toil delegation

Scheduling is the **control plane's responsibility**, not the harness's. The control plane decides when to dispatch toil jobs (cron, webhook trigger, event-driven). The harness simply supports the toil mode — it receives a toil task assignment via gRPC like any other mode, runs it, streams results back, and exits.

This keeps the harness single-purpose: receive task, execute, report, exit.

### 6. Observability

Every harness run produces a structured trace via the `TraceEmitter` interface:

```typescript
interface RunTrace {
  id: string;
  config: RunConfig;              // full config for reproducibility
  startedAt: Date;
  completedAt: Date;
  turns: number;
  tokenUsage: { input: number; output: number };
  toolCalls: { name: string; durationMs: number; success: boolean }[];
  verificationResults: VerificationResult[];
  outcome: "success" | "error" | "max_turns" | "verification_failed";
  cost: number;
}
```

The `config` field in the trace is critical — it records exactly which components and settings were used, making any run fully reproducible and comparable to other runs. The config is always passed through `RunConfig.redact()` before persistence, which replaces secret references with placeholder values (e.g. `"secret://anthropic-key"` → `"secret://[REDACTED]"`) so that traces, recordings, and lakehouse entries never contain resolvable credential pointers.

### 7. Security foundations

The sandbox tiers (section 5) provide the primary isolation boundary, but several security measures must be built into the harness from Phase 1 — they are cheap to implement early and expensive to retrofit. Post-V1 hardening (container image supply chain, MCP sandboxing, gRPC mutual authentication, network phase splitting, etc.) is documented separately in `SECURITY_HARDENING.md`.

#### Secret references, not inline secrets

The `RunConfig` never contains raw secrets. Provider configs, MCP server credentials, and VCS tokens are stored as references to a `SecretStore`:

```typescript
interface SecretStore {
  resolve(ref: string): Promise<string>;  // "secret://anthropic-key" → "sk-ant-..."
}

// Provider configs use references:
// { type: "anthropic", apiKeyRef: "secret://anthropic-key" }
// NOT: { type: "anthropic", apiKey: "sk-ant-..." }
```

The `SecretStore` interface has concrete implementations for different environments:
- **Environment variables** — `secret://FOO` resolves to `process.env.FOO`. Simplest, suitable for local dev and CI.
- **File-based** — `secret://file:///run/secrets/api-key` reads a mounted secret file. Suitable for Docker/K8s secrets.
- **Cloud KMS** — `secret://aws-ssm://param-name` or `secret://gcp-sm://secret-name`. For production deployments.

This ensures that `RunConfig` objects — which are serialised into traces, recordings, and lakehouse entries — never carry resolvable credentials. `RunConfig.redact()` strips even the reference URIs before persistence.

#### RunConfig validation with security invariants

The factory (`factory.ts`) validates every `RunConfig` before constructing components. Validation enforces hard security constraints that cannot be overridden by the control plane or CLI flags:

```typescript
function validateRunConfig(config: RunConfig): ValidationResult {
  const errors: string[] = [];

  // Tool input validation is mandatory — cannot be disabled
  // (enforced by the ToolRegistry, not configurable)

  // Read-only modes must use deny-side-effects or ask-upstream
  if (["planning", "review", "research", "toil"].includes(config.mode)) {
    if (config.permissionPolicy.type === "allow-all") {
      errors.push(`Mode "${config.mode}" requires a restrictive permission policy`);
    }
  }

  // maxTurns must be bounded
  if (config.maxTurns > 100) {
    errors.push("maxTurns exceeds maximum of 100");
  }

  // timeout must be set
  if (!config.timeout || config.timeout > 3600) {
    errors.push("timeout is required and must be <= 3600 seconds");
  }

  return { valid: errors.length === 0, errors };
}
```

These invariants prevent misconfiguration from creating security holes. The factory rejects invalid configs before any component is constructed.

#### Tool input validation

The `ToolRegistry` validates every tool call's `input` against the tool's `inputSchema` before dispatching to the handler. This is mandatory and cannot be disabled.

```typescript
// In the core loop, before dispatching a tool call:
const tool = this.tools.resolve(call.name);
if (!tool) {
  return { error: `Unknown tool: ${call.name}` };
}

const validation = validateJsonSchema(call.input, tool.inputSchema);
if (!validation.valid) {
  return { error: `Invalid input for ${call.name}: ${validation.errors.join(", ")}` };
}

// Strip dangerous keys before passing to handler
const sanitisedInput = stripPrototypeKeys(call.input);
await tool.handler(sanitisedInput);
```

`stripPrototypeKeys` removes `__proto__`, `constructor`, and `prototype` keys from tool input objects to prevent prototype pollution. `validateJsonSchema` rejects inputs with unexpected fields (via `additionalProperties: false` on all tool schemas) and enforces type constraints.

#### Structured delimiters for untrusted content

The `PromptBuilder` wraps all `dynamicContext` values in structured delimiters before injecting them into the system prompt. This reduces the surface for prompt injection from external sources (issue bodies, PR descriptions, file contents injected as context).

```typescript
// PromptBuilder wraps dynamic context values:
function wrapUntrustedContext(key: string, value: string): string {
  return `<untrusted_context name="${key}">\n${value}\n</untrusted_context>`;
}
```

The system prompt instructs the model to treat content within `<untrusted_context>` tags as data, not instructions. This is not a security boundary — prompt injection cannot be fully prevented at the prompt level — but it raises the bar and makes injection attempts more visible in logs and recordings.

#### Executor resource limits

The `Executor` interface enforces hard limits on resource consumption, even at tier 1 (local):

| Resource | Limit | Enforcement |
|---|---|---|
| File read size | 10 MB per `readFile` call | Check `stat.size` before reading; reject with error |
| File write size | 10 MB per `writeFile` call | Check `content.length` before writing |
| Command output | 1 MB combined stdout + stderr | Truncate and append `[output truncated at 1MB]` |
| Command timeout | 30s default, configurable up to `maxTimeout` | `child_process` timeout + SIGKILL after grace period |
| Symlink resolution | `resolvePath` calls `realpath()` and verifies canonical path is within workspace | Prevents symlink traversal attacks (e.g. `/workspace/link` → `/etc/shadow`) |

These limits apply to all executor implementations. The `Executor` interface documents them as invariants that concrete implementations must enforce.

#### Log scrubbing

All structured log events pass through a `LogScrubber` before emission. The scrubber applies regex-based redaction to all string values in the `data` field:

```typescript
const SECRET_PATTERNS = [
  /sk-ant-[a-zA-Z0-9_-]+/g,        // Anthropic API keys
  /ghp_[a-zA-Z0-9]+/g,              // GitHub personal access tokens
  /ghs_[a-zA-Z0-9]+/g,              // GitHub app installation tokens
  /AKIA[A-Z0-9]{16}/g,              // AWS access key IDs
  /Bearer\s+[a-zA-Z0-9._-]+/g,      // Bearer tokens
  /-----BEGIN\s+\w+\s+KEY-----/g,    // PEM private keys
  /secret:\/\/[^\s"']+/g,            // Secret store references
];

function scrub(value: string): string {
  return SECRET_PATTERNS.reduce(
    (v, pattern) => v.replace(pattern, "[REDACTED]"),
    value,
  );
}
```

The same scrubber is applied to `RunRecording` data before persistence. This provides defense in depth — even if a tool result contains a secret read from a file, the secret is redacted before it reaches logs, traces, or recordings.

#### Security event logging

A dedicated `security` log component emits events for anomalous or security-relevant actions:

| Event | Level | Data | Trigger |
|---|---|---|---|
| `path_traversal_blocked` | warn | `{ path, resolvedPath, workspace }` | `resolvePath` detects escape attempt |
| `tool_input_rejected` | warn | `{ tool, errors }` | Schema validation fails on tool input |
| `prototype_pollution_blocked` | warn | `{ tool, keys }` | `__proto__`/`constructor` keys stripped from input |
| `config_validation_failed` | error | `{ errors }` | RunConfig fails security invariant checks |
| `secret_redacted_in_output` | info | `{ pattern, location }` | LogScrubber detected and redacted a secret pattern |
| `file_size_limit_exceeded` | warn | `{ path, size, limit }` | File read/write blocked by size limit |
| `output_truncated` | info | `{ command, originalSize, limit }` | Command output exceeded cap |

These events feed into the alerting system (see Observability & monitoring). A spike in `path_traversal_blocked` or `tool_input_rejected` events may indicate an active prompt injection attempt.

## Experimentation framework

The swappable component architecture and RunConfig/RunTrace types provide the *mechanism* for experimentation — change one field, compare traces. But mechanism isn't enough. You also need a way to define experiments, run them reproducibly, and draw conclusions from results. This section describes the missing pieces.

### The experimentation loop

```
Define eval suite (tasks + expected outcomes)
    │
    ▼
Define experiment (baseline config + variant configs)
    │
    ▼
Run each task × each variant × N repetitions
    │
    ▼
Collect RunTraces + recorded tool calls
    │
    ▼
Compute metrics + compare variants
    │
    ▼
Decide: adopt variant, reject, or investigate further
```

### Eval suites

An eval suite is a collection of tasks, each with a reproducible starting state and a way to judge the outcome. This is the most important piece — without it, everything else is theatre.

```typescript
interface EvalSuite {
  id: string;
  description: string;
  tasks: EvalTask[];
}

interface EvalTask {
  id: string;
  description: string;

  // Starting state — a git ref that the workspace is reset to before each run.
  // This makes runs reproducible regardless of what previous runs did.
  repo: string;                    // git remote URL or local path
  ref: string;                     // commit SHA, tag, or branch

  // What to do
  prompt: string;
  mode: string;

  // How to judge the outcome — layered, not exclusive
  judge: EvalJudge;
}

// Judges are composable. A task can require tests to pass AND specific files
// to exist AND an LLM judge to approve the diff.
type EvalJudge =
  | { type: "test-command"; command: string }              // exit code 0 = pass
  | { type: "file-exists"; paths: string[] }               // all paths must exist after run
  | { type: "file-contains"; path: string; pattern: string } // regex match in file
  | { type: "diff-review"; criteria: string }              // LLM reviews the diff against criteria
  | { type: "composite"; judges: EvalJudge[]; require: "all" | "any" };
```

**Where tasks come from:**

The GPT-5.4 research document recommends mining tasks from past PRs/incidents. Concretely:

1. Pick 20-50 closed PRs from a real repo
2. For each PR: record the base commit (pre-PR), the prompt (issue description or PR description), and the post-merge test suite as the judge
3. The eval task is: "starting from commit X, given prompt Y, can the harness produce changes that make the tests pass?"

This is essentially the SWE-bench methodology applied to your own repos. It's more valuable than public benchmarks because it measures what matters: performance on *your* code, *your* conventions, *your* test expectations.

### Experiment definition

An experiment holds one or more variables constant while varying others. The `RunConfig` already supports this — an experiment is just a template with holes.

```typescript
interface Experiment {
  id: string;
  description: string;
  suite: string;                         // eval suite to run against

  // The baseline config — all fields except the variable(s) under test
  baseConfig: Partial<RunConfig>;

  // Variants — each overrides specific fields of baseConfig
  variants: {
    name: string;
    overrides: Partial<RunConfig>;
  }[];

  // How many times to run each task × variant combination.
  // N=1 is a smoke test. N=5+ gives statistical signal.
  // Non-determinism comes from model sampling (temperature > 0)
  // and from tool execution timing/output.
  runsPerVariant: number;
}
```

Example: "Does search-replace editing outperform whole-file on execution tasks?"

```typescript
const editStrategyExperiment: Experiment = {
  id: "edit-strategy-comparison-2026-03",
  description: "Compare whole-file vs search-replace editing on execution tasks",
  suite: "core-repo-50",
  baseConfig: {
    mode: "execution",
    provider: { type: "anthropic" },
    modelRouter: { type: "static", provider: "anthropic", model: "claude-sonnet-4-6" },
    contextStrategy: { type: "sliding-window", maxTokens: 180_000 },
    verifier: { type: "test-runner", command: "npm test" },
    maxTurns: 20,
  },
  variants: [
    { name: "whole-file", overrides: { editStrategy: { type: "whole-file" } } },
    { name: "search-replace", overrides: { editStrategy: { type: "search-replace" } } },
  ],
  runsPerVariant: 5,
};
```

### Metrics

Pass/fail is necessary but not sufficient. Two configs might both pass 80% of tasks, but one might be 3x cheaper, or one might produce minimal diffs while the other rewrites entire files. The trace already captures most of what we need; the experimentation framework adds derived metrics.

| Metric | Source | What it tells you |
|---|---|---|
| **Pass rate** | `RunTrace.outcome` | Raw correctness |
| **Cost** | `RunTrace.cost` | Efficiency in dollars |
| **Turns to completion** | `RunTrace.turns` | How quickly the agent converges |
| **Token usage** | `RunTrace.tokenUsage` | Context pressure / verbosity |
| **Tool call count** | `RunTrace.toolCalls.length` | Exploration efficiency |
| **Tool failure rate** | `RunTrace.toolCalls` where `success: false` | Harness/tool reliability |
| **Wall-clock time** | `completedAt - startedAt` | End-to-end latency |
| **Diff size** | Derived from git diff post-run | Surgical precision (smaller = better for equivalent correctness) |
| **Verification retries** | `RunTrace.verificationResults.length` | How often the model needs correction |
| **Consistency** | Variance across N runs of same task | Reliability / non-determinism |

```typescript
interface ExperimentReport {
  experimentId: string;
  suite: string;
  variants: {
    name: string;
    config: Partial<RunConfig>;
    results: {
      passRate: number;              // 0.0 - 1.0
      meanCost: number;
      medianTurns: number;
      meanTokens: { input: number; output: number };
      meanToolCalls: number;
      toolFailureRate: number;
      meanWallClockMs: number;
      meanDiffLines: number;
      meanVerificationRetries: number;
      consistency: number;           // 0.0 - 1.0 (% of tasks with same outcome across all N runs)
      perTask: TaskResult[];         // detailed per-task breakdown
    };
  }[];
}
```

### Tool call recording and replay

Every harness run should record the full sequence of tool calls and their results. This serves three purposes:

1. **Debugging** — inspect exactly what happened on turn 7 without re-running
2. **Replay** — re-evaluate a recorded run against a new verifier or judge without spending model tokens
3. **Isolation** — compare two runs and see where they diverged (same task, different config — did the model make different tool calls, or the same calls with different outcomes?)

```typescript
interface TurnRecord {
  turn: number;
  modelInput: {
    messages: Message[];           // what the model saw
    tools: ToolDefinition[];
    model: string;
  };
  modelOutput: ContentBlock[];     // what the model produced
  toolCalls: {
    id: string;
    name: string;
    input: unknown;
    output: string;
    durationMs: number;
    success: boolean;
  }[];
}

// A full recording of a run — stored alongside the RunTrace
interface RunRecording {
  runId: string;
  config: RunConfig;
  turns: TurnRecord[];
  finalOutcome: RunTrace;
}
```

Recordings are stored as gzipped JSONL alongside traces. They're large (megabytes per run) but compressible, and only needed for debugging and analysis — not for normal operation.

A `ReplayProvider` adapter can feed recorded model outputs back through the loop, bypassing the real model API. Combined with a `ReplayExecutor` that feeds recorded tool outputs, this gives you fully deterministic replay of any recorded run — useful for testing changes to the loop itself without re-running against the API.

### Workspace snapshotting

For eval runs to be reproducible, every run must start from the same workspace state. The simplest approach:

1. The eval task specifies a `repo` + `ref` (git remote + commit SHA)
2. Before each run, the harness (or the eval runner) does `git clone --depth 1` + `git checkout <ref>`
3. The workspace is a fresh directory per run (or a git worktree)

This is cheap (shallow clones are fast) and deterministic (a commit SHA is immutable). No need for filesystem snapshotting or container image builds per task.

For tasks that need installed dependencies (node_modules, vendor/), the eval task can specify a `setup` command that runs before the harness starts. This is cached per `repo + ref + setup command` hash.

### CI/CD regression gating

Eval runs cost real money (API calls, compute). The solution is tiered evaluation — cheap checks on every PR, comprehensive checks at release gates only.

**Tier 1: Unit tests (every PR, seconds, $0)**

Test harness logic with `ReplayProvider` + `ReplayExecutor`. No API calls. These test:
- Core loop state machine behavior (does it stop correctly? does it handle tool errors? does rollback work?)
- Context compaction (given these messages and this budget, what comes out?)
- Edit strategy application (given this search-replace input and this file content, what's the result?)
- Permission policy logic
- RunConfig validation and factory construction

These are normal unit tests. They run in CI like any other test suite. The replay infrastructure we built for experimentation does double duty here — recorded runs become test fixtures.

**Tier 2: Smoke eval (every PR, ~5 minutes, ~$5-10)**

Run 5-10 tasks from the eval suite, N=1, against the real API. Catches obvious regressions: "the loop crashes on tool errors," "the edit strategy can't apply a simple change," "context compaction loses the system prompt." Fast enough to be a merge gate.

The smoke suite should be the highest-signal subset of the full eval suite — tasks that historically discriminate between working and broken harness versions.

**Tier 3: Full eval (pre-release, ~30-60 minutes, ~$50-200)**

Complete eval suite, N=3-5. Run before cutting a release tag. The results are compared to the previous release's eval results:

```
eval run --suite core-repo-50 --config release-candidate.yaml --runs 3
eval compare --current run-abc123 --baseline release-v0.4.0
```

If any metric regresses beyond a threshold (e.g. pass rate drops > 5%, cost increases > 20%), the release is blocked and the report is attached to the release PR for human review.

**Tier 4: Post-deploy canary (post-release, continuous)**

After deploying a new harness version, the control plane routes a fraction of production tasks to the new version and compares outcomes to the previous version's production metrics. This isn't the eval framework's job directly — it's the control plane's — but the eval framework provides the comparison logic and metric definitions.

**CI pipeline shape:**

```
PR opened / updated
  → tier 1: unit tests (replay-based, no API)
  → tier 2: smoke eval (5-10 tasks, real API)
  → both pass → mergeable

Release branch cut
  → tier 3: full eval (complete suite, N=3-5)
  → compare to previous release baseline
  → no regression → tag release
  → regression → block + report

Post-deploy (control plane responsibility)
  → tier 4: canary traffic split
  → compare production metrics to pre-deploy baseline
  → degradation → alert (rollback decision is human/control-plane)
```

### Production metrics feedback loop

Every production run emits a `RunTrace` to the control plane via gRPC. The control plane persists these to a data store — the "lakehouse." This creates a feedback loop from production back into development.

**What the lakehouse stores:**

The same `RunTrace` type used in eval, plus production-specific metadata:

```typescript
interface ProductionTrace extends RunTrace {
  // Production context — not present in eval traces
  harness_version: string;         // git SHA or semver of the deployed harness
  task_source: string;             // "api" | "toil-scheduler" | "manual"
  target_repo: string;             // what repo was the task run against
  user_id?: string;                // who triggered it (if applicable)
}
```

**What development reads from the lakehouse:**

```typescript
interface TraceLakehouse {
  // Baselines: "what does production look like right now?"
  baseline(filter: TraceFilter): Promise<BaselineMetrics>;

  // Drift detection: "has performance changed over this time window?"
  drift(filter: TraceFilter, window: {
    current: DateRange;
    previous: DateRange;
  }): Promise<DriftReport>;

  // Failure mining: "what failed recently? Can we turn these into eval tasks?"
  failedRuns(filter: TraceFilter, limit: number): Promise<ProductionTrace[]>;

  // Validation: "does production match our lab experiment predictions?"
  compareToExperiment(
    experimentId: string,
    productionFilter: TraceFilter,
  ): Promise<LabVsProductionReport>;
}
```

The concrete implementation could be BigQuery, ClickHouse, Postgres with JSONB, or even S3 + Athena. The interface is what matters — the eval framework consumes it without caring about the backing store.

**Four things this enables:**

1. **Production baselines for experiments.** Instead of comparing variant A to variant B in a vacuum, compare both to "what production is actually doing." An experiment might show search-replace is 10% better than whole-file in the lab, but if production is already using search-replace and achieving 85% pass rate, the experiment needs to beat 85%, not the lab baseline.

2. **Drift detection.** "Pass rate on execution tasks dropped from 82% to 71% this week." Could be a model provider change (Anthropic updated Sonnet), a repo change (the target codebase got harder), or a harness regression. The `harness_version` field lets you isolate the cause: if the harness version didn't change, the regression is external.

3. **Failure mining.** Production failures are the best source of new eval tasks. When a run fails, you have: the starting repo state, the prompt, the full recording, and the failure mode. Turning this into an eval task is mostly automated:
   ```
   eval mine-failures --lakehouse prod --since 7d --output suites/mined-failures.yaml
   ```
   This keeps the eval suite evolving with real-world failure modes instead of stagnating on synthetic tasks.

4. **Lab-to-production validation.** "Our experiment predicted a 10% pass rate improvement. After deploying, production shows 8%. That's within noise — the experiment was predictive." Or: "Lab said +10%, production says -2%. The eval suite doesn't capture something important about production workloads — investigate and add missing task types."

### Project structure: monorepo with packages

**Yes, split out the eval framework — but keep it in the same monorepo.** The harness and the eval framework have different deployment targets, different dependencies, and different release cadences, but they share types and evolve together. A monorepo with workspace packages gets this right.

Three packages:

```
stirrup/
  packages/
    types/                           # @stirrup/types
      src/
        run-config.ts                # RunConfig, ModePreset, component config types
        run-trace.ts                 # RunTrace, TurnRecord, RunRecording
        eval.ts                      # EvalSuite, EvalTask, EvalJudge, Experiment
        events.ts                    # HarnessEvent, ControlEvent
        metrics.ts                   # ExperimentReport, BaselineMetrics, DriftReport
      package.json

    harness/                         # @stirrup/harness
      src/
        core/                        # loop, factory
        providers/                   # anthropic, bedrock, openai-compatible
        router/                      # static, per-mode
        prompts/                     # default, composed
        context/                     # sliding-window, summarise
        tools/                       # registry, builtins, mcp client
        executor/                    # local, docker
        edit/                        # whole-file, search-replace, udiff
        verifier/                    # none, test-runner, composite
        permissions/                 # allow-all, deny-side-effects, ask-upstream
        git/                         # none, (future implementations)
        transport/                   # grpc, stdio
        trace/                       # jsonl, otel
        proto/                       # harness.proto
        cli/                         # CLI entrypoint
        job.ts                       # K8s job entrypoint
      Dockerfile
      package.json                   # depends on @stirrup/types

    eval/                            # @stirrup/eval
      src/
        runner.ts                    # orchestrate runs (local or via control plane)
        report.ts                    # compute metrics, generate comparison tables
        replay.ts                    # replay recorded runs
        ci.ts                        # CI-specific: tier selection, regression detection, exit codes
        lakehouse/
          interface.ts               # TraceLakehouse interface
          bigquery.ts                # (or postgres.ts, clickhouse.ts — concrete adapter)
        mine.ts                      # mine eval tasks from production failures
      suites/                        # eval suite definitions (YAML)
      experiments/                   # experiment definitions (YAML)
      package.json                   # depends on @stirrup/types, @stirrup/harness

  package.json                       # workspace root (pnpm workspaces)
  turbo.json                         # build orchestration
```

**Why three packages, not two:**

- `@stirrup/types` is the contract between everything. It has zero dependencies and changes rarely. The control plane (a separate service entirely) can depend on it too, for protobuf type generation and trace schemas.
- `@stirrup/harness` is the deployable artifact — a Docker image. It depends on types and nothing else from the monorepo.
- `@stirrup/eval` is a development/CI tool. It depends on types (for RunConfig/RunTrace definitions) and optionally on harness (to invoke runs locally). It has its own dependencies (lakehouse clients, data analysis, reporting) that the harness shouldn't carry.

**Why not a fully separate repo:**

- Shared types would need cross-repo versioning and publishing. This is manageable but adds friction that's not justified at this stage.
- Harness changes often require eval changes (new component type → new RunConfig field → new eval suite config). Co-locating them means one PR, one review, one merge.
- If the eval framework grows to serve multiple harnesses (via A2A), it can be extracted to its own repo later. Monorepo-to-multi-repo is easier than the reverse.

### Development workflow with all pieces connected

```
1. Developer changes harness code (e.g. new edit strategy)
   │
   ▼
2. PR opened
   → Unit tests with ReplayProvider (free, seconds)
   → Smoke eval: 10 tasks, N=1 ($5)
   → CI posts results as PR comment: "pass rate 9/10, cost $0.42 avg, 4.2 turns avg"
   │
   ▼
3. Developer runs targeted experiment locally
   → "eval run --experiment edit-strategy-comparison --suite core-repo-50 --runs 3"
   → Report: search-replace beats whole-file by 12% pass rate, 40% less cost
   → Developer also pulls production baseline: "eval baseline --lakehouse prod --mode execution"
   → Confirms the improvement exceeds production baseline, not just lab baseline
   │
   ▼
4. PR merged → release branch cut
   → Full eval: 50 tasks, N=3 ($150)
   → Compared to previous release: no regressions → release tagged
   │
   ▼
5. Deployed
   → Control plane routes 10% canary traffic to new version
   → After 24h, compare new version's production traces to old version's
   → No degradation → ramp to 100%
   │
   ▼
6. Ongoing
   → Weekly: "eval mine-failures --lakehouse prod --since 7d" → new eval tasks
   → Monthly: "eval drift --lakehouse prod --window 30d" → detect slow regressions
   → Quarterly: review eval suite health — are tasks still discriminating?
```

## Observability & monitoring

The `TraceEmitter` interface (component 12) and `RunTrace` type provide per-run telemetry, but a production system needs deeper observability across three dimensions: **structured logging**, **distributed tracing**, and **metrics**. This section describes the observability architecture that wraps the harness and feeds the control plane.

### Structured logging

Every harness process emits structured JSON logs to stdout (12-factor style — the deployment environment routes them to a log aggregator). Logs are not free-form strings; every log line is a typed event with a consistent schema.

```typescript
interface LogEvent {
  timestamp: string;               // ISO 8601
  level: "debug" | "info" | "warn" | "error";
  runId: string;                   // correlates all log lines for a single run
  component: string;               // "loop" | "provider" | "executor" | "tool" | "verifier" | ...
  event: string;                   // machine-readable event name
  data?: Record<string, unknown>;  // event-specific payload
  durationMs?: number;             // for events that measure latency
}
```

**Key log events by component:**

| Component | Event | Level | Data |
|---|---|---|---|
| `loop` | `turn_start` | info | `{ turn, tokenUsage, messageCount }` |
| `loop` | `turn_complete` | info | `{ turn, stopReason, toolCallCount, durationMs }` |
| `loop` | `run_complete` | info | `{ outcome, turns, totalTokens, totalCost, durationMs }` |
| `loop` | `context_compacted` | info | `{ strategy, messagesBefore, messagesAfter, tokensBefore, tokensAfter }` |
| `loop` | `max_turns_reached` | warn | `{ maxTurns, lastStopReason }` |
| `loop` | `rollback` | warn | `{ turn, error, messagesRolledBack }` |
| `provider` | `request_start` | debug | `{ provider, model, inputTokens, toolCount }` |
| `provider` | `request_complete` | info | `{ provider, model, outputTokens, stopReason, durationMs, ttftMs }` |
| `provider` | `request_error` | error | `{ provider, model, error, retryable, attempt }` |
| `provider` | `rate_limited` | warn | `{ provider, retryAfterMs }` |
| `tool` | `call_start` | info | `{ tool, inputSummary }` |
| `tool` | `call_complete` | info | `{ tool, success, outputLength, durationMs }` |
| `tool` | `call_error` | error | `{ tool, error, input }` |
| `tool` | `permission_denied` | warn | `{ tool, policy, reason }` |
| `executor` | `exec_start` | debug | `{ command, timeout }` |
| `executor` | `exec_complete` | info | `{ command, exitCode, durationMs, stdoutLength, stderrLength }` |
| `executor` | `exec_timeout` | warn | `{ command, timeoutMs }` |
| `executor` | `exec_oom` | error | `{ command, memoryLimitMb }` |
| `verifier` | `verification_start` | info | `{ verifier, attempt }` |
| `verifier` | `verification_result` | info | `{ verifier, passed, attempt, durationMs }` |
| `edit` | `apply_start` | debug | `{ strategy, path }` |
| `edit` | `apply_result` | info | `{ strategy, path, applied, diffLines, fallback? }` |
| `git` | `checkpoint` | info | `{ sha, message, filesChanged }` |
| `mcp` | `server_connected` | info | `{ uri, toolCount }` |
| `mcp` | `server_disconnected` | warn | `{ uri, reason }` |

The logger is injected into every component (not imported globally), making it testable and allowing log suppression in unit tests without global state.

### Distributed tracing (OpenTelemetry)

Each harness run is a single OTel trace. Spans map to the natural hierarchy of the system:

```
run (root span)
├── turn[0]
│   ├── context_prepare          (ContextStrategy.prepare)
│   ├── model_request            (ProviderAdapter.stream)
│   │   ├── ttft                 (time to first token — measured within the stream)
│   │   └── stream_complete
│   ├── tool_dispatch[read_file] (ToolRegistry.resolve + handler)
│   ├── tool_dispatch[exec]
│   │   └── executor_exec        (Executor.exec)
│   └── permission_check         (PermissionPolicy.check)
├── turn[1]
│   ├── context_prepare
│   ├── model_request
│   └── tool_dispatch[write_file]
│       ├── edit_apply           (EditStrategy.apply)
│       └── executor_write       (Executor.writeFile)
├── verification
│   └── test_runner_exec         (Executor.exec for test command)
└── trace_emit                   (TraceEmitter.finish)
```

**Span attributes** follow OpenTelemetry semantic conventions where applicable, extended with AI-specific attributes:

```typescript
// On the root run span
"stirrup.run.id": string;
"stirrup.run.mode": string;
"stirrup.run.model": string;
"stirrup.run.provider": string;
"stirrup.run.max_turns": number;
"stirrup.run.executor_type": string;       // "local" | "container" | "api" | "microvm"

// On model_request spans
"gen_ai.system": string;                   // "anthropic" | "openai" | "bedrock"
"gen_ai.request.model": string;
"gen_ai.response.model": string;
"gen_ai.usage.input_tokens": number;
"gen_ai.usage.output_tokens": number;
"gen_ai.response.stop_reason": string;
"stirrup.model.ttft_ms": number;           // time to first token
"stirrup.model.cost_usd": number;

// On tool_dispatch spans
"stirrup.tool.name": string;
"stirrup.tool.side_effects": boolean;
"stirrup.tool.success": boolean;
"stirrup.tool.output_length": number;
```

**Export targets:**

The `TraceEmitter` interface already supports pluggable backends. For OTel specifically:

| Target | When | How |
|---|---|---|
| JSONL file | Local dev, CI | `TraceEmitter` writes spans as JSONL — no collector needed |
| OTel Collector | Production | OTLP/gRPC export to a sidecar or cluster-level collector |
| Jaeger / Tempo / Honeycomb | Production (visualisation) | Collector forwards to the backend of choice |

The harness ships the `@opentelemetry/sdk-node` + `@opentelemetry/exporter-otlp-grpc` packages. The OTel exporter is selected via `TraceEmitterConfig` in the `RunConfig`, defaulting to JSONL for local use.

### Metrics

Metrics are derived from traces and logs, not collected independently. This avoids dual-write inconsistencies and keeps the harness simple — it emits events, and the infrastructure derives counters/histograms.

**Two collection paths:**

1. **OTel Collector span metrics processor** — the collector extracts metrics from span attributes automatically. No harness code needed beyond well-attributed spans.
2. **Control plane aggregation** — the control plane receives `RunTrace` objects via gRPC and computes aggregate metrics server-side. This is the primary path for business-level metrics.

**Key metrics (all derived from RunTrace / spans):**

| Metric | Type | Labels | Source |
|---|---|---|---|
| `stirrup.runs.total` | counter | `mode`, `outcome`, `provider`, `model` | `RunTrace.outcome` |
| `stirrup.runs.duration_seconds` | histogram | `mode`, `outcome` | `completedAt - startedAt` |
| `stirrup.runs.turns` | histogram | `mode`, `outcome` | `RunTrace.turns` |
| `stirrup.runs.cost_usd` | histogram | `mode`, `provider`, `model` | `RunTrace.cost` |
| `stirrup.tokens.input` | counter | `provider`, `model` | `RunTrace.tokenUsage.input` |
| `stirrup.tokens.output` | counter | `provider`, `model` | `RunTrace.tokenUsage.output` |
| `stirrup.tools.calls_total` | counter | `tool`, `success` | `RunTrace.toolCalls` |
| `stirrup.tools.duration_seconds` | histogram | `tool` | `RunTrace.toolCalls[].durationMs` |
| `stirrup.model.ttft_seconds` | histogram | `provider`, `model` | span attribute |
| `stirrup.model.errors_total` | counter | `provider`, `model`, `error_type` | provider error spans |
| `stirrup.model.retries_total` | counter | `provider`, `model` | provider retry spans |
| `stirrup.verification.attempts` | histogram | `verifier`, `passed` | `RunTrace.verificationResults` |
| `stirrup.executor.exec_duration_seconds` | histogram | `executor_type` | executor spans |
| `stirrup.executor.timeouts_total` | counter | `executor_type` | executor timeout events |
| `stirrup.context.compactions_total` | counter | `strategy` | context compaction events |
| `stirrup.context.tokens_reclaimed` | histogram | `strategy` | `tokensBefore - tokensAfter` |

### Alerting

Alerting rules are defined in the control plane / monitoring infrastructure, not in the harness. The harness's job is to emit high-quality signals; the control plane's job is to act on them. Recommended alert conditions:

| Alert | Condition | Severity | Rationale |
|---|---|---|---|
| Run failure rate spike | `stirrup.runs.total{outcome=error}` / `stirrup.runs.total` > 0.2 over 15m | critical | Indicates a systemic issue (provider outage, broken tool, bad config) |
| Provider error rate | `stirrup.model.errors_total` rate > 5/min for a single provider | warning | Provider degradation — may need failover |
| Cost anomaly | `stirrup.runs.cost_usd` p95 > 2x baseline for same mode | warning | Model regression, infinite loop, or context explosion |
| Turn count anomaly | `stirrup.runs.turns` p95 > 2x baseline for same mode | warning | Agent stuck in a loop or unable to converge |
| TTFT latency | `stirrup.model.ttft_seconds` p99 > 30s | warning | Provider latency degradation |
| Executor timeout rate | `stirrup.executor.timeouts_total` rate > 3/min | warning | Commands hanging — sandbox issue or resource exhaustion |
| Verification failure rate | `stirrup.verification.attempts{passed=false}` rate increasing | info | Model quality regression or test instability |

### Health checks

The harness is a short-lived job, not a long-running server, so traditional HTTP health endpoints don't apply. Instead, health is signalled through the gRPC transport:

1. **Liveness** — the gRPC stream itself. If the stream drops, the control plane knows the harness is dead (K8s Job failure detection handles the rest).
2. **Heartbeat events** — the harness emits periodic `heartbeat` events on the gRPC stream during long-running tool executions (e.g. a test suite that takes 2 minutes). This prevents the control plane from timing out a healthy-but-busy run.
3. **Startup readiness** — the harness emits a `ready` event after constructing all components from the RunConfig but before starting the first turn. If component construction fails (bad config, unreachable MCP server, invalid credentials), it emits an `error` event and exits with a non-zero code.

```typescript
type HarnessLifecycleEvent =
  | { type: "ready"; runId: string; config: RunConfig }
  | { type: "heartbeat"; runId: string; turn: number; uptimeMs: number }
  | { type: "shutdown"; runId: string; reason: "complete" | "error" | "cancelled" };
```

### Debugging failed runs

When a run fails in production, the operator needs to answer: *what happened, at which turn, and why?* The observability stack provides three levels of investigation:

1. **Metrics** — "runs are failing more often" → identify the mode, provider, and time window.
2. **Traces** — find the specific run in Jaeger/Tempo, see the span tree, identify the turn where things went wrong (long span, error span, missing span).
3. **Run recording** — if the run was recorded (configurable via `RunConfig`), load the full `RunRecording` to see every model input/output and tool call/result. This is the equivalent of a core dump for agentic runs.

The `eval replay` command can replay a recorded run locally for reproduction:

```
eval replay --recording /traces/run-abc123.jsonl.gz --stop-at-turn 7
```

This reconstructs the exact state at turn 7 — messages, tool results, context — so the developer can inspect what the model saw and why it made the decision it did.

### Cost tracking

Cost is a first-class observable, not an afterthought. Every `RunTrace` includes a `cost` field computed from token usage and the provider's pricing. The harness tracks cost per-turn and enforces budgets configured in the `RunConfig`:

```typescript
interface CostTracker {
  // Called after each model response
  recordTurn(turn: number, usage: { inputTokens: number; outputTokens: number }, pricing: ModelPricing): void;

  // Returns cumulative cost so far
  currentCost(): number;

  // Checks against RunConfig budgets — called by the loop before each turn
  checkBudget(config: { maxCostBudget?: number; maxTokenBudget?: number }): {
    withinBudget: boolean;
    currentCost: number;
    currentTokens: { input: number; output: number };
    reason?: string;  // "cost_limit_exceeded" | "token_limit_exceeded"
  };
}

interface ModelPricing {
  inputPer1M: number;   // $ per 1M input tokens
  outputPer1M: number;  // $ per 1M output tokens
}
```

When a budget is exceeded, the loop emits a `budget_exceeded` log event and terminates the run with outcome `"budget_exceeded"`. The `RunTrace` records the final cost and the budget that was hit.

Cost data flows to the control plane via the `RunTrace`, where it feeds into:
- Per-team / per-repo cost dashboards
- Cost anomaly alerting (see alerting table above)
- Chargeback / showback reporting
- Experiment cost comparison (in the eval framework)

## What to carry forward from stirrup

The current Ruby codebase validates several patterns worth preserving:

1. **The agentic loop structure** (`Conversation#run_loop` in `stirrup.rb:95-109`) — stream a turn, check stop reason, dispatch tools, repeat. This maps directly to the new core loop.
2. **Tool abstraction** (`Tool` class in `stirrup.rb:9-31`) — definition + handler in one object. Same pattern, now behind the `Tool` / `ToolRegistry` interfaces.
3. **Rollback on error** (`say` in `stirrup.rb:84-91`) — checkpoint message history and restore on failure. Keep this in the core loop.
4. **Workspace path sandboxing** (`workspace_path` in `server.rb:24-31`) — now lives behind the `Executor` interface (`resolvePath`).
5. **Streaming event types** — `text_delta`, `tool_call`, `tool_result`, `done`, `error` remain the vocabulary, now carried over gRPC/protobuf instead of WebSocket JSON.

What to drop:
- Manual SSE parsing (use the SDK's streaming support instead)
- Sinatra/Puma/Faye server stack (harness is now a job, not a server)
- Single-model coupling (replaced by `ProviderAdapter` + `ModelRouter`)
- Hardcoded system prompt and tool set (replaced by `PromptBuilder` + `ModePreset` + `RunConfig`)

## Implementation plan

Each phase delivers a working system. The approach: start with the simplest concrete implementation of every interface, then add alternatives.

### Phase 1: Core loop + CLI with minimal implementations (week 1)

Deliver: interactive CLI that can take a prompt, run an agentic loop against Anthropic, use tools, and stream output to stdout. Security foundations are built in from day one — not retrofitted later.

| Component | Implementation | Notes |
|---|---|---|
| Provider | Anthropic | `@anthropic-ai/sdk`, credentials via `SecretStore` (env var backend) |
| Router | Static | One model: `claude-sonnet-4-6` |
| Prompt builder | Default | Hardcoded per-mode templates, `<untrusted_context>` delimiters for dynamic context |
| Context strategy | Sliding window | Drop oldest turns beyond budget |
| Tools | Built-in | filesystem, search, shell, web-fetch. JSON Schema validation on all tool inputs. |
| Executor | Local (tier 1) | Direct fs + child_process, workspace-scoped. Symlink resolution via `realpath`. File size limits (10MB). Output capping (1MB). |
| Edit strategy | Whole-file | Model writes entire file via `write_file` |
| Verifier | None | Model decides when done |
| Permissions | Deny-side-effects (read-only modes), allow-all (execution mode) | Read-only modes enforce `deny-side-effects` from the start |
| Transport | Stdio | JSON lines to stdout |
| Git | None | No git management |
| Trace | JSONL | Append to local file, `RunConfig.redact()` applied before persistence |
| Security | Built-in | `SecretStore` (env backend), `RunConfig.validate()`, `LogScrubber`, tool input validation, prototype pollution guard |

Steps:
1. Scaffold TypeScript project with the directory structure from the component map
2. Define all 12 interfaces (`.ts` files in each `interface.ts`)
3. Implement `SecretStore` interface + environment variable backend
4. Implement the simplest concrete version of each component
5. Implement `RunConfig.validate()` with security invariants (see section 7)
6. Implement `RunConfig.redact()` for trace/recording persistence
7. Implement `LogScrubber` with secret pattern redaction
8. Implement `factory.ts` — constructs components from `RunConfig`, rejects invalid configs
9. Implement core loop with tool input validation (JSON Schema + prototype key stripping)
10. CLI entrypoint: build `RunConfig` from flags/env, resolve secrets via `SecretStore`, run loop
11. Verify end-to-end: prompt -> Anthropic -> tool use -> streamed output

### Phase 2: Modes + MCP + edit strategies + API executor (week 2)

Deliver: mode presets for all 5 modes, MCP tool integration, search-replace editing, tier 0 API-backed executor.

1. Define mode presets (execution, planning, review, research, toil) — each preset selects the appropriate permission policy (`deny-side-effects` already implemented in Phase 1)
2. MCP client integration (connect to MCP servers, register tools in registry). MCP tools default to `sideEffects: true` unless explicitly marked otherwise.
3. Search-replace edit strategy (the highest-leverage upgrade from whole-file)
4. API-backed executor (tier 0): `VcsBackend` interface + GitHub implementation. `read_file` → GitHub Contents API, `search_files` → GitHub Code Search API, `list_directory` → GitHub Trees API. No clone, no filesystem, no sandbox overhead.
5. Executor `capabilities()` method + tool registry filtering (don't offer `write_file`/`run_command` when executor reports `canWrite: false`)
6. Mode selection via CLI flags

### Phase 3: gRPC transport + job entrypoint (week 3)

Deliver: harness runnable as a K8s Job that connects to a control plane.

1. Define protobuf contract (`harness.proto`) — including `RunConfig` as a proto message
2. gRPC bidi streaming client transport
3. `ask-upstream` permission policy (sends approval requests via transport)
4. Job entrypoint: deserialise `TaskAssignment` -> `RunConfig`, run, exit
5. Dockerfile

### Phase 4: Container sandbox + verification + multi-provider (week 3-4)

Deliver: tier 2 container executor, test-runner verifier, all three provider adapters, model routing. The container sandbox ships before multi-provider support because more providers means more API keys at risk.

1. Container executor (tier 2): Docker-based sandbox with network isolation (`--network none` default), resource limits (CPU, memory, PIDs), read-only root filesystem, `--cap-drop ALL`, `--security-opt no-new-privileges`. Harness process stays outside; only tool execution runs in the container. See `SECURITY_HARDENING.md` for post-V1 hardening (image supply chain, volume mount security, Docker socket alternatives, user namespace remapping).
2. `SecretStore` cloud backends: AWS SSM Parameter Store and/or GCP Secret Manager adapters, so production deployments don't rely on environment variables for API keys.
3. Test-runner verifier (run command, parse output, feed failures back)
4. OpenAI-compatible provider adapter — covers OpenAI GPT (native), LiteLLM, Azure OpenAI, vLLM, Ollama. Uses `openai` SDK with configurable `baseUrl`. Credentials via `SecretStore`.
5. AWS Bedrock Converse provider adapter — covers Claude, Llama, Mistral via Bedrock. Uses `@aws-sdk/client-bedrock-runtime`. Translates between internal message/tool types and Bedrock's Converse wire format. Auth via standard AWS credential chain (instance role, IRSA, env vars, SSO profile).
6. Per-mode model router (e.g. Haiku for toil, Sonnet for execution, Opus for planning). Router selects provider + model, so it can route different modes to different backends (e.g. planning via Bedrock, toil via direct Anthropic).
7. LLM-summarise context strategy (as an alternative to sliding window)

### Phase 5: Eval framework + CI integration (week 4-5)

Deliver: eval runner, first eval suite, CI pipeline with tier 1 + tier 2 gates.

1. Extract `@stirrup/types` package (RunConfig, RunTrace, EvalSuite, etc.)
2. Scaffold `@stirrup/eval` package
3. Implement `ReplayProvider` + `ReplayExecutor` (enables tier 1 unit tests without API calls)
4. Implement eval runner (orchestrate runs locally, collect traces)
5. Implement comparison report (metrics table, regression detection)
6. Mine 10-20 eval tasks from a real repo's closed PRs → first eval suite
7. CI pipeline: tier 1 (unit tests with replay) + tier 2 (smoke eval) as PR gates

### Phase 6: Production feedback loop (week 5-6)

Deliver: lakehouse integration, failure mining, drift detection.

1. Define `TraceLakehouse` interface
2. Implement first concrete adapter (Postgres JSONB or BigQuery — depends on control plane choices)
3. `eval baseline` command: pull production metrics as experiment baselines
4. `eval mine-failures` command: turn production failures into eval tasks
5. `eval drift` command: detect metric changes over time windows
6. Tier 3 (full eval) as release gate in CI

### Phase 7: Remaining features + security hardening (ongoing)

1. Unified diff edit strategy + multi-strategy fallback
2. LLM-as-judge verifier
3. OpenTelemetry trace emitter
4. Token budgets and cost caps in the core loop
5. Sub-agent spawning (fresh loop instance with subset of context)
6. `eval compare-to-production` command (lab-vs-production validation)
7. Security hardening items from `SECURITY_HARDENING.md` — prioritised by deployment context (see that document for the full roadmap)

Note: scheduling/toil triggering is the control plane's responsibility. The harness just needs to support the toil mode config — the control plane decides *when* to dispatch toil jobs.

## Resolved design decisions

1. **State persistence** — **No persistence in the harness.** Conversation history is ephemeral. Persistence is the responsibility of the control plane that dispatches jobs. The harness is a worker, not an orchestrator.

2. **Git integration depth** — **Deferred.** Requires dedicated research session to evaluate model-driven vs deterministic vs hybrid git management. Will be resolved before implementation begins.

3. **Communication model** — **The harness is a job, not a server.** It is started by a control plane (likely as a Kubernetes Job), connects *outbound* to the control plane via **gRPC bidirectional streaming**, streams events upstream, and exits when done. No inbound HTTP/WebSocket server. Transport-level auth is the control plane's responsibility (mTLS, service mesh, or VPC isolation). The harness should verify the control plane's TLS certificate against a pinned CA or fingerprint, not just trust the system CA store. See `SECURITY_HARDENING.md` for post-V1 application-layer mutual authentication (session tokens, sequence numbers, replay protection).

   This is a fundamental change from stirrup's server architecture. The protobuf contract replaces the WebSocket JSON protocol:

   ```protobuf
   service HarnessControl {
     // The harness job calls this on startup; control plane sends the task
     // and receives streamed events until completion.
     rpc Run(stream HarnessEvent) returns (stream ControlEvent);
   }

   message ControlEvent {
     oneof event {
       TaskAssignment task = 1;       // initial task (mode, prompt, config)
       UserResponse user_response = 2; // if interactive: user replies
       CancelSignal cancel = 3;
     }
   }

   message HarnessEvent {
     oneof event {
       TextDelta text_delta = 1;
       ToolCall tool_call = 2;
       ToolResult tool_result = 3;
       RunComplete done = 4;
       RunError error = 5;
       RunTrace trace = 6;            // structured telemetry on completion
     }
   }
   ```

4. **Interoperability and component swappability** — The system must support swapping out any component (harness, model provider, tool backend, even the control plane itself) without rewriting the rest of the stack.

   **Protocol layering:**

   - **Internal transport: gRPC bidi streaming.** The primary protocol between our own harness and our own control plane. Strongly typed via protobuf, efficient, native to K8s. This is the fast path.
   - **External interop: A2A (Agent-to-Agent Protocol) compatibility.** The control plane exposes (or adapts to) A2A's HTTP/JSON-RPC/SSE interface so it can dispatch tasks to *any* A2A-compliant agent, not just our harness. This is the swappability path.

   **Why A2A over ACP:**

   - A2A models the right relationship: "send a task to an opaque agent, receive streaming status updates and artifacts." This maps directly to our control-plane-to-worker model.
   - ACP (Agent Client Protocol) is designed for editor-to-agent communication — interactive UX, tool approval flows, stdin/stdout transport. Wrong abstraction for cloud-native job dispatch.
   - A2A has Google backing and growing enterprise adoption, making it the likelier standard for coding agent interoperability.

   **How swappability works in practice:**

   ```
   Control Plane
     │
     ├── gRPC adapter ──→ Our harness (K8s Job, TypeScript)
     │                     Fast path. Protobuf contract.
     │
     ├── A2A adapter  ──→ OpenHands / SWE-agent / any A2A agent
     │                     HTTP/JSON-RPC/SSE. Standard contract.
     │
     └── (future)     ──→ Other protocols as they emerge
   ```

   The control plane owns a `WorkerDispatcher` interface. Each adapter (gRPC, A2A, future) implements it. The control plane doesn't know or care what's behind the adapter — only that it can send a task and receive streaming events.

   This also works in reverse: if we later want to swap out the *control plane*, the harness only depends on the gRPC contract (or could be adapted to connect to any A2A-compliant orchestrator).

   **Scope:** The A2A adapter is a control plane concern, not a harness concern. The harness speaks gRPC. The control plane translates. This keeps the harness simple and fast, and puts the interop complexity where it belongs — in the orchestration layer.

5. **MCP support** — **Yes, from the start.** The tool registry accepts both built-in tools and MCP server connections. This makes the harness immediately extensible for toil mode (GitHub MCP for PR checks, Slack MCP for briefing delivery, etc.) without writing bespoke tool handlers.

6. **Web fetching** — **Simple HTTP fetch tool initially.** Fetches a URL, converts HTML to markdown for readability. A Playwright-based headless browser tool will be added later for JavaScript-heavy/dynamic content.
