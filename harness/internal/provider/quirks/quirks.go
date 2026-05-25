// Package quirks implements the per-(provider, model) wire-shape and
// behaviour override registry. Adapters call Registry.Resolve at the
// top of each Stream call to get a ProviderQuirks for the request.
package quirks

import "fmt"

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

	// --- Capabilities ---

	// ToolChoice declares whether and how the resolved (provider, model)
	// supports a native tool-choice control. It is a TOP-LEVEL capability
	// rather than a per-provider behaviour flag because tool_choice is a
	// cross-provider concept: Anthropic, OpenAI-compatible, and Gemini all
	// expose some form (tool_choice / functionCallingConfig). Modelling it
	// under one provider's sub-struct would force the other adapters to
	// reach across family boundaries to read it, which the BehaviourFlags
	// ownership rule forbids. The zero value advertises no support, so an
	// adapter for a provider with no rule emits no tool-choice field — the
	// graceful no-op the StreamParams.ToolChoice contract requires.
	ToolChoice ToolChoiceCapability `json:"toolChoice"`

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

	// StrictMode, when true, instructs the adapter to mark every tool with
	// `strict: true` and normalise the tool's JSON Schema into the shape
	// OpenAI's structured-outputs path requires:
	//
	//   - every property listed in `required`,
	//   - optional fields modelled as nullable (`["type","null"]`),
	//   - `additionalProperties: false` at every object level.
	//
	// The normalisation is a pure rewrite: no field is deleted and no type
	// is narrowed beyond nullability. When a schema contains a construct
	// that cannot be expressed in strict form (e.g. an unsupported `oneOf`
	// branch shape) the adapter fails the request before any wire bytes
	// are sent.
	//
	// Opt-in per (provider, model) via quirks rules; operators do not
	// toggle this directly. The OpenAI structured-outputs documentation
	// names the models that support it — the BuiltinRules() entries
	// reflect that surface and grow as it expands.
	StrictMode bool `json:"strictMode"`
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

// MarshalJSON renders OpenAITokenField as a human-readable string so
// the CLI introspection output names the wire key rather than the
// underlying int constant. An unknown value is rendered as
// "unknown(N)" rather than failing, so a forward-compatible reader
// can still parse output produced by a newer harness.
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

// UnmarshalJSON is the inverse of MarshalJSON, so a tool that emits
// CLI output and feeds it back through json.Unmarshal round-trips
// cleanly. Unknown strings are rejected — silently accepting them
// would defeat the point of the named constants.
func (f *OpenAITokenField) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"max_completion_tokens"`:
		*f = TokenFieldMaxCompletionTokens
	case `"max_tokens"`:
		*f = TokenFieldMaxTokens
	default:
		return fmt.Errorf("quirks: unknown OpenAITokenField %s", data)
	}
	return nil
}

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

	// SchemaUnsupportedFeatures lists JSON Schema keywords that Vertex AI's
	// function-declaration Schema dialect rejects for the resolved model.
	// The Gemini adapter lints each tool's input schema against this list
	// before serialising the request; a match fails the request before any
	// wire bytes are sent.
	//
	// Represented as []string (rather than a typed enum) so a rule can name
	// any JSON Schema keyword without a follow-up code change here.
	// Recognised entries today: "pattern", "format", "oneOf", "anyOf",
	// "allOf", "$ref", "patternProperties", "if", "then", "else", "not",
	// "contains", "minContains", "maxContains", "unevaluatedProperties",
	// "unevaluatedItems", "dependencies", "dependentRequired",
	// "dependentSchemas", "propertyNames", "const", "examples".
	// A linter that sees an entry it does not recognise treats it as
	// "the keyword name on the schema" and matches by key — extension is
	// data-only.
	//
	// Note: Gemini's Schema implementation also rejects `oneOf`, `anyOf`,
	// `allOf`, and `$ref` at the structural level; ConvertSchema already
	// errors on those for any model. The linter is the place to express
	// model-scoped rejections beyond the structural floor — e.g. some
	// Gemini families reject `pattern` and `format` outright.
	SchemaUnsupportedFeatures []string `json:"schemaUnsupportedFeatures"`
}

// GeminiStreamArgsShape enumerates the streamFunctionCallArguments shapes.
type GeminiStreamArgsShape int

const (
	StreamArgsOff        GeminiStreamArgsShape = 0 // off; post-#191 safe default
	StreamArgsV2Snapshot GeminiStreamArgsShape = 1 // Gemini 2.x cumulative snapshot
	StreamArgsV3Deltas   GeminiStreamArgsShape = 2 // Gemini 3.x JSON-path delta array
)

// MarshalJSON renders GeminiStreamArgsShape as a human-readable string
// for the same reason as OpenAITokenField.MarshalJSON: CLI output is
// the operator-facing surface, and an opaque integer there is a
// regression-in-waiting once Step 3 ships a non-default rule.
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
		return fmt.Errorf("quirks: unknown GeminiStreamArgsShape %s", data)
	}
	return nil
}

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
