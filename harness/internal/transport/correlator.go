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

	// retained holds entries registered via StartPending — the
	// emit-now / await-or-poll-later path used by detached sub-agent
	// sessions (#71). Unlike pending (one-shot Await), a retained entry
	// is NOT removed on delivery: its payload is buffered until a caller
	// Polls/AwaitIDs it, and only Forget removes it. This lets a response
	// that arrives before any waiter be observed rather than dropped.
	retained map[string]*retainedEntry
}

// retainedEntry buffers the response to a StartPending request. delivered
// and payload are guarded by Correlator.mu; done is closed exactly once
// (under mu) when the response arrives, unblocking any AwaitID waiter.
type retainedEntry struct {
	done      chan struct{}
	delivered bool
	payload   any
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
		retained: make(map[string]*retainedEntry),
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
	// Retained (StartPending) path: record the payload once and signal
	// any AwaitID waiter, but do NOT remove the entry — the buffered
	// payload must survive until Poll/AwaitID reads it or Forget discards
	// it. A given id lives in at most one map (pending and retained draw
	// from the same monotonic counter), so at most one branch fires.
	if entry, rok := c.retained[id]; rok && !entry.delivered {
		entry.delivered = true
		entry.payload = payload
		close(entry.done)
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

// StartPending allocates a fresh request ID, registers a retained entry,
// and invokes emit with that ID — the non-blocking counterpart to Await.
// It returns as soon as emit succeeds, leaving the response to be picked
// up later via Poll (non-blocking) or AwaitID (blocking). A response that
// arrives before any such call is buffered in the entry rather than
// dropped.
//
// On emit failure the entry is removed and the error returned wrapped.
// The caller owns the returned request ID and must eventually Forget it
// to release the buffered state.
//
// This backs the start-and-detach session model (#71): start_session
// calls StartPending and returns the ID to the model as a session handle;
// wait_session/check_session resolve it later.
func (c *Correlator) StartPending(emit func(requestID string) error) (string, error) {
	if emit == nil {
		return "", errors.New("correlator: emit callback is required")
	}

	requestID := c.nextRequestID()
	entry := &retainedEntry{done: make(chan struct{})}

	c.mu.Lock()
	c.retained[requestID] = entry
	c.mu.Unlock()

	if err := emit(requestID); err != nil {
		c.Forget(requestID)
		return "", fmt.Errorf("correlator: emit failed: %w", err)
	}
	return requestID, nil
}

// Poll reports whether the retained request identified by requestID has
// received its response. When ready is true, payload carries the delivered
// value; the value is cached, so repeated Polls and a later AwaitID all
// observe it. ready is false when the request is unknown or still in
// flight. Non-blocking.
func (c *Correlator) Poll(requestID string) (payload any, ready bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.retained[requestID]
	if !ok || !entry.delivered {
		return nil, false
	}
	return entry.payload, true
}

// AwaitID blocks until the retained request identified by requestID
// receives its response, the timeout elapses, or ctx is cancelled.
//
// Unlike Await, the entry is NOT removed on return: the same id may be
// AwaitID'd or Poll'd again and still observe the cached payload until
// Forget is called. A non-positive timeout is replaced with
// DefaultCorrelatorTimeout. Returns an error if requestID is unknown
// (never registered via StartPending, or already Forgotten).
func (c *Correlator) AwaitID(ctx context.Context, requestID string, timeout time.Duration) (any, error) {
	if timeout <= 0 {
		timeout = DefaultCorrelatorTimeout
	}

	c.mu.Lock()
	entry, ok := c.retained[requestID]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("correlator: unknown request id %q", requestID)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-entry.done:
		return c.retainedPayload(entry), nil
	case <-timer.C:
		return nil, fmt.Errorf("correlator: timed out after %s waiting for response (requestId=%s)", timeout, requestID)
	case <-ctx.Done():
		return nil, fmt.Errorf("correlator: cancelled (requestId=%s): %w", requestID, ctx.Err())
	}
}

// retainedPayload reads an entry's delivered payload under the lock. Only
// called after observing entry.done closed, so delivered is guaranteed
// true; the lock is taken purely for the memory barrier on payload.
func (c *Correlator) retainedPayload(entry *retainedEntry) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return entry.payload
}

// Forget removes a retained entry and any buffered payload. Idempotent:
// safe to call when the entry is already absent. Use it to release a
// session's state once its result has been consumed, or to abandon a
// detached session that the agent has chosen to forget.
func (c *Correlator) Forget(requestID string) {
	c.mu.Lock()
	delete(c.retained, requestID)
	c.mu.Unlock()
}

// RetainedCount returns the number of live retained entries. Intended for
// tests and diagnostics (e.g. enforcing a live-session cap).
func (c *Correlator) RetainedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.retained)
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
