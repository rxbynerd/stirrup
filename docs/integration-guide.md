# Integration guide

This guide is for engineers integrating stirrup as the agentic
workload runner behind a control plane, orchestrator, or automation
pipeline. It covers the integration surfaces, the gRPC wire contract,
and cookbook-style recipes for the major features. The single
touchstone for the wire contract is
[`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto);
every message and event referenced here is defined there.

Deep dives live in the per-topic docs — this guide links to them
rather than repeating them. For the component model behind the
`RunConfig`, see [`architecture.md`](architecture.md); for the full
field reference, [`configuration.md`](configuration.md).

## Integration surfaces at a glance

| Surface | Shape | Reach for it when |
|---|---|---|
| **gRPC control plane** (`stirrup job`) | The harness dials out and opens one `RunTask` bidi stream; the control plane sends a `RunConfig` and consumes events. | Multi-run orchestration, human-in-the-loop approvals, follow-up turns, sandbox identity tokens, provider batch bundling. The primary integration surface. |
| **CLI composition** (`stirrup harness`, `stirrup run-config`) | `RunConfig` via flags, file, or stdin; results via exit code, `STIRRUP_RESULT` stdout line, traces, and sinks. | CI pipelines, cron jobs, shell-driven automation, and any feature not yet carried on the proto (see [Features not on the wire](#features-not-on-the-wire)). |
| **Serverless job** (Cloud Run) | The unmodified container runs `stirrup harness --config` with a secret-mounted `RunConfig`; results leave via stdout logging, GCS traces, and workspace export. | Elastic burst capacity with no cluster to run. Walkthrough: [`cloud-run-jobs.md`](cloud-run-jobs.md). |
| **Go embedding** (`harness/harnessapi`) | `BuildLoopWithTransport(ctx, config, transport)` constructs an in-process loop; `Loop.Run` executes it. | Single-binary tools bundling their own control-plane logic. Everything under `harness/internal/*` is not public API. |

The four surfaces use the same `RunConfig` concepts, but they are not
perfectly interchangeable. The CLI applies convenience defaults that
the gRPC path does not, and a few JSON fields have no proto mirror (see
[Features not on the wire](#features-not-on-the-wire)). For equivalent
fields, protobuf JSON uses lower-camel-case names such as `runId` and
`maxTurns`, matching the file-config form.

## The wire contract

### One RPC

```proto
service HarnessService {
  rpc RunTask(stream HarnessEvent) returns (stream ControlEvent);
}
```

The **control plane implements the server**; the harness is the
client and connects outbound. There is no inbound port on the harness
and no shared filesystem. One stream carries the whole run: task
assignment, streaming output, permission round-trips, and the terminal
status.

### Getting the schema

- Source of truth: [`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto).
  Generate stubs with `buf generate` or protoc.
- Every CI and release run publishes a compiled
  `google.protobuf.FileDescriptorSet` as the `proto-descriptor-set`
  workflow artifact (`stirrup-<label>.fds.binpb` + `.sha256`) for
  clients that need the schema without a checkout — `grpcurl`, Envoy
  gRPC-JSON transcoding, reflection-less generators.

### Stream lifecycle

1. The harness (launched as `stirrup job` with `CONTROL_PLANE_ADDR`
   set) dials the control plane and opens the `RunTask` stream.
2. The harness sends `HarnessEvent{type:"ready", harness_version}`.
   When `CONTROL_PLANE_SESSION_ID` is set in the Pod environment,
   the `id` field echoes it so the stream can be correlated to the
   session that launched the Pod.
3. The control plane sends
   `ControlEvent{type:"task_assignment", task: RunConfig}` — this
   must be the first `ControlEvent`. The harness waits at most
   **5 minutes** for it, then exits. A `cancel` sent before
   assignment makes the harness exit cleanly without running.
4. If the `RunConfig` opts in, loop construction sends one
   `sandbox_token_request` and blocks (fail-closed, 60 s) for the
   `sandbox_token_response` before creating the sandbox.
5. During execution the harness streams `text_delta`, `tool_call`,
   `tool_result`, occasional `warning` events, and a `heartbeat` every
   **30 seconds**. Depending on configuration it may also emit
   `permission_request`, `tool_result_request`, `batch_submission`,
   `batch_waiting`, and `batch_cancel_request`.
6. A built loop ends by emitting `HarnessEvent{type:"done",
   stop_reason}`. Some early run failures emit a human-readable `error`
   event first. The current `stirrup job` path does **not** attach the
   proto's optional `trace` field to `done`.
7. If a follow-up grace window is configured, the stream stays open
   for `user_response` events that trigger fresh runs in the same
   process and workspace; otherwise the stream closes and the process
   exits.

### Events from the harness

| `type` | Fields | Control-plane obligation |
|---|---|---|
| `ready` | `harness_version`, `id` (session echo) | Optionally verify version compatibility before assigning work. |
| `text_delta` | `text` | Render / accumulate. Fragments are incremental model output. |
| `tool_call` | `id`, `name`, `input` (JSON bytes) | Informational. |
| `tool_result` | `tool_use_id`, `content` | Informational. |
| `permission_request` | `request_id`, `tool_name`, `input` | **Must respond** with `permission_response` echoing `request_id` before the policy timeout (default 60 s) or the call is auto-denied. |
| `tool_result_request` | `request_id`, `tool_use_id`, `tool_name`, `input` | **Must respond** with `tool_result_response` echoing `request_id`; the loop blocks on it under a per-call timeout. |
| `heartbeat` | — | Liveness signal every 30 s. Treat sustained absence as a hang and reap the Pod. |
| `warning` | `message` | Log; non-fatal. |
| `error` | `message` | Optional diagnostic for some early run failures; when emitted by a built loop, a `done` follows. |
| `done` | `stop_reason` | Terminal status for that run. The proto has a `trace` field, but `stirrup job` does not currently populate it. |
| `batch_submission` | `request_id`, `input` (BatchSubmission JSON) | Batch mode only — see [Batch mode](#batch-mode-amortised-token-pricing). |
| `batch_waiting` | `request_id` | Batch keep-alive (roughly every 5 minutes) distinguishing "provider batch still pending" from a stall. |
| `batch_cancel_request` | `request_id` | Best-effort: cancel the provider-side batch. The harness does not wait for a reply. |
| `sandbox_token_request` | `request_id`, `audience` | See [Sandbox identity tokens](#sandbox-identity-tokens-authenticated-git-egress). |

### Events to the harness

| `type` | Fields | Semantics |
|---|---|---|
| `task_assignment` | `task` (RunConfig) | First event on the stream. Duplicate assignments are ignored. |
| `user_response` | `user_response` | During the follow-up grace window, starts a fresh run with this text as its prompt. Events sent during an active run are currently ignored. |
| `permission_response` | `request_id`, `allowed`, `reason` | Decision for a `permission_request`. `reason` on a denial is passed to the model as context. |
| `tool_result_response` | `request_id`, `content`, `is_error` | Result payload for a `tool_result_request`. `is_error: true` surfaces to the model as a tool failure. |
| `batch_result` | `request_id`, `content` (BatchResult JSON) | Completes a `batch_submission`. Encode failures in `content.err`; the current batch client ignores `is_error`. |
| `sandbox_token_response` | `request_id`, `token`, `expires_at`, `is_error`, `reason` | The signed sandbox identity JWT, or an explicit refusal. |
| `cancel` | — | Abort the run within one turn boundary. Git finalisation still runs; the final `done` carries `stop_reason:"cancelled"`. |

Correlation rule: every `*_request` event carries a `request_id`
that the response **must echo verbatim**. Requests may interleave;
responses are matched by ID, not order.

### Terminal semantics

For the current `stirrup job` implementation, read
**`done.stop_reason`** as the run outcome. Values include `success`,
`error`, `max_turns`, `verification_failed`, `verification_error`,
`budget_exceeded`, `stalled`, `tool_failures`, `cancelled`, `timeout`,
`max_tokens`, and feature-specific outcomes such as `setup_failed`,
`hook_failed`, `guardrail_blocked`, and `rule_of_two_violation`.
Consumers should preserve unknown values so outcomes can be added
without breaking the protocol.

The proto also defines `done.trace`, but the job path currently emits
`done` before trace finalisation and leaves that field unset (tracked in
[issue #453](https://github.com/rxbynerd/stirrup/issues/453)). Obtain
turn counts, token usage, duration, verifier details, and final
assistant text from a configured `resultSink`, process stdout, or a
trace emitter instead. `cost_usd` is not calculated by the harness.

A `done` is emitted only after the loop has been constructed, and its
send is necessarily best-effort if the transport is failing. Config
validation, secret resolution, policy loading, sandbox-token exchange,
or other component-construction failures close the stream and exit
non-zero without an `error`/`done` pair. Validate configs before
sending (for example with `stirrup run-config --validate` or
`types.ValidateRunConfig`) and treat stream closure before `done` as a
setup or transport failure. With follow-ups enabled, each run has its
own `done`; it is terminal for the run, not necessarily for the
stream.

### Transport security posture (v0.1)

The gRPC transport is **plaintext and unauthenticated** — the
harness dials with insecure credentials and `TransportConfig`
exposes only `{type, address}`. Acceptable deployment shapes are
same-host loopback, a private network, or mesh-provided mTLS
(Istio, Linkerd). API keys never transit the stream (they travel as
`secret://` references and outbound events are scrubbed for
secret-shaped strings), but prompts, tool results, and especially
the raw sandbox identity JWT do. Do not point a harness at a control
plane across an untrusted network. TLS at the binary/config surface is
planned. The public embedding API accepts a caller-provided `Transport`,
so an embedder can supply its own authenticated transport; the built-in
TLS constructor is internal. Full discussion:
[`deployment.md`](deployment.md#transport-security-posture-v01).

## A minimal control plane

The smallest correct `RunTask` implementation:

1. Accept the stream; wait for `ready`.
2. Send `task_assignment` with a valid `RunConfig`.
3. Consume events. Log `text_delta` / `tool_call` / `tool_result`;
   track `heartbeat` arrival times.
4. Answer every `permission_request` (even a blanket deny keeps the
   contract; an unanswered request stalls the tool call until the
   timeout auto-denies it).
5. On `done`, record `stop_reason` and close. (A server that supports
   follow-up runs keeps the stream open instead.)

This sketch supports one run with optional `ask-upstream` approvals; it
does not opt into sandbox identity, batching, async results, or
follow-ups. Add handlers for those events before enabling the
corresponding config.

Sketch (server side, generated stubs from `buf generate`):

```go
func (s *Server) RunTask(stream pb.HarnessService_RunTaskServer) error {
	for {
		ev, err := stream.Recv()
		if err != nil {
			return err // stream closed: harness exited
		}
		switch ev.Type {
		case "ready":
			if err := stream.Send(&pb.ControlEvent{
				Type: "task_assignment",
				Task: s.nextRunConfig(),
			}); err != nil {
				return err
			}
		case "permission_request":
			if err := stream.Send(&pb.ControlEvent{
				Type:      "permission_response",
				RequestId: ev.RequestId,
				Allowed:   &pb.OptionalBool{Value: s.decide(ev.ToolName, ev.Input)},
			}); err != nil {
				return err
			}
		case "done":
			s.recordOutcome(ev.StopReason)
			return nil
		}
	}
}
```

Launch the harness as a Kubernetes Job running `stirrup job` with
`CONTROL_PLANE_ADDR` pointing at this server. Set
`Job.spec.activeDeadlineSeconds` above `RunConfig.timeout`, allowing
additional time for the pre-assignment wait (up to five minutes),
component construction, post-run hooks, and result/export flushing.
The process writes `/tmp/healthy`, but the published distroless image
contains no `test` or shell binary; inspect that file from a sidecar
sharing the volume, or use a custom image/probe helper. The 30-second
wire heartbeat is the control plane's direct run-liveness signal.

## Composing a RunConfig

The `RunConfig` in `task_assignment` is the composition root — it
selects every component of the run. Crucially, **a wire-delivered
`RunConfig` skips the CLI's convenience defaulting**
(`applyModeDefaults` runs only in the CLI path). The harness
validates it (`ValidateRunConfig`) but a control plane must be
explicit about fields the CLI would have filled:

| Field | Wire requirement |
|---|---|
| `run_id` | Set it. Used in traces, sandbox token `sub` claims, and GCS object names. No path separators, `..`, or control bytes. |
| `mode` | Required: `execution`, or a read-only mode (`planning`, `review`, `research`, `toil`). |
| `prompt` | Required. |
| `provider.type` | Required: `anthropic`, `bedrock`, `openai-compatible`, `openai-responses`, `gemini`. |
| `max_turns` | Required, 1–100. The CLI default of 20 is **not** applied on the wire. |
| `timeout` | Required, 1–3600 seconds. The CLI default of 600 is **not** applied on the wire. |
| `permission_policy.type` | **Set it explicitly.** An empty type passes validation but fails at loop construction (`unsupported permissionPolicy.type ""`). |
| `tools.built_in` | Required (non-empty) for read-only modes, and must exclude the mutating tools. Optional for `execution` (empty enables all built-ins). |

Validation fills sensible defaults for the rest: `editStrategy.type`
→ `multi`, `codeScanner` → `patterns` in execution mode / `none` in
read-only modes, provider retry policy → 3 attempts with capped
exponential backoff. `executor.type` defaults to `local`.

Built-in tool names accepted in `tools.built_in`:

```
read_file  write_file  edit_file  search_replace  apply_diff
list_directory  grep_files  find_files
git_status  git_changed_files  git_diff  git_show
run_command  read_command_output  web_fetch  spawn_agent
```

The mutating set rejected in read-only modes is `write_file`,
`run_command`, `edit_file`, `search_replace`, `apply_diff`.
`read_command_output` additionally requires
`tools.commandOutput.enabled`; that command-output config is not yet
available on the proto.

Secret **values** must never appear in a `RunConfig`; references and
credential-federation settings do. When an `apiKeyRef` is used, it must
be a `secret://` reference — `secret://NAME` (environment variable on
the harness), `secret://file:///path`, or
`secret://ssm:///param-name` (AWS SSM) — resolved harness-side at
runtime. A literal key is rejected by validation. Production trace
emitters persist `RunConfig.Redact()` rather than the live config;
debug builds can explicitly relax trace redaction. Cross-cloud
alternatives (IRSA, GKE Workload Identity,
Azure/Anthropic/OpenAI WIF) are configured via
`provider.credential`; see
[`credential-federation.md`](credential-federation.md).

## Cookbook

Each recipe uses protobuf JSON's lower-camel-case field names, which
also work in CLI config files. Fields covered by an earlier recipe are
elided with `…`; those fragments are explanatory, not complete
standalone JSON documents.

### Read-only triage run (safe default posture)

Goal: investigate, review, or plan without any write surface.

```json
{
  "runId": "triage-8f2e",
  "mode": "review",
  "prompt": "Review the diff between main and release-1.2 for regressions.",
  "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "modelRouter": { "type": "static", "model": "claude-sonnet-4-6" },
  "permissionPolicy": { "type": "deny-side-effects" },
  "tools": { "builtIn": ["read_file", "list_directory", "grep_files", "find_files", "git_status", "git_diff", "git_show"] },
  "maxTurns": 20,
  "timeout": 600
}
```

- Read-only modes structurally cannot hold `allow-all` (nor a
  Cedar fallback of `allow-all`) or mutating tools —
  `ValidateRunConfig` rejects the config, not just the call.
- `deny-side-effects` still permits `web_fetch` and `spawn_agent`
  when listed; gate them with `ask-upstream` or Cedar if that
  matters.
- The `api` executor (`executor.type: "api"` + `vcsBackend`) can read
  a GitHub or GitLab repository without a local workspace. Content
  tools operate through the remote API, but the `git_*` tools require a
  real checkout, so omit them for an API-executor run.

### Editable run in a container sandbox

Goal: let the agent write code and run commands inside a hardened
container instead of the harness host.

```json
{
  "runId": "fix-1234",
  "mode": "execution",
  "prompt": "Fix the failing test in main_test.go.",
  "provider": { "…": "…" },
  "executor": {
    "type": "container",
    "image": "ghcr.io/rxbynerd/stirrup-sandbox:latest",
    "runtime": "runsc",
    "network": { "mode": "none" },
    "resources": { "cpus": 2.0, "memoryMb": 2048, "diskMb": 8192, "pids": 256 }
  },
  "permissionPolicy": { "type": "ask-upstream" },
  "gitStrategy": { "type": "deterministic" },
  "maxTurns": 40,
  "timeout": 1800
}
```

- The container is hardened unconditionally: `CapDrop ALL`,
  `no-new-privileges`, a read-only rootfs, and the nobody uid.
  `runtime: "runsc"` (gVisor) adds a kernel boundary when that OCI
  runtime is installed on the host.
- Sandbox executors require an explicit `network` block. Use
  `network.mode: "none"` for no network. `mode: "allowlist"` plus
  `network.allowlist` starts an in-process egress proxy enforcing an
  FQDN allowlist.
- `gitStrategy: "deterministic"` creates a branch from the run ID
  and commits the result — the natural handoff artifact for a
  control plane that opens PRs.
- On Kubernetes, use `executor.type: "k8s"` (or `k8s-sandbox` for
  the Agent Sandbox CRD) with `k8sNamespace` etc.; egress is
  enforced by a per-Pod NetworkPolicy plus a shared proxy
  Deployment (`stirrup egress-proxy`), wired via
  `k8sEgressProxyUrl`. See [`executors/k8s.md`](executors/k8s.md).

### Human-in-the-loop approvals (`ask-upstream`)

Goal: a human (or policy service) approves each side-effecting tool
call.

```json
"permissionPolicy": { "type": "ask-upstream", "timeout": 120 }
```

- Approval-required tools (all mutating tools, plus `web_fetch` and
  `spawn_agent`) emit `permission_request`; everything else is
  auto-allowed.
- Respond within `timeout` seconds (0 = default 60) or the call is
  denied fail-closed. A timeout surfaces to the model as a tool
  error; an explicit deny surfaces as `Permission denied: <reason>`.
- Denials with a good `reason` steer the model; a bare deny often
  just gets retried differently.
- MCP-prefixed tools (`mcp_*`) are always treated as
  approval-required under this policy.

### Cedar policy engine (rules, not round-trips)

Goal: express the approval decision as reviewable policy instead of
answering every request interactively.

```json
"permissionPolicy": {
  "type": "policy-engine",
  "policyFile": "/etc/stirrup/policy.cedar",
  "fallback": "deny-all"
}
```

- `policyFile` is read by the **harness process** during loop
  construction. Stage it via a ConfigMap mount or bake it into the
  image. Policy text cannot be shipped inside the `RunConfig`.
- Cedar answers allow/deny/no-decision; `fallback` decides
  no-decisions. A permit-based allowlist is only meaningful with
  `fallback: "deny-all"` — the default `deny-side-effects` would
  still allow non-mutating gated tools.
- Policies are linted at load; error-severity findings (dead
  clauses, unanchored wildcard hosts) abort the run before any turn.
- Starter policies: [`examples/policies/`](../examples/policies/).
  Semantics: [`safety-rings.md`](safety-rings.md#ring-3--cedar-policy-engine-per-call-authorization).

### Injecting task context

Goal: give the agent issue bodies, retrieved documents, or customer
records while marking the content as untrusted.

```json
"dynamicContext": {
  "issue_body": { "value": "…external issue text…" },
  "customer_record": { "value": "…", "sensitive": true }
}
```

- Every entry is sanitised (tag stripping, 50 KB cap) and wrapped in
  `<untrusted_context>` blocks. The system prompt tells the model to
  treat it as data rather than instructions; the delimiter is a prompt
  defence, not a hard security boundary.
- `sensitive: true` per entry — or `sensitiveData: true` at the top
  level — declares the "sensitive data" leg of the Rule of Two.
  Declare honestly: the harness deliberately does not infer
  sensitivity from credentials it manages itself.

### Rule of Two: the posture invariant

A run should not simultaneously hold **untrusted input**, **sensitive
data**, and **external communication**. `ValidateRunConfig` rejects
the all-three combination unless the policy is `ask-upstream` or the
operator explicitly sets `ruleOfTwo.enforce: false`; two legs produce
a `rule_of_two_warning` security event.

Control-plane levers:

- Declare sensitivity via `sensitiveData` / per-entry `sensitive`
  (above).
- When untrusted input + external communication hold and no
  sensitivity was declared, a runtime classifier auto-arms and
  watches conversation content; on detection the default action
  revokes external-communication tools for the rest of the run
  (`block-external`). `ruleOfTwo.runtime.onDetect` selects
  `block-external` | `ask-upstream` (gRPC transport only) |
  `redact` | `abort` | `warn`.
- `ruleOfTwo: { "enforce": false }` bypasses the invariant and
  emits an auditable `rule_of_two_disabled` security event. There
  is deliberately no CLI flag for this.

Full design: [`safety-rings.md`](safety-rings.md#ring-4--rule-of-two-pre-flight-invariant--runtime-classifier).

### Custom system prompts

Goal: control the system prompt from the control plane.

```json
"systemPromptOverride": "You are the refund-triage agent. …"
```

- `systemPromptOverride` replaces the shipped mode prompt verbatim —
  never template-parsed, so prompts compiled by an external
  prompt-management system (e.g. Langfuse) pass through untouched.
  Structural sections (workspace path, turn budget, dynamic
  context) are still appended.
- Alternatively `promptBuilder.template` supplies a Go
  `text/template` rendered against `.Model` / `.Mode` / `.Tier` /
  `.ModelIs` for model-conditional prompts, and
  `promptBuilder.promptModel` pins which model identity the shipped
  templates render for (prompt/model comparison runs). The override
  is mutually exclusive with both.
- Preview offline with `stirrup prompt render --mode execution`.
  Reference: [`configuration.md`](configuration.md#system-prompt-templating).

### Model routing and multi-provider

Goal: cheap model for easy turns, expensive model when the run gets
hard — or different providers per mode.

```json
"providers": {
  "anthropic": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "local":     { "type": "openai-compatible", "baseUrl": "http://vllm:8000/v1", "apiKeyRef": "secret://VLLM_KEY" }
},
"modelRouter": {
  "type": "dynamic",
  "provider": "local",    "model": "qwen-3.6",
  "cheapProvider": "local",    "cheapModel": "qwen-3.6",
  "expensiveProvider": "anthropic", "expensiveModel": "claude-sonnet-4-6",
  "expensiveTurnThreshold": 5,
  "expensiveTokenThreshold": 50000,
  "cheapStopReasons": ["end_turn"]
}
```

- Router types: `static` (one model), `per-mode` (`modeModels` map),
  and `dynamic`. The dynamic router chooses the cheap model when the
  previous stop reason is in `cheapStopReasons`, the expensive model
  after a configured turn/output-token threshold, and the default
  model otherwise.
- Behaviour knobs are provider-neutral: `temperature` and
  `reasoningEffort` sit at the top level of `RunConfig`. Adapters with
  a supported native control project them; other adapters may ignore
  `reasoningEffort`.
- Provider-specific wire quirks (Azure headers/query params, Z.ai
  compat profile, Gemini safety settings) live on `ProviderConfig`.
  See [`providers.md`](providers.md).

### Budgets and limits

```json
"maxTurns": 40,
"timeout": 1800,
"maxTokenBudget": 2000000,
"contextStrategy": { "type": "summarise", "maxTokens": 150000 }
```

- The token budget is enforced between provider calls and terminates
  the run with `outcome: "budget_exceeded"`; wall-clock expiry yields
  `timeout`. Ceilings are 50 M tokens and 3600 s.
- `maxCostBudget` is accepted and capped at $100, but the harness does
  not currently calculate cost or enforce that budget. Enforce money
  limits in the control plane using provider billing/pricing data.
- `contextStrategy` bounds the conversation itself:
  `sliding-window` (default), `summarise`, or `offload-to-file`.
- Pair `timeout` with an infrastructure-level deadline
  (`activeDeadlineSeconds`) that also allows for assignment, setup,
  and teardown.

### Verification: use the terminal outcome

```json
"verifier": {
  "type": "composite",
  "verifiers": [
    { "type": "test-runner", "command": "go test ./...", "timeout": 300 },
    { "type": "llm-judge", "criteria": "The diff addresses the issue without unrelated changes." }
  ]
}
```

A model turn that ends `end_turn` but fails verification produces
`done.stop_reason: "verification_failed"`; the control plane can retry,
escalate to a stronger model, or route to a human without parsing model
output.

### Follow-up turns

Goal: run another prompt in the same process and workspace without
re-provisioning the sandbox.

- Set a positive `followUpGrace` (seconds, ≤ 3600) in the `RunConfig`,
  or set `STIRRUP_FOLLOWUP_GRACE` in the Pod environment. A positive
  config value wins; zero or unset falls back to the environment.
- After `done`, the stream stays open for the grace window. A
  `user_response` starts a fresh run with a new run ID and the response
  text as its prompt. Current follow-ups **do not preserve the previous
  run's conversation history**.
- Send `user_response` only after `done`; one sent during an active run
  is ignored. Each accepted follow-up ends with its own `done` and
  resets the grace timer.
- Follow-ups share the primary run's original context deadline; the
  `timeout` budget does not restart. The job also does not re-run its
  `resultSink` or workspace-export steps for follow-ups, so consume each
  follow-up's `done` and configure a trace emitter when those runs need
  durable detail.
- A `cancel` during the grace window closes the stream promptly without
  an extra `done`.

### Cancelling a run

During an active run, send `ControlEvent{type:"cancel"}`. The harness
cancels in-flight provider streams and tool calls via context, runs git
finalisation, and emits `done` with `stop_reason:"cancelled"`. Before
assignment, `cancel` exits cleanly without `done`; during follow-up
wait it closes without another `done`. Cancellation during synchronous
component construction is not a reliable boundary, so retain an
infrastructure deadline/SIGTERM fallback. On process shutdown, the job
uses bounded contexts to flush traces and the result sink.

### Control-plane-fulfilled tools (`tool_result_request`)

The async-tool contract lets a tool's *result* come from the control
plane instead of the harness: the loop emits `tool_result_request`
(`request_id`, `tool_use_id`, `tool_name`, `input`) and blocks under
a per-call timeout; the control plane answers with
`tool_result_response` (`content`, optional `is_error: true` to
surface a failure to the model). Concurrent async calls in one turn
fan out under the `toolDispatch.maxParallel` semaphore (default 4,
ceiling 16). No shipped built-in tool defers upstream today, and the
public embedding API does not expose custom tool registration. A
control plane therefore need not implement this response path unless
it is paired with a future or internally extended harness that emits
the request.

### Sandbox identity tokens (authenticated git egress)

Goal: give the sandbox a short-lived, per-run credential for a git
proxy such as [Haybale](https://github.com/rxbynerd/haybale) —
without the harness ever holding a signing key.

```json
"executor": {
  "type": "k8s",
  "…": "…",
  "sandboxIdentity": { "source": "control-plane", "audience": "https://haybale.internal" },
  "gitProxy": { "url": "https://haybale.internal", "hosts": ["github.com"], "rewriteSsh": true }
}
```

The control plane must:

1. Answer the single `sandbox_token_request` (sent after
   `task_assignment`, before sandbox creation) with a signed JWT in
   `sandbox_token_response.token` within 60 s — or refuse explicitly
   with `is_error: true`. Silence aborts the run fail-closed.
2. Derive the run's identity from the stream, never from the
   request body; validate `audience` against its own allowlist
   before minting.
3. Scope `sub` to `run-<runId>` and provision the per-run proxy
   policy (repos, verbs) alongside issuance.
4. Set `expires_at` so the harness can warn when the token expires
   before the run's wall-clock budget.

The token is the one intentionally raw credential on the stream —
plaintext in v0.1 — so this flow requires the trusted-network posture.
The feature requires a pre-established gRPC transport before the
executor is built; `stirrup job` is the normal topology, while an
embedder may supply an equivalent transport. Both `executor` blocks
ride on the proto, so a control plane can send them in the
`task_assignment` that starts the run. A timeout or refusal fails loop
construction and closes the stream without `done`. Full contract:
[`deployment.md`](deployment.md#sandbox-identity-token-issuance-control-plane-implementers).

### Batch mode (amortised token pricing)

Goal: run high-volume, latency-tolerant workloads (`research`,
`toil`) at provider batch pricing by letting the control plane
bundle many runs' turns into provider-side batches.

```json
"provider": {
  "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY",
  "batch": { "enabled": true, "maxWaitSeconds": 3600 }
}
```

Per turn the harness emits `batch_submission` whose `input` is:

```json
{ "schema_version": 1, "provider_type": "anthropic",
  "custom_id": "stirrup-<runId>-turn-<n>", "body": { …provider request… } }
```

Control-plane responsibilities:

- `body` is the fully-formed provider request the streaming adapter
  would have POSTed. Wrap it as one entry (with `custom_id`
  preserved verbatim) in a provider batch — Anthropic
  `/v1/messages/batches`, OpenAI `/v1/batches` — and own the whole
  lifecycle: polling or webhooks, results fetch, bundling entries
  from concurrent runs into shared batches.
- Reply with `batch_result` echoing `request_id`; `content` is
  `{"response": …}` on success or
  `{"err": {"type": "batch_expired" | "batch_cancelled" | "invalid_request_error" | "server_error", …}}`.
  The error discriminator must be inside `content`; the current client
  ignores the ControlEvent's `is_error` field. Responses over 4 MiB are
  rejected harness-side as an `invalid_request_error`.
- `batch_waiting` heartbeats mark the wait as healthy. The batch
  client gives up after `maxWaitSeconds` (default 24 h), optionally
  falling back to streaming (`fallbackOnTimeout`), but the task's
  `timeout` context wins first. Because `stirrup job` limits `timeout`
  to 3600 s, a gRPC batch wait cannot currently reach the 24-hour
  default.

Mode gating: `execution` never batches; `research`/`toil` batch
freely; `planning`/`review` need `allowInteractiveModes: true`.
Policy and cost analysis: [`batch.md`](batch.md).

### Collecting results

Four independent channels; use the ones the topology supports:

| Channel | Carries | Topology |
|---|---|---|
| `done` event | Outcome in `stop_reason` | gRPC. The primary terminal signal for a control plane; its `trace` field is currently unset. |
| `STIRRUP_RESULT` stdout line / `resultSink` | `RunResult` JSON: outcome, token usage, duration, `finalAssistantText` (capped at 128 KiB by default), verifier verdict, command-output archive pointer | Any (`resultSink.type: "stdout-json"`); `--output json` is the CLI equivalent. The line lands on process stdout, so a job control plane must collect Pod logs if it needs this detail. Parse the **last** matching line — the sentinel is defence against a model echoing a fake one. |
| Trace emitter | Full event-by-event record (`jsonl` file, `gcs` object `gs://bucket/prefix/<runId>.jsonl`, or `otel` spans/metrics) | Any. JSONL schema: [`trace-inspection.md`](trace-inspection.md). Production builds embed a `Redact()`-ed `RunConfig`; debug builds can explicitly relax trace redaction. |
| Workspace export | `tar.gz` of the sandbox workspace to `gs://…` (`executor.workspaceExportTo`) | Any. Soft-fail by default; `--export-workspace-required` hardens it on the CLI. |

For OTel specifically: `sessionName` labels the run in logs, traces,
and root spans; `observability.environment` / `serviceNamespace`
ride the OTel Resource; `traceEmitter.captureContent` opts spans
into GenAI message-content attributes (off by default, PII).
Backends and protocol matrix:
[`observability-cloud.md`](observability-cloud.md).

### Safety posture quick reference

The five operator-configurable rings, from
[`safety-rings.md`](safety-rings.md):

| Ring | Surface | Default |
|---|---|---|
| 1 — Sandbox runtime class | `executor.runtime` (gVisor/Kata) | Engine default (`runc`); hardening flags always on |
| 2 — Egress allowlist | `executor.network` + proxy / NetworkPolicy | Must be explicit for sandbox executors; `none` gives no network |
| 3 — Cedar policy engine | `permissionPolicy.type: "policy-engine"` | Off; select a non-Cedar permission policy explicitly |
| 4 — Rule of Two | `ruleOfTwo`, `sensitiveData` | Enforced; classifier auto-arms |
| 5 — Code scanner | `codeScanner` | `patterns` in execution mode |

A production-recommended starting posture: container executor with
`runsc`, `network.mode: "allowlist"`, Cedar policy with `deny-all`
fallback, `codeScanner: "patterns"`, Rule of Two enforcement on.
Add `guardRail` (Granite Guardian via vLLM, or `cloud-judge`) for
LLM-based content classification at pre-turn/pre-tool/post-turn —
see [`guardrails.md`](guardrails.md).

## Features not on the wire

The proto mirrors `RunConfig` field-for-field, with a small set of
exceptions. These fields exist on the JSON `RunConfig` (file/stdin
topologies) but **cannot be expressed in a `task_assignment`**. Apart
from `transport`, which is harness-local by design, an integration
that needs one supplies the config file itself (CLI or Cloud Run
topology):

| Surface | Feature | Why |
|---|---|---|
| `transport` | Transport selection. | Deliberate. It describes how the harness was launched, not the work assigned: a config arriving in a `task_assignment` came over the gRPC stream by construction, so the harness sets `grpc` itself. Accepting a wire value would let a control plane declare a transport contradicting the stream carrying the declaration. |
| `traceEmitter.credential`, `traceEmitter.archive` | Credential override for the `gcs` emitter; durable storage for command-output sidecars. | Not yet mapped into the existing proto credential types and translation layer. |
| `tools.profile`, `tools.commandOutput` | Tool presentation profile; command-output capture bounds. | Not yet mirrored. |

Everything else on `RunConfig` is expressible in a
`task_assignment`, including the executor's
`registryAllowlist`, `workspaceExportTo`, `k8sEgressProxyUrl`,
`sandboxIdentity`, and `gitProxy` blocks, plus `resultSink`,
`toolChoiceEscalation`, and `observability.logsExport`.

When a field in this table becomes load-bearing for a control plane,
the project invariant is that it ships with its proto mirror and
translation — treat an entry here as "open an issue", not "work
around silently".

## Compatibility

- `ready.harness_version` carries the build label
  (`v1.2.3 (abc1234)` for releases). Gate task assignment on it if
  the control plane depends on contract features by version.
- For mirrored fields, file-config JSON and protobuf JSON use the same
  lower-camel-case names. The MCP host allowlist spelling is
  `tools.mcpServers[].allowedMcpHosts`; the pre-#554 file spelling
  `allowedMCPHosts` is no longer accepted.
- File-config JSON rejects unknown fields (`DisallowUnknownFields`).
  Protobuf binary compatibility is different: an older harness ignores
  unknown fields. Gate assignment on `ready.harness_version` when a
  task depends on a newly added wire field; do not assume it will fail
  loudly.
- The container image is distroless (`nonroot`, no shell):
  `ghcr.io/rxbynerd/stirrup:<tag>` per release, `:main` per merge.
