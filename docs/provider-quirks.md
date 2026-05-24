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
  proprietary `tool_stream: true` extension.
- Vertex AI's Gemini 3.x stream emits a `thoughtSignature` blob on
  every `parts[]` entry that the harness must preserve verbatim for
  multi-turn reasoning continuity. Issue #194 traced the symptom; the
  fix is a registry-driven `ReplayFields` capture.
- DeepSeek's reasoner and v4 families surface chain-of-thought
  through a `reasoning_content` sibling field on the assistant
  delta. Parse-side recognition lets the harness expose the field
  in traces; outbound threading (round-tripping it on the next turn)
  is a deferred follow-up.

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
        zai.go      — CompatRule() returning the Z.ai GLM rule
        zai_test.go — httptest contract test
```

Fixture trees for the wire-shape contract tests:

```
harness/internal/provider/testdata/quirks/
    openai-compatible/<model>/{request.json, response.sse}
    openai-compatible/<deepseek-model>/{response.sse, replay.json}
    gemini/<model>/{request.json, response.sse[, replay.json]}
```

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
    ReplayFields    []string                  // assistant-message paths to preserve (parse-side only in v1)

    // Behaviour flags (per-adapter typed sub-structs).
    BehaviourFlags  ProviderBehaviourFlags
}

type ProviderBehaviourFlags struct {
    OpenAI OpenAIBehaviourFlags
    Gemini GeminiBehaviourFlags
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
```

The typed per-adapter sub-struct is the design choice that replaces
PR #196's original `StructuralFlags any`: it preserves compile-time
type safety and removes the runtime type-assertion every adapter
would otherwise need.

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
| `openai-compatible` | `deepseek-reasoner*` | DeepSeek reasoner: preserve `reasoning_content` (parse-side only)  |
| `openai-compatible` | `deepseek-v4*`     | DeepSeek v4: preserve `reasoning_content` (parse-side only)          |
| `gemini`            | `*`                | Gemini: off `streamFunctionCallArguments` (post-#191 default)        |
| `gemini`            | `gemini-3*`        | Gemini 3: preserve `thoughtSignature` on `functionCall` parts (parse-side only) |

The compat profile rule for Z.ai GLM is registered separately via
`harness/internal/provider/compat/zai/CompatRule()`; it is not in
`BuiltinRules()` and only loads when `provider.compatProfile =
"zai-glm"` is set.

### 3.1 ReplayFields rules (parse-side only)

The three `ReplayFields` rules added in Wave 2 land **parse-side
recognition only**. Captured values surface in a per-stream debug
log so operators can see the rule fired, but the values are not yet
threaded back into outbound message history. Multi-turn runs against
affected models continue to fail on turn 2 in the same way they did
before the rule; the observable improvement is the debug attribute.

Outbound threading is design §9 risk 7 and is tracked separately.
The Description of every ReplayFields rule ends in `(parse-side
only)` so trace consumers know the captured value is observable but
not round-tripped. `TestBuiltinRulesParseSideOnlySuffix` enforces
the convention.

The captured-fields debug log line is `quirks replay fields
captured` at slog DEBUG level. It records a per-path summary of
`{count, total_len}` only — captured values themselves are
provider-private (DeepSeek reasoning_content, Gemini
thoughtSignature) and never appear in log sinks.
`TestReplayFields_DeepSeekReasoner_LogIsLengthOnly` pins the
side-channel guard.

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

## 5. Operator surface

| Surface | How | Notes |
|---|---|---|
| `provider.compatProfile` on `ProviderConfig` | `RunConfig` string field | Closed enum. Only legal value in v1: `"zai-glm"`. Unknown values fail at startup via `ValidateRunConfig`. |
| `stirrup providers quirks --provider X --model Y` | CLI subcommand | Prints resolved `ProviderQuirks` as JSON, plus `Description`, `LastVerified`, staleness flag of every contributing rule. Side-effect-free. |
| `provider quirks resolved` / `gemini quirks resolved` slog DEBUG line | structured log | One line per Stream call, listing every contributing rule's Description in apply order. Last entry is the rule whose writes won on overlapping fields. Emitted even when no rule fired (rules:[]) so a missing line unambiguously means "the resolution did not run". |
| `quirks replay fields captured` slog DEBUG line | structured log | Per-stream summary of `{count, total_len}` per captured ReplayFields path, emitted on stream exit. Length-only — captured values themselves are not logged. |
| `openai quirks suppressed caller temperature` slog WARN line | structured log | Fires when `OmitSamplingParams` suppresses a caller-supplied non-nil `Temperature`. Names the rule that caused the suppression. The suppressed value itself is not logged. |

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

Z.ai/GLM is a compatibility-only target. The rule lives at
`harness/internal/provider/compat/zai/zai.go` and exports a single
`CompatRule()` function. The factory injects it into the registry
when `provider.compatProfile = "zai-glm"` is set.

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

- **Tool-contract metadata (#222).** Provider-facing tool metadata
  (strict mode, tool-choice policy, parallel-call policy, input
  examples, annotations) is a property of `types.ToolDefinition`,
  keyed by tool name — not by (provider, model). The two
  cardinalities are different. When #222 lands, each adapter's
  `translateTools()` will gain the mapping; whether a model supports
  a given tool feature (e.g. `strict: true`) is a one-line check
  against the resolved `ProviderQuirks` (likely a `SupportsStrictMode
  bool` flag on `OpenAIBehaviourFlags`). This is a small extension to
  the quirks shape, not a redesign.
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

7. **ReplayFields threading gap.** Wave 2 lands parse-side
   recognition only. Operators seeing `provider.quirk.applied:
   ["Gemini 3: preserve thought_signature on functionCall parts
   (parse-side only)"]` in their trace must not assume the threading
   is complete. The `(parse-side only)` suffix on every ReplayFields
   rule's `Description` is the load-bearing observability signal;
   `TestBuiltinRulesParseSideOnlySuffix` enforces it. Outbound
   threading is a follow-up.

8. **`DefaultRegistry()` singleton and test isolation.** The
   singleton is constructed once via `sync.Once` and is read-only
   after construction. Tests that need a custom rule set use
   `NewRegistry` and inject via the adapter's exported `Registry`
   field. `TestDefaultRegistryConcurrentAccess` stresses the
   singleton under `-race`.

9. **DeepSeek field-path verification gap.** The `reasoning_content`
   path used by the DeepSeek-reasoner and DeepSeek-v4 rules is
   sourced from the DeepSeek API documentation, not from a live
   capture. If a live capture surfaces a different shape (e.g. a
   structured `thinking` object rather than a flat string), the
   rule's path and the corresponding `replay.json` fixture need
   to be adjusted in lockstep. The 180-day staleness window flags
   the rule for re-verification.
