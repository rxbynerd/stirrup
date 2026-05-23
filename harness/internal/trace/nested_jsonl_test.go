package trace

import (
	"context"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// recordingEmitter is a test double that records every call into a slice
// without writing anywhere. Used to assert forwarding behaviour on the
// NestedJSONLEmitter without coupling tests to a particular wire format.
type recordingEmitter struct {
	mu          sync.Mutex
	starts      []startCall
	turns       []types.TurnTrace
	turnRecords []types.TurnRecord
	toolCalls   []types.ToolCallTrace
	finishes    []string
	finishErr   error
	finishRet   *types.RunTrace
	finishCtxs  []context.Context
}

type startCall struct {
	runID  string
	config *types.RunConfig
}

func (r *recordingEmitter) Start(runID string, config *types.RunConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, startCall{runID: runID, config: config})
}

func (r *recordingEmitter) RecordTurn(turn types.TurnTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, turn)
}

func (r *recordingEmitter) RecordTurnRecord(turn types.TurnRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnRecords = append(r.turnRecords, turn)
}

func (r *recordingEmitter) RecordToolCall(call types.ToolCallTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolCalls = append(r.toolCalls, call)
}

func (r *recordingEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishes = append(r.finishes, outcome)
	r.finishCtxs = append(r.finishCtxs, ctx)
	return r.finishRet, r.finishErr
}

func TestNestedJSONLEmitter_ForwardsTurnsAndToolCalls(t *testing.T) {
	parent := &recordingEmitter{}
	child := NewNestedJSONLEmitter(parent, "parent-run-1")

	child.Start("sub-run-1", &types.RunConfig{Mode: "execution"})

	child.RecordTurn(types.TurnTrace{
		Turn:       0,
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		StopReason: "end_turn",
		DurationMs: 1000,
	})
	child.RecordToolCall(types.ToolCallTrace{
		Name:       "test_tool",
		DurationMs: 5,
		Success:    true,
	})

	if len(parent.turns) != 1 {
		t.Fatalf("expected 1 forwarded turn, got %d", len(parent.turns))
	}
	if len(parent.toolCalls) != 1 {
		t.Fatalf("expected 1 forwarded tool call, got %d", len(parent.toolCalls))
	}
	if parent.turns[0].RunID != "sub-run-1" {
		t.Errorf("forwarded turn RunID: got %q, want %q", parent.turns[0].RunID, "sub-run-1")
	}
	if parent.turns[0].ParentRunID != "parent-run-1" {
		t.Errorf("forwarded turn ParentRunID: got %q, want %q", parent.turns[0].ParentRunID, "parent-run-1")
	}
	if parent.toolCalls[0].RunID != "sub-run-1" {
		t.Errorf("forwarded tool call RunID: got %q, want %q", parent.toolCalls[0].RunID, "sub-run-1")
	}
	if parent.toolCalls[0].ParentRunID != "parent-run-1" {
		t.Errorf("forwarded tool call ParentRunID: got %q, want %q", parent.toolCalls[0].ParentRunID, "parent-run-1")
	}
	// Original turn fields must round-trip unchanged.
	if parent.turns[0].Tokens.Input != 100 || parent.turns[0].Tokens.Output != 50 {
		t.Errorf("forwarded turn token usage corrupted: %+v", parent.turns[0].Tokens)
	}
	if parent.turns[0].StopReason != "end_turn" {
		t.Errorf("forwarded turn StopReason: got %q, want %q", parent.turns[0].StopReason, "end_turn")
	}
}

func TestNestedJSONLEmitter_DoesNotCallParentStartOrFinish(t *testing.T) {
	parent := &recordingEmitter{}
	child := NewNestedJSONLEmitter(parent, "parent-run-1")

	child.Start("sub-run-1", &types.RunConfig{Mode: "execution"})

	if len(parent.starts) != 0 {
		t.Errorf("NestedJSONLEmitter.Start must not call parent.Start; got %d calls", len(parent.starts))
	}

	if _, err := child.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("child Finish: %v", err)
	}

	if len(parent.finishes) != 0 {
		t.Errorf("NestedJSONLEmitter.Finish must not call parent.Finish; got %d calls", len(parent.finishes))
	}
}

func TestNestedJSONLEmitter_FinishReturnsLocalRunTrace(t *testing.T) {
	parent := &recordingEmitter{}
	child := NewNestedJSONLEmitter(parent, "parent-run-1")

	cfg := &types.RunConfig{
		RunID: "sub-run-1",
		Mode:  "execution",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://ANTHROPIC_KEY",
		},
	}
	child.Start("sub-run-1", cfg)
	child.RecordTurn(types.TurnTrace{
		Turn:       0,
		Tokens:     types.TokenUsage{Input: 10, Output: 20},
		StopReason: "end_turn",
		DurationMs: 5,
	})
	child.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 30, Output: 40},
		StopReason: "end_turn",
		DurationMs: 5,
	})
	child.RecordToolCall(types.ToolCallTrace{
		Name:       "test_tool",
		DurationMs: 2,
		Success:    true,
	})

	rt, err := child.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("child Finish: %v", err)
	}
	if rt == nil {
		t.Fatal("child Finish returned nil RunTrace")
	}
	if rt.ID != "sub-run-1" {
		t.Errorf("RunTrace.ID: got %q, want %q", rt.ID, "sub-run-1")
	}
	if rt.Turns != 2 {
		t.Errorf("RunTrace.Turns: got %d, want 2", rt.Turns)
	}
	if rt.TokenUsage.Input != 40 || rt.TokenUsage.Output != 60 {
		t.Errorf("RunTrace.TokenUsage: got %+v, want {40,60}", rt.TokenUsage)
	}
	if len(rt.ToolCalls) != 1 {
		t.Errorf("RunTrace.ToolCalls: got %d, want 1", len(rt.ToolCalls))
	}
	if rt.Outcome != "success" {
		t.Errorf("RunTrace.Outcome: got %q, want %q", rt.Outcome, "success")
	}
	if rt.Config.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("APIKeyRef must be redacted in returned RunTrace, got %q", rt.Config.Provider.APIKeyRef)
	}
}

func TestNestedJSONLEmitter_NilParentIsSafe(t *testing.T) {
	// A nil parent must not panic. This protects against accidental
	// construction paths in tests or future callers.
	child := NewNestedJSONLEmitter(nil, "parent-run-1")
	child.Start("sub-run-1", nil)
	child.RecordTurn(types.TurnTrace{Turn: 0})
	child.RecordToolCall(types.ToolCallTrace{Name: "t"})
	if _, err := child.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish with nil parent: %v", err)
	}
}
