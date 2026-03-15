// Package trace defines the TraceEmitter interface and implementations for
// recording harness run telemetry.
package trace

import (
	"context"

	"github.com/rubynerd/stirrup/types"
)

// TraceEmitter records telemetry for a single harness run.
type TraceEmitter interface {
	// Start initialises a new trace with the given run ID and configuration.
	Start(runID string, config *types.RunConfig)

	// RecordTurn appends a turn trace to the current run.
	RecordTurn(turn types.TurnTrace)

	// RecordToolCall appends a tool call trace to the current run.
	RecordToolCall(call types.ToolCallTrace)

	// Finish finalises the trace, writes it to the backing store, and
	// returns the completed RunTrace.
	Finish(ctx context.Context, outcome string) (*types.RunTrace, error)
}
