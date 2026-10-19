package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

func TestTokenLedger_AdmitSpawnCarvesTheRemainder(t *testing.T) {
	l := NewTokenLedger(intPtr(100))
	l.Charge(types.TokenUsage{Input: 30, Output: 10})
	budget, err := l.AdmitSpawn()
	require.NoError(t, err)
	require.NotNil(t, budget)
	require.Equal(t, 60, *budget, "admitted budget is the remainder")

	l.Charge(types.TokenUsage{Input: 60})
	_, err = l.AdmitSpawn()
	require.ErrorContains(t, err, "100 of 100", "no remainder refuses with the budget")

	l.Charge(types.TokenUsage{Output: 1})
	_, err = l.AdmitSpawn()
	require.Error(t, err, "over budget")
}

func TestTokenLedger_AdmitSpawnCapsAdmittedSpawns(t *testing.T) {
	l := NewTokenLedger(nil)
	for i := range maxSpawnsPerRun {
		budget, err := l.AdmitSpawn()
		require.NoError(t, err, "spawn %d", i+1)
		require.Nil(t, budget, "spawn %d carries no budget in an unbounded run", i+1)
	}
	_, err := l.AdmitSpawn()
	require.ErrorContains(t, err, fmt.Sprintf("%d sub-agents", maxSpawnsPerRun), "spawn past the cap")
}

func TestTokenLedger_BudgetRefusalDoesNotConsumeASpawn(t *testing.T) {
	l := NewTokenLedger(intPtr(10))
	l.Charge(types.TokenUsage{Input: 10})
	for range maxSpawnsPerRun + 2 {
		_, err := l.AdmitSpawn()
		require.Error(t, err, "AdmitSpawn with no remainder")
	}
	require.Zero(t, l.spawns, "spawns after refusals")
}

func TestTokenLedger_AuxiliaryAndExternalUsage(t *testing.T) {
	l := NewTokenLedger(intPtr(100))
	l.Charge(types.TokenUsage{Input: 50})
	l.ChargeAuxiliary(types.TokenUsage{Input: 30, Output: 5})
	l.ReportExternal(types.TokenUsage{Input: 1000})

	assert.Equal(t, types.TokenUsage{Input: 80, Output: 5}, l.Total(), "total is turns plus auxiliary")
	assert.Equal(t, types.TokenUsage{Input: 30, Output: 5}, l.Auxiliary())
	assert.Equal(t, types.TokenUsage{Input: 1000}, l.External())
	assert.True(t, l.CheckBudget().WithinBudget, "external usage counted toward the budget")

	l.ChargeAuxiliary(types.TokenUsage{Input: 16})
	assert.False(t, l.CheckBudget().WithinBudget, "auxiliary usage past the limit left the run within budget")
}

func TestBudgetExhausted_AppliesOwnAndSharedLimits(t *testing.T) {
	cases := []struct {
		name      string
		own       int
		ownLimit  *int
		shared    int
		runLimit  *int
		exhausted bool
	}{
		{"unbounded", 1_000, nil, 1_000, nil, false},
		{"own limit exceeded", 11, intPtr(10), 50, intPtr(100), true},
		{"shared limit exceeded", 5, intPtr(10), 101, intPtr(100), true},
		{"both within", 10, intPtr(10), 100, intPtr(100), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			own := &TokenTracker{}
			own.RecordTurn(types.TokenUsage{Input: tc.own})
			ledger := NewTokenLedger(tc.runLimit)
			ledger.Charge(types.TokenUsage{Input: tc.shared})
			config := &types.RunConfig{MaxTokenBudget: tc.ownLimit}
			require.Equal(t, tc.exhausted, budgetExhausted(own, ledger, config))
		})
	}
}

type chargeRecorder struct {
	mu      sync.Mutex
	charges []types.TokenUsage
	done    chan struct{}
}

func newChargeRecorder() *chargeRecorder {
	return &chargeRecorder{done: make(chan struct{}, 16)}
}

func (c *chargeRecorder) charge(u types.TokenUsage) {
	c.mu.Lock()
	c.charges = append(c.charges, u)
	c.mu.Unlock()
	c.done <- struct{}{}
}

