package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// TestLoop_ToolResultEventReportsNameOutcomeAndDuration pins that each
// tool_result event names the tool the matching tool_call named and
// reports whether the call failed and how long it ran.
func TestLoop_ToolResultEventReportsNameOutcomeAndDuration(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "failing_tool",
		Description: "Always fails",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			time.Sleep(15 * time.Millisecond)
			return "", errors.New("boom")
		},
	})
	loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_ok", Name: "test_tool", Input: map[string]any{}},
			{Type: "tool_call", ID: "tc_bad", Name: "failing_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	tp := &recordingTransport{}
	loop.Transport = tp

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	results := eventsOfType(tp, "tool_result")
	if len(results) != 2 {
		t.Fatalf("got %d tool_result events, want 2", len(results))
	}
	ok, bad := results[0], results[1]
	if ok.ToolUseID != "tc_ok" || ok.Name != "test_tool" || ok.IsError {
		t.Errorf("first tool_result = id %q name %q isError %v, want tc_ok test_tool false", ok.ToolUseID, ok.Name, ok.IsError)
	}
	if bad.ToolUseID != "tc_bad" || bad.Name != "failing_tool" || !bad.IsError {
		t.Errorf("second tool_result = id %q name %q isError %v, want tc_bad failing_tool true", bad.ToolUseID, bad.Name, bad.IsError)
	}
	if bad.DurationMs < 15 {
		t.Errorf("failing_tool duration = %dms, want at least the 15ms it slept", bad.DurationMs)
	}
}
