package transport

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// fakeOnControl is a minimal HasOnControl for tests. simulate() fans events
// out to all registered handlers.
type fakeOnControl struct {
	mu       sync.Mutex
	handlers []func(types.ControlEvent)
}

func (f *fakeOnControl) OnControl(handler func(types.ControlEvent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, handler)
}

func (f *fakeOnControl) simulate(event types.ControlEvent) {
	f.mu.Lock()
	hs := make([]func(types.ControlEvent), len(f.handlers))
	copy(hs, f.handlers)
	f.mu.Unlock()
	for _, h := range hs {
		h(event)
	}
}

// extractByType returns an extractor that resolves events whose Type matches
// wantType, surfacing the event itself as the payload.
func extractByType(wantType string) PayloadExtractor {
	return func(event types.ControlEvent) (string, any) {
		if event.Type != wantType {
			return "", nil
		}
		return event.RequestID, event
	}
}

func TestCorrelator_Await_ReturnsMatchingResponse(t *testing.T) {
	t.Parallel()
	fc := &fakeOnControl{}
	c := NewCorrelator("perm")
	c.AttachTo(fc, extractByType("permission_response"))

	allowed := true
	go func() {
		// Wait briefly for Await to register the pending entry.
		for c.PendingCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		fc.simulate(types.ControlEvent{
			Type:      "permission_response",
			RequestID: "perm-1",
			Allowed:   &allowed,
		})
	}()

	// Await calls emit synchronously on this goroutine, so require is safe inside it.
	got, err := c.Await(context.Background(), time.Second, func(id string) error {
		require.Equal(t, "perm-1", id)
		return nil
	})
	require.NoError(t, err)
	require.IsType(t, types.ControlEvent{}, got)
	ev := got.(types.ControlEvent)
	require.NotNil(t, ev.Allowed)
	require.True(t, *ev.Allowed)
	require.Zero(t, c.PendingCount())
}

func TestCorrelator_Await_TimesOutWithoutResponse(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")
	// No attach: no extractor will ever resolve this.

	_, err := c.Await(context.Background(), 25*time.Millisecond, func(_ string) error {
		return nil
	})
	require.ErrorContains(t, err, "timed out")
	require.Zero(t, c.PendingCount())
}

func TestCorrelator_Await_CancelledContextReturnsCanceled(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for c.PendingCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	_, err := c.Await(ctx, time.Second, func(_ string) error {
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, c.PendingCount())
}

func TestCorrelator_Await_EmitFailureClearsPending(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")

	emitErr := errors.New("transport broken")
	_, err := c.Await(context.Background(), time.Second, func(_ string) error {
		return emitErr
	})
	require.ErrorIs(t, err, emitErr)
	require.Zero(t, c.PendingCount())
}

func TestCorrelator_Await_ResolvesOutOfOrder(t *testing.T) {
	t.Parallel()
	fc := &fakeOnControl{}
	c := NewCorrelator("req")
	c.AttachTo(fc, extractByType("response"))

	type result struct {
		id      string
		payload any
		err     error
	}
	results := make(chan result, 2)

	// Two concurrent awaits. Each goroutine reports back the request ID it
	// was assigned alongside its eventual payload, so the test does not
	// depend on goroutine scheduling order.
	for i := 0; i < 2; i++ {
		go func() {
			var assignedID string
			payload, err := c.Await(context.Background(), 2*time.Second, func(id string) error {
				assignedID = id
				return nil
			})
			results <- result{id: assignedID, payload: payload, err: err}
		}()
	}

	// Wait for both Awaits to register pending entries.
	require.Eventually(t, func() bool { return c.PendingCount() >= 2 }, time.Second, time.Millisecond)
	require.Equal(t, 2, c.PendingCount())

	// Resolve "req-2" first, then "req-1". Both must surface the right
	// payload regardless of which goroutine got which id.
	fc.simulate(types.ControlEvent{Type: "response", RequestID: "req-2", Reason: "second"})
	fc.simulate(types.ControlEvent{Type: "response", RequestID: "req-1", Reason: "first"})

	wantReasonByID := map[string]string{
		"req-1": "first",
		"req-2": "second",
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			require.NoError(t, r.err, "await id=%s", r.id)
			require.IsType(t, types.ControlEvent{}, r.payload, "await id=%s", r.id)
			ev := r.payload.(types.ControlEvent)
			require.Contains(t, wantReasonByID, r.id)
			require.False(t, seen[r.id], "duplicate result for id %q", r.id)
			seen[r.id] = true
			require.Equal(t, wantReasonByID[r.id], ev.Reason, "await id=%s", r.id)
		case <-time.After(2 * time.Second):
			require.Fail(t, "timed out waiting for resolution", "await %d", i)
		}
	}

	require.Zero(t, c.PendingCount())
}

