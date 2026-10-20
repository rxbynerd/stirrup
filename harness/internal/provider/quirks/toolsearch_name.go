package quirks

// NativeToolName returns the name the provider's native tool search
// carries in a response: "tool_search" for OpenAI Responses' hosted
// search, "tool_search_tool_regex" for Anthropic's regex variant and
// "tool_search_tool_bm25" otherwise, matching the Anthropic adapter's
// default variant.
func (c ToolSearchCapability) NativeToolName() string {
	switch c.Variant {
	case "hosted":
		return "tool_search"
	case "regex":
		return "tool_search_tool_regex"
	default:
		return "tool_search_tool_bm25"
	}
}
