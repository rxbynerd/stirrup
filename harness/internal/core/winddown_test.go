package core

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

const windDownPrefix = "Budget is nearly exhausted."

func windDownConfig(maxTurns int) *types.RunConfig {
	cfg := buildTestConfig()
	cfg.MaxTurns = maxTurns
	cfg.WindDown = &types.WindDownConfig{Enabled: true}
	return cfg
}

// distinctToolTurn calls test_tool with an input unique to n, so a run of
// these turns never trips the stall detector.
func distinctToolTurn(n int, usage ...types.StreamEvent) []types.StreamEvent {
	events := []types.StreamEvent{
		{Type: "tool_call", ID: fmt.Sprintf("tc_%d", n), Name: "test_tool", Input: map[string]any{"n": n}},
	}
	if len(usage) > 0 {
		return append(events, usage...)
	}
	return append(events, stopWith("tool_use"))
}

func windDownCount(msgs []types.Message) int {
	n := 0
	for _, m := range msgs {
		for _, text := range textBlocks(m) {
			n += strings.Count(text, windDownPrefix)
		}
	}
	return n
}

func windDownTriggers(e *turnCapturingEmitter) (turns, records []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.turns {
		turns = append(turns, t.WindDown)
	}
	for _, r := range e.records {
		records = append(records, r.WindDown)
	}
	return turns, records
}

func windDownWarnings(h *outputLimitHarness) []string {
	var out []string
	for _, ev := range h.transport.Events() {
		if ev.Type == "warning" && strings.Contains(ev.Message, "wind down") {
			out = append(out, ev.Message)
		}
	}
	return out
}

// assertWindDownCounts checks that request i carries the wind-down text
// exactly want[i] times.
func assertWindDownCounts(t *testing.T, calls []types.StreamParams, want []int) {
	t.Helper()
	if len(calls) != len(want) {
		t.Fatalf("provider calls = %d, want %d", len(calls), len(want))
	}
	for i, c := range calls {
		if got := windDownCount(c.Messages); got != want[i] {
			t.Errorf("request %d carries %d wind-down messages, want %d", i, got, want[i])
		}
	}
}

func assertNoConsecutiveUserMessages(t *testing.T, calls []types.StreamParams) {
	t.Helper()
	for i, c := range calls {
		for j := 1; j < len(c.Messages); j++ {
			if c.Messages[j].Role == "user" && c.Messages[j-1].Role == "user" {
				t.Errorf("request %d has consecutive user messages at %d and %d", i, j-1, j)
			}
		}
	}
}

func assertTriggers(t *testing.T, e *turnCapturingEmitter, want []string) {
	t.Helper()
	turns, records := windDownTriggers(e)
	if !reflect.DeepEqual(turns, want) {
		t.Errorf("TurnTrace windDown = %q, want %q", turns, want)
	}
	if !reflect.DeepEqual(records, want) {
		t.Errorf("TurnRecord windDown = %q, want %q", records, want)
	}
}

func TestLoop_WindDown_TurnThresholdSendsOnceTwoTurnsBeforeTheEnd(t *testing.T) {
	h, reader := newStallHarness(t,
		distinctToolTurn(0), distinctToolTurn(1), distinctToolTurn(2), distinctToolTurn(3), distinctToolTurn(4))

	runTrace, err := h.loop.Run(context.Background(), windDownConfig(5))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "max_turns" {
		t.Fatalf("Outcome = %q, want max_turns", runTrace.Outcome)
	}
	calls := h.provider.params()
	assertWindDownCounts(t, calls, []int{0, 0, 0, 1, 1})
	assertNudgeJoinsToolResults(t, calls[3].Messages, windDownText, "tc_2")
	assertNoConsecutiveUserMessages(t, calls)
	assertTriggers(t, h.emitter, []string{"", "", "", windDownTriggerTurns, ""})
	if got := counterTotal(t, reader, "stirrup.harness.wind_downs"); got != 1 {
		t.Errorf("wind_downs = %d, want 1", got)
	}
	if w := windDownWarnings(h); len(w) != 1 || !strings.Contains(w[0], "turns budget") {
		t.Errorf("wind-down warnings = %q, want one naming the turns budget", w)
	}
}

func TestLoop_WindDown_TokenThresholdSendsOnce(t *testing.T) {
	budget := 1000
	h, reader := newStallHarness(t,
		distinctToolTurn(0, types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: 500, OutputTokens: 400}),
		distinctToolTurn(1, types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: 1, OutputTokens: 1}),
		distinctToolTurn(2, types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: 1, OutputTokens: 1}),
		append([]types.StreamEvent{{Type: "text_delta", Text: "done"}},
			types.StreamEvent{Type: "message_complete", StopReason: "end_turn", InputTokens: 1, OutputTokens: 1}),
	)
	cfg := windDownConfig(4)
	cfg.MaxTokenBudget = &budget

	runTrace, err := h.loop.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	calls := h.provider.params()
	// Turn 2 also crosses the turn threshold; one instruction per run.
	assertWindDownCounts(t, calls, []int{0, 1, 1, 1})
	assertNudgeJoinsToolResults(t, calls[1].Messages, windDownText, "tc_0")
	assertTriggers(t, h.emitter, []string{"", windDownTriggerTokens, "", ""})
	if got := counterTotal(t, reader, "stirrup.harness.wind_downs"); got != 1 {
		t.Errorf("wind_downs = %d, want 1", got)
	}
}

