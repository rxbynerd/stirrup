package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// attemptProvider ends its turn on every turnsPerAttempt-th call and
// requests a uniquely-keyed test_tool call otherwise, so each
// verification attempt takes exactly turnsPerAttempt turns.
type attemptProvider struct {
	mu              sync.Mutex
	turnsPerAttempt int
	calls           int
	models          []string
}

func (p *attemptProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.models = append(p.models, params.Model)
	p.mu.Unlock()

	ch := make(chan types.StreamEvent, 2)
	if n%p.turnsPerAttempt == 0 {
		ch <- types.StreamEvent{Type: "text_delta", Text: "Done."}
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	} else {
		ch <- types.StreamEvent{
			Type:  "tool_call",
			ID:    fmt.Sprintf("tc_%d", n),
			Name:  "test_tool",
			Input: map[string]any{"call": n},
		}
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	}
	close(ch)
	return ch, nil
}

func (p *attemptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// passAfterVerifier fails its first failures calls and passes afterwards.
type passAfterVerifier struct {
	failures int
	calls    int
}

func (v *passAfterVerifier) Verify(_ context.Context, _ verifier.VerifyContext) (*types.VerificationResult, error) {
	v.calls++
	if v.calls <= v.failures {
		return &types.VerificationResult{Passed: false, Feedback: "still failing"}, nil
	}
	return &types.VerificationResult{Passed: true}, nil
}

type checkpointRecordingGit struct {
	git.NoneGitStrategy
	mu       sync.Mutex
	messages []string
}

func (g *checkpointRecordingGit) Checkpoint(_ context.Context, message string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.messages = append(g.messages, message)
	return nil
}

type turnRecordingRouter struct {
	inner router.ModelRouter
	mu    sync.Mutex
	turns []int
}

func (r *turnRecordingRouter) Select(ctx context.Context, rc router.RouterContext) router.ModelSelection {
	r.mu.Lock()
	r.turns = append(r.turns, rc.Turn)
	r.mu.Unlock()
	return r.inner.Select(ctx, rc)
}

// promptScanCountingGuard allows everything and counts the PreTurn
// checks whose content carries the run prompt or the dynamic-context
// marker.
type promptScanCountingGuard struct {
	mu             sync.Mutex
	prompt         string
	dynamicMarker  string
	promptScans    int
	dynamicScans   int
	preTurnSources []string
}

func (g *promptScanCountingGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if in.Phase == guard.PhasePreTurn {
		g.preTurnSources = append(g.preTurnSources, in.Source)
		if strings.Contains(in.Content, g.prompt) {
			g.promptScans++
		}
		if strings.Contains(in.Content, g.dynamicMarker) {
			g.dynamicScans++
		}
	}
	return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "counting"}, nil
}

// buildRepairTestLoop wires a loop whose test_tool records the
// CallContext turn of every dispatch.
func buildRepairTestLoop(prov *attemptProvider) (*AgenticLoop, *[]int) {
	loop := buildTestLoop(nil)
	loop.Provider = prov
	var mu sync.Mutex
	callTurns := &[]int{}
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			*callTurns = append(*callTurns, tool.CallContextFrom(ctx).Turn)
			return "tool result", nil
		},
	})
	loop.Tools = registry
	return loop, callTurns
}

func assertStrictlyIncreasing(t *testing.T, label string, got []int) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("%s not strictly increasing: %v", label, got)
			return
		}
	}
}

func TestLoop_VerificationRepairsShareMaxTurns(t *testing.T) {
	tests := []struct {
		name            string
		maxTurns        int
		turnsPerAttempt int
		wantCalls       int
		wantOutcome     string
	}{
		{name: "single turn budget", maxTurns: 1, turnsPerAttempt: 1, wantCalls: 1, wantOutcome: "verification_failed"},
		{name: "turns run out mid-repair", maxTurns: 5, turnsPerAttempt: 2, wantCalls: 5, wantOutcome: "max_turns"},
		{name: "turns run out at a verdict", maxTurns: 4, turnsPerAttempt: 2, wantCalls: 4, wantOutcome: "verification_failed"},
		{name: "retry cap reached first", maxTurns: 20, turnsPerAttempt: 1, wantCalls: 1 + maxVerificationRetries, wantOutcome: "verification_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &attemptProvider{turnsPerAttempt: tc.turnsPerAttempt}
			loop, _ := buildRepairTestLoop(prov)
			loop.Verifier = &failingVerifier{}
			config := buildTestConfig()
			config.MaxTurns = tc.maxTurns

			runTrace, err := loop.Run(context.Background(), config)
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if got := prov.callCount(); got != tc.wantCalls {
				t.Errorf("provider calls = %d, want %d", got, tc.wantCalls)
			}
			if runTrace.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", runTrace.Outcome, tc.wantOutcome)
			}
			if runTrace.Turns != tc.wantCalls {
				t.Errorf("RunTrace.Turns = %d, want %d", runTrace.Turns, tc.wantCalls)
			}
		})
	}
}

