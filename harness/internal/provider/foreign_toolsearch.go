package provider

import "github.com/rxbynerd/stirrup/types"

// hasToolSearchBlocks reports whether content holds a provider-side tool
// search block. Adapters without a native search drop these blocks, so a
// message that held nothing else translates to no content.
func hasToolSearchBlocks(content []types.ContentBlock) bool {
	for _, b := range content {
		if b.Type == "tool_search" || b.Type == "tool_search_result" {
			return true
		}
	}
	return false
}
