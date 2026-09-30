package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// TestStreamEventsToResult_ThinkingBlocksPersistedInOrder pins that
// thinking and redacted_thinking events become history blocks in stream
// order, flushing pending text, and never reach the transport.
func TestStreamEventsToResult_ThinkingBlocksPersistedInOrder(t *testing.T) {
	var transportBuf bytes.Buffer
	tp := transport.NewStdioTransport(&transportBuf, &bytes.Buffer{})

	ch := make(chan types.StreamEvent, 6)
	ch <- types.StreamEvent{Type: "thinking", Text: "plan", ThoughtSignature: "sig-1"}
	ch <- types.StreamEvent{Type: "text_delta", Text: "Reading."}
	ch <- types.StreamEvent{Type: "redacted_thinking", ThoughtSignature: "data-1"}
	ch <- types.StreamEvent{Type: "tool_call", ID: "tc_1", Name: "read_file", Input: map[string]any{"path": "a"}}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	close(ch)

	result, err := streamEventsToResult(context.Background(), ch, tp, slog.Default())
	if err != nil {
		t.Fatalf("streamEventsToResult() error: %v", err)
	}

	want := []types.ContentBlock{
		{Type: "thinking", Text: "plan", ThoughtSignature: "sig-1"},
		{Type: "text", Text: "Reading."},
		{Type: "redacted_thinking", ThoughtSignature: "data-1"},
		{Type: "tool_use", ID: "tc_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
	}
	if len(result.Blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d: %+v", len(result.Blocks), len(want), result.Blocks)
	}
	for i := range want {
		got := result.Blocks[i]
		if got.Type != want[i].Type || got.Text != want[i].Text || got.ThoughtSignature != want[i].ThoughtSignature ||
			got.ID != want[i].ID || string(got.Input) != string(want[i].Input) {
			t.Errorf("block %d = %+v, want %+v", i, got, want[i])
		}
	}

	for _, line := range strings.Split(strings.TrimSpace(transportBuf.String()), "\n") {
		var e types.HarnessEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal emitted event: %v", err)
		}
		if strings.Contains(e.Type, "thinking") {
			t.Errorf("transport received a %q event; thinking is history-only", e.Type)
		}
	}
	if strings.Contains(transportBuf.String(), "sig-1") || strings.Contains(transportBuf.String(), "data-1") {
		t.Errorf("transport output carries a thinking signature:\n%s", transportBuf.String())
	}
}

func TestThinkingBlocksSkippedByTextAndToolCallReaders(t *testing.T) {
	blocks := []types.ContentBlock{
		{Type: "thinking", Text: "private reasoning", ThoughtSignature: "sig"},
		{Type: "text", Text: "answer"},
		{Type: "redacted_thinking", ThoughtSignature: "data"},
		{Type: "tool_use", ID: "tc_1", Name: "read_file", Input: json.RawMessage(`{}`)},
	}
	if got := lastAssistantText(blocks); got != "answer" {
		t.Errorf("lastAssistantText = %q, want %q", got, "answer")
	}
	calls := collectToolCalls(blocks)
	if len(calls) != 1 || calls[0].ID != "tc_1" {
		t.Errorf("collectToolCalls = %+v, want only tc_1", calls)
	}
}

// TestEstimateCurrentTokens_CountsThinkingSignature pins that a replayed
// thinking block's signature counts toward the budget, while a signature
// on any other block type (Gemini's) is left out as before.
func TestEstimateCurrentTokens_CountsThinkingSignature(t *testing.T) {
	sig := strings.Repeat("s", 400) // 100 tokens
	thinking := []types.Message{{
		Role:    "assistant",
		Content: []types.ContentBlock{{Type: "thinking", ThoughtSignature: sig}},
	}}
	// 4 (msg) + 3 (block) + 100 (signature) = 107
	if got := estimateCurrentTokens(thinking); got != 107 {
		t.Errorf("thinking block: got %d, want 107", got)
	}

	gemini := []types.Message{{
		Role:    "assistant",
		Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", 40), ThoughtSignature: sig}},
	}}
	// 4 (msg) + 3 (block) + 10 (text); the signature is not counted.
	if got := estimateCurrentTokens(gemini); got != 17 {
		t.Errorf("text block with signature: got %d, want 17", got)
	}
}

