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
