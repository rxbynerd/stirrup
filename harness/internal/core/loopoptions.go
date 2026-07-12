package core

// LoopOptions carries CLI-only, build-tag-gated debug behaviour into
// BuildLoop / BuildLoopWithTransport (issues #219, #220). It has no
// corresponding RunConfig field — --debug and --trace-wire are CLI flags
// only, never serialisable into a RunConfig, so a control-plane submission
// can never request either behaviour.
//
// Every bit here is meaningless in a release binary: each is re-checked
// against debugbuild.DebugBuildEnabled() at its point of effect
// (buildTraceEmitter for DebugRedactionDisabled, the provider-adapter
// wiring loop for WireTrace) rather than trusted as-is, so a release
// build stays physically incapable of disabling redaction or dumping
// unredacted wire traffic even if a future caller sets these without
// going through the CLI's own debugbuild gate. See
// docs/security.md#debug-builds.
type LoopOptions struct {
	debugRedactionDisabled bool
	wireTrace              bool
}

// LoopOption configures a LoopOptions value. Follows the functional-
// options shape so BuildLoop/BuildLoopWithTransport's signature stays
// stable as debug-only knobs are added.
type LoopOption func(*LoopOptions)

// WithDebugRedactionDisabled disables RunConfig.Redact() and the trace
// emitter's security.Scrub content-scrub chain at trace-emitter
// construction (--debug, issue #219). No-op in a release binary.
func WithDebugRedactionDisabled(disabled bool) LoopOption {
	return func(o *LoopOptions) { o.debugRedactionDisabled = disabled }
}

// WithWireTrace installs a wire-tap RoundTripper on provider HTTP
// clients that dumps raw, unredacted request/response traffic to stderr
// (--trace-wire, issue #220). No-op in a release binary.
func WithWireTrace(enabled bool) LoopOption {
	return func(o *LoopOptions) { o.wireTrace = enabled }
}
