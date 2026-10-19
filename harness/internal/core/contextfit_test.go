package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// requestRecordingProvider replays scripted turns and records every
// request it receives.
type requestRecordingProvider struct {
	mu       sync.Mutex
	calls    [][]types.StreamEvent
	requests []types.StreamParams
}

func (p *requestRecordingProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	params.Messages = append([]types.Message(nil), params.Messages...)
	p.requests = append(p.requests, params)
	events := []types.StreamEvent{{Type: "message_complete", StopReason: "end_turn"}}
	if i := len(p.requests) - 1; i < len(p.calls) {
		events = p.calls[i]
	}
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// historyCapturingStrategy passes Prepare through and keeps a copy of the
// stored history it was given on each call.
type historyCapturingStrategy struct {
	inner   contextpkg.ContextStrategy
	history [][]types.Message
}

func (s *historyCapturingStrategy) Prepare(ctx context.Context, messages []types.Message, budget contextpkg.TokenBudget) ([]types.Message, error) {
	clone := make([]types.Message, len(messages))
	for i, msg := range messages {
		clone[i] = msg
		clone[i].Content = append([]types.ContentBlock(nil), msg.Content...)
	}
	s.history = append(s.history, clone)
	return s.inner.Prepare(ctx, messages, budget)
}

func (s *historyCapturingStrategy) LastCompaction() *contextpkg.CompactionEvent {
	return s.inner.LastCompaction()
}

func fitToolCallTurn(id, name string) []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "tool_call", ID: id, Name: name, Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use"},
	}
}

func requestTokens(params types.StreamParams) int {
	return estimateCurrentTokens(params.Messages, true) +
		estimateSystemPromptTokens(params.System) +
		estimateToolDefinitionTokens(params.Tools)
}

// TestLoop_OversizedToolResultIsTruncatedToFit pins the fit postcondition
// for a 40 KB tool result under a 2,000-token budget: every request fits
// the limit, the result is sent with its head, tail and a marker right
// after the call it answers, and the stored history keeps it whole.
func TestLoop_OversizedToolResultIsTruncatedToFit(t *testing.T) {
	big := "HEAD:" + strings.Repeat("x", 39_990) + ":TAIL"
	prov := &requestRecordingProvider{calls: [][]types.StreamEvent{
		fitToolCallTurn("tc_big", "big_tool"),
		fitToolCallTurn("tc_small", "test_tool"),
	}}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "big_tool",
		Description: "Returns a large output",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return big, nil
		},
	})
	capture := &historyCapturingStrategy{inner: contextpkg.NewSlidingWindowStrategy()}
	loop.Context = capture
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 2000

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Len(t, prov.requests, 3)

	limit := contextpkg.FitLimit(contextpkg.TokenBudget{MaxTokens: 2000, ReserveForResponse: effectiveReserveForResponse(2000)})
	for i, req := range prov.requests {
		t.Run(fmt.Sprintf("request %d fits the limit", i+1), func(t *testing.T) {
			require.LessOrEqual(t, requestTokens(req), limit, "estimated tokens exceed the fit limit")
		})
	}

	msgs := prov.requests[1].Messages
	call, result := msgs[1].Content[0], msgs[2].Content[0]
	require.Equal(t, "tool_use", call.Type, "message 1 is not a tool call: %+v", call)
	require.Equal(t, "tc_big", call.ID, "message 1 is not the big_tool call: %+v", call)
	require.Equal(t, "tool_result", result.Type, "message 2 is not a tool result: %+v", result)
	require.Equal(t, "tc_big", result.ToolUseID, "message 2 is not the big_tool result: %+v", result)

	assert.True(t, strings.HasPrefix(result.Content, "HEAD:"), "sent result lacks its head")
	assert.True(t, strings.HasSuffix(result.Content, ":TAIL"), "sent result lacks its tail")
	assert.Contains(t, result.Content, "[...truncated ", "sent result lacks the truncation marker")
	assert.Less(t, len(result.Content), len(big), "sent result was not truncated")

	// The next turn's Prepare receives the stored history.
	stored := capture.history[2]
	assert.Equal(t, big, stored[2].Content[0].Content, "stored history should keep the full tool result")
}

