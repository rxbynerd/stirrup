package core

import (
	"context"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// dispatchToolCallCategorized runs preflightToolCall then executeToolCall
// for one call, without the PhasePreTool guard planAndDispatch interleaves
// between them. Test-only: production dispatch goes through planAndDispatch.
func (l *AgenticLoop) dispatchToolCallCategorized(ctx context.Context, call types.ToolCall) (string, bool, observability.ToolFailureCategory, structuredOutput) {
	t := l.Tools.Resolve(call.Name)
	input, rejection, category := l.preflightToolCall(call, t)
	if category != "" {
		return rejection, false, category, structuredOutput{}
	}
	return l.executeToolCall(ctx, t, call, input)
}

// dispatchToolCall is a (string, bool) view of dispatchToolCallCategorized
// for tests that assert only on output and success.
func (l *AgenticLoop) dispatchToolCall(ctx context.Context, call types.ToolCall) (string, bool) {
	out, ok, _, _ := l.dispatchToolCallCategorized(ctx, call)
	return out, ok
}
