package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// structuredEchoTool returns a tool with a StructuredHandler that yields a
// text fallback, a structured payload, and a kind discriminator.
func structuredEchoTool() *tool.Tool {
	return &tool.Tool{
		Name:        "structured_echo",
		Description: "test structured tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		StructuredHandler: func(_ context.Context, _ json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{
				Text:       "text fallback",
				Structured: json.RawMessage(`{"field":"value"}`),
				Kind:       "command_result",
			}, nil
		},
	}
}

// plainEchoTool exposes only a plain Handler — it must produce a zero
// structuredOutput so a text-only tool stays text-only.
func plainEchoTool() *tool.Tool {
	return &tool.Tool{
		Name:        "plain_echo",
		Description: "test plain tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "plain text", nil
		},
	}
}

func TestDispatch_StructuredHandlerCarriesPayload(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, structuredEchoTool())

	out, success, _, structured := loop.dispatchToolCallCategorized(
		context.Background(),
		types.ToolCall{ID: "tc1", Name: "structured_echo", Input: json.RawMessage(`{}`)},
	)
	if !success {
		t.Fatalf("expected success, got failure: %q", out)
	}
	if out != "text fallback" {
		t.Errorf("text fallback mismatch: %q", out)
	}
	if string(structured.payload) != `{"field":"value"}` {
		t.Errorf("structured payload mismatch: %s", structured.payload)
	}
	if structured.kind != "command_result" {
		t.Errorf("structured kind mismatch: %q", structured.kind)
	}
}

func TestDispatch_PlainHandlerHasNilStructured(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, plainEchoTool())

	out, success, _, structured := loop.dispatchToolCallCategorized(
		context.Background(),
		types.ToolCall{ID: "tc1", Name: "plain_echo", Input: json.RawMessage(`{}`)},
	)
	if !success || out != "plain text" {
		t.Fatalf("unexpected result: success=%v out=%q", success, out)
	}
	if structured.payload != nil {
		t.Errorf("plain-Handler tool must yield nil structured payload, got: %s", structured.payload)
	}
	if structured.kind != "" {
		t.Errorf("plain-Handler tool must yield empty kind, got: %q", structured.kind)
	}
}

// TestPlanAndDispatch_StructuredFlowsToResult pins that the structured payload
// and kind reach both the ToolResult (model-facing) and the ToolCallRecord
// (trace).
func TestPlanAndDispatch_StructuredFlowsToResult(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, structuredEchoTool())

	results, records, stall := loop.planAndDispatch(
		context.Background(),
		&types.RunConfig{Mode: "execution"},
		[]types.ToolCall{{ID: "tc1", Name: "structured_echo", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic",
		"claude-sonnet-4-6",
	)
	if stall != "" {
		t.Fatalf("unexpected stall outcome: %q", stall)
	}
	if len(results) != 1 || len(records) != 1 {
		t.Fatalf("expected 1 result/record, got %d/%d", len(results), len(records))
	}
	if results[0].Content != "text fallback" {
		t.Errorf("result text fallback mismatch: %q", results[0].Content)
	}
	if string(results[0].Structured) != `{"field":"value"}` {
		t.Errorf("ToolResult.Structured mismatch: %s", results[0].Structured)
	}
	if results[0].Kind != "command_result" {
		t.Errorf("ToolResult.Kind mismatch: %q", results[0].Kind)
	}
	if string(records[0].Structured) != `{"field":"value"}` {
		t.Errorf("ToolCallRecord.Structured mismatch: %s", records[0].Structured)
	}
	if records[0].Kind != "command_result" {
		t.Errorf("ToolCallRecord.Kind mismatch: %q", records[0].Kind)
	}
}

// TestPlanAndDispatch_StructuredResultAndRecordDoNotAlias pins that the
// ToolResult and ToolCallRecord receive independent copies of the structured
// payload rather than aliasing one backing slice. The two flow to different
// surfaces (model-facing result vs. trace record), and a later in-place edit
// (scrub, redaction) to one must not silently mutate the other.
func TestPlanAndDispatch_StructuredResultAndRecordDoNotAlias(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, structuredEchoTool())

	results, records, stall := loop.planAndDispatch(
		context.Background(),
		&types.RunConfig{Mode: "execution"},
		[]types.ToolCall{{ID: "tc1", Name: "structured_echo", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic",
		"claude-sonnet-4-6",
	)
	if stall != "" {
		t.Fatalf("unexpected stall outcome: %q", stall)
	}
	if len(results) != 1 || len(records) != 1 {
		t.Fatalf("expected 1 result/record, got %d/%d", len(results), len(records))
	}
	if len(results[0].Structured) == 0 || len(records[0].Structured) == 0 {
		t.Fatalf("both structured payloads must be populated")
	}
	// Mutate the result's payload in place; the record's copy must be unaffected.
	results[0].Structured[0] = 'X'
	if string(records[0].Structured) != `{"field":"value"}` {
		t.Errorf("record payload mutated via result alias: %s", records[0].Structured)
	}
}
