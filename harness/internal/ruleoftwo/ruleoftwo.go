// Package ruleoftwo defines the Monitor interface and supporting types
// for the harness's Rule-of-Two runtime sensitive-data classifier.
//
// A Monitor watches untrusted content as it enters the conversation
// (the operator prompt, dynamic context, tool results) and owns the
// run-scoped one-way sensitive-data latch: once sensitive content is
// observed, the run "holds sensitive data" for the rest of its life.
// LLM guards participate through TripFromGuard — a guard decision whose
// criterion matches the configured set tightens the latch but can never
// loosen it, so a coerced guard stays fail-safe.
//
// Like the guard package, this package is deliberately leaf-level: its
// only harness-internal import is security (for the deterministic
// detector). The factory decides arming and injects a Monitor into the
// agentic loop; the loop calls through the interface unconditionally
// (Noop when unarmed).
//
// Interface note: TripFromGuard returns whether the call transitioned
// the latch false→true. The plan sketched a void return, but the loop
// needs the transition signal to emit rule_of_two_triggered exactly
// once under concurrent observation, and only the monitor's atomic
// latch can adjudicate the winner.
package ruleoftwo

import "context"

// Monitor classifies freshly-arrived untrusted content and owns the
// run-scoped sensitive-data latch. Implementations must be safe for
// concurrent use: sub-agent loops share the parent's monitor.
type Monitor interface {
	// ObserveChunks scans content that just entered the conversation.
	// source and turn identify the provenance for the caller's
	// telemetry; the returned Detection reports the deduplicated
	// pattern names, the aggregate tier, and whether this call
	// transitioned the latch.
	ObserveChunks(ctx context.Context, source string, turn int, chunks []string) Detection

	// TripFromGuard is the one-way ratchet from an LLM guard. The
	// monitor filters internally: a criterion outside the configured
	// guard-criteria set is ignored. Returns true only when this call
	// transitioned the latch false→true.
	TripFromGuard(guardID, criterion string) bool

	// Tripped reports whether the latch is set. There is no reset.
	Tripped() bool

	// Enforcing reports whether detections are allowed to change run
	// behaviour. False means observe-only: events and metrics fire but
	// no consumer acts on the latch.
	Enforcing() bool

	// Action is the effective on-detect action: the configured
	// onDetect when enforcing, "warn" otherwise.
	Action() string

	// Redact rewrites latch-tier sensitive spans in content with a
	// fixed placeholder and returns the rewritten content plus the
	// number of spans replaced. Not called by the loop in this wave;
	// the onDetect=redact consumer lands with enforcement.
	Redact(content string) (string, int)
}

// Detection is the result of one ObserveChunks call.
type Detection struct {
	// Patterns is the deduplicated set of pattern names that matched
	// across all chunks (e.g. "secret/aws_access_key_id",
	// "pii/credit_card"). Names only — never matched content.
	Patterns []string
	// Tier is the aggregate tier: security.TierLatch when any finding
	// is latch-tier, security.TierWarn otherwise. Empty when Patterns
	// is empty.
	Tier string
	// Transition is true only when this call flipped the latch
	// false→true. At most one Detection per run carries it.
	Transition bool
}
