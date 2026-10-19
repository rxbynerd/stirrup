package core

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// ctxObservation records what a post-run step saw on the context it was
// handed.
type ctxObservation struct {
	called      bool
	err         error
	hasDeadline bool
	deadline    time.Time
}

func observe(ctx context.Context) ctxObservation {
	deadline, ok := ctx.Deadline()
	return ctxObservation{called: true, err: ctx.Err(), hasDeadline: ok, deadline: deadline}
}

// ctxProbeGit is a GitStrategy whose Finalise records its context and
// optionally runs onFinalise before returning. It reports no git state,
// so it never changes the run's outcome.
type ctxProbeGit struct {
	mu         sync.Mutex
	finalise   ctxObservation
	onFinalise func(ctx context.Context)
}

func (g *ctxProbeGit) Setup(context.Context, string, string) error { return nil }
func (g *ctxProbeGit) Checkpoint(context.Context, string) error    { return nil }

func (g *ctxProbeGit) Finalise(ctx context.Context, _ string) (*types.GitHandoff, error) {
	if g.onFinalise != nil {
		g.onFinalise(ctx)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finalise = observe(ctx)
	return nil, nil
}

func (g *ctxProbeGit) observed() ctxObservation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.finalise
}

// ctxProbeEmitter wraps a JSONL emitter, records the context handed to
// Finish, and can inject a persistence failure that still returns the
// assembled trace, as the TraceEmitter contract requires.
type ctxProbeEmitter struct {
	*trace.JSONLTraceEmitter
	mu        sync.Mutex
	finish    ctxObservation
	finishErr error
}

func newCtxProbeEmitter() *ctxProbeEmitter {
	return &ctxProbeEmitter{JSONLTraceEmitter: trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false)}
}

func (e *ctxProbeEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	e.mu.Lock()
	e.finish = observe(ctx)
	injected := e.finishErr
	e.mu.Unlock()
	rt, err := e.JSONLTraceEmitter.Finish(ctx, outcome)
	if err != nil {
		return rt, err
	}
	return rt, injected
}

func (e *ctxProbeEmitter) observed() ctxObservation {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.finish
}

// assertLiveDetached checks that a post-run step ran on a context that has
// not ended and carries a deadline within budget of start. Callers run on
// the test goroutine.
func assertLiveDetached(t *testing.T, step string, obs ctxObservation, budget time.Duration, start time.Time) {
	t.Helper()
	require.True(t, obs.called, "%s was not called", step)
	require.NoError(t, obs.err, "%s ctx must not inherit the ended run context", step)
	if assert.True(t, obs.hasDeadline, "%s ctx has no deadline, want a bounded budget", step) {
		assert.False(t, obs.deadline.After(start.Add(budget+time.Second)),
			"%s ctx deadline %v exceeds the %v budget", step, obs.deadline.Sub(start), budget)
	}
}

func TestRun_FinaliseAndTraceFinishSurviveRunDeadline(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Provider = &fireAndCloseProvider{
		onStream: func() { time.Sleep(150 * time.Millisecond) },
	}
	probeGit := &ctxProbeGit{}
	loop.Git = probeGit
	emitter := newCtxProbeEmitter()
	loop.Trace = emitter

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	runTrace, err := loop.Run(ctx, buildTestConfig())
	require.NoError(t, err)
	require.NotNil(t, runTrace)
	require.Equal(t, "timeout", runTrace.Outcome)
	assertLiveDetached(t, "Git.Finalise", probeGit.observed(), gitFinaliseBudget, start)
	assertLiveDetached(t, "Trace.Finish", emitter.observed(), traceFinishBudget, start)
}

func TestRun_FinaliseAndTraceFinishSurviveParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loop := buildTestLoop(nil)
	loop.Provider = &fireAndCloseProvider{onStream: cancel}
	probeGit := &ctxProbeGit{}
	loop.Git = probeGit
	emitter := newCtxProbeEmitter()
	loop.Trace = emitter

	start := time.Now()
	runTrace, err := loop.Run(ctx, buildTestConfig())
	require.NoError(t, err)
	require.NotNil(t, runTrace)
	require.Equal(t, "cancelled", runTrace.Outcome)
	assertLiveDetached(t, "Git.Finalise", probeGit.observed(), gitFinaliseBudget, start)
	assertLiveDetached(t, "Trace.Finish", emitter.observed(), traceFinishBudget, start)
}

func TestRun_ShutdownCancelsDetachedPostRunWork(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()
	loop.Shutdown = shutdownCtx

	finaliseCancelled := make(chan struct{})
	probeGit := &ctxProbeGit{
		onFinalise: func(ctx context.Context) {
			go func() {
				time.Sleep(20 * time.Millisecond)
				shutdownCancel()
			}()
			<-ctx.Done()
			close(finaliseCancelled)
		},
	}
	loop.Git = probeGit
	emitter := newCtxProbeEmitter()
	loop.Trace = emitter

	start := time.Now()
	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotNil(t, runTrace)
	require.Equal(t, "success", runTrace.Outcome)
	select {
	case <-finaliseCancelled:
	default:
		require.Fail(t, "Git.Finalise ctx was never cancelled by the shutdown signal")
	}
	assert.LessOrEqual(t, elapsed, 2*time.Second,
		"Run() took %v, want well under the %v finalise budget", elapsed, gitFinaliseBudget)
	require.ErrorIs(t, probeGit.observed().err, context.Canceled, "Git.Finalise ctx.Err()")
	assert.ErrorIs(t, emitter.observed().err, context.Canceled,
		"Trace.Finish ctx should be cancelled once Shutdown is done")
}

func TestRun_TraceFinishFailureStillReturnsTrace(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	emitter := newCtxProbeEmitter()
	emitter.finishErr = errors.New("upload trace: HTTP 503")
	loop.Trace = emitter

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	require.Error(t, err, "Run() should return the trace finish failure")
	require.ErrorContains(t, err, "finish trace")
	require.ErrorContains(t, err, "HTTP 503")
	require.NotNil(t, runTrace, "Run() should return the assembled trace")
	assert.Equal(t, "success", runTrace.Outcome, "a trace finish failure does not change the outcome")
}

func TestFinishWithOutcome_FinishesTraceOnDetachedContext(t *testing.T) {
	loop := buildTestLoop(nil)
	emitter := newCtxProbeEmitter()
	emitter.Start("run", &types.RunConfig{})
	loop.Trace = emitter

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	primary := errors.New("preRun hook failed")
	runTrace, err := loop.finishWithOutcome(ctx, &types.RunConfig{}, "setup_failed", primary, func() {})
	require.ErrorIs(t, err, primary, "finishWithOutcome should return the primary error")
	require.NotNil(t, runTrace)
	require.Equal(t, "setup_failed", runTrace.Outcome)
	assertLiveDetached(t, "Trace.Finish", emitter.observed(), traceFinishBudget, start)
}
