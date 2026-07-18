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

	// RecordPermissionDenial increments the run-level permission-denial
	// counter. Callers should invoke it at the policy denial site, not
	// infer denials later from tool error strings.
	RecordPermissionDenial()

	// Finish finalises the trace, writes it to the backing store, and
	// returns the completed RunTrace.
	Finish(ctx context.Context, outcome string) (*types.RunTrace, error)
}

// SystemInstructionsRecorder is an optional capability a TraceEmitter
// can implement to receive the run's built system prompt via a type
// assertion after PromptBuilder.Build succeeds. Only the OTel emitter
// implements it today, to emit gen_ai.system_instructions when content
// capture is opted into. Not wired through NestedJSONLEmitter: a
// sub-agent's system prompt would clobber the parent's single stored
// value, so sub-agent system instructions are not captured.
type SystemInstructionsRecorder interface {
	RecordSystemInstructions(system string)
}

// PromptResolutionRecorder is an optional capability a TraceEmitter can
// implement to receive the resolved prompt model and tier. Recording is
// not gated on content capture, since this is config metadata rather
// than message content — a prompt/model comparison run must be
// attributable from its trace alone.
type PromptResolutionRecorder interface {
	RecordPromptResolution(model, tier string)
}

// HookRecorder is an optional capability a TraceEmitter can implement to
// receive lifecycle hook results as each PreRun/PostRun hook completes.
// Only the JSONL emitter implements it: it streams a hook_record line
// per execution and folds the accumulated results into
// RunTrace.HookResults at Finish, mirroring RecordToolCall.
type HookRecorder interface {
	RecordHookExecution(exec types.HookExecution)
}

// FinalAssistantTextRecorder is an optional capability a TraceEmitter
// can implement to receive the run's final assistant text just before
// Finish, so the emitter can stamp it onto the RunTrace before
// marshaling it: a post-Finish assignment would never reach the
// persisted trace.
type FinalAssistantTextRecorder interface {
	RecordFinalAssistantText(text string)
}
