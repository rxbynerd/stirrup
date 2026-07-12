// Package types — RunResult is the payload schema all resultSink adapters
// serialise at end-of-run. The schema is intentionally small: it carries
// the *answer* (outcome, turn count, token usage, the loop's last
// assistant text, optional verifier verdict), not the *evidence* (full
// trace + workspace snapshot, which the trace emitter and workspace
// exporter handle independently).
//
// Schema versioning rule: SchemaVersion is bumped on incompatible
// changes. A consumer reading an older payload sees absence of the
// SchemaVersion field as version 0 for back-compat — i.e. the field's
// zero value is reserved for "unset" rather than "version 0 explicitly".
// New fields must remain backward-compatible (additive, omitempty) so
// the version bump is reserved for breaking changes such as renaming or
// removing a field.
package types

import "unicode/utf8"

// DefaultMaxFinalAssistantTextBytes bounds RunResult.FinalAssistantText
// when a run does not set ResultSinkConfig.MaxFinalAssistantTextBytes
// (or sets it to zero). Cloud Logging caps a single log entry at
// roughly 256 KiB; the stdout-json result sink serialises the entire
// RunResult envelope — not just this field — onto one STIRRUP_RESULT
// line, and JSON string escaping can expand the text further, so
// bounding this field to 128 KiB leaves headroom under the entry
// ceiling for the rest of the envelope and any escaping overhead.
const DefaultMaxFinalAssistantTextBytes = 1 << 17 // 128 KiB

// finalAssistantTextTruncationMarker is appended to
// RunResult.FinalAssistantText when CapFinalAssistantText truncates it.
// Mirrors asyncResultTruncationSuffix in harness/internal/core/types.go
// so both truncation surfaces read the same to an operator scanning
// logs.
const finalAssistantTextTruncationMarker = "... [truncated by harness]"

// CapFinalAssistantText truncates s to at most maxBytes bytes of
// pre-marker content and appends finalAssistantTextTruncationMarker
// when truncation occurs. The returned capped string is therefore up
// to len(finalAssistantTextTruncationMarker) bytes longer than
// maxBytes. Exported so the RunResult builder (harness/cmd/stirrup/cmd,
// outside this package) can apply the cap without duplicating the
// rune-boundary logic.
//
// Truncation always lands on a UTF-8 rune boundary: FinalAssistantText
// is model-produced text that must stay valid UTF-8 so the
// STIRRUP_RESULT JSON line stays well-formed, so — unlike
// extractAsyncToolResult's byte-boundary truncation of untrusted
// control-plane content — this backs off to the start of the rune
// straddling the cut rather than splitting it.
//
// maxBytes < 0 is treated as 0 rather than "no cap": callers resolve
// the effective cap via ResultSinkConfig.ResolvedMaxFinalAssistantTextBytes
// before calling this helper, so a negative value here should not
// silently pass the string through unbounded.
func CapFinalAssistantText(s string, maxBytes int) (capped string, truncated bool) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(s) <= maxBytes {
		return s, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + finalAssistantTextTruncationMarker, true
}

// RunResult is the payload all resultSink adapters serialise at the end
// of a run. Constructed from the existing RunTrace at end-of-run.
//
// The schema is the contract across resultSink adapters (stdout-json,
// gcp-pubsub, gcs, and the AWS / Azure follow-ups reserved in
// validResultSinkTypes). Keep it small and stable; large artefacts
// (full traces, workspace tarballs) flow through trace emitters and the
// workspace exporter instead.
type RunResult struct {
	// SchemaVersion is bumped on incompatible changes. Absence on the
	// wire is treated as version 0 by back-compat consumers.
	SchemaVersion int `json:"schemaVersion"`

	// RunID is the run's identifier (mirrors RunConfig.RunID).
	RunID string `json:"runId"`

	// Outcome mirrors RunTrace.Outcome — e.g. "success", "error",
	// "stalled", "tool_failures", "timeout", "max_turns",
	// "verification_failed", "verification_error", "budget_exceeded",
	// "cancelled", "max_tokens". The sentinel "internal-error" is
	// reserved for the case where the loop produced no RunTrace at all
	// (e.g. a panic before the first turn); consumers should treat it
	// as distinguishable from any RunTrace.Outcome value because no
	// trace exists to inspect.
	Outcome string `json:"outcome"`

	// Turns is the number of agentic loop iterations that ran before
	// the loop terminated.
	Turns int `json:"turns"`

	// TokenUsage is the cumulative input/output token count for the
	// run, as reported by the model provider.
	TokenUsage TokenUsage `json:"tokenUsage"`

	// DurationMs is the wall-clock duration of the run in milliseconds,
	// measured from RunTrace.StartedAt to RunTrace.CompletedAt.
	DurationMs int64 `json:"durationMs"`

	// FinalAssistantText is the loop's last assistant text. Omitted
	// when the run produced no assistant turn (e.g. early validation
	// failure). Callers that framed their prompt for JSON output parse
	// this field; for free-form runs it is informational.
	FinalAssistantText string `json:"finalAssistantText,omitempty"`

	// FinalAssistantTextTruncated is true when FinalAssistantText was
	// truncated to satisfy ResultSinkConfig.ResolvedMaxFinalAssistantTextBytes
	// (issue #463). The full, untruncated text is still available on the
	// trace via the trace emitter's RecordFinalAssistantText — this flag
	// only describes the copy carried on this RunResult.
	FinalAssistantTextTruncated bool `json:"finalAssistantTextTruncated,omitempty"`

	// VerifierVerdict is the optional verifier outcome, present only
	// when a verifier ran. Distinct from VerificationResult (the
	// internal trace shape) so the wire schema can evolve
	// independently — the issue calls the field "verifierVerdict",
	// and keeping the result-sink schema small is an explicit goal.
	VerifierVerdict *VerifierResult `json:"verifierVerdict,omitempty"`

	// Error carries a short human-readable reason when Outcome is not
	// "success". Omitted on success.
	Error string `json:"error,omitempty"`

	// HookFailures is the count of lifecycle hook executions (issue
	// #461) whose Error is non-empty, across both phases. Zero (the
	// default) when no hooks were configured or every hook succeeded.
	// A non-zero count is possible even when Outcome is "success": a
	// continueOnError hook that failed does not override the outcome
	// but is still counted here so a consumer scanning RunResult alone
	// (without the full trace) can see that setup or teardown was not
	// entirely clean.
	HookFailures int `json:"hookFailures,omitempty"`
}

// VerifierResult is the verifier outcome carried on RunResult. It
// mirrors the shape of VerificationResult but is intentionally a
// distinct, smaller type: the RunResult payload is a closed-set
// contract across resultSink adapters and must stay independent of the
// internal trace's evolving fields.
type VerifierResult struct {
	Passed   bool   `json:"passed"`
	Feedback string `json:"feedback,omitempty"`
}