func (c *chargeRecorder) snapshot() []types.TokenUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]types.TokenUsage(nil), c.charges...)
}

func TestChargingProvider_ChargesBeforeTheStreamCloses(t *testing.T) {
	inner := &scriptedProvider{turns: [][]types.StreamEvent{{
		{Type: "text_delta", Text: "summary"},
		{Type: "message_complete", InputTokens: 40, CacheReadTokens: 10},
		{Type: "message_complete", StopReason: "end_turn", OutputTokens: 5},
	}}}
	rec := newChargeRecorder()
	ch, err := newChargingProvider(inner, rec.charge).Stream(context.Background(), types.StreamParams{})
	require.NoError(t, err)
	var events int
	for range ch {
		events++
	}
	require.Equal(t, 3, events, "forwarded events")
	want := types.TokenUsage{Input: 40, Output: 5, CacheRead: 10}
	require.Equal(t, []types.TokenUsage{want}, rec.snapshot(), "charges at stream close")
}

func TestChargingProvider_EstimatesUnreportedInput(t *testing.T) {
	inner := &scriptedProvider{turns: [][]types.StreamEvent{{
		{Type: "message_complete", StopReason: "end_turn", OutputTokens: 7},
	}}}
	params := types.StreamParams{
		System:   strings.Repeat("system prompt ", 50),
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("history ", 100)}}}},
	}
	rec := newChargeRecorder()
	ch, err := newChargingProvider(inner, rec.charge).Stream(context.Background(), params)
	require.NoError(t, err)
	for range ch {
	}
	wantInput := estimateCurrentTokens(params.Messages, true) + estimateSystemPromptTokens(params.System)
	got := rec.snapshot()
	require.Len(t, got, 1, "charges")
	require.Equal(t, wantInput, got[0].Input, "estimated input")
	require.Equal(t, 7, got[0].Output, "reported output")
}

func TestChargingProvider_EstimateHonoursStructuredResultSupport(t *testing.T) {
	params := types.StreamParams{
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "call-1", Content: "ok", Kind: types.ToolResultKindFileExcerpt,
				Structured: json.RawMessage(`{"path":"a.txt","startLine":1,"endLine":1,"totalLines":1,"truncated":false,"text":"` + strings.Repeat("output ", 200) + `"}`)},
		}}},
	}
	estimate := func(opts ...chargingOption) int {
		inner := &scriptedProvider{turns: [][]types.StreamEvent{{
			{Type: "message_complete", StopReason: "end_turn", OutputTokens: 1},
		}}}
		rec := newChargeRecorder()
		ch, err := newChargingProvider(inner, rec.charge, opts...).Stream(context.Background(), params)
		require.NoError(t, err)
		for range ch {
		}
		got := rec.snapshot()
		require.Len(t, got, 1, "charges")
		return got[0].Input
	}

	with := estimate(withStructuredResults(func(string) bool { return true }))
	without := estimate(withStructuredResults(func(string) bool { return false }))
	want := estimateCurrentTokens(params.Messages, true) + estimateSystemPromptTokens("") + estimateToolDefinitionTokens(nil)
	require.Equal(t, want, with, "estimate with structured result envelope")
	require.Less(t, without, with, "estimate without structured result envelope")
}

func TestChargingProvider_NoChargeForAFailedStream(t *testing.T) {
	inner := &scriptedProvider{turns: [][]types.StreamEvent{{
		{Type: "error", Error: errors.New("overloaded")},
	}}}
	rec := newChargeRecorder()
	ch, err := newChargingProvider(inner, rec.charge).Stream(context.Background(), types.StreamParams{})
	require.NoError(t, err)
	for range ch {
	}
	require.Empty(t, rec.snapshot(), "a stream with no usage is not charged")
}

func TestChargingProvider_PassesThroughAStreamError(t *testing.T) {
	m := newChargingProvider(&errorProvider{err: errors.New("connection refused")}, func(types.TokenUsage) {
		assert.Fail(t, "charged a stream that never opened")
	})
	_, err := m.Stream(context.Background(), types.StreamParams{})
	require.ErrorContains(t, err, "connection refused")
	require.Nil(t, newChargingProvider(nil, nil), "nil inner provider")
}

