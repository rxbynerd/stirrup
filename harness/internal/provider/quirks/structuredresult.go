package quirks

// StructuredToolResultCapability declares whether a resolved (provider,
// model) pair accepts a structured (non-string) tool-result payload on the
// wire, alongside the canonical text fallback the harness always produces.
// The zero value advertises no support, so a provider without a matching
// rule stays text-only.
type StructuredToolResultCapability struct {
	// Supported is the master switch; when false the adapter sends the
	// text Content only, regardless of the shape fields below.
	Supported bool `json:"supported"`

	// ObjectResponse: the provider's tool-result slot is a free-form JSON
	// object (Gemini's functionResponse.response).
	ObjectResponse bool `json:"objectResponse"`

	// ContentBlockArray: the provider's tool-result content is a typed
	// content-block array that can carry a JSON block alongside the text
	// block (Anthropic's tool_result content array).
	ContentBlockArray bool `json:"contentBlockArray"`
}
