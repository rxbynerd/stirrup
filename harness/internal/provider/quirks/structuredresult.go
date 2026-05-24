package quirks

// StructuredToolResultCapability declares whether a resolved (provider,
// model) pair accepts a structured (non-string) tool-result payload on the
// wire, alongside the canonical text fallback the harness always produces
// (issue #231). It is the cross-provider capability the adapters consult
// before serialising a types.ToolResult.Structured envelope.
//
// Modelled as a top-level ProviderQuirks field rather than a per-provider
// behaviour flag for the same reason ToolChoiceCapability is (see
// quirks.go): "tool results can carry structure" is a concept every family
// expresses differently (Anthropic's tool_result content-block array,
// Gemini's functionResponse.response object, OpenAI's plain-string tool
// message) but that the loop reasons about uniformly. Placing it under one
// provider's sub-struct would force the others to reach across family
// boundaries to read it, which the BehaviourFlags ownership rule forbids.
//
// The zero value advertises NO support: Supported is false. An adapter
// resolving a provider with no structured-result rule therefore sends only
// the text Content, leaving the request byte-identical to the pre-#231
// shape. This is the no-regression guarantee — a provider stays text-only
// until a rule opts it in, and the text fallback is never dropped.
type StructuredToolResultCapability struct {
	// Supported is the master switch. When false, the adapter MUST NOT
	// emit any structured representation of a tool result regardless of
	// the shape below; it sends the text Content only. A rule that sets
	// any shape bool is expected to also set Supported; the registry
	// self-test pins that relationship.
	Supported bool `json:"supported"`

	// ObjectResponse reports whether the provider's tool-result slot is a
	// free-form JSON object (Gemini's functionResponse.response). When set,
	// the adapter places the structured envelope directly into that object
	// slot; the text fallback rides alongside it under a reserved key so a
	// provider that ignores the extra fields still receives the canonical
	// rendering.
	ObjectResponse bool `json:"objectResponse"`

	// ContentBlockArray reports whether the provider's tool-result content
	// is a typed content-block array that can carry a JSON-serialised block
	// in addition to the text block (Anthropic's tool_result content array).
	// When set, the adapter emits the content as an array: the canonical
	// text block plus a text block carrying the structured JSON, so the
	// model receives both renderings and a provider that only reads the
	// first block still sees the text fallback.
	ContentBlockArray bool `json:"contentBlockArray"`
}
