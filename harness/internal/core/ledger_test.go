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
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	if budget == nil || *budget != 60 {
		t.Fatalf("admitted budget = %v, want 60", budget)
	}

	l.Charge(types.TokenUsage{Input: 60})
	if _, err := l.AdmitSpawn(); err == nil || !strings.Contains(err.Error(), "100 of 100") {
		t.Fatalf("AdmitSpawn with no remainder = %v, want a budget refusal naming 100 of 100", err)
	}
	l.Charge(types.TokenUsage{Output: 1})
	if _, err := l.AdmitSpawn(); err == nil {
		t.Fatal("AdmitSpawn over budget succeeded, want a refusal")
	}
}

func TestTokenLedger_AdmitSpawnCapsAdmittedSpawns(t *testing.T) {
	l := NewTokenLedger(nil)
	for i := range maxSpawnsPerRun {
		budget, err := l.AdmitSpawn()
		if err != nil {
			t.Fatalf("spawn %d: %v", i+1, err)
		}
		if budget != nil {
			t.Fatalf("spawn %d admitted budget = %d, want nil for an unbounded run", i+1, *budget)
		}
	}
	if _, err := l.AdmitSpawn(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d sub-agents", maxSpawnsPerRun)) {
		t.Fatalf("spawn %d = %v, want a cap refusal", maxSpawnsPerRun+1, err)
	}
}

func TestTokenLedger_BudgetRefusalDoesNotConsumeASpawn(t *testing.T) {
	l := NewTokenLedger(intPtr(10))
	l.Charge(types.TokenUsage{Input: 10})
	for range maxSpawnsPerRun + 2 {
		if _, err := l.AdmitSpawn(); err == nil {
			t.Fatal("AdmitSpawn with no remainder succeeded")
		}
	}
	if l.spawns != 0 {
		t.Errorf("spawns = %d after refusals, want 0", l.spawns)
	}
}

func TestTokenLedger_AuxiliaryAndExternalUsage(t *testing.T) {
	l := NewTokenLedger(intPtr(100))
	l.Charge(types.TokenUsage{Input: 50})
	l.ChargeAuxiliary(types.TokenUsage{Input: 30, Output: 5})
	l.ReportExternal(types.TokenUsage{Input: 1000})

	if got := l.Total(); got != (types.TokenUsage{Input: 80, Output: 5}) {
		t.Errorf("Total = %+v, want turns plus auxiliary", got)
	}
	if got := l.Auxiliary(); got != (types.TokenUsage{Input: 30, Output: 5}) {
		t.Errorf("Auxiliary = %+v", got)
	}
	if got := l.External(); got != (types.TokenUsage{Input: 1000}) {
		t.Errorf("External = %+v", got)
	}
	if !l.CheckBudget().WithinBudget {
		t.Error("external usage counted toward the budget")
	}
	l.ChargeAuxiliary(types.TokenUsage{Input: 16})
	if l.CheckBudget().WithinBudget {
		t.Error("auxiliary usage past the limit left the run within budget")
	}
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
			if got := budgetExhausted(own, ledger, config); got != tc.exhausted {
				t.Errorf("budgetExhausted = %v, want %v", got, tc.exhausted)
			}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var events int
	for range ch {
		events++
	}
	if events != 3 {
		t.Errorf("forwarded %d events, want 3", events)
	}
	want := types.TokenUsage{Input: 40, Output: 5, CacheRead: 10}
	if got := rec.snapshot(); len(got) != 1 || got[0] != want {
		t.Errorf("charges at close = %+v, want exactly %+v", got, want)
	}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	wantInput := estimateCurrentTokens(params.Messages, true) + estimateSystemPromptTokens(params.System)
	got := rec.snapshot()
	if len(got) != 1 || got[0].Input != wantInput || got[0].Output != 7 {
		t.Errorf("charges = %+v, want input estimate %d and output 7", got, wantInput)
	}
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
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		for range ch {
		}
		got := rec.snapshot()
		if len(got) != 1 {
			t.Fatalf("charges = %+v, want one", got)
		}
		return got[0].Input
	}

	with := estimate(withStructuredResults(func(string) bool { return true }))
	without := estimate(withStructuredResults(func(string) bool { return false }))
	if want := estimateCurrentTokens(params.Messages, true) + estimateSystemPromptTokens("") + estimateToolDefinitionTokens(nil); with != want {
		t.Errorf("estimate with envelope = %d, want %d", with, want)
	}
	if without >= with {
		t.Errorf("estimate without envelope = %d, want less than %d", without, with)
	}
}

