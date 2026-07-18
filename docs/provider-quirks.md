# Provider quirks layer

**Status:** implemented (Wave 2)

The quirks layer encapsulates per-(provider, model) wire-shape and
behaviour divergences that the canonical `ProviderAdapter` interface
cannot express. Adapters resolve a `ProviderQuirks` value at the top
of every `Stream()` call and apply it to request marshalling and
response parsing. The registry is a first-party Go construct;
operators do not author quirk rules, but can introspect resolution
via `stirrup providers quirks --provider X --model Y`.

This document is the canonical specification of the layer. The
design history (the problem framing, the prior-art survey, the open
risks) lives in §1 and §9 below.

## 1. Problem

A coding-agent harness has to talk to several model providers, and
several model families within each provider. Each surface diverges
in small ways that the canonical `ProviderAdapter` interface cannot
absorb without either:

- carrying a knob per divergence on the interface (bloat the surface),
- branching on model substrings inside adapters (drift across adapters), or
- letting the operator patch the request shape (RunConfig becomes
  wire-shape).

Concrete v1 divergences:

- OpenAI's reasoning-class models (o-series, gpt-5 except `gpt-5-chat*`)
  reject `temperature` / `top_p` / `presence_penalty` /
  `frequency_penalty` / `logprobs` / `top_logprobs` / `logit_bias`
  with HTTP 400. They require `max_completion_tokens` rather than
  `max_tokens`.
- Z.ai's GLM endpoint speaks the OpenAI Chat Completions wire format
  but requires `max_tokens` (the legacy key) and accepts a
  proprietary `tool_stream: true` extension. The GLM-4.5-and-above
  thinking family (`glm-4.5`/`4.6`/`4.7` and the `glm-5` line)
  additionally emits `reasoning_content` on the assistant delta and
  accepts a top-level `thinking: {"type":"enabled"}` object; the
  reasoning is replayed back outbound (same `ReplayFields` threading
  as DeepSeek). The legacy hyphenated line (`glm-4-plus` etc.) has no
  thinking mode and receives only the base quirks.
- Vertex AI's Gemini 3.x stream emits a `thoughtSignature` blob on
  every `parts[]` entry that the harness must preserve verbatim for
  multi-turn reasoning continuity. Issue #194 traced the symptom; the
  fix is a registry-driven `ReplayFields` capture.
