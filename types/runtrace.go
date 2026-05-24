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
// runs whose trace was forwarded to this emitter (see #55 nested
// trace forwarding). Entries with a non-empty RunID distinct from the
// parent run's ID — equivalently, a non-empty ParentRunID — are sub-
// agent calls. Consumers computing parent-only aggregates must filter
// on RunID/ParentRunID; otherwise sub-agent activity is double-counted.
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
	Outcome             string               `json:"outcome"` // "success" | "error" | "max_turns" | "verification_failed" | "verification_error" | "budget_exceeded" | "stalled" | "tool_failures" | "cancelled" | "timeout" | "max_tokens"
}

// ToolCallSummary records a single tool call's outcome for the trace.
type ToolCallSummary struct {
	Name        string `json:"name"`
	DurationMs  int64  `json:"durationMs"`
	Success     bool   `json:"success"`
	ErrorReason string `json:"errorReason,omitempty"`
	InputSize   int    `json:"inputSize,omitempty"`
	OutputSize  int    `json:"outputSize,omitempty"`
	// RunID identifies the run that produced this tool call. Populated only
	// when the call originated in a sub-agent run forwarded to a parent
	// emitter; absent (omitempty) on parent-emitted events to preserve
	// the existing wire shape.
	RunID string `json:"runId,omitempty"`
	// ParentRunID is the run ID of the sub-agent's parent. Populated only
	// for forwarded sub-agent events.
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

// TurnMode* are the documented values of TurnTrace.Mode. Empty
// string ("") means streaming for backward compatibility with
// pre-phase-5 traces (#138) and for error turns that fail before a
// concrete provider is resolved.
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
	// RunID identifies the run that produced this turn. Populated only
	// when the turn originated in a sub-agent run forwarded to a parent
	// emitter; absent (omitempty) on parent-emitted events to preserve
	// the existing wire shape.
	RunID string `json:"runId,omitempty"`
	// ParentRunID is the run ID of the sub-agent's parent. Populated only
	// for forwarded sub-agent events.
	ParentRunID string `json:"parentRunId,omitempty"`
	// Mode is "streaming" or "batch". Empty string deserialises from
	// traces that predate this field; downstream consumers treat empty
	// as streaming for backward compatibility (#138).
	Mode string `json:"mode,omitempty"`
	// BatchID is the provider-assigned batch identifier for batch turns.
	// Empty for streaming turns. Allows cross-referencing a TurnTrace
	// with the provider's batch console / API.
	BatchID string `json:"batchId,omitempty"`
}

// IsBatch reports whether the turn was submitted via async batch. An
// empty Mode is treated as streaming for backward compatibility with
// traces that predate the Mode field (phase 5, #138).
func (t TurnTrace) IsBatch() bool {
	return t.Mode == TurnModeBatch
}

// ToolCallTrace records telemetry for a single tool call.
//
// Field order MUST match ToolCallSummary so the cast in trace emitters
// (types.ToolCallSummary(tc)) remains valid.
type ToolCallTrace struct {
	Name        string `json:"name"`
	DurationMs  int64  `json:"durationMs"`
	Success     bool   `json:"success"`
	ErrorReason string `json:"errorReason,omitempty"`
	InputSize   int    `json:"inputSize,omitempty"`
	OutputSize  int    `json:"outputSize,omitempty"`
	// RunID identifies the run that produced this tool call. Populated only
	// when the call originated in a sub-agent run forwarded to a parent
	// emitter; absent (omitempty) on parent-emitted events to preserve
	// the existing wire shape.
	RunID string `json:"runId,omitempty"`
	// ParentRunID is the run ID of the sub-agent's parent. Populated only
	// for forwarded sub-agent events.
	ParentRunID string `json:"parentRunId,omitempty"`
	// ErrorCategory is the normalised failure category for failed calls.
	// Always empty on Success=true; one of the
	// observability.ToolFailureCategory enum values otherwise. Kept here
	// so the JSONL trace files carry the same taxonomy that the
	// stirrup.harness.tool_failures metric labels.
	ErrorCategory string `json:"errorCategory,omitempty"`
}

// TurnRecord captures the full input/output of a single agentic loop turn.
type TurnRecord struct {
	Turn        int              `json:"turn"`
	ModelInput  ModelInput       `json:"modelInput"`
	ModelOutput []ContentBlock   `json:"modelOutput"`
	ToolCalls   []ToolCallRecord `json:"toolCalls"`
}

// ModelInput records what the model saw on a given turn.
type ModelInput struct {
	Messages []Message        `json:"messages"`
	Tools    []ToolDefinition `json:"tools"`
	Model    string           `json:"model"`
}

// ToolCallRecord records a single tool call and its result.
type ToolCallRecord struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input"`
	Output     string          `json:"output"`
	DurationMs int64           `json:"durationMs"`
	Success    bool            `json:"success"`
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