// An abandoned consumer must not lose usage: once ctx ends the meter
// drains the inner stream and charges what it reported.
func TestChargingProvider_ChargesUsageOfAnAbandonedStream(t *testing.T) {
	inner := make(chan types.StreamEvent, 3)
	inner <- types.StreamEvent{Type: "text_delta", Text: "partial"}
	inner <- types.StreamEvent{Type: "text_delta", Text: "more"}
	inner <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn", InputTokens: 25, OutputTokens: 3}
	close(inner)
	rec := newChargeRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := newChargingProvider(channelProvider(inner), rec.charge).Stream(ctx, types.StreamParams{})
	require.NoError(t, err)
	<-ch
	cancel()
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "abandoned stream was never charged")
	}
	got := rec.snapshot()
	require.NotEmpty(t, got, "charges")
	require.Equal(t, types.TokenUsage{Input: 25, Output: 3}, got[0], "drained usage")
}

type channelProvider <-chan types.StreamEvent

func (c channelProvider) Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error) {
	return c, nil
}

// usageTurn appends a message_complete reporting tokens of input to a
// scripted turn's events.
func usageTurn(events []types.StreamEvent, tokens int) []types.StreamEvent {
	out := append([]types.StreamEvent(nil), events[:len(events)-1]...)
	last := events[len(events)-1]
	last.InputTokens = tokens
	return append(out, last)
}

// readTurn scripts one read_file call; distinct paths keep the stall
// detector's repeated-call rule out of the way.
func readTurn(path string, tokens int) []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "tool_call", ID: "tc_read", Name: "read_file", Input: map[string]any{"path": path}},
		{Type: "message_complete", StopReason: "tool_use", InputTokens: tokens},
	}
}

// spawnCallsTurn scripts one spawn_agent call per prompt.
func spawnCallsTurn(tokens int, prompts ...string) []types.StreamEvent {
	events := make([]types.StreamEvent, 0, len(prompts)+1)
	for i, p := range prompts {
		events = append(events, types.StreamEvent{
			Type: "tool_call", ID: fmt.Sprintf("tc_spawn_%d", i), Name: "spawn_agent",
			Input: map[string]any{"prompt": p},
		})
	}
	return append(events, types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: tokens})
}

type spawnRecorder struct {
	mu      sync.Mutex
	results []SubAgentResult
	errs    []error
	ledger  *TokenLedger
}

func (r *spawnRecorder) snapshot() ([]SubAgentResult, []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SubAgentResult(nil), r.results...), append([]error(nil), r.errs...)
}

// ledgerSpawnLoop builds a parent loop whose spawn_agent tool runs real
// sub-agents on the same scripted provider, so parent and child calls
// draw from one script in order.
func ledgerSpawnLoop(t *testing.T, turns [][]types.StreamEvent, budget *int) (*AgenticLoop, *types.RunConfig, *spawnCountingProvider, *spawnRecorder) {
	t.Helper()
	prov := &spawnCountingProvider{scriptedProvider: scriptedProvider{turns: turns}}
	loop := buildSubAgentTestLoop(&mockProvider{})
	loop.Provider = prov
	config := buildTestConfig()
	config.MaxTokenBudget = budget

	rec := &spawnRecorder{}
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "read_file",
		Description: "stub read_file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "file contents", nil
		},
	})
	registry.Register(builtins.SpawnAgentTool(func(ctx context.Context, prompt, mode string, maxTurns int) (json.RawMessage, error) {
		rec.mu.Lock()
		rec.ledger = loop.activeLedger()
		rec.mu.Unlock()
		result, err := SpawnSubAgent(ctx, loop, config, SubAgentConfig{Prompt: prompt, Mode: mode, MaxTurns: maxTurns})
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if err != nil {
			rec.errs = append(rec.errs, err)
			return nil, err
		}
		rec.results = append(rec.results, *result)
		return json.Marshal(result)
	}))
	loop.Tools = registry
	return loop, config, prov, rec
}

