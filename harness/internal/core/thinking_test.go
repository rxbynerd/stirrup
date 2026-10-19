package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
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
	result, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
	require.NoError(t, err)
	return result.Blocks, transportBuf.String()
}

// recordStreamEvents feeds events through streamEventsToResult and returns
// the persisted blocks and the events the transport received.
func recordStreamEvents(t *testing.T, events []types.StreamEvent) ([]types.ContentBlock, []types.HarnessEvent) {
	t.Helper()
	return recordStreamEventsWith(t, events, true)
}

func recordStreamEventsWith(t *testing.T, events []types.StreamEvent, streamThinking bool) ([]types.ContentBlock, []types.HarnessEvent) {
	t.Helper()
	rec := &recordingTransport{}
	ch := make(chan types.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	result, err := streamEventsToResult(context.Background(), ch, rec, streamThinking, ignoreEmitFailure, nil)
	require.NoError(t, err)
	return result.Blocks, rec.events
}

func thinkingDeltaTexts(events []types.HarnessEvent) []string {
	var texts []string
	for _, ev := range events {
		if ev.Type == "thinking_delta" {
			texts = append(texts, ev.Text)
		}
	}
	return texts
}

// TestStreamEventsToResult_ThinkingBlocksPersistedInOrder pins that
// thinking and redacted_thinking events interleaved with text and tool
// calls become history blocks in stream order, flushing pending text, and
// that a block arriving whole reaches the transport as one thinking_delta
// in stream position, while redacted reasoning and signatures never do.
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
	// Input is compared as text, so a nil and an empty raw message match.
	type blockFields struct {
		Type, Text, ThoughtSignature, ID, Input string
	}
	fields := func(items []types.ContentBlock) []blockFields {
		out := make([]blockFields, 0, len(items))
		for _, b := range items {
			out = append(out, blockFields{b.Type, b.Text, b.ThoughtSignature, b.ID, string(b.Input)})
		}
		return out
	}
	require.Equal(t, fields(want), fields(blocks))

	for _, secret := range []string{"sig-A", "sig-B", "data-C"} {
		t.Run("transport omits "+secret, func(t *testing.T) {
			require.NotContains(t, withThinking, secret, "signatures and redacted data stay out of the transport")
		})
	}

	_, emitted := recordStreamEvents(t, interleaved)
	var gotTypes []string
	for _, ev := range emitted {
		gotTypes = append(gotTypes, ev.Type)
	}
	wantTypes := []string{"thinking_delta", "text_delta", "thinking_delta", "tool_call", "tool_call"}
	require.Equal(t, wantTypes, gotTypes)
	require.Equal(t, []string{"plan A", "plan B"}, thinkingDeltaTexts(emitted))
}

// TestStreamEventsToResult_ThinkingDeltasForwardedOnce pins that streamed
// reasoning fragments are forwarded as they arrive and the block that
// closes them is not forwarded again, while a later block that arrives
// whole is still forwarded, and an empty block produces no event.
func TestStreamEventsToResult_ThinkingDeltasForwardedOnce(t *testing.T) {
	blocks, emitted := recordStreamEvents(t, []types.StreamEvent{
		{Type: "thinking_delta", Text: "Let me "},
		{Type: "thinking_delta", Text: "check."},
		{Type: "thinking", Text: "Let me check.", ThoughtSignature: "sig-A"},
		{Type: "text_delta", Text: "Done."},
		{Type: "thinking", Text: "whole block", ThoughtSignature: "sig-B"},
		{Type: "thinking", ThoughtSignature: "sig-C"},
		{Type: "message_complete", StopReason: "end_turn"},
	})

	require.Equal(t, []string{"Let me ", "check.", "whole block"}, thinkingDeltaTexts(emitted))

	var thinkingBlocks int
	for _, b := range blocks {
		if b.Type == "thinking" {
			thinkingBlocks++
		}
	}
	require.Equal(t, 3, thinkingBlocks, "deltas must not add history")
}

// TestStreamEventsToResult_ThinkingNotForwardedWhenOff pins that with
// thinking streaming off neither streamed fragments nor a block arriving
// whole reach the transport, while every thinking block is still persisted
// and answer text is still forwarded.
func TestStreamEventsToResult_ThinkingNotForwardedWhenOff(t *testing.T) {
	blocks, emitted := recordStreamEventsWith(t, []types.StreamEvent{
		{Type: "thinking_delta", Text: "Let me "},
		{Type: "thinking_delta", Text: "check."},
		{Type: "thinking", Text: "Let me check.", ThoughtSignature: "sig-A"},
		{Type: "text_delta", Text: "Done."},
		{Type: "thinking", Text: "whole block", ThoughtSignature: "sig-B"},
		{Type: "message_complete", StopReason: "end_turn"},
	}, false)

	require.Empty(t, thinkingDeltaTexts(emitted))
	require.Equal(t, []string{"text_delta"}, eventTypeList(emitted))

	var thinkingTexts []string
	for _, b := range blocks {
		if b.Type == "thinking" {
			thinkingTexts = append(thinkingTexts, b.Text)
		}
	}
	require.Equal(t, []string{"Let me check.", "whole block"}, thinkingTexts)
}

func TestThinkingBlocks_SkippedByTextAndToolCallReaders(t *testing.T) {
	blocks := []types.ContentBlock{
		{Type: "thinking", Text: "private reasoning", ThoughtSignature: "sig"},
		{Type: "text", Text: "answer"},
		{Type: "redacted_thinking", ThoughtSignature: "data"},
		{Type: "tool_use", ID: "tc_1", Name: "read_file", Input: json.RawMessage(`{}`)},
	}
	require.Equal(t, "answer", lastAssistantText(blocks))

	calls := collectToolCalls(blocks, nil)
	require.Len(t, calls, 1)
	require.Equal(t, "tc_1", calls[0].ID)
}

