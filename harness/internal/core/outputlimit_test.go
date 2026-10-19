package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// turnCapturingEmitter records every TurnTrace and TurnRecord while the
// wrapped emitter still produces the RunTrace that Run returns.
type turnCapturingEmitter struct {
	trace.TraceEmitter
	mu      sync.Mutex
	turns   []types.TurnTrace
	records []types.TurnRecord
}

func (e *turnCapturingEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	e.turns = append(e.turns, turn)
	e.mu.Unlock()
	e.TraceEmitter.RecordTurn(turn)
}

func (e *turnCapturingEmitter) RecordTurnRecord(record types.TurnRecord) {
	e.mu.Lock()
	e.records = append(e.records, record)
	e.mu.Unlock()
	e.TraceEmitter.RecordTurnRecord(record)
}

func (e *turnCapturingEmitter) RecordFinalAssistantText(text string) {
	if r, ok := e.TraceEmitter.(trace.FinalAssistantTextRecorder); ok {
		r.RecordFinalAssistantText(text)
	}
}

func (e *turnCapturingEmitter) recoveryFlags() (turns, records []bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.turns {
		turns = append(turns, t.OutputLimitRecovery)
	}
	for _, r := range e.records {
		records = append(records, r.OutputLimitRecovery)
	}
	return turns, records
}

type outputLimitHarness struct {
	loop      *AgenticLoop
	provider  *recordingScriptProvider
	transport *cancellableTransport
	emitter   *turnCapturingEmitter

	mu     sync.Mutex
	writes []string
}

func (h *outputLimitHarness) writtenPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.writes...)
}

func (h *outputLimitHarness) warnings() []string {
	var out []string
	for _, ev := range h.transport.Events() {
		if ev.Type == "warning" && strings.Contains(ev.Message, "output limit") {
			out = append(out, ev.Message)
		}
	}
	return out
}

// newOutputLimitHarness wires a scripted provider, a recording transport
// and trace emitter, and a write_file tool that records each path it
// runs with.
func newOutputLimitHarness(script ...[]types.StreamEvent) *outputLimitHarness {
	h := &outputLimitHarness{
		provider:  &recordingScriptProvider{script: script},
		transport: &cancellableTransport{},
	}
	loop := buildTestLoop(&mockProvider{})
	loop.Provider = h.provider
	loop.Transport = h.transport
	h.emitter = &turnCapturingEmitter{TraceEmitter: loop.Trace}
	loop.Trace = h.emitter
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "tool result", nil
		},
	})
	registry.Register(&tool.Tool{
		Name:        "write_file",
		Description: "Write a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
		Handler: func(_ context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			h.mu.Lock()
			h.writes = append(h.writes, args.Path)
			h.mu.Unlock()
			return "written", nil
		},
	})
	loop.Tools = registry
	h.loop = loop
	return h
}

func writeFileCall(id, path string) types.StreamEvent {
	return types.StreamEvent{Type: "tool_call", ID: id, Name: "write_file", Input: map[string]any{"path": path, "content": "x"}}
}

func stopWith(reason string) types.StreamEvent {
	return types.StreamEvent{Type: "message_complete", StopReason: reason}
}

// assertToolUsePaired reports every assistant tool_use block that is not
// answered by a tool_result in the message that follows it.
func assertToolUsePaired(t *testing.T, label string, msgs []types.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		answered := map[string]bool{}
		if i+1 < len(msgs) && msgs[i+1].Role == "user" {
			for _, b := range msgs[i+1].Content {
				if b.Type == "tool_result" {
					answered[b.ToolUseID] = true
				}
			}
		}
		for _, b := range m.Content {
			if b.Type == "tool_use" {
				assert.True(t, answered[b.ID], "%s: message %d holds tool_use %q (%s) with no paired tool_result", label, i, b.ID, b.Name)
			}
		}
	}
}

func assertAllRequestsPaired(t *testing.T, calls []types.StreamParams) {
	t.Helper()
	for i, c := range calls {
		assertToolUsePaired(t, fmt.Sprintf("request %d", i), c.Messages)
	}
}

