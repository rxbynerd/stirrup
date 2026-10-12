// Package trace defines the TraceEmitter interface and implementations for
// recording harness run telemetry.
package trace

import (
	"context"
	"encoding/json"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

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
	// returns the completed RunTrace. When writing fails after the trace
	// was assembled, Finish returns the trace together with the error so
	// the caller can still report the run.
	Finish(ctx context.Context, outcome string) (*types.RunTrace, error)
}

// SystemInstructionsRecorder is an optional capability a TraceEmitter
// can implement to receive the run's built system prompt via a type
// assertion after PromptBuilder.Build succeeds. The OTel emitter uses it
// to emit gen_ai.system_instructions when content capture is opted into;
// the JSONL emitter writes a system_prompt record. Not wired through
// NestedJSONLEmitter: a
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

// StopDetailsRecorder is an optional capability a TraceEmitter can
// implement to receive RunTrace.StopDetails just before Finish, for the
// same reason as FinalAssistantTextRecorder.
type StopDetailsRecorder interface {
	RecordStopDetails(details *types.StopDetails)
}

// CommandOutputRecorder receives bounded metadata for complete command
// captures. Full stream content remains in the sidecar archive.
type CommandOutputRecorder interface {
	RecordCommandOutput(types.CommandOutputRecord)
}

// CommandOutputArchiveRecorder attaches the finalized sidecar location to the
// run summary and, for OTel, bounded root-span attributes.
type CommandOutputArchiveRecorder interface {
	RecordCommandOutputArchive(location string)
}

// GitHandoffRecorder is an optional capability a TraceEmitter can
// implement to receive RunTrace.Git just before Finish, for the same
// reason as FinalAssistantTextRecorder.
type GitHandoffRecorder interface {
	RecordGitHandoff(handoff types.GitHandoff)
}

// WorkspaceExportRecorder is an optional capability a TraceEmitter can
// implement to receive RunTrace.WorkspaceExport just before Finish, for
// the same reason as FinalAssistantTextRecorder.
type WorkspaceExportRecorder interface {
	RecordWorkspaceExport(receipt types.ExportReceipt)
}

// RunErrorRecorder is an optional capability a TraceEmitter can
// implement to receive RunTrace.Error just before Finish, for the same
// reason as FinalAssistantTextRecorder.
type RunErrorRecorder interface {
	RecordRunError(text string)
}

// StructuredOutputRecorder is an optional capability a TraceEmitter can
// implement to receive RunTrace.StructuredOutput just before Finish, for
// the same reason as FinalAssistantTextRecorder.
type StructuredOutputRecorder interface {
	RecordStructuredOutput(doc json.RawMessage)
}

// SubRunRecorder is an optional capability a TraceEmitter can implement
// to receive the start and finish of a sub-agent run from
// NestedJSONLEmitter. The JSONL emitter streams them as subrun_started
// and subrun_finished lines so a reader can rebuild each child's history
// from the parent's file.
type SubRunRecorder interface {
	RecordSubRunStarted(runID, parentRunID string, startedAt time.Time, config *types.RunConfig)
	RecordSubRunFinished(summary types.SubRunSummary)
}

// RootContextProvider is an optional capability for emitters that open a
// span for the run. The loop parents its component spans on the returned
// context. A nil return means the emitter records no run span.
type RootContextProvider interface {
	RootContext() context.Context
}

// LiveSpanStarter is an optional capability for emitters that record model
// requests and tool calls as live spans. A nil handle means the emitter
// cannot record this span (for example a NestedJSONLEmitter over a parent
// without the capability); the caller then uses its own fallback.
type LiveSpanStarter interface {
	StartChatSpan(parent context.Context, start ChatSpanStart) ChatSpan
	StartToolSpan(parent context.Context, start ToolSpanStart) ToolSpan
}

// ChatSpanStart describes one model request at the moment it is sent.
type ChatSpanStart struct {
	Turn int
	// ProviderType is the stirrup provider type, e.g. "anthropic".
	ProviderType string
	Model        string
	// ServerAddress and ServerPort locate the provider endpoint; empty
	// and zero when the adapter does not report one.
	ServerAddress string
	ServerPort    int
	MaxTokens     int
	Temperature   *float64
	// RunID and ParentRunID identify a sub-agent's request; both are
	// empty for the top-level run.
	RunID       string
	ParentRunID string
}

// ChatSpanEnd describes how a model request finished.
type ChatSpanEnd struct {
	// Summary is nil when the request failed before a summary was built.
	Summary *types.TurnTrace
	// Record carries the request's input and output. A failed request
	// carries the input it sent and no output.
	Record *types.TurnRecord
	// ErrorType is a bounded category; empty on success.
	ErrorType string
	// ErrorText is already scrubbed by the caller; the emitter scrubs it
	// again before it reaches a span.
	ErrorText string
}

// ChatSpan is one live model request. Span exposes the underlying span so
// the caller can parent children on it; End is idempotent.
type ChatSpan interface {
	Span() oteltrace.Span
	End(ChatSpanEnd)
}

// ToolSpanStart describes one tool call at dispatch.
type ToolSpanStart struct {
	CallID string
	// Name is the model-facing tool name.
	Name string
	// Known is false for a name that resolved to no registered tool; the
	// span name then omits the model-controlled name.
	Known       bool
	InputSize   int
	RunID       string
	ParentRunID string
}

// ToolSpanEnd describes how a tool call finished.
type ToolSpanEnd struct {
	Summary *types.ToolCallTrace
	// Record carries the raw input and output; the emitter scrubs it
	// before recording content. It is nil for a call dropped after a
	// stall.
	Record *types.ToolCallRecord
	// EndTime is when the call finished; zero means now.
	EndTime time.Time
	// Queued is how long the call waited for a dispatch slot after its
	// span opened; the span's duration includes it, Summary's does not.
	Queued time.Duration
}

// ToolSpan is one live tool call. End is idempotent.
type ToolSpan interface {
	Span() oteltrace.Span
	End(ToolSpanEnd)
}

// SubAgentSpanStarter is an optional capability for emitters that record a
// sub-agent run as its own agent span beneath the spawning tool call.
type SubAgentSpanStarter interface {
	StartSubAgentSpan(parent context.Context, start SubAgentSpanStart) AgentSpan
}

// SubAgentSpanStart identifies a sub-agent run at its start.
type SubAgentSpanStart struct {
	RunID       string
	ParentRunID string
	Config      *types.RunConfig
}

// AgentSpanEnd describes how a sub-agent run finished.
type AgentSpanEnd struct {
	Outcome string
	Turns   int
}

// AgentSpan is one live sub-agent run. Context carries the span for the
// child run's own spans to nest under; End is idempotent.
type AgentSpan interface {
	Context() context.Context
	End(AgentSpanEnd)
}
