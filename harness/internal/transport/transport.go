// Package transport defines the Transport interface for emitting harness
// events and receiving control events from the orchestration layer.
package transport

import "github.com/rubynerd/stirrup/types"

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
