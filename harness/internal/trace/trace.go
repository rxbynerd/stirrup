// Package trace defines the TraceEmitter interface and implementations for
// recording harness run telemetry.
package trace

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

// TraceEmitter records telemetry for a single harness run.
type TraceEmitter interface {
	// Start initialises a new trace with the given run ID and configuration.
	Start(runID string, config *types.RunConfig)

	// RecordTurn appends a turn summary to the current run. The summary
	// carries counters (tokens, duration, stop reason) but no transcript
	// content; the full ModelInput/ModelOutput/tool I/O lives in
	// RecordTurnRecord.
	RecordTurn(turn types.TurnTrace)

	// RecordTurnRecord appends a full transcript record for one turn,
	// including the messages the model saw, the model's output content
	// blocks, and the raw inputs/outputs of every tool call dispatched
	// in that turn. Recording emitters write this as a streamed
	// `turn_record` event; summary-only emitters (OTel, GCS, the
	// nested-forwarder) may ignore it.
	RecordTurnRecord(turn types.TurnRecord)

	// RecordToolCall appends a tool call trace to the current run.
	RecordToolCall(call types.ToolCallTrace)

	// Finish finalises the trace, writes it to the backing store, and
	// returns the completed RunTrace.
	Finish(ctx context.Context, outcome string) (*types.RunTrace, error)
}
