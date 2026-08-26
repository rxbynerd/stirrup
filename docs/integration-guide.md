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
| **Go embedding** (`harness/harnessapi`) | `BuildLoopWithTransport(ctx, config, transport)` runs the loop in-process. | Single-binary tools bundling their own control-plane logic. Everything under `harness/internal/*` is not public API. |

The four surfaces consume the same `RunConfig` composition root, so a
config developed interactively with the CLI transfers to the gRPC
path field-for-field — the JSON field names are the proto field
names.

## The wire contract

### One RPC

```proto
service HarnessService {
  rpc RunTask(stream HarnessEvent) returns (stream ControlEvent);
}
```

The **control plane implements the server**; the harness is the
client and connects outbound. There is no inbound port on the
harness, no service mesh hop, and no shared filesystem. One stream
carries the whole run: task assignment, streaming output, permission
round-trips, and the terminal result.

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
   When `CONTROL_PLANE_SESSION_ID` was set in the Pod environment,
   the `id` field echoes it so the stream can be correlated to the
   session that launched the Pod.
3. The control plane sends
   `ControlEvent{type:"task_assignment", task: RunConfig}` — this
   must be the first `ControlEvent`. The harness waits at most
   **5 minutes** for it, then exits. A `cancel` sent before
   assignment makes the harness exit cleanly without running.
4. If the `RunConfig` opts in, the harness sends one
   `sandbox_token_request` and blocks (fail-closed, 60 s) for the
   `sandbox_token_response` before creating the sandbox.
5. During execution the harness streams `text_delta`, `tool_call`,
   `tool_result`, `warning`, and a `heartbeat` every **30 seconds**.
   Depending on configuration it may also emit `permission_request`,
   `tool_result_request`, `batch_submission`, `batch_waiting`, and
   `batch_cancel_request`.
6. The run ends with `HarnessEvent{type:"done", stop_reason, trace}`.
   On a fatal error an `error` event (human-readable `message`)
   precedes the `done` — `done` is always the terminal signal.
