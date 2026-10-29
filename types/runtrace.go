package types

import (
	"encoding/json"
	"math"
	"time"
)

// TokenUsage tracks token counts for a turn or a run.
//
// Invariants: Input includes cached tokens (CacheRead + CacheWrite <=
// Input), and Reasoning is a subset of Output (Reasoning <= Output).
// Input is the provider-reported prompt size when the provider reports
// one, otherwise the harness's estimate; TurnTrace.InputReported tells
// them apart per turn. The three breakdown fields are zero when the
// provider does not report them.
type TokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead,omitempty"`
	CacheWrite int `json:"cacheWrite,omitempty"`
	Reasoning  int `json:"reasoning,omitempty"`
}

// Add accumulates o into u, saturating at math.MaxInt so hostile
// provider counts cannot wrap a running total negative.
func (u *TokenUsage) Add(o TokenUsage) {
	u.Input = saturatingAdd(u.Input, o.Input)
	u.Output = saturatingAdd(u.Output, o.Output)
	u.CacheRead = saturatingAdd(u.CacheRead, o.CacheRead)
	u.CacheWrite = saturatingAdd(u.CacheWrite, o.CacheWrite)
	u.Reasoning = saturatingAdd(u.Reasoning, o.Reasoning)
}

// Total is Input + Output, saturating at math.MaxInt. Cached input
// counts in full.
func (u TokenUsage) Total() int {
	return saturatingAdd(u.Input, u.Output)
}

// MergeEvent folds the counts of one message_complete event into u. An
// event carrying InputTokens is a complete usage snapshot and replaces
// u; any other event overwrites only its non-zero counts, so a bare stop
// event or a trailing output-only event never clears a reported count.
func (u *TokenUsage) MergeEvent(ev StreamEvent) {
	if ev.InputTokens > 0 {
		*u = TokenUsage{}
	}
	for _, f := range []struct {
		dst *int
		v   int
	}{
		{&u.Input, ev.InputTokens},
		{&u.Output, ev.OutputTokens},
		{&u.CacheRead, ev.CacheReadTokens},
		{&u.CacheWrite, ev.CacheWriteTokens},
		{&u.Reasoning, ev.ReasoningTokens},
	} {
		if f.v > 0 {
			*f.dst = f.v
		}
	}
}

