// Package quirks implements the per-(provider, model) wire-shape and
// behaviour override registry. Adapters call Registry.Resolve at the
// top of each Stream call to get a ProviderQuirks for the request.
package quirks

// ProviderQuirks is the in-memory result of resolving the registry for a
// (providerType, model) pair. Adapters read it when building a request and
// (for paths that diverge) when interpreting a response.
//
// Registry.Resolve always returns a ProviderQuirks with all map fields
// pre-initialised to empty non-nil maps and all slice fields to empty
// non-nil slices, so Apply functions may write freely without nil checks.
// A zero-value ProviderQuirks constructed outside the registry is NOT safe
// to mutate.
type ProviderQuirks struct {
	// --- Wire-shape overrides ---

	// FieldRenames maps canonical adapter-internal field name to the wire
	// JSON key the request should emit. Empty key means "use canonical name".
	// Adapters validate that every key is in their declared canonical set at
	// registry build; unknown keys panic via TestBuiltinRulesValidate.
	FieldRenames map[string]string `json:"fieldRenames"`

	// OmitFields lists canonical fields the adapter MUST NOT emit, even when
	// non-zero. Applied after ValueOverrides (omission wins).
	OmitFields []string `json:"omitFields"`

	// ValueOverrides forces a canonical field's serialised value, ignoring
	// StreamParams. Applied before OmitFields.
	ValueOverrides map[string]Value `json:"valueOverrides"`

	// EnumCoercions maps canonical field name → (caller-value → wire-value).
	// A present outer key with no inner match means the caller's value is
	// unsupported and the field is dropped (equivalent to OmitFields for that
	// value). v1 ships no rules; the surface is declared and tested with
	// synthetic rules.
	EnumCoercions map[string]map[string]string `json:"enumCoercions"`

	// ReplayFields lists assistant-message field paths to preserve verbatim
	// across turns. Parse-side recognition only in v1; outbound threading is
	// a follow-up. Paths use dot-separated keys with [] for array-of-objects.
	ReplayFields []string `json:"replayFields"`

	// --- Behaviour flags ---

	// BehaviourFlags carries adapter-internal behaviour flags that cannot be
	// expressed as flat field operations. Each provider family has a typed
	// sub-struct; adapters access only the sub-struct they own.
	BehaviourFlags ProviderBehaviourFlags `json:"behaviourFlags"`
}

// ProviderBehaviourFlags holds per-provider structural flags. Fields are
// safe to read if zero — the zero value preserves today's adapter behaviour
// in every case.
type ProviderBehaviourFlags struct {
	OpenAI OpenAIBehaviourFlags `json:"openai"`
	Gemini GeminiBehaviourFlags `json:"gemini"`
	// Future: Anthropic AnthropicBehaviourFlags (reserved; Anthropic v1 has no
	// structural divergences beyond what StreamParams already encodes).
}

// OpenAIBehaviourFlags covers behaviour divergences in openai-compatible
// adapters. The zero value reproduces today's behaviour.
type OpenAIBehaviourFlags struct {
	// TokenField selects which JSON key carries the token budget.
	// TokenFieldMaxCompletionTokens is the default (matches the current
	// hard-coded behaviour of openai.go). TokenFieldMaxTokens is the
	// legacy key required by some compat providers (Z.ai GLM, older vLLM
	// builds, Ollama before 0.7). Only rules that explicitly need the legacy
	// key set this; the default is always the modern field.
	TokenField OpenAITokenField `json:"tokenField"`

	// OmitSamplingParams, when true, omits temperature, top_p,
	// presence_penalty, frequency_penalty, logprobs, top_logprobs, and
	// logit_bias from the request body. Used for reasoning-class models that
	// reject these parameters. Note: temperature is already omitted when
	// StreamParams.Temperature is nil (omitempty); this flag additionally
	// omits the other six fields and guarantees temperature is never sent
	// even if the caller set a non-nil value.
	OmitSamplingParams bool `json:"omitSamplingParams"`

	// ExtraBodyFields carries provider-specific top-level request fields that
	// do not exist in the canonical OpenAI Chat Completions schema. The
	// marshaller merges these into the request body after building the
	// canonical fields. Used for Z.ai's `tool_stream: true` and similar
	// gateway-specific extensions.
	//
	// Values must be JSON-serialisable. Keys that collide with canonical
	// request fields are an error detected at registry build time.
	// Secrets MUST NOT appear here — the registry self-test asserts that no
	// ExtraBodyField value contains a secret:// reference.
	ExtraBodyFields map[string]any `json:"extraBodyFields"`
}

// OpenAITokenField controls which JSON key carries the token budget in an
// openai-compatible request.
type OpenAITokenField int

const (
	// TokenFieldMaxCompletionTokens is the default: emits
	// "max_completion_tokens". Matches the current hard-coded behaviour of
	// openai.go and is required by OpenAI reasoning models and GPT-5+.
	TokenFieldMaxCompletionTokens OpenAITokenField = 0 // zero value = default

	// TokenFieldMaxTokens emits the legacy "max_tokens" key required by
	// Z.ai GLM, older vLLM, Ollama, and other compat providers that have
	// not adopted the reasoning-model naming.
	TokenFieldMaxTokens OpenAITokenField = 1
)

// GeminiBehaviourFlags covers behaviour divergences in the Gemini adapter.
// The zero value reproduces today's post-#191 behaviour.
type GeminiBehaviourFlags struct {
	// StreamFunctionCallArgsShape controls how the Gemini adapter configures
	// functionCallingConfig.streamFunctionCallArguments and parses inbound
	// partial-args chunks.
	//
	// Default (StreamArgsOff = 0) preserves the post-#191 behaviour: the
	// flag is set to false for all models and no partial-args parsing
	// occurs. Future rules can model-scope the V2 and V3 shapes.
	StreamFunctionCallArgsShape GeminiStreamArgsShape `json:"streamFunctionCallArgsShape"`
}

// GeminiStreamArgsShape enumerates the streamFunctionCallArguments shapes.
type GeminiStreamArgsShape int

const (
	StreamArgsOff        GeminiStreamArgsShape = 0 // off; post-#191 safe default
	StreamArgsV2Snapshot GeminiStreamArgsShape = 1 // Gemini 2.x cumulative snapshot
	StreamArgsV3Deltas   GeminiStreamArgsShape = 2 // Gemini 3.x JSON-path delta array
)

// Value is a typed JSON scalar used by ProviderQuirks.ValueOverrides.
// Exactly one field is set; New* constructors enforce the invariant.
// JSON tags use camelCase + omitempty so the CLI introspection output
// stays consistent with the rest of ProviderQuirks and only emits the
// populated field.
type Value struct {
	String *string  `json:"string,omitempty"`
	Int    *int     `json:"int,omitempty"`
	Float  *float64 `json:"float,omitempty"`
	Bool   *bool    `json:"bool,omitempty"`
	Null   bool     `json:"null,omitempty"`
}

// NewStringValue returns a Value carrying the given string.
func NewStringValue(s string) Value { return Value{String: &s} }

// NewIntValue returns a Value carrying the given int.
func NewIntValue(i int) Value { return Value{Int: &i} }

// NewFloatValue returns a Value carrying the given float64.
func NewFloatValue(f float64) Value { return Value{Float: &f} }

// NewBoolValue returns a Value carrying the given bool.
func NewBoolValue(b bool) Value { return Value{Bool: &b} }

// NewNullValue returns a Value representing a JSON null.
func NewNullValue() Value { return Value{Null: true} }
