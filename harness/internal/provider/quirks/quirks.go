// Package quirks implements the per-(provider, model) wire-shape and
// behaviour override registry. Adapters call Registry.Resolve at the
// top of each Stream call to get a ProviderQuirks for the request.
package quirks

import "fmt"

// ProviderQuirks is the in-memory result of resolving the registry for a
// (providerType, model) pair. Adapters read it when building a request and
// (for paths that diverge) when interpreting a response.
//
// Registry.Resolve always returns a ProviderQuirks with every map and slice
// field pre-initialised to a non-nil empty value; a zero-value ProviderQuirks
// constructed outside the registry is NOT safe to mutate. See
// docs/provider-quirks.md for the design rationale.
type ProviderQuirks struct {
	// --- Wire-shape overrides ---

	// FieldRenames maps canonical adapter-internal field name to the wire
	// JSON key the request should emit. Empty key means "use canonical name".
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
	// value).
	EnumCoercions map[string]map[string]string `json:"enumCoercions"`

	// ReplayFields lists assistant-message field paths to preserve verbatim
	// across turns. Paths use dot-separated keys with [] for array-of-objects.
	// Every adapter captures the named paths parse-side; the openai-compatible
	// adapter additionally threads single-segment, non-colliding paths back
	// onto subsequent requests. A rule's Description suffix — "(threaded)" vs
	// "(parse-side only)" — records which behaviour applies.
	ReplayFields []string `json:"replayFields"`

	// --- Capabilities ---

	// ToolChoice declares whether and how the resolved (provider, model)
	// supports a native tool-choice control. Top-level rather than a
	// per-provider behaviour flag because tool_choice is a cross-provider
	// concept. The zero value advertises no support.
	ToolChoice ToolChoiceCapability `json:"toolChoice"`

	// StructuredToolResults declares whether the resolved (provider, model)
	// accepts a structured (non-string) tool-result payload on the wire, and
	// in which wire shape. The zero value advertises no support, so an
	// adapter for a provider with no rule sends only the text Content.
	StructuredToolResults StructuredToolResultCapability `json:"structuredToolResults"`

	// ParallelToolCalls declares whether the resolved (provider, model)
	// supports a native parallel-tool-call control. The zero value
	// advertises no support, so an adapter with no rule emits nothing.
	ParallelToolCalls ParallelToolCallsCapability `json:"parallelToolCalls"`

	// ToolExamples declares whether the resolved (provider, model) accepts
	// the JSON-Schema `examples` keyword inside a tool's parameters object.
	// The zero value advertises no support. Gemini deliberately stays at the
	// zero value — its Schema dialect rejects `examples`.
	ToolExamples ToolExamplesCapability `json:"toolExamples"`

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
	// OpenAIResponses carries the wire divergences of the OpenAI Responses
	// API (POST /v1/responses) that OpenAIBehaviourFlags cannot express. The
	// Responses adapter owns this sub-struct; the Chat adapter never reads it.
	OpenAIResponses OpenAIResponsesBehaviourFlags `json:"openaiResponses"`
	// Anthropic carries the Anthropic Messages API adapter's behaviour
	// divergences.
	Anthropic AnthropicBehaviourFlags `json:"anthropic"`
}

// AnthropicBehaviourFlags covers behaviour divergences in the Anthropic
// Messages API adapter. The zero value reproduces today's behaviour
// (sampling params forwarded whenever StreamParams.Temperature is non-nil).
type AnthropicBehaviourFlags struct {
	// OmitSamplingParams, when true, forces "temperature" out of the
	// request body even when StreamParams.Temperature is non-nil. Used
	// for model families that reject a non-default sampling parameter
	// outright with an HTTP 400: see docs/provider-quirks.md for the
	// affected model list.
	OmitSamplingParams bool `json:"omitSamplingParams"`
}

// OpenAIBehaviourFlags covers behaviour divergences in openai-compatible
// adapters. The zero value reproduces today's behaviour.
type OpenAIBehaviourFlags struct {
	// TokenField selects which JSON key carries the token budget.
	// TokenFieldMaxCompletionTokens is the default. TokenFieldMaxTokens is
	// the legacy key required by some compat providers (Z.ai GLM, older
	// vLLM, Ollama before 0.7).
	TokenField OpenAITokenField `json:"tokenField"`

	// OmitSamplingParams, when true, omits temperature, top_p,
	// presence_penalty, frequency_penalty, logprobs, top_logprobs, and
	// logit_bias from the request body, and guarantees temperature is never
	// sent even if the caller set a non-nil value. Used for reasoning-class
	// models that reject these parameters.
	OmitSamplingParams bool `json:"omitSamplingParams"`

	// ExtraBodyFields carries provider-specific top-level request fields that
	// do not exist in the canonical OpenAI Chat Completions schema. The
	// marshaller merges these into the request body after building the
	// canonical fields. Values must be JSON-serialisable; keys that collide
	// with canonical request fields are an error at registry build time.
	// Secrets MUST NOT appear here — the registry self-test asserts that no
	// ExtraBodyField value contains a secret:// reference.
	ExtraBodyFields map[string]any `json:"extraBodyFields"`

	// StrictMode, when true, instructs the adapter to mark every tool with
	// `strict: true` and normalise the tool's JSON Schema into the shape
	// OpenAI's structured-outputs path requires: every property listed in
	// `required`, optional fields modelled as nullable, and
	// `additionalProperties: false` at every object level. A schema
	// containing a construct that cannot be expressed in strict form fails
	// the request before any wire bytes are sent.
	StrictMode bool `json:"strictMode"`
}

