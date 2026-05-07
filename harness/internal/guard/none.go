package guard

import "context"

// Noop is the default GuardRail: it allows everything. The agentic
// loop unconditionally calls through to the configured guard so this
// type is what we wire in when no guard is configured, avoiding a
// nil-check on every call site.
type Noop struct{}

// NewNoop returns a GuardRail that always allows content through. The
// returned Decision has GuardID "none" so traces can distinguish a
// no-op allow from an adapter's allow.
func NewNoop() GuardRail {
	return Noop{}
}

// Check always allows the content.
func (Noop) Check(_ context.Context, _ Input) (*Decision, error) {
	return &Decision{Verdict: VerdictAllow, GuardID: "none"}, nil
}
