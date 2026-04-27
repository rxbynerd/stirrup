package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// cancellableTransport is a Transport implementation that captures emitted
// harness events (so tests can assert on the final "done" event) and lets
// the test inject control events via FireControl. OnControl registrations
// are fanned out in the same way the production StdioTransport does.
type cancellableTransport struct {
	mu       sync.Mutex
	handlers []func(types.ControlEvent)
	events   []types.HarnessEvent
}

func (t *cancellableTransport) Emit(event types.HarnessEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
	return nil
}

func (t *cancellableTransport) OnControl(handler func(types.ControlEvent)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handlers = append(t.handlers, handler)
}

func (t *cancellableTransport) Close() error { return nil }

func (t *cancellableTransport) FireControl(event types.ControlEvent) {
	t.mu.Lock()
	hs := make([]func(types.ControlEvent), len(t.handlers))
	copy(hs, t.handlers)
	t.mu.Unlock()
	for _, h := range hs {
		h(event)
	}
}

// Events returns a snapshot of the events emitted so far.
func (t *cancellableTransport) Events() []types.HarnessEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]types.HarnessEvent, len(t.events))
	copy(out, t.events)
	return out
}

// cancellingProvider fires a cancel ControlEvent on the first call, then
// blocks the stream until the run context is cancelled. This simulates a
// slow provider whose outbound call is interrupted by a mid-run cancel.
type cancellingProvider struct {
	tr        *cancellableTransport
	fireOnce  sync.Once
	calls     int
	callsLock sync.Mutex
}

func (p *cancellingProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	p.callsLock.Lock()
	p.calls++
	p.callsLock.Unlock()

	ch := make(chan types.StreamEvent)
	go func() {
		defer close(ch)
		// Fire cancel from the provider goroutine so the outer Run has
		// already reached the streaming stage and has registered the
		// transport's cancel handler.
		p.fireOnce.Do(func() {
			p.tr.FireControl(types.ControlEvent{Type: "cancel"})
		})
		// Block until the run context is cancelled — then exit the stream
		// with a ctx.Err error event. runInnerLoop will map this to the
		// ctx-done sentinel and the outer Run will classify it.
		<-ctx.Done()
		ch <- types.StreamEvent{Type: "error", Error: ctx.Err()}
	}()
	return ch, nil
}

// slowProvider blocks on Stream until the context is cancelled. Used to
// test deadline-based timeout classification.
type slowProvider struct{}

func (p *slowProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent)
	go func() {
		defer close(ch)
		<-ctx.Done()
		ch <- types.StreamEvent{Type: "error", Error: ctx.Err()}
	}()
	return ch, nil
}

func TestLoop_CancelControlEvent_Cancelled(t *testing.T) {
	tr := &cancellableTransport{}
	provider := &cancellingProvider{tr: tr}

	loop := buildTestLoop(nil)
	loop.Provider = provider
	loop.Transport = tr

	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace")
	}
	if runTrace.Outcome != "cancelled" {
		t.Errorf("expected outcome 'cancelled', got %q", runTrace.Outcome)
	}

	// Verify the "done" HarnessEvent carries stop_reason="cancelled".
	var doneEvents []types.HarnessEvent
	for _, ev := range tr.Events() {
		if ev.Type == "done" {
			doneEvents = append(doneEvents, ev)
		}
	}
	if len(doneEvents) != 1 {
		t.Fatalf("expected exactly one 'done' event, got %d", len(doneEvents))
	}
	if doneEvents[0].StopReason != "cancelled" {
		t.Errorf("expected done.stop_reason 'cancelled', got %q", doneEvents[0].StopReason)
	}
}

