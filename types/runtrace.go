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
type RunTrace struct {
	ID                  string               `json:"id"`
	Config              RunConfig            `json:"config"`
	StartedAt           time.Time            `json:"startedAt"`
	CompletedAt         time.Time            `json:"completedAt"`
	Turns               int                  `json:"turns"`
	TokenUsage          TokenUsage           `json:"tokenUsage"`
	ToolCalls           []ToolCallSummary    `json:"toolCalls"`
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
}

// VerificationResult holds the outcome of a verification check.
type VerificationResult struct {
	Passed   bool           `json:"passed"`
	Feedback string         `json:"feedback,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

// TurnTrace captures telemetry for a single agentic loop turn.
type TurnTrace struct {
	Turn       int        `json:"turn"`
	Tokens     TokenUsage `json:"tokens"`
	ToolCalls  int        `json:"toolCalls"`
	StopReason string     `json:"stopReason"`
	DurationMs int64      `json:"durationMs"`
}

// ToolCallTrace records telemetry for a single tool call.
type ToolCallTrace struct {
	Name        string `json:"name"`
	DurationMs  int64  `json:"durationMs"`
	Success     bool   `json:"success"`
	ErrorReason string `json:"errorReason,omitempty"`
	InputSize   int    `json:"inputSize,omitempty"`
	OutputSize  int    `json:"outputSize,omitempty"`
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
