package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// structuredEchoTool is a sync tool exposing a StructuredHandler that returns
// both a text fallback and a typed structured payload, exercising the issue
// #231 dispatch seam.
func structuredEchoTool() *tool.Tool {
	return &tool.Tool{
		Name:        "structured_echo",
		Description: "test structured tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		StructuredHandler: func(_ context.Context, _ json.RawMessage) (string, json.RawMessage, error) {
			return "text fallback", json.RawMessage(`{"field":"value"}`), nil
		},
	}
}

// plainEchoTool exposes only a plain Handler — it must produce a nil
// structured payload so a text-only tool stays text-only.
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
	if string(structured) != `{"field":"value"}` {
		t.Errorf("structured payload mismatch: %s", structured)
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
	if structured != nil {
		t.Errorf("plain-Handler tool must yield nil structured, got: %s", structured)
	}
}

// TestPlanAndDispatch_StructuredFlowsToResult pins that the structured payload
// reaches both the ToolResult (model-facing) and the ToolCallRecord (trace).
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
	if string(records[0].Structured) != `{"field":"value"}` {
		t.Errorf("ToolCallRecord.Structured mismatch: %s", records[0].Structured)
	}
}
