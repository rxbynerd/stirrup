package quirks

// ParallelToolCallsCapability declares the native parallel-tool-call control
// of a resolved (provider, model) pair. It is the cross-provider capability
// the adapters consult before serialising a types.StreamParams.ParallelToolCalls
// onto the wire (issue #222).
//
// Modelled as a top-level ProviderQuirks field for the same reason
// ToolChoiceCapability is (see quirks.go): "limit the model to one tool call
// per turn" is a concept the loop reasons about uniformly even though each
// family encodes it differently — OpenAI Chat/Responses expose a top-level
// `parallel_tool_calls` bool, Anthropic rides it on the tool_choice object via
// `disable_parallel_tool_use`, and Gemini/Bedrock have no native control.
//
// The zero value advertises NO support: Supported is false. An adapter
// resolving a provider with no parallel rule therefore emits nothing, leaving
// the request byte-identical to the pre-#222 shape. Parallelism is an
// efficiency hint, not a correctness lever, so there is no prompt-based
// fallback for unsupported providers — the field is simply a no-op.
type ParallelToolCallsCapability struct {
	// Supported is the master switch. When false, the adapter MUST NOT emit
	// any parallel-tool-call control regardless of Disable below. A rule
	// that sets Disable is expected to also set Supported; the registry
	// self-test pins that relationship.
	Supported bool `json:"supported"`

	// Disable reports whether the provider can express "do not call tools in
	// parallel" natively. Every first-party provider with a control can
	// (OpenAI via `parallel_tool_calls:false`, Anthropic via
	// `tool_choice.disable_parallel_tool_use:true`), so first-party rules set
	// it alongside Supported. It is a distinct bool so a future gateway that
	// accepts only the enable direction can advertise Supported without
	// Disable, and the adapter can fall back rather than assume the disable
	// form exists.
	Disable bool `json:"disable"`
}
