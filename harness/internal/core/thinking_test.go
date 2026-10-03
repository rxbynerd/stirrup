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

// runStreamEvents feeds events through streamEventsToResult and returns the
// persisted blocks and the bytes the transport received.
func runStreamEvents(t *testing.T, events []types.StreamEvent) ([]types.ContentBlock, string) {
	t.Helper()
	var transportBuf bytes.Buffer
	tp := transport.NewStdioTransport(&transportBuf, &bytes.Buffer{})
	ch := make(chan types.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	result, err := streamEventsToResult(context.Background(), ch, tp, slog.Default())
	if err != nil {
		t.Fatalf("streamEventsToResult() error: %v", err)
	}
	return result.Blocks, transportBuf.String()
}

// TestStreamEventsToResult_ThinkingBlocksPersistedInOrder pins that
// thinking and redacted_thinking events interleaved with text and tool
// calls become history blocks in stream order, flushing pending text, and
// that the transport receives exactly the bytes it would for the same
// stream with no thinking events.
func TestStreamEventsToResult_ThinkingBlocksPersistedInOrder(t *testing.T) {
	visible := []types.StreamEvent{
		{Type: "text_delta", Text: "Reading."},
		{Type: "tool_call", ID: "tc_1", Name: "read_file", Input: map[string]any{"path": "a"}},
		{Type: "tool_call", ID: "tc_2", Name: "read_file", Input: map[string]any{"path": "b"}},
		{Type: "message_complete", StopReason: "tool_use"},
	}
	interleaved := []types.StreamEvent{
		{Type: "thinking", Text: "plan A", ThoughtSignature: "sig-A"},
		visible[0],
		{Type: "thinking", Text: "plan B", ThoughtSignature: "sig-B"},
		visible[1],
		{Type: "redacted_thinking", ThoughtSignature: "data-C"},
		visible[2],
		visible[3],
	}

	blocks, withThinking := runStreamEvents(t, interleaved)

	want := []types.ContentBlock{
		{Type: "thinking", Text: "plan A", ThoughtSignature: "sig-A"},
		{Type: "text", Text: "Reading."},
		{Type: "thinking", Text: "plan B", ThoughtSignature: "sig-B"},
		{Type: "tool_use", ID: "tc_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
		{Type: "redacted_thinking", ThoughtSignature: "data-C"},
		{Type: "tool_use", ID: "tc_2", Name: "read_file", Input: json.RawMessage(`{"path":"b"}`)},
	}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d: %+v", len(blocks), len(want), blocks)
	}
	for i := range want {
		got := blocks[i]
		if got.Type != want[i].Type || got.Text != want[i].Text || got.ThoughtSignature != want[i].ThoughtSignature ||
			got.ID != want[i].ID || string(got.Input) != string(want[i].Input) {
			t.Errorf("block %d = %+v, want %+v", i, got, want[i])
		}
	}

	_, withoutThinking := runStreamEvents(t, visible)
	if withThinking != withoutThinking {
		t.Errorf("transport output differs when thinking events are present\nwith:\n%s\nwithout:\n%s", withThinking, withoutThinking)
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

// TestEstimateCurrentTokens_CountsThinkingTextNotSignature pins that a
// thinking block contributes its reasoning text to the budget and its
// signature does not, and that a signature on any other block type
// (Gemini's) is likewise left out.
func TestEstimateCurrentTokens_CountsThinkingTextNotSignature(t *testing.T) {
	sig := strings.Repeat("s", 400)
	thinking := []types.Message{{
		Role: "assistant",
		Content: []types.ContentBlock{
			{Type: "thinking", Text: strings.Repeat("t", 40), ThoughtSignature: sig},
			{Type: "redacted_thinking", ThoughtSignature: sig},
		},
	}}
	// 4 (msg) + 3 + 10 (thinking block and its text) + 3 (redacted block)
	if got := estimateCurrentTokens(thinking); got != 20 {
		t.Errorf("thinking blocks: got %d, want 20", got)
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
// compaction from its compactFrom-th Prepare call onward (through the
// compactUntil-th call when compactUntil is non-zero), standing in for a
// strategy that rewrites history.
type rewriteFromCallStrategy struct {
	compactFrom  int
	compactUntil int
	calls        int
	last         *contextpkg.CompactionEvent
}

func (s *rewriteFromCallStrategy) Prepare(_ context.Context, messages []types.Message, _ contextpkg.TokenBudget) ([]types.Message, error) {
	s.calls++
	s.last = nil
	if s.calls >= s.compactFrom && (s.compactUntil == 0 || s.calls <= s.compactUntil) {
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

// TestLoop_StripsThinkingForRestOfRunAfterOneRewrite pins that a single
// rewrite ends thinking replay for the rest of the run, even when the
// strategy reports no compaction on later turns.
func TestLoop_StripsThinkingForRestOfRunAfterOneRewrite(t *testing.T) {
	prov := &thinkingParamsProvider{turns: 3}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Context = &rewriteFromCallStrategy{compactFrom: 2, compactUntil: 2}

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.sent) != 4 {
		t.Fatalf("provider saw %d requests, want 4", len(prov.sent))
	}
	for i := 1; i < len(prov.sent); i++ {
		if got := thinkingSignatures(prov.sent[i]); len(got) != 0 {
			t.Errorf("request %d carried thinking %v after the run's rewrite, want none", i+1, got)
		}
	}
}

// recordingStrategy passes Prepare through to a real strategy and records,
// per call, the loop's token estimate, the thinking blocks in the stored
// history it was given, and whether the strategy reported a compaction.
type recordingStrategy struct {
	inner          contextpkg.ContextStrategy
	currentTokens  []int
	storedThinking []int
	compacted      []bool
}

func (r *recordingStrategy) Prepare(ctx context.Context, messages []types.Message, budget contextpkg.TokenBudget) ([]types.Message, error) {
	r.currentTokens = append(r.currentTokens, budget.CurrentTokens)
	r.storedThinking = append(r.storedThinking, len(thinkingSignatures(messages)))
	out, err := r.inner.Prepare(ctx, messages, budget)
	r.compacted = append(r.compacted, r.inner.LastCompaction() != nil)
	return out, err
}

func (r *recordingStrategy) LastCompaction() *contextpkg.CompactionEvent {
	return r.inner.LastCompaction()
}

type summaryTextProvider struct{}

func (summaryTextProvider) Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: "summary of earlier turns"}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

type discardFileWriter struct{}

func (discardFileWriter) WriteFile(context.Context, string, string) error { return nil }

// maxTokensCompactingFrom returns a contextStrategy.maxTokens whose usable
// budget admits the loop's estimate for call first-1 but not for call
// first, so an over-budget strategy first compacts on call index first.
func maxTokensCompactingFrom(t *testing.T, estimates []int, first int) int {
	t.Helper()
	lo, hi := estimates[first-1], estimates[first]
	for m := lo; m <= 2*hi+defaultReserveForResponse; m++ {
		if avail := m - effectiveReserveForResponse(m); avail >= lo && avail < hi {
			return m
		}
	}
	t.Fatalf("no maxTokens separates estimates %d and %d", lo, hi)
	return 0
}

// TestLoop_RealStrategiesStripThinkingAfterCompaction drives each context
// strategy past its budget mid-run. It pins that the strategy reports a
// compaction on every call after its first ("once compacted, always
// compacted"), that the stored history keeps every thinking block, and
// that no request after the first compaction carries one.
func TestLoop_RealStrategiesStripThinkingAfterCompaction(t *testing.T) {
	const turns = 5
	cases := []struct {
		name  string
		first int
		build func() contextpkg.ContextStrategy
	}{
		{"sliding-window", 2, func() contextpkg.ContextStrategy { return contextpkg.NewSlidingWindowStrategy() }},
		{"summarise", 3, func() contextpkg.ContextStrategy {
			return contextpkg.NewSummariseStrategy(summaryTextProvider{}, "summary-model")
		}},
		{"offload-to-file", 2, func() contextpkg.ContextStrategy {
			return contextpkg.NewOffloadToFileStrategy(discardFileWriter{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calibrate := &recordingStrategy{inner: tc.build()}
			loop := buildTestLoop(nil)
			loop.Provider = &thinkingParamsProvider{turns: turns}
			loop.Context = calibrate
			if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
				t.Fatalf("calibration Run: %v", err)
			}

			prov := &thinkingParamsProvider{turns: turns}
			rec := &recordingStrategy{inner: tc.build()}
			cfg := buildTestConfig()
			cfg.ContextStrategy.MaxTokens = maxTokensCompactingFrom(t, calibrate.currentTokens, tc.first)
			loop = buildTestLoop(nil)
			loop.Provider = prov
			loop.Context = rec
			if _, err := loop.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(prov.sent) != turns+1 {
				t.Fatalf("provider saw %d requests, want %d", len(prov.sent), turns+1)
			}

			for i := range prov.sent {
				if wantCompacted := i >= tc.first; rec.compacted[i] != wantCompacted {
					t.Errorf("call %d: compacted = %v, want %v", i, rec.compacted[i], wantCompacted)
				}
				if rec.storedThinking[i] != i {
					t.Errorf("call %d: stored history has %d thinking blocks, want %d", i, rec.storedThinking[i], i)
				}
				wantSent := i
				if i >= tc.first {
					wantSent = 0
				}
				if got := len(thinkingSignatures(prov.sent[i])); got != wantSent {
					t.Errorf("request %d carried %d thinking blocks, want %d", i+1, got, wantSent)
				}
			}
		})
	}
}