// A 100-token run spends 10 per parent call and each child would spend 90.
// The second spawn is refused and the run ends budget_exceeded with no
// provider call after the refusal.
func TestSpawnSubAgent_SecondSpawnIsRefusedOnceTheBudgetIsSpent(t *testing.T) {
	loop, config, prov, rec := ledgerSpawnLoop(t, [][]types.StreamEvent{
		spawnCallsTurn(10, "first subtask"),
		usageTurn(spawnTextTurn("first child done"), 90),
		spawnCallsTurn(10, "second subtask"),
		usageTurn(spawnTextTurn("must not run"), 90),
		usageTurn(spawnTextTurn("must not run"), 10),
	}, intPtr(100))

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "budget_exceeded", runTrace.Outcome)
	require.EqualValues(t, 3, prov.calls.Load(), "provider calls: parent, first child, parent")

	results, errs := rec.snapshot()
	require.Len(t, results, 1, "admitted spawns")
	require.NotNil(t, results[0].TokenBudget, "admitted spawn budget")
	require.Equal(t, 90, *results[0].TokenBudget)

	require.Len(t, errs, 1, "spawn refusals")
	require.ErrorContains(t, errs[0], "token budget is spent")

	total, failed := countCalls(runTrace, "spawn_agent")
	require.Equal(t, 2, total, "spawn_agent calls")
	require.Equal(t, 1, failed, "failed spawn_agent calls")

	require.Equal(t, rec.ledger.Total(), runTrace.TokenUsage, "run usage equals the ledger total")
	require.Equal(t, 110, runTrace.TokenUsage.Total())
}

func TestSpawnSubAgent_ChildSpawnedNearTheLimitMakesOneCall(t *testing.T) {
	loop, config, prov, rec := ledgerSpawnLoop(t, [][]types.StreamEvent{
		spawnCallsTurn(99, "subtask"),
		readTurn("a.txt", 10),
		usageTurn(spawnTextTurn("child must not get a second call"), 10),
		usageTurn(spawnTextTurn("parent must not get a second call"), 10),
	}, intPtr(100))

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.EqualValues(t, 2, prov.calls.Load(), "provider calls: parent, one child call")

	results, _ := rec.snapshot()
	require.Len(t, results, 1, "spawn results")
	require.Equal(t, "budget_exceeded", results[0].Outcome, "child outcome")
	require.Equal(t, 1, results[0].Turns, "child turns")
	require.NotNil(t, results[0].TokenBudget, "child budget")
	require.Equal(t, 1, *results[0].TokenBudget)

	require.Equal(t, "budget_exceeded", runTrace.Outcome, "parent outcome")
}

func TestSpawnSubAgent_NinthSpawnReturnsAToolError(t *testing.T) {
	prompts := make([]string, maxSpawnsPerRun+1)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("subtask %d", i+1)
	}
	turns := [][]types.StreamEvent{spawnCallsTurn(1, prompts...)}
	for range maxSpawnsPerRun {
		turns = append(turns, usageTurn(spawnTextTurn("child done"), 1))
	}
	turns = append(turns, usageTurn(spawnTextTurn("parent done"), 1))
	loop, config, prov, rec := ledgerSpawnLoop(t, turns, nil)

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)

	results, errs := rec.snapshot()
	require.Len(t, results, maxSpawnsPerRun, "admitted spawns")
	require.Len(t, errs, 1, "spawn refusals")
	require.ErrorContains(t, errs[0], "maximum per run")
	require.EqualValues(t, maxSpawnsPerRun+2, prov.calls.Load(), "provider calls")

	total, failed := countCalls(runTrace, "spawn_agent")
	require.Equal(t, maxSpawnsPerRun+1, total, "spawn_agent calls")
	require.Equal(t, 1, failed, "failed spawn_agent calls")
}

func TestPrepareSubAgent_RejectedSpawnDoesNotCountTowardTheCap(t *testing.T) {
	registry, _ := spawnParentRegistry(t, false)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	parentConfig := buildTestConfig()
	parentConfig.Mode = "research"
	parentConfig.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	ledger, release := parent.acquireLedger(parentConfig)
	defer release()

	for range maxSpawnsPerRun {
		_, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p", Mode: "execution"})
		require.Error(t, err, "a research parent spawned an execution child")
	}
	for i := range maxSpawnsPerRun {
		_, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"})
		require.NoError(t, err, "spawn %d", i+1)
	}
	_, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"})
	require.Error(t, err, "spawn %d past the cap", maxSpawnsPerRun+1)
	require.Equal(t, maxSpawnsPerRun, ledger.spawns, "ledger spawns")
}