func TestChargingProvider_NoChargeForAFailedStream(t *testing.T) {
	inner := &scriptedProvider{turns: [][]types.StreamEvent{{
		{Type: "error", Error: errors.New("overloaded")},
	}}}
	rec := newChargeRecorder()
	ch, err := newChargingProvider(inner, rec.charge).Stream(context.Background(), types.StreamParams{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("charges = %+v, want none for a stream with no usage", got)
	}
}

func TestChargingProvider_PassesThroughAStreamError(t *testing.T) {
	m := newChargingProvider(&errorProvider{err: errors.New("connection refused")}, func(types.TokenUsage) {
		t.Error("charged a stream that never opened")
	})
	if _, err := m.Stream(context.Background(), types.StreamParams{}); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Stream error = %v, want the inner error", err)
	}
	if newChargingProvider(nil, nil) != nil {
		t.Error("newChargingProvider(nil) returned a non-nil adapter")
	}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	<-ch
	cancel()
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned stream was never charged")
	}
	if got := rec.snapshot(); got[0] != (types.TokenUsage{Input: 25, Output: 3}) {
		t.Errorf("charge = %+v, want the drained usage", got[0])
	}
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

// The issue's reproduction: a 100-token run spends 10 per parent call and
// each child would spend 90. The second spawn is refused and the run
// ends budget_exceeded with no provider call after the refusal.
func TestSpawnSubAgent_SecondSpawnIsRefusedOnceTheBudgetIsSpent(t *testing.T) {
	loop, config, prov, rec := ledgerSpawnLoop(t, [][]types.StreamEvent{
		spawnCallsTurn(10, "first subtask"),
		usageTurn(spawnTextTurn("first child done"), 90),
		spawnCallsTurn(10, "second subtask"),
		usageTurn(spawnTextTurn("must not run"), 90),
		usageTurn(spawnTextTurn("must not run"), 10),
	}, intPtr(100))

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "budget_exceeded" {
		t.Errorf("Outcome = %q, want budget_exceeded", runTrace.Outcome)
	}
	if n := prov.calls.Load(); n != 3 {
		t.Errorf("provider calls = %d, want 3 (parent, first child, parent)", n)
	}
	results, errs := rec.snapshot()
	if len(results) != 1 || results[0].TokenBudget == nil || *results[0].TokenBudget != 90 {
		t.Errorf("admitted spawns = %+v, want one with a 90-token budget", results)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "token budget is spent") {
		t.Errorf("spawn errors = %v, want one budget refusal", errs)
	}
	if total, failed := countCalls(runTrace, "spawn_agent"); total != 2 || failed != 1 {
		t.Errorf("spawn_agent calls total=%d failed=%d, want 2/1", total, failed)
	}
	if got, want := runTrace.TokenUsage, rec.ledger.Total(); got != want {
		t.Errorf("RunTrace.TokenUsage = %+v, want the ledger total %+v", got, want)
	}
	if got := runTrace.TokenUsage.Total(); got != 110 {
		t.Errorf("RunTrace.TokenUsage total = %d, want 110", got)
	}
}

