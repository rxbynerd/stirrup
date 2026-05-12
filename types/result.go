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

// RunResult is the payload all resultSink adapters serialise at the end
// of a run. Constructed from the existing RunTrace at end-of-run.
//
// The schema is the contract across resultSink adapters (stdout-json,
// pubsub, gcs, and the AWS / Azure follow-ups reserved in
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
	// "cancelled", "max_tokens".
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

	// VerifierVerdict is the optional verifier outcome, present only
	// when a verifier ran. Distinct from VerificationResult (the
	// internal trace shape) so the wire schema can evolve
	// independently — the issue calls the field "verifierVerdict",
	// and keeping the result-sink schema small is an explicit goal.
	VerifierVerdict *VerifierResult `json:"verifierVerdict,omitempty"`

	// Error carries a short human-readable reason when Outcome is not
	// "success". Omitted on success.
	Error string `json:"error,omitempty"`
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
