package quirks

// ToolSearchCapability declares whether a resolved (provider, model) pair
// has a native tool search: a provider-side tool that lets the model load
// deferred tool definitions on demand (Anthropic's tool search tool with
// defer_loading, OpenAI Responses' hosted tool_search with
// defer_loading). The loop consults it under lazy tool loading to decide
// between the native search and the harness's own search tool; adapters
// consult it before honouring StreamParams.ToolSearch. The zero value
// advertises no support, so an unresolved provider never emits a search
// tool or a deferred definition.
type ToolSearchCapability struct {
	// Supported is the master switch; when false the adapter sends every
	// tool loaded and no search tool, whatever StreamParams asks.
	Supported bool `json:"supported"`

	// Variant names the native search the adapter emits, in the adapter's
	// own vocabulary: "bm25" or "regex" for Anthropic, "hosted" for OpenAI
	// Responses. Meaningful only when Supported is true.
	Variant string `json:"variant"`
}
