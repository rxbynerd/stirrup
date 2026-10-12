package trace

import (
	"context"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// NestedJSONLEmitter is a TraceEmitter that wraps a parent emitter and
// forwards events to it live, tagging each event with the child's runID
// and the parentRunID, so a single trace file (or OTel stream) carries
// the full call graph rather than discarding sub-agent observations.
// When the parent records live spans, the child run gets its own agent
// span beneath the spawning tool call, and the child's request and tool
// spans nest under it.
//
// The wrapped emitter is NOT started or finished by NestedJSONLEmitter:
//
//   - parent.Start was called by the parent's own Run.
//   - parent.Finish will be called by the parent's own Run.
//
// Calling either of those on the parent from inside the child loop would
// reset the parent's accumulated trace state. NestedJSONLEmitter therefore
// forwards only the per-event hooks, while keeping its own local
// accumulator so its Finish can return a *RunTrace describing the child
// run (the existing core.SpawnSubAgent flow consumes runTrace.Outcome and
// runTrace.Turns from the returned value). Its own Start and Finish reach
// a parent implementing SubRunRecorder as the child's start and outcome
// summary instead.
type NestedJSONLEmitter struct {
	parent      TraceEmitter
	parentRunID string
	// spanParent is the spawning tool call's context, the parent of the
	// child's agent span.
	spanParent context.Context //nolint:containedctx // span parent captured at spawn

	mu                 sync.Mutex
	runID              string
	config             *types.RunConfig
	startedAt          time.Time
	turns              []types.TurnTrace
	toolCalls          []types.ToolCallTrace
	permissionDenials  int
	finalAssistantText string
	stopDetails        *types.StopDetails
	agentSpan          AgentSpan
}

var _ FinalAssistantTextRecorder = (*NestedJSONLEmitter)(nil)
var _ StopDetailsRecorder = (*NestedJSONLEmitter)(nil)
var _ CommandOutputRecorder = (*NestedJSONLEmitter)(nil)
var _ SubRunRecorder = (*NestedJSONLEmitter)(nil)
var _ RootContextProvider = (*NestedJSONLEmitter)(nil)
var _ LiveSpanStarter = (*NestedJSONLEmitter)(nil)

// NewNestedJSONLEmitter returns an emitter that forwards Turn/ToolCall
// events to parent, tagged with parentRunID and the child's runID set
// at Start time. parent must be non-nil; the child is responsible for
// calling Finish on the returned emitter to obtain the *RunTrace
// describing its own run. spanParent is the spawning tool call's
// context.
func NewNestedJSONLEmitter(parent TraceEmitter, parentRunID string, spanParent context.Context) *NestedJSONLEmitter {
	return &NestedJSONLEmitter{
		parent:      parent,
		parentRunID: parentRunID,
		spanParent:  spanParent,
	}
}

// Start records the child's runID and config, reports the child's start
// to a parent implementing SubRunRecorder, and opens the child's agent
// span on a parent implementing SubAgentSpanStarter. It does NOT call
// parent.Start: the parent already started its own root run when the
// outer Run() began, and re-starting would reset its accumulated state.
func (e *NestedJSONLEmitter) Start(runID string, config *types.RunConfig) {
	var agentSpan AgentSpan
	if starter, ok := e.parent.(SubAgentSpanStarter); ok && e.spanParent != nil {
		agentSpan = starter.StartSubAgentSpan(e.spanParent, SubAgentSpanStart{
			RunID:       runID,
			ParentRunID: e.parentRunID,
			Config:      config,
		})
	}

	e.mu.Lock()
	e.runID = runID
	e.config = config
	e.startedAt = time.Now()
	e.turns = nil
	e.toolCalls = nil
	e.permissionDenials = 0
	e.finalAssistantText = ""
	e.stopDetails = nil
	e.agentSpan = agentSpan
	startedAt := e.startedAt
	e.mu.Unlock()

	if recorder, ok := e.parent.(SubRunRecorder); ok {
		recorder.RecordSubRunStarted(runID, e.parentRunID, startedAt, config)
	}
}

// RootContext returns the child's agent span context when one was
// opened, else the spawn context.
func (e *NestedJSONLEmitter) RootContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.agentSpan != nil {
		return e.agentSpan.Context()
	}
	return e.spanParent
}

// StartChatSpan forwards to a parent that records live spans, tagging
// the request with the child's run IDs; nil otherwise.
func (e *NestedJSONLEmitter) StartChatSpan(parent context.Context, start ChatSpanStart) ChatSpan {
	starter, ok := e.parent.(LiveSpanStarter)
	if !ok {
		return nil
	}
	start.RunID, start.ParentRunID = e.tagRunIDs(start.RunID, start.ParentRunID)
	return starter.StartChatSpan(parent, start)
}

// StartToolSpan forwards like StartChatSpan.
func (e *NestedJSONLEmitter) StartToolSpan(parent context.Context, start ToolSpanStart) ToolSpan {
	starter, ok := e.parent.(LiveSpanStarter)
	if !ok {
		return nil
	}
	start.RunID, start.ParentRunID = e.tagRunIDs(start.RunID, start.ParentRunID)
	return starter.StartToolSpan(parent, start)
}

