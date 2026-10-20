package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// recordingEmitter is a test double that records every call into a slice
// without writing anywhere. Used to assert forwarding behaviour on the
// NestedJSONLEmitter without coupling tests to a particular wire format.
type recordingEmitter struct {
	mu                sync.Mutex
	starts            []startCall
	turns             []types.TurnTrace
	turnRecords       []types.TurnRecord
	toolCalls         []types.ToolCallTrace
	permissionDenials int
	finishes          []string
	finishErr         error
	finishRet         *types.RunTrace
	finishCtxs        []context.Context
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

func (r *recordingEmitter) RecordPermissionDenial() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.permissionDenials++
}

func (r *recordingEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishes = append(r.finishes, outcome)
	r.finishCtxs = append(r.finishCtxs, ctx)
	return r.finishRet, r.finishErr
}

func TestNestedJSONLEmitter_ForwardsTurnsAndToolCalls(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	parent := &recordingEmitter{}
	child := NewNestedJSONLEmitter(parent, "parent-run-1", context.Background())

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
	child.RecordPermissionDenial()

	must.Len(parent.turns, 1, "forwarded turns")
	must.Len(parent.toolCalls, 1, "forwarded tool calls")
	must.Equal(1, parent.permissionDenials, "forwarded permission denials")

	turn := parent.turns[0]
	check.Equal("sub-run-1", turn.RunID, "forwarded turn RunID")
	check.Equal("parent-run-1", turn.ParentRunID, "forwarded turn ParentRunID")
	check.Equal("sub-run-1", parent.toolCalls[0].RunID, "forwarded tool call RunID")
	check.Equal("parent-run-1", parent.toolCalls[0].ParentRunID, "forwarded tool call ParentRunID")
	// Original turn fields must round-trip unchanged.
	check.Equal(100, turn.Tokens.Input, "forwarded turn input tokens")
	check.Equal(50, turn.Tokens.Output, "forwarded turn output tokens")
	check.Equal("end_turn", turn.StopReason, "forwarded turn StopReason")
}

func TestNestedJSONLEmitter_ForwardsToolsLoaded(t *testing.T) {
	must := require.New(t)
	parent := &recordingEmitter{}
	child := NewNestedJSONLEmitter(parent, "parent-run-1", context.Background())
	child.Start("sub-run-1", nil)

	child.RecordTurn(types.TurnTrace{Turn: 0, ToolsLoaded: []string{"mcp_github_create_issue"}})
	child.RecordTurnRecord(types.TurnRecord{Turn: 0, ToolsLoaded: []string{"mcp_github_create_issue"}})

	must.Len(parent.turns, 1)
	must.Equal([]string{"mcp_github_create_issue"}, parent.turns[0].ToolsLoaded)
	must.Len(parent.turnRecords, 1)
	must.Equal([]string{"mcp_github_create_issue"}, parent.turnRecords[0].ToolsLoaded)
	must.Equal("sub-run-1", parent.turnRecords[0].RunID)
}

func TestNestedJSONLEmitter_DoesNotCallParentStartOrFinish(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	parent := &recordingEmitter{}
	child := NewNestedJSONLEmitter(parent, "parent-run-1", context.Background())

	child.Start("sub-run-1", &types.RunConfig{Mode: "execution"})

	check.Empty(parent.starts, "NestedJSONLEmitter.Start must not call parent.Start")

	_, err := child.Finish(context.Background(), "success")
	must.NoError(err, "child Finish")

	check.Empty(parent.finishes, "NestedJSONLEmitter.Finish must not call parent.Finish")
}

