package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

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
	require.True(t, success, "expected success, got failure: %q", out)

	t.Run("text fallback", func(t *testing.T) {
		require.Equal(t, "text fallback", out)
	})
	t.Run("structured payload", func(t *testing.T) {
		require.JSONEq(t, `{"field":"value"}`, string(structured.payload))
	})
	t.Run("structured kind", func(t *testing.T) {
		require.Equal(t, "command_result", structured.kind)
	})
}

func TestDispatch_PlainHandlerHasNilStructured(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, plainEchoTool())

	out, success, _, structured := loop.dispatchToolCallCategorized(
		context.Background(),
		types.ToolCall{ID: "tc1", Name: "plain_echo", Input: json.RawMessage(`{}`)},
	)
	require.True(t, success, "expected success, got failure: %q", out)
	require.Equal(t, "plain text", out)

	t.Run("nil structured payload", func(t *testing.T) {
		require.Nil(t, structured.payload, "plain-Handler tool must yield nil structured payload")
	})
	t.Run("empty kind", func(t *testing.T) {
		require.Empty(t, structured.kind, "plain-Handler tool must yield empty kind")
	})
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
	require.Empty(t, stall, "unexpected stall outcome")
	require.Len(t, results, 1)
	require.Len(t, records, 1)

	t.Run("tool result", func(t *testing.T) {
		require.Equal(t, "text fallback", results[0].Content)
		require.JSONEq(t, `{"field":"value"}`, string(results[0].Structured))
		require.Equal(t, "command_result", results[0].Kind)
	})
	t.Run("tool call record", func(t *testing.T) {
		require.JSONEq(t, `{"field":"value"}`, string(records[0].Structured))
		require.Equal(t, "command_result", records[0].Kind)
	})
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
	require.Empty(t, stall, "unexpected stall outcome")
	require.Len(t, results, 1)
	require.Len(t, records, 1)
	require.NotEmpty(t, results[0].Structured, "result structured payload must be populated")
	require.NotEmpty(t, records[0].Structured, "record structured payload must be populated")

	// Mutate the result's payload in place; the record's copy must be unaffected.
	results[0].Structured[0] = 'X'
	require.JSONEq(t, `{"field":"value"}`, string(records[0].Structured))
}