func saturatingAdd(a, b int) int {
	switch {
	case b > 0 && a > math.MaxInt-b:
		return math.MaxInt
	case b < 0 && a < math.MinInt-b:
		return math.MinInt
	}
	return a + b
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
	// "cancelled" | "timeout" | "max_tokens" | "setup_failed" | "hook_failed" |
	// "handoff_failed" | "control_stream_lost" | "structured_output_failed" |
	// "guardrail_blocked" | "rule_of_two_violation" |
	// "command_output_capture_failed" | "command_output_archive_failed" |
	// "context_exhausted" (the smallest request the context strategy and
	// tool result truncation could prepare did not fit the context budget,
	// so it was not sent, or a turn exceeded the model's context window
	// again after a forced compaction), or a provider stop reason passed
	// through verbatim, such as "refusal" (the model declined to respond;
	// see StopDetails).
	// See docs/configuration.md#lifecycle-hooks for setup_failed/hook_failed.
	// handoff_failed reports a deterministic-git final checkpoint that failed
	// or left the working tree dirty (see Git), or a required workspace
	// export that failed (see WorkspaceExport). handoff_failed, hook_failed
	// and both command_output_* outcomes claim only an otherwise-successful
	// run — the primary failure cause stays authoritative — and a capture
	// failure outranks an archive failure. control_stream_lost reports a
	// run cancelled because its gRPC control stream ended while it was
	// active. structured_output_failed reports a final answer that still
	// did not satisfy RunConfig.OutputSchema after the corrective attempts,
	// or that satisfied it only before secrets were scrubbed from it; such
	// a run never reaches the verifier.
	Outcome string `json:"outcome"`
	// StopDetails is the final model turn's provider-reported stop detail
	// when the run ended on a non-tool stop reason, such as the category
	// and explanation of a "refusal". Nil otherwise.
	StopDetails *StopDetails `json:"stopDetails,omitempty"`
	// FinalAssistantText is the loop's last non-empty assistant text,
	// concatenated across the text blocks of the final response and carried
	// through to RunResult.FinalAssistantText. Omitted when the run produced
	// no assistant turn (e.g. an early validation failure before any turn).
	FinalAssistantText string `json:"finalAssistantText,omitempty"`
	// StructuredOutput is the scrubbed, compact JSON document of a final
	// answer that satisfied RunConfig.OutputSchema. Set only when the
	// outcome is "success"; omitted otherwise, and when no schema was
	// configured.
	StructuredOutput json.RawMessage `json:"structuredOutput,omitempty"`
	// HookResults carries every lifecycle hook execution recorded during the
	// run, across both phases, in dispatch order. Empty when no hooks were
	// configured. Populated by trace emitters implementing trace.HookRecorder
	// (today, the JSONL emitter).
	HookResults          []HookExecution `json:"hookResults,omitempty"`
	CommandOutputArchive string          `json:"commandOutputArchive,omitempty"`
	// Git is the git state the run handed off after its final checkpoint.
	// Nil when the git strategy does not manage git (gitStrategy "none").
	Git *GitHandoff `json:"git,omitempty"`
	// WorkspaceExport is the receipt of the end-of-run workspace export.
	// Nil when no export destination is configured.
	WorkspaceExport *ExportReceipt `json:"workspaceExport,omitempty"`
	// Error is the scrubbed, length-capped reason for a non-success
	// Outcome: the run's earliest failure. It is also set on a successful
	// run whose trace could not be persisted, in which case only the
	// returned trace carries it. Empty otherwise.
	Error string `json:"error,omitempty"`
	// DegradedCapabilities lists configured capabilities the run proceeded
	// without, such as an optional MCP server that was unavailable at
	// startup. Empty when nothing degraded.
	DegradedCapabilities []DegradedCapability `json:"degradedCapabilities,omitempty"`
	// RepositoryGuidance records each configured repositoryGuidance file as
	// loaded for the system prompt, in render order. Populated by trace
	// emitters implementing trace.RepositoryGuidanceRecorder.
	RepositoryGuidance []RepositoryGuidanceRecord `json:"repositoryGuidance,omitempty"`
	// EffectiveTools lists the internal IDs of every tool a sub-agent was
	// offered, including MCP and control-plane tools that Config.Tools.BuiltIn
	// cannot name. Set only on sub-agent traces.
	EffectiveTools []string `json:"effectiveTools,omitempty"`
	// AuxiliaryTokens is the part of TokenUsage spent outside agentic
	// turns on the run's provider credentials: summary generation, the
	// llm-judge verifier and the cloud-judge guard.
	AuxiliaryTokens TokenUsage `json:"auxiliaryTokens,omitzero"`
	// ExternalGuardTokens is the usage reported by a guard on its own
	// endpoint (granite-guardian). It is not part of TokenUsage and is not
	// charged to maxTokenBudget.
	ExternalGuardTokens TokenUsage `json:"externalGuardTokens,omitzero"`
	// PostEditChecks carries every tools.postEdit check execution, in
	// dispatch order. The diagnostics themselves are not recorded here;
	// they reach the trace only inside the tool result they were appended
	// to.
	PostEditChecks []PostEditCheckRecord `json:"postEditChecks,omitempty"`
	// TraceReference identifies the run's OTel root span. Nil when the
	// trace emitter records no OTel span.
	TraceReference *TraceReference `json:"traceReference,omitempty"`
}

