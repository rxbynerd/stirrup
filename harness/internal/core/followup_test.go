package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// controllableTransport is a test Transport that lets the caller fire control
// events programmatically via FireControl. Emitted harness events are
// discarded.
type controllableTransport struct {
	mu       sync.Mutex
	handlers []func(types.ControlEvent)
}

func (t *controllableTransport) Emit(_ types.HarnessEvent) error { return nil }

func (t *controllableTransport) OnControl(handler func(types.ControlEvent)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handlers = append(t.handlers, handler)
}

func (t *controllableTransport) Close() error { return nil }

// FireControl dispatches a control event to all registered handlers.
func (t *controllableTransport) FireControl(event types.ControlEvent) {
	t.mu.Lock()
	hs := make([]func(types.ControlEvent), len(t.handlers))
	copy(hs, t.handlers)
	t.mu.Unlock()
	for _, h := range hs {
		h(event)
	}
}

// buildFollowUpTestLoop creates an AgenticLoop with a controllableTransport
// (returned for injecting control events) and a provider that always
// succeeds.
func buildFollowUpTestLoop(t *testing.T) (*AgenticLoop, *controllableTransport) {
	t.Helper()
	tr := &controllableTransport{}
	loop := buildTestLoop(&mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Follow-up done."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	})
	loop.Transport = tr
	return loop, tr
}

func TestRunFollowUpLoop_ZeroGracePeriod(t *testing.T) {
	loop, _ := buildFollowUpTestLoop(t)
	config := buildTestConfig()

	start := time.Now()
	RunFollowUpLoop(context.Background(), loop, config, 0, FollowUpOptions{})
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected immediate return for zero grace period, took %v", elapsed)
	}
}

func TestRunFollowUpLoop_FollowUpRequestArrives(t *testing.T) {
	loop, tr := buildFollowUpTestLoop(t)
	config := buildTestConfig()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Long grace period: the test exits via context cancellation, not
		// the timer, proving the follow-up path was taken.
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{})
	}()

	time.Sleep(50 * time.Millisecond)

	// Fire a follow-up control event with a new prompt.
	tr.FireControl(types.ControlEvent{
		Type:         "user_response",
		UserResponse: "Please also add tests.",
	})

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		if config.Prompt != "Please also add tests." {
			t.Errorf("expected config.Prompt to be updated to follow-up prompt, got %q", config.Prompt)
		}
		if config.RunID == "test-run-1" {
			t.Error("expected config.RunID to be refreshed for follow-up run")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunFollowUpLoop did not return within timeout after follow-up")
	}
}

func TestRunFollowUpLoop_GracePeriodExpiresNoFollowUp(t *testing.T) {
	loop, _ := buildFollowUpTestLoop(t)
	config := buildTestConfig()

	originalPrompt := config.Prompt
	originalRunID := config.RunID

	start := time.Now()
	RunFollowUpLoop(context.Background(), loop, config, 1, FollowUpOptions{}) // 1-second grace
	elapsed := time.Since(start)

	// Should have waited approximately 1 second (the grace period).
	if elapsed < 800*time.Millisecond {
		t.Fatalf("expected ~1s wait for grace period, returned after %v", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("expected return after ~1s grace period, took %v", elapsed)
	}

	// Config should be unchanged since no follow-up arrived.
	if config.Prompt != originalPrompt {
		t.Errorf("expected prompt to remain %q, got %q", originalPrompt, config.Prompt)
	}
	if config.RunID != originalRunID {
		t.Errorf("expected RunID to remain %q, got %q", originalRunID, config.RunID)
	}
}

func TestRunFollowUpLoop_ContextCancelledDuringWait(t *testing.T) {
	loop, _ := buildFollowUpTestLoop(t)
	config := buildTestConfig()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{}) // long grace — should not be reached
	}()

	// Give the goroutine time to enter the select loop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good — returned promptly after cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("RunFollowUpLoop did not return within 2s of context cancellation")
	}
}