func TestLoop_DeadlineExceeded_Timeout(t *testing.T) {
	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Provider = &slowProvider{}
	loop.Transport = tr

	config := buildTestConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace")
	}
	if runTrace.Outcome != "timeout" {
		t.Errorf("expected outcome 'timeout', got %q", runTrace.Outcome)
	}

	// Verify the "done" HarnessEvent carries stop_reason="timeout".
	var gotStopReason string
	for _, ev := range tr.Events() {
		if ev.Type == "done" {
			gotStopReason = ev.StopReason
		}
	}
	if gotStopReason != "timeout" {
		t.Errorf("expected done.stop_reason 'timeout', got %q", gotStopReason)
	}
}

// cancellingProviderOnStream fires the given callback on every Stream call
// and then blocks until the run context is cancelled. Unlike
// cancellingProvider it does not force a FireControl, so callers control
// when/how the cancel is delivered.
type cancellingProviderOnStream struct {
	onStream func()
}

func (p *cancellingProviderOnStream) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent)
	go func() {
		defer close(ch)
		if p.onStream != nil {
			p.onStream()
		}
		<-ctx.Done()
		ch <- types.StreamEvent{Type: "error", Error: ctx.Err()}
	}()
	return ch, nil
}

// TestLoop_CallerCancel_Cancelled verifies that a plain caller cancel()
// (no cause attached, as would occur via the root cobra signal handler)
// produces RunTrace.Outcome == "cancelled" and done.stop_reason == "cancelled".
//
// The cancel is fired from inside the provider's Stream call rather than
// from a sleep-based timer goroutine, so this test deterministically
// exercises the mid-stream cancel path regardless of CI scheduling jitter.
func TestLoop_CallerCancel_Cancelled(t *testing.T) {
	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Transport = tr

	config := buildTestConfig()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loop.Provider = &cancellingProviderOnStream{
		onStream: func() {
			// Cancel from the provider goroutine so the outer Run has
			// already entered the streaming stage and installed its
			// cancel handler. The plain cancel() leaves context.Cause
			// nil, mirroring SIGINT/SIGTERM delivery via the root
			// signal handler.
			cancel()
		},
	}

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace")
	}
	if runTrace.Outcome != "cancelled" {
		t.Errorf("expected outcome 'cancelled' for caller cancellation, got %q", runTrace.Outcome)
	}

	// H1: also assert the wire "done" event carries the expected stop_reason.
	assertDoneStopReason(t, tr, "cancelled")
}

// TestLoop_CallerCancelWithCause_Error verifies that a caller-side cancel
// with a custom non-nil cause (not control-plane, not deadline) maps to
// outcome="error". This is the code path retained after B1: a non-nil,
// unrecognised cause cannot be attributed to a known cancel/timeout
// reason and is surfaced as an error.
func TestLoop_CallerCancelWithCause_Error(t *testing.T) {
	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Transport = tr

	config := buildTestConfig()

	customCause := errors.New("test sentinel cause")

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	loop.Provider = &cancellingProviderOnStream{
		onStream: func() {
			cancel(customCause)
		},
	}

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace")
	}
	if runTrace.Outcome != "error" {
		t.Errorf("expected outcome 'error' for non-nil unrecognised cause, got %q", runTrace.Outcome)
	}
	assertDoneStopReason(t, tr, "error")
}

func TestLoop_PreExistingCancelledContext_Cancelled(t *testing.T) {
	// When the parent context is cancelled before Run starts, the first
	// inner-loop ctx check trips. With no cause attached (plain cancel()),
	// this now classifies as "cancelled" — matching the user intent of a
	// signal-initiated or caller-initiated cancel.
	tr := &cancellableTransport{}
	loop := buildTestLoop(&mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	})
	loop.Transport = tr

	config := buildTestConfig()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace.Outcome != "cancelled" {
		t.Errorf("expected outcome 'cancelled', got %q", runTrace.Outcome)
	}
	assertDoneStopReason(t, tr, "cancelled")
}

