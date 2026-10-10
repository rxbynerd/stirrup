// Package types defines RunResult, the payload schema all resultSink
// adapters serialise at end-of-run.
package types

import (
	"encoding/json"
	"unicode/utf8"
)

// DefaultMaxFinalAssistantTextBytes bounds RunResult.FinalAssistantText
// when a run does not set ResultSinkConfig.MaxFinalAssistantTextBytes.
// Leaves headroom under Cloud Logging's ~256 KiB entry ceiling for the
// rest of the RunResult envelope and JSON escaping overhead.
const DefaultMaxFinalAssistantTextBytes = 1 << 17 // 128 KiB

// finalAssistantTextTruncationMarker mirrors asyncResultTruncationSuffix
// in harness/internal/core/types.go so both truncation surfaces read
// the same to an operator scanning logs.
const finalAssistantTextTruncationMarker = "... [truncated by harness]"

// CapFinalAssistantText truncates s to at most maxBytes bytes of
// pre-marker content, appending finalAssistantTextTruncationMarker when
// truncation occurs; the result can exceed maxBytes by the marker's
// length. Always backs off to a UTF-8 rune boundary rather than
// splitting a rune. maxBytes < 0 is treated as 0.
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
// of a run, constructed from the RunTrace. Kept small and stable; full
// traces and workspace tarballs flow through trace emitters and the
// workspace exporter instead.
type RunResult struct {
	// SchemaVersion is bumped on incompatible changes. Absence on the
	// wire is treated as version 0 by back-compat consumers.
	SchemaVersion int `json:"schemaVersion"`

	// RunID is the run's identifier (mirrors RunConfig.RunID).
	RunID string `json:"runId"`

	// Outcome mirrors RunTrace.Outcome. The sentinel "internal-error" is
	// reserved for when the loop produced no RunTrace at all (e.g. a
	// panic before the first turn).
	Outcome string `json:"outcome"`

	// Turns is the number of agentic loop iterations that ran before
	// the loop terminated.
	Turns int `json:"turns"`

	// TokenUsage is the cumulative token count for the run. Input is
	// provider-reported where the provider reports it and estimated
	// otherwise; see TokenUsage.
	TokenUsage TokenUsage `json:"tokenUsage"`

	// DurationMs is the wall-clock duration of the run in milliseconds,
	// measured from RunTrace.StartedAt to RunTrace.CompletedAt.
	DurationMs int64 `json:"durationMs"`

	// FinalAssistantText is the loop's last assistant text. Omitted
	// when the run produced no assistant turn.
	FinalAssistantText string `json:"finalAssistantText,omitempty"`

	// FinalAssistantTextTruncated is true when FinalAssistantText was
	// truncated. The untruncated text remains on the trace.
	FinalAssistantTextTruncated bool `json:"finalAssistantTextTruncated,omitempty"`

	// StructuredOutput mirrors RunTrace.StructuredOutput: the final answer
	// as a JSON document that satisfied RunConfig.OutputSchema. Never
	// truncated; an answer over the size limit fails the run instead.
	StructuredOutput json.RawMessage `json:"structuredOutput,omitempty"`

	// VerifierVerdict is the optional verifier outcome, present only
	// when a verifier ran.
	VerifierVerdict *VerifierResult `json:"verifierVerdict,omitempty"`

	// Error mirrors RunTrace.Error: a short human-readable reason when
	// Outcome is not "success", or the trace persistence failure of an
	// otherwise-successful run. Omitted otherwise.
	Error string `json:"error,omitempty"`

	// HookFailures is the count of lifecycle hook executions whose
	// Error is non-empty, across both phases. Can be non-zero even when
	// Outcome is "success" (a continueOnError hook failed).
	HookFailures int `json:"hookFailures,omitempty"`

	// CommandOutputArchive is the local path or durable URI of the compressed
	// command-output compliance archive, when command output was captured.
	CommandOutputArchive string `json:"commandOutputArchive,omitempty"`

	// Git mirrors RunTrace.Git: the branch, result SHA and base SHA of a
	// deterministic-git run. Omitted when the git strategy is "none".
	Git *GitHandoff `json:"git,omitempty"`

	// WorkspaceExport mirrors RunTrace.WorkspaceExport. Omitted when no
	// export destination is configured.
	WorkspaceExport *ExportReceipt `json:"workspaceExport,omitempty"`

	// DegradedCapabilities names configured capabilities the run
	// proceeded without, such as an optional MCP server that was
	// unavailable at startup. Can be non-empty when Outcome is "success".
	DegradedCapabilities []DegradedCapability `json:"degradedCapabilities,omitempty"`
}

// VerifierResult is the verifier outcome carried on RunResult. It
// mirrors VerificationResult but is intentionally a distinct, smaller
// type, independent of the internal trace's evolving fields.
type VerifierResult struct {
	Passed   bool   `json:"passed"`
	Feedback string `json:"feedback,omitempty"`
}

// Workspace export statuses carried on ExportReceipt.Status.
const (
	// ExportStatusUploaded means a non-empty archive reached the destination.
	ExportStatusUploaded = "uploaded"
	// ExportStatusEmpty means the workspace had no files; an empty archive
	// still reached the destination.
	ExportStatusEmpty = "empty"
	// ExportStatusFailed means no archive reached the destination.
	ExportStatusFailed = "failed"
)

// ExportReceipt describes one workspace export attempt.
type ExportReceipt struct {
	// URI is the destination object URI.
	URI string `json:"uri"`

	// Status is one of the ExportStatus* constants.
	Status string `json:"status"`

	// Bytes is the size of the uploaded gzipped tarball.
	Bytes int64 `json:"bytes,omitempty"`

	// SHA256 is the lowercase hex SHA-256 of the uploaded gzipped tarball.
	SHA256 string `json:"sha256,omitempty"`

	// Files counts the archive's non-directory entries (regular files,
	// symlinks and hard links).
	Files int `json:"files,omitempty"`

	// Required reports whether a failed export fails the run; see
	// ExecutorConfig.WorkspaceExportRequired.
	Required bool `json:"required"`

	// Error is the scrubbed, length-capped export failure. Empty unless
	// Status is ExportStatusFailed.
	Error string `json:"error,omitempty"`
}

// Delivered reports whether an archive, possibly empty, reached the
// destination. A required export fails whenever this is false.
func (r ExportReceipt) Delivered() bool {
	return r.Status == ExportStatusUploaded || r.Status == ExportStatusEmpty
}