func TestSpawnSubAgent_ChildSpawnedNearTheLimitMakesOneCall(t *testing.T) {
	loop, config, prov, rec := ledgerSpawnLoop(t, [][]types.StreamEvent{
		spawnCallsTurn(99, "subtask"),
		readTurn("a.txt", 10),
		usageTurn(spawnTextTurn("child must not get a second call"), 10),
		usageTurn(spawnTextTurn("parent must not get a second call"), 10),
	}, intPtr(100))

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := prov.calls.Load(); n != 2 {
		t.Errorf("provider calls = %d, want 2 (parent, one child call)", n)
	}
	results, _ := rec.snapshot()
	if len(results) != 1 {
		t.Fatalf("spawn results = %+v, want one", results)
	}
	if results[0].Outcome != "budget_exceeded" || results[0].Turns != 1 {
		t.Errorf("child outcome=%q turns=%d, want budget_exceeded after 1 turn", results[0].Outcome, results[0].Turns)
	}
	if results[0].TokenBudget == nil || *results[0].TokenBudget != 1 {
		t.Errorf("child budget = %v, want 1", results[0].TokenBudget)
	}
	if runTrace.Outcome != "budget_exceeded" {
		t.Errorf("parent Outcome = %q, want budget_exceeded", runTrace.Outcome)
	}
}

func TestSpawnSubAgent_NinthSpawnReturnsAToolError(t *testing.T) {
	prompts := make([]string, maxSpawnsPerRun+1)
	turns := [][]types.StreamEvent{nil}
	for i := range prompts {
		prompts[i] = fmt.Sprintf("subtask %d", i+1)
	}
	turns[0] = spawnCallsTurn(1, prompts...)
	for range maxSpawnsPerRun {
		turns = append(turns, usageTurn(spawnTextTurn("child done"), 1))
	}
	turns = append(turns, usageTurn(spawnTextTurn("parent done"), 1))
	loop, config, prov, rec := ledgerSpawnLoop(t, turns, nil)

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", runTrace.Outcome)
	}
	results, errs := rec.snapshot()
	if len(results) != maxSpawnsPerRun {
		t.Errorf("admitted spawns = %d, want %d", len(results), maxSpawnsPerRun)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "maximum per run") {
		t.Errorf("spawn errors = %v, want one cap refusal", errs)
	}
	if n := prov.calls.Load(); n != int32(maxSpawnsPerRun+2) {
		t.Errorf("provider calls = %d, want %d", n, maxSpawnsPerRun+2)
	}
	if total, failed := countCalls(runTrace, "spawn_agent"); total != maxSpawnsPerRun+1 || failed != 1 {
		t.Errorf("spawn_agent calls total=%d failed=%d, want %d/1", total, failed, maxSpawnsPerRun+1)
	}
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
		if _, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p", Mode: "execution"}); err == nil {
			t.Fatal("a research parent spawned an execution child")
		}
	}
	for i := range maxSpawnsPerRun {
		if _, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"}); err != nil {
			t.Fatalf("spawn %d: %v", i+1, err)
		}
	}
	if _, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"}); err == nil {
		t.Fatalf("spawn %d admitted, want a cap refusal", maxSpawnsPerRun+1)
	}
	if ledger.spawns != maxSpawnsPerRun {
		t.Errorf("ledger spawns = %d, want %d", ledger.spawns, maxSpawnsPerRun)
	}
}

func TestPrepareSubAgent_ChildSharesTheParentLedger(t *testing.T) {
	parent := buildSubAgentTestLoop(&mockProvider{})
	parentConfig := buildTestConfig()
	parentConfig.MaxTokenBudget = intPtr(500)
	ledger, release := parent.acquireLedger(parentConfig)
	defer release()
	ledger.Charge(types.TokenUsage{Input: 120})

	run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"})
	if err != nil {
		t.Fatalf("prepareSubAgent: %v", err)
	}
	if run.config.MaxTokenBudget == nil || *run.config.MaxTokenBudget != 380 {
		t.Errorf("child MaxTokenBudget = %v, want the 380-token remainder", run.config.MaxTokenBudget)
	}
	if *parentConfig.MaxTokenBudget != 500 {
		t.Errorf("parent MaxTokenBudget mutated to %d", *parentConfig.MaxTokenBudget)
	}
	childLedger, childRelease := run.loop.acquireLedger(run.config)
	childRelease()
	if childLedger != ledger {
		t.Error("the child loop acquired its own ledger instead of the parent's")
	}
	if parent.activeLedger() != ledger {
		t.Error("the child's release unbound the parent's ledger")
	}
}

