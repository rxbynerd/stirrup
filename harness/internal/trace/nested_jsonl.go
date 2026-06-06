package trace

import (
	"context"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// NestedJSONLEmitter is a TraceEmitter that wraps a parent emitter and
// forwards events to it live, tagging each event with the child's runID
// and the parentRunID. It is used by the harness to surface sub-agent
// telemetry on the parent's trace stream so a single trace file (or OTel
// stream) carries the full call graph rather than discarding child
// observations.
//
// TODO(#89): When the parent emitter is an OTelTraceEmitter, the
// turn[N] spans this emitter forwards still parent off the
// OTelTraceEmitter's internal rootCtx (derived from
// context.Background()), so #55's AC-2 — child turn[N] spans nesting
// under the parent's tool.spawn_agent — is only partly satisfied.
// The preferred long-term fix injects a parentCtx into
// OTelTraceEmitter for child emitter variants; tracked in #89.
//
// The wrapped emitter is NOT started or finished by NestedJSONLEmitter:
//
//   - parent.Start was called by the parent's own Run.
//   - parent.Finish will be called by the parent's own Run.
//
// Calling either of those on the parent from inside the child loop would
// reset the parent's accumulated trace state. NestedJSONLEmitter therefore
// forwards only the per-event RecordTurn and RecordToolCall hooks, while
// keeping its own local accumulator so its Finish can return a *RunTrace
// describing the child run (the existing core.SpawnSubAgent flow consumes
// runTrace.Outcome and runTrace.Turns from the returned value).
type NestedJSONLEmitter struct {
	parent      TraceEmitter
	parentRunID string

	mu                sync.Mutex
	runID             string
	config            *types.RunConfig
	startedAt         time.Time
	turns             []types.TurnTrace
	toolCalls         []types.ToolCallTrace
	permissionDenials int
}

// NewNestedJSONLEmitter returns an emitter that forwards Turn/ToolCall
// events to parent, tagged with parentRunID and the child's runID set
// at Start time. parent must be non-nil; the child is responsible for
// calling Finish on the returned emitter to obtain the *RunTrace
// describing its own run.
func NewNestedJSONLEmitter(parent TraceEmitter, parentRunID string) *NestedJSONLEmitter {
	return &NestedJSONLEmitter{
		parent:      parent,
		parentRunID: parentRunID,
	}
}

// Start records the child's runID and config. It does NOT call
// parent.Start: the parent already started its own root run when the
// outer Run() began, and re-starting would reset its accumulated state.
func (e *NestedJSONLEmitter) Start(runID string, config *types.RunConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.runID = runID
	e.config = config
	e.startedAt = time.Now()
	e.turns = nil
	e.toolCalls = nil
	e.permissionDenials = 0
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
//
// The tag is load-bearing for the OTel emitter's content capture: it
// pairs turn records with turn summaries by RunID+Turn, so an untagged
// child record could merge onto the parent's same-numbered turn span.
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

// Finish builds and returns the child's RunTrace. It does NOT call
// parent.Finish: the parent's own Run finishes its emitter exactly
// once when the outer run completes; calling it from a child would
// truncate the parent's trace mid-run.
func (e *NestedJSONLEmitter) Finish(_ context.Context, outcome string) (*types.RunTrace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	var totalTokens types.TokenUsage
	for _, turn := range e.turns {
		totalTokens.Input += turn.Tokens.Input
		totalTokens.Output += turn.Tokens.Output
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
		ID:                e.runID,
		Config:            redactedConfig,
		StartedAt:         e.startedAt,
		CompletedAt:       now,
		Turns:             len(e.turns),
		TokenUsage:        totalTokens,
		ToolCalls:         summaries,
		PermissionDenials: e.permissionDenials,
		Outcome:           outcome,
	}, nil
}