// OpenAITokenField controls which JSON key carries the token budget in an
// openai-compatible request.
type OpenAITokenField int

const (
	// TokenFieldMaxCompletionTokens emits "max_completion_tokens"; required
	// by OpenAI reasoning models and GPT-5+. Zero value = default.
	TokenFieldMaxCompletionTokens OpenAITokenField = 0

	// TokenFieldMaxTokens emits the legacy "max_tokens" key required by
	// Z.ai GLM, older vLLM, Ollama, and similar compat providers.
	TokenFieldMaxTokens OpenAITokenField = 1
)

// MarshalJSON renders OpenAITokenField as the wire key it selects, for the
// CLI introspection surface. An unknown value renders as "unknown(N)" so a
// forward-compatible reader can still parse output from a newer harness.
func (f OpenAITokenField) MarshalJSON() ([]byte, error) {
	switch f {
	case TokenFieldMaxCompletionTokens:
		return []byte(`"max_completion_tokens"`), nil
	case TokenFieldMaxTokens:
		return []byte(`"max_tokens"`), nil
	default:
		return []byte(fmt.Sprintf(`"unknown(%d)"`, int(f))), nil
	}
}

// UnmarshalJSON is the inverse of MarshalJSON. Unknown strings are
// rejected — silently accepting them would defeat the point of the
// named constants.
func (f *OpenAITokenField) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"max_completion_tokens"`:
		*f = TokenFieldMaxCompletionTokens
	case `"max_tokens"`:
		*f = TokenFieldMaxTokens
	default:
		return fmt.Errorf("quirks: unknown OpenAITokenField %.64s", data)
	}
	return nil
}

// OpenAIResponsesBehaviourFlags covers the wire divergences of the OpenAI
// Responses API. The zero value reproduces the Responses adapter's
// pre-quirks hard-coded behaviour.
type OpenAIResponsesBehaviourFlags struct {
	// TokenField selects which JSON key carries the token budget. The
	// Responses API uses "max_output_tokens" (the zero value), distinct
	// from Chat Completions' keys — hence a separate enum from
	// OpenAITokenField.
	TokenField OpenAIResponsesTokenField `json:"tokenField"`

	// StoreMode controls the top-level `store` field. The Responses adapter
	// sends an explicit `store:false` (the zero value) because stirrup
	// manages its own conversation history; leaving the key unset would
	// default to server-side persistence on some endpoints.
	StoreMode OpenAIResponsesStoreMode `json:"storeMode"`

	// InputItemShape selects how conversation history is serialised into the
	// Responses `input` array. The zero value (TypedInputItems) emits the
	// discriminated-union shape with per-variant wire structs. No
	// alternative shape ships in v1.
	InputItemShape OpenAIResponsesInputShape `json:"inputItemShape"`
}

// OpenAIResponsesTokenField controls which JSON key carries the token
// budget in a Responses API request.
type OpenAIResponsesTokenField int

const (
	// TokenFieldMaxOutputTokens emits "max_output_tokens", the key the
	// Responses API requires. Zero value = default.
	TokenFieldMaxOutputTokens OpenAIResponsesTokenField = 0
)

// MarshalJSON renders OpenAIResponsesTokenField as the wire key it selects,
// for the CLI introspection surface.
func (f OpenAIResponsesTokenField) MarshalJSON() ([]byte, error) {
	switch f {
	case TokenFieldMaxOutputTokens:
		return []byte(`"max_output_tokens"`), nil
	default:
		return []byte(fmt.Sprintf(`"unknown(%d)"`, int(f))), nil
	}
}

// UnmarshalJSON is the inverse of MarshalJSON. Unknown strings are
// rejected rather than silently zero-ing the field.
func (f *OpenAIResponsesTokenField) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"max_output_tokens"`:
		*f = TokenFieldMaxOutputTokens
	default:
		return fmt.Errorf("quirks: unknown OpenAIResponsesTokenField %.64s", data)
	}
	return nil
}

// OpenAIResponsesStoreMode controls the top-level `store` field of a
// Responses request.
type OpenAIResponsesStoreMode int