func TestLoop_WindDown_TimeThresholdSendsOnce(t *testing.T) {
	h, reader := newStallHarness(t,
		distinctToolTurn(0), distinctToolTurn(1), distinctToolTurn(2), distinctToolTurn(3), scriptEndTurn)
	base := time.Now()
	var offset atomic.Int64
	h.loop.clock = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	h.provider.onCall = func(call int) { offset.Store(int64(time.Duration(call+1) * 31 * time.Second)) }

	ctx, cancel := context.WithDeadline(context.Background(), base.Add(100*time.Second))
	defer cancel()
	runTrace, err := h.loop.Run(ctx, windDownConfig(20))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	calls := h.provider.params()
	// 31 s per turn of a 100 s window: turn 3 starts at 93 s elapsed.
	assertWindDownCounts(t, calls, []int{0, 0, 0, 1, 1})
	assertTriggers(t, h.emitter, []string{"", "", "", windDownTriggerTime, ""})
	if got := counterTotal(t, reader, "stirrup.harness.wind_downs"); got != 1 {
		t.Errorf("wind_downs = %d, want 1", got)
	}
}

func TestLoop_WindDown_ReadOnlyModeGetsSummaryOnlyVariant(t *testing.T) {
	h, _ := newStallHarness(t, distinctToolTurn(0), distinctToolTurn(1), scriptEndTurn)
	cfg := windDownConfig(3)
	cfg.Mode = "planning"

	if _, err := h.loop.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := h.provider.params()
	assertWindDownCounts(t, calls, []int{0, 1, 1})
	assertNudgeJoinsToolResults(t, calls[1].Messages, windDownReadOnlyText, "tc_0")
}

func TestLoop_WindDown_FollowsStallNudgeInOneUserMessage(t *testing.T) {
	h, _ := newStallHarness(t,
		testToolTurn("tc_0"), testToolTurn("tc_1"), testToolTurn("tc_2"), distinctToolTurn(3), scriptEndTurn)

	runTrace, err := h.loop.Run(context.Background(), windDownConfig(5))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	calls := h.provider.params()
	want := fmt.Sprintf(stallNudgeText, stallRepeats) + "\n\n" + windDownText
	assertNudgeJoinsToolResults(t, calls[3].Messages, want, "tc_2")
	assertNoConsecutiveUserMessages(t, calls)
}

func TestLoop_WindDown_OncePerRunAcrossVerificationRetries(t *testing.T) {
	h, _ := newStallHarness(t, distinctToolTurn(0), scriptEndTurn, distinctToolTurn(2), scriptEndTurn)
	h.loop.Verifier = &scriptedVerifier{passes: []bool{false, true}}

	runTrace, err := h.loop.Run(context.Background(), windDownConfig(4))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	calls := h.provider.params()
	assertWindDownCounts(t, calls, []int{0, 0, 1, 1})
	assertNoConsecutiveUserMessages(t, calls)
	assertTriggers(t, h.emitter, []string{"", "", windDownTriggerTurns, ""})
}

func TestLoop_WindDown_ResetsPerRun(t *testing.T) {
	h, reader := newStallHarness(t, distinctToolTurn(0), scriptEndTurn)
	cfg := windDownConfig(2)

	for run := 0; run < 2; run++ {
		h.provider.calls = nil
		if _, err := h.loop.Run(context.Background(), cfg); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		assertWindDownCounts(t, h.provider.params(), []int{0, 1})
	}
	if got := counterTotal(t, reader, "stirrup.harness.wind_downs"); got != 2 {
		t.Errorf("wind_downs = %d, want 2 (one per run)", got)
	}
}