- DeepSeek's reasoner and v4 families surface chain-of-thought
  through a `reasoning_content` sibling field on the assistant
  delta. DeepSeek v4 thinking mode (default-on) additionally
  *requires* the field replayed on every request after a tool-call
  turn — the API returns 400 otherwise — so the `ReplayFields`
  capture is threaded back outbound on the openai-compatible
  adapter (see [§3.1](#31-replayfields-rules)).

### 1.1 Prior art

LiteLLM, LangChain, the Vercel AI SDK, OpenRouter, and vLLM all
solve variants of the same problem with a registry of per-model
rules. The cross-cutting findings their implementations share:

- **One source of truth for send and parse.** Rules that govern
  request shape and response parsing must be in the same data
  structure; splitting them produces silent drift when one side is
  updated and the other is not.
- **Forward-compatible defaults.** A model id that matches no rule
  must produce a working request — the zero-rule path is the
  baseline, not an error.

The quirks layer adopts both invariants. The Codec invariant (one
resolved `ProviderQuirks` drives both send and parse paths) is the
load-bearing structural property; the empty-registry path produces
the same wire body as today's hand-rolled adapters.

## 2. Naming and shape

The layer lives at `harness/internal/provider/quirks/`:

```
harness/internal/provider/quirks/
    quirks.go       — ProviderQuirks, Value, ProviderBehaviourFlags,
                      OpenAIBehaviourFlags, GeminiBehaviourFlags,
                      OpenAITokenField, GeminiStreamArgsShape types
    registry.go     — Rule, Registry, Resolve, Date helper
    builtin.go      — BuiltinRules() — the first-party rule set
    helpers.go      — applyOpenAIReasoningClass, removeFromOmit,
                      shared helper fns for rules
    replay.go       — ReplayFields path parser + walker
    quirks_test.go  — Resolve, TestBuiltinRulesValidate, TestRuleCarveOuts,
                      TestNoMetacharsInKnownModelIDs, TestRuleStaleness
    replay_test.go  — ReplayFields path parser + walker tests
    testdata/
        model-ids.txt — catalogue for TestNoMetacharsInKnownModelIDs
```

Compatibility profiles (operator-selected via
`provider.compatProfile`) live in a parallel tree:

```
harness/internal/provider/compat/
    zai/
        zai.go      — CompatRules() returning the Z.ai GLM rule set
        zai_test.go — httptest contract test
```

Fixture trees for the wire-shape contract tests:

```
harness/internal/provider/testdata/quirks/
    openai-compatible/<model>/{request.json, response.sse}
    openai-compatible/<deepseek-model>/{request.json, response.sse, replay.json}
    gemini/<model>/{request.json, response.sse[, replay.json]}
```

A gateway-prefixed model id (e.g. `deepseek/deepseek-v4-flash`) maps
onto a nested fixture directory; the `<model>` path component is the
id verbatim.

### 2.1 ProviderQuirks

`ProviderQuirks` is the in-memory result of resolving the registry
for a (providerType, model) pair. Adapters read it when building a
request and (for paths that diverge) when interpreting a response.

```go
type ProviderQuirks struct {
    // Wire-shape overrides.
    FieldRenames    map[string]string         // canonical adapter field → wire JSON key
    OmitFields      []string                  // canonical fields the adapter MUST NOT emit
    ValueOverrides  map[string]Value          // forced serialised value, ignoring StreamParams
    EnumCoercions   map[string]map[string]string // caller value → wire value
    ReplayFields    []string                  // assistant-message paths to preserve across turns

    // Cross-provider capabilities (top-level, not per-adapter).
    ToolChoice            ToolChoiceCapability           // native tool_choice support (auto/required/none/named)
    StructuredToolResults StructuredToolResultCapability // accepts a non-string tool-result payload, and in which shape
    ParallelToolCalls     ParallelToolCallsCapability    // native parallel-tool-call control (#222)
    ToolExamples          ToolExamplesCapability         // accepts the JSON-Schema `examples` keyword in a tool's parameters (#222)

    // Behaviour flags (per-adapter typed sub-structs).
    BehaviourFlags  ProviderBehaviourFlags
}
```

A **capability** is a top-level field rather than a per-adapter
behaviour flag when it expresses a concept the loop reasons about
uniformly across families even though each provider encodes it
differently. `ToolChoice`, `StructuredToolResults`,
`ParallelToolCalls`, and `ToolExamples` are the four: every family
has *some* form of (or lack of) each, so modelling any of them under
one provider's sub-struct would force the other adapters to reach
across family boundaries to read it, which the behaviour-flag
ownership rule forbids. Each capability's zero value advertises no
support, so an adapter resolving a provider with no rule emits the
pre-capability wire shape — the graceful default the
`StreamParams`/`ToolResult` contracts require.

The two `#222` capabilities gate the tool-reliability controls the
model-facing contract added on top of tool-choice and strict mode:

| Control | Source | OpenAI Chat | OpenAI Responses | Anthropic | Gemini | Bedrock |
|---|---|---|---|---|---|---|
| Parallel-tool-call policy | `StreamParams.ParallelToolCalls` | `parallel_tool_calls` | `parallel_tool_calls` | `tool_choice.disable_parallel_tool_use` | — | — |
| Input examples | `ToolDefinition.Presentation.InputExamples` | schema `examples`¹ | schema `examples`¹ | schema `examples` | —² | — |
| Tool annotations | `ToolDefinition.Presentation.Annotations` | —³ | —³ | —³ | —³ | —³ |

¹ Folded only on non-strict tools: OpenAI's structured-outputs
subset rejects the `examples` keyword, so strict tools rely on the
description text (which carries the same example).
² Gemini's function-declaration Schema dialect rejects `examples`, so
the capability stays off and the description text is the carrier.
³ No first-party provider has a tool-annotation wire field; annotations
are carried for internal use and round-tripped from MCP servers (see
[#222](architecture.md)), and are a deliberate no-op on every adapter.

`StructuredToolResults` (issue #231) gates whether the structured
tool-result envelope is serialised onto the wire. The first-party
rules opt in Anthropic (content-block array form) and Gemini
(`functionResponse.response` object); OpenAI and any unruled provider
stay text-only. See the
[structured tool results](architecture.md#structured-tool-results)
section of the architecture doc for the per-provider wire shapes.

```go
type ProviderBehaviourFlags struct {
    OpenAI          OpenAIBehaviourFlags
    Gemini          GeminiBehaviourFlags
    OpenAIResponses OpenAIResponsesBehaviourFlags
    Anthropic       AnthropicBehaviourFlags
}
```

`Registry.Resolve` always returns a `ProviderQuirks` with every map
and slice pre-initialised so Apply closures can read-modify-write
without nil guards.

Behaviour-flag sub-structs:

```go
type OpenAIBehaviourFlags struct {
    TokenField         OpenAITokenField  // max_completion_tokens (default) or max_tokens
    OmitSamplingParams bool              // suppress temperature, top_p, penalties, log* fields
    ExtraBodyFields    map[string]any    // gateway-specific top-level keys (Z.ai's tool_stream)
}

type GeminiBehaviourFlags struct {
    StreamFunctionCallArgsShape GeminiStreamArgsShape // off (post-#191 default) / v2_snapshot / v3_deltas
}

type OpenAIResponsesBehaviourFlags struct {
    TokenField     OpenAIResponsesTokenField // max_output_tokens (default; distinct from Chat's keys)
    StoreMode      OpenAIResponsesStoreMode  // store_false (default): always emit explicit store:false
    InputItemShape OpenAIResponsesInputShape // typed_input_items (default): #172 + #199 discriminated union
}

type AnthropicBehaviourFlags struct {
    OmitSamplingParams bool // suppress temperature (400 on non-default value for the newest Claude tier)
}
```

`AnthropicBehaviourFlags` mirrors `OpenAIBehaviourFlags.OmitSamplingParams`
for the one Anthropic wire divergence the harness has needed so far: Claude
Opus 4.7+, Claude Sonnet 5, and Claude Fable 5 / Mythos 5 return an HTTP 400
on a non-default `temperature` rather than ignoring it, and the harness
loop unconditionally resolves a non-nil default temperature
(`core.defaultTemperature = 0.1`) for every provider call when
`RunConfig.Temperature` is unset — so without this flag every request to
one of those models 400s on its first turn. `buildAnthropicRequest` forces
`anthropicRequest.Temperature` to `nil` when the flag is set, relying on
the existing `omitempty` tag rather than a custom `MarshalJSON` (unlike
`openaiRequest`, which needs one for unrelated token-field-selection
reasons). `StreamParams` carries no `top_p`/`top_k` fields today; the flag
will cover them too if those are added later.

The typed per-adapter sub-struct is the design choice that replaces
PR #196's original `StructuralFlags any`: it preserves compile-time
type safety and removes the runtime type-assertion every adapter
would otherwise need.

`OpenAIResponsesBehaviourFlags` carries the wire divergences the OpenAI
Responses API (`POST /v1/responses`) has over Chat Completions that the
`OpenAIBehaviourFlags` sub-struct cannot express (#332). It is owned by
the Responses adapter; the Chat adapter never reads it. The zero value
of every field reproduces the adapter's pre-quirks hard-coded shape, so
a Responses request resolved with no rule is byte-identical:

- `TokenField` selects the token-budget key. The Responses API uses
  `max_output_tokens` — a distinct key from Chat Completions'
  `max_completion_tokens` / `max_tokens`, which is why it is a separate
  enum rather than a shared `OpenAITokenField`.
- `StoreMode` controls the top-level `store` field, always emitted
  explicitly. `store_false` (the default) sends `"store":false` because
  the harness manages its own conversation history and never opts into
  server-side state — leaving the key unset would default to
  persistence on some endpoints (a privacy concern for self-hosted
  gateways, a billing concern for long-running runs).
- `InputItemShape` selects how conversation history is serialised into
  the `input` array. `typed_input_items` (the default) emits the
  per-variant discriminated-union shape — the structural fix for #199
  (stricter validators reject `"output":""` on message / function_call
  items) that preserves the #172 invariant (a `function_call_output`
  item always carries the `output` key, even when empty). No alternative
  shape ships in v1; the flag exists so the resolved quirks struct is
  the single source of truth for the input-item decision and a future
  divergent gateway shape branches in the adapter's `MarshalJSON` rather
  than re-shaping the adapter.

Like the Chat Completions `openaiRequest`, the Responses adapter's
`responsesRequest` carries the resolved flags as steering fields and a
`MarshalJSON` that projects the wire body they select — the Codec
invariant (one resolved quirks struct drives both the send and parse
paths) now holds for Responses as it does for Chat, Anthropic, and
Gemini.

### 2.2 Rule and Registry

```go
type Rule struct {
    ProviderType string        // exact RunConfig provider.type
    ModelMatch   string        // path.Match glob; "" matches all models
    Description  string        // required; surfaces in traces and CLI introspection
    LastVerified time.Time     // set via Date("YYYY-MM-DD"); staleness signal at 180 days
    Apply        func(*ProviderQuirks)
}

type Registry struct { /* unexported */ }
func NewRegistry(rules []Rule) *Registry
func DefaultRegistry() *Registry
func (r *Registry) Resolve(providerType, model string) ProviderQuirks
func (r *Registry) ResolveWithRules(providerType, model string) (ProviderQuirks, []Rule)
```

Composition is **specificity-then-declaration-order**: rules with a
longer `ModelMatch` glob run later so their writes override earlier
rules. Ties (identical glob length) break on declaration order. An
empty `ModelMatch` is treated as length 0 and matches every model,
so a "default for this provider" rule can be declared once and
overridden by a specific glob. The `gpt-5*` + `gpt-5-chat*` carve-out
in `BuiltinRules()` is the worked example.

`DefaultRegistry()` is constructed once via `sync.Once` and is
read-only after construction. Tests inject custom registries via
the adapter's `Registry *quirks.Registry` field (the field is
exported on `OpenAICompatibleAdapter` and `GeminiAdapter`).

### 2.3 ReplayFields path syntax

`ReplayFields` lists assistant-message field paths to capture from a
provider response. The path grammar is intentionally narrow:

```
path     := segment ( "." segment )*
segment  := key ( "[]" )?
key      := [A-Za-z_][A-Za-z0-9_]*
```

`[]` always means "iterate every element" of an array. A path that
ends in `[]` is a syntax error (it would name a value rather than a
field). Indexed access (`[0]`) is intentionally NOT supported in v1:
a rule that pins a single index is almost certainly modelling
provider behaviour wrong.

The parser sits in `quirks/replay.go` so both adapters share one
implementation. `BuiltinRulesValidate` and the
`TestBuiltinRulesValidate_ReplayFieldsPathsAreSyntacticallyValid`
test catch malformed paths at registry-build time.

## 3. Wave 2 rules (`BuiltinRules`)

| ProviderType        | ModelMatch         | Description                                                          |
|---------------------|--------------------|----------------------------------------------------------------------|
| `openai-compatible` | `o[1-9]*`          | OpenAI reasoning-class (o-series): omit sampling params              |
| `openai-compatible` | `gpt-5*`           | OpenAI gpt-5 family: omit sampling params                            |
| `openai-compatible` | `gpt-5-chat*`      | OpenAI gpt-5-chat carve-out: chat-class accepts sampling params      |
| `openai-compatible` | `deepseek-reasoner*` | DeepSeek reasoner: replay `reasoning_content`, omit sampling params, legacy `max_tokens` (threaded) |
| `openai-compatible` | `deepseek-v4*`     | DeepSeek v4: replay `reasoning_content`, omit sampling params, legacy `max_tokens` (threaded) |
| `openai-compatible` | `deepseek/deepseek-v4*` | DeepSeek v4 via gateway prefix (OpenRouter-style ids): same quirk set as `deepseek-v4*` (threaded) |
| `gemini`            | `*`                | Gemini: off `streamFunctionCallArguments` (post-#191 default)        |
| `gemini`            | `gemini-3*`        | Gemini 3: preserve `thoughtSignature` as a sibling of `functionCall` on each `parts[]` element (parse-side only) |
| `openai-responses`  | `*`                | OpenAI Responses: typed input items, `max_output_tokens`, `store:false`; top-level `parallel_tool_calls`; accepts schema examples (#222, #332) |
| `anthropic`         | `claude-opus-4-7*` | Anthropic Claude Opus 4.7: omit sampling params (400 on non-default temperature/top_p/top_k) |
| `anthropic`         | `claude-opus-4-8*` | Anthropic Claude Opus 4.8: omit sampling params (400 on non-default temperature/top_p/top_k) |
| `anthropic`         | `claude-sonnet-5*` | Anthropic Claude Sonnet 5: omit sampling params (400 on non-default temperature/top_p/top_k) |
| `anthropic`         | `claude-fable-5*`  | Anthropic Claude Fable 5: omit sampling params (400 on non-default temperature/top_p/top_k) |
| `anthropic`         | `claude-mythos-5*` | Anthropic Claude Mythos 5: omit sampling params (same API surface as Fable 5; 400 on non-default temperature/top_p/top_k) |

`claude-opus-4-6*`, `claude-sonnet-4-6*`, and `claude-haiku-4-5*` are
deliberately unmatched — those models still accept a non-default
temperature. `claude-mythos-preview` (the Mythos 5 predecessor) is also
unmatched: its sampling-param behaviour is not confirmed against a live
capture, so a rule is added once verified rather than assumed from the
Fable 5 family resemblance.

The `gpt-4o-mini*` strict-mode rule is deliberately narrower than
`gpt-4o*`: OpenAI's structured-outputs guide lists bare `gpt-4o` as
supporting strict mode too, but that has not been verified against a
current snapshot, and a wider glob risks a 400 on a deployment whose
`gpt-4o` snapshot diverges from the guide. `TestBuiltinRulesStrictMode`
pins the negative case (bare `gpt-4o` gets no rule) so widening the
glob is a deliberate, tested edit rather than an accidental regression.

The `gemini-3*` schema-lint rule (`SchemaUnsupportedFeatures: pattern,
format`) takes a conservative reject-at-build-time position: Gemini's
function-declaration Schema dialect does not reliably honour the
JSON-Schema `pattern` and `format` keywords across the Gemini 3.x
rollout — some surfaces silently ignore them, others reject the
request outright — so the harness rejects a tool schema that uses
either keyword rather than risk the wire transform silently dropping
a validation rule. The built-in tool schemas do not use either
keyword, so the rule only catches operator-supplied or MCP-imported
schemas.

The `openai-responses / *` rule pins the Responses-specific behaviour
flags (`OpenAIResponsesBehaviourFlags`) to their zero values so the
resolved struct, not the adapter, is the source of truth for the
Responses send path; the pinned values reproduce the adapter's prior
hard-coded shape byte-for-byte. See [§2.1](#21-providerquirks).

The compat profile rules for Z.ai GLM are registered separately via
`harness/internal/provider/compat/zai/CompatRules()`; they are not in
`BuiltinRules()` and only load when `provider.compatProfile =
"zai-glm"` is set. See [§6](#6-zai-compat-placement) for the rule set.

### 3.1 ReplayFields rules

`ReplayFields` rules are no longer uniformly parse-side. Every
adapter captures the named paths from its decoded stream into a
per-stream accumulator; what happens next splits into two classes,
recorded by a mandatory Description suffix:

- **`(threaded)`** — the openai-compatible adapter additionally
  round-trips the capture. When the stream completes, the
  accumulator is flattened onto the `message_complete` event's
  `ReplayFields` (per path: if every captured value is a string,
  the pieces are concatenated in arrival order and marshalled as
  one JSON string — the streamed-string case, e.g.
  `reasoning_content` arriving across many chunks; otherwise the
  last captured value is marshalled verbatim, snapshot semantics).
  The agentic loop attaches the map to the assistant `Message` it
  persists, and on subsequent requests the adapter emits each
  qualifying entry as a top-level key on the assistant wire
  message. Qualifying means: the path is named by the quirks
  resolved for *that* stream (the registry stays the single source
  of truth — state captured under another model's rules is never
  replayed), the path is single-segment (no `.`, no `[]`), and it
  does not collide with a canonical message key (`role`, `content`,
  `tool_calls`, `tool_call_id`, `name`). Non-qualifying paths are
  silently skipped at runtime; first-party rules that declare one
  fail the registry's build-time test instead.
- **`(parse-side only)`** — the capture surfaces in the length-only
  observability summaries below but is not round-tripped via this
  surface. The Gemini 3 `thoughtSignature` rule stays in this
  class: Gemini's real round-trip is the typed block-level
  `ContentBlock.ThoughtSignature`, and the ReplayFields capture is
  corroborating observability.

`TestBuiltinRulesReplayFieldsSuffix` enforces the convention: a
ReplayFields rule's Description must end in exactly one of the two
markers, openai-compatible rules must be `(threaded)` with
threadable paths, and other providers must be `(parse-side only)`.

Known limitation: the gRPC `BatchAdapter` shares the
openai-compatible request builder, so the outbound half rides along
for batch submissions — but its result-parse path
(`fabricateStream`) has no ReplayFields capture, so nothing is
accumulated to replay. Multi-turn tool-calling against DeepSeek v4
thinking mode through the batch path therefore still fails with 400
on the turn after a tool call; see §9 risk 7.

A related gap exists in the eval `ReplayProvider`
(`provider/replay.go`), which replays recorded `TurnRecord`s as
streaming model responses for eval testing without live API calls.
`TurnRecord` does not carry the message-level ReplayFields state —
the trace scrubber deliberately drops it as provider-opaque — so a
live-continuation run seeded from a recording (e.g. `mineFailureTasks`)
against DeepSeek v4 thinking mode will miss `reasoning_content` on the
replayed turn and 400 on the first real tool-call request. Closing it
needs a `TurnRecord` schema change (a recording-side carrier exempt
from the scrub drop); live continuation against v4 thinking mode is
not a supported workflow today. This contrasts with
`ContentBlock.ThoughtSignature`, which rides the typed block and is
forwarded by `ReplayProvider` today, so Gemini 3 live-continuation
seeded from a recording does carry the model's prior reasoning state
into the next Vertex request — pure eval replay never resubmits it
anywhere, so forwarding it adds no leakage surface.

The captured-fields debug log line is `quirks replay fields
captured` at slog DEBUG level. It records a per-path summary of
`{count, total_len}` only — captured values themselves are
provider-private (DeepSeek `reasoning_content`, Gemini
`thoughtSignature` — the latter is a sibling of `functionCall` on
each `parts[]` element, not a child; see `geminiPart` in
`harness/internal/provider/gemini_types.go`) and never appear in
log sinks. The same totals are mirrored onto the OTel span as
`replay_fields_captured.count` and `replay_fields_captured.total_len`,
so trace-only consumers see the rule fired without correlating
back to slog. `TestReplayFields_DeepSeekReasoner_LogIsLengthOnly`
pins the side-channel guard on the slog side.

## 4. Composition with the NormalizingAdapter

The `NormalizingAdapter` (PR #303, merged) wraps the concrete
adapter and sits **outside** the quirks layer in the call stack. The
quirks resolution is *inside* the concrete adapter's `Stream()`.
The factory order is:

```
loop.Provider
  └── NormalizingAdapter          ← outermost; tool-name normalization
        └── OpenAICompatibleAdapter
              └── quirks.Resolve  ← innermost; wire-shape/behaviour per model
```

Two separate responsibilities: tool-name normalization is a
loop→adapter concern; quirk resolution is an adapter→wire concern.
The factory composes both without either knowing about the other.

### 4.1 Strict-schema normalisation cache (`provider/strictschema_cache.go`)

`NormalizeStrictSchema` (the OpenAI structured-outputs rewrite —
`additionalProperties: false`, all properties required, optional
fields nullable-wrapped) is an expensive recursive walk, and the same
tool schema is re-sent on every turn of a run. `strictSchemaCache`
memoises the result within a single adapter instance; the factory
builds one adapter per run, so per-adapter scope matches "per-run"
caching: a tool's schema is stable within a run, and different runs
route through different adapter instances, so a cache entry from one
run cannot leak into another.

The cache key is `(model, tool-name, schema-bytes-hash)`. Including
`model` is load-bearing: a dynamic-router run can switch models turn
to turn, and a strict-mode rule may pin different models to different
strict-mode contracts (e.g. a future model with stricter
`additionalProperties` semantics). Hashing the raw schema bytes
protects against a runtime overwrite of a tool's canonical schema —
if the bytes change, the hash changes, and the stale rewrite is
bypassed.

Errors are NOT cached: a schema that fails the strict-mode lint
re-fails on every turn, so a rule change that introduces strict mode
mid-run does not paper over a transient parse problem, and an
operator sees the failure surface in logs each time. The miss path
takes a write lock and re-checks for the key after acquiring it, so
concurrent first-misses on the same key produce exactly one
normalisation call and one `Misses` increment — this gives the cache
singleflight semantics without a separate singleflight dependency.

## 5. Operator surface

| Surface | How | Notes |
|---|---|---|
| `provider.compatProfile` on `ProviderConfig` | `RunConfig` string field | Closed enum. Only legal value in v1: `"zai-glm"`. Unknown values fail at startup via `ValidateRunConfig`. |
| `stirrup providers quirks --provider X --model Y` | CLI subcommand | Prints resolved `ProviderQuirks` as JSON, plus `Description`, `LastVerified`, staleness flag of every contributing rule. Side-effect-free. |
| `openai quirks resolved` / `gemini quirks resolved` / `anthropic quirks resolved` slog DEBUG line (per-adapter prefix, identical body shape) | structured log | One line per Stream call, listing every contributing rule's Description in apply order. Last entry is the rule whose writes won on overlapping fields. Emitted even when no rule fired (rules:[]) so a missing line unambiguously means "the resolution did not run". Operators writing log filters should match the exact per-adapter prefix; a generic `"quirks resolved"` substring will not find every record. |
| `provider.quirk.applied` OTel span attribute | trace attribute | Set on the active `provider.stream` span at the same point the slog DEBUG line is emitted. Carries the same `[]string` of rule Descriptions. Lets a trace-only consumer (Datadog, Honeycomb, Jaeger) see which rules fired without needing to correlate with a separate log sink. |
| `quirks replay fields captured` slog DEBUG line | structured log | Per-stream summary of `{count, total_len}` per captured ReplayFields path, emitted on stream exit. Length-only — captured values themselves are not logged. |
| `replay_fields_captured.count` / `replay_fields_captured.total_len` OTel span attributes | trace attributes | Set on the active `provider.stream` span on stream exit, in parallel with the slog DEBUG record above. Totals across every captured path; length-only invariant matches the slog surface. |
| `openai quirks suppressed caller temperature` / `anthropic quirks suppressed caller temperature` slog WARN line | structured log | Fires when `OmitSamplingParams` suppresses a caller-supplied non-nil `Temperature`. Names the rule that caused the suppression. The suppressed value itself is not logged. |

### 5.1 Read-only registry

Operators cannot author arbitrary quirk rules. Quirks are first-party
Go code (`harness/internal/provider/quirks/builtin.go`), tested
exhaustively, and shared across every operator targeting the same
(provider, model) pair. The introspection subcommand makes the
registry transparent enough for debugging without needing authoring
capability.

A future `provider.quirkOverrides` emergency-override field is
sketched but remains deferred. If operational pressure justifies it,
the surface must be narrow (only `OmitFields` and `FieldRenames`,
not `BehaviourFlags`, `ValueOverrides`, or `ReplayFields`) and every
applied override must fire a `SecurityLogger` event.

## 6. Z.ai compat placement

Z.ai/GLM is a compatibility-only target. The rules live at
`harness/internal/provider/compat/zai/zai.go` and are exported by
`CompatRules()`, which returns a composing set rather than a single
rule. The factory injects the set into the registry when
`provider.compatProfile = "zai-glm"` is set; `resolveCompatProfile`
returns the whole slice and the injection site spreads it after
`BuiltinRules()`.

The set composes — `quirks.Registry.Resolve` runs every matching
rule's `Apply` in specificity-then-declaration order, so the more
specific thinking-family rules ADD to the base rule without re-setting
its fields:

| ModelMatch       | Quirks                                                       | Notes |
|------------------|--------------------------------------------------------------|-------|
| `glm-*`          | legacy `max_tokens`; `tool_stream: true`                     | all GLM, incl. the legacy hyphenated line (`glm-4-plus`) |
| `glm-4.[5-9]*`   | + replay `reasoning_content`; + `thinking: {"type":"enabled"}` | GLM-4.5/4.6/4.7 thinking family; the dot excludes the hyphenated legacy line |
| `glm-5*`         | same as `glm-4.[5-9]*`                                        | GLM-5/5.1 thinking family |
| `z-ai/glm-*`     | legacy `max_tokens`; + replay `reasoning_content`            | OpenRouter gateway-prefixed ids (`*` does not cross `/`); no `tool_stream`/`thinking` — vendor extras unverified through gateways |

`reasoning_content` threading (the `(threaded)` ReplayFields suffix in
[§3.1](#31-replayfields-rules)) now applies to the GLM-4.5+ thinking
family: the captured reasoning is round-tripped onto subsequent
openai-compatible requests, with zero adapter code — the rule only
appends the path to `ReplayFields`. Sampling params are NOT suppressed
for GLM (it accepts and recommends `temperature`/`top_p`), unlike the
DeepSeek and OpenAI reasoning-class rules.

`CompatProfile` does not violate the "no wire-shape on `RunConfig`"
invariant: it is a named profile selector from a closed enum (only
`""` and `"zai-glm"` are accepted; unknown values fail at startup),
not a free-form wire-shape patch. The wire-shape knowledge stays in
the `compat/` package; `RunConfig` carries only a string
discriminator. This is the same pattern as `provider.type` itself.

`RunConfig.Redact()` is unaffected — `CompatProfile` contains no
credentials.

Z.ai tests live entirely in
`harness/internal/provider/compat/zai/zai_test.go` and use
`httptest.Server` to capture the request body. No live Z.ai calls
in CI.

## 7. Wire-shape fixture format

Contract tests for the wire-shape rules use real captures (or
explicitly-marked synthetics) under:

```
harness/internal/provider/testdata/quirks/<provider-type>/<model>/
    request.json   — canonical JSON body the adapter produces;
                     stored in go-sorted-key form (unmarshal →
                     re-marshal). Updated whenever the rule's Apply
                     changes.
    response.sse   — real SSE response body captured from the
                     upstream, sanitised of auth headers and run
                     IDs. Synthetic equivalents carry a top-of-file
                     comment:
                       "# synthetic: derived from <source> because <reason>"
    replay.json    — present only for ReplayFields rules; the
                     post-parse assistant-message snapshot showing
                     the preserved field shape per captured path.
```

Capture discipline:

- Secrets (API keys, session tokens, project IDs in URLs, GCP `ya29.*`
  OAuth tokens) must be scrubbed before committing. The helper
  `quirkstest.ScrubFixture` enforces the scrub pass; the CI gate
  `TestFixturesScrubbed` fails the build if a fixture contains a
  substring the scrubber would rewrite.
- `quirkstest.AssertWireEqual(t, wantPath, got)` is the canonical
  form check: unmarshal → re-marshal → byte-compare. Used by every
  adapter's contract test.
- Fixtures live in the `provider` module at
  `harness/internal/provider/testdata/`. They are excluded from the
  binary via the `_test` convention.

## 8. Adjacent concerns (not part of this layer)

The quirks layer is deliberately scoped to per-(provider, model)
request-shape and response-parsing divergence. Several adjacent
concerns are tracked separately:

- **Tool-contract metadata (#222, landed).** Provider-facing tool
  metadata splits along its two cardinalities. The per-tool payload —
  input examples and annotations — lives on
  `types.ToolDefinition.Presentation`, keyed by tool name. Whether a
  (provider, model) *accepts* a given control is a check against the
  resolved `ProviderQuirks`: strict mode is `OpenAIBehaviourFlags.StrictMode`,
  and the parallel-call and examples controls are the top-level
  `ParallelToolCalls` / `ToolExamples` capabilities documented in
  [§2.1](#21-providerquirks). Each adapter's `translateTools()`
  consults the resolved capability before projecting the per-tool
  payload onto its wire shape — the per-provider mapping in the §2.1
  table.
- **Toolset profiles and aliases (#234, Wave 5).** Operator-facing
  toolset selection is a `RunConfig` concern, not a per-model
  one.
- **Tool error budgets (#230) and tool-call deduplication (#231,
  Wave 4).** Loop-level concerns that observe tool-call behaviour
  across turns, distinct from per-model wire shape.

## 9. Open risks

1. **`openai.go` `max_completion_tokens` is already unconditional on main.**
   The quirks integration does not restore `max_tokens` for providers
   that now work fine with `max_completion_tokens`. The Z.ai compat
   rule deliberately sets `TokenFieldMaxTokens`; all other
   `openai-compatible` providers default to the existing field.
   `TestNoRegressionMaxCompletionTokensDefault` pins this.

2. **`OmitSamplingParams` and a caller-supplied non-nil `Temperature`.**
   When an operator sets `--temperature 0.5` and the model is
   `o1-mini`, the quirks layer suppresses the temperature field.
   Mitigation: the debug log line at the top of `Stream()` lists the
   applied rules, and a warn-level log line fires when a non-nil
   `Temperature` is suppressed, naming the rule responsible. The
   suppressed value itself is not logged.

3. **`CompatProfile` string enum vs long-term extensibility.**
   Starting with a single value (`"zai-glm"`) is fine. The risk is
   that this field becomes a dumping ground for provider-specific
   workarounds that should instead be first-party rules. Any new
   `CompatProfile` value requires both an entry in
   `validCompatProfiles` and a matching `harness/internal/core/factory.go`
   switch arm; undocumented strings are errors at startup.

4. **`ExtraBodyFields` and the secrets invariant.** A compat rule
   that accidentally stored a secret reference in `ExtraBodyFields`
   would violate the `RunConfig.Redact()` invariant even though
   `RunConfig` is not involved. Mitigated by
   `TestBuiltinRulesExtraBodyFieldsNoSecrets`, which asserts no
   value in any registered rule's `ExtraBodyFields` contains the
   `secret://` prefix.

5. **Z.ai `tool_stream` semantics ambiguity.** The Z.ai docs
   describe `tool_stream: true` as enabling streaming tool call
   results, but it is unclear whether this affects all tool calls or
   only specific tool types, and whether it changes the SSE event
   shape on the inbound side. If a live verification finds the
   inbound shape differs, an `OpenAIBehaviourFlags.StreamToolCallShape`
   flag will be needed alongside the send-side `ExtraBodyFields`
   entry.

6. **Glob carve-out fragility.** The `gpt-5-chat*` carve-out depends
   on specificity ordering; `TestRuleCarveOuts` is the load-bearing
   negative test.

7. **ReplayFields threading scope.** Outbound threading is
   implemented for the openai-compatible adapter (see
   [§3.1](#31-replayfields-rules)): captured values ride the
   `message_complete` event onto the persisted assistant message and
   are echoed back as top-level wire keys on subsequent requests.
   Gemini ReplayFields rules remain parse-side observability — the
   typed block-level `ThoughtSignature` is Gemini's actual
   round-trip — and no rules target the Anthropic or OpenAI
   Responses adapters. The Description suffix (`(threaded)` vs
   `(parse-side only)`) is the load-bearing observability signal;
   `TestBuiltinRulesReplayFieldsSuffix` enforces it. Remaining gap:
   the BatchAdapter's result-parse path has no capture, so batch
   submissions against DeepSeek v4 thinking mode cannot sustain
   multi-turn tool-calling (§3.1).

8. **`DefaultRegistry()` singleton and test isolation.** The
   singleton is constructed once via `sync.Once` and is read-only
   after construction. Tests that need a custom rule set use
   `NewRegistry` and inject via the adapter's exported `Registry`
   field. `TestDefaultRegistryConcurrentAccess` stresses the
   singleton under `-race`.

9. **DeepSeek verification status.** The `reasoning_content` path
   and the v4 replay requirement are doc-verified as of 2026-06-07
   against the DeepSeek thinking-mode guide
   (https://api-docs.deepseek.com/guides/thinking_mode), with broad
   community corroboration of the 400-on-missing-replay behaviour on
   both first-party and OpenRouter-served v4 traffic — but no live
   first-party capture exists yet. Remaining live-capture gaps,
   each flagged in the rule comments: whether the v4 endpoint also
   accepts `max_completion_tokens` (the rules pin the doc-consistent
   legacy `max_tokens`), where `reasoning_effort` sits on the wire
   (the API reference and the guide disagree on nested-vs-top-level
   placement, so no rule ships for it), and whether any gateway
   renames the field to `reasoning`. If a live capture surfaces a
   different shape (e.g. a structured `thinking` object rather than
   a flat string), the rule's path and the corresponding
   `replay.json` fixture need to be adjusted in lockstep. The
   180-day staleness window flags the rules for re-verification.

   Operators who want v4 *non-thinking* behaviour can inject a rule
   via `NewRegistry` whose `ExtraBodyFields` carries the documented
   toggle, `{"thinking": {"type": "disabled"}}`. No builtin rule
   sets it: thinking default-on is the desired behaviour for a
   coding agent, and disabling it also forfeits the documented
   reasoning quality. The legacy `deepseek-chat` /
   `deepseek-reasoner` ids are first-party aliases for v4-flash
   non-thinking / thinking modes, fully retired after 2026-07-24
   15:59 UTC; the reasoner rule carries a matching sunset note and
   no `deepseek-chat` rule exists (non-thinking, no
   `reasoning_content`).
