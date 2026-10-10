package core

import "time"

// LoopOptions carries entrypoint-supplied behaviour into BuildLoop. The debug
// bits are CLI-only and build-tag-gated; they deliberately have no RunConfig
// counterpart, so a control-plane submission can never request them. Every bit is re-checked
// against debugbuild.DebugBuildEnabled() at its point of effect rather than
// trusted here, keeping a release build incapable of disabling redaction
// even if a caller bypasses the CLI's gate. See
// docs/security.md#debug-builds.
type LoopOptions struct {
	debugRedactionDisabled bool
	wireTrace              bool
	sessionBudget          time.Duration
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

// WithSessionBudget sets the longest wall-clock span, from loop construction,
// that the sandbox must stay usable: the primary run plus any follow-up runs
// and the idle windows between them. The sandbox's own lifetime bound is this
// span plus executor.SandboxGCMargin, so it only fires after an unclean exit.
// A budget shorter than RunConfig.Timeout is raised to it.
func WithSessionBudget(d time.Duration) LoopOption {
	return func(o *LoopOptions) { o.sessionBudget = d }
}
