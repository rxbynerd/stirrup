package types

import (
	"encoding/json"
	"time"
)

// TokenUsage tracks input and output token counts.
type TokenUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// RunTrace captures the full telemetry of a single harness run.
//
// ToolCalls contains tool call summaries for this run and any sub-agent
// runs whose trace was forwarded to this emitter. Entries with a non-empty
// ParentRunID are sub-agent calls; consumers computing parent-only
// aggregates must filter on RunID/ParentRunID or sub-agent activity is
// double-counted.
type RunTrace struct {
	ID                  string               `json:"id"`
	Config              RunConfig            `json:"config"`
	StartedAt           time.Time            `json:"startedAt"`
	CompletedAt         time.Time            `json:"completedAt"`
	Turns               int                  `json:"turns"`
	TokenUsage          TokenUsage           `json:"tokenUsage"`
	ToolCalls           []ToolCallSummary    `json:"toolCalls"`
	PermissionDenials   int                  `json:"permissionDenials,omitempty"`
	VerificationResults []VerificationResult `json:"verificationResults"`
	// Outcome is "success" | "error" | "max_turns" | "verification_failed" |
	// "verification_error" | "budget_exceeded" | "stalled" | "tool_failures" |
	// "cancelled" | "timeout" | "max_tokens" | "setup_failed" | "hook_failed".
	// See docs/configuration.md#lifecycle-hooks for setup_failed/hook_failed.
	Outcome string `json:"outcome"`
	// FinalAssistantText is the loop's last non-empty assistant text,
	// concatenated across the text blocks of the final response and carried
	// through to RunResult.FinalAssistantText. Omitted when the run produced
	// no assistant turn (e.g. an early validation failure before any turn).
	FinalAssistantText string `json:"finalAssistantText,omitempty"`
	// HookResults carries every lifecycle hook execution recorded during the
	// run, across both phases, in dispatch order. Empty when no hooks were
	// configured. Populated by trace emitters implementing trace.HookRecorder
	// (today, the JSONL emitter).
	HookResults []HookExecution `json:"hookResults,omitempty"`
}

// HookExecution records the outcome of a single lifecycle hook. Phase
// distinguishes "preRun" (before GitStrategy.Setup) from "postRun" (after
// GitStrategy.Finalise); Index is the hook's position within its phase's
// configured list, correlating a result back to its RunConfig entry even
// when Name is empty.
//
// OutputTail and Command are scrubbed (security.Scrub) by the trace
// emitter before persistence — defence-in-depth alongside
// ValidateRunConfig's rejection of "secret://" references in hook
// commands. Truncated reports whether the tail-capped (4KB) output was
// cut; the cap keeps the tail, not the head, since a failing command's
// most useful diagnostic is usually printed last.
type HookExecution struct {
	Phase   string `json:"phase"` // "preRun" | "postRun"
	Index   int    `json:"index"`
	Name    string `json:"name,omitempty"`
	Command string `json:"command"`
	// ExitCode is the hook command's process exit code. Its zero value is
	// ambiguous: 0 means either "the command exited successfully" or "no
	// exit code was ever obtained" (e.g. an executor transport error
	// before the process ran, or a Skipped entry). Consumers must not
	// infer success from ExitCode == 0 — key off Error != "" instead, or
	// call Failed().
	ExitCode         int    `json:"exitCode"`
	DurationMs       int64  `json:"durationMs"`
	TimedOut         bool   `json:"timedOut,omitempty"`
	Skipped          bool   `json:"skipped,omitempty"`
	ContinuedOnError bool   `json:"continuedOnError,omitempty"`
	Error            string `json:"error,omitempty"`
	OutputTail       string `json:"outputTail,omitempty"`
	Truncated        bool   `json:"truncated,omitempty"`
}

// Failed reports whether the hook execution failed. Prefer this over
// comparing ExitCode or Error directly, since ExitCode's zero value is
// ambiguous (see the ExitCode field doc).
func (h HookExecution) Failed() bool {
	return h.Error != ""
}

