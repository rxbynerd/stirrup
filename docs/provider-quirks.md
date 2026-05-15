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
| `openai-compatible` | OpenAI o-series; Azure OpenAI deployments backed by newer reasoning models | `max_tokens` rejected, `max_completion_tokens` required | **Broken** |
| `openai-compatible` | OpenAI reasoning models | `temperature` rejected on some o-series deployments | Suspected |
| `openai-responses` | Various Azure Foundry deployments | Required-key rules for empty `output` / `text` fields | Resolved per-case in [#172] / [#176] |

[#191]: https://github.com/rxbynerd/stirrup/pull/191
[#172]: https://github.com/rxbynerd/stirrup/issues/172
[#176]: https://github.com/rxbynerd/stirrup/issues/176

The generalised shape: provider P serves models M1 and M2 under one
adapter wire shape S. M1 accepts S; M2 rejects S or interprets S
differently. The harness's only existing knobs (`--query-param`,
`--api-key-header`, `--base-url`) live at the URL/header layer; they
do not address body shape, field renames, conditional field omission,
or value reshaping.

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
  rewrite spec; everything is implicit per upstream.

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
   on `RunConfig`.
3. Coverage of the **field-rename**, **conditional-omission**, and
   **value-override** categories — the 80% case the OpenAI Chat
   Completions divergences fall into.
4. A typed-capability flag surface for adapter-internal structural
   branching — the 20% case the Gemini 3.x streaming-deltas divergence
   falls into.
5. Forward-compatible defaults: a model name unmatched by any rule
   inherits the latest known shape for its provider type, not the
   legacy one. Explicit fallback rules carry older model families
   forward.
6. Every applied quirk is observable: emitted as span attributes on
   the existing `provider.stream` span and as a structured log line
   at debug level.
7. Each registry entry is paired with captured wire fixtures
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
- **Bedrock.** The `bedrock` adapter speaks through the AWS SDK's
  Converse API, which already abstracts model-family wire
  differences. Bedrock-family divergences (e.g. Llama-3 vs Claude-on-
  Bedrock stop-reason semantics) are out of scope here and would be
  addressed by extending the stop-reason mapping path in
  [`harness/internal/provider/bedrock.go`](../harness/internal/provider/bedrock.go),
  not by the registry. The registry's interface is provider-
  agnostic, so Bedrock can plug in later without redesign.
- **`thoughtSignature` round-trip.** Tracked separately as a follow-up
  to [#191].
- **Multimodal content shape variations.** Tracked under
  [issue #103]; multimodal blocks have their own per-provider
  serialisation concerns that overlap but are largely additive to the
  quirks surface.

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
    // OpenAI o-series rejects `temperature` entirely.
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

    // StructuralFlags carries adapter-specific flags for divergences
    // that cannot be expressed as flat field operations. The flag set
    // is closed and typed per adapter (e.g. `openai.StructuralFlags`,
    // `gemini.StructuralFlags`) so the compiler enforces that an
    // adapter only ever reads flags it knows about.
    //
    // Example: Gemini's StructuralFlags carries
    // `StreamFunctionCallArgsShape ∈ {Off, V2Snapshot, V3Deltas}`.
    // The default (`Off`) preserves the post-#191 production
    // behaviour.
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
- `o1-*` matches `o1-mini` and `o1-preview`, but NOT bare `o1` (the
  trailing hyphen is literal).
- `gpt-4*` matches `gpt-4`, `gpt-4o`, `gpt-4o-mini`.

Regex was considered and rejected: glob covers every realistic case,
the trace attribute is more readable, and there is no need for the
expressive power of capture groups or character classes.

`path.Match`'s metacharacters are `*`, `?`, `[`, `]`, and `\`. A
survey of current OpenAI, Anthropic, Vertex AI, Bedrock, and
Mistral catalogue identifiers shows none contains any of these
characters; v1 assumes that property and the registry validates it
at startup via `TestNoMetacharsInKnownModelIDs`. A future identifier
that breaks the assumption forces a switch to literal-then-glob
escaping, not a redesign.

### 3.2.1 Rule composition: longest-pattern wins, then declaration order

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

### 3.3 Adapter integration

Each adapter exposes a small, typed surface that the registry
populates. The intent is that the registry never knows the JSON keys
an adapter ultimately emits; it speaks only in canonical names the
adapter package owns.

```go
// In harness/internal/provider/openai_quirks.go (illustrative)
type openaiCanonicalField string

const (
    fieldMaxTokens     openaiCanonicalField = "max_tokens"
    fieldTemperature   openaiCanonicalField = "temperature"
    fieldStream        openaiCanonicalField = "stream"
    // ... etc
)

// openaiStructuralFlags is the typed flag set the Chat Completions
// adapter reads off ProviderQuirks.StructuralFlags. v1 is empty;
// future Chat Completions divergences declare flags here.
type openaiStructuralFlags struct {
    // (none in v1; the openai-compatible divergences known today are
    // all expressible as FieldRenames / OmitFields.)
}
```

The adapter's request-marshalling path becomes:

1. `quirks := registry.Resolve(providerType, params.Model)` at the
   top of each `Stream` call. Section 4 explains why per-stream
   rather than per-adapter.
2. The marshaller asks `quirks` for the wire key of each canonical
   field, the omission set, and the value-override set, then emits
   the body accordingly.
3. The marshaller branches on `StructuralFlags` for any divergence
   it has been written to honour.
4. The same `quirks` value is captured into the SSE-reader closure
   so the parse path consults the same source of truth as the send
   path (the Codec invariant; see 3.3.1).

For Gemini, the same pattern applies; `geminiStructuralFlags` would
contain `StreamFunctionCallArgsShape` and the adapter would key off
that when constructing the `toolConfig.functionCallingConfig` block.

### 3.3.1 The Codec invariant

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
   description of every rule that contributed. The subcommand is
   side-effect-free and reads only the in-memory registry. It exists
   so an operator hitting an unexpected 400 from an upstream gateway
   can confirm what shape the harness was sending without enabling
   debug logs.

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
(e.g. only `OmitFields` and `FieldRenames`; no `StructuralFlags` or
`ValueOverrides`), and every applied override should fire a
`security.SecurityLogger` event so audit trails can flag custom
wire-shape adjustments.

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
| Field rename | `FieldRenames` map | OpenAI `max_tokens` → `max_completion_tokens` for `o1-*`, `o3-*`, `gpt-5*` |
| Conditional omission | `OmitFields` slice | OpenAI o-series rejects `temperature`; omit when matched |
| Value override | `ValueOverrides` map | A model that accepts `temperature` only when it equals 1.0 |
| Structural shape | typed `StructuralFlags` per adapter | Gemini 3.x `streamFunctionCallArguments` deltas (currently set to "off" universally; the flag exists so a future reinstatement can be model-scoped) |
| Response parsing | adapter-side branch on `StructuralFlags` | Gemini 3.x `partialArgs` array vs 2.x snapshot |

Out of v1 scope (deferred to follow-up issues):

- Cross-cutting metadata renames (e.g. `usage.completion_tokens` →
  `usage.output_tokens`). The parse-side rename surface is more
  invasive than the send side and warrants its own design pass once
  a concrete case lands.
- Authentication-shape divergences (already handled by the existing
  `APIKeyHeader` / `Credential` surface; out of registry scope).
- Endpoint path divergences (e.g. moving from
  `/v1/chat/completions` to `/v2/...`). `BaseURL` already covers
  this; the registry should not.

## 6. Concrete cases addressed at v1 ship

Each entry below is a rule that lands in the v1 registry. Every rule
ships with one or more captured wire fixtures and a regression test.

### 6.1 OpenAI Chat Completions: `max_tokens` → `max_completion_tokens`

```go
// inside BuiltinRules() []Rule { return []Rule{
{
    ProviderType: "openai-compatible",
    ModelMatch:   "o1-*",
    Description:  "OpenAI o1 family requires max_completion_tokens",
    Apply: func(q *ProviderQuirks) {
        q.FieldRenames["max_tokens"] = "max_completion_tokens"
        q.OmitFields = append(q.OmitFields, "temperature")
    },
},
{
    ProviderType: "openai-compatible",
    ModelMatch:   "o3-*",
    Description:  "OpenAI o3 family requires max_completion_tokens",
    Apply: func(q *ProviderQuirks) {
        q.FieldRenames["max_tokens"] = "max_completion_tokens"
        q.OmitFields = append(q.OmitFields, "temperature")
    },
},
{
    ProviderType: "openai-compatible",
    ModelMatch:   "gpt-5*",
    Description:  "GPT-5 family requires max_completion_tokens",
    Apply: func(q *ProviderQuirks) {
        q.FieldRenames["max_tokens"] = "max_completion_tokens"
    },
},
// }}
```

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

### 6.2 Gemini 3.x: streamed function-call argument shape

The current state is "flag globally off". The registry codifies that
and gives a future re-enablement a model-scoped seam:

```go
// inside BuiltinRules() []Rule { return []Rule{
{
    ProviderType: "gemini",
    ModelMatch:   "*",
    Description:  "Default: do not stream function-call arguments",
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

### 6.4 OpenAI Chat Completions: `temperature` rejection on o-series

Covered by the rules in 6.1 (the `OmitFields` clause). When the
o-series rule is matched, `temperature` is dropped from the request
body. `StreamParams.Temperature` retains its value for tracing; the
omission happens at marshal time.

## 7. Validation and testing

### 7.1 Captured wire fixtures

Each rule lands with at least one captured wire fixture stored under
`harness/internal/provider/testdata/quirks/<provider>/<model>/`:

- `request.json` — the body the harness produces under the rule.
- `response.sse` — a recorded successful SSE response from the
  upstream gateway, or a synthetic equivalent when the rule covers a
  response-parse path. Synthetic fixtures must include a comment
  explaining their derivation.

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
E.g. `gpt-4o` does NOT take the `max_completion_tokens` rule.

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

### 7.4 End-to-end smoke

The Azure OpenAI smoke workflow tracked in [issue #160] gains a
second matrix row pinning a deployment of an o-series or GPT-5
model. The smoke test is the integration-side confirmation that the
matched rule produces a body the upstream gateway accepts.

[issue #160]: https://github.com/rxbynerd/stirrup/issues/160

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
  `provider quirks resolved provider=openai-compatible model=o1-mini rules=[...]`
- **CLI introspection.** `stirrup providers quirks --provider X --model Y`
  prints the resolved `ProviderQuirks` as JSON for human inspection.

The trace attribute is the load-bearing one: every recorded run can
be replayed against the resolved quirks, and a regression in the
registry surfaces as a diff against the trace.

## 9. Rollout / migration

The registry and `ProviderQuirks` type can land without changing any
adapter's external behaviour. The rollout is a sequence of small,
independently mergeable waves:

### Wave 1 — Scaffolding (no behaviour change)

- Land `harness/internal/provider/quirks.go` with the `Rule`,
  `Registry`, `ProviderQuirks`, and `Value` types.
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
- Land the `o1-*`, `o3-*`, `gpt-5*` rules from section 6.1 with
  captured fixtures.
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

### Wave 5 — Documentation pass

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
  prefix per provider; the choice is a per-rule judgment.
- **Gateway-shaped divergences.** Azure, OpenRouter, LiteLLM, and
  vLLM each add their own variants. The registry keys on
  `(provider-type, model)` only; a gateway that breaks the
  upstream's shape today does not have a clean expression unless a
  distinct `provider.type` is configured for it. v1 accepts this
  limitation; a future revision could extend rules with a
  `BaseURLMatch` predicate.

### Open questions

These are intentionally surfaced for the implementation review.

1. **Glob vs prefix-with-version-suffix.** `path.Match` is the v1
   pick. Are there real-world model names with characters
   `path.Match` mistreats (e.g. embedded `[`, `]`)? Survey of
   OpenAI / Azure / Vertex / OpenRouter catalogues suggests no, but
   pinning a test that exercises every known model name against the
   matcher would catch regressions early.

2. **Order vs first-match.** v1 chooses all-matches-apply-in-order.
   The alternative is first-match-wins. The former composes cleanly
   for "provider-wide default + model-specific overlay" patterns;
   the latter is easier to reason about. A real-world rule set of
   ten entries should make the choice obvious; v1's choice can flip
   if the rule-set ergonomics demand it.

3. **Per-stream vs per-adapter resolution.** v1 picks per-stream
   (section 4) for dynamic-router correctness. Confirm with a
   micro-benchmark that the cost is genuinely lost in the noise of
   the SSE read.

4. **Where the canonical-field constants live.** Section 3.3 sketches
   per-adapter constants. An alternative is a single
   `harness/internal/provider/quirks/fields.go` package consolidating
   every adapter's canonical fields. v1 keeps them per-adapter; the
   alternative is easier to audit but adds an import cycle.

5. **Bedrock.** Section 2 explicitly defers Bedrock. Confirm with
   reviewers that the SDK abstraction genuinely covers the
   model-family divergences known today (Claude-on-Bedrock vs
   Llama-3 vs Mistral). If the SDK leaks shape variations through
   `additional_model_request_fields`, Bedrock joins the registry.

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
   provisions `StructuralFlags` for parse-path branching, but the
   only known case today is Gemini 3.x. Survey the other adapters'
   response shapes against their newest models (Claude 4.x SSE
   events, OpenAI Responses content-part lifecycle additions, Azure
   Foundry SSE extensions) to confirm there are no latent
   divergences the plan hasn't accounted for. A quick fixture
   capture pass before Wave 3 would be cheap insurance.

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
   `{ProviderType: "openai-compatible", ModelMatch: "o1-*"}` matches;
   its `Apply` sets:
   - `q.FieldRenames["max_tokens"] = "max_completion_tokens"`
   - `q.OmitFields = ["temperature"]`
5. The adapter's marshaller emits:
   ```json
   {
     "model": "o1-mini",
     "messages": [...],
     "max_completion_tokens": 4096,
     "stream": true
   }
   ```
   (No `temperature`; `max_completion_tokens` instead of `max_tokens`.)
6. The `provider.stream` span carries
   `provider.quirk.applied = ["OpenAI o1 family requires max_completion_tokens"]`.
7. An operator running
   `stirrup providers quirks --provider openai-compatible --model o1-mini`
   sees the same description plus the resolved `ProviderQuirks` JSON.

## 12. Implementation summary

- New package contents: `harness/internal/provider/quirks.go`
  (types + registry + `BuiltinRules`).
- Modified files: `openai.go`, `openai_responses.go`, `gemini.go`,
  `gemini_request.go` — each gains a per-stream
  `registry.Resolve` call and threads `ProviderQuirks` through its
  marshaller.
- New CLI subcommand: `harness/cmd/stirrup/cmd/providers_quirks.go`.
- New test fixtures: `harness/internal/provider/testdata/quirks/`.
- Documentation: this file, plus updates to
  [`docs/providers.md`](./providers.md),
  [`docs/configuration.md`](./configuration.md), and the trace-schema
  section of [`docs/architecture.md`](./architecture.md).

The rollout is sequenced so each wave is mergeable independently and
preserves today's wire output until the rules that change it land.
