package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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

func TestLoop_CallerCancel_Error(t *testing.T) {
	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Provider = &slowProvider{}
	loop.Transport = tr

	config := buildTestConfig()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the run starts so that the signal maps to
	// context.Canceled (not DeadlineExceeded).
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace")
	}
	if runTrace.Outcome != "error" {
		t.Errorf("expected outcome 'error' for caller cancellation, got %q", runTrace.Outcome)
	}
}

func TestLoop_PreExistingCancelledContext_Error(t *testing.T) {
	// When the parent context is cancelled before Run starts, the first
	// inner-loop ctx check trips and we should classify as "error" (signal).
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
	if runTrace.Outcome != "error" {
		t.Errorf("expected outcome 'error', got %q", runTrace.Outcome)
	}
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
		{"caller", context.Canceled, "error"},
		{"other", errors.New("something else"), "error"},
		{"nil", nil, "error"},
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