// thinkingParamsProvider emits a signed thinking block and a tool call on
// every turn and records the history each request carried.
type thinkingParamsProvider struct {
	turns    int
	sent     [][]types.Message
	numCalls int
}

func (p *thinkingParamsProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.sent = append(p.sent, params.Messages)
	p.numCalls++
	ch := make(chan types.StreamEvent, 4)
	if p.numCalls > p.turns {
		ch <- types.StreamEvent{Type: "text_delta", Text: "done"}
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
		close(ch)
		return ch, nil
	}
	ch <- types.StreamEvent{Type: "thinking", ThoughtSignature: fmt.Sprintf("sig-%d", p.numCalls)}
	ch <- types.StreamEvent{
		Type:  "tool_call",
		ID:    fmt.Sprintf("tc_%d", p.numCalls),
		Name:  "test_tool",
		Input: map[string]any{"turn": p.numCalls},
	}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	close(ch)
	return ch, nil
}

// rewriteFromCallStrategy returns history unchanged but reports a
// compaction from its compactFrom-th Prepare call onward, standing in for
// a strategy that has started rewriting history.
type rewriteFromCallStrategy struct {
	compactFrom int
	calls       int
	last        *contextpkg.CompactionEvent
}

func (s *rewriteFromCallStrategy) Prepare(_ context.Context, messages []types.Message, _ contextpkg.TokenBudget) ([]types.Message, error) {
	s.calls++
	s.last = nil
	if s.calls >= s.compactFrom {
		s.last = &contextpkg.CompactionEvent{Strategy: "test-rewrite", MessagesBefore: len(messages), MessagesAfter: len(messages)}
	}
	return messages, nil
}

func (s *rewriteFromCallStrategy) LastCompaction() *contextpkg.CompactionEvent {
	return s.last
}

func thinkingSignatures(messages []types.Message) []string {
	var sigs []string
	for _, msg := range messages {
		for _, b := range msg.Content {
			if types.IsThinkingBlock(b) {
				sigs = append(sigs, b.ThoughtSignature)
			}
		}
	}
	return sigs
}

// TestLoop_ReplaysThinkingUntilContextRewrite pins the prefix-binding rule:
// thinking blocks ride along on every request while history is append-only,
// and every one of them is stripped from a request whose history a context
// strategy rewrote.
func TestLoop_ReplaysThinkingUntilContextRewrite(t *testing.T) {
	prov := &thinkingParamsProvider{turns: 3}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Context = &rewriteFromCallStrategy{compactFrom: 3}

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.sent) != 4 {
		t.Fatalf("provider saw %d requests, want 4", len(prov.sent))
	}

	if got := thinkingSignatures(prov.sent[0]); len(got) != 0 {
		t.Errorf("request 1 carried thinking %v before any was produced", got)
	}
	if got := thinkingSignatures(prov.sent[1]); len(got) != 1 || got[0] != "sig-1" {
		t.Errorf("request 2 thinking = %v, want [sig-1] replayed", got)
	}
	for i := 2; i < len(prov.sent); i++ {
		if got := thinkingSignatures(prov.sent[i]); len(got) != 0 {
			t.Errorf("request %d after a rewrite carried thinking %v, want none", i+1, got)
		}
	}
	// Stripping must not lose the tool calls the thinking preceded.
	var toolUses int
	for _, msg := range prov.sent[3] {
		for _, b := range msg.Content {
			if b.Type == "tool_use" {
				toolUses++
			}
		}
	}
	if toolUses != 3 {
		t.Errorf("request 4 carried %d tool_use blocks, want 3", toolUses)
	}
}
