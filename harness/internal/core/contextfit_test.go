package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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
	return estimateCurrentTokens(params.Messages) +
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", runTrace.Outcome)
	}
	if len(prov.requests) != 3 {
		t.Fatalf("provider saw %d requests, want 3", len(prov.requests))
	}

	limit := contextpkg.FitLimit(contextpkg.TokenBudget{MaxTokens: 2000, ReserveForResponse: effectiveReserveForResponse(2000)})
	for i, req := range prov.requests {
		if got := requestTokens(req); got > limit {
			t.Errorf("request %d estimates %d tokens, over the fit limit of %d", i+1, got, limit)
		}
	}
	msgs := prov.requests[1].Messages
	call, result := msgs[1].Content[0], msgs[2].Content[0]
	if call.Type != "tool_use" || call.ID != "tc_big" {
		t.Fatalf("message 1 is not the big_tool call: %+v", call)
	}
	if result.Type != "tool_result" || result.ToolUseID != "tc_big" {
		t.Fatalf("message 2 is not the big_tool result: %+v", result)
	}
	if !strings.HasPrefix(result.Content, "HEAD:") || !strings.HasSuffix(result.Content, ":TAIL") ||
		!strings.Contains(result.Content, "[...truncated ") || len(result.Content) >= len(big) {
		t.Errorf("result was not sent truncated with head, tail and marker (%d bytes)", len(result.Content))
	}

	// The next turn's Prepare receives the stored history.
	stored := capture.history[2]
	if got := stored[2].Content[0].Content; got != big {
		t.Errorf("stored history holds %d bytes of the result, want the full %d", len(got), len(big))
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "context_exhausted" {
		t.Fatalf("outcome = %q, want context_exhausted", runTrace.Outcome)
	}
	if len(prov.requests) != 0 {
		t.Fatalf("provider saw %d requests, want none", len(prov.requests))
	}
	out := events.String()
	for _, want := range []string{"context budget exhausted", "system prompt ", "tool definitions ", "contextStrategy.maxTokens=100", `"stopReason":"context_exhausted"`} {
		if !strings.Contains(out, want) {
			t.Errorf("transport events do not mention %q:\n%s", want, out)
		}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "context_exhausted" {
		t.Fatalf("outcome = %q, want context_exhausted", runTrace.Outcome)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider saw %d requests, want only the first", len(prov.requests))
	}
}
