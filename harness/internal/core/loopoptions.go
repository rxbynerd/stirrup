package core

// LoopOptions carries CLI-only, build-tag-gated debug behaviour into
// BuildLoop. Deliberately has no RunConfig counterpart, so a control-plane
// submission can never request either behaviour. Every bit is re-checked
// against debugbuild.DebugBuildEnabled() at its point of effect rather than
// trusted here, keeping a release build incapable of disabling redaction
// even if a caller bypasses the CLI's gate. See
// docs/security.md#debug-builds.
type LoopOptions struct {
	debugRedactionDisabled bool
	wireTrace              bool
}

// LoopOption configures a LoopOptions value, keeping BuildLoop's signature
// stable as debug-only knobs are added.
type LoopOption func(*LoopOptions)

// WithDebugRedactionDisabled disables RunConfig.Redact() and the trace
// emitter's scrub chain (--debug). No-op in a release binary.
func WithDebugRedactionDisabled(disabled bool) LoopOption {
	return func(o *LoopOptions) { o.debugRedactionDisabled = disabled }
}

// WithWireTrace dumps raw, unredacted provider traffic to stderr
// (--trace-wire). No-op in a release binary.
func WithWireTrace(enabled bool) LoopOption {
	return func(o *LoopOptions) { o.wireTrace = enabled }
}
