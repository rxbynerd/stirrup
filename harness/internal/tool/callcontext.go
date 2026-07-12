package tool

import "context"

// CallContext identifies the run, turn, and tool_use block a tool invocation
// belongs to. The loop attaches it to the dispatch context before invoking a
// handler so components that persist per-call artefacts (e.g. the command
// output store) can correlate bytes with transcript identifiers without
// depending on loop internals.
type CallContext struct {
	RunID       string
	ParentRunID string
	Turn        int
	ToolUseID   string
}

type callContextKey struct{}

// WithCallContext returns a context carrying meta.
func WithCallContext(ctx context.Context, meta CallContext) context.Context {
	return context.WithValue(ctx, callContextKey{}, meta)
}

// CallContextFrom extracts the CallContext attached by the loop; the zero
// value when none is attached.
func CallContextFrom(ctx context.Context) CallContext {
	meta, _ := ctx.Value(callContextKey{}).(CallContext)
	return meta
}
