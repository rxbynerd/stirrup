package quirks

// ToolSearchCapability declares whether a resolved (provider, model) pair
// has a native tool search for deferred tool definitions. The zero value
// advertises no support. See docs/configuration.md#lazy-tool-loading.
type ToolSearchCapability struct {
	// Supported is the master switch; when false the adapter sends every
	// tool loaded and no search tool.
	Supported bool `json:"supported"`

	// Variant names the native search the adapter emits, in the adapter's
	// own vocabulary: "bm25" or "regex" for Anthropic, "hosted" for OpenAI
	// Responses. Meaningful only when Supported is true.
	Variant string `json:"variant"`
}