// GitHandoff is the git state a deterministic-git run hands off: the run
// branch, the commit holding the run's result, and the commit the branch
// was created from, so a control plane can diff the two.
type GitHandoff struct {
	Branch string `json:"branch,omitempty"`
	SHA    string `json:"sha,omitempty"`
	// BaseSHA is HEAD when the run branch was created. Empty when the
	// branch was created on a repository with no commits.
	BaseSHA string `json:"baseSha,omitempty"`
	// Dirty is true when `git status --porcelain` still reported changes
	// (ignored files excluded) after the final checkpoint.
	Dirty bool `json:"dirty,omitempty"`
	// Error is the scrubbed, length-capped reason the final checkpoint
	// failed or left the working tree dirty. Empty on success.
	Error string `json:"error,omitempty"`
}

// DegradedCapabilityMCP is the DegradedCapability.Kind for an MCP server.
const DegradedCapabilityMCP = "mcp"

// DegradedCapability records one configured capability that was skipped at
// startup because it was unavailable and marked optional.
type DegradedCapability struct {
	// Kind is the capability family, currently DegradedCapabilityMCP.
	Kind string `json:"kind"`
	// Name is the operator-supplied capability name (the MCP server name).
	Name string `json:"name"`
	// Error is the scrubbed, length-bounded reason it was unavailable.
	Error string `json:"error"`
}