func TestLoop_VerificationRepairPassesWithinMaxTurns(t *testing.T) {
	prov := &attemptProvider{turnsPerAttempt: 2}
	loop, _ := buildRepairTestLoop(prov)
	loop.Verifier = &passAfterVerifier{failures: 1}
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success", runTrace.Outcome)
	}
	if got := prov.callCount(); got != 4 {
		t.Errorf("provider calls = %d, want 4", got)
	}
}

func TestLoop_TurnNumbersIncreaseAcrossVerificationRepairs(t *testing.T) {
	prov := &attemptProvider{turnsPerAttempt: 2}
	loop, callTurns := buildRepairTestLoop(prov)
	loop.Verifier = &failingVerifier{}
	rec := &recordingTraceEmitter{}
	loop.Trace = rec
	gitRec := &checkpointRecordingGit{}
	loop.Git = gitRec
	config := buildTestConfig()
	config.MaxTurns = 6

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	rec.mu.Lock()
	var traceTurns, recordTurns []int
	for _, tt := range rec.turns {
		traceTurns = append(traceTurns, tt.Turn)
	}
	for _, tr := range rec.turnRecords {
		recordTurns = append(recordTurns, tr.Turn)
	}
	rec.mu.Unlock()

	wantTurns := []int{0, 1, 2, 3, 4, 5}
	if fmt.Sprint(traceTurns) != fmt.Sprint(wantTurns) {
		t.Errorf("TurnTrace turns = %v, want %v", traceTurns, wantTurns)
	}
	if fmt.Sprint(recordTurns) != fmt.Sprint(wantTurns) {
		t.Errorf("TurnRecord turns = %v, want %v", recordTurns, wantTurns)
	}

	// Tool turns are 0, 2 and 4; CallContext.Turn is one-based.
	if fmt.Sprint(*callTurns) != fmt.Sprint([]int{1, 3, 5}) {
		t.Errorf("CallContext turns = %v, want [1 3 5]", *callTurns)
	}

	wantCheckpoints := []string{
		"Turn 0: 1 tool calls",
		"Turn 2: 1 tool calls",
		"Turn 4: 1 tool calls",
	}
	if fmt.Sprint(gitRec.messages) != fmt.Sprint(wantCheckpoints) {
		t.Errorf("checkpoint messages = %q, want %q", gitRec.messages, wantCheckpoints)
	}

	assertStrictlyIncreasing(t, "TurnTrace turns", traceTurns)
	assertStrictlyIncreasing(t, "TurnRecord turns", recordTurns)
	assertStrictlyIncreasing(t, "CallContext turns", *callTurns)
}

func TestLoop_PreTurnPromptScanRunsOncePerRunAcrossRepairs(t *testing.T) {
	prov := &attemptProvider{turnsPerAttempt: 1}
	loop, _ := buildRepairTestLoop(prov)
	loop.Verifier = &failingVerifier{}
	config := buildTestConfig()
	config.DynamicContext = map[string]types.DynamicContextValue{
		"ticket": {Value: "dynamic-context-marker"},
	}
	g := &promptScanCountingGuard{prompt: config.Prompt, dynamicMarker: "dynamic-context-marker"}
	loop.GuardRail = g

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "verification_failed" {
		t.Fatalf("outcome = %q, want verification_failed", runTrace.Outcome)
	}
	if got := prov.callCount(); got != 1+maxVerificationRetries {
		t.Fatalf("provider calls = %d, want %d", got, 1+maxVerificationRetries)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.promptScans != 1 {
		t.Errorf("prompt scanned %d times, want 1 (pre-turn sources %v)", g.promptScans, g.preTurnSources)
	}
	if g.dynamicScans != 1 {
		t.Errorf("dynamic context scanned %d times, want 1", g.dynamicScans)
	}
}

func TestLoop_RouterTurnsNonDecreasingAcrossRepairs(t *testing.T) {
	prov := &attemptProvider{turnsPerAttempt: 1}
	loop, _ := buildRepairTestLoop(prov)
	loop.Verifier = &failingVerifier{}
	rr := &turnRecordingRouter{inner: router.NewDynamicRouter(router.DynamicRouterConfig{
		DefaultSelection:       router.ModelSelection{Provider: "anthropic", Model: "default-model"},
		ExpensiveSelection:     router.ModelSelection{Provider: "anthropic", Model: "expensive-model"},
		ExpensiveTurnThreshold: 2,
	})}
	loop.Router = rr
	config := buildTestConfig()

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	rr.mu.Lock()
	turns := append([]int(nil), rr.turns...)
	rr.mu.Unlock()
	for i := 1; i < len(turns); i++ {
		if turns[i] < turns[i-1] {
			t.Fatalf("router turns decreased: %v", turns)
		}
	}
	if fmt.Sprint(turns) != fmt.Sprint([]int{0, 1, 2, 3}) {
		t.Errorf("router turns = %v, want [0 1 2 3]", turns)
	}

	prov.mu.Lock()
	models := append([]string(nil), prov.models...)
	prov.mu.Unlock()
	want := []string{"default-model", "default-model", "expensive-model", "expensive-model"}
	if fmt.Sprint(models) != fmt.Sprint(want) {
		t.Errorf("models per call = %v, want %v", models, want)
	}
}
