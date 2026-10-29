package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestStartHeartbeat_EmitsHeartbeatEvents(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	stop := loop.startHeartbeat(context.Background(), 10*time.Millisecond)

	// Wait long enough for several heartbeats to fire.
	time.Sleep(60 * time.Millisecond)
	stop()

	must.GreaterOrEqual(rec.heartbeatCount(), 2)
}

func TestStartHeartbeat_StopFunctionHaltsHeartbeats(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	stop := loop.startHeartbeat(context.Background(), 10*time.Millisecond)

	time.Sleep(30 * time.Millisecond)
	stop()

	countAtStop := rec.heartbeatCount()

	// The production guarantee is "stops eventually": a ticker tick that has
	// already been selected before the stop signal is observed may still fire
	// once after stop(). Tolerate at most one extra heartbeat.
	time.Sleep(30 * time.Millisecond)
	countAfter := rec.heartbeatCount()

	must.LessOrEqual(countAfter-countAtStop, 1,
		"heartbeat continued after stop beyond tolerance: %d at stop, %d after", countAtStop, countAfter)
}

func TestStartHeartbeat_RespectsContextCancellation(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	ctx, cancel := context.WithCancel(context.Background())
	_ = loop.startHeartbeat(ctx, 10*time.Millisecond)

	time.Sleep(30 * time.Millisecond)
	cancel()

	countAtCancel := rec.heartbeatCount()

	// The production guarantee is "stops eventually": a ticker tick that has
	// already been selected before ctx.Done is observed may still fire once
	// after cancel(). Tolerate at most one extra heartbeat.
	time.Sleep(30 * time.Millisecond)
	countAfter := rec.heartbeatCount()

	must.LessOrEqual(countAfter-countAtCancel, 1,
		"heartbeat continued after context cancel beyond tolerance: %d at cancel, %d after", countAtCancel, countAfter)
}

func TestStartHeartbeat_CancelBeforeFirstTick(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Transport: rec,
	}

	// Cancel before startHeartbeat runs, so the goroutine starts with an
	// already-done context and should exit before the first tick.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = loop.startHeartbeat(ctx, 10*time.Millisecond)

	// Three ticker intervals is enough for the goroutine to exit. One heartbeat
	// is tolerated because the goroutine may reach the blocking select before it
	// observes the cancellation.
	time.Sleep(30 * time.Millisecond)

	must.LessOrEqual(rec.heartbeatCount(), 1,
		"expected at most 1 heartbeat when the context is cancelled before the first tick")
}

func TestHeartbeatInterval_ZeroSelectsDefault(t *testing.T) {
	check := assert.New(t)
	check.Equal(time.Duration(types.DefaultHeartbeatSeconds)*time.Second, (&AgenticLoop{}).heartbeatInterval(), "zero")
	check.Equal(5*time.Second, (&AgenticLoop{HeartbeatInterval: 5 * time.Second}).heartbeatInterval(), "configured")
}
