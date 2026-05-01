package egressproxy

// SecurityEventEmitter is the minimal interface the egress proxy needs to
// emit egress_allowed / egress_blocked audit events. It intentionally
// does not import the security package so the executor tree can stay
// free of cycles. *security.SecurityLogger satisfies this interface
// implicitly via its Emit method.
type SecurityEventEmitter interface {
	Emit(level, event string, data map[string]any)
}
