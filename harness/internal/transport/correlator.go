package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// DefaultCorrelatorTimeout is the default per-await timeout used when a
// caller passes a non-positive timeout to Correlator.Await.
const DefaultCorrelatorTimeout = 60 * time.Second

// HasOnControl is the minimal Transport surface the correlator needs. It is
// declared in this package so the helper can be reused without a circular
// import via consumers (e.g. permission, tool dispatch) that define the same
// shape locally.
type HasOnControl interface {
	OnControl(handler func(event types.ControlEvent))
}

// PayloadExtractor inspects a control event and returns the request ID it
// resolves and the payload to deliver to the awaiting goroutine. If the
// event is unrelated (different type, or otherwise should be ignored),
// the returned id should be empty.
type PayloadExtractor func(event types.ControlEvent) (id string, payload any)

// Correlator pairs outbound HarnessEvents with their inbound ControlEvent
// responses by request ID. It encapsulates the
// "emit + register pending channel + select on response/timer/ctx + cleanup"
// pattern so multiple subsystems (permission gating, async tool dispatch)
// can share one implementation.
//
// A single Correlator may be attached to at most one event type per
// AttachTo call but may be attached multiple times to different types
// on the same transport. Concurrent calls to Await are safe; each call
// receives a unique request ID and its own response channel.
type Correlator struct {
	idPrefix string

	mu      sync.Mutex
	nextID  int
	pending map[string]chan any
}

// NewCorrelator constructs a Correlator. The idPrefix is used to format
// request IDs as "<prefix>-<n>" with a monotonic counter starting at 1.
// If idPrefix is empty, "req" is used.
func NewCorrelator(idPrefix string) *Correlator {
	if idPrefix == "" {
		idPrefix = "req"
	}
	return &Correlator{
		idPrefix: idPrefix,
		pending:  make(map[string]chan any),
	}
}

// AttachTo registers a control handler on the transport that routes
// matching events to their pending Await calls. The extract function
// inspects each incoming event and returns the request ID it resolves
// (empty to ignore the event) along with the payload to deliver.
//
// AttachTo may be called multiple times with different extract functions
// (typically one per response event type). Each registration adds a new
// handler; none unregister.
func (c *Correlator) AttachTo(t HasOnControl, extract PayloadExtractor) {
	if t == nil || extract == nil {
		return
	}
	t.OnControl(func(event types.ControlEvent) {
		id, payload := extract(event)
		if id == "" {
			return
		}
		c.deliver(id, payload)
	})
}

// deliver routes a payload to the pending channel for the given request
// ID, if one is registered. The channel is buffered to capacity 1 so a
// late delivery (after the awaiter has timed out and removed the entry)
// would never reach this code path; if the entry is missing the delivery
// is silently dropped.
func (c *Correlator) deliver(id string, payload any) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if ok {
		// Channel has capacity 1 and this is the only sender for this
		// id (we just removed it from pending under the lock), so the
		// send cannot block.
		ch <- payload
	}
}

// Await registers a fresh request ID, invokes emit with that ID, and
// blocks until either:
//
//   - a correlated payload is delivered (returns payload, nil)
//   - timeout elapses (returns nil, error containing "timed out")
//   - ctx is cancelled (returns nil, error wrapping ctx.Err())
//   - emit returns an error (returns nil, that error wrapped)
//
// The pending entry is removed on every exit path. The emit callback
// receives the request ID it should attach to its outbound event; this
// keeps the wire format under the caller's control.
//
// A non-positive timeout is replaced with DefaultCorrelatorTimeout.
func (c *Correlator) Await(
	ctx context.Context,
	timeout time.Duration,
	emit func(requestID string) error,
) (any, error) {
	if emit == nil {
		return nil, errors.New("correlator: emit callback is required")
	}
	if timeout <= 0 {
		timeout = DefaultCorrelatorTimeout
	}

	requestID := c.nextRequestID()
	ch := make(chan any, 1)

	c.mu.Lock()
	c.pending[requestID] = ch
	c.mu.Unlock()

	if err := emit(requestID); err != nil {
		c.cancel(requestID)
		return nil, fmt.Errorf("correlator: emit failed: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case payload := <-ch:
		// deliver() already removed the pending entry under lock.
		return payload, nil
	case <-timer.C:
		c.cancel(requestID)
		return nil, fmt.Errorf("correlator: timed out after %s waiting for response (requestId=%s)", timeout, requestID)
	case <-ctx.Done():
		c.cancel(requestID)
		return nil, fmt.Errorf("correlator: cancelled (requestId=%s): %w", requestID, ctx.Err())
	}
}

// cancel removes a pending entry. Safe to call when the entry has already
// been removed (e.g. because deliver() resolved it concurrently with a
// timeout firing).
func (c *Correlator) cancel(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	c.mu.Unlock()
}

// PendingCount returns the number of in-flight Awaits. Intended for tests
// and diagnostics.
func (c *Correlator) PendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func (c *Correlator) nextRequestID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return fmt.Sprintf("%s-%d", c.idPrefix, c.nextID)
}