func TestNestedJSONLEmitter_FinishReturnsLocalRunTrace(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	parent := &recordingEmitter{}
	child := NewNestedJSONLEmitter(parent, "parent-run-1", context.Background())

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
		Tokens:     types.TokenUsage{Input: 10, Output: 20, CacheRead: 4},
		StopReason: "end_turn",
		DurationMs: 5,
	})
	child.RecordTurn(types.TurnTrace{
		Turn:          1,
		Tokens:        types.TokenUsage{Input: 30, Output: 40, CacheRead: 6, CacheWrite: 2, Reasoning: 3},
		InputReported: true,
		StopReason:    "end_turn",
		DurationMs:    5,
	})
	child.RecordToolCall(types.ToolCallTrace{
		Name:       "test_tool",
		DurationMs: 2,
		Success:    true,
	})
	child.RecordPermissionDenial()
	child.RecordPermissionDenial()

	rt, err := child.Finish(context.Background(), "success")
	must.NoError(err, "child Finish")
	must.NotNil(rt)

	check.Equal("sub-run-1", rt.ID, "RunTrace.ID")
	check.Equal(2, rt.Turns, "RunTrace.Turns")
	wantTokens := types.TokenUsage{Input: 40, Output: 60, CacheRead: 10, CacheWrite: 2, Reasoning: 3}
	check.Equal(wantTokens, rt.TokenUsage, "RunTrace.TokenUsage")
	check.Len(rt.ToolCalls, 1, "RunTrace.ToolCalls")
	check.Equal(2, rt.PermissionDenials, "RunTrace.PermissionDenials")
	check.Equal("success", rt.Outcome, "RunTrace.Outcome")
	check.Equal("secret://[REDACTED]", rt.Config.Provider.APIKeyRef, "APIKeyRef redacted in returned RunTrace")

	encoded, err := json.Marshal(rt)
	must.NoError(err, "marshal RunTrace")
	check.Contains(string(encoded),
		`"tokenUsage":{"input":40,"output":60,"cacheRead":10,"cacheWrite":2,"reasoning":3}`,
		"encoded RunTrace token usage breakdown")

	parent.mu.Lock()
	forwarded := append([]types.TurnTrace(nil), parent.turns...)
	parent.mu.Unlock()
	must.Len(forwarded, 2, "forwarded turns")
	check.Equal(types.TokenUsage{Input: 30, Output: 40, CacheRead: 6, CacheWrite: 2, Reasoning: 3},
		forwarded[1].Tokens, "forwarded turn usage breakdown")
	check.True(forwarded[1].InputReported, "forwarded turn InputReported")
}

// TestNestedJSONLEmitter_ThinkingBlocksScrubbedOnParentFile pins that a
// sub-agent's thinking blocks reach the parent's JSONL file under the same
// rules as the parent's own: the signature and redacted data never persist,
// and a secret in the thinking text is redacted.
func TestNestedJSONLEmitter_ThinkingBlocksScrubbedOnParentFile(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	parent := NewJSONLTraceEmitter(&buf, false)
	parent.Start("parent-run-1", nil)

	const signature = "child-thinking-signature-do-not-persist"
	const redacted = "child-redacted-data-do-not-persist"
	const secret = "sk-ant-api03-childthinkingleak"
	thinking := []types.ContentBlock{
		{Type: "thinking", Text: "the key is " + secret, ThoughtSignature: signature},
		{Type: "redacted_thinking", ThoughtSignature: redacted},
		{Type: "text", Text: "done"},
	}

	child := NewNestedJSONLEmitter(parent, "parent-run-1", context.Background())
	child.Start("sub-run-1", nil)
	child.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{
			Model:    "claude-sonnet-5-5",
			Messages: []types.Message{{Role: "assistant", Content: thinking}},
		},
		ModelOutput: thinking,
	})

	_, err := child.Finish(context.Background(), "success")
	must.NoError(err, "child Finish")
	_, err = parent.Finish(context.Background(), "success")
	must.NoError(err, "parent Finish")

	onDisk := buf.String()
	must.Contains(onDisk, `"kind":"turn_record"`, "child turn record on the parent file")

	for _, tc := range []struct {
		name   string
		leaked string
	}{
		{name: "signature is not persisted", leaked: signature},
		{name: "redacted data is not persisted", leaked: redacted},
		{name: "secret is not persisted", leaked: secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(onDisk, tc.leaked)
		})
	}

	must.Contains(onDisk, `"type":"thinking","text":"the key is [REDACTED]`,
		"thinking text should persist scrubbed")
}

func TestNestedJSONLEmitter_NilParentIsSafe(t *testing.T) {
	must := require.New(t)
	// A nil parent must not panic. This protects against accidental
	// construction paths in tests or future callers.
	child := NewNestedJSONLEmitter(nil, "parent-run-1", context.Background())
	child.Start("sub-run-1", nil)
	child.RecordTurn(types.TurnTrace{Turn: 0})
	child.RecordToolCall(types.ToolCallTrace{Name: "t"})
	child.RecordPermissionDenial()

	_, err := child.Finish(context.Background(), "success")
	must.NoError(err, "Finish with nil parent")
}