7. If a follow-up grace window is configured, the stream stays open
   for `user_response` events that trigger additional runs;
   otherwise the stream closes and the process exits.

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
| `error` | `message` | Record; a `done` follows. |
| `done` | `stop_reason`, `trace` | Terminal. Read `trace.outcome` for the canonical status. |
| `batch_submission` | `request_id`, `input` (BatchSubmission JSON) | Batch mode only — see [Batch mode](#batch-mode-amortised-token-pricing). |
| `batch_waiting` | `request_id` | Batch keep-alive (roughly every 5 minutes) distinguishing "provider batch still pending" from a stall. |
| `batch_cancel_request` | `request_id` | Best-effort: cancel the provider-side batch. The harness does not wait for a reply. |
| `sandbox_token_request` | `request_id`, `audience` | See [Sandbox identity tokens](#sandbox-identity-tokens-authenticated-git-egress). |

### Events to the harness

| `type` | Fields | Semantics |
|---|---|---|
| `task_assignment` | `task` (RunConfig) | First event on the stream. Duplicate assignments are ignored. |
| `user_response` | `user_response` | Free text injected as a user message. Mid-run: next turn. During the follow-up grace window: starts a follow-up run. |
| `permission_response` | `request_id`, `allowed`, `reason` | Decision for a `permission_request`. `reason` on a denial is passed to the model as context. |
| `tool_result_response` | `request_id`, `content`, `is_error` | Result payload for a `tool_result_request`. `is_error: true` surfaces to the model as a tool failure. |
| `batch_result` | `request_id`, `content` (BatchResult JSON), `is_error` | Completes a `batch_submission`. |
| `sandbox_token_response` | `request_id`, `token`, `expires_at`, `is_error`, `reason` | The signed sandbox identity JWT, or an explicit refusal. |
| `cancel` | — | Abort the run within one turn boundary. Git finalisation still runs; the final `done` carries `stop_reason:"cancelled"`. |

Correlation rule: every `*_request` event carries a `request_id`
that the response **must echo verbatim**. Requests may interleave;
responses are matched by ID, not order.

### Terminal semantics

`done.trace` is a `RunTrace`: `run_id`, `turns`, `input_tokens`,
`output_tokens`, `cost_usd`, `duration_ms`, `stop_reason`, and
`outcome`. Read **`trace.outcome`** — it is the canonical terminal
status for analytics:

```
success | error | max_turns | verification_failed | verification_error |
budget_exceeded | stalled | tool_failures | cancelled | timeout | max_tokens
```

`trace.stop_reason` mirrors `outcome` for backward compatibility.
The top-level `done.stop_reason` additionally surfaces
`setup_failed` (a `preRun` hook or git setup failed before any turn)
and `hook_failed` (a fatal `postRun` hook overrode an otherwise
successful run).

One caveat: `done` is guaranteed only once the loop has been built.
A `RunConfig` that fails validation on arrival never produces a
loop — the process exits non-zero and the stream closes with no
`error`/`done` pair. Validate configs control-plane-side before
sending (mirror the harness with `stirrup run-config --validate`, or
call `types.ValidateRunConfig` from Go) rather than treating stream
closure as a result.

### Transport security posture (v0.1)

The gRPC transport is **plaintext and unauthenticated** — the
harness dials with insecure credentials and `TransportConfig`
exposes only `{type, address}`. Acceptable deployment shapes are
same-host loopback, a private network, or mesh-provided mTLS
(Istio, Linkerd). API keys never transit the stream (they travel as
`secret://` references and outbound events are scrubbed for
secret-shaped strings), but prompts, tool results, and especially
the raw sandbox identity JWT do. Do not point a harness at a control
plane across an untrusted network. TLS at the config surface is
planned; Go embedders can already wire
`transport.WithTLSCredentials` directly. Full discussion:
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
5. On `done`, record `trace.outcome` and close.

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
			stream.Send(&pb.ControlEvent{
				Type:      "permission_response",
				RequestId: ev.RequestId,
				Allowed:   &pb.OptionalBool{Value: s.decide(ev.ToolName, ev.Input)},
			})
		case "done":
			s.recordOutcome(ev.Trace)
			return nil
		}
	}
}
```

Launch the harness as a Kubernetes Job running `stirrup job` with
`CONTROL_PLANE_ADDR` pointing at this server. Set
`Job.spec.activeDeadlineSeconds` slightly above `RunConfig.timeout`
so Kubernetes reaps a stuck Pod even if the harness's own wall-clock
timeout fails, and probe liveness with
`test -f /tmp/healthy`. See the
[operator checklist](deployment.md#operator-checklist).

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

Secrets follow one rule: **credentials never appear in a
`RunConfig`**. Every `apiKeyRef` must be a `secret://` reference —
`secret://NAME` (environment variable on the harness),
`secret://file:///path`, or `secret://ssm:///param-name` (AWS SSM) —
resolved harness-side at runtime. A literal key is rejected by
validation, and `RunConfig.Redact()` strips references before any
trace is persisted. Cross-cloud alternatives (IRSA, GKE Workload
Identity, Azure/Anthropic/OpenAI WIF) are configured via
`provider.credential` — see
[`credential-federation.md`](credential-federation.md).

## Cookbook

Each recipe shows the JSON shape of the `task_assignment` payload
(field names are identical on the proto and in CLI config files).
Fields covered by an earlier recipe are elided with `…`.

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
- The `api` executor (`executor.type: "api"` + `vcsBackend`) runs
  this shape against a GitHub/GitLab repo with no workspace at all.

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
  `no-new-privileges`, read-only rootfs, nobody-uid, no network by
  default. `runtime: "runsc"` (gVisor) adds a kernel boundary.
- `network.mode: "allowlist"` + `network.allowlist` starts an
  in-process egress proxy enforcing an FQDN allowlist; `none` means
  no network at all.
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

- `policyFile` is read by the **harness process** at boot — stage it
  via ConfigMap mount or bake it into the image. Policy text cannot
  be shipped inside the `RunConfig`.
- Cedar answers allow/deny/no-decision; `fallback` decides
  no-decisions. A permit-based allowlist is only meaningful with
  `fallback: "deny-all"` — the default `deny-side-effects` would
  still allow non-mutating gated tools.
- Policies are linted at load; error-severity findings (dead
  clauses, unanchored wildcard hosts) abort the run before any turn.
