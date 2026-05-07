# Guardrails

The `GuardRail` component is a *probabilistic* safety classifier that
runs at three intervention points in the agentic loop: **before the
turn** (untrusted text entering context), **before each tool call**
(model-proposed actions), and **after the turn** (assistant text
leaving the loop). It is the LLM-based counterpart to the
deterministic [safety rings](safety-rings.md) shipped in #42 — the
two are complementary, not alternatives. Deterministic rings catch
attack *shapes* (a tool call to a forbidden host, an edit that lands
a hardcoded secret); guardrails catch attack *content* (a jailbreak
phrasing, a prompt-injection payload, a hallucinated function
reference).

This guide is operator-facing. It is written for someone choosing
whether and how to enable guardrails for the first time — what each
phase does, what to expect at runtime, and how to bring the upstream
classifier online — not for an engineer reading the source.

If you only have time for two things, read [§ When to enable each
phase](#when-to-enable-each-phase) and [§ Fail-open posture](#fail-open-posture).
Those set expectations; the rest is detail.

## Overview

A guardrail is a small classifier model called at three points in
every turn:

| Phase | Where in the loop | What it inspects | What a deny does |
|---|---|---|---|
| `pre_turn`  | At the start of every turn iteration, before context preparation | Untrusted text blocks (tool outputs, fetched web content, dynamic context, the initial user prompt on turn 0) | Aborts the run with outcome `guardrail_blocked`; the offending content never reaches the model |
| `pre_tool`  | Inside the tool dispatch loop, before each tool call is sent to the executor | The model-proposed tool name and JSON input | Returns a synthetic tool result with `IsError: true`; the model sees the failure and may retry. Repeated denies trip the existing stall detector |
| `post_turn` | After `end_turn` stop reason, before the assistant text leaves the loop | The final assistant message | Aborts the run with outcome `guardrail_blocked` |

All three are optional: a `GuardRail.Type: none` (or an unset
GuardRail block) is the default, and the call sites are no-ops.
Enabling guardrails is opt-in per run.

The guard is a *probability reducer*, not an authoriser. A
`VerdictAllow` from the guard does **not** override a deny from
the [Cedar policy engine](safety-rings.md#ring-3--cedar-policy-engine-per-tool-call).
The two questions are different:

- **PermissionPolicy** asks: *is this tool call permitted to run?*
  (Operator-defined rules, evaluated outside the model.)
- **GuardRail** asks: *is this content / proposed call safe to
  evaluate?* (LLM classifier, evaluated by the guard model.)

Both must agree to allow. A real run benefits from layering them.

## When to enable each phase

Guardrail calls are not free — each phase adds a network round-trip
to the classifier and a small amount of latency. Choose phases based
on the attack shape that matters for the run.

### `pre_turn` (recommended for runs with `web_fetch` or `dynamicContext`)

Pre-turn is the strongest defence against **prompt injection** —
attacker-controlled text arriving via a tool output (most commonly
`web_fetch`) or via dynamic context piped in by the control plane
(comments on a fetched issue, fields from a Linear ticket, etc.).

Enable when:
- The mode allows `web_fetch` or any MCP server backed by user-
  generated content.
- The run includes `dynamicContext` from a control plane.
- The agent is acting on a third-party-controlled artefact (a PR
  description, an issue body, a fetched README).

Skip when:
- The run is read-only against a fully-trusted internal corpus and
  no untrusted strings can enter context.

The shipped Granite Guardian adapter applies a **batched** check at
this phase by default: all untrusted chunks for a turn are
concatenated under per-chunk delimiters and submitted in one
outbound request, not one per chunk. Combined with the `MinChunkChars`
skip threshold (default 256), this keeps the per-turn overhead bounded
even when many small tool outputs are returned.

### `pre_tool` (recommended for production)

Pre-tool catches three things:

1. **Hallucinated tool calls** — the model invents a tool that
   doesn't exist or invokes a real tool with malformed input.
   Granite Guardian's built-in `function_call` criterion is
   purpose-built for this.
2. **Coerced tool calls** — a prompt-injected model emits a
   syntactically valid call that semantically does the attacker's
   bidding. Note: deciding whether a syntactically valid call is
   *safe* is the [PermissionPolicy](safety-rings.md#ring-3--cedar-policy-engine-per-tool-call)'s
   job, not the guard's. They overlap in practice; neither replaces
   the other.
3. **Compromised gateway rewrites** — anything between the harness
   and the provider can rewrite tool-call payloads. The guard
   evaluates the call as the harness sees it, so a rewritten call
   gets the same scrutiny as a model-emitted one.

Enable when:
- The mode permits side-effecting tools (`run_command`, `edit_file`,
  `web_fetch`, `spawn_agent`).

### `post_turn` (recommended for surfaces that show output to humans)

Post-turn checks the final assistant message before it is forwarded
to the user. Built-in default criterion combines:

- **Harm** — output that promotes harm to people, property, or
  systems.
- **Groundedness** — factual claims unsupported by the documents
  in the prior turns. Useful for read-only modes where the agent's
  job is to summarise or extract.
- **Secret leak** — AWS access keys, SSH private keys, and a
  configurable corp-domain pattern in the response.

Enable when:
- The output goes to a human reviewer or downstream system.
- The run is a research / planning / review mode (read-only, but
  whose written brief is the deliverable).

Skip when:
- The run is a fully-automated execution loop where the assistant
  text is observability noise and the workspace edits are the
  deliverable.

## Adapters

Three adapter types ship in the harness, plus a no-op default:

### `none` (default)

The guard is a no-op. Call sites short-circuit before any work.
This is the default when `GuardRail` is unset or `Type: ""`.

### `granite-guardian`

[Granite Guardian 4.1-8B](https://huggingface.co/ibm-granite/granite-guardian-4.1-8b)
served via vLLM (or any OpenAI-compatible chat completions endpoint).
The harness ships vetted criteria text per phase and constructs the
classifier prompt — you supply only the endpoint and (optionally) a
model name and bespoke criteria.

Minimal config:

```json
{
  "guardRail": {
    "type": "granite-guardian",
    "endpoint": "http://127.0.0.1:1234"
  }
}
```

Or via flags:

```sh
stirrup harness \
  --prompt "..." \
  --guardrail granite-guardian \
  --guardrail-endpoint http://127.0.0.1:1234
```

### `cloud-judge`

Reuses an existing `ProviderAdapter` (Anthropic, OpenAI-compatible)
with a hard-coded cheap model — Anthropic Haiku is the documented
default. No new SDK dependency: it streams a single low-temperature
completion and extracts the verdict from a JSON field in the
response. Use this when the deployment cannot run its own GPU-
attached vLLM.

### `composite` (operator escape hatch)

Layers multiple stages, optionally restricted to specific phases.
The harness only ships `granite-guardian` and `cloud-judge`, but
the composite primitive lets operators add their own classifiers
(e.g. a fast-path Llama Prompt Guard 2 served via TEI in front of
the slower Granite Guardian) without modifying the harness. See
[§ Operator escape hatch](#operator-escape-hatch) below.

There is **no fast-path adapter shipped in the harness**. If you
need sub-100ms guard latency, write a custom adapter and compose
it via `phase-gated` in front of `granite-guardian`.

## Latency budget

Approximate added overhead per turn with a stub vLLM responding
instantly. Real numbers depend on classifier throughput and prompt
length; treat these as advisory.

| Phase | p50 | p99 | Notes |
|---|---|---|---|
| `pre_turn`  | < 50 ms | < 600 ms | Single batched call per turn (all untrusted chunks). Skipped entirely for chunks shorter than `MinChunkChars` (default 256). |
| `pre_tool`  | < 30 ms | < 200 ms | One call per tool invocation. Adds up over many tool calls per turn. |
| `post_turn` | < 50 ms | < 600 ms | Single composite-criterion call per turn end. |

With `failOpen: true`, the error path is a single timeout (defaults
to `timeoutMs: 1500`) before the request is allowed to proceed. With
`failOpen: false` (the default), a transport error or timeout
produces a `Deny` and the offending content does not pass.

The two load-bearing latency mitigations are:

1. **`MinChunkChars` skip** at `pre_turn`. Chunks shorter than the
   threshold (default 256 chars) are not sent to the classifier;
   a `guard_skipped` event is emitted instead.
2. **Batched composite criterion** at `pre_turn` and `post_turn`.
   The default config issues one outbound request per phase per
   turn, regardless of chunk count.

If you change the default criteria to a per-criterion list (`Criteria:
["jailbreak", "harm", "groundedness"]`), the adapter falls back to
serial calls and the per-turn overhead grows linearly with the list.

## Fail-open posture

The default is **fail-closed**: any transport error, timeout, or
malformed response from the classifier produces a `VerdictDeny` and
the run is blocked. This matches the principle that an unreachable
guardrail is no guardrail.

`failOpen` is a **top-level GuardRailConfig setting only**. It governs
the entire guardrail tree for the run. Per-stage `failOpen` is not
supported: a composite policy with mixed fail-open settings would
silently mask transport errors in one stage while blocking on
another, and that is confusing rather than useful. Set the field once
at the top level, alongside `type` / `endpoint` / `phases`.

Set `failOpen: true` only when:
- Degraded vLLM availability is part of the normal operating
  envelope (e.g. a shared cluster with frequent rollouts).
- The classifier sits in front of an already-defence-in-depth stack
  (Cedar policy + container runtime + egress allowlist) and the
  guardrail is supplementary.

When `failOpen: true` triggers, the harness emits a `guard_error`
security event with the underlying cause. Production deployments
should alert on the rate of `guard_error` events: a healthy fleet
sees ~0%; a sudden rise indicates the classifier is down.

## Bringing up vLLM locally

Granite Guardian 4.1-8B is published on Hugging Face and served by
vLLM via the OpenAI-compatible chat completions endpoint. The
shipped `just guardian-smoke` recipe expects the endpoint at
`http://127.0.0.1:1234`.

The minimal docker invocation (assumes an Nvidia GPU host with the
container toolkit installed):

```sh
docker run --rm -p 1234:8000 \
  --gpus all \
  vllm/vllm-openai:latest \
  --model ibm-granite/granite-guardian-4.1-8b \
  --port 8000
```

After the container starts, verify connectivity:

```sh
just guardian-smoke
```

If the model is reachable and named in `/v1/models`, the recipe
prints `ok: granite-guardian available at http://127.0.0.1:1234`.

For non-GPU operators, use the `cloud-judge` adapter instead. It
reuses the Anthropic adapter with Haiku as the classifier model
and adds no new dependencies.

### LM Studio and other DeepSeek-style runtimes

vLLM is the reference runtime: it honours Granite Guardian's
`<no-think>` directive verbatim and emits the full classifier output
into the OpenAI `content` field. Other OpenAI-compatible runtimes —
notably LM Studio, but also any backend modelled after the DeepSeek
chat template — behave differently in two ways that matter for the
adapter:

1. **`<no-think>` is silently ignored.** The underlying model still
   reasons before emitting `<score>`, typically burning ~80 tokens of
   reasoning. The default `noThinkMaxTokens` budget (256) is sized to
   absorb this with margin; if you see `ErrResponseTruncated` errors
   in operator logs, bump the budget further or move to vLLM.
2. **Reasoning is routed to a separate `reasoning_content` field.**
   The score itself still arrives in `content`, so the adapter parses
   correctly — but the tokens spent on reasoning are charged against
   the same `max_tokens` budget. This is the failure mode the
   ErrResponseTruncated detector exists to surface: empty `content` +
   `finish_reason: "length"` is the unmistakable fingerprint.

If you intend to run guardrails on a non-vLLM runtime in production,
either run with `Think: true` (uses the larger 512-token budget and
stops feeling like a knife edge), or characterise your runtime's
typical reasoning cost and set `Think: false` only when you are
confident the score head can fire under the configured budget.

**Latency expectations.** Local runtimes are also markedly slower
than vLLM on a datacentre GPU. With `stream: false` (which the
adapter uses — guards must be synchronous), runtimes generate the
full response before emitting headers, so first-byte latency tracks
total latency almost exactly. Empirically, LM Studio on Apple
Silicon serving Granite Guardian 4.1-8B lands at ~5-6s per call for
the default no-think configuration; a vLLM A100 deployment lands
sub-second. The shipped default `timeoutMs` (10000 = 10s) is sized
to absorb the LM Studio case with margin. Operators on fast
hardware should tighten this in their RunConfig:

```json
"guardRail": {
  "type": "granite-guardian",
  "endpoint": "http://your-vllm:8000",
  "timeoutMs": 1500
}
```

A guard timeout fires `guard_error` and, with the default
`failOpen: false`, aborts the run on PhasePreTurn or denies the
tool call on PhasePreTool. That is the correct safety posture, but
on slow local hardware it surfaces as "every dev run instantly
fails" — almost always a sign that `timeoutMs` is too low for the
runtime, not that the classifier is genuinely unhealthy.

## Operator escape hatch

The composite primitive lets you layer additional adapters in front
of (or instead of) the shipped ones. A common pattern: drop a
fast-path classifier (e.g. [Llama Prompt Guard
2](https://huggingface.co/meta-llama/Llama-Prompt-Guard-2-86M),
served via Hugging Face Text Embeddings Inference) in front of
Granite Guardian. The fast-path catches obvious injections in
single-digit milliseconds; only the survivors pay the full Granite
Guardian round-trip.

Composite config skeleton:

```json
{
  "guardRail": {
    "type": "composite",
    "stages": [
      {
        "type": "your-fast-path-adapter",
        "endpoint": "http://127.0.0.1:8080",
        "phases": ["pre_tool"]
      },
      {
        "type": "granite-guardian",
        "endpoint": "http://127.0.0.1:1234"
      }
    ]
  }
}
```

Each stage can carry its own `phases` restriction; an unset
`phases` means the stage runs at every phase. Stages run
sequentially: the first deny short-circuits.

To add an adapter type, implement the `GuardRail` interface in a
new file under `harness/internal/guard/` and register it in the
factory's switch statement. The interface is small (one method)
and intentionally stable; existing implementations are good
references.

## Trace and metrics

Enable guardrails and you get a stable observability surface:

### OTel spans

Each guard call produces a span named `guard.<phase>` (e.g.
`guard.pre_turn`, `guard.pre_tool`, `guard.post_turn`), child of
the corresponding `turn` / `tool.<name>` / `provider.stream` span.
Standard attributes: `guard.id`, `guard.criterion`, `guard.score`,
`guard.verdict`, `guard.latency_ms`.

### OTel metrics

Five instruments emitted by `harness/internal/observability/metrics.go`:

| Metric | Type | Attributes |
|---|---|---|
| `stirrup.guard.checks` | counter | `guard.id`, `guard.phase`, `guard.verdict` |
| `stirrup.guard.errors` | counter | `guard.id`, `guard.phase` |
| `stirrup.guard.skips` | counter | `guard.id`, `guard.phase`, `reason` |
| `stirrup.guard.spotlights` | counter | `guard.id` |
| `stirrup.guard.duration_ms` | histogram | `guard.id`, `guard.phase` |

A healthy production fleet sees `errors` and `skips` at low rates
relative to `checks`. A sudden rise in `errors` is the operational
signal for a degraded classifier.

### Security events

Five new event types on the `SecurityEventEmitter`:

- `guard_allowed` (debug-level)
- `guard_spotlighted`
- `guard_denied`
- `guard_skipped` (debug-level — emitted for `MinChunkChars` skips)
- `guard_error`

Content under inspection is **not** logged at info level — only its
hash and length. Set `--log-level debug` to include content payloads
in logs (do this only when investigating false positives, and never
in shared environments).

For broader context on how guardrails fit alongside the
deterministic safety rings, see [`docs/safety-rings.md`](safety-rings.md).
The short version: guardrails catch what the rings cannot
(content-level attacks), and the rings catch what guardrails cannot
(structural and policy violations). Both are needed.

## References

- IBM, *Granite Guardian 4.1-8B* model card —
  <https://huggingface.co/ibm-granite/granite-guardian-4.1-8b>.
- Hines et al., *Defending Against Indirect Prompt Injection
  Attacks With Spotlighting* — arXiv:2403.14720.
- Issue [#43](https://github.com/rxbynerd/stirrup/issues/43) for
  the original design rationale and chunk-by-chunk implementation
  history.
