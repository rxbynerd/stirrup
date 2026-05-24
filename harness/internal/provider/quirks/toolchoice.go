package quirks

// ToolChoiceCapability declares the native tool-choice support of a
// resolved (provider, model) pair. It is the cross-provider capability
// the adapters consult before serialising a types.StreamParams.ToolChoice
// onto the wire.
//
// The zero value advertises NO support: Supported is false and every mode
// bool is false. An adapter resolving a provider with no tool-choice rule
// therefore emits no tool-choice field, leaving the request byte-identical
// to the pre-#230 shape. This is the graceful no-op the StreamParams
// contract requires — the prompt-based fallback for unsupported providers
// is handled by the loop escalation chunk, not here.
//
// The mode bools are independent rather than a single "max level" because
// providers do not support the modes as a strict superset: a hypothetical
// gateway might accept "required" but not a specific-named-tool form, and
// the adapter must be able to fall back per-mode rather than assume that
// supporting one mode implies supporting all of them.
type ToolChoiceCapability struct {
	// Supported is the master switch. When false, the adapter MUST NOT
	// emit any tool-choice field regardless of the per-mode bools below.
	// A rule that sets any per-mode bool is expected to also set
	// Supported; the registry self-test pins that relationship.
	Supported bool `json:"supported"`

	// Auto reports whether the provider accepts an explicit "let the
	// model decide" tool-choice value. This is distinct from the
	// zero-value StreamParams.ToolChoiceAuto, which is always satisfied
	// by emitting nothing: Auto here matters only for a future caller
	// that wants to pin auto explicitly. Every provider with a native
	// control supports it, so first-party rules set it alongside
	// Supported.
	Auto bool `json:"auto"`

	// Required reports whether the provider can force at least one tool
	// call on the turn (Anthropic "any", OpenAI "required", Gemini
	// "ANY"). This is the load-bearing mode for issue #230's escalation:
	// an adapter that resolves Required == false must not emit the field
	// for a ToolChoiceRequired request, deferring to the prompt fallback.
	Required bool `json:"required"`

	// None reports whether the provider can forbid tool calls on the turn
	// via a native value (OpenAI "none", Gemini "NONE"). Anthropic has no
	// native "none" — a ToolChoiceNone request is expressed by omitting
	// the tool list — so the Anthropic rule leaves this false and the
	// adapter handles the mode structurally rather than via the field.
	None bool `json:"none"`

	// NamedTool reports whether the provider can force one specific tool
	// (Anthropic {"type":"tool"}, OpenAI {"type":"function"}, Gemini ANY
	// with allowedFunctionNames). Set independently of Required because a
	// gateway may accept the coarse "required" form without the named
	// form.
	NamedTool bool `json:"namedTool"`
}