- Starter policies: [`examples/policies/`](../examples/policies/).
  Semantics: [`safety-rings.md`](safety-rings.md#ring-3--cedar-policy-engine).

### Injecting task context

Goal: give the agent issue bodies, retrieved documents, or customer
records without granting them instruction authority.

```json
"dynamicContext": {
  "issue_body": { "value": "…external issue text…" },
  "customer_record": { "value": "…", "sensitive": true }
}
```

- Every entry is wrapped in `<untrusted_context>` blocks by the
  prompt builder and treated as data, not instructions. Entries are
  sanitised (tag stripping, 50 KB cap).
- `sensitive: true` per entry — or `sensitiveData: true` at the top
  level — declares the "sensitive data" leg of the Rule of Two.
  Declare honestly: the harness deliberately does not infer
  sensitivity from credentials it manages itself.

### Rule of Two: the posture invariant

A run must not simultaneously hold **untrusted input**, **sensitive
data**, and **external communication** unless every dangerous call
is gated by `ask-upstream`. `ValidateRunConfig` rejects the
all-three combination outright; two legs produce a
`rule_of_two_warning` security event.

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

Full design: [`safety-rings.md`](safety-rings.md#ring-4--agents-rule-of-two).

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
  `dynamic` (escalate on turn count, token usage, or a stop reason
  outside `cheapStopReasons`).
- Behaviour knobs are provider-neutral: `temperature` and
  `reasoningEffort` sit at the top level of `RunConfig` and each
  adapter projects them onto its native wire control.
- Provider-specific wire quirks (Azure headers/query params, Z.ai
  compat profile, Gemini safety settings) live on `ProviderConfig`.
  See [`providers.md`](providers.md).

### Budgets and limits

```json
"maxTurns": 40,
"timeout": 1800,
"maxTokenBudget": 2000000,
"maxCostBudget": 5.0,
"contextStrategy": { "type": "summarise", "maxTokens": 150000 }
```

- Token/cost budgets terminate the run with
  `outcome: "budget_exceeded"`; wall-clock expiry yields `timeout`.
  Ceilings: 50 M tokens, $100, 3600 s.
- `contextStrategy` bounds the conversation itself:
  `sliding-window` (default), `summarise`, or `offload-to-file`.
- Pair `timeout` with an infrastructure-level deadline
  (`activeDeadlineSeconds`) — defence in depth against a wedged
  process.

### Verification: don't trust "done"

```json
"verifier": {
  "type": "composite",
  "verifiers": [
    { "type": "test-runner", "command": "go test ./...", "timeout": 300 },
    { "type": "llm-judge", "criteria": "The diff addresses the issue without unrelated changes." }
  ]
}
```

A run that ends `end_turn` but fails verification reports
`outcome: "verification_failed"` — the control plane can retry,
escalate to a stronger model, or route to a human without parsing
any output.

### Follow-up turns

Goal: keep the conversation alive after the run completes — cheap
steering without re-provisioning a sandbox.

- Set `followUpGrace` (seconds, ≤ 3600) in the `RunConfig`, or
  `STIRRUP_FOLLOWUP_GRACE` in the Pod environment (the config field
  wins).
- After `done`, the stream stays open for the grace window. A
  `user_response` ControlEvent starts a follow-up run in the same
  process and workspace; each follow-up ends with its own `done`.
- A `cancel` during the grace window closes the stream promptly
  without an extra `done`.

### Cancelling a run

Send `ControlEvent{type:"cancel"}` at any time. The harness stops
within one turn boundary: in-flight provider streams and tool calls
are cancelled via context, git finalisation still runs, and the
final `done` carries `stop_reason:"cancelled"`. Infrastructure-level
kill (SIGTERM) is the fallback; the harness flushes traces and the
result sink on a bounded shutdown grace.

### Control-plane-fulfilled tools (`tool_result_request`)

The async-tool contract lets a tool's *result* come from the control
plane instead of the harness: the loop emits `tool_result_request`
(`request_id`, `tool_use_id`, `tool_name`, `input`) and blocks under
a per-call timeout; the control plane answers with
`tool_result_response` (`content`, optional `is_error: true` to
surface a failure to the model). Concurrent async calls in one turn
fan out under the `toolDispatch.maxParallel` semaphore (default 4,
ceiling 16). No shipped built-in tool defers upstream today — the
mechanism is the extension point for embedder-registered tools and
future MCP deferral.

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

The token is the one raw credential on the stream — plaintext in
v0.1 — so this flow requires the trusted-network posture. This
feature is exclusive to the `stirrup job` topology (a
pre-established transport must exist before the executor is built);
these `executor` fields travel in the file-based `RunConfig` today,
not the proto (see [Features not on the wire](#features-not-on-the-wire)).
Full contract:
[`deployment.md`](deployment.md#sandbox-identity-token-issuance-control-plane-implementers).

### Batch mode (amortised token pricing)

Goal: run high-volume, latency-tolerant workloads (`research`,
`toil`) at provider batch pricing by letting the control plane
bundle many runs' turns into provider-side batches.

```json
"provider": {
  "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY",
  "batch": { "enabled": true, "maxWaitSeconds": 86400 }
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
  `{"err": {"type": "batch_expired" | "batch_cancelled" | "invalid_request_error" | "server_error", …}}`
  with `is_error: true`. Responses over 4 MiB are rejected
  harness-side as an `invalid_request_error`.
- `batch_waiting` heartbeats mark the wait as healthy; the harness
  gives up after `maxWaitSeconds` (default 24 h) with
  `batch_expired`, optionally falling back to streaming
  (`fallbackOnTimeout`).

Mode gating: `execution` never batches; `research`/`toil` batch
freely; `planning`/`review` need `allowInteractiveModes: true`.
Policy and cost analysis: [`batch.md`](batch.md).

### Collecting results

Four independent channels; use the ones the topology supports:

| Channel | Carries | Topology |
|---|---|---|
| `done` event | `RunTrace` metrics + outcome | gRPC. The primary result for a control plane. |
| `STIRRUP_RESULT` stdout line / `resultSink` | `RunResult` JSON: outcome, token usage, `finalAssistantText` (capped 128 KiB), verifier verdict, command-output archive pointer | CLI / Cloud Run (`resultSink.type: "stdout-json"` or `--output json`). Parse the **last** matching line — the sentinel is defence against a model echoing a fake one. |
| Trace emitter | Full event-by-event record (`jsonl` file, `gcs` object `gs://bucket/prefix/<runId>.jsonl`, or `otel` spans/metrics) | Any. JSONL schema: [`trace-inspection.md`](trace-inspection.md). The `RunConfig` embedded in a trace is always `Redact()`-ed. |
| Workspace export | `tar.gz` of the sandbox workspace to `gs://…` (`executor.workspaceExportTo`) | CLI / Cloud Run today (not on the proto). Soft-fail by default; `--export-workspace-required` hardens it. |

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
| 2 — Egress allowlist | `executor.network` + proxy / NetworkPolicy | `none` (no network) |
| 3 — Cedar policy engine | `permissionPolicy.type: "policy-engine"` | Off (simple policies) |
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
deliberate exceptions. These features exist on the JSON `RunConfig`
(file/stdin topologies) but **cannot be expressed in a
`task_assignment`** today; an integration that needs them supplies
the config file itself (CLI or Cloud Run topology):

| Surface | Feature |
|---|---|
| `resultSink` | Sink selection (`stdout-json`). gRPC control planes read the `done` event instead. |
| `executor.workspaceExportTo` | GCS workspace export. |
| `executor.sandboxIdentity` / `executor.gitProxy` | Requested via file config; the token *exchange* itself runs over the gRPC stream. |
| `executor.registryAllowlist` | Sandbox image registry pinning. |
| `executor.k8sEgressProxyUrl` | K8s egress proxy wiring. |
| `toolChoiceEscalation` | Bounded tool-choice recovery loop. |
| `observability.logsExport` | OTLP log export. |
| `transport` | Inherently harness-local. |

The `stirrup job` path composes with none of these except
`sandboxIdentity` (whose event pair is on the wire); everything else
requires the harness to have been started with a file-based config.
When a field here becomes load-bearing for a control plane, the
project invariant is that it ships with its proto mirror and
translation — treat an entry in this table as "open an issue", not
"work around silently".

## Compatibility

- `ready.harness_version` carries the build label
  (`v1.2.3 (abc1234)` for releases). Gate task assignment on it if
  the control plane depends on contract features by version.
- JSON `RunConfig` field names are the proto field names — configs
  are portable between the CLI, Cloud Run, and gRPC surfaces modulo
  the table above.
- Unknown JSON fields are rejected (`DisallowUnknownFields`), so a
  config written for a newer harness fails loudly on an older one.
- The container image is distroless (`nonroot`, no shell):
  `ghcr.io/rxbynerd/stirrup:<tag>` per release, `:main` per merge.
