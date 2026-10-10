// Package transport defines the Transport interface for emitting harness
// events and receiving control events from the orchestration layer.
package transport

import "github.com/rxbynerd/stirrup/types"

// Transport is the bidirectional channel between the harness and the
// control plane (or CLI). It emits HarnessEvents outward and receives
// ControlEvents inward.
type Transport interface {
	// Emit sends a harness event to the control plane.
	Emit(event types.HarnessEvent) error

	// OnControl registers a handler that is called for each incoming
	// control event. The handler is called synchronously from the
	// transport's read loop.
	OnControl(handler func(event types.ControlEvent))

	// Close releases transport resources.
	Close() error
}

// Liveness is implemented by transports whose connection to the control
// plane can end independently of the harness.
type Liveness interface {
	// Done is closed once the transport stops receiving control events.
	Done() <-chan struct{}

	// Err reports why the connection ended: nil while it is live and
	// after a local Close or cancellation of the transport's context;
	// otherwise the cause of the loss.
	Err() error
}