// tagRunIDs fills empty run IDs with the child's, leaving a deeper
// sub-run's own tags in place.
func (e *NestedJSONLEmitter) tagRunIDs(runID, parentRunID string) (string, string) {
	if runID != "" {
		return runID, parentRunID
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runID, e.parentRunID
}

// RecordTurn appends to the child's local trace and forwards a tagged
// copy to the parent emitter. The child's local copy is left untagged
// so its own Finish() returns a self-consistent RunTrace (the child's
// runID is already on the wrapping RunTrace).
func (e *NestedJSONLEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	e.turns = append(e.turns, turn)
	e.mu.Unlock()

	if e.parent == nil {
		return
	}
	tagged := turn
	tagged.RunID = e.runID
	tagged.ParentRunID = e.parentRunID
	e.parent.RecordTurn(tagged)
}

// RecordTurnRecord forwards the child's full transcript turn to the
// parent emitter, tagged with the child's runID and the parentRunID
// like RecordTurn and RecordToolCall. The child's RunRecording is
// reassembled on the reader side from the streamed events on the
// parent's file, so a nested run's transcripts land in the same JSONL
// stream as the parent's. The child does not retain a local copy:
// TurnRecord is the streaming-only surface, distinct from the summary
// RunTrace the child's Finish returns.
func (e *NestedJSONLEmitter) RecordTurnRecord(turn types.TurnRecord) {
	if e.parent == nil {
		return
	}
	tagged := turn
	tagged.RunID = e.runID
	tagged.ParentRunID = e.parentRunID
	e.parent.RecordTurnRecord(tagged)
}

// RecordToolCall appends to the child's local trace and forwards a
// tagged copy to the parent emitter.
func (e *NestedJSONLEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	e.toolCalls = append(e.toolCalls, call)
	e.mu.Unlock()

	if e.parent == nil {
		return
	}
	tagged := call
	tagged.RunID = e.runID
	tagged.ParentRunID = e.parentRunID
	e.parent.RecordToolCall(tagged)
}

// RecordPermissionDenial records the child's local count and forwards the
// denial into the parent aggregate, matching nested tool-call forwarding.
func (e *NestedJSONLEmitter) RecordPermissionDenial() {
	e.mu.Lock()
	e.permissionDenials++
	e.mu.Unlock()

	if e.parent == nil {
		return
	}
	e.parent.RecordPermissionDenial()
}

// RecordFinalAssistantText stores the child run's final assistant text
// on the local accumulator so the child's own Finish returns it. It is
// deliberately NOT forwarded to the parent: the parent stamps its own
// final text at its own Finish, and forwarding would clobber that single
// stored value — the same reason sub-agent system instructions are not
// forwarded (see SystemInstructionsRecorder).
func (e *NestedJSONLEmitter) RecordFinalAssistantText(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finalAssistantText = text
}

// RecordStopDetails stores the child run's final stop details locally and,
// like RecordFinalAssistantText, does not forward them to the parent.
func (e *NestedJSONLEmitter) RecordStopDetails(details *types.StopDetails) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopDetails = details
}

func (e *NestedJSONLEmitter) RecordCommandOutput(record types.CommandOutputRecord) {
	if parent, ok := e.parent.(CommandOutputRecorder); ok {
		parent.RecordCommandOutput(record)
	}
}

// RecordSubRunStarted forwards a nested child's start unchanged, so a
// deeper sub-run still lands on the root file.
func (e *NestedJSONLEmitter) RecordSubRunStarted(runID, parentRunID string, startedAt time.Time, config *types.RunConfig) {
	if recorder, ok := e.parent.(SubRunRecorder); ok {
		recorder.RecordSubRunStarted(runID, parentRunID, startedAt, config)
	}
}

// RecordSubRunFinished forwards a nested child's outcome unchanged.
func (e *NestedJSONLEmitter) RecordSubRunFinished(summary types.SubRunSummary) {
	if recorder, ok := e.parent.(SubRunRecorder); ok {
		recorder.RecordSubRunFinished(summary)
	}
}

// Finish builds and returns the child's RunTrace and reports its outcome
// summary to a parent implementing SubRunRecorder. It does NOT call
// parent.Finish: the parent's own Run finishes its emitter exactly
// once when the outer run completes; calling it from a child would
// truncate the parent's trace mid-run.
func (e *NestedJSONLEmitter) Finish(_ context.Context, outcome string) (*types.RunTrace, error) {
	runTrace := e.buildRunTrace(outcome)
	e.mu.Lock()
	agentSpan := e.agentSpan
	e.agentSpan = nil
	e.mu.Unlock()
	if agentSpan != nil {
		agentSpan.End(AgentSpanEnd{Outcome: runTrace.Outcome, Turns: runTrace.Turns})
	}
	if recorder, ok := e.parent.(SubRunRecorder); ok {
		recorder.RecordSubRunFinished(types.SubRunSummary{
			RunID:       runTrace.ID,
			ParentRunID: e.parentRunID,
			Outcome:     runTrace.Outcome,
			Turns:       runTrace.Turns,
			TokenUsage:  runTrace.TokenUsage,
			StartedAt:   runTrace.StartedAt,
			CompletedAt: runTrace.CompletedAt,
			DurationMs:  runTrace.CompletedAt.Sub(runTrace.StartedAt).Milliseconds(),
		})
	}
	return runTrace, nil
}

func (e *NestedJSONLEmitter) buildRunTrace(outcome string) *types.RunTrace {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	var totalTokens types.TokenUsage
	for _, turn := range e.turns {
		totalTokens.Add(turn.Tokens)
	}

	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if e.config != nil {
		redactedConfig = e.config.Redact()
	}

	return &types.RunTrace{
		ID:                 e.runID,
		Config:             redactedConfig,
		StartedAt:          e.startedAt,
		CompletedAt:        now,
		Turns:              len(e.turns),
		TokenUsage:         totalTokens,
		ToolCalls:          summaries,
		PermissionDenials:  e.permissionDenials,
		Outcome:            outcome,
		StopDetails:        e.stopDetails,
		FinalAssistantText: e.finalAssistantText,
	}
}