// TestRunFollowUpLoop_CancelControlEventExitsWait verifies that a "cancel"
// ControlEvent arriving during the grace window causes RunFollowUpLoop to
// exit promptly via its cancelCh select arm, without waiting for the grace
// timer to expire. This exercises both the cancel handler registered
// inside RunFollowUpLoop and the <-cancelCh receive in its select loop.
func TestRunFollowUpLoop_CancelControlEventExitsWait(t *testing.T) {
	loop, tr := buildFollowUpTestLoop(t)
	config := buildTestConfig()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Long grace period — if the cancel arm is not wired correctly,
		// the test will hang for the full 30s and the timeout below fires.
		RunFollowUpLoop(context.Background(), loop, config, 30, FollowUpOptions{})
	}()

	// Give OnControl registration a moment to take effect.
	time.Sleep(50 * time.Millisecond)

	tr.FireControl(types.ControlEvent{Type: "cancel"})

	select {
	case <-done:
		// Good — returned promptly via the cancelCh select arm.
	case <-time.After(2 * time.Second):
		t.Fatal("RunFollowUpLoop did not return within 2s of cancel ControlEvent")
	}
}

// TestRunFollowUpLoop_OnRunCompleteReceivesEachRun pins the per-run
// result contract: the callback fires once per accepted follow-up with
// the config the run used (fresh RunID, the follow-up prompt) and the
// trace the run produced, in arrival order.
func TestRunFollowUpLoop_OnRunCompleteReceivesEachRun(t *testing.T) {
	loop, tr := buildFollowUpTestLoop(t)
	config := buildTestConfig()

	type completed struct {
		runID, prompt string
		trace         *types.RunTrace
		err           error
	}
	var mu sync.Mutex
	var seen []completed
	ran := make(chan struct{}, 2)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{
			OnRunComplete: func(cfg *types.RunConfig, rt *types.RunTrace, err error) {
				mu.Lock()
				seen = append(seen, completed{runID: cfg.RunID, prompt: cfg.Prompt, trace: rt, err: err})
				mu.Unlock()
				ran <- struct{}{}
			},
		})
	}()

	time.Sleep(50 * time.Millisecond)
	for _, prompt := range []string{"first follow-up", "second follow-up"} {
		tr.FireControl(types.ControlEvent{Type: "user_response", UserResponse: prompt})
		select {
		case <-ran:
		case <-time.After(3 * time.Second):
			t.Fatalf("OnRunComplete not called for %q", prompt)
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("OnRunComplete called %d times, want 2", len(seen))
	}
	for i, want := range []string{"first follow-up", "second follow-up"} {
		got := seen[i]
		if got.prompt != want {
			t.Errorf("call %d prompt = %q, want %q", i, got.prompt, want)
		}
		if got.err != nil {
			t.Errorf("call %d err = %v, want nil", i, got.err)
		}
		if got.trace == nil {
			t.Fatalf("call %d trace is nil", i)
		}
		if got.trace.ID != got.runID {
			t.Errorf("call %d trace.ID = %q, want the run's config.RunID %q", i, got.trace.ID, got.runID)
		}
		if got.runID == "test-run-1" {
			t.Errorf("call %d still carries the primary RunID", i)
		}
	}
	if seen[0].runID == seen[1].runID {
		t.Errorf("both follow-ups share RunID %q; each run must get its own", seen[0].runID)
	}
}

// deadlineProvider records the deadline of every Stream context so a
// test can pin the per-run budget a follow-up actually receives.
type deadlineProvider struct {
	mu        sync.Mutex
	deadlines []time.Time
	hadDL     []bool
	delay     time.Duration
}