// Without a run in progress there is no ledger to admit against; the
// child keeps the parent's configured budget and builds its own ledger.
func TestPrepareSubAgent_OutsideARunKeepsTheParentBudget(t *testing.T) {
	parent := buildSubAgentTestLoop(&mockProvider{})
	parentConfig := buildTestConfig()
	parentConfig.MaxTokenBudget = intPtr(500)
	run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "p"})
	if err != nil {
		t.Fatalf("prepareSubAgent: %v", err)
	}
	if run.config.MaxTokenBudget == nil || *run.config.MaxTokenBudget != 500 {
		t.Errorf("child MaxTokenBudget = %v, want 500", run.config.MaxTokenBudget)
	}
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
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if runTrace.Outcome != "success" || runTrace.TokenUsage.Total() != want {
			t.Errorf("run %d outcome=%q tokens=%d, want success with %d", i+1, runTrace.Outcome, runTrace.TokenUsage.Total(), want)
		}
		if loop.activeLedger() != nil {
			t.Errorf("run %d left its ledger bound", i+1)
		}
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
		if _, err := loop.Run(context.Background(), config); err != nil {
			t.Fatalf("calibration Run: %v", err)
		}
		return rec.compacted
	}
	loop, _ := newReadFileLoop(newProv())
	ample := &recordingStrategy{inner: contextpkg.NewSummariseStrategy(summaryTextProvider{}, "summary-model", 0)}
	loop.Context = ample
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("calibration Run: %v", err)
	}
	start := maxTokensCompactingFrom(t, ample.currentTokens, 4)
	for m := start; m > start/2; m -= 25 {
		if firstCompaction(run(m)) == 4 {
			return m
		}
	}
	t.Fatalf("no maxTokens makes the summarise strategy first compact on the fifth call (started from %d)", start)
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	summaries := summaryProv.turn
	if summaries == 0 {
		t.Fatal("the summarise strategy never called its provider")
	}
	wantAux := types.TokenUsage{Input: 300 * summaries, Output: 20 * summaries}
	if runTrace.AuxiliaryTokens != wantAux {
		t.Errorf("AuxiliaryTokens = %+v, want %+v", runTrace.AuxiliaryTokens, wantAux)
	}
	wantTotal := types.TokenUsage{Input: 6000 + wantAux.Input, Output: wantAux.Output}
	if runTrace.TokenUsage != wantTotal {
		t.Errorf("TokenUsage = %+v, want turns plus summary usage %+v", runTrace.TokenUsage, wantTotal)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "budget_exceeded" {
		t.Errorf("Outcome = %q, want budget_exceeded from the summary's usage", runTrace.Outcome)
	}
	// The summary runs inside the fifth turn's context preparation, so
	// that turn's call is the documented one-call overshoot.
	if n := loopProv.calls.Load(); n != 5 {
		t.Errorf("loop provider calls = %d, want 5", n)
	}
}

func TestSpawnAgentDescriptionStatesTheSpawnCap(t *testing.T) {
	desc := builtins.SpawnAgentTool(nil).Description
	if !strings.Contains(desc, fmt.Sprintf("at most %d sub-agents", maxSpawnsPerRun)) {
		t.Errorf("spawn_agent description does not state the cap of %d:\n%s", maxSpawnsPerRun, desc)
	}
}