func TestLoop_OutputLimitRecovery_ContinuesTruncatedText(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Partial answer"}, stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "rest of the answer"}, stopWith("end_turn")},
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Equal(t, 2, runTrace.Turns)
	require.Equal(t, "Partial answerrest of the answer", runTrace.FinalAssistantText)

	calls := h.provider.params()
	require.Len(t, calls, 2)
	msgs := calls[1].Messages
	require.Len(t, msgs, 3, "second request: prompt, truncated turn, instruction")
	require.Equal(t, "assistant", msgs[1].Role)
	require.Equal(t, "Partial answer", strings.Join(textBlocks(msgs[1]), ""))
	require.Equal(t, "user", msgs[2].Role)
	require.True(t, msgs[2].Synthetic)
	require.Equal(t, outputLimitContinueText, strings.Join(textBlocks(msgs[2]), ""))

	turns, records := h.emitter.recoveryFlags()
	require.Equal(t, []bool{true, false}, turns)
	require.Equal(t, []bool{true, false}, records)
	warnings := h.warnings()
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "recovery 1 of 2")
}

func TestLoop_OutputLimitRecovery_JoinsEveryContinuationOfTheAnswer(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "one "}, stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "two "}, stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "three"}, stopWith("end_turn")},
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Equal(t, "one two three", runTrace.FinalAssistantText)
}

// postTurnDenyGuard allows every phase but PhasePostTurn and records the
// content it denied.
type postTurnDenyGuard struct {
	mu     sync.Mutex
	denied []string
}

func (g *postTurnDenyGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	if in.Phase != guard.PhasePostTurn {
		return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "post-turn-deny"}, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.denied = append(g.denied, in.Content)
	return &guard.Decision{Verdict: guard.VerdictDeny, GuardID: "post-turn-deny"}, nil
}

func TestLoop_OutputLimitRecovery_PostTurnGuardChecksTheJoinedAnswer(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Checking first."}, {Type: "tool_call", ID: "tc_0", Name: "test_tool", Input: map[string]any{}}, stopWith("tool_use")},
		[]types.StreamEvent{{Type: "text_delta", Text: "first half "}, stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "second half"}, stopWith("end_turn")},
	)
	g := &postTurnDenyGuard{}
	h.loop.GuardRail = g

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "guardrail_blocked", runTrace.Outcome)
	require.Equal(t, []string{"first half second half"}, g.denied)
	require.Equal(t, "Checking first.", runTrace.FinalAssistantText)
}

func TestLoop_OutputLimitRecovery_ToolCallPreambleIsNotJoined(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Writing it now."}, writeFileCall("tc_big", "big.txt"), stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "Done."}, stopWith("end_turn")},
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "Done.", runTrace.FinalAssistantText)
}

func TestLoop_OutputLimitRecovery_DropsTruncatedToolCalls(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{
			{Type: "text_delta", Text: "Writing both files now."},
			writeFileCall("tc_small", "small.txt"),
			writeFileCall("tc_big", "big.txt"),
			stopWith("max_tokens"),
		},
		[]types.StreamEvent{writeFileCall("tc_a", "part1.txt"), writeFileCall("tc_b", "part2.txt"), stopWith("tool_use")},
		scriptEndTurn,
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Equal(t, []string{"part1.txt", "part2.txt"}, h.writtenPaths())

	calls := h.provider.params()
	require.Len(t, calls, 3)
	msgs := calls[1].Messages
	require.Len(t, msgs, 3, "second request: prompt, truncated turn, instruction")
	require.Equal(t, []string{"text"}, blockTypes(msgs[1]))
	require.True(t, msgs[2].Synthetic)
	require.Equal(t, fmt.Sprintf(outputLimitToolCallText, "write_file"), strings.Join(textBlocks(msgs[2]), ""))
	assertAllRequestsPaired(t, calls)

	warnings := h.warnings()
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "dropped 2 unrun tool call(s)")
}

