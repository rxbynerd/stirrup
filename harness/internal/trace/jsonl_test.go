package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestJSONLTraceEmitter_FullLifecycle(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	timeout := 300
	config := &types.RunConfig{
		RunID:    "run-123",
		Mode:     "execution",
		MaxTurns: 20,
		Timeout:  &timeout,
		Provider: types.ProviderConfig{
			APIKeyRef: "secret://ANTHROPIC_KEY",
		},
	}

	emitter.Start("run-123", config)

	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		ToolCalls:  2,
		StopReason: "tool_use",
		DurationMs: 1500,
	})
	emitter.RecordTurn(types.TurnTrace{
		Turn:       2,
		Tokens:     types.TokenUsage{Input: 200, Output: 75},
		ToolCalls:  0,
		StopReason: "end_turn",
		DurationMs: 800,
	})

	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "read_file",
		DurationMs: 10,
		Success:    true,
	})
	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "write_file",
		DurationMs: 25,
		Success:    true,
	})

	trace, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify trace fields.
	if trace.ID != "run-123" {
		t.Errorf("ID: got %q, want %q", trace.ID, "run-123")
	}
	if trace.Turns != 2 {
		t.Errorf("Turns: got %d, want 2", trace.Turns)
	}
	if trace.TokenUsage.Input != 300 || trace.TokenUsage.Output != 125 {
		t.Errorf("TokenUsage: got %+v, want {300, 125}", trace.TokenUsage)
	}
	if len(trace.ToolCalls) != 2 {
		t.Errorf("ToolCalls: got %d, want 2", len(trace.ToolCalls))
	}
	if trace.Outcome != "success" {
		t.Errorf("Outcome: got %q, want %q", trace.Outcome, "success")
	}

	// Verify config was redacted.
	if trace.Config.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("APIKeyRef should be redacted, got %q", trace.Config.Provider.APIKeyRef)
	}

	// Verify JSONL output is valid.
	var written types.RunTrace
	if err := json.Unmarshal(buf.Bytes(), &written); err != nil {
		t.Fatalf("unmarshal written trace: %v", err)
	}
	if written.ID != "run-123" {
		t.Errorf("written ID: got %q, want %q", written.ID, "run-123")
	}
}

func TestJSONLTraceEmitter_EmptyRun(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	emitter.Start("run-empty", nil)

	trace, err := emitter.Finish(context.Background(), "error")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if trace.Turns != 0 {
		t.Errorf("Turns: got %d, want 0", trace.Turns)
	}
	if trace.Outcome != "error" {
		t.Errorf("Outcome: got %q, want %q", trace.Outcome, "error")
	}
	if buf.Len() == 0 {
		t.Error("expected output to be written")
	}
}
