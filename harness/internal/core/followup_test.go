package core

import (
	"context"
	"fmt"
	"strings"
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
// ControlEvent arriving during the grace window — with no run active —
// ends the session promptly rather than waiting for the grace timer.
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
		// Good — returned promptly via the idle-cancel route.
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

// resettingFinalizer records the run IDs a loop re-keys its owned
// command-output store to.
type resettingFinalizer struct {
	mu     sync.Mutex
	resets []string
}

func (f *resettingFinalizer) FatalError() error                        { return nil }
func (f *resettingFinalizer) Finalize(context.Context) (string, error) { return "", nil }
func (f *resettingFinalizer) Reset(runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets = append(f.resets, runID)
	return nil
}

// TestRunFollowUpLoop_ResetsCommandOutputPerRun pins that an owned
// command-output store is re-keyed to every run's ID — primary and each
// follow-up — before that run captures anything.
func TestRunFollowUpLoop_ResetsCommandOutputPerRun(t *testing.T) {
	loop, tr := buildFollowUpTestLoop(t)
	store := &resettingFinalizer{}
	loop.CommandOutput = store
	loop.OwnsCommandOutput = true
	config := buildTestConfig()

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("primary Run: %v", err)
	}

	ran := make(chan string, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{
			OnRunComplete: func(cfg *types.RunConfig, _ *types.RunTrace, _ error) { ran <- cfg.RunID },
		})
	}()
	var followUps []string
	for _, prompt := range []string{"first", "second"} {
		tr.FireControl(types.ControlEvent{Type: "user_response", UserResponse: prompt})
		select {
		case id := <-ran:
			followUps = append(followUps, id)
		case <-time.After(3 * time.Second):
			t.Fatalf("follow-up %q did not complete", prompt)
		}
	}
	cancel()
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	want := append([]string{"test-run-1"}, followUps...)
	if strings.Join(store.resets, ",") != strings.Join(want, ",") {
		t.Fatalf("store reset to %v, want one reset per run %v", store.resets, want)
	}
}

// TestRunFollowUpLoop_InputDuringFollowUpRunJoinsThatRun pins that the
// mid-run contract applies to follow-up runs too: input arriving while a
// follow-up is active is injected into that run at its next turn
// boundary, not held back as the prompt of yet another run.
func TestRunFollowUpLoop_InputDuringFollowUpRunJoinsThatRun(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn, scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	config := buildTestConfig()
	prov.onCall = func(i int) {
		if i == 0 {
			tr.FireControl(userResponse("while you are at it", "r2"))
		}
	}

	completed := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{
			OnRunComplete: func(*types.RunConfig, *types.RunTrace, error) { completed <- struct{}{} },
		})
	}()

	time.Sleep(50 * time.Millisecond)
	tr.FireControl(userResponse("start a follow-up", "r1"))
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("follow-up run did not complete")
	}
	// Anything still queued would start another run here.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if n := len(completed); n != 0 {
		t.Fatalf("%d extra follow-up run(s) started; the mid-run input should have joined the active run", n)
	}
	calls := prov.params()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2 (one run, continued once with the injected input)", len(calls))
	}
	if calls[0].Messages[0].Content[0].Text != "start a follow-up" {
		t.Errorf("follow-up prompt = %q", calls[0].Messages[0].Content[0].Text)
	}
	if got := strings.Join(textBlocks(lastMessage(t, calls[1])), ""); got != "while you are at it" {
		t.Errorf("turn-1 injected input = %q", got)
	}
}
