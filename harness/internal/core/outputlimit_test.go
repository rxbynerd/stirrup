package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

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

// assertToolUsePaired fails when an assistant tool_use block is not
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
			if b.Type == "tool_use" && !answered[b.ID] {
				t.Errorf("%s: message %d holds tool_use %q (%s) with no paired tool_result", label, i, b.ID, b.Name)
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if runTrace.Turns != 2 {
		t.Errorf("Turns = %d, want 2", runTrace.Turns)
	}
	if runTrace.FinalAssistantText != "Partial answerrest of the answer" {
		t.Errorf("FinalAssistantText = %q, want the cut-off text joined with its continuation", runTrace.FinalAssistantText)
	}

	calls := h.provider.params()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	msgs := calls[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages, want prompt, truncated turn, instruction", len(msgs))
	}
	if msgs[1].Role != "assistant" || strings.Join(textBlocks(msgs[1]), "") != "Partial answer" {
		t.Errorf("truncated assistant turn = %+v, want its text kept", msgs[1])
	}
	if msgs[2].Role != "user" || !msgs[2].Synthetic || strings.Join(textBlocks(msgs[2]), "") != outputLimitContinueText {
		t.Errorf("instruction = %+v, want a synthetic user message with the continue text", msgs[2])
	}

	turns, records := h.emitter.recoveryFlags()
	if fmt.Sprint(turns) != "[true false]" || fmt.Sprint(records) != "[true false]" {
		t.Errorf("recovery flags: turns %v, records %v; want [true false] for both", turns, records)
	}
	warnings := h.warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "recovery 1 of 2") {
		t.Errorf("warnings = %q, want one naming recovery 1 of 2", warnings)
	}
}

func TestLoop_OutputLimitRecovery_JoinsEveryContinuationOfTheAnswer(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "one "}, stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "two "}, stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "three"}, stopWith("end_turn")},
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if runTrace.FinalAssistantText != "one two three" {
		t.Errorf("FinalAssistantText = %q, want %q", runTrace.FinalAssistantText, "one two three")
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "guardrail_blocked" {
		t.Fatalf("Outcome = %q, want guardrail_blocked", runTrace.Outcome)
	}
	if fmt.Sprint(g.denied) != "[first half second half]" {
		t.Errorf("post-turn guard saw %q, want the joined answer", g.denied)
	}
	if runTrace.FinalAssistantText != "Checking first." {
		t.Errorf("FinalAssistantText = %q, want the text from before the denied answer began", runTrace.FinalAssistantText)
	}
}

func TestLoop_OutputLimitRecovery_ToolCallPreambleIsNotJoined(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Writing it now."}, writeFileCall("tc_big", "big.txt"), stopWith("max_tokens")},
		[]types.StreamEvent{{Type: "text_delta", Text: "Done."}, stopWith("end_turn")},
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.FinalAssistantText != "Done." {
		t.Errorf("FinalAssistantText = %q, want only the final turn's text", runTrace.FinalAssistantText)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if got := strings.Join(h.writtenPaths(), ","); got != "part1.txt,part2.txt" {
		t.Errorf("write_file ran for %q, want only the follow-up calls part1.txt,part2.txt", got)
	}

	calls := h.provider.params()
	if len(calls) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(calls))
	}
	msgs := calls[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages, want prompt, truncated turn, instruction", len(msgs))
	}
	if got := strings.Join(blockTypes(msgs[1]), ","); got != "text" {
		t.Errorf("truncated assistant turn blocks = %s, want its text only", got)
	}
	want := fmt.Sprintf(outputLimitToolCallText, "write_file")
	if !msgs[2].Synthetic || strings.Join(textBlocks(msgs[2]), "") != want {
		t.Errorf("instruction = %+v, want synthetic %q", msgs[2], want)
	}
	assertAllRequestsPaired(t, calls)

	warnings := h.warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "dropped 2 unrun tool call(s)") {
		t.Errorf("warnings = %q, want one naming the two dropped calls", warnings)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if paths := h.writtenPaths(); len(paths) != 0 {
		t.Errorf("write_file ran for %v, want no dispatch of the truncated call", paths)
	}

	calls := h.provider.params()
	if len(calls) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(calls))
	}
	msgs := calls[2].Messages
	if len(msgs) != 3 {
		t.Fatalf("third request has %d messages, want prompt, first tool turn, results", len(msgs))
	}
	last := msgs[2]
	if last.Synthetic || strings.Join(blockTypes(last), ",") != "tool_result,text" {
		t.Errorf("trailing user message = %+v, want unmarked tool_result then text", last)
	}
	if want := fmt.Sprintf(outputLimitToolCallText, "write_file"); strings.Join(textBlocks(last), "") != want {
		t.Errorf("instruction text = %q, want %q", textBlocks(last), want)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	calls := h.provider.params()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	msgs := calls[1].Messages
	if len(msgs) != 1 {
		t.Fatalf("second request has %d messages, want the prompt alone", len(msgs))
	}
	want := buildTestConfig().Prompt + "\n\n" + outputLimitNoTextText
	if got := strings.Join(textBlocks(msgs[0]), ""); got != want {
		t.Errorf("prompt message text = %q, want %q", got, want)
	}
}