// ToolCallSummary records a single tool call's outcome for the trace.
//
// Name is the model-facing name the call arrived under — under a toolset
// profile this is the alias presented to the model. InternalName is the
// canonical internal tool ID the alias dispatched to; equal to Name and
// omitted from the wire under the default (no-alias) profile. An empty
// InternalName is ambiguous in isolation — check RunConfig's tools.profile
// to disambiguate "default profile" from "name did not resolve".
type ToolCallSummary struct {
	// ID is the provider-assigned tool_use identifier the call arrived
	// under. Empty on traces that predate the field and for providers
	// without call identifiers; mirrors ToolCallRecord.ID.
	ID           string `json:"id,omitempty"`
	Name         string `json:"name"`
	InternalName string `json:"internalName,omitempty"`
	DurationMs   int64  `json:"durationMs"`
	Success      bool   `json:"success"`
	ErrorReason  string `json:"errorReason,omitempty"`
	InputSize    int    `json:"inputSize,omitempty"`
	OutputSize   int    `json:"outputSize,omitempty"`
	// RunID identifies the run that produced this tool call. Populated only
	// for sub-agent calls forwarded to a parent emitter.
	RunID string `json:"runId,omitempty"`
	// ParentRunID is the run ID of the sub-agent's parent, when forwarded.
	ParentRunID string `json:"parentRunId,omitempty"`
	// ErrorCategory is the normalised failure category for failed calls.
	// Always empty on Success=true; one of the
	// observability.ToolFailureCategory enum values otherwise. Kept here
	// so the JSONL trace files carry the same taxonomy that the
	// stirrup.harness.tool_failures metric labels.
	ErrorCategory string `json:"errorCategory,omitempty"`
}