func TestCorrelator_Await_LateResponseAfterTimeoutIsIgnored(t *testing.T) {
	t.Parallel()
	fc := &fakeOnControl{}
	c := NewCorrelator("perm")
	c.AttachTo(fc, extractByType("permission_response"))

	goroutinesBefore := runtime.NumGoroutine()

	_, err := c.Await(context.Background(), 25*time.Millisecond, func(_ string) error {
		return nil
	})
	require.Error(t, err)

	// Now deliver a late response with the (now stale) id. Must not panic.
	// Because the Correlator increments monotonically, the id used was
	// "perm-1".
	fc.simulate(types.ControlEvent{
		Type:      "permission_response",
		RequestID: "perm-1",
	})

	require.Zero(t, c.PendingCount())

	// Allow the runtime a moment to reap the goroutine that ran Await.
	time.Sleep(20 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	// Tolerance for unrelated runtime goroutines, but we should not be
	// strictly higher than before by more than a small slack.
	require.LessOrEqual(t, goroutinesAfter, goroutinesBefore+2, "possible goroutine leak")
}

func TestCorrelator_IgnoresUnrelatedEvents(t *testing.T) {
	t.Parallel()
	fc := &fakeOnControl{}
	c := NewCorrelator("perm")
	c.AttachTo(fc, extractByType("permission_response"))

	go func() {
		for c.PendingCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		// Unrelated event type: ignored.
		fc.simulate(types.ControlEvent{Type: "user_response", RequestID: "perm-1"})
		// Wrong request id: ignored.
		fc.simulate(types.ControlEvent{Type: "permission_response", RequestID: "perm-999"})
		// Finally, the matching event.
		allowed := true
		fc.simulate(types.ControlEvent{
			Type:      "permission_response",
			RequestID: "perm-1",
			Allowed:   &allowed,
		})
	}()

	got, err := c.Await(context.Background(), time.Second, func(_ string) error {
		return nil
	})
	require.NoError(t, err)
	require.IsType(t, types.ControlEvent{}, got)
	ev := got.(types.ControlEvent)
	require.NotNil(t, ev.Allowed, "expected matching allowed event, got %+v", ev)
	require.True(t, *ev.Allowed, "expected matching allowed event, got %+v", ev)
}

func TestCorrelator_AssignsSequentialRequestIDs(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")

	ids := make([]string, 3)
	for i := range ids {
		_, _ = c.Await(context.Background(), 5*time.Millisecond, func(id string) error {
			ids[i] = id
			return errors.New("stop") // exit fast via emit error
		})
	}

	require.Equal(t, []string{"perm-1", "perm-2", "perm-3"}, ids)
}

func TestCorrelator_EmptyPrefixDefaultsToReq(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("")

	var got string
	_, _ = c.Await(context.Background(), 5*time.Millisecond, func(id string) error {
		got = id
		return errors.New("stop")
	})
	require.Equal(t, "req-1", got)
}

func TestCorrelator_Await_NilEmitReturnsError(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")
	_, err := c.Await(context.Background(), time.Second, nil)
	require.Error(t, err)
}

// TestCorrelator_Await_ZeroTimeoutUsesDefault verifies the per-await timeout falls
// back to DefaultCorrelatorTimeout when the caller passes zero.
func TestCorrelator_Await_ZeroTimeoutUsesDefault(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")

	// We cannot wait the full default; instead cancel via ctx and assert
	// that the call did not return immediately because of a zero-timer.
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := c.Await(ctx, 0, func(_ string) error { return nil })
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, elapsed, 15*time.Millisecond, "zero timeout may have fired immediately")
}

// Compile-time interface satisfaction check: Transport implementations all
// satisfy HasOnControl. This guards against drift if Transport adds methods
// that aren't in HasOnControl.
var (
	_ HasOnControl = (Transport)(nil)
	_ HasOnControl = (*NullTransport)(nil)
)

// TestCorrelator_NewIsEmpty verifies NewCorrelator returns a non-nil
// instance with PendingCount starting at zero.
func TestCorrelator_NewIsEmpty(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("x")
	require.NotNil(t, c)
	require.Zero(t, c.PendingCount())
}