// assertDoneStopReason pulls the single "done" event off the given
// cancellableTransport and checks its StopReason matches want.
func assertDoneStopReason(t *testing.T, tr *cancellableTransport, want string) {
	t.Helper()
	var doneEvents []types.HarnessEvent
	for _, ev := range tr.Events() {
		if ev.Type == "done" {
			doneEvents = append(doneEvents, ev)
		}
	}
	if len(doneEvents) != 1 {
		t.Fatalf("expected exactly one 'done' event, got %d", len(doneEvents))
	}
	if doneEvents[0].StopReason != want {
		t.Errorf("expected done.stop_reason %q, got %q", want, doneEvents[0].StopReason)
	}
}

// cancelOnVerifyVerifier fires a cancel ControlEvent the first time Verify
// is called, then sleeps briefly and returns an error. This reproduces the
// race where a cancel arrives between the inner loop returning and Verify
// completing: the naive implementation would set outcome="verification_error"
// because Verify errored, masking the true cancel cause on the wire.
type cancelOnVerifyVerifier struct {
	tr       *cancellableTransport
	fireOnce sync.Once
}

func (v *cancelOnVerifyVerifier) Verify(ctx context.Context, _ verifier.VerifyContext) (*types.VerificationResult, error) {
	v.fireOnce.Do(func() {
		v.tr.FireControl(types.ControlEvent{Type: "cancel"})
	})
	// Give the cancel handler a beat to set ctx.Err() on the run context
	// before we return. We use a short timer rather than <-ctx.Done() so
	// the test remains deterministic even if the verifier's own ctx is not
	// wired to the run ctx in every implementation.
	select {
	case <-time.After(25 * time.Millisecond):
	case <-ctx.Done():
	}
	return nil, errors.New("verifier failure triggered alongside cancel")
}

// TestLoop_CancelDuringVerify_CancelledWinsOverVerificationError covers B3:
// a cancel ControlEvent arriving while Verify is running must produce
// RunTrace.Outcome == "cancelled" and done.stop_reason == "cancelled", not
// "verification_error". Without the runCtx.Err() override in Run, the
// verifier's error would be recorded as the outcome.
func TestLoop_CancelDuringVerify_CancelledWinsOverVerificationError(t *testing.T) {
	tr := &cancellableTransport{}

	// A provider that returns one complete turn so the inner loop exits
	// with outcome="success" and the outer loop proceeds to Verify.
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "All done."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	loop.Transport = tr
	loop.Verifier = &cancelOnVerifyVerifier{tr: tr}

	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace")
	}
	if runTrace.Outcome != "cancelled" {
		t.Errorf("expected outcome 'cancelled' (cancel wins over verifier error), got %q", runTrace.Outcome)
	}
	assertDoneStopReason(t, tr, "cancelled")
}

func TestClassifyCtxOutcome(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{"control_plane", ErrCancelledByControlPlane, "cancelled"},
		{"wrapped_control_plane", fmt.Errorf("wrap: %w", ErrCancelledByControlPlane), "cancelled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		// context.Canceled (either directly via plain cancel(), or
		// propagated from a parent's WithCancel into our
		// WithCancelCause child) is the signal-handler / caller-side
		// cancel path — user-initiated.
		{"caller_canceled_as_cause", context.Canceled, "cancelled"},
		{"wrapped_caller_canceled", fmt.Errorf("wrap: %w", context.Canceled), "cancelled"},
		// No cause attached — plain cancel() on our own WithCancelCause.
		{"nil", nil, "cancelled"},
		// A non-nil cause that is neither a recognised cancel sentinel
		// nor a deadline is an error.
		{"other", errors.New("something else"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCtxOutcome(tc.cause); got != tc.want {
				t.Errorf("classifyCtxOutcome(%v) = %q, want %q", tc.cause, got, tc.want)
			}
		})
	}
}

func TestErrCancelledByControlPlane_IsMatchable(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", ErrCancelledByControlPlane)
	if !errors.Is(wrapped, ErrCancelledByControlPlane) {
		t.Error("errors.Is should match wrapped ErrCancelledByControlPlane")
	}
}
