package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

var errFake = errors.New("fake provider error")

// fakeAdapter is a recording stub that captures the StreamParams it
// receives and replays a caller-supplied script of StreamEvents. Used
// by the normalizer tests to assert "what the wrapped provider saw on
// the wire" without bringing up real HTTP transport.
type fakeAdapter struct {
	receivedParams types.StreamParams
	scripted       []types.StreamEvent
	streamErr      error
	batchID        string
}

func (f *fakeAdapter) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	f.receivedParams = params
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan types.StreamEvent, len(f.scripted))
	for _, ev := range f.scripted {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// fakeBatchAdapter implements both ProviderAdapter and the batchModeAdapter
// duck-typed interface so the wrapper's pass-through is exercised.
type fakeBatchAdapter struct {
	fakeAdapter
}

func (f *fakeBatchAdapter) LastBatchID() string { return f.batchID }

// toolCallEvents drains ch and returns its tool_call events in order.
func toolCallEvents(ch <-chan types.StreamEvent) []types.StreamEvent {
	var calls []types.StreamEvent
	for ev := range ch {
		if ev.Type == "tool_call" {
			calls = append(calls, ev)
		}
	}
	return calls
}

func TestNormalizingAdapter_OpenAIToolNamesUnchanged(t *testing.T) {
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "openai-compatible")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "read_file", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	_, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	tools := inner.receivedParams.Tools
	t.Run("plain name unchanged", func(t *testing.T) {
		require.Equal(t, "read_file", tools[0].Name)
	})
	// OpenAI allows hyphens, so the name passes through verbatim.
	t.Run("hyphenated name unchanged", func(t *testing.T) {
		require.Equal(t, "mcp_jira_create-issue", tools[1].Name)
	})
}

func TestNormalizingAdapter_GeminiOutboundNamesSanitized(t *testing.T) {
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_slack_post.message", InputSchema: json.RawMessage(`{}`)},
		},
	}
	_, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	for i, td := range inner.receivedParams.Tools {
		t.Run(td.Name, func(t *testing.T) {
			require.False(t, strings.ContainsAny(td.Name, "-."),
				"tool %d name %q still contains a character Gemini rejects", i, td.Name)
		})
	}
}

func TestNormalizingAdapter_InboundToolCallReverseTranslated(t *testing.T) {
	inner := &fakeAdapter{
		scripted: []types.StreamEvent{
			{Type: "text_delta", Text: "hello"},
			{Type: "tool_call", ID: "call_1", Name: "mcp_jira_create_issue"},
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	ch, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	// The provider saw the normalised name.
	require.Equal(t, "mcp_jira_create_issue", inner.receivedParams.Tools[0].Name)

	calls := toolCallEvents(ch)
	require.NotEmpty(t, calls, "expected to observe a tool_call event")
	for _, ev := range calls {
		require.Equal(t, "mcp_jira_create-issue", ev.Name)
	}
}

func TestNormalizingAdapter_MultipleToolCallsInBatchReverseTranslated(t *testing.T) {
	// Every tool_call in a parallel-tool turn is reverse-translated, not
	// only the first. Gemini's policy is the strictest, so both MCP names
	// need normalising on the outbound side.
	inner := &fakeAdapter{
		scripted: []types.StreamEvent{
			{Type: "text_delta", Text: "running both"},
			{Type: "tool_call", ID: "call_1", Name: "mcp_jira_create_issue"},
			{Type: "tool_call", ID: "call_2", Name: "mcp_slack_post_message"},
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_slack_post.message", InputSchema: json.RawMessage(`{}`)},
		},
	}
	ch, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	var names []string
	for _, ev := range toolCallEvents(ch) {
		names = append(names, ev.Name)
	}
	require.Equal(t, []string{"mcp_jira_create-issue", "mcp_slack_post.message"}, names)
}

func TestNormalizingAdapter_MessageHistoryToolUseNamesRewritten(t *testing.T) {
	// A tool_use block from an earlier assistant turn carries the internal
	// name. The provider must receive the wire-facing name so the block
	// correlates with its declared tool.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
		Messages: []types.Message{
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					{Type: "text", Text: "I'll create the issue."},
					{Type: "tool_use", ID: "call_1", Name: "mcp_jira_create-issue", Input: json.RawMessage(`{}`)},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					{Type: "tool_result", ToolUseID: "call_1", Content: "OK"},
				},
			},
		},
	}

	_, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	messages := inner.receivedParams.Messages
	toolUse := messages[0].Content[1]
	require.Equal(t, "tool_use", toolUse.Type)

	t.Run("tool_use name rewritten", func(t *testing.T) {
		require.Equal(t, "mcp_jira_create_issue", toolUse.Name)
	})
	t.Run("text block unchanged", func(t *testing.T) {
		require.Equal(t, "I'll create the issue.", messages[0].Content[0].Text)
	})
	t.Run("tool_result block unchanged", func(t *testing.T) {
		require.Equal(t, "tool_result", messages[1].Content[0].Type)
	})
}

