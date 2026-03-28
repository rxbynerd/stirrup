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
func (t *recordingTransport) Close() error                        { return nil }

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

	// Wait a bit more and verify no additional heartbeats arrived.
	time.Sleep(30 * time.Millisecond)
	countAfter := rec.heartbeatCount()

	if countAfter != countAtStop {
		t.Errorf("heartbeat continued after cancel: %d at stop, %d after", countAtStop, countAfter)
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

	time.Sleep(30 * time.Millisecond)
	countAfter := rec.heartbeatCount()

	if countAfter != countAtCancel {
		t.Errorf("heartbeat continued after context cancel: %d at cancel, %d after", countAtCancel, countAfter)
	}
}
