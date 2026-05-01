package transport

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestCorrelator_HappyPath(t *testing.T) {
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

	got, err := c.Await(context.Background(), time.Second, func(id string) error {
		if id != "perm-1" {
			t.Errorf("expected first id 'perm-1', got %q", id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ev, ok := got.(types.ControlEvent)
	if !ok {
		t.Fatalf("expected ControlEvent payload, got %T", got)
	}
	if ev.Allowed == nil || !*ev.Allowed {
		t.Errorf("expected allowed=true, got %v", ev.Allowed)
	}
	if c.PendingCount() != 0 {
		t.Errorf("expected pending=0 after happy path, got %d", c.PendingCount())
	}
}

func TestCorrelator_Timeout(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")
	// No attach: no extractor will ever resolve this.

	_, err := c.Await(context.Background(), 25*time.Millisecond, func(_ string) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected 'timed out' in error, got: %v", err)
	}
	if c.PendingCount() != 0 {
		t.Errorf("expected pending=0 after timeout, got %d", c.PendingCount())
	}
}

func TestCorrelator_ContextCancellation(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in chain, got: %v", err)
	}
	if c.PendingCount() != 0 {
		t.Errorf("expected pending=0 after cancellation, got %d", c.PendingCount())
	}
}

func TestCorrelator_EmitFailureCleansUp(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")

	emitErr := errors.New("transport broken")
	_, err := c.Await(context.Background(), time.Second, func(_ string) error {
		return emitErr
	})
	if err == nil {
		t.Fatal("expected emit error")
	}
	if !errors.Is(err, emitErr) {
		t.Errorf("expected emit error in chain, got: %v", err)
	}
	if c.PendingCount() != 0 {
		t.Errorf("expected pending=0 after emit failure, got %d", c.PendingCount())
	}
}

func TestCorrelator_OutOfOrderResolution(t *testing.T) {
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
	deadline := time.Now().Add(time.Second)
	for c.PendingCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.PendingCount() != 2 {
		t.Fatalf("expected 2 pending, got %d", c.PendingCount())
	}

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
			if r.err != nil {
				t.Errorf("await id=%s: unexpected error: %v", r.id, r.err)
				continue
			}
			ev, ok := r.payload.(types.ControlEvent)
			if !ok {
				t.Errorf("await id=%s: expected ControlEvent payload, got %T", r.id, r.payload)
				continue
			}
			want, known := wantReasonByID[r.id]
			if !known {
				t.Errorf("unexpected request id %q", r.id)
				continue
			}
			if seen[r.id] {
				t.Errorf("duplicate result for id %q", r.id)
			}
			seen[r.id] = true
			if ev.Reason != want {
				t.Errorf("await id=%s: got reason %q, want %q", r.id, ev.Reason, want)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("await %d: timed out waiting for resolution", i)
		}
	}

	if c.PendingCount() != 0 {
		t.Errorf("expected pending=0 after both resolved, got %d", c.PendingCount())
	}
}

func TestCorrelator_LateResponseDoesNotPanic(t *testing.T) {
	t.Parallel()
	fc := &fakeOnControl{}
	c := NewCorrelator("perm")
	c.AttachTo(fc, extractByType("permission_response"))

	goroutinesBefore := runtime.NumGoroutine()

	_, err := c.Await(context.Background(), 25*time.Millisecond, func(_ string) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}

	// Now deliver a late response with the (now stale) id. Must not panic.
	// Because the Correlator increments monotonically, the id used was
	// "perm-1".
	fc.simulate(types.ControlEvent{
		Type:      "permission_response",
		RequestID: "perm-1",
	})

	if c.PendingCount() != 0 {
		t.Errorf("expected pending=0 after late response, got %d", c.PendingCount())
	}

	// Allow the runtime a moment to reap the goroutine that ran Await.
	time.Sleep(20 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	// Tolerance for unrelated runtime goroutines, but we should not be
	// strictly higher than before by more than a small slack.
	if goroutinesAfter > goroutinesBefore+2 {
		t.Errorf("possible goroutine leak: before=%d after=%d", goroutinesBefore, goroutinesAfter)
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ev := got.(types.ControlEvent)
	if ev.Allowed == nil || !*ev.Allowed {
		t.Errorf("expected matching allowed event, got %+v", ev)
	}
}

func TestCorrelator_RequestIDFormat(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")

	ids := make([]string, 3)
	for i := range ids {
		_, _ = c.Await(context.Background(), 5*time.Millisecond, func(id string) error {
			ids[i] = id
			return errors.New("stop") // exit fast via emit error
		})
	}

	want := []string{"perm-1", "perm-2", "perm-3"}
	for i, w := range want {
		if ids[i] != w {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], w)
		}
	}
}

func TestCorrelator_DefaultPrefix(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("")

	var got string
	_, _ = c.Await(context.Background(), 5*time.Millisecond, func(id string) error {
		got = id
		return errors.New("stop")
	})
	if got != "req-1" {
		t.Errorf("expected 'req-1' default-prefixed id, got %q", got)
	}
}

func TestCorrelator_NilEmit(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("perm")
	_, err := c.Await(context.Background(), time.Second, nil)
	if err == nil {
		t.Fatal("expected error for nil emit")
	}
}

// TestCorrelator_DefaultTimeoutWhenZero verifies the per-await timeout falls
// back to DefaultCorrelatorTimeout when the caller passes zero.
func TestCorrelator_DefaultTimeoutWhenZero(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if elapsed < 15*time.Millisecond {
		t.Errorf("returned too quickly (%s) — zero timeout may have fired immediately", elapsed)
	}
}

// Compile-time interface satisfaction check: Transport implementations all
// satisfy HasOnControl. This guards against drift if Transport adds methods
// that aren't in HasOnControl.
var (
	_ HasOnControl = (Transport)(nil)
	_ HasOnControl = (*NullTransport)(nil)
)

// Sanity: NewCorrelator returns a non-nil instance and PendingCount starts
// at zero. (Belt-and-braces; covered indirectly above.)
func TestCorrelator_NewIsEmpty(t *testing.T) {
	t.Parallel()
	c := NewCorrelator("x")
	if c == nil {
		t.Fatal("NewCorrelator returned nil")
	}
	if c.PendingCount() != 0 {
		t.Errorf("expected empty correlator, got pending=%d", c.PendingCount())
	}
	// Avoid unused-import lint on fmt by referencing it in a tiny check.
	_ = fmt.Sprintf("ok-%d", c.PendingCount())
}