// TestLoop_FitTruncationStripsThinkingBlocks pins that a request whose
// tool results the fit postcondition truncated, with no compaction by the
// strategy, carries no thinking blocks, nor does any later request.
func TestLoop_FitTruncationStripsThinkingBlocks(t *testing.T) {
	thinkingToolTurn := func(n int, name string) []types.StreamEvent {
		return []types.StreamEvent{
			{Type: "thinking", Text: "plan", ThoughtSignature: fmt.Sprintf("sig-%d", n)},
			{Type: "tool_call", ID: fmt.Sprintf("tc_%d", n), Name: name, Input: map[string]any{"turn": n}},
			{Type: "message_complete", StopReason: "tool_use"},
		}
	}
	prov := &requestRecordingProvider{calls: [][]types.StreamEvent{
		thinkingToolTurn(1, "test_tool"),
		thinkingToolTurn(2, "big_tool"),
		thinkingToolTurn(3, "test_tool"),
	}}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "big_tool",
		Description: "Returns a large output",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 40_000), nil
		},
	})
	loop.Context = &rewriteFromCallStrategy{compactFrom: math.MaxInt}
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 2000

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Len(t, prov.requests, 4)
	require.NotEmpty(t, thinkingSignatures(prov.requests[1].Messages), "request 2 carries no thinking blocks before any truncation")
	require.Contains(t, prov.requests[2].Messages[4].Content[0].Content, "[...truncated ", "request 3 did not truncate the big_tool result")
	for i := 2; i < len(prov.requests); i++ {
		t.Run(fmt.Sprintf("request %d has no thinking blocks", i+1), func(t *testing.T) {
			require.Empty(t, thinkingSignatures(prov.requests[i].Messages), "thinking blocks replayed after a fit truncation")
		})
	}
}

// TestLoop_ContextExhaustedBeforeAnyProviderCall pins that a budget the
// system prompt and tool definitions alone exceed ends the run as
// context_exhausted without a provider call, naming the sizes involved.
func TestLoop_ContextExhaustedBeforeAnyProviderCall(t *testing.T) {
	prov := &requestRecordingProvider{}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	var events bytes.Buffer
	loop.Transport = transport.NewStdioTransport(&events, &bytes.Buffer{})
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 100

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "context_exhausted", runTrace.Outcome)
	require.Empty(t, prov.requests, "provider should not receive a request")

	out := events.String()
	cases := []struct {
		name string
		want string
	}{
		{name: "budget message", want: "context budget exhausted"},
		{name: "system prompt size", want: "system prompt "},
		{name: "tool definitions size", want: "tool definitions "},
		{name: "config hint", want: "contextStrategy.maxTokens=100"},
		{name: "stop reason", want: `"stopReason":"context_exhausted"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, out, tc.want)
		})
	}
}

// TestLoop_ContextExhaustedWhenTruncationCannotFit pins that a request
// still over the limit after every tool result is truncated, here because
// of an oversized assistant turn, is never sent.
func TestLoop_ContextExhaustedWhenTruncationCannotFit(t *testing.T) {
	prov := &requestRecordingProvider{calls: [][]types.StreamEvent{{
		{Type: "text_delta", Text: strings.Repeat("a", 40_000)},
		{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use"},
	}}}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 2000

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "context_exhausted", runTrace.Outcome)
	require.Len(t, prov.requests, 1, "provider should see only the first request")
}

// TestLoop_SlidingWindowCompactionNeedsNoFitTruncation pins that the
// sliding window compacts a history of small exchanges to the fit limit,
// so the fit postcondition never truncates a tool result or warns.
func TestLoop_SlidingWindowCompactionNeedsNoFitTruncation(t *testing.T) {
	const turns = 16
	var calls [][]types.StreamEvent
	for i := range turns {
		calls = append(calls, []types.StreamEvent{
			{Type: "tool_call", ID: fmt.Sprintf("tc_%d", i), Name: "small_tool", Input: map[string]any{"n": i}},
			{Type: "message_complete", StopReason: "tool_use"},
		})
	}
	prov := &requestRecordingProvider{calls: calls}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	var logs bytes.Buffer
	loop.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "small_tool",
		Description: "Returns a small output",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Handler: func(_ context.Context, input json.RawMessage) (string, error) {
			return string(input) + strings.Repeat("r", 600), nil
		},
	})
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 2400

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Len(t, prov.requests, turns+1)
	require.Less(t, len(prov.requests[turns].Messages), 1+2*turns, "the budget never forced a compaction")
	for i, req := range prov.requests {
		for _, msg := range req.Messages {
			for _, block := range msg.Content {
				require.NotContains(t, block.Content, "[...truncated ", "request %d sent a truncated tool result", i+1)
			}
		}
	}
	assert.NotContains(t, logs.String(), "tool results truncated", "the fit postcondition truncated tool results after a compaction")
}