func TestPrepareSubAgent_ChildSharesTheParentLedger(t *testing.T) {
	parent := buildSubAgentTestLoop(&mockProvider{})
	parentConfig := buildTestConfig()
	parentConfig.MaxTokenBudget = intPtr(500)
	ledger, release := parent.acquireLedger(parentConfig)
	defer release()
	ledger.Charge(types.TokenUsage{Input: 120})

	run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"})
	require.NoError(t, err)
	require.NotNil(t, run.config.MaxTokenBudget, "child budget")
	require.Equal(t, 380, *run.config.MaxTokenBudget, "child budget is the remainder")
	require.Equal(t, 500, *parentConfig.MaxTokenBudget, "parent budget is unchanged")

	childLedger, childRelease := run.loop.acquireLedger(run.config)
	childRelease()
	require.Same(t, ledger, childLedger, "child loop shares the parent ledger")
	require.Same(t, ledger, parent.activeLedger(), "child release keeps the parent ledger bound")
}

// Without a run in progress there is no ledger to admit against; the
// child keeps the parent's configured budget and builds its own ledger.
func TestPrepareSubAgent_OutsideARunKeepsTheParentBudget(t *testing.T) {
	parent := buildSubAgentTestLoop(&mockProvider{})
	parentConfig := buildTestConfig()
	parentConfig.MaxTokenBudget = intPtr(500)
	run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"})
	require.NoError(t, err)
	require.NotNil(t, run.config.MaxTokenBudget, "child budget")
	require.Equal(t, 500, *run.config.MaxTokenBudget)
}

func TestRun_EachTopLevelRunGetsAFreshLedger(t *testing.T) {
	prov := &scriptedProvider{turns: [][]types.StreamEvent{
		usageTurn(spawnTextTurn("first run"), 150),
		usageTurn(spawnTextTurn("second run"), 10),
	}}
	loop := buildSubAgentTestLoop(&mockProvider{})
	loop.Provider = prov
	config := buildTestConfig()
	config.MaxTokenBudget = intPtr(100)

	for i, want := range []int{150, 10} {
		runTrace, err := loop.Run(context.Background(), config)
		require.NoError(t, err, "run %d", i+1)
		require.Equal(t, "success", runTrace.Outcome, "run %d outcome", i+1)
		require.Equal(t, want, runTrace.TokenUsage.Total(), "run %d tokens", i+1)
		require.Nil(t, loop.activeLedger(), "run %d left its ledger bound", i+1)
	}
}

// newReadFileLoop returns a loop whose read_file results are large enough
// for the summarise strategy to compact, bound to a fresh ledger.
func newReadFileLoop(prov provider.ProviderAdapter) (*AgenticLoop, *ledgerBinding) {
	loop := buildSubAgentTestLoop(&mockProvider{})
	loop.Provider = prov
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "read_file",
		Description: "stub read_file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("long file contents ", 200), nil
		},
	})
	loop.Tools = registry
	binding := &ledgerBinding{}
	loop.ledger = binding
	return loop, binding
}

// firstCompaction returns the index of the first call a recording strategy
// compacted on, or -1.
func firstCompaction(compacted []bool) int {
	for i, c := range compacted {
		if c {
			return i
		}
	}
	return -1
}

// maxTokensCompactingOnFifthCall returns the contextStrategy.maxTokens at
// which a summarise strategy first compacts on the loop's fifth model
// call. The loop scales its budget by the ratio of reported to estimated
// input, so the starting point taken from an ample-window run is refined
// by trial runs of the same script.
func maxTokensCompactingOnFifthCall(t *testing.T, newProv func() provider.ProviderAdapter) int {
	t.Helper()
	run := func(maxTokens int) []bool {
		loop, _ := newReadFileLoop(newProv())
		rec := &recordingStrategy{inner: contextpkg.NewSummariseStrategy(summaryTextProvider{}, "summary-model", 0)}
		loop.Context = rec
		config := buildTestConfig()
		config.ContextStrategy.MaxTokens = maxTokens
		_, err := loop.Run(context.Background(), config)
		require.NoError(t, err, "calibration run")
		return rec.compacted
	}
	loop, _ := newReadFileLoop(newProv())
	ample := &recordingStrategy{inner: contextpkg.NewSummariseStrategy(summaryTextProvider{}, "summary-model", 0)}
	loop.Context = ample
	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err, "calibration run")

	start := maxTokensCompactingFrom(t, ample.currentTokens, 4)
	for m := start; m > start/2; m -= 25 {
		if firstCompaction(run(m)) == 4 {
			return m
		}
	}
	require.Failf(t, "no maxTokens makes the summarise strategy first compact on the fifth call", "started from %d", start)
	return 0
}

