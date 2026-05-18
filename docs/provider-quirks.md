# Provider quirks: per-model request-shape overrides

**Status:** Design plan. Tracks [issue #192].

[issue #192]: https://github.com/rxbynerd/stirrup/issues/192

This document specifies the framework Stirrup will adopt to handle
divergences between models served under the same provider type. It
covers the registry shape, where it integrates with the existing
adapters, how it stays visible to operators, what ships in v1, and
which divergences are deferred. Implementation is out of scope here;
this is the architectural contract that subsequent waves will
implement against.

## 1. Problem

Stirrup ships five provider adapters
([`harness/internal/provider/*.go`](../harness/internal/provider/)).
Each adapter assumes a single wire shape applies to every model under
that provider. Recent breakage shows the assumption is false:

| Provider | Model pattern | Divergence | Status |
|---|---|---|---|
| `gemini` (Vertex AI) | `gemini-3*` | `streamFunctionCallArguments` deltas; `thoughtSignature` on parts | Worked around in [#191] (flag off); thoughtSignature deferred |
| `openai-compatible` | OpenAI o-series; Azure OpenAI deployments backed by newer reasoning models | `max_tokens` rejected, `max_completion_tokens` required; `temperature`, `top_p`, `presence_penalty`, `frequency_penalty`, `logprobs`, `top_logprobs`, `logit_bias` all rejected | **Broken** |
| `openai-compatible` | GPT-5 family | `max_tokens` rejected; reasoning variants reject the same sampling param set as o-series; gpt-5.1 defaults `reasoning_effort=none`; gpt-5-pro accepts only `high`; gpt-5.1-codex-max adds `xhigh` | **Broken** |
| `openai-responses` | Various Azure Foundry deployments | Required-key rules for empty `output` / `text` fields | Resolved per-case in [#172] / [#176] |

[#191]: https://github.com/rxbynerd/stirrup/pull/191
[#172]: https://github.com/rxbynerd/stirrup/issues/172
[#176]: https://github.com/rxbynerd/stirrup/issues/176

The generalised shape: provider P serves models M1 and M2 under one
adapter wire shape S. M1 accepts S; M2 rejects S or interprets S
differently. The harness's only existing knobs (`--query-param`,
`--api-key-header`, `--base-url`) live at the URL/header layer; they
do not address body shape, field renames, conditional field omission,
value reshaping, or replay-required opaque state.

Two non-starters frame the design space:

- **Per-model adapters.** N providers × M models is an explosion the
  hand-rolled stdlib-HTTP philosophy cannot absorb. Five adapters of
  several hundred lines is the budget.
- **Wire-shape knobs on `RunConfig`.** `RunConfig` is a description of
  *intent* (provider, model, max tokens), not a request-shape patch.
  Bleeding wire fields onto the operator surface would mean operators
  routinely encode adapter internals, which both defeats Stirrup's
  declarative-config philosophy and creates a permanent backwards-
  compatibility burden as quirks shift.

## 1.1 Prior art

A focused survey of comparable projects (LiteLLM, LangChain
`langchain_openai`, Vercel AI SDK, OpenRouter, vLLM) shows the
industry has converged on a **capability table plus small transform
hooks** pattern. Where projects differ is in keying strategy and
parse-path organisation. Full research packet:
[`reviews/issue-192-prior-art.md`](../reviews/issue-192-prior-art.md).

Headline findings that shape this plan:

- **LiteLLM** uses a class-per-family hierarchy (`OpenAIOSeriesConfig`
  extends `OpenAIGPTConfig`) with a hand-edited dispatch ladder.
  Power: high; readability: low; new-model lag in the issue tracker
  is a recurring complaint.
- **LangChain** splits data and behaviour: a `_PROFILES` dict keyed
  by exact model id, a sibling TOML overrides file, and a separate
  `_compat.py` module for response-shape converters.
- **Vercel AI SDK** is the cleanest expression: a single typed
  capability struct, a `getOpenAILanguageModelCapabilities(modelId)`
  function using `startsWith` prefix chains, and inline conditionals
  in the adapter. Defaults to the modern shape; new models are
  forward-compatible by default.
- **vLLM** is illustrative of the server-side mirror: it accepts
  both `max_tokens` and `max_completion_tokens` via Pydantic
  deprecation and prefers the new one — "tolerant inbound, strict
  outbound." Azure OpenAI's strict outbound is what the harness
  absorbs.
- **OpenRouter** is a negative result: no public wire-format
  rewrite spec; everything is implicit per upstream. The interesting
  signal is what they normalise externally (a fixed `finish_reason`
  vocabulary plus a typed `reasoning_details[]` opaque-replay array),
  which informs this plan's `ReplayFields` design.

Two invariants from the survey are load-bearing for the design that
follows:

- **One source of truth for send and parse.** LiteLLM's `*Config`
  class hosts both `transform_request` and `chunk_parser`; LangChain
  ties `_compat.py` converters to the same profile entry. The cost of
  the parse path drifting from the send path is silent decode bugs
  when a future contributor updates only one half. The design pins
  send and parse to the same `ProviderQuirks`.
- **Forward-compatible defaults.** Vercel's struct defaults to
  "modern shape" so new model ids inherit the latest known wire
  format. Explicit older-family rules carry the legacy shape forward.
  This matches Stirrup's stated preference verbatim.

A note on naming. "Model profile" (LangChain, Vercel) and "model
capabilities" (Vercel) are the closest industry terms. This document
uses **"provider quirks"** to scope the abstraction tightly: the
registry holds *wire-shape divergences only*, not the broader
capability matrix (context-window size, image input, tool-calling
support) that LangChain's profiles also carry. Those concerns belong
elsewhere — `ModelRouter`, the per-provider request struct, and the
multimodal work tracked in [issue #103]. Naming the abstraction
"profile" risks expanding scope by default.

## 2. Goals and non-goals

### Goals (v1)

1. A single internal abstraction, `ProviderQuirks`, that adapters
   consult when marshalling a request and (where relevant)
   demarshalling a response.
2. A registry keyed on `(provider-type, model-pattern)` that maps to
   `ProviderQuirks` instances. Lives inside the `provider` package, not
   on `RunConfig`. The registry is designed to accommodate a future
   `BaseURLMatch` predicate without renumbering existing rules (see
   §3.2.2).
3. Coverage of the **field-rename**, **conditional-omission**,
   **value-override**, and **enum-coercion** categories — the 80% case
   the OpenAI Chat Completions divergences fall into.
4. A typed-capability flag surface for adapter-internal structural
   branching — the 20% case the Gemini 3.x streaming-deltas divergence
   falls into.
5. A declared but not-yet-implemented `ReplayFields` surface for
   opaque-state fields the next turn must thread back verbatim
   (`reasoning_content`, `reasoning.encrypted_content`,
   `thinking.signature`, `thought_signature`). Declaring the surface
   in v1 pins the shape the deferred-work PRs will implement against.
6. Forward-compatible defaults: a model name unmatched by any rule
   inherits the latest known shape for its provider type, not the
   legacy one. Explicit fallback rules carry older model families
   forward.
7. Every applied quirk is observable: emitted as span attributes on
   the existing `provider.stream` span and as a structured log line
   at debug level.
8. Each registry entry is paired with captured wire fixtures
   (request + response) and exercised in the existing
   `httptest`-based test suite. The fixture is the contract.

### Non-goals (v1)

- **Operator-authored quirks.** Operators cannot register custom
  quirks via `RunConfig`. This is by design (predictability,
  audit-clean trace shape). A future emergency-override field can be
  added if operational pressure justifies the surface; v1 expects new
  divergences to land as registered rules in a follow-up PR.
- **Auto-detection / retry-on-error.** Some divergences could be
  inferred from a parseable 400 and retried on the alternate shape.
  v1 ships declaration-first because (a) the error catalogue differs
  per gateway (Azure, OpenRouter, LiteLLM, vLLM each phrase the same
  refusal differently), (b) silent retry doubles request cost on
  every miss, and (c) "predictable behaviour" is a load-bearing
  Stirrup invariant. Detection is a v2 layer, opt-in.
- **Replay-fields conversation threading.** The `ReplayFields`
  surface lands in v1 (so the rule shape stabilises) but the
  conversation-history builder that consults it is deferred to its
  own PR. Rules declaring `ReplayFields` parse the field on
  responses but the next-turn replay is not yet wired. Tracked
  alongside the [#191] follow-up for `thoughtSignature`.
- **Bedrock.** The `bedrock` adapter speaks through the AWS SDK's
  Converse API, which already abstracts model-family wire
  differences. Bedrock-family divergences (e.g. Llama-3 vs Claude-on-
  Bedrock stop-reason semantics) are out of scope here and would be
  addressed by extending the stop-reason mapping path in
  [`harness/internal/provider/bedrock.go`](../harness/internal/provider/bedrock.go),
  not by the registry. The registry's interface is provider-
  agnostic, so Bedrock can plug in later without redesign. Note:
  AWS Bedrock now also serves a separate OpenAI-compatible endpoint
  (`bedrock-runtime.*.amazonaws.com/openai/v1/`) which would be
  reached via `provider.type = openai-compatible`; quirks for that
  endpoint do land in the registry, just under the openai-compatible
  provider type.
- **`thoughtSignature` round-trip.** The `ReplayFields` surface in
  §3.1 declares the shape; the actual history-builder threading is
  tracked separately as a follow-up to [#191] and lands in the same
  PR that wires the broader replay-fields path.
- **Multimodal content shape variations.** Tracked under
  [issue #103]; multimodal blocks have their own per-provider
  serialisation concerns that overlap but are largely additive to the
  quirks surface.
- **Message-role rewriting** (e.g. mapping `system` → `developer`
  for OpenAI o-series / gpt-5; mapping `system` → `developer` on
  Cerebras gpt-oss-120b). The transformation is well-defined but
  invasive — it touches the conversation-history builder, not just
  the marshaller — and the production failure rate on this is low
  (most o-series deployments tolerate `system` and the documented
  rename to `developer` is forward-looking). Deferred to a follow-up
  once the `ReplayFields` work lands, since both touch the same
  history-builder path.

[issue #103]: https://github.com/rxbynerd/stirrup/issues/103

## 3. Design

### 3.1 Core type

```go
// ProviderQuirks describes the request- and response-shape
// adjustments that apply to a specific (provider, model) pair. It is
// the in-memory result of consulting the quirks registry, not a wire
// type. Adapters read fields off this struct when building a request
// and (where relevant) when interpreting a response.
//
// Registry.Resolve always returns a ProviderQuirks with every map
// pre-initialised to empty (never nil) so a Rule's Apply may write
// freely. A zero value of the struct as constructed by a caller
// outside the registry is NOT safe to mutate; the registry is the
// only construction path.
type ProviderQuirks struct {
    // FieldRenames maps a canonical adapter-internal field name to
    // the wire JSON key the request should use. Empty map (or a key
    // not present) means "emit the field under its canonical name".
    //
    // Example: {"max_tokens": "max_completion_tokens"} flips the
    // OpenAI Chat Completions body to use the newer reasoning-model
    // key. Renames apply only to the canonical fields the adapter
    // declares in its rename surface (see section 3.3). Unknown keys
    // are a programming error: each adapter registers its canonical-
    // field set with the package at init(), and Registry.Resolve
    // validates every Rule's outputs against the registered set on
    // construction. A rule referencing an unknown canonical field
    // panics at startup, surfaced by TestBuiltinRulesValidate.
    FieldRenames map[string]string

    // OmitFields lists canonical fields the adapter MUST NOT emit on
    // the wire for this (provider, model) pair, even when their value
    // is non-zero. Used for parameters models reject outright — e.g.
    // OpenAI o-series rejects `temperature`, `top_p`,
    // `presence_penalty`, `frequency_penalty`, `logprobs`,
    // `top_logprobs`, and `logit_bias` entirely.
    OmitFields []string

    // ValueOverrides forces a canonical field's serialised value,
    // ignoring whatever `StreamParams` carried in. Used sparingly —
    // when a model accepts a parameter only when set to a specific
    // value (e.g. some o-series builds accept `temperature` only when
    // it equals 1.0). The override is applied before OmitFields, so a
    // field both overridden and omitted is omitted (omission wins).
    //
    // Values are typed via a small union to avoid stringly-typed
    // surprises; see `quirks.Value`.
    ValueOverrides map[string]Value

    // EnumCoercions maps a canonical field's caller-supplied string
    // value to the wire value the request should emit. Used when a
    // model accepts a parameter but only at a subset of canonical
    // enum values — e.g. Cohere's compatibility endpoint accepts
    // `reasoning_effort` only as "none" or "high", so a caller-
    // supplied "medium" needs coercing to "high"; DeepSeek-V4 thinking
    // mode coerces "low"/"medium" up to "high" and "xhigh" up to "max".
    //
    // Distinct from ValueOverrides: ValueOverrides forces a value
    // irrespective of caller input; EnumCoercions transforms the
    // caller's value through a per-field translation table. A
    // missing outer key falls through to the canonical value
    // unchanged. A present outer key with no matching inner entry
    // means the caller's value is unsupported and the field is
    // dropped (equivalent to a one-off OmitFields entry); the
    // registry self-test warns on rules that use this pattern
    // without explicit intent, since it can hide breakage from
    // operators.
    //
    // Restricted to string-valued enums in v1. Reasoning-effort,
    // service-tier, and similar canonical fields all fit this
    // shape. A future numeric-enum case would extend to Value-typed
    // coercion; the keyed-by-string design preserves trace
    // readability (`reasoning_effort: medium → high` reads cleanly
    // in span attributes).
    EnumCoercions map[string]map[string]string

    // ReplayFields lists canonical assistant-message field paths the
    // conversation-history builder MUST thread verbatim across turns.
    // Used for opaque-state fields the upstream gateway treats as
    // required-on-follow-up: omitting them on the next turn returns
    // 400 with a gateway-specific error message.
    //
    // Known cases (lands in v1 as parse-side recognition; history-
    // builder threading deferred to the [#191] follow-up PR):
    //   - "reasoning_content"           — DeepSeek-V4 thinking
    //   - "reasoning.encrypted_content" — OpenAI Responses reasoning
    //   - "thinking.signature"          — Anthropic thinking blocks
    //   - "tool_calls[].thought_signature" — Gemini 3 partial-args
    //
    // Paths use a small JSON-pointer-ish dialect: dot-separated keys,
    // `[]` denoting array-of-objects iteration. The history builder
    // walks the path on each prior assistant message and preserves
    // the value byte-for-byte on the next turn.
    //
    // v1 only requires the response parser to recognise and preserve
    // the field on the inbound side; the threading on outbound is
    // implemented in the follow-up. Until then, multi-turn runs
    // against models that require replay will fail on turn 2 — same
    // failure mode as today, but at least observable via the
    // resolved quirks.
    ReplayFields []string

    // StructuralFlags carries adapter-specific flags for divergences
    // that cannot be expressed as flat field operations. The flag set
    // is closed and typed per adapter (e.g. `openai.StructuralFlags`,
    // `gemini.StructuralFlags`) so the compiler enforces that an
    // adapter only ever reads flags it knows about.
    //
    // Examples:
    //   - Gemini's StructuralFlags carries
    //     `StreamFunctionCallArgsShape ∈ {Off, V2Snapshot, V3Deltas}`.
    //     The default (`Off`) preserves the post-#191 production
    //     behaviour.
    //   - openai-compatible's StructuralFlags will gain a
    //     `StreamFraming ∈ {OpenAIChat, OpenRouterEnveloped,
    //     LMStudioResponses}` flag in a future wave when Stirrup
    //     starts proxying through gateways whose SSE shape differs.
    //     v1's openaiStructuralFlags is empty; the type exists so
    //     the registry already speaks the right shape.
    StructuralFlags any
}
```

The `Value` type is a small tagged union covering the JSON scalar
shapes the adapters currently marshal:

```go
// Value is a typed JSON scalar used by ProviderQuirks.ValueOverrides.
// Exactly one of String, Int, Float, Bool, or Null is set per value;
// the registry's New<Kind> constructors enforce the invariant. The
// union exists so a rule that forces `temperature: 1.0` cannot
// accidentally serialise as `"1.0"` (string) or trip the marshaller
// into emitting `temperature: null` when the rule meant "omit".
type Value struct {
    String *string
    Int    *int
    Float  *float64
    Bool   *bool
    Null   bool
}

func NewStringValue(s string) Value { return Value{String: &s} }
func NewIntValue(i int) Value       { return Value{Int: &i} }
func NewFloatValue(f float64) Value { return Value{Float: &f} }
func NewBoolValue(b bool) Value     { return Value{Bool: &b} }
func NewNullValue() Value           { return Value{Null: true} }
```

Adding a new scalar shape is a deliberate change-control point: the
new field plus its constructor, the marshaller switch in each
adapter, and a vet rule asserting every constructor is covered must
all land together. A linter check (`quirks_value_exhaustive`) gates
the union against silent drift.

### 3.2 Registry shape

```go
// Rule is one entry in the quirks registry. Rules are evaluated in
// specificity order (longest ModelMatch glob wins) with declaration
// order as tiebreaker; every matching rule's Apply runs in turn,
// composing onto the accumulating ProviderQuirks. Section 3.2.1
// explains why specificity-then-order rather than first-match-wins
// or pure declaration order.
type Rule struct {
    // ProviderType is the exact RunConfig provider.type the rule
    // applies to (e.g. "openai-compatible", "gemini"). The empty
    // string is reserved as a "matches every provider" wildcard for
    // future use; v1 rules always pin a specific provider.
    ProviderType string

    // ModelMatch is a glob pattern (Go's path.Match grammar) tested
    // against StreamParams.Model. Empty matches every model under
    // ProviderType. Patterns are case-sensitive; the canonical model
    // identifier is whatever the operator passed in.
    ModelMatch string

    // Description is a one-line human label that appears in trace
    // attributes ("provider.quirk.applied") and the CLI introspection
    // subcommand. Required: an empty Description causes
    // BuiltinRules's startup self-check to panic, asserted by
    // TestBuiltinRulesNonEmptyDescriptions. The string is the only
    // identifier of a rule that surfaces in observability, so it
    // doubles as the rule's audit name.
    Description string

    // LastVerified is the date the rule's behaviour was last
    // empirically confirmed against the upstream gateway. Used for
    // staleness signalling: rules whose LastVerified is older than
    // 180 days emit a debug-level "rule may be stale" log line on
    // first match in a run, and the introspection subcommand renders
    // a warning column. Not a correctness gate — just a maintenance
    // signal so perishable rules (DeepSeek's 2026-07-24 model
    // retirement, Azure Foundry API rebrands, mid-life model
    // renames) surface for review before they break.
    //
    // Set via the helper `quirks.Date("2026-05-01")` which panics
    // on parse error; rules are static so a bad date is effectively
    // a compile-time failure.
    LastVerified time.Time

    // Apply composes this rule's adjustments onto q. Implementations
    // mutate q in place. Apply is called only after a positive match,
    // so the function does not re-check ProviderType / ModelMatch.
    Apply func(q *ProviderQuirks)
}

// Registry is the ordered list of rules consulted at adapter
// construction time. The default registry is built by `BuiltinRules`;
// tests may swap in a different registry via dependency injection.
type Registry struct {
    rules []Rule
}

// Resolve walks the registry in declaration order, applying every
// matching rule to a freshly-initialised ProviderQuirks. The returned
// value is safe to retain for the lifetime of an adapter — quirks are
// not reactive to runtime state.
func (r *Registry) Resolve(providerType, model string) ProviderQuirks
```

Matching uses Go's standard `path.Match` so the pattern grammar is
familiar to operators reading test output and matches the directory-
glob conventions already used in the codebase. Examples:

- `gemini-3*` matches `gemini-3.1-pro-preview` and `gemini-3-flash`.
- `o[1-9]-*` matches `o1-mini`, `o3-mini`, `o4-mini`, but NOT
  bare `o1` (the trailing hyphen is literal).
- `gpt-4*` matches `gpt-4`, `gpt-4o`, `gpt-4o-mini`.
- `gpt-5*` matches `gpt-5`, `gpt-5-mini`, `gpt-5.1-codex-max`.

Regex was considered and rejected: glob covers every realistic case,
the trace attribute is more readable, and there is no need for the
expressive power of capture groups or character classes.

`path.Match`'s metacharacters are `*`, `?`, `[`, `]`, and `\`. A
survey of current OpenAI, Anthropic, Vertex AI, Bedrock, and
Mistral catalogue identifiers shows none contains any of these
characters. Self-hosted echoes are noisier — Ollama emits tags
(`llama3.3:70b-instruct`), LM Studio emits variant qualifiers
(`google/gemma-3-12b@q3_k_l`), and vLLM emits LoRA module names
that are operator-chosen. The colon, slash, and `@` are all safe
for `path.Match`, but the registry self-test
`TestNoMetacharsInKnownModelIDs` pins the assumption against a
catalogue file that includes representative samples from each
self-hosted server in addition to the hosted catalogues. A future
identifier that breaks the assumption forces a switch to
literal-then-glob escaping, not a redesign.

#### 3.2.1 Rule composition: longest-pattern wins, then declaration order

The registry composes by **specificity, then declaration order**:

1. Every rule whose `ProviderType` matches and whose `ModelMatch`
   glob matches the model is a candidate.
2. Candidates are sorted by pattern length (longer wins), then by
   declaration order (earlier wins).
3. The full ordered list applies in sequence — each `Apply`
   composes onto the accumulating `ProviderQuirks`.

This lets a wide rule (`gpt-5*`) establish a baseline and a narrower
rule (`gpt-5-chat*`) override it without depending on rule authors
to remember a brittle "less-specific first" convention.
Specificity-then-order is the rule operators learn from CSS, glibc
`gettext`, and `kubectl`'s label selectors; it carries here.

The alternative (first-match-wins, Vercel's choice) was considered.
It is simpler when each model needs at most one rule, but composes
poorly when a family-wide default and a narrower exception coexist.
v1 ships specificity-ordered composition; if the rule set proves to
be one-rule-per-model in practice, the registry can be simplified
post hoc.

Composable Apply helpers — small named functions in
`harness/internal/provider/quirks_helpers.go` — let several rules
share a common policy without duplicating its logic. The
`applyOpenAIReasoningClass` helper used in §6.1 is the first such
helper; further helpers can be added as patterns emerge across
rules.

#### 3.2.2 Future seam: `BaseURLMatch` predicate

The registry keys on `(ProviderType, ModelMatch)` in v1. A future
revision will add an optional `BaseURLMatch` glob that, when
non-empty, narrows the rule to gateways whose base URL matches the
glob:

```go
// (sketch, not in v1)
type Rule struct {
    ProviderType  string
    ModelMatch    string
    BaseURLMatch  string  // optional; "" matches all base URLs
    Description   string
    LastVerified  time.Time
    Apply         func(q *ProviderQuirks)
}
```

The seam matters because real gateways break the upstream's shape:
Azure Foundry adds preview-header quirks under
`*.openai.azure.com`; OpenRouter wraps errors in a nested envelope
and exposes `native_finish_reason` under `openrouter.ai/api/*`;
self-hosted vLLM/llama.cpp under `localhost:*` is more lenient than
hosted OpenAI under `api.openai.com/*`. v1 punts on this — operators
configuring a non-canonical gateway under `provider.type =
openai-compatible` get the canonical OpenAI rules, which the gateway
will usually tolerate.

The reason to sketch the seam now rather than later: rules
declared in v1 must be representable in the v2 shape without
ambiguity. The v1 rules below all assume the canonical hosted
endpoint; a future rule that explicitly targets Azure can add
`BaseURLMatch: "*openai.azure.com*"` without renumbering or
restructuring. The composition order extends naturally:
specificity is the sum of `len(ModelMatch) + len(BaseURLMatch)`,
so a `(ProviderType + ModelMatch + BaseURLMatch)` triple is more
specific than a `(ProviderType + ModelMatch)` pair.

### 3.3 Adapter integration

Each adapter exposes a small, typed surface that the registry
populates. The intent is that the registry never knows the JSON keys
an adapter ultimately emits; it speaks only in canonical names the
adapter package owns.

```go
// In harness/internal/provider/openai_quirks.go (illustrative)
type openaiCanonicalField string

const (
    fieldMaxTokens         openaiCanonicalField = "max_tokens"
    fieldTemperature       openaiCanonicalField = "temperature"
    fieldTopP              openaiCanonicalField = "top_p"
    fieldPresencePenalty   openaiCanonicalField = "presence_penalty"
    fieldFrequencyPenalty  openaiCanonicalField = "frequency_penalty"
    fieldLogprobs          openaiCanonicalField = "logprobs"
    fieldTopLogprobs       openaiCanonicalField = "top_logprobs"
    fieldLogitBias         openaiCanonicalField = "logit_bias"
    fieldReasoningEffort   openaiCanonicalField = "reasoning_effort"
    fieldStream            openaiCanonicalField = "stream"
    // ... etc
)

// openaiStructuralFlags is the typed flag set the Chat Completions
// adapter reads off ProviderQuirks.StructuralFlags. v1 is empty;
// future Chat Completions divergences declare flags here. The first
// known case landing in a follow-up wave is SSE-dialect selection
// (OpenAI canonical vs OpenRouter envelope vs LM Studio Responses
// shape), keyed off provider.type + BaseURLMatch once §3.2.2 lands.
type openaiStructuralFlags struct {
    // (none in v1; the openai-compatible divergences known today are
    // all expressible as FieldRenames / OmitFields / EnumCoercions.)
}
```

The adapter's request-marshalling path becomes:

1. `quirks := registry.Resolve(providerType, params.Model)` at the
   top of each `Stream` call. Section 4 explains why per-stream
   rather than per-adapter.
2. The marshaller asks `quirks` for the wire key of each canonical
   field, the omission set, the value-override set, and the
   enum-coercion table; then emits the body accordingly.
3. The marshaller branches on `StructuralFlags` for any divergence
   it has been written to honour.
4. The same `quirks` value is captured into the SSE-reader closure
   so the parse path consults the same source of truth as the send
   path (the Codec invariant; see 3.3.1).
5. The response parser preserves any path in `ReplayFields` from
   the inbound assistant message onto the message struct so the
   history builder (when it lands in the follow-up PR) can thread
   it on the next turn.

For Gemini, the same pattern applies; `geminiStructuralFlags` would
contain `StreamFunctionCallArgsShape` and the adapter would key off
that when constructing the `toolConfig.functionCallingConfig` block.

#### 3.3.1 The Codec invariant

The same resolved `ProviderQuirks` value drives both the **send
path** (request marshalling) and the **parse path** (response/SSE
demarshalling). Adapters must not consult separate quirks for the
two halves, and the registry must not carry separate "request" and
"response" rule lists.

The reason is the most consistent finding from the prior-art survey:
every project that split the parse and send paths into independent
configurations ended up shipping bugs where one half was updated and
the other was not. LiteLLM keeps both methods on the same
`*Config` class for exactly this reason; LangChain ties its
`_compat.py` converters to the profile registry; Vercel keeps the
capability struct and the inline branches both in
`openai-chat-language-model.ts`. The invariant is operational, not
just stylistic.

Concretely:

- A rule that introduces a request-side rename (e.g. emit
  `max_completion_tokens` instead of `max_tokens`) must also handle
  the response side if and only if the response carries the same
  field. (For the OpenAI o-series case, `usage.completion_tokens`
  is unchanged; no parse-side work is needed. The Gemini 3.x case
  is the opposite — request-side wire is unchanged but the
  response chunk shape diverges; the structural flag drives the
  parse branch.)
- The adapter's parse code reads off the same `ProviderQuirks`
  captured at request time. The streaming closure carries the
  quirks value into `consumeSSE`, so a long-lived stream cannot
  see a different rule resolution mid-flight.
- `ReplayFields` is the canonical example of the invariant: the
  same path string drives the response parser's "preserve this
  field" behaviour and (in the follow-up PR) the history builder's
  "thread this field" behaviour. The two halves cannot diverge
  because they consult the same slice.
- Tests assert symmetry: each registered rule has both a request
  fixture and (where the parse path diverges) a response fixture,
  and the test exercises both halves through the same `Resolve`
  call.

### 3.4 Operator surface

`RunConfig` is unchanged. Operators do not author quirks.

Two visibility seams are added:

1. **Trace attribute.** The `provider.stream` span already exists.
   The matched rules' `Description` values are emitted as a single
   string-slice attribute `provider.quirk.applied`. OTel slice
   attributes survive the supported export pipelines (OTLP
   traces and the JSONL emitter at
   [`harness/internal/trace/jsonl.go`](../harness/internal/trace/jsonl.go));
   confirm before merge that the `runs replay` rendering surfaces
   slice attributes as a comma-separated list. Empty (no rule
   matched, the common case) renders as the attribute being absent.

2. **CLI introspection.** A new subcommand:

   ```
   stirrup providers quirks \
       --provider openai-compatible \
       --model gpt-5-nano
   ```

   prints the resolved `ProviderQuirks` as JSON, including the
   description, `LastVerified` date, and staleness status of every
   rule that contributed. The subcommand is side-effect-free and
   reads only the in-memory registry. It exists so an operator
   hitting an unexpected 400 from an upstream gateway can confirm
   what shape the harness was sending without enabling debug logs.

### 3.5 Future operator override (deliberately not in v1)

The plan deliberately defers an operator-authored override
(`provider.quirks` on `ProviderConfig`). Reasons:

- Every quirk landed as a registered rule benefits every operator
  hitting the same upstream gateway. An operator-authored override
  silos the workaround.
- The blast radius of a misconfigured quirk is large (request body
  rejected, run dies on turn 1) and the failure mode is opaque.
- The introspection subcommand makes the registry visible enough
  that operators can confirm coverage without authoring overrides.

If operational pressure ever justifies it, the override should be a
narrow surface that piggybacks the same `ProviderQuirks` type
(e.g. only `OmitFields`, `FieldRenames`, and `EnumCoercions`; no
`StructuralFlags`, `ValueOverrides`, or `ReplayFields`), and every
applied override should fire a `security.SecurityLogger` event so
audit trails can flag custom wire-shape adjustments.

## 4. Where quirks are resolved

The adapter is constructed in
[`harness/internal/core/factory.go`](../harness/internal/core/factory.go)
once per run. The `ModelRouter` then selects a `(provider, model)`
pair per turn, which the loop hands to the adapter via `StreamParams`.

There are two reasonable resolution points:

- **Per-adapter (at factory time).** Resolve quirks using the
  `ModelRouter`'s *default* model. Cheaper, but breaks under the
  per-mode and dynamic routers, which can pick a different model per
  turn.
- **Per-stream (at request-build time).** Resolve quirks using
  `params.Model`. Pays one map lookup and one ordered walk per turn,
  which is negligible against the network round-trip. Always correct.

V1 resolves **per-stream**. The marginal cost is dominated by the SSE
read; the correctness gain matters because dynamic routing across an
`o1` cheap model and a `gpt-4o` expensive model within one run is a
configuration the documentation explicitly supports.

To avoid recomputing on every line of an SSE stream, the resolution
happens once at the top of `Stream` and is captured in a local
variable threaded through `consumeSSE`. The `Registry.Resolve` call
walks an ordered slice — typical rule counts are in the tens, so the
cost is genuinely irrelevant.

## 5. Categories of divergence covered in v1

| Category | Mechanism | Example |
|---|---|---|
| Field rename | `FieldRenames` map | OpenAI `max_tokens` → `max_completion_tokens` for `o1-*`, `o3-*`, `o4-*`, `gpt-5*` |
| Conditional omission | `OmitFields` slice | OpenAI reasoning class rejects `temperature`, `top_p`, `presence_penalty`, `frequency_penalty`, `logprobs`, `top_logprobs`, `logit_bias`; omit when matched |
| Value override | `ValueOverrides` map | A model that accepts `temperature` only when it equals 1.0 |
| Enum coercion | `EnumCoercions` map | Cohere compat accepts `reasoning_effort ∈ {none, high}` only; DeepSeek-V4 thinking coerces `low\|medium → high` and `xhigh → max` |
| Replay-required state (parse-side only in v1) | `ReplayFields` slice | DeepSeek `reasoning_content`, OpenAI Responses `reasoning.encrypted_content`, Anthropic `thinking.signature`, Gemini 3 `thought_signature` |
| Structural shape | typed `StructuralFlags` per adapter | Gemini 3.x `streamFunctionCallArguments` deltas (currently set to "off" universally; the flag exists so a future reinstatement can be model-scoped). Future SSE-dialect selection on openai-compatible (OpenRouter envelope, LM Studio Responses) will land here. |
| Response parsing | adapter-side branch on `StructuralFlags` | Gemini 3.x `partialArgs` array vs 2.x snapshot |

Out of v1 scope (deferred to follow-up issues):

- **Replay-fields threading on outbound** — the field surface is
  declared and the response parser preserves the data; the
  conversation-history builder that emits the preserved values on
  the next turn is the deferred piece. Tracked with [#191].
- **Message-role rewriting** (system → developer for OpenAI o-
  series / gpt-5 and Cerebras gpt-oss-120b). The transformation
  touches the conversation-history builder, not the marshaller; it
  lands alongside the replay-fields threading work.
- **Cross-cutting metadata renames** (e.g. `usage.completion_tokens`
  → `usage.output_tokens`). The parse-side rename surface is more
  invasive than the send side and warrants its own design pass once
  a concrete case lands.
- **Authentication-shape divergences** (already handled by the
  existing `APIKeyHeader` / `Credential` surface; out of registry
  scope).
- **Endpoint path divergences** (e.g. moving from
  `/v1/chat/completions` to `/v2/...`). `BaseURL` already covers
  this; the registry should not.

## 6. Concrete cases addressed at v1 ship

Each entry below is a rule that lands in the v1 registry. Every rule
ships with one or more captured wire fixtures and a regression test.

### 6.1 OpenAI Chat Completions: reasoning-class param restrictions

The o-series and gpt-5 reasoning models share a common rejection set:
`max_tokens` is replaced by `max_completion_tokens`, and the sampling
parameters `temperature`, `top_p`, `presence_penalty`,
`frequency_penalty`, `logprobs`, `top_logprobs`, and `logit_bias` are
rejected outright. (Source: Microsoft Foundry docs,
`learn.microsoft.com/en-us/azure/foundry/openai/how-to/reasoning`;
empirically confirmed against `api.openai.com` Chat Completions and
Azure deployments backed by o-series.)

A composable helper expresses the shared policy without duplicating
seven omissions across each family rule:

```go
// In harness/internal/provider/quirks_helpers.go
func applyOpenAIReasoningClass(q *ProviderQuirks) {
    q.FieldRenames["max_tokens"] = "max_completion_tokens"
    q.OmitFields = append(q.OmitFields,
        "temperature",
        "top_p",
        "presence_penalty",
        "frequency_penalty",
        "logprobs",
        "top_logprobs",
        "logit_bias",
    )
}
```

The rules call the helper:

```go
// inside BuiltinRules() []Rule { return []Rule{
{
    ProviderType: "openai-compatible",
    ModelMatch:   "o[1-9]-*",
    Description:  "OpenAI o-series: reasoning-class param restrictions",
    LastVerified: Date("2026-05-01"),
    Apply:        applyOpenAIReasoningClass,
},
{
    ProviderType: "openai-compatible",
    ModelMatch:   "gpt-5*",
    Description:  "OpenAI GPT-5 family: max_completion_tokens required",
    LastVerified: Date("2026-05-01"),
    Apply: func(q *ProviderQuirks) {
        // GPT-5 family always requires max_completion_tokens, even
        // for non-reasoning variants (gpt-5-chat-latest).
        q.FieldRenames["max_tokens"] = "max_completion_tokens"
    },
},
{
    // Narrower override: gpt-5 reasoning variants (everything in
    // the gpt-5 family that is NOT gpt-5-chat*) inherit the full
    // reasoning-class omissions.
    ProviderType: "openai-compatible",
    ModelMatch:   "gpt-5*",
    Description:  "OpenAI GPT-5 reasoning: sampling param restrictions",
    LastVerified: Date("2026-05-01"),
    Apply: func(q *ProviderQuirks) {
        // applyOpenAIReasoningClass adds the rename and omissions.
        // The rename is idempotent; the omissions are append-only,
        // so re-applying it is safe.
        applyOpenAIReasoningClass(q)
    },
},
{
    // Carve-out: gpt-5-chat is non-reasoning and accepts the full
    // sampling set. The rule applies AFTER the broader gpt-5* rule
    // by virtue of being a longer pattern (specificity-then-order;
    // see §3.2.1).
    ProviderType: "openai-compatible",
    ModelMatch:   "gpt-5-chat*",
    Description:  "OpenAI gpt-5-chat: non-reasoning, restore sampling",
    LastVerified: Date("2026-05-01"),
    Apply: func(q *ProviderQuirks) {
        // The broader gpt-5* rules have populated OmitFields; this
        // rule clears the entries that gpt-5-chat actually accepts.
        // A small helper, removeFromOmit, keeps the semantics
        // explicit.
        removeFromOmit(q,
            "temperature", "top_p",
            "presence_penalty", "frequency_penalty",
            "logprobs", "top_logprobs", "logit_bias",
        )
    },
},
// }}
```

The composition relies on specificity ordering: `gpt-5*` (5 chars,
not counting `*`) applies before `gpt-5-chat*` (10 chars). The
negative-test discipline in §7.2 pins this — a test asserts
`gpt-5-chat-latest` ends with no entries in `OmitFields`, while
`gpt-5-mini` ends with the full reasoning-class set.

#### 6.1.1 Azure deployment-name handling

The Azure-deployment-name dimension is handled by the same rules:
Azure OpenAI deployment names default to the underlying model name,
and operators are encouraged to keep that mapping for the model
field. When a deployment is named non-canonically (e.g.
`my-gpt5-prod`), the registry cannot match. v1 solution: document
the convention in [`docs/providers.md`](./providers.md) and treat
non-canonical deployment names as falling back to the default shape
(which will fail loudly at first request). A v2 surface could
expose a `provider.modelFamily` hint on `RunConfig` so operators
with non-canonical Azure deployment names can opt into a model
family; this is the operator-authored-override seam from
section 3.5, kept behind a tighter contract than free-form quirks.

The future `BaseURLMatch` predicate (§3.2.2) covers the inverse
case: an Azure-only quirk that does not apply to OpenAI direct
deployments under the same model name. Azure preview-header
behaviour and Azure-side rate-limit-header inconsistencies (both
documented as buggy by Microsoft) are candidates once `BaseURLMatch`
lands.

### 6.2 Gemini 3.x: streamed function-call argument shape

The current state is "flag globally off". The registry codifies that
and gives a future re-enablement a model-scoped seam:

```go
// inside BuiltinRules() []Rule { return []Rule{
{
    ProviderType: "gemini",
    ModelMatch:   "*",
    Description:  "Default: do not stream function-call arguments",
    LastVerified: Date("2026-05-01"),
    Apply: func(q *ProviderQuirks) {
        q.StructuralFlags = geminiStructuralFlags{
            StreamFunctionCallArgsShape: StreamArgsOff,
        }
    },
},
// Future entry (illustrative, NOT shipped in v1):
// {
//     ProviderType: "gemini",
//     ModelMatch:   "gemini-2.*",
//     Description:  "Gemini 2.x supports cumulative-snapshot streaming",
//     LastVerified: Date("2026-XX-XX"),
//     Apply: func(q *ProviderQuirks) {
//         q.StructuralFlags = geminiStructuralFlags{
//             StreamFunctionCallArgsShape: StreamArgsV2Snapshot,
//         }
//     },
// },
// }}
```

The Gemini adapter's `BuildGenerateContentRequest` consults
`StructuralFlags.StreamFunctionCallArgsShape` instead of the
hard-coded `false` it sets today. v1 preserves today's wire output
exactly — the flag's default is `StreamArgsOff`.

### 6.3 OpenAI Responses: required-key serialisation invariants

The required-key invariants for `output` and `text` (resolved in
[#172] / [#176]) are not registry-resolved divergences; they apply
to every Responses request regardless of model. The shape: the
upstream rejects requests where the `output` / `text` keys are
absent, so the marshaller always emits the keys even when the value
is the empty string. This is encoded by omitting `omitempty` from
the `json` struct tags for those fields in
[`openai_responses.go`](../harness/internal/provider/openai_responses.go);
no quirks-registry entry participates.

Documented here because the case appears in the issue. The reason
it is not in the registry: the divergence is "the wire format
requires a present-but-empty value", not "this model differs from
that model". Registry membership is reserved for genuine per-model
splits.

### 6.4 OpenAI Chat Completions: sampling-parameter rejection on reasoning models

Covered by the rules in 6.1 (the `applyOpenAIReasoningClass` helper).
When a reasoning-class rule is matched, the seven rejected sampling
parameters are dropped from the request body. `StreamParams`'
populated fields retain their values for tracing; the omission
happens at marshal time.

### 6.5 Replay-fields recognition (parse-side; threading deferred)

Three rules land in v1 to declare the `ReplayFields` surface and wire
the response-parse recognition. The corresponding outbound threading
in the conversation-history builder is deferred to the [#191]
follow-up PR. Until that PR lands, multi-turn runs against the
affected models will fail on turn 2 — the same failure mode as today,
but at least the resolved quirks make the cause observable.

```go
// inside BuiltinRules() []Rule { return []Rule{
{
    ProviderType: "openai-compatible",
    ModelMatch:   "deepseek-reasoner*",
    Description:  "DeepSeek thinking: preserve reasoning_content (parse-side only)",
    LastVerified: Date("2026-05-01"),
    Apply: func(q *ProviderQuirks) {
        q.ReplayFields = append(q.ReplayFields, "reasoning_content")
    },
},
{
    ProviderType: "openai-compatible",
    ModelMatch:   "deepseek-v4*",
    Description:  "DeepSeek-V4 thinking: preserve reasoning_content (parse-side only)",
    LastVerified: Date("2026-05-01"),
    Apply: func(q *ProviderQuirks) {
        q.ReplayFields = append(q.ReplayFields, "reasoning_content")
    },
},
{
    ProviderType: "gemini",
    ModelMatch:   "gemini-3*",
    Description:  "Gemini 3: preserve thought_signature on tool-call parts (parse-side only)",
    LastVerified: Date("2026-05-01"),
    Apply: func(q *ProviderQuirks) {
        q.ReplayFields = append(q.ReplayFields,
            "tool_calls[].thought_signature",
        )
    },
},
// }}
```

The OpenAI Responses `reasoning.encrypted_content` and Anthropic
`thinking.signature` cases will land as rules when their adapters
gain the response-parse hook; both are tracked in the same follow-up.

### 6.6 Enum coercion (deferred but with sketched rules)

The `EnumCoercions` surface lands with no rules in v1 — the
existing five-adapter set does not include Cohere or DeepSeek-V4.
The surface is sketched here so the design intent is on record and
the registry self-tests can pin its semantics with synthetic rules.

The intended shape, when Cohere is added:

```go
// (illustrative; not in v1 BuiltinRules)
{
    ProviderType: "openai-compatible",
    ModelMatch:   "command-*",
    BaseURLMatch: "*.cohere.ai/compatibility/*",  // requires §3.2.2
    Description:  "Cohere compat: reasoning_effort accepts only none/high",
    LastVerified: Date("2026-XX-XX"),
    Apply: func(q *ProviderQuirks) {
        q.EnumCoercions["reasoning_effort"] = map[string]string{
            "low":     "none",
            "medium":  "high",
            "minimal": "none",
            "xhigh":   "high",
        }
    },
},
```

## 7. Validation and testing

### 7.1 Captured wire fixtures

Each rule lands with at least one captured wire fixture stored under
`harness/internal/provider/testdata/quirks/<provider>/<model>/`:

- `request.json` — the body the harness produces under the rule.
- `response.sse` — a recorded successful SSE response from the
  upstream gateway, or a synthetic equivalent when the rule covers a
  response-parse path. Synthetic fixtures must include a comment
  explaining their derivation.
- `replay.json` — for `ReplayFields` rules, an assistant-message
  snapshot showing the preserved field as the response parser
  retains it. Present only for rules that touch `ReplayFields`.

The fixture is the contract: if a rule's `Apply` changes, the
captured request must change with it. The test:

1. Runs `Registry.Resolve("openai-compatible", "o1-mini")`.
2. Hands the resulting quirks plus a canonical `StreamParams` to the
   adapter's request marshaller.
3. Asserts byte-equality with `testdata/quirks/openai-compatible/o1-mini/request.json`
   after a canonicalisation pass: unmarshal to a generic value,
   re-marshal with `json.Marshal` (Go sorts object keys
   lexicographically by default), and strip insignificant
   whitespace. The fixture file is stored in the same canonical
   form so a diff fails on semantic change, not on encoder
   formatting drift. A single helper, `quirkstest.AssertWireEqual`,
   encapsulates the pass so every adapter's tests use the same
   comparison.

### 7.2 Negative tests

For each shipped rule, a parallel negative test confirms a sibling
model that should NOT match the rule produces the default wire shape.
The set is enumerated explicitly:

- `gpt-4o` does NOT inherit `applyOpenAIReasoningClass`.
- `gpt-5-chat-latest` does NOT carry the reasoning-class omissions
  (the `gpt-5-chat*` carve-out clears them; the test pins this).
- `gpt-5-mini` DOES carry the full omission set.
- `gemini-2.5-pro` does NOT match the `gemini-3*` `ReplayFields`
  rule.
- `deepseek-chat` (non-thinking) does NOT carry the
  `reasoning_content` `ReplayFields` entry.

A `TestRuleCarveOuts` table-driven test pins all the carve-out cases
so a rule re-ordering or `path.Match` semantics change cannot
silently broaden a rule's reach.

### 7.3 Replay safety

The `ReplayProvider`
([`harness/internal/provider/replay.go`](../harness/internal/provider/replay.go))
emits stream events from recorded turns and has no wire-format
concept. Quirks do not affect replay. The existing eval suites stay
deterministic.

`provider.quirk.applied` is captured into the recorded
`RunTrace` at the trace-emitter layer (not the provider layer), so
replays preserve the attribute even though the replay provider
bypasses quirk resolution entirely. A drift-detection eval that
compares a replayed run to a fresh live run can therefore still
flag a registry change as an attribute-set diff.

### 7.4 Glob safety

`TestNoMetacharsInKnownModelIDs` exercises every model identifier
from a catalogue file (`testdata/model-ids.txt`) against the
`path.Match` grammar's metacharacter set (`*`, `?`, `[`, `]`, `\`).
The catalogue includes:

- Hosted: OpenAI, Azure OpenAI, Anthropic, Vertex AI, Bedrock,
  Mistral, Groq, Together, Fireworks, DeepSeek, Cerebras,
  Perplexity, Cohere, xAI, OpenRouter (sampled).
- Self-hosted echoes: Ollama tags (`llama3.3:70b-instruct`,
  `qwen2.5-coder:32b`), LM Studio variant qualifiers
  (`google/gemma-3-12b@q3_k_l`), vLLM and llama.cpp HF-style
  identifiers, LiteLLM proxy prefixes.

The catalogue is refreshed quarterly; the test fails CI when a new
identifier introduces a metacharacter, forcing a design conversation
rather than a silent escaping pass.

### 7.5 End-to-end smoke

The Azure OpenAI smoke workflow tracked in [issue #160] gains a
second matrix row pinning a deployment of an o-series or GPT-5
model. The smoke test is the integration-side confirmation that the
matched rule produces a body the upstream gateway accepts.

[issue #160]: https://github.com/rxbynerd/stirrup/issues/160

### 7.6 Staleness signal

A `TestRuleStaleness` test enumerates every `BuiltinRules` entry
and asserts `LastVerified` is set (non-zero). A separate
`TestRuleStalenessWarning` invokes the staleness-check helper and
confirms a synthetic rule dated 200 days ago produces the expected
debug log line. The 180-day threshold is a constant in
`harness/internal/provider/quirks.go`; staleness is a maintenance
signal, not a correctness gate, so the test does not fail on real
stale rules — it pins the warning mechanism.

## 8. Observability

Quirks are observable through three channels:

- **Span attributes.** The existing `provider.stream` span gains
  `provider.quirk.applied` as a slice attribute carrying the
  `Description` of every rule that contributed. Empty when no rule
  matched (the common case for models without divergences). The
  attribute is consumed today by the OTLP exporters and the JSONL
  emitter; the `runs replay` rendering reads slice attributes via
  the standard OTel SDK so no replay-side changes are needed.
- **Structured log line.** At debug level, a single line at the
  top of each `Stream` call:
  `provider quirks resolved provider=openai-compatible model=o1-mini rules=[...]`.
  A separate `warn`-level line fires when any matched rule's
  `LastVerified` is older than 180 days.
- **CLI introspection.** `stirrup providers quirks --provider X --model Y`
  prints the resolved `ProviderQuirks` as JSON for human inspection,
  including the description, `LastVerified` date, and staleness
  status of every rule that contributed.

The trace attribute is the load-bearing one: every recorded run can
be replayed against the resolved quirks, and a regression in the
registry surfaces as a diff against the trace.

## 9. Rollout / migration

The registry and `ProviderQuirks` type can land without changing any
adapter's external behaviour. The rollout is a sequence of small,
independently mergeable waves:

### Wave 1 — Scaffolding (no behaviour change)

- Land `harness/internal/provider/quirks.go` with the `Rule`,
  `Registry`, `ProviderQuirks`, and `Value` types, including the
  `EnumCoercions` and `ReplayFields` fields and the `LastVerified`
  field on `Rule`.
- Land `harness/internal/provider/quirks_helpers.go` with
  `applyOpenAIReasoningClass`, `removeFromOmit`, and `Date`.
- Land an empty default registry and the `BuiltinRules` constructor.
- Land the introspection subcommand at
  `harness/cmd/stirrup/cmd/providers_quirks.go`. With an empty
  registry it always reports "no rules apply" — useful as the smoke
  test that the subcommand is wired.
- Add `provider.quirk.applied` to the trace schema in
  [`docs/architecture.md`](./architecture.md).

### Wave 2 — OpenAI Chat Completions integration (smallest, breaks first)

- Rewire `openai.go`'s `openaiRequest` marshalling to consult
  `ProviderQuirks`. The default-rule registry still produces today's
  exact bytes; this is enforced by golden-file tests on every
  existing supported model.
- Land the `o[1-9]-*`, `gpt-5*`, and `gpt-5-chat*` rules from
  section 6.1 with captured fixtures, including the carve-out
  negative tests in §7.2.
- Expand the Azure smoke workflow ([issue #160]) to exercise an
  o-series deployment.

### Wave 3 — Gemini integration

- Rewire `gemini_request.go` to consult `StructuralFlags`.
  Defaults preserve today's "stream args off" behaviour.
- Land the default `gemini` rule from section 6.2.
- Captured fixtures pin the post-#191 wire shape for `gemini-2.5-pro`
  and `gemini-3.1-pro-preview` and confirm they are byte-identical to
  the pre-quirks output.

### Wave 4 — OpenAI Responses integration

Minimal in v1 — the `output`/`text` required-key invariants stay in
the struct tags. The Responses adapter still gets `ProviderQuirks`
plumbing so future Responses-side divergences (Azure Foundry's
content-part lifecycle events, server-side state events that some
gateways inject) can be addressed without re-plumbing the adapter.

### Wave 5 — Replay-fields recognition (parse-side)

- Land the response-parse hook in `openai.go` and `gemini.go` that
  preserves `ReplayFields` paths onto the assistant-message struct.
- Land the §6.5 rules for `deepseek-reasoner*`, `deepseek-v4*`, and
  `gemini-3*`.
- Captured `replay.json` fixtures pin the preserved-field shape.

The conversation-history-builder outbound threading is a separate
follow-up PR (tracked alongside [#191]) and not part of v1.

### Wave 6 — Documentation pass

- [`docs/providers.md`](./providers.md) cross-references this
  document.
- [`docs/configuration.md`](./configuration.md) notes that
  per-model wire-shape decisions are registry-driven and not
  operator-configurable.
- A short worked example in this document, walking through
  `o1-mini` → resolved quirks → wire body, for a future
  contributor adding their first rule.

### Future (not in v1) — Optional auto-detection

Sketch only; not scheduled for v1. Listed here so future contributors
can see the seam left for it.

- An opt-in `provider.quirkDiscovery` flag on `ProviderConfig` that
  enables a single-shot retry on a parseable 400 from a known
  gateway. Discovery hits are reported as `provider.quirk.discovered`
  span events with the inferred rule, so operators see drift before
  it becomes a registered rule.
- The retry is bounded to one attempt; cost-doubling for the
  remainder of the run is unacceptable.
- The inferred quirk is NOT cached across runs (every run does its
  own discovery) — durable rules belong in the registry, not in
  per-run state. The signal is what the discovery report carries.

## 10. Risks and open questions

### Risks

- **Registry as a god object.** Mitigated by the typed-canonical-field
  contract (section 3.3): adapters declare their canonical surface,
  rules can only touch declared fields. Tests panic on unknown keys.
- **Silent over-application.** The introspection subcommand and the
  span attribute give operators a clear "what did the harness send?"
  view; the rules are short and ordered, so misapplication is easy
  to diagnose from the trace alone.
- **Model-name churn.** OpenAI and Vertex have both renamed models
  mid-life. A rule keyed on `gpt-5*` survives the rename from
  `gpt-5-nano-2025-10-01` to `gpt-5-nano-stable`; a rule keyed on
  the dated suffix does not. Rules should match the longest stable
  prefix per provider; the choice is a per-rule judgment. The
  `LastVerified` field surfaces stale rules for review before they
  break, but is not a substitute for prefix discipline.
- **Replay-fields half-implementation gap.** Wave 5 lands the
  parse-side recognition but not the outbound threading. Multi-turn
  runs against `deepseek-v4*` and `gemini-3*` will continue to fail
  on turn 2 until the [#191] follow-up lands. The risk is operators
  seeing `provider.quirk.applied` in their trace and assuming the
  full fix has shipped. Mitigation: the rule descriptions in §6.5
  end with `(parse-side only)` and the docs cross-reference [#191].
- **Carve-out fragility.** The `gpt-5-chat*` carve-out in §6.1
  depends on specificity ordering AND on the broader `gpt-5*` rule
  having populated `OmitFields` before it runs. A reordering of
  `BuiltinRules` would break it silently if not for the negative
  test in §7.2. The negative test is therefore load-bearing, not
  documentary.
- **Enum coercion as silent breakage.** A rule that adds an
  `EnumCoercions` entry can effectively drop a caller's parameter
  (when the inner table has no match). The registry self-test
  warns on this pattern but operators can still be surprised.
  Mitigation: the resolved quirks include the coercion table, so
  the introspection subcommand surfaces "your `medium` will be
  sent as `high`" directly.

### Open questions

These are intentionally surfaced for the implementation review.

1. **Glob vs prefix-with-version-suffix.** `path.Match` is the v1
   pick. The §7.4 catalogue test pins the assumption against current
   identifiers; confirm reviewers are satisfied with the catalogue's
   coverage of self-hosted echoes (Ollama, LM Studio, vLLM) before
   merging Wave 1.

2. **Order vs first-match.** v1 chooses specificity-then-declaration-
   order. The alternative is first-match-wins. The former composes
   cleanly for "provider-wide default + model-specific overlay"
   patterns (which the `gpt-5*` / `gpt-5-chat*` carve-out exercises);
   the latter is easier to reason about. v1's choice can flip if the
   rule-set ergonomics demand it.

3. **Per-stream vs per-adapter resolution.** v1 picks per-stream
   (section 4) for dynamic-router correctness. Confirm with a
   micro-benchmark that the cost is genuinely lost in the noise of
   the SSE read.

4. **Where the canonical-field constants live.** Section 3.3 sketches
   per-adapter constants. An alternative is a single
   `harness/internal/provider/quirks/fields.go` package consolidating
   every adapter's canonical fields. v1 keeps them per-adapter; the
   alternative is easier to audit but adds an import cycle.

5. **Bedrock.** Section 2 explicitly defers Bedrock (Converse API).
   Confirm with reviewers that the SDK abstraction genuinely covers
   the model-family divergences known today (Claude-on-Bedrock vs
   Llama-3 vs Mistral). If the SDK leaks shape variations through
   `additional_model_request_fields`, Bedrock joins the registry.
   Bedrock's separate OpenAI-compatible endpoint
   (`bedrock-runtime.*.amazonaws.com/openai/v1/`) is reached via
   the openai-compatible adapter and is in scope; rules for it land
   once `BaseURLMatch` (§3.2.2) is implemented.

6. **Eval-suite implications.** [`stirrup-eval`](./eval.md)
   replays recorded runs and does not need quirks. But a *live* eval
   run (one that hits a real provider) should produce identical
   resolved quirks to the production run it's evaluating, otherwise
   drift detection becomes self-poisoning. Confirm the eval CLI
   threads the same registry; v1 implementation must not fork.

7. **Publisher-prefix normalisation for Bedrock and Vertex.** When
   Bedrock joins the registry, model ids will carry publisher
   prefixes (`anthropic.claude-sonnet-4-5-v1:0`,
   `meta.llama3-3-70b-instruct-v1:0`). Vertex's full model identifier
   is already path-segmented
   (`publishers/google/models/gemini-3.1-pro-preview`) but the
   adapter already strips it to the bare model name before calling
   `Stream`, so the registry sees `gemini-3.1-pro-preview`. A
   normalisation pass (`stripPublisherPrefix(provider, model)`) is
   the cleanest fix; document it as part of the Bedrock rollout
   wave when that lands. Flagged by the prior-art survey as an
   issue every multi-provider router has had to solve.

8. **Response-side parse divergences beyond Gemini 3.x.** The plan
   provisions `StructuralFlags` for parse-path branching, and
   `ReplayFields` for opaque-state preservation. A fixture-capture
   pass before Wave 3 should confirm there are no latent
   divergences the plan hasn't accounted for: Claude 4.x SSE events,
   OpenAI Responses content-part lifecycle additions, Azure
   Foundry SSE extensions, DeepSeek's `insufficient_system_resource`
   `finish_reason` value, Perplexity's citation tokens in usage,
   llama.cpp's `timings` block.

9. **`BaseURLMatch` design seam.** The §3.2.2 sketch needs reviewer
   sign-off on the specificity formula (`len(ModelMatch) +
   len(BaseURLMatch)`) before v1 lands, since it pins the v2 PR's
   composition semantics. The alternative (lexical: `BaseURLMatch`
   beats `ModelMatch` ties) is simpler but breaks the "longer = more
   specific" mental model that §3.2.1 establishes.

## 11. Worked example

Walking the OpenAI o-series case end-to-end:

```
RunConfig {
  Provider: { Type: "openai-compatible", BaseURL: "https://api.openai.com/v1" },
  ModelRouter: { Type: "static", Model: "o1-mini" },
  ...
}
```

1. Factory constructs `OpenAICompatibleAdapter`.
2. The agentic loop calls `Stream(ctx, StreamParams{Model: "o1-mini", ...})`.
3. The adapter calls `registry.Resolve("openai-compatible", "o1-mini")`.
4. The registry walks its rules. The rule
   `{ProviderType: "openai-compatible", ModelMatch: "o[1-9]-*"}` matches;
   its `Apply` (via `applyOpenAIReasoningClass`) sets:
   - `q.FieldRenames["max_tokens"] = "max_completion_tokens"`
   - `q.OmitFields = ["temperature", "top_p", "presence_penalty",
     "frequency_penalty", "logprobs", "top_logprobs", "logit_bias"]`
5. The adapter's marshaller emits:
   ```json
   {
     "model": "o1-mini",
     "messages": [...],
     "max_completion_tokens": 4096,
     "stream": true
   }
   ```
   (No sampling parameters; `max_completion_tokens` instead of
   `max_tokens`.)
6. The `provider.stream` span carries
   `provider.quirk.applied = ["OpenAI o-series: reasoning-class param restrictions"]`.
7. An operator running
   `stirrup providers quirks --provider openai-compatible --model o1-mini`
   sees the same description, `LastVerified: 2026-05-01`, and the
   resolved `ProviderQuirks` JSON.

## 12. Implementation summary

- New package contents:
  - `harness/internal/provider/quirks.go` — types, `Registry`,
    `BuiltinRules`, `Date` helper.
  - `harness/internal/provider/quirks_helpers.go` —
    `applyOpenAIReasoningClass`, `removeFromOmit`, future helpers.
- Modified files: `openai.go`, `openai_responses.go`, `gemini.go`,
  `gemini_request.go` — each gains a per-stream
  `registry.Resolve` call and threads `ProviderQuirks` through its
  marshaller. `openai.go` and `gemini.go` also gain a `ReplayFields`
  parse hook in Wave 5.
- New CLI subcommand: `harness/cmd/stirrup/cmd/providers_quirks.go`.
- New test fixtures: `harness/internal/provider/testdata/quirks/`
  and `harness/internal/provider/testdata/model-ids.txt`.
- Documentation: this file, plus updates to
  [`docs/providers.md`](./providers.md),
  [`docs/configuration.md`](./configuration.md), and the trace-schema
  section of [`docs/architecture.md`](./architecture.md).

The rollout is sequenced so each wave is mergeable independently and
preserves today's wire output until the rules that change it land.
