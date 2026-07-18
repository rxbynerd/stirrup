package quirks

// ToolChoiceCapability declares the native tool-choice support of a
// resolved (provider, model) pair; adapters consult it before
// serialising a types.StreamParams.ToolChoice onto the wire. The zero
// value advertises no support, so an unresolved provider emits no
// tool-choice field. Mode bools are independent, not a strict
// superset, because a gateway may support one mode without another.
type ToolChoiceCapability struct {
	// Supported is the master switch; when false the adapter must not
	// emit any tool-choice field regardless of the per-mode bools.
	Supported bool `json:"supported"`

	// Auto reports whether the provider accepts an explicit "let the
	// model decide" tool-choice value.
	Auto bool `json:"auto"`

	// Required reports whether the provider can force at least one tool
	// call on the turn (Anthropic "any", OpenAI "required", Gemini
	// "ANY"); an adapter resolving false defers to the prompt fallback.
	Required bool `json:"required"`

	// None reports whether the provider can forbid tool calls natively
	// (OpenAI "none", Gemini "NONE"). Anthropic has no native "none" —
	// a ToolChoiceNone request is expressed by omitting the tool list.
	None bool `json:"none"`

	// NamedTool reports whether the provider can force one specific tool
	// (Anthropic {"type":"tool"}, OpenAI {"type":"function"}, Gemini ANY
	// with allowedFunctionNames).
	NamedTool bool `json:"namedTool"`
}