func TestRun_AuxiliaryUsageIsChargedAndReportedSeparately(t *testing.T) {
	newLoopProv := func() provider.ProviderAdapter {
		return &scriptedProvider{turns: [][]types.StreamEvent{
			readTurn("a.txt", 1000),
			readTurn("b.txt", 1000),
			readTurn("c.txt", 1000),
			readTurn("d.txt", 1000),
			readTurn("e.txt", 1000),
			usageTurn(spawnTextTurn("done"), 1000),
		}}
	}
	summaryProv := &scriptedProvider{turns: [][]types.StreamEvent{{
		{Type: "text_delta", Text: "condensed history"},
		{Type: "message_complete", StopReason: "end_turn", InputTokens: 300, OutputTokens: 20},
	}}}

	loop, binding := newReadFileLoop(newLoopProv())
	loop.Context = contextpkg.NewSummariseStrategy(newChargingProvider(summaryProv, binding.chargeAuxiliary), "summary-model", 0)

	config := buildTestConfig()
	config.ContextStrategy = types.ContextStrategyConfig{Type: "summarise", MaxTokens: maxTokensCompactingOnFifthCall(t, newLoopProv)}

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)

	summaries := summaryProv.turn
	require.NotZero(t, summaries, "the summarise strategy never called its provider")
	wantAux := types.TokenUsage{Input: 300 * summaries, Output: 20 * summaries}
	require.Equal(t, wantAux, runTrace.AuxiliaryTokens, "auxiliary usage")
	wantTotal := types.TokenUsage{Input: 6000 + wantAux.Input, Output: wantAux.Output}
	require.Equal(t, wantTotal, runTrace.TokenUsage, "total is turns plus summary usage")
}

func TestRun_AuxiliaryUsageCountsTowardTheBudget(t *testing.T) {
	newLoopProv := func() *spawnCountingProvider {
		return &spawnCountingProvider{scriptedProvider: scriptedProvider{turns: [][]types.StreamEvent{
			readTurn("a.txt", 100),
			readTurn("b.txt", 100),
			readTurn("c.txt", 100),
			readTurn("d.txt", 100),
			readTurn("e.txt", 100),
			usageTurn(spawnTextTurn("must not run"), 100),
		}}}
	}
	summaryProv := &scriptedProvider{turns: [][]types.StreamEvent{{
		{Type: "text_delta", Text: "condensed history"},
		{Type: "message_complete", StopReason: "end_turn", InputTokens: 5000},
	}}}
	loopProv := newLoopProv()
	loop, binding := newReadFileLoop(loopProv)
	loop.Context = contextpkg.NewSummariseStrategy(newChargingProvider(summaryProv, binding.chargeAuxiliary), "summary-model", 0)

	config := buildTestConfig()
	config.ContextStrategy = types.ContextStrategyConfig{
		Type:      "summarise",
		MaxTokens: maxTokensCompactingOnFifthCall(t, func() provider.ProviderAdapter { return newLoopProv() }),
	}
	config.MaxTokenBudget = intPtr(1000)

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	assert.Equal(t, "budget_exceeded", runTrace.Outcome, "the summary's usage should exhaust the budget")
	// The summary runs inside the fifth turn's context preparation, so
	// that turn's call is the documented one-call overshoot.
	assert.EqualValues(t, 5, loopProv.calls.Load(), "loop provider calls")
}

func TestSpawnAgent_DescriptionStatesTheSpawnCap(t *testing.T) {
	desc := builtins.SpawnAgentTool(nil).Description
	require.Contains(t, desc, fmt.Sprintf("at most %d sub-agents", maxSpawnsPerRun))
}
