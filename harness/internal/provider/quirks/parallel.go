package quirks

// ParallelToolCallsCapability declares the native parallel-tool-call
// control of a resolved (provider, model) pair; adapters consult it
// before serialising a types.StreamParams.ParallelToolCalls onto the
// wire. The zero value advertises no support, so an unresolved
// provider emits nothing — parallelism is an efficiency hint, not a
// correctness lever, so there is no prompt-based fallback.
type ParallelToolCallsCapability struct {
	// Supported is the master switch; when false the adapter must not
	// emit any parallel-tool-call control regardless of Disable.
	Supported bool `json:"supported"`

	// Disable reports whether the provider can express "do not call
	// tools in parallel" natively (OpenAI `parallel_tool_calls:false`,
	// Anthropic `tool_choice.disable_parallel_tool_use:true`). Distinct
	// from Supported so a gateway accepting only the enable direction
	// can advertise one without the other.
	Disable bool `json:"disable"`
}
