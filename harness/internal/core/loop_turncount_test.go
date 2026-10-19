package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

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
	require.IsIncreasingf(t, got, "%s not strictly increasing", label)
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
			require.NoError(t, err)
			require.Equal(t, tc.wantCalls, prov.callCount(), "provider calls")
			require.Equal(t, tc.wantOutcome, runTrace.Outcome)
			require.Equal(t, tc.wantCalls, runTrace.Turns, "RunTrace.Turns")
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
	require.NoError(t, err)

	t.Run("outcome", func(t *testing.T) {
		require.Equal(t, "success", runTrace.Outcome)
	})
	t.Run("provider calls", func(t *testing.T) {
		require.Equal(t, 4, prov.callCount())
	})
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

	_, err := loop.Run(context.Background(), config)
	require.NoError(t, err)

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
	t.Run("turn trace", func(t *testing.T) {
		require.Equal(t, wantTurns, traceTurns)
		assertStrictlyIncreasing(t, "turn trace", traceTurns)
	})
	t.Run("turn record", func(t *testing.T) {
		require.Equal(t, wantTurns, recordTurns)
		assertStrictlyIncreasing(t, "turn record", recordTurns)
	})
	// Tool turns are 0, 2 and 4; CallContext.Turn is one-based.
	t.Run("call context", func(t *testing.T) {
		require.Equal(t, []int{1, 3, 5}, *callTurns)
		assertStrictlyIncreasing(t, "call context", *callTurns)
	})
	t.Run("checkpoints", func(t *testing.T) {
		require.Equal(t, []string{
			"Turn 0: 1 tool calls",
			"Turn 2: 1 tool calls",
			"Turn 4: 1 tool calls",
		}, gitRec.messages)
	})
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
	require.NoError(t, err)
	require.Equal(t, "verification_failed", runTrace.Outcome)
	require.Equal(t, 1+maxVerificationRetries, prov.callCount(), "provider calls")

	g.mu.Lock()
	promptScans, dynamicScans := g.promptScans, g.dynamicScans
	preTurnSources := g.preTurnSources
	g.mu.Unlock()

	t.Run("prompt scanned once", func(t *testing.T) {
		require.Equal(t, 1, promptScans, "pre-turn sources %v", preTurnSources)
	})
	t.Run("dynamic context scanned once", func(t *testing.T) {
		require.Equal(t, 1, dynamicScans)
	})
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

	_, err := loop.Run(context.Background(), config)
	require.NoError(t, err)

	rr.mu.Lock()
	turns := append([]int(nil), rr.turns...)
	rr.mu.Unlock()

	prov.mu.Lock()
	models := append([]string(nil), prov.models...)
	prov.mu.Unlock()

	t.Run("turns non-decreasing", func(t *testing.T) {
		require.IsNonDecreasing(t, turns)
	})
	t.Run("turn numbers", func(t *testing.T) {
		require.Equal(t, []int{0, 1, 2, 3}, turns)
	})
	t.Run("models per call", func(t *testing.T) {
		require.Equal(t, []string{"default-model", "default-model", "expensive-model", "expensive-model"}, models)
	})
}
