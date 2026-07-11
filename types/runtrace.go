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
	// Outcome is "success" | "error" | "max_turns" | "verification_failed" |
	// "verification_error" | "budget_exceeded" | "stalled" | "tool_failures" |
	// "cancelled" | "timeout" | "max_tokens" | "setup_failed" | "hook_failed".
	// setup_failed and hook_failed (issue #461) report a fatal lifecycle-hook
	// failure: setup_failed means a PreRun hook failed before the session
	// started (zero turns ran); hook_failed means a PostRun hook failed after
	// an otherwise-successful run (it never overrides a non-success outcome —
	// the primary failure cause stays authoritative).
	Outcome string `json:"outcome"`
	// FinalAssistantText is the loop's last non-empty assistant text,
	// concatenated across the text blocks of the final response and carried
	// through to RunResult.FinalAssistantText. Omitted when the run produced
	// no assistant turn (e.g. an early validation failure before any turn).
	FinalAssistantText string `json:"finalAssistantText,omitempty"`
	// HookResults carries every lifecycle hook execution recorded during
	// the run (issue #461), across both phases, in dispatch order. Empty
	// when no hooks were configured. Populated by trace emitters that
	// implement the optional trace.HookRecorder capability (today, the
	// JSONL emitter).
	HookResults []HookExecution `json:"hookResults,omitempty"`
}

// HookExecution records the outcome of a single lifecycle hook (issue
// #461). Phase distinguishes "preRun" (before GitStrategy.Setup) from
// "postRun" (after GitStrategy.Finalise); Index is the hook's position
// within its phase's configured list (RunConfig.Hooks.PreRun /
// .PostRun), so a trace consumer can correlate a result back to the
// RunConfig entry that produced it even when Name is empty.
//
// OutputTail is the scrubbed (security.Scrub), tail-capped combined
// stdout+stderr of the hook — trace-only, never surfaced to the model.
// Truncated reports whether the persisted (scrubbed) output exceeded the
// 4KB tail cap; truncation keeps the tail (not the head) because a
// failing command's most useful diagnostic is usually printed last.
type HookExecution struct {
	Phase            string `json:"phase"` // "preRun" | "postRun"
	Index            int    `json:"index"`
	Name             string `json:"name,omitempty"`
	Command          string `json:"command"`
	ExitCode         int    `json:"exitCode"`
	DurationMs       int64  `json:"durationMs"`
	TimedOut         bool   `json:"timedOut,omitempty"`
	Skipped          bool   `json:"skipped,omitempty"`
	ContinuedOnError bool   `json:"continuedOnError,omitempty"`
	Error            string `json:"error,omitempty"`
	OutputTail       string `json:"outputTail,omitempty"`
	Truncated        bool   `json:"truncated,omitempty"`
}

// ToolCallSummary records a single tool call's outcome for the trace.
//
// Name is the model-facing name the call arrived under: under a toolset
// profile (issue #234) this is the alias presented to the model.
// InternalName is the canonical internal tool ID the alias dispatched to.
// Under the default profile (no aliasing) the two are equal, and
// InternalName is omitted from the wire to keep the existing trace shape
// byte-identical; a non-default profile records both so a trace is
// auditable and the alias→internal binding is recoverable.
//
// An empty InternalName is ambiguous in isolation: it means the tool was
// called by its internal name under the default profile, OR the name did
// not resolve to a known tool under a non-default profile. The active
// profile is recorded in the run's RunConfig (tools.profile); read it
// alongside the record to disambiguate.
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
//
// Name is the model-facing name (the alias under a toolset profile);
// InternalName is the internal tool ID it dispatched to (issue #234).
// See ToolCallSummary for the omitempty rationale and the empty-value
// ambiguity (default profile vs unresolved name under a non-default
// profile).
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
//
// RunID and ParentRunID mirror the forwarding tags on TurnTrace and
// ToolCallTrace: populated only when the record originated in a
// sub-agent run forwarded to a parent emitter (see
// trace.NestedJSONLEmitter), absent (omitempty) on parent-emitted
// records so the existing wire shape is preserved. The OTel emitter's
// content-capture path keys its turn-summary pairing on RunID+Turn so
// a sub-agent's turn N cannot be misattributed to the parent's turn N
// when both are in flight.
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
// Structured carries the optional typed result envelope (issue #231) when the
// tool produced one; it is nil for text-only tools and so omitted from the
// persisted trace. It is scrubbed for secret-shaped content at record time on
// the same footing as Output — a file excerpt or command transcript captured
// in the structured payload can contain credentials just as the text can.
// Kind names the Structured payload's shape (see ToolResult.Kind); empty and
// omitted for text-only calls.
type ToolCallRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// InternalName is the canonical internal tool ID the model-facing Name
	// dispatched to under a toolset profile (issue #234). Equal to Name and
	// omitted from the wire under the default (no-alias) profile.
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