// A truncated turn with no text leaves no assistant message, so the
// instruction joins the trailing user message instead of following it.
func TestLoop_OutputLimitRecovery_ToolOnlyTurnJoinsTrailingUserMessage(t *testing.T) {
	h := newOutputLimitHarness(
		scriptCallTestTool,
		[]types.StreamEvent{writeFileCall("tc_big", "big.txt"), stopWith("max_tokens")},
		scriptEndTurn,
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Empty(t, h.writtenPaths(), "the truncated call must not be dispatched")

	calls := h.provider.params()
	require.Len(t, calls, 3)
	msgs := calls[2].Messages
	require.Len(t, msgs, 3, "third request: prompt, first tool turn, results")
	last := msgs[2]
	require.False(t, last.Synthetic)
	require.Equal(t, []string{"tool_result", "text"}, blockTypes(last))
	require.Equal(t, fmt.Sprintf(outputLimitToolCallText, "write_file"), strings.Join(textBlocks(last), ""))
	assertAllRequestsPaired(t, calls)
}

// Thinking alone may be cut before its signature arrives, so the turn
// is not replayed.
func TestLoop_OutputLimitRecovery_ThinkingOnlyTurnIsNotReplayed(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "thinking", Text: "a very long chain of reasoning"}, stopWith("max_tokens")},
		scriptEndTurn,
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)

	calls := h.provider.params()
	require.Len(t, calls, 2)
	msgs := calls[1].Messages
	require.Len(t, msgs, 1, "second request: the prompt alone")
	want := buildTestConfig().Prompt + "\n\n" + outputLimitNoTextText
	require.Equal(t, want, strings.Join(textBlocks(msgs[0]), ""))
}

func TestLoop_OutputLimitRecovery_BoundEndsRunWithMaxTokens(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Writing."}, writeFileCall("tc_big", "big.txt"), stopWith("max_tokens")},
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "max_tokens", runTrace.Outcome)
	require.Equal(t, maxOutputLimitRecoveries+1, runTrace.Turns)
	require.Empty(t, h.writtenPaths(), "the truncated call must not be dispatched")

	turns, _ := h.emitter.recoveryFlags()
	require.Equal(t, []bool{true, true, false}, turns)
	require.Len(t, h.warnings(), maxOutputLimitRecoveries)
	assertAllRequestsPaired(t, h.provider.params())
}

func TestLoop_OutputLimitRecovery_LastTurnStopIsTerminal(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Partial"}, stopWith("max_tokens")},
	)
	config := buildTestConfig()
	config.MaxTurns = 1

	runTrace, err := h.loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "max_tokens", runTrace.Outcome)
	require.Len(t, h.provider.params(), 1)

	turns, _ := h.emitter.recoveryFlags()
	require.Equal(t, []bool{false}, turns)
	require.Empty(t, h.warnings())
}

func TestLoop_OutputLimitRecovery_BoundResetsPerRun(t *testing.T) {
	truncated := []types.StreamEvent{{Type: "text_delta", Text: "Partial"}, stopWith("max_tokens")}
	h := newOutputLimitHarness(truncated, truncated, scriptEndTurn, truncated, truncated, scriptEndTurn)

	for run := 1; run <= 2; run++ {
		t.Run(fmt.Sprintf("run %d", run), func(t *testing.T) {
			config := buildTestConfig()
			config.RunID = fmt.Sprintf("test-run-%d", run)
			runTrace, err := h.loop.Run(context.Background(), config)
			require.NoError(t, err)
			require.Equal(t, "success", runTrace.Outcome, "after %d recoveries", maxOutputLimitRecoveries)
		})
	}
}

func TestLoop_OutputLimitRecovery_VerificationRetriesShareTheBound(t *testing.T) {
	truncated := []types.StreamEvent{{Type: "text_delta", Text: "Partial"}, stopWith("max_tokens")}
	h := newOutputLimitHarness(truncated, scriptEndTurn, truncated, truncated)
	h.loop.Verifier = &scriptedVerifier{passes: []bool{false, true}}

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "max_tokens", runTrace.Outcome)
	require.Len(t, h.provider.params(), 4)
	require.Len(t, h.warnings(), maxOutputLimitRecoveries, "recovery warnings across the whole run")
}