func TestNormalizingAdapter_CallerParamsNotMutated(t *testing.T) {
	// The wrapper must not write through to the caller's StreamParams,
	// because callers and the batch adapter retain those slices.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	tools := []types.ToolDefinition{
		{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
	}
	messages := []types.Message{
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "call_1", Name: "mcp_jira_create-issue"},
			},
		},
	}
	params := types.StreamParams{Tools: tools, Messages: messages}

	_, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	t.Run("tools slice unchanged", func(t *testing.T) {
		require.Equal(t, "mcp_jira_create-issue", tools[0].Name)
	})
	t.Run("messages slice unchanged", func(t *testing.T) {
		require.Equal(t, "mcp_jira_create-issue", messages[0].Content[0].Name)
	})
}

func TestNormalizingAdapter_CollisionResolvedByDisambiguation(t *testing.T) {
	// Two distinct internal names that sanitize to the same Gemini name stay
	// distinct after hash-suffix disambiguation. The fail-closed path is
	// covered by TestNormalizingAdapter_IrresolvableCollisionFailsClosed.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira-issue", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_jira.issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	_, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	require.NotEqual(t, inner.receivedParams.Tools[0].Name, inner.receivedParams.Tools[1].Name)
}

func TestNormalizingAdapter_IrresolvableCollisionFailsClosed(t *testing.T) {
	// A normalization failure must surface as an error without reaching the
	// inner adapter. Build's duplicate-name guard is the reachable failure
	// here, because every production policy has MaxLen=64. The stillCollides
	// branch is covered by TestBuild_IrresolvableCollisionErrors.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "duplicate_tool", InputSchema: json.RawMessage(`{}`)},
			{Name: "duplicate_tool", InputSchema: json.RawMessage(`{}`)},
		},
	}
	_, err := w.Stream(context.Background(), params)
	require.ErrorContains(t, err, "tool name normalization")

	t.Run("inner adapter not called", func(t *testing.T) {
		require.Nil(t, inner.receivedParams.Tools)
		require.Empty(t, inner.receivedParams.Model)
	})
}

func TestNormalizingAdapter_InnerStreamError_Propagated(t *testing.T) {
	inner := &fakeAdapter{streamErr: errFake}
	w := NewNormalizingAdapter(inner, "anthropic")
	_, err := w.Stream(context.Background(), types.StreamParams{})
	require.Error(t, err)
}

func TestNormalizingAdapter_LastBatchIDPassThrough(t *testing.T) {
	inner := &fakeBatchAdapter{}
	inner.batchID = "batch-xyz"
	w := NewNormalizingAdapter(inner, "anthropic")
	require.Equal(t, "batch-xyz", w.LastBatchID())
}

func TestNormalizingAdapter_LastBatchIDEmptyWhenInnerNotBatch(t *testing.T) {
	w := NewNormalizingAdapter(&fakeAdapter{}, "anthropic")
	require.Empty(t, w.LastBatchID())
}

func TestNormalizingAdapter_UnwrapReturnsInner(t *testing.T) {
	// Pins Unwrap's pointer-identity contract directly.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "anthropic")
	require.Same(t, inner, w.Unwrap())
}

func TestNormalizingAdapter_NonASCIINameRoundTrip_Gemini(t *testing.T) {
	// The adapter stack keeps a non-ASCII internal name off the wire, since
	// Gemini rejects non-ASCII, and restores it on the inbound tool_call so
	// dispatch still resolves the handler.
	//
	// "café_search" sanitizes to "caf__search": the "é" rune becomes one underscore.
	const internalName = "café_search"
	const wireName = "caf__search"

	inner := &fakeAdapter{
		scripted: []types.StreamEvent{
			{Type: "tool_call", ID: "call_1", Name: wireName},
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: internalName, InputSchema: json.RawMessage(`{}`)},
		},
	}
	ch, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	for _, r := range inner.receivedParams.Tools[0].Name {
		require.LessOrEqual(t, r, rune(127), "outbound tool name contains a non-ASCII rune")
	}

	calls := toolCallEvents(ch)
	require.NotEmpty(t, calls, "expected to observe a tool_call event")
	for _, ev := range calls {
		require.Equal(t, internalName, ev.Name)
	}
}

func TestNormalizingAdapter_RoundTripDispatch_OpenAIResponses(t *testing.T) {
	// On a provider that permits hyphens, both directions keep the internal
	// name unchanged so dispatch resolves the handler by its identifier.
	inner := &fakeAdapter{
		scripted: []types.StreamEvent{
			{Type: "tool_call", ID: "1", Name: "mcp_jira_create-issue"},
		},
	}
	w := NewNormalizingAdapter(inner, "openai-responses")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	ch, err := w.Stream(context.Background(), params)
	require.NoError(t, err)

	for _, ev := range toolCallEvents(ch) {
		require.Equal(t, "mcp_jira_create-issue", ev.Name)
	}
}