const (
	// StoreFalse emits an explicit `"store":false`. Zero value = default;
	// stirrup manages its own history and never opts into server-side state.
	StoreFalse OpenAIResponsesStoreMode = 0
)

// MarshalJSON renders OpenAIResponsesStoreMode as a human-readable string
// for the CLI introspection surface.
func (s OpenAIResponsesStoreMode) MarshalJSON() ([]byte, error) {
	switch s {
	case StoreFalse:
		return []byte(`"store_false"`), nil
	default:
		return []byte(fmt.Sprintf(`"unknown(%d)"`, int(s))), nil
	}
}

// UnmarshalJSON is the inverse of MarshalJSON.
func (s *OpenAIResponsesStoreMode) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"store_false"`:
		*s = StoreFalse
	default:
		return fmt.Errorf("quirks: unknown OpenAIResponsesStoreMode %.64s", data)
	}
	return nil
}

// OpenAIResponsesInputShape enumerates the supported serialisations of the
// Responses `input` array.
type OpenAIResponsesInputShape int

const (
	// TypedInputItems emits the per-variant discriminated-union shape
	// (message / function_call / function_call_output). Zero value =
	// default; carries the #172 + #199 fixes. No other shape ships in v1.
	TypedInputItems OpenAIResponsesInputShape = 0
)

// MarshalJSON renders OpenAIResponsesInputShape as a human-readable string
// for the CLI introspection surface.
func (s OpenAIResponsesInputShape) MarshalJSON() ([]byte, error) {
	switch s {
	case TypedInputItems:
		return []byte(`"typed_input_items"`), nil
	default:
		return []byte(fmt.Sprintf(`"unknown(%d)"`, int(s))), nil
	}
}

// UnmarshalJSON is the inverse of MarshalJSON.
func (s *OpenAIResponsesInputShape) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"typed_input_items"`:
		*s = TypedInputItems
	default:
		return fmt.Errorf("quirks: unknown OpenAIResponsesInputShape %.64s", data)
	}
	return nil
}

// GeminiBehaviourFlags covers behaviour divergences in the Gemini adapter.
// The zero value reproduces today's default behaviour.
type GeminiBehaviourFlags struct {
	// StreamFunctionCallArgsShape controls how the Gemini adapter configures
	// functionCallingConfig.streamFunctionCallArguments and parses inbound
	// partial-args chunks. Default (StreamArgsOff) disables the flag and
	// partial-args parsing for all models.
	StreamFunctionCallArgsShape GeminiStreamArgsShape `json:"streamFunctionCallArgsShape"`

	// SchemaUnsupportedFeatures lists JSON Schema keywords that Vertex AI's
	// function-declaration Schema dialect rejects for the resolved model.
	// The Gemini adapter lints each tool's input schema against this list
	// before serialising the request; a match fails the request before any
	// wire bytes are sent. Represented as []string so a rule can name any
	// keyword without a code change here; an unrecognised entry still
	// matches by key. ConvertSchema already rejects oneOf/anyOf/allOf/$ref
	// structurally for every model — this list is for rejections beyond
	// that floor (e.g. some Gemini families also reject pattern/format).
	SchemaUnsupportedFeatures []string `json:"schemaUnsupportedFeatures"`
}

// GeminiStreamArgsShape enumerates the streamFunctionCallArguments shapes.
type GeminiStreamArgsShape int

const (
	StreamArgsOff        GeminiStreamArgsShape = 0 // off; safe default
	StreamArgsV2Snapshot GeminiStreamArgsShape = 1 // Gemini 2.x cumulative snapshot
	StreamArgsV3Deltas   GeminiStreamArgsShape = 2 // Gemini 3.x JSON-path delta array
)

// MarshalJSON renders GeminiStreamArgsShape as a human-readable string for
// the CLI introspection surface.
func (s GeminiStreamArgsShape) MarshalJSON() ([]byte, error) {
	switch s {
	case StreamArgsOff:
		return []byte(`"off"`), nil
	case StreamArgsV2Snapshot:
		return []byte(`"v2_snapshot"`), nil
	case StreamArgsV3Deltas:
		return []byte(`"v3_deltas"`), nil
	default:
		return []byte(fmt.Sprintf(`"unknown(%d)"`, int(s))), nil
	}
}

// UnmarshalJSON is the inverse of MarshalJSON. Rejects unknown
// strings rather than silently zero-ing the field.
func (s *GeminiStreamArgsShape) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"off"`:
		*s = StreamArgsOff
	case `"v2_snapshot"`:
		*s = StreamArgsV2Snapshot
	case `"v3_deltas"`:
		*s = StreamArgsV3Deltas
	default:
		return fmt.Errorf("quirks: unknown GeminiStreamArgsShape %.64s", data)
	}
	return nil
}

// Value is a typed JSON scalar used by ProviderQuirks.ValueOverrides.
// Exactly one field is set; New* constructors enforce the invariant.
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
