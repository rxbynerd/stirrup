package core

import (
	"context"
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
// and a provider that always returns a simple successful response. The
// returned transport handle lets the caller inject control events.
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
	RunFollowUpLoop(context.Background(), loop, config, 0)
	elapsed := time.Since(start)

	// With graceSecs == 0 the timer fires immediately. The function should
	// return in well under a second.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected immediate return for zero grace period, took %v", elapsed)
	}
}

func TestRunFollowUpLoop_FollowUpRequestArrives(t *testing.T) {
	loop, tr := buildFollowUpTestLoop(t)
	config := buildTestConfig()

	// Use a cancellable context so we can exit after verifying the follow-up
	// was processed (otherwise the reset grace timer would block the test).
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Long grace period — the test exits via context cancellation, not
		// the timer. This proves the follow-up path was taken.
		RunFollowUpLoop(ctx, loop, config, 30)
	}()

	// Give OnControl registration a moment to take effect.
	time.Sleep(50 * time.Millisecond)

	// Fire a follow-up control event with a new prompt.
	tr.FireControl(types.ControlEvent{
		Type:         "user_response",
		UserResponse: "Please also add tests.",
	})

	// Wait briefly for the inner loop to complete and config to be mutated,
	// then cancel so RunFollowUpLoop exits the reset grace window.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// RunFollowUpLoop returned. Verify the config was updated with the
		// follow-up prompt (RunFollowUpLoop mutates config.Prompt).
		if config.Prompt != "Please also add tests." {
			t.Errorf("expected config.Prompt to be updated to follow-up prompt, got %q", config.Prompt)
		}
		// RunID should have been refreshed.
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
	RunFollowUpLoop(context.Background(), loop, config, 1) // 1-second grace
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
		RunFollowUpLoop(ctx, loop, config, 30) // long grace — should not be reached
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

