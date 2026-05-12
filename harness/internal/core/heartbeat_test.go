package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// recordingTransport captures emitted events for test assertions.
type recordingTransport struct {
	mu     sync.Mutex
	events []types.HarnessEvent
}

func (t *recordingTransport) Emit(event types.HarnessEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
	return nil
}

func (t *recordingTransport) OnControl(_ func(types.ControlEvent)) {}
func (t *recordingTransport) Close() error                         { return nil }

func (t *recordingTransport) heartbeatCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, e := range t.events {
		if e.Type == "heartbeat" {
			count++
		}
	}
	return count
}

func TestStartHeartbeat_EmitsEvents(t *testing.T) {
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	ctx := context.Background()
	stop := loop.startHeartbeat(ctx, 10*time.Millisecond)

	// Wait long enough for several heartbeats to fire.
	time.Sleep(60 * time.Millisecond)
	stop()

	count := rec.heartbeatCount()
	if count < 2 {
		t.Errorf("expected at least 2 heartbeat events, got %d", count)
	}
}

func TestStartHeartbeat_StopsOnCancel(t *testing.T) {
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	ctx := context.Background()
	stop := loop.startHeartbeat(ctx, 10*time.Millisecond)

	time.Sleep(30 * time.Millisecond)
	stop()

	countAtStop := rec.heartbeatCount()

	// Wait a bit more and verify no additional heartbeats arrived. The
	// production guarantee is "stops eventually": a ticker tick that has
	// already been selected before ctx.Done is observed may still fire once
	// after stop(). Tolerate at most one extra heartbeat post-cancel.
	time.Sleep(30 * time.Millisecond)
	countAfter := rec.heartbeatCount()

	if countAfter-countAtStop > 1 {
		t.Errorf("heartbeat continued after cancel beyond tolerance: %d at stop, %d after (max +1 allowed)", countAtStop, countAfter)
	}
}

func TestStartHeartbeat_RespectsContextCancellation(t *testing.T) {
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	ctx, cancel := context.WithCancel(context.Background())
	_ = loop.startHeartbeat(ctx, 10*time.Millisecond)

	time.Sleep(30 * time.Millisecond)
	cancel()

	countAtCancel := rec.heartbeatCount()

	// Wait a bit more and verify no additional heartbeats arrived beyond
	// tolerance. The production guarantee is "stops eventually": a ticker
	// tick that has already been selected before ctx.Done is observed may
	// still fire once after cancel(). Tolerate at most one extra heartbeat.
	time.Sleep(30 * time.Millisecond)
	countAfter := rec.heartbeatCount()

	if countAfter-countAtCancel > 1 {
		t.Errorf("heartbeat continued after context cancel beyond tolerance: %d at cancel, %d after (max +1 allowed)", countAtCancel, countAfter)
	}
}

func TestStartHeartbeat_CancelBeforeFirstTick(t *testing.T) {
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	// Cancel the context before startHeartbeat is invoked so the goroutine's
	// first action is to observe an already-Done context. This exercises the
	// cold path of the non-blocking pre-check added in ca80ca7: the outer
	// select's `ctx.Done` arm should fire on the very first iteration, before
	// any ticker tick is consumed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = loop.startHeartbeat(ctx, 10*time.Millisecond)

	// Sleep three ticker intervals: ample time for the goroutine to have
	// either taken the pre-check exit or, in the worst case, entered the
	// blocking select before noticing cancellation.
	time.Sleep(30 * time.Millisecond)

	count := rec.heartbeatCount()
	if count > 1 {
		t.Errorf("expected at most 1 heartbeat on cold-path cancel-before-first-tick (pre-check should usually catch ctx.Done with count=0; tolerate 1 to avoid flakiness when the goroutine entered the blocking select before observing cancellation), got %d", count)
	}
}
