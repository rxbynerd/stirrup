package transport

import "github.com/rxbynerd/stirrup/types"

// NullTransport is a Transport that silently discards all emitted events
// and never produces control events. It is used by sub-agents that should
// not stream to the control plane.
type NullTransport struct{}

// NewNullTransport creates a NullTransport.
func NewNullTransport() *NullTransport {
	return &NullTransport{}
}

// Emit discards the event.
func (t *NullTransport) Emit(_ types.HarnessEvent) error { return nil }

// OnControl is a no-op; NullTransport never receives control events.
func (t *NullTransport) OnControl(_ func(types.ControlEvent)) {}

// Close is a no-op.
func (t *NullTransport) Close() error { return nil }

// IsNullTransport returns true. Subsystems that depend on a live control
// plane (e.g. async tool dispatch) check for this capability via the
// NullCapable interface and fail fast rather than blocking on a transport
// that will never deliver a response.
func (t *NullTransport) IsNullTransport() bool { return true }

// NullCapable is satisfied by transports that report whether they are
// equivalent to a NullTransport for control-event delivery purposes. The
// async tool dispatch path uses this to refuse to start a request that
// would never resolve. Other transports may embed NullTransport (e.g. the
// sub-agent's captureTransport); this interface lets the embedder still be
// detected as null-equivalent.
type NullCapable interface {
	IsNullTransport() bool
}

// IsNull reports whether t is, or wraps, a NullTransport for control-event
// delivery. Returns false for nil so callers do not need a separate guard.
func IsNull(t HasOnControl) bool {
	if t == nil {
		return false
	}
	if nc, ok := t.(NullCapable); ok {
		return nc.IsNullTransport()
	}
	return false
}
