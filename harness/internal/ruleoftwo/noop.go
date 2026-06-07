package ruleoftwo

import "context"

// Noop is the default Monitor: it never trips and never enforces. The
// agentic loop unconditionally calls through the configured monitor, so
// this type is what the factory wires in when the arming matrix leaves
// the run unarmed, avoiding a nil-check on every call site.
type Noop struct{}

var _ Monitor = Noop{}

// NewNoop returns a Monitor that observes nothing and never latches.
func NewNoop() Monitor {
	return Noop{}
}

// ObserveChunks never detects.
func (Noop) ObserveChunks(_ context.Context, _ string, _ int, _ []string) Detection {
	return Detection{}
}

// TripFromGuard never latches.
func (Noop) TripFromGuard(_, _ string) bool { return false }

// Tripped is always false.
func (Noop) Tripped() bool { return false }

// Enforcing is always false.
func (Noop) Enforcing() bool { return false }

// Action mirrors the not-enforcing contract of Monitor.Action.
func (Noop) Action() string { return "warn" }

// Redact returns the content unchanged.
func (Noop) Redact(content string) (string, int) { return content, 0 }