// VerificationResult holds the outcome of a verification check.
type VerificationResult struct {
	Passed   bool           `json:"passed"`
	Feedback string         `json:"feedback,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

// TurnMode* are the documented values of TurnTrace.Mode. Empty string
// means streaming, for backward compatibility with older traces and for
// error turns that fail before a concrete provider is resolved.
const (
	TurnModeStreaming = "streaming"
	TurnModeBatch     = "batch"
)

// TurnTrace captures telemetry for a single agentic loop turn.
type TurnTrace struct {
	Turn       int        `json:"turn"`
	Tokens     TokenUsage `json:"tokens"`
	ToolCalls  int        `json:"toolCalls"`
	StopReason string     `json:"stopReason"`
	DurationMs int64      `json:"durationMs"`
	// Model is the router's resolved model for this turn. Empty on traces
	// that predate the field; consumers fall back to the run-level
	// configured model when absent.
	Model string `json:"model,omitempty"`
	// RunID identifies the run that produced this turn. Populated only
	// when the turn originated in a sub-agent run forwarded to a parent
	// emitter; absent (omitempty) on parent-emitted events to preserve
	// the existing wire shape.
	RunID string `json:"runId,omitempty"`
	// ParentRunID is the run ID of the sub-agent's parent. Populated only
	// for forwarded sub-agent events.
	ParentRunID string `json:"parentRunId,omitempty"`
	// Mode is "streaming" or "batch". Empty deserialises from traces that
	// predate this field; treated as streaming for backward compatibility.
	Mode string `json:"mode,omitempty"`
	// BatchID is the provider-assigned batch identifier for batch turns.
	// Empty for streaming turns. Allows cross-referencing a TurnTrace
	// with the provider's batch console / API.
	BatchID string `json:"batchId,omitempty"`
}

// IsBatch reports whether the turn was submitted via async batch.
func (t TurnTrace) IsBatch() bool {
	return t.Mode == TurnModeBatch
}

// ToolCallTrace records telemetry for a single tool call.
//
// Field order MUST match ToolCallSummary so the cast in trace emitters
// (types.ToolCallSummary(tc)) remains valid. See ToolCallSummary for the
// Name/InternalName omitempty rationale.
type ToolCallTrace struct {
	// ID is the provider-assigned tool_use identifier the call arrived
	// under. Empty on traces that predate the field and for providers
	// without call identifiers. The OTel emitter's content-capture path
	// keys its tool-call pairing on (RunID, ID); an empty ID opts the
	// call out of pairing, never out of its span.
	ID           string `json:"id,omitempty"`
	Name         string `json:"name"`
	InternalName string `json:"internalName,omitempty"`
	DurationMs   int64  `json:"durationMs"`
	Success      bool   `json:"success"`
	ErrorReason  string `json:"errorReason,omitempty"`
	InputSize    int    `json:"inputSize,omitempty"`
	OutputSize   int    `json:"outputSize,omitempty"`
	// RunID identifies the run that produced this tool call. Populated only
	// for sub-agent calls forwarded to a parent emitter.
	RunID string `json:"runId,omitempty"`
	// ParentRunID is the run ID of the sub-agent's parent, when forwarded.
	ParentRunID string `json:"parentRunId,omitempty"`
	// ErrorCategory is the normalised failure category for failed calls.
	// Always empty on Success=true; one of the
	// observability.ToolFailureCategory enum values otherwise. Kept here
	// so the JSONL trace files carry the same taxonomy that the
	// stirrup.harness.tool_failures metric labels.
	ErrorCategory string `json:"errorCategory,omitempty"`
}

// TurnRecord captures the full input/output of a single agentic loop turn.
//
// RunID and ParentRunID mirror the forwarding tags on TurnTrace and
// ToolCallTrace (see trace.NestedJSONLEmitter). The OTel emitter keys its
// turn-summary pairing on RunID+Turn so a sub-agent's turn N is never
// misattributed to the parent's turn N.
type TurnRecord struct {
	Turn        int              `json:"turn"`
	ModelInput  ModelInput       `json:"modelInput"`
	ModelOutput []ContentBlock   `json:"modelOutput"`
	ToolCalls   []ToolCallRecord `json:"toolCalls"`
	RunID       string           `json:"runId,omitempty"`
	ParentRunID string           `json:"parentRunId,omitempty"`
}

// ModelInput records what the model saw on a given turn.
type ModelInput struct {
	Messages []Message        `json:"messages"`
	Tools    []ToolDefinition `json:"tools"`
	Model    string           `json:"model"`
}

// ToolCallRecord records a single tool call and its result.
//
// Structured carries the optional typed result envelope when the tool
// produced one; nil and omitted for text-only tools. It is scrubbed for
// secret-shaped content at record time on the same footing as Output.
// Kind names the Structured payload's shape (see ToolResult.Kind); empty
// and omitted for text-only calls.
type ToolCallRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// InternalName is the canonical internal tool ID the model-facing Name
	// dispatched to under a toolset profile. Equal to Name and omitted
	// from the wire under the default (no-alias) profile.
	InternalName string          `json:"internalName,omitempty"`
	Input        json.RawMessage `json:"input"`
	Output       string          `json:"output"`
	DurationMs   int64           `json:"durationMs"`
	Success      bool            `json:"success"`
	Structured   json.RawMessage `json:"structured,omitempty"`
	Kind         string          `json:"kind,omitempty"`
}

// RunRecording is a full recording of a run.
type RunRecording struct {
	RunID        string       `json:"runId"`
	Config       RunConfig    `json:"config"`
	Turns        []TurnRecord `json:"turns"`
	FinalOutcome RunTrace     `json:"finalOutcome"`
}

// BudgetCheck holds the result of a token budget check.
type BudgetCheck struct {
	WithinBudget  bool       `json:"withinBudget"`
	CurrentTokens TokenUsage `json:"currentTokens"`
	Reason        string     `json:"reason,omitempty"`
}