// TestEstimateCurrentTokens_CountsThinkingTextNotSignature pins that a
// thinking block contributes its reasoning text to the budget and its
// signature does not, and that a signature on any other block type
// (Gemini's) is likewise left out.
func TestEstimateCurrentTokens_CountsThinkingTextNotSignature(t *testing.T) {
	sig := strings.Repeat("s", 400)

	t.Run("thinking blocks count their text only", func(t *testing.T) {
		thinking := []types.Message{{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "thinking", Text: strings.Repeat("t", 40), ThoughtSignature: sig},
				{Type: "redacted_thinking", ThoughtSignature: sig},
			},
		}}
		// 4 (msg) + 3 + 10 (thinking block and its text) + 3 (redacted block)
		require.Equal(t, 20, estimateCurrentTokens(thinking, true))
	})

	t.Run("signature on a text block is not counted", func(t *testing.T) {
		gemini := []types.Message{{
			Role:    "assistant",
			Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", 40), ThoughtSignature: sig}},
		}}
		// 4 (msg) + 3 (block) + 10 (text); the signature is not counted.
		require.Equal(t, 17, estimateCurrentTokens(gemini, true))
	})
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

	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Len(t, prov.sent, 4)

	require.Empty(t, thinkingSignatures(prov.sent[0]), "request 1 precedes any thinking")
	require.Equal(t, []string{"sig-1"}, thinkingSignatures(prov.sent[1]), "request 2 replays sig-1")
	for i := 2; i < len(prov.sent); i++ {
		t.Run(fmt.Sprintf("request %d after rewrite", i+1), func(t *testing.T) {
			require.Empty(t, thinkingSignatures(prov.sent[i]))
		})
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
	require.Equal(t, 3, toolUses, "tool_use blocks in request 4")
}

// TestLoop_StripsThinkingForRestOfRunAfterOneRewrite pins that a single
// rewrite ends thinking replay for the rest of the run, even when the
// strategy reports no compaction on later turns.
func TestLoop_StripsThinkingForRestOfRunAfterOneRewrite(t *testing.T) {
	prov := &thinkingParamsProvider{turns: 3}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Context = &rewriteFromCallStrategy{compactFrom: 2, compactUntil: 2}

	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Len(t, prov.sent, 4)
	for i := 1; i < len(prov.sent); i++ {
		t.Run(fmt.Sprintf("request %d after rewrite", i+1), func(t *testing.T) {
			require.Empty(t, thinkingSignatures(prov.sent[i]), "thinking after the run's rewrite")
		})
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

// maxTokensCompactingFrom returns the largest contextStrategy.maxTokens
// whose fit limit admits the loop's estimate for call first-1 but whose
// usable budget does not admit the estimate for call first, so an
// over-budget strategy first compacts on call index first.
func maxTokensCompactingFrom(t *testing.T, estimates []int, first int) int {
	t.Helper()
	lo, hi := estimates[first-1], estimates[first]
	found := 0
	for m := lo; m <= 2*hi+defaultReserveForResponse; m++ {
		budget := contextpkg.TokenBudget{MaxTokens: m, ReserveForResponse: effectiveReserveForResponse(m)}
		if contextpkg.FitLimit(budget) >= lo && m-budget.ReserveForResponse < hi {
			found = m
		}
	}
	require.NotZero(t, found, "no maxTokens separates estimates %d and %d", lo, hi)
	return found
}

// registerLargeTestTool replaces test_tool with one whose results are
// large enough for every strategy to shrink, so each compacted request
// fits its budget.
func registerLargeTestTool(t *testing.T, loop *AgenticLoop) {
	t.Helper()
	registry, ok := loop.Tools.(*tool.Registry)
	require.True(t, ok, "loop Tools is %T, want *tool.Registry", loop.Tools)
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("tool result line\n", 150), nil
		},
	})
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
		// Summarising needs an aged exchange between the pinned task and
		// the recent messages, so the first one exists on call 4.
		{"summarise", 4, func() contextpkg.ContextStrategy {
			return contextpkg.NewSummariseStrategy(summaryTextProvider{}, "summary-model", 0)
		}},
		{"offload-to-file", 2, func() contextpkg.ContextStrategy {
			return contextpkg.NewOffloadToFileStrategy(discardFileWriter{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calibration := &recordingStrategy{inner: tc.build()}
			loop := buildTestLoop(nil)
			registerLargeTestTool(t, loop)
			loop.Provider = &thinkingParamsProvider{turns: turns}
			loop.Context = calibration
			_, err := loop.Run(context.Background(), buildTestConfig())
			require.NoError(t, err, "calibration run")

			prov := &thinkingParamsProvider{turns: turns}
			rec := &recordingStrategy{inner: tc.build()}
			cfg := buildTestConfig()
			cfg.ContextStrategy.MaxTokens = maxTokensCompactingFrom(t, calibration.currentTokens, tc.first)
			loop = buildTestLoop(nil)
			registerLargeTestTool(t, loop)
			loop.Provider = prov
			loop.Context = rec
			_, err = loop.Run(context.Background(), cfg)
			require.NoError(t, err)
			require.Len(t, prov.sent, turns+1)

			for i := range prov.sent {
				t.Run(fmt.Sprintf("call %d", i), func(t *testing.T) {
					require.Equal(t, i >= tc.first, rec.compacted[i], "compacted")
					require.Equal(t, i, rec.storedThinking[i], "thinking blocks in stored history")

					wantSent := i
					if i >= tc.first {
						wantSent = 0
					}
					require.Len(t, thinkingSignatures(prov.sent[i]), wantSent, "thinking blocks in request %d", i+1)
				})
			}
		})
	}
}