func TestLoop_OutputLimitRecovery_BoundEndsRunWithMaxTokens(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Writing."}, writeFileCall("tc_big", "big.txt"), stopWith("max_tokens")},
	)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "max_tokens" {
		t.Errorf("Outcome = %q, want max_tokens", runTrace.Outcome)
	}
	if runTrace.Turns != maxOutputLimitRecoveries+1 {
		t.Errorf("Turns = %d, want %d", runTrace.Turns, maxOutputLimitRecoveries+1)
	}
	if paths := h.writtenPaths(); len(paths) != 0 {
		t.Errorf("write_file ran for %v, want no truncated call dispatched", paths)
	}
	turns, _ := h.emitter.recoveryFlags()
	if fmt.Sprint(turns) != "[true true false]" {
		t.Errorf("turn recovery flags = %v, want [true true false]", turns)
	}
	if n := len(h.warnings()); n != maxOutputLimitRecoveries {
		t.Errorf("recovery warnings = %d, want %d", n, maxOutputLimitRecoveries)
	}
	assertAllRequestsPaired(t, h.provider.params())
}

func TestLoop_OutputLimitRecovery_LastTurnStopIsTerminal(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "Partial"}, stopWith("max_tokens")},
	)
	config := buildTestConfig()
	config.MaxTurns = 1

	runTrace, err := h.loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "max_tokens" {
		t.Errorf("Outcome = %q, want max_tokens", runTrace.Outcome)
	}
	if n := len(h.provider.params()); n != 1 {
		t.Errorf("provider calls = %d, want 1", n)
	}
	if turns, _ := h.emitter.recoveryFlags(); fmt.Sprint(turns) != "[false]" {
		t.Errorf("turn recovery flags = %v, want [false]", turns)
	}
	if w := h.warnings(); len(w) != 0 {
		t.Errorf("warnings = %q, want none", w)
	}
}

func TestLoop_OutputLimitRecovery_BoundResetsPerRun(t *testing.T) {
	truncated := []types.StreamEvent{{Type: "text_delta", Text: "Partial"}, stopWith("max_tokens")}
	h := newOutputLimitHarness(truncated, truncated, scriptEndTurn, truncated, truncated, scriptEndTurn)

	for run := 1; run <= 2; run++ {
		config := buildTestConfig()
		config.RunID = fmt.Sprintf("test-run-%d", run)
		runTrace, err := h.loop.Run(context.Background(), config)
		if err != nil {
			t.Fatalf("run %d: Run: %v", run, err)
		}
		if runTrace.Outcome != "success" {
			t.Errorf("run %d: Outcome = %q, want success after %d recoveries", run, runTrace.Outcome, maxOutputLimitRecoveries)
		}
	}
}