func TestLoop_WindDown_OffLeavesRequestsUnchanged(t *testing.T) {
	budget := 1000
	script := [][]types.StreamEvent{
		distinctToolTurn(0, types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: 950, OutputTokens: 1}),
		distinctToolTurn(1, types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: 1, OutputTokens: 1}),
		distinctToolTurn(2, types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: 1, OutputTokens: 1}),
	}
	run := func(wd *types.WindDownConfig) ([]types.StreamParams, *outputLimitHarness, int64) {
		t.Helper()
		h, reader := newStallHarness(t, script...)
		base := time.Now()
		h.loop.clock = func() time.Time { return base.Add(time.Hour) }
		ctx, cancel := context.WithDeadline(context.Background(), base.Add(time.Minute))
		defer cancel()
		cfg := buildTestConfig()
		cfg.MaxTurns = 3
		cfg.MaxTokenBudget = &budget
		cfg.WindDown = wd
		runTrace, err := h.loop.Run(ctx, cfg)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if runTrace.Outcome != "max_turns" {
			t.Fatalf("Outcome = %q, want max_turns", runTrace.Outcome)
		}
		return h.provider.params(), h, counterTotal(t, reader, "stirrup.harness.wind_downs")
	}

	baseline, _, _ := run(nil)
	for _, wd := range []*types.WindDownConfig{nil, {}, {Enabled: false}} {
		calls, h, metric := run(wd)
		if len(calls) != len(baseline) {
			t.Fatalf("windDown=%+v: provider calls = %d, want %d", wd, len(calls), len(baseline))
		}
		for i := range calls {
			if !reflect.DeepEqual(calls[i].Messages, baseline[i].Messages) {
				t.Errorf("windDown=%+v: request %d differs from the baseline", wd, i)
			}
			if windDownCount(calls[i].Messages) != 0 {
				t.Errorf("windDown=%+v: request %d carries a wind-down message", wd, i)
			}
		}
		assertTriggers(t, h.emitter, []string{"", "", ""})
		if metric != 0 {
			t.Errorf("windDown=%+v: wind_downs = %d, want 0", wd, metric)
		}
		if w := windDownWarnings(h); len(w) != 0 {
			t.Errorf("windDown=%+v: warnings = %q, want none", wd, w)
		}
	}

	// The same run with the flag on crosses every threshold.
	calls, _, _ := run(&types.WindDownConfig{Enabled: true})
	if windDownCount(calls[1].Messages) != 1 {
		t.Error("enabled control run did not send a wind-down message")
	}
}

func TestWindDownTrigger(t *testing.T) {
	budget := 1000
	zero := 0
	base := time.Now()
	withDeadline := func(d time.Duration) context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), base.Add(d))
		t.Cleanup(cancel)
		return ctx
	}
	cases := []struct {
		name     string
		ctx      context.Context
		maxTurns int
		turn     int
		budget   *int
		tokens   int
		start    time.Time
		elapsed  time.Duration
		want     string
	}{
		{name: "nothing crossed", ctx: context.Background(), maxTurns: 10, turn: 1},
		{name: "two turns left", ctx: context.Background(), maxTurns: 10, turn: 8, want: windDownTriggerTurns},
		{name: "three turns left", ctx: context.Background(), maxTurns: 10, turn: 7},
		{name: "first turn of a short run", ctx: context.Background(), maxTurns: 2, turn: 0},
		{name: "tokens at 90 percent", ctx: context.Background(), maxTurns: 10, turn: 1, budget: &budget, tokens: 900, want: windDownTriggerTokens},
		{name: "tokens below 90 percent", ctx: context.Background(), maxTurns: 10, turn: 1, budget: &budget, tokens: 899},
		{name: "zero budget", ctx: context.Background(), maxTurns: 10, turn: 1, budget: &zero, tokens: 5},
		{name: "tokens on a first turn", ctx: context.Background(), maxTurns: 10, turn: 0, budget: &budget, tokens: 950, want: windDownTriggerTokens},
		{name: "time at 90 percent", ctx: withDeadline(100 * time.Second), maxTurns: 10, turn: 1, start: base, elapsed: 90 * time.Second, want: windDownTriggerTime},
		{name: "time below 90 percent", ctx: withDeadline(100 * time.Second), maxTurns: 10, turn: 1, start: base, elapsed: 89 * time.Second},
		{name: "no deadline", ctx: context.Background(), maxTurns: 10, turn: 1, start: base, elapsed: time.Hour},
		{name: "no run start", ctx: withDeadline(time.Second), maxTurns: 10, turn: 1, elapsed: time.Hour},
		{name: "turns win over tokens and time", ctx: withDeadline(time.Second), maxTurns: 3, turn: 1, budget: &budget, tokens: 1000, start: base, elapsed: time.Hour, want: windDownTriggerTurns},
		{name: "tokens win over time", ctx: withDeadline(time.Second), maxTurns: 10, turn: 1, budget: &budget, tokens: 1000, start: base, elapsed: time.Hour, want: windDownTriggerTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &AgenticLoop{windDown: windDownState{start: tc.start}}
			l.clock = func() time.Time { return base.Add(tc.elapsed) }
			cfg := &types.RunConfig{MaxTurns: tc.maxTurns, MaxTokenBudget: tc.budget}
			got := l.windDownTrigger(tc.ctx, cfg, tc.turn, types.TokenUsage{Input: tc.tokens})
			if got != tc.want {
				t.Errorf("windDownTrigger = %q, want %q", got, tc.want)
			}
		})
	}
}
