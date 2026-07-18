// Package ruleoftwo defines the Monitor interface and supporting types
// for the harness's Rule-of-Two runtime sensitive-data classifier. See
// docs/safety-rings.md for the design.
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
