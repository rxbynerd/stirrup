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
| `pre_turn`  | At the start of every turn iteration, before context preparation | Untrusted text blocks (tool outputs, fetched web content, dynamic context, the initial user prompt on turn 0, verifier feedback on the turn a verification retry starts (or when it is appended, if it joins a user message already in history), and mid-run `user_response` input at the turn it is injected — the latter classified regardless of length, since a short relayed turn is the common case) | Aborts the run with outcome `guardrail_blocked`; the offending content never reaches the model |
| `pre_tool`  | Inside the tool dispatch loop, after the deterministic input checks and before the permission check and executor (see [§ Dispatch order](#dispatch-order)) | The resolved tool name and the cleaned JSON input of a call that passed the deterministic checks | Returns a synthetic tool result with `IsError: true`; the model sees the failure and may retry. Repeated denies trip the existing stall detector |
| `post_turn` | After `end_turn` stop reason, before the assistant text leaves the loop | The final assistant message | Aborts the run with outcome `guardrail_blocked` |

All three are optional: a `GuardRail.Type: none` (or an unset
GuardRail block) is the default, and the call sites are no-ops.
Enabling guardrails is opt-in per run.

The guard is a *probability reducer*, not an authoriser. A
`VerdictAllow` from the guard does **not** override a deny from
the [Cedar policy engine](safety-rings.md#ring-3--cedar-policy-engine-per-call-authorization).
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
outbound request, not one per chunk. Combined with the opt-in
`minChunkChars` skip threshold (zero, meaning disabled, unless set),
this keeps the per-turn overhead bounded even when many small tool
outputs are returned.

Every `pre_turn` input reaches the guard with the log scrubber applied,
and text the scrubber hid from the guard is fenced for the model in
tool results and turn-0 dynamic context; see [Guard input](#guard-input)
and
[Guard input is scrubbed, and hidden spans are fenced](security.md#scrubbed-content-is-fenced).
A deny or spotlight of arrived tool results, like the harness's own
fence, drops each result's structured payload, which the Anthropic and
Gemini adapters would otherwise send beside the rewritten text.

### `pre_tool` (recommended for production)

Pre-tool catches three things:

1. **Hallucinated tool calls** — the model invokes a real tool with
   arguments that are well-formed but make no sense for the task.
   Calls to tools that do not exist, and inputs that fail the tool's
   JSON schema, never reach the guard: the deterministic checks reject
   them first (see [§ Dispatch order](#dispatch-order)). Granite
   Guardian's built-in `function_call` criterion, the `pre_tool`
   default, therefore sees only schema-valid calls.
2. **Coerced tool calls** — a prompt-injected model emits a
   syntactically valid call that semantically does the attacker's
   bidding. Note: deciding whether a syntactically valid call is
   *safe* is the [PermissionPolicy](safety-rings.md#ring-3--cedar-policy-engine-per-call-authorization)'s
   job, not the guard's. They overlap in practice; neither replaces
   the other.
3. **Compromised gateway rewrites** — anything between the harness
   and the provider can rewrite tool-call payloads. The guard
   evaluates the call as the harness sees it, so a rewritten call
   gets the same scrutiny as a model-emitted one.

Enable when:
- The mode permits side-effecting tools (`run_command`, `edit_file`,
  `web_fetch`, `spawn_agent`).

#### Dispatch order

Every tool call passes the free, deterministic checks before any
classifier call, so a call that is rejected anyway never pays a
guard round-trip:

```text
tool_use block from the model
  1. resolve the tool name          unknown          -> unknown_tool
                                    deferred and not -> tool_not_loaded
                                    yet loaded
  2. strip __proto__ / constructor  keys dropped     -> prototype_pollution_blocked event
  3. JSON-schema validation         invalid          -> schema_validation_failed
  4. tool-input tripwires           match            -> security_guard_denied
  5. pre_tool guard                 deny, or error   -> guardrail_denied
                                    without failOpen
  6. PermissionPolicy               deny             -> permission_denied
  7. handler / executor
```

A call to a tool that [lazy tool loading](configuration.md#lazy-tool-loading)
defers and no search has loaded stops at step 1, before the `pre_tool`
guard and the permission policy see it. Like any failed call, repeated
rejections count toward the stall detector's limit of consecutive
failures.

Step 4 is `security.GuardToolCall`: network-exfiltration utilities
and shell-escape syntax in command fields, credential paths, writes
to loaded repository guidance or `tools.protectedWriteTargets`, and
long base64-like payloads. It scans the original input, including
keys step 2 drops, so a tripwire nested under a stripped key still
fires. Steps 1-4 are mandatory and cannot be disabled.

The guard at step 5 classifies the input after step 2's strip, the
same bytes the handler receives, and sees the resolved internal tool
name rather than a toolset-profile alias. A call rejected at steps
1-4 emits no `guard_*` event and reports its deterministic failure
category. The permission check follows the guard because
`ask-upstream` may block on a human approval, which a call the guard
denies should never request.

**Telemetry shift.** With this order, a call that both the
deterministic checks and a classifier would reject reports
`unknown_tool`, `schema_validation_failed`, or
`security_guard_denied`, where releases that ran the guard first
reported `guardrail_denied`. Such a call is never classified: it
produces no `guard.pre_tool` span, no `stirrup.guard.checks` or
`stirrup.guard.duration` samples, and no `guard_*` security event,
so the mix of tool-failure categories shifts towards the
deterministic ones. Alerts that count `guard_denied` events to detect
injection attempts no longer see calls the deterministic checks
reject; step 4 still emits `tool_call_guard_triggered` for its
matches.

### `post_turn` (recommended for surfaces that show output to humans)

Post-turn checks the final assistant message before it is forwarded
to the user. Built-in default criterion combines:

- **Harm** — output that promotes harm to people, property, or
  systems.
- **Groundedness** — factual claims unsupported by the documents
  in the prior turns. Useful for read-only modes where the agent's
  job is to summarise or extract.
- **Secret leak** — AWS access keys, SSH private keys, and a
  configurable corp-domain pattern in the response. The guard reads the
  response with the log scrubber applied, so a secret in a format the
  scrubber recognises reaches it as `[REDACTED]` and is already
  redacted from the control stream and trace; this criterion catches
  the formats the scrubber does not know.

Enable when:
- The output goes to a human reviewer or downstream system.
- The run is a research / planning / review mode (read-only, but
  whose written brief is the deliverable).

Skip when:
- The run is a fully-automated execution loop where the assistant
  text is observability noise and the workspace edits are the
  deliverable.

## Guard input

Every `guard.Input` reaches the guard with the log scrubber applied,
when a guard checks that phase, except that `secret://` references are
left as written: they point at a secret rather than hold one. The
scrubber replaces each match with the literal text `[REDACTED]`, which
can also occur in genuine content. `Input.Metadata["redacted_spans"]`, when set, is the number of
scrubber matches across the whole input: two matches inside one fence
count two, and a secret in both a tool result's text and its
structured payload counts twice. The shipped adapters (`granite-guardian`,
`shieldstral`, `cloud-judge`) do not forward it, so for them the
placeholder is the only signal that something was replaced.

| `Source` | Phase | Content | What the model sees | Hidden spans fenced | Guard `spotlight` verdict |
|---|---|---|---|---|---|
| `batched:n=N` (turn 0) | `pre_turn` | the prompt, then each dynamic-context value in key order, after sanitisation and the Rule-of-Two `redact` action | the prompt as written; dynamic context with hidden spans fenced | dynamic context only | not applied (a deny ends the run) |
| `user_response:n=N` | `pre_turn` | the queued inputs in arrival order | as written | no | not applied (a deny ends the run) |
| `batched:n=N` (tool results) | `pre_turn` | each result's text, then its structured payload | the result with hidden spans fenced | yes, on allow | wraps each result once |
| `verifier_feedback:n=N` | `pre_turn` | the feedback's text blocks | the scrubbed copy | no (already scrubbed) | wraps the feedback |
| `model_output` | `post_turn` | the final assistant text | unchanged | no | logged only |
| `tool_call:<name>` | `pre_tool` | the call's arguments, also as `ToolInput` (valid JSON) | unchanged; the handler gets the arguments as written | no | not applicable |

`N` is the number of chunks. A batched `pre_turn` input is the chunks
joined under `--- chunk N ---` header lines, counted from 0; a
single-chunk batch still has one header. Turn 0 and arrived tool
results share the `batched:` source, which is kept so the
`guard.source` span attribute stays stable. Repository guidance is not
part of the turn-0 input.

A guard cannot judge text the scrubber hid. For tool results and
dynamic context the harness fences that text for the model; the
prompt and `user_response` are operator input and are not fenced, so
ticket or chat text a control plane relays through them is a blind
spot inside scrubber matches. For `pre_tool` the hidden text can be
exactly the argument under review. See
[Guard input is scrubbed, and hidden spans are fenced](security.md#scrubbed-content-is-fenced).

## Adapters

Four adapter types ship in the harness, plus a no-op default:

### `none` (default)

The guard is a no-op. Call sites short-circuit before any work.
This is the default when `GuardRail` is unset or `Type: ""`.

### `granite-guardian`

[Granite Guardian 4.1-8B](https://huggingface.co/ibm-granite/granite-guardian-4.1-8b)
served via vLLM (or any OpenAI-compatible chat completions endpoint).
The harness ships vetted criteria text per phase and constructs the
classifier prompt — you supply only the endpoint and (optionally) a
model name and bespoke criteria.

The adapter is intentionally narrow: it owns the Granite prompt
template and parses the `<score>yes|no</score>` verdict head, but
delegates auth, retries, and fail-open policy to the caller — whether
to allow content through when the classifier is slow or unreachable
is a loop-side policy decision, not something baked into adapter
code. Wire format is a single non-streaming POST to
`{endpoint}/v1/chat/completions` (or just `{endpoint}` when the
endpoint already pins the path). vLLM is usually unauthenticated;
adding auth would need an API-key field rather than smuggling
credentials through the endpoint URL.

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

**Criterion polarity convention.** The classifier's scoring schema
returns `yes` when the response *meets* the stated criterion, and the
adapter maps `yes` to `VerdictDeny`. Every criterion — built-in or
supplied via `customCriteria` — must therefore be phrased as a
**risk**: meeting the criterion has to mean the content is
problematic, not that it satisfies a requirement. For example, phrase
groundedness as "the response contains unsupported claims" rather
than "claims must be supported" — the latter is met by a good answer,
so the classifier says `yes` and the adapter denies it.
`builtInCriteria` in `harness/internal/guard/graniteguardian.go` is
the reference for this phrasing.

### `shieldstral`

[Shieldstral](https://huggingface.co/mistralai/Shieldstral-1.0-3B)
is an open-weights safety classifier fine-tuned by Mistral AI for
conversational and agentic safety intervention points. It is
self-hosted behind an OpenAI-compatible chat-completions API, the
same shape as `granite-guardian`.

**Model id.** `model` is required; there is no default, because each
runtime names the model differently. Set the id the server exposes:
`mistralai/Shieldstral-1.0-3B` on vLLM, while LM Studio lists its own
ids (for example `shieldstral-1.0-3b-mlx`). A mismatched id is
rejected by the server (LM Studio answers HTTP 400 `Invalid model
identifier`).

**Tested runtimes.** The adapter has been live-tested only against
LM Studio serving MLX conversions of `mistralai/Shieldstral-1.0-3B`.
vLLM, Ollama, and TGI are expected to work through the
OpenAI-compatible chat-completions API but are untested.

**Phases.** A `shieldstral` guard with no `phases` checks `pre_turn`
and `post_turn` only; `pre_tool` is skipped without a classifier
call. The `pre_tool` check receives only the tool's JSON arguments,
and the built-in `function_call` criterion wrongly denied harmless
read and search calls (such as `{"path":"README.md"}`) in testing
against the 3B MLX builds: 2 of 8 harmless calls on the 4-bit build
and 3 of 8 on the MXFP8 build, while all 7 malicious calls were
denied on both. With the fail-closed default, each false positive
blocks the tool call. Listing `pre_tool` in `phases` opts in, for
example `"phases": ["pre_turn", "pre_tool", "post_turn"]`. The
default applies to `shieldstral` stages inside a `composite` as well.

**Availability.** Shieldstral is not available on Mistral AI's hosted
API (`api.mistral.ai`) or on OpenRouter; both were checked live and
neither serves a `shieldstral*` model. The `endpoint` / `apiKeyRef`
fields exist for operators pointing at their own authenticated
deployment (a private inference endpoint, a proxy in front of a
self-hosted instance).

**LM Studio chat template.** Some LM Studio builds of the model ship
a `chat_template.jinja` that calls its `render_content` macro with
keyword arguments, which LM Studio's template engine rejects: every
`/v1/chat/completions` call returns HTTP 400 with
`Missing positional argument: content`. Rewriting those calls to pass
the arguments positionally fixes it.

Authentication is supported via `apiKeyRef` (resolving a
`secret://...` reference via `security.SecretStore`) for endpoints
that require it. For unauthenticated local instances, `apiKeyRef`
can be omitted.

The `endpoint` is either a bare host (`/v1/chat/completions` is
appended automatically) or a full chat-completions URL. A
path-bearing endpoint is used as-is.

Minimal config:

```json
{
  "guardRail": {
    "type": "shieldstral",
    "endpoint": "http://127.0.0.1:1234",
    "model": "mistralai/Shieldstral-1.0-3B"
  }
}
```

Or via flags:

```sh
stirrup harness \
  --prompt "..." \
  --guardrail shieldstral \
  --guardrail-endpoint http://127.0.0.1:1234 \
  --guardrail-model mistralai/Shieldstral-1.0-3B
```

### `cloud-judge`

Reuses an existing `ProviderAdapter` (Anthropic, OpenAI-compatible)
with a cheap default model, `claude-haiku-5-5`. No new SDK
dependency: it streams a single completion at temperature 0 and low
reasoning effort, asks the provider to constrain the response to the
verdict object as a structured output, and extracts the verdict from
the response text. Each setting reaches the wire only where the model's
quirks allow it. Claude Haiku 5.5 rejects a non-default temperature, so
the Anthropic adapter drops it and keeps the effort setting. A model
with no advertised structured-output control receives the request
without the schema. Use this when the deployment cannot run its own
GPU-attached vLLM.

`cloud-judge` is an escalation-grade classifier, not a fast path:
every check is a full round trip to a hosted model, on the order of
a second or more. At `pre_tool` that cost is paid once per tool call.

**Prompt.** The prompt keeps the Granite Guardian structure (criteria,
then scoring schema) and the same per-phase default criteria, so
switching adapters does not change policy. The content under
classification is fenced as untrusted data:

- It sits between an open and a close marker that carry a random
  128-bit nonce drawn for each call, after the content already
  exists, so the content cannot predict the close marker.
- Every run of three `<` characters in the content gets a space
  inserted (`<<<` becomes `<< <`), so the content cannot contain or
  imitate either marker, even with a guessed nonce.
- The prompt states, ahead of the fence, that the fenced text is
  data to evaluate and never instructions; the system prompt states
  the same rule.

At `pre_tool` the prompt also names the tool being called (the
resolved internal name) and the call source, both quoted so a name
containing newlines or quotes cannot add prompt structure. A phase
the adapter does not recognise is classified under the `post_turn`
default criteria.

**Structured output.** Every check sets `StreamParams.OutputSchema`
to the verdict object as a JSON Schema: `nonce`, then `reasoning`,
then `verdict` as an enum of `allow` and `deny`, every member required
and no other member allowed. Where the model's quirks advertise the
capability (see [provider-quirks.md § 3.4](provider-quirks.md#34-structured-outputs)),
the adapter projects it onto the provider's native field, and the
provider constrains decoding to the schema, with documented exceptions:
a refusal, output cut at the token cap, and, on Anthropic, enum values
that differ only in capitalisation. Elsewhere the request goes out
without the schema, and the prompt instruction below is the only
contract. The text parse and the stop-reason check described next run
in every case. Anthropic and both OpenAI wires have rules; Bedrock has
none (the Converse API exposes the control, the adapter does not send
it), and no Gemini model has one because the Vertex AI field is
unprobed, so judges on those providers are always prompt-only.

The decision records which request was made as `structuredOutput`:
`json_schema` when the request carried the schema and the built-in
registry advertises the control for the judge model, `prompt_only`
otherwise. The value describes what was requested and advertised, not
what the provider did with it: a gateway can drop the field without
changing the record, and a `compatProfile` rule set is not consulted
for it. It is recorded on the `guard.structured_output` span attribute
for decisions and for failed checks alike, and on the fail-open
decision, so parse failures can be split by it. In a sequential
composite the attribute reflects the decisive stage, so a cloud-judge
allow followed by another stage's allow carries no value.

If the provider rejects the schema-bearing request with a 4xx other
than 429, the check is sent once more without the schema and recorded
as `prompt_only`; a provider that silently ignores the field is
likewise covered by the text parse. A rule that is wrong for a
deployment therefore costs one extra round trip per check, not the
guard. The symptom is a `guard_error` naming a provider stream error
on every check, or repeated denies under the default `failOpen: false`.
`stirrup providers quirks --provider X --model Y` prints the resolved
`outputSchema.supported` and the contributing rules, and the adapter's
`outputSchema ignored` debug line shows when a schema was dropped
before send. The only lever is the judge model: one without a rule is
prompt-only from the start.

A schema property named `reasoning` can draw a `refusal` stop from a
model that treats it as a request for hidden thinking; the prompt asks
for one sentence to keep it an explanation. A refusal is not `end_turn`,
so it is an incomplete-stream error and takes the guard-error path.

**Verdict extraction.** The verdict must carry the call's fence
nonce. The instruction after the fence asks for
`{"nonce": "<nonce>", "reasoning": "<one sentence>", "verdict": "allow"|"deny"}`
and states the nonce value; the content was written before the nonce
existed, so a verdict planted in it cannot carry the right one. The
adapter scans the response for top-level JSON objects, reads members
by exact key, and accepts only an object whose `nonce` member equals
the call's nonce. The verdict is compared case-insensitively, and a
reasoning member keyed `reason` is read as the reasoning:

- Objects without the nonce, or with a different one, are ignored
  wherever they appear, so a planted verdict the model echoes before
  or after its own answer never decides the call.
- Braces inside JSON strings are text, so a reasoning string that quotes code
  parses, and a verdict wrapped in prose or a markdown fence is
  found.
- A balanced brace pair that is not valid JSON, such as a reasoning
  string quoting a planted object with unescaped quotes, is skipped
  whole: nothing nested inside it is a candidate. An unterminated
  `{` is skipped one byte at a time, so a stray brace in prose does
  not hide a later verdict.
- Two objects that carry the nonce but differ are a conflict;
  identical copies count as one.

Only a stream that closes before the deadline with a normal stop
reason (`end_turn`, or `stop_sequence`) is parsed. A stream cut by
the deadline, stopped at the token cap (`max_tokens`), blocked by
the provider, or closed without a stop reason is an error, whatever
text arrived before it ended.

A response with no matching object, conflicting objects, mistyped
members, or a verdict other than `allow` or `deny` is an error. With
the default `failOpen: false` an error denies. With `failOpen: true`
every one of these errors allows the call, so the parsing rules
constrain nothing in a run that sets it: content that makes the
classifier answer ambiguously gets through. The nonce also does not
detect a classifier that the content persuades to answer `allow` with
the right nonce; the fence and notice make that less likely but do
not rule it out.

**Content size.** At `pre_tool` the classified content is capped at
64 KiB. A larger tool input is cut back to a UTF-8 boundary, and the
prompt states after the fence how many of the input's bytes are
shown. Only that prefix is classified: anything past the cap never
reaches the classifier. The deterministic tripwires (step 4 of
[§ Dispatch order](#dispatch-order)) still scan the full raw input.
`pre_turn` and `post_turn` content is not capped by the adapter.

**Timeouts.** With `timeoutMs` unset, the per-call deadline is 4 s at
`pre_tool` and 5 s at `pre_turn` and `post_turn`. Setting
`timeoutMs` overrides every phase. The deadline bounds the whole
response stream, not just the time to its first token.

- Under the default `failOpen: false`, a check that runs past the
  deadline denies the call, and repeated denies trip the stall
  detector, so provider slowness becomes failed tool calls. The 4 s
  `pre_tool` default leaves headroom over Claude Haiku 5.5, which
  takes 1 to 3 s per check at low effort without a schema. Providers
  compile a new schema into a grammar on its first use and cache it
  for a period (Anthropic documents a 24 h cache), so the first check
  after a cold cache pays extra latency; its size on `claude-haiku-5-5`
  is unmeasured. Deployments on a slower model or provider, or that see
  a first-check timeout, set `timeoutMs` explicitly.
- Under `failOpen: true`, a timeout allows the call. The content
  under classification influences how long the classifier takes
  (its length, and what it asks the classifier to write), so a tight
  deadline combined with `failOpen: true` lets content that slows the
  classifier turn into an allow.

### `composite` (operator escape hatch)

Layers multiple stages, optionally restricted to specific phases.
The harness only ships `granite-guardian`, `shieldstral`, and
`cloud-judge`, but the composite primitive lets operators add their
own classifiers
(e.g. a fast-path Llama Prompt Guard 2 served via TEI in front of
the slower Granite Guardian) without modifying the harness. See
[§ Operator escape hatch](#operator-escape-hatch) below.

There is **no fast-path adapter shipped in the harness**. If you
need sub-100ms guard latency, write a custom adapter and compose
it via `phase-gated` in front of `granite-guardian`.

Config-driven composites (`GuardRailConfig`) always wire a sequential
chain — deny short-circuits, and a non-deny result is the last guard's
decision. The harness also exports a `Parallel` composite (deny beats
spotlight beats allow, aggregated across every guard) but it has no
config-file equivalent yet, and `harnessapi` does not expose guard
types, so it is constructed only in Go inside the harness module.

## Latency budget

The figures below were measured against a stub vLLM endpoint that
responds instantly, so they capture the harness's own per-call
overhead, not classifier inference. Real numbers depend on classifier
throughput and prompt length; treat these as advisory.

| Phase | p50 | p99 | Notes |
|---|---|---|---|
| `pre_turn`  | < 50 ms | < 600 ms | Single batched call per turn (all untrusted chunks). Skipped entirely for chunks shorter than `MinChunkChars` (default 256 for `granite-guardian`; `shieldstral` skips only when the threshold is set). |
| `pre_tool`  | < 30 ms | < 200 ms | One call per tool invocation, after the deterministic checks. Adds up over many tool calls per turn. |
| `post_turn` | < 50 ms | < 600 ms | Single composite-criterion call per turn end. |

None of these figures applies to `cloud-judge`. It is an
escalation-grade classifier whose calls take seconds, not
milliseconds, because each is a hosted-model round trip; see
[§ `cloud-judge`](#cloud-judge).

On a transport error or timeout, the error path costs one adapter
timeout: `timeoutMs` when set, otherwise the adapter default
(`granite-guardian` 10 s; `cloud-judge` 4 s at `pre_tool` and 5 s
elsewhere). With `failOpen: true` the request then proceeds. With
`failOpen: false` (the default) it produces a `Deny` and the
offending content does not pass.

The two load-bearing latency mitigations are:

1. **`MinChunkChars` skip** at `pre_turn`. Chunks shorter than the
   threshold (default 256 chars for `granite-guardian`; zero, which
   disables the skip, for `shieldstral`) are not sent to the
   classifier; a `guard_skipped` event is emitted instead. Mid-run
   `user_response` input is exempt: it is classified at any length,
   since it is one call per turn boundary at most and a short relayed
   turn is exactly the case that needs screening.
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
reuses the Anthropic adapter with Claude Haiku 5.5 as the classifier model
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
`phases` means the stage runs at every phase, except that a
`shieldstral` stage then skips `pre_tool` (see
[`shieldstral`](#shieldstral)). Stages run
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
`guard.pre_turn`, `guard.pre_tool`, `guard.post_turn`). `pre_turn` and
`post_turn` spans are children of the run's `invoke_agent` span;
`pre_tool` spans are children of the call's `execute_tool` span.
Standard attributes: `guard.id`, `guard.criterion`, `guard.score`,
`guard.verdict`, `guard.latency_ms`, and, when `cloud-judge` is the
decisive guard, `guard.structured_output` (`json_schema` or
`prompt_only`, on decisions and on failed checks alike).

### OTel metrics

Five instruments emitted by `harness/internal/observability/metrics.go`:

| Metric | Type | Attributes |
|---|---|---|
| `stirrup.guard.checks` | counter | `guard.id`, `guard.phase`, `guard.verdict` |
| `stirrup.guard.errors` | counter | `guard.id`, `guard.phase` |
| `stirrup.guard.skips` | counter | `guard.id`, `guard.phase`, `reason` |
| `stirrup.guard.spotlights` | counter | `guard.id` (`harness:scrubbed-spans` for the harness's own fence), `guard.phase` |
| `stirrup.guard.duration` | histogram | `guard.id`, `guard.phase` |

A healthy production fleet sees `errors` and `skips` at low rates
relative to `checks`. A sudden rise in `errors` is the operational
signal for a degraded classifier.

### Security events

Five event types on the `SecurityEventEmitter`:

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

One place a guardrail *feeds* a ring is the Rule-of-Two
criterion ratchet. When a guard `Decision.Criterion` matches
`ruleOfTwo.runtime.guardCriteria` (default `["sensitive_data", "pii"]`),
it trips Ring 4's runtime sensitive-data latch — one-way, false→true
only. A guard can therefore *tighten* the Rule of Two mid-run (revoking
egress once it judges sensitive content has entered the conversation)
but can never loosen it, which keeps the LLM's involvement fail-safe.
See [the runtime
classifier](safety-rings.md#the-runtime-classifier) for the latch, the
arming matrix, and the on-detect actions.

## References

- IBM, *Granite Guardian 4.1-8B* model card —
  <https://huggingface.co/ibm-granite/granite-guardian-4.1-8b>.
- Hines et al., *Defending Against Indirect Prompt Injection
  Attacks With Spotlighting* — arXiv:2403.14720.
- Issue [#43](https://github.com/rxbynerd/stirrup/issues/43) for
  the original design rationale and chunk-by-chunk implementation
  history.