// PostEditCheckRecord is the bounded trace metadata for one
// tools.postEdit check execution. Command is the configured template,
// before path substitution. Error is set when the check did not produce
// an exit code (timeout, rejected path, executor failure); as with
// HookExecution, an ExitCode of 0 alone does not mean the check passed.
type PostEditCheckRecord struct {
	ToolUseID   string `json:"toolUseId,omitempty"`
	Turn        int    `json:"turn,omitempty"`
	Tool        string `json:"tool"`
	Path        string `json:"path"`
	Index       int    `json:"index"`
	Command     string `json:"command"`
	ExitCode    int    `json:"exitCode"`
	DurationMs  int64  `json:"durationMs"`
	TimedOut    bool   `json:"timedOut,omitempty"`
	Error       string `json:"error,omitempty"`
	OutputBytes int    `json:"outputBytes,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// Failed reports whether the check reported a problem: a non-zero exit,
// a timeout, or a failure to run.
func (r PostEditCheckRecord) Failed() bool {
	return r.Error != "" || r.ExitCode != 0
}

// CommandOutputStreamRecord describes one complete command stream without
// embedding its content in the trace.
type CommandOutputStreamRecord struct {
	RawBytes          int64    `json:"rawBytes"`
	RawSHA256         string   `json:"rawSha256"`
	ScrubbedBytes     int64    `json:"scrubbedBytes"`
	ScrubbedSHA256    string   `json:"scrubbedSha256"`
	ArchiveMember     string   `json:"archiveMember,omitempty"`
	Reference         string   `json:"reference,omitempty"`
	RedactionCount    int      `json:"redactionCount,omitempty"`
	RedactionPatterns []string `json:"redactionPatterns,omitempty"`
}

// CommandOutputReadRecord identifies the exact scrubbed byte range returned
// by a read_command_output call.
type CommandOutputReadRecord struct {
	ToolUseID     string `json:"toolUseId"`
	Reference     string `json:"reference"`
	Offset        int64  `json:"offset"`
	EndOffset     int64  `json:"endOffset"`
	EOF           bool   `json:"eof"`
	ResultSHA256  string `json:"resultSha256"`
	ArchiveMember string `json:"archiveMember,omitempty"`
}

// CommandOutputRecord is the bounded trace metadata for a run_command
// capture. Full scrubbed bytes live only in the sidecar archive.
type CommandOutputRecord struct {
	ArchiveID                  string                    `json:"archiveId"`
	RunID                      string                    `json:"runId"`
	ParentRunID                string                    `json:"parentRunId,omitempty"`
	Turn                       int                       `json:"turn,omitempty"`
	ToolUseID                  string                    `json:"toolUseId"`
	StartedAt                  time.Time                 `json:"startedAt"`
	CompletedAt                time.Time                 `json:"completedAt"`
	ExitCode                   int                       `json:"exitCode"`
	TimedOut                   bool                      `json:"timedOut,omitempty"`
	Cancelled                  bool                      `json:"cancelled,omitempty"`
	CaptureComplete            bool                      `json:"captureComplete"`
	CaptureError               string                    `json:"captureError,omitempty"`
	Stdout                     CommandOutputStreamRecord `json:"stdout"`
	Stderr                     CommandOutputStreamRecord `json:"stderr"`
	InitialResultSHA256        string                    `json:"initialResultSha256,omitempty"`
	InitialResultMember        string                    `json:"initialResultMember,omitempty"`
	Reads                      []CommandOutputReadRecord `json:"reads,omitempty"`
	LegacySingleRepresentation bool                      `json:"legacySingleRepresentation,omitempty"`
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
//
// RunTrace.VerificationResults holds one per verifier attempt, in order,
// and is empty when the run's verifier is "none". There, Attempt is the
// 1-based attempt number and Error is set, with Passed false, when the
// verifier failed to produce a verdict. Recorded strings are scrubbed
// and capped.
type VerificationResult struct {
	Attempt  int            `json:"attempt,omitempty"`
	Passed   bool           `json:"passed"`
	Feedback string         `json:"feedback,omitempty"`
	Error    string         `json:"error,omitempty"`
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
	Turn   int        `json:"turn"`
	Tokens TokenUsage `json:"tokens"`
	// InputReported is true when Tokens.Input is the provider's figure
	// rather than the harness's estimate.
	InputReported bool   `json:"inputReported,omitempty"`
	ToolCalls     int    `json:"toolCalls"`
	StopReason    string `json:"stopReason"`
	DurationMs    int64  `json:"durationMs"`
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
	// StopDetails is the provider-reported detail qualifying StopReason;
	// nil when the provider reported none.
	StopDetails *StopDetails `json:"stopDetails,omitempty"`
	// Retries counts the attempts at this turn that failed with a
	// retryable provider error and were discarded before the attempt
	// this record describes. Tokens includes any usage the provider
	// reported for those attempts.
	Retries int `json:"retries,omitempty"`
	// ResponseModel is the model identifier the provider reported for this
	// turn's response, which can differ from Model (the requested alias).
	// Empty when the provider reports none.
	ResponseModel string `json:"responseModel,omitempty"`
	// OutputLimitRecovery is true when the turn stopped at the output
	// limit (StopReason "max_tokens") and the run continued with a
	// synthetic instruction instead of ending.
	OutputLimitRecovery bool `json:"outputLimitRecovery,omitempty"`
	// WindDown names the budget threshold that made the harness send its
	// wind-down instruction at the start of this turn: "turns", "tokens"
	// or "time". Empty on every other turn.
	WindDown string `json:"windDown,omitempty"`
	// ToolsLoaded lists, by internal ID, the deferred tools that tool
	// searches in this turn made visible to the model for the rest of the
	// run. Empty when lazy tool loading is off or no search loaded a new
	// tool. Only in-process emitters see this summary field; the persisted
	// copy is TurnRecord.ToolsLoaded, beside ModelInput.Tools with each
	// definition's Deferred flag.
	ToolsLoaded []string `json:"toolsLoaded,omitempty"`
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
	// ResponseModel is the model identifier the provider reported
	// answering with; empty when it reported none. The same value rides on
	// TurnTrace.ResponseModel for emitters that keep only summaries.
	ResponseModel string `json:"responseModel,omitempty"`
	// OutputLimitRecovery mirrors TurnTrace.OutputLimitRecovery.
	OutputLimitRecovery bool `json:"outputLimitRecovery,omitempty"`
	// StallNudge is true when the turn's tool calls completed a repeated
	// call pattern and the next request carries a synthetic instruction
	// to change approach instead of the run ending as stalled.
	StallNudge bool `json:"stallNudge,omitempty"`
	// WindDown mirrors TurnTrace.WindDown.
	WindDown string `json:"windDown,omitempty"`
	// ToolsLoaded mirrors TurnTrace.ToolsLoaded: the deferred tools this
	// turn's searches made visible, by internal ID. Recorded here because
	// the turn record is what the recording emitters persist.
	ToolsLoaded []string `json:"toolsLoaded,omitempty"`
}

// ModelInput records what the model saw on a given turn. Request is nil
// on traces written before request parameters were recorded.
type ModelInput struct {
	Messages []Message        `json:"messages"`
	Tools    []ToolDefinition `json:"tools"`
	Model    string           `json:"model"`
	Request  *RequestParams   `json:"request,omitempty"`
}

// RequestParams records the sampling and steering parameters of the
// request sent for one turn, copied from the StreamParams handed to the
// provider. Temperature is the value after defaulting; nil means the
// request carried none. ReasoningSummary is the value resolved for the
// selected adapter (see ResolveReasoningSummary), never empty on a turn
// the loop sent. CacheKey is the provider-neutral affinity digest, never
// the run ID itself.
type RequestParams struct {
	Temperature      *float64       `json:"temperature,omitempty"`
	MaxTokens        int            `json:"maxTokens"`
	ReasoningEffort  string         `json:"reasoningEffort,omitempty"`
	ReasoningSummary string         `json:"reasoningSummary,omitempty"`
	ToolChoice       ToolChoiceMode `json:"toolChoice"`
	CacheKey         string         `json:"cacheKey,omitempty"`
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
	IsError      bool            `json:"isError,omitempty"`
}

// RunRecording is a full recording of a run.
//
// Turns holds only the recorded run's own turns. Sub-agent runs that
// streamed into the same trace are in SubRuns, one entry per child in
// order of first appearance. CommandOutputs describes the run's whole
// sidecar archive, which sub-agents share, so its entries span every
// run and carry their own RunID.
type RunRecording struct {
	RunID          string                `json:"runId"`
	Config         RunConfig             `json:"config"`
	Turns          []TurnRecord          `json:"turns"`
	FinalOutcome   RunTrace              `json:"finalOutcome"`
	CommandOutputs []CommandOutputRecord `json:"commandOutputs,omitempty"`
	// SkippedLines counts trace lines the reader dropped for exceeding
	// its per-line cap. A non-zero value marks the recording as partial.
	SkippedLines int               `json:"skippedLines,omitempty"`
	SubRuns      []SubRunRecording `json:"subRuns,omitempty"`
	// HarnessVersion is the harness build that wrote the trace; empty for
	// traces written before it was recorded.
	HarnessVersion string `json:"harnessVersion,omitempty"`
	// SystemPrompt is the scrubbed system prompt the run's requests
	// carried, and SystemPromptSHA256 the hex digest of the prompt as it
	// was sent, before scrubbing. Both are empty when the trace holds no
	// system_prompt record.
	SystemPrompt       string `json:"systemPrompt,omitempty"`
	SystemPromptSHA256 string `json:"systemPromptSha256,omitempty"`
}

// SubRunRecording is one sub-agent run reassembled from the parent's
// trace. Summary is nil when the trace ends before the child finished.
type SubRunRecording struct {
	RunID       string         `json:"runId"`
	ParentRunID string         `json:"parentRunId,omitempty"`
	Config      RunConfig      `json:"config"`
	Turns       []TurnRecord   `json:"turns"`
	Summary     *SubRunSummary `json:"summary,omitempty"`
}

// SubRunSummary is the outcome of a sub-agent run, streamed on the
// parent's trace when the child finishes.
type SubRunSummary struct {
	RunID       string     `json:"runId"`
	ParentRunID string     `json:"parentRunId,omitempty"`
	Outcome     string     `json:"outcome"`
	Turns       int        `json:"turns"`
	TokenUsage  TokenUsage `json:"tokenUsage"`
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt time.Time  `json:"completedAt"`
	DurationMs  int64      `json:"durationMs"`
}

// BudgetCheck holds the result of a token budget check.
type BudgetCheck struct {
	WithinBudget  bool       `json:"withinBudget"`
	CurrentTokens TokenUsage `json:"currentTokens"`
	Reason        string     `json:"reason,omitempty"`
}