func (p *deadlineProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	dl, ok := ctx.Deadline()
	p.mu.Lock()
	p.deadlines = append(p.deadlines, dl)
	p.hadDL = append(p.hadDL, ok)
	p.mu.Unlock()
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: "ok"}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// TestRunFollowUpLoop_EachRunGetsFreshTimeout pins the budget contract:
// the parent context carries no deadline, and every follow-up run is
// bounded by a fresh RunTimeout minted when it starts — not by whatever
// the primary run left over.
func TestRunFollowUpLoop_EachRunGetsFreshTimeout(t *testing.T) {
	loop, tr := buildFollowUpTestLoop(t)
	prov := &deadlineProvider{}
	loop.Provider = prov
	config := buildTestConfig()

	const runTimeout = 30 * time.Second
	ran := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{
			RunTimeout:    runTimeout,
			OnRunComplete: func(*types.RunConfig, *types.RunTrace, error) { ran <- struct{}{} },
		})
	}()

	time.Sleep(50 * time.Millisecond)
	var started []time.Time
	for _, prompt := range []string{"first", "second"} {
		// Space the runs out so a shared deadline would be visibly
		// shorter on the second run than on the first.
		time.Sleep(300 * time.Millisecond)
		started = append(started, time.Now())
		tr.FireControl(types.ControlEvent{Type: "user_response", UserResponse: prompt})
		select {
		case <-ran:
		case <-time.After(3 * time.Second):
			t.Fatalf("follow-up %q did not complete", prompt)
		}
	}
	cancel()
	<-done

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.deadlines) != 2 {
		t.Fatalf("provider saw %d runs, want 2", len(prov.deadlines))
	}
	for i, dl := range prov.deadlines {
		if !prov.hadDL[i] {
			t.Fatalf("run %d had no deadline; want a fresh %v budget", i, runTimeout)
		}
		budget := dl.Sub(started[i])
		if budget < runTimeout-time.Second || budget > runTimeout+time.Second {
			t.Errorf("run %d budget = %v, want ≈ %v measured from its own start", i, budget, runTimeout)
		}
	}
}

// liveCtxProvider fails the stream when the context it receives is
// already done, which is how a run inheriting a previous run's context
// surfaces at the provider boundary.
type liveCtxProvider struct{ mockProvider }

func (p *liveCtxProvider) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.mockProvider.Stream(ctx, params)
}

// TestLoop_SecondRunOnSameLoopGetsLiveContext pins that a loop reused
// for a follow-up establishes a fresh span-parent context per run: the
// provider must never be called with the previous run's cancelled
// context.
func TestLoop_SecondRunOnSameLoopGetsLiveContext(t *testing.T) {
	prov := &liveCtxProvider{mockProvider: mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "ok"},
		{Type: "message_complete", StopReason: "end_turn"},
	}}}
	loop := buildTestLoop(&mockProvider{})
	loop.Provider = prov
	config := buildTestConfig()

	for i := range 2 {
		config.RunID = fmt.Sprintf("run-%d", i)
		rt, err := loop.Run(context.Background(), config)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if rt.Outcome != "success" {
			t.Fatalf("run %d outcome = %q, want success", i, rt.Outcome)
		}
	}
}

// TestRunFollowUpLoop_GraceCountsIdleTimeAfterRun pins that the grace
// window measures idle time between runs: a follow-up that outlives the
// grace period does not close the window the moment it finishes.
func TestRunFollowUpLoop_GraceCountsIdleTimeAfterRun(t *testing.T) {
	loop, tr := buildFollowUpTestLoop(t)
	loop.Provider = &deadlineProvider{delay: 1200 * time.Millisecond}
	config := buildTestConfig()

	const graceSecs = 1
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(context.Background(), loop, config, graceSecs, FollowUpOptions{})
	}()

	time.Sleep(50 * time.Millisecond)
	fired := time.Now()
	tr.FireControl(types.ControlEvent{Type: "user_response", UserResponse: "slow follow-up"})

	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("RunFollowUpLoop did not return")
	}
	// ≈ 1.2 s run + 1 s idle grace; well over the 1.2 s an
	// arrival-reset timer would allow.
	if elapsed := time.Since(fired); elapsed < 2*time.Second {
		t.Fatalf("returned %v after the follow-up arrived; the grace window should restart after the run completes", elapsed)
	}
}
