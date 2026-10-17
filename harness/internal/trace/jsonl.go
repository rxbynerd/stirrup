package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

// JSONLTraceEmitter writes a streaming, line-delimited JSON event log
// for a single harness run. Each call to Start, RecordTurnRecord,
// RecordToolCall (via the inline tool_call_record path), and Finish
// emits one line, flushed immediately so a SIGKILL leaves a parseable
// partial recording on disk. The on-wire shape is documented in
// events.go. Defence-in-depth scrubbing runs over message content and
// tool I/O before each line is written, independent of the upstream
// LogScrubber in the agentic loop.
//
// Concurrency: every public method takes a write lock around the
// underlying io.Writer, held only for a single Marshal+Write.
type JSONLTraceEmitter struct {
	writer io.Writer
	closer io.Closer

	// redactionDisabled is the --debug bit, immutable after construction.
	// When true every scrub this emitter would make is bypassed, so the
	// persisted trace carries unredacted secrets. Set only by the factory,
	// and only under a debug build. See docs/security.md#debug-builds.
	redactionDisabled bool

	mu        sync.Mutex
	runID     string
	config    *types.RunConfig
	startedAt time.Time

	// Accumulators retained for the run_finished summary. The streaming
	// event log carries the full transcript, but Finish still produces a
	// canonical RunTrace (token totals, tool call summaries, outcome)
	// for the in-process caller (harness factory, eval runner) that
	// reads it directly without re-parsing the file.
	turns                []types.TurnTrace
	toolCalls            []types.ToolCallTrace
	permissionDenials    int
	hookResults          []types.HookExecution
	verificationResults  []types.VerificationResult
	finalAssistantText   string
	stopDetails          *types.StopDetails
	commandOutputArchive string
	gitHandoff           *types.GitHandoff
	workspaceExport      *types.ExportReceipt
	runError             string
	structuredOutput     json.RawMessage
	degradedCapabilities []types.DegradedCapability
	repositoryGuidance   []types.RepositoryGuidanceRecord
}

var _ FinalAssistantTextRecorder = (*JSONLTraceEmitter)(nil)
var _ VerificationRecorder = (*JSONLTraceEmitter)(nil)
var _ RepositoryGuidanceRecorder = (*JSONLTraceEmitter)(nil)
var _ StopDetailsRecorder = (*JSONLTraceEmitter)(nil)
var _ DegradedCapabilitiesRecorder = (*JSONLTraceEmitter)(nil)
var _ CommandOutputRecorder = (*JSONLTraceEmitter)(nil)
var _ CommandOutputArchiveRecorder = (*JSONLTraceEmitter)(nil)
var _ GitHandoffRecorder = (*JSONLTraceEmitter)(nil)
var _ WorkspaceExportRecorder = (*JSONLTraceEmitter)(nil)
var _ RunErrorRecorder = (*JSONLTraceEmitter)(nil)
var _ StructuredOutputRecorder = (*JSONLTraceEmitter)(nil)
var _ SubRunRecorder = (*JSONLTraceEmitter)(nil)
var _ SystemInstructionsRecorder = (*JSONLTraceEmitter)(nil)

// NewJSONLTraceEmitter creates a streaming trace emitter that writes to w.
// If w implements io.Closer, Close on the emitter closes it.
//
// redactionDisabled is the --debug bit; see the field docstring.
// Production callers pass the factory's already-gated value; every other
// caller passes false.
func NewJSONLTraceEmitter(w io.Writer, redactionDisabled bool) *JSONLTraceEmitter {
	emitter := &JSONLTraceEmitter{writer: w, redactionDisabled: redactionDisabled}
	if closer, ok := w.(io.Closer); ok {
		emitter.closer = closer
	}
	return emitter
}

// writeLineLocked marshals ev as a single newline-terminated JSON line
// and writes it to the backing writer. Must be called with e.mu held.
//
// The emitter has no retry/buffering policy. On write failure the
// emitter does not mutate its in-memory accumulators — the next event
// can still land — and the run's outer error path surfaces the failure.
func (e *JSONLTraceEmitter) writeLineLocked(ev Event) error {
	line, err := MarshalLine(ev)
	if err != nil {
		return fmt.Errorf("marshal trace event: %w", err)
	}
	if _, err := e.writer.Write(line); err != nil {
		return fmt.Errorf("write trace event: %w", err)
	}
	return nil
}

// Start initialises the trace and writes the run_started event. The
// config is redacted via RunConfig.Redact() so secret references never
// appear on disk even if a future config field exposes a raw value —
// unless redactionDisabled (--debug) is set, in which case the raw,
// unredacted config is written verbatim.
func (e *JSONLTraceEmitter) Start(runID string, config *types.RunConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.runID = runID
	e.config = config
	e.startedAt = time.Now()
	e.turns = nil
	e.toolCalls = nil
	e.permissionDenials = 0
	e.hookResults = nil
	e.verificationResults = nil
	e.finalAssistantText = ""
	e.stopDetails = nil
	e.degradedCapabilities = nil
	e.commandOutputArchive = ""
	e.gitHandoff = nil
	e.workspaceExport = nil
	e.runError = ""
	e.structuredOutput = nil
	e.repositoryGuidance = nil

	startedAt := e.startedAt
	var redacted types.RunConfig
	if config != nil {
		redacted = e.redactConfig(config)
	}
	ev := Event{
		Kind:           EventKindRunStarted,
		SchemaVersion:  CurrentSchemaVersion,
		RunID:          runID,
		StartedAt:      &startedAt,
		Config:         &redacted,
		HarnessVersion: version.Full(),
	}
	// A failure to write the run_started line is non-fatal: the trace
	// will be missing its preamble but the rest of the run still
	// produces records. Start has no error channel to preserve the
	// existing interface contract.
	_ = e.writeLineLocked(ev)
}

// RecordSystemInstructions writes the run's system prompt as a
// system_prompt line: the scrubbed text and the digest of the prompt as
// sent. The prompt is scrubbed like any other recorded content, so a
// secret-shaped string never reaches the file; with redaction disabled
// (--debug) it is written verbatim.
func (e *JSONLTraceEmitter) RecordSystemInstructions(system string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	sum := sha256.Sum256([]byte(system))
	ev := Event{
		Kind:               EventKindSystemPrompt,
		RunID:              e.runID,
		SystemPrompt:       scrubOrPass(system, e.redactionDisabled),
		SystemPromptSHA256: hex.EncodeToString(sum[:]),
	}
	_ = e.writeLineLocked(ev)
}

// RecordTurn appends a turn summary to the in-memory accumulator. The
// summary is written to disk as part of the run_finished event's
// embedded RunTrace; per-turn detail belongs in RecordTurnRecord.
func (e *JSONLTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = append(e.turns, turn)
}

// RecordTurnRecord scrubs the transcript and writes one turn_record
// line. The scrubber runs over message content, model output blocks,
// and each tool call's raw input/output. The line is flushed before
// RecordTurnRecord returns so an immediately-following SIGKILL still
// leaves a parseable record on disk.
func (e *JSONLTraceEmitter) RecordTurnRecord(turn types.TurnRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()

	scrubbed := scrubTurnRecord(turn, e.redactionDisabled)
	ev := Event{
		Kind:                EventKindTurnRecord,
		RunID:               e.runIDLocked(scrubbed.RunID),
		ParentRunID:         scrubbed.ParentRunID,
		Turn:                scrubbed.Turn,
		ModelInput:          &scrubbed.ModelInput,
		ModelOutput:         scrubbed.ModelOutput,
		ToolCalls:           scrubbed.ToolCalls,
		ResponseModel:       scrubbed.ResponseModel,
		OutputLimitRecovery: scrubbed.OutputLimitRecovery,
		StallNudge:          scrubbed.StallNudge,
		WindDown:            scrubbed.WindDown,
	}
	_ = e.writeLineLocked(ev)
}

// RecordToolCall appends a tool call summary to the in-memory
// accumulator AND streams an inline tool_call_record event. The
// summary feeds the run_finished aggregate; the streamed event lets a
// live consumer (or a SIGKILL-resilient post-hoc reader) see calls as
// they land. The summary carries counters only — raw I/O is captured
// at RecordTurnRecord time on the enclosing turn.
func (e *JSONLTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, call)
	// Output carries ErrorReason (already scrubbed at the dispatch
	// site) so a live consumer sees the failure detail for failed calls.
	tc := types.ToolCallRecord{
		ID:      call.ID,
		Name:    call.Name,
		Success: call.Success,

		Output:     call.ErrorReason,
		DurationMs: call.DurationMs,
	}
	ev := Event{
		Kind:        EventKindToolCallRecord,
		RunID:       e.runIDLocked(call.RunID),
		ParentRunID: call.ParentRunID,
		ToolCall:    &tc,
	}
	_ = e.writeLineLocked(ev)
}

// RecordPermissionDenial increments the run-level permission denial count.
func (e *JSONLTraceEmitter) RecordPermissionDenial() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.permissionDenials++
}

// RecordHookExecution appends a lifecycle hook result to the in-memory
// accumulator AND streams an inline hook_record event, mirroring
// RecordToolCall's tool_call_record. OutputTail, Error, and Command are
// all re-scrubbed here as defence-in-depth — the persisted trace must
// not depend on upstream scrubbing/validation guards being airtight.
func (e *JSONLTraceEmitter) RecordHookExecution(exec types.HookExecution) {
	e.mu.Lock()
	defer e.mu.Unlock()

	scrubbed := exec
	scrubbed.Command = scrubOrPass(exec.Command, e.redactionDisabled)
	scrubbed.OutputTail = scrubOrPass(exec.OutputTail, e.redactionDisabled)
	scrubbed.Error = scrubOrPass(exec.Error, e.redactionDisabled)

	e.hookResults = append(e.hookResults, scrubbed)

	ev := Event{
		Kind:  EventKindHookRecord,
		RunID: e.runID,
		Hook:  &scrubbed,
	}
	_ = e.writeLineLocked(ev)
}

// RecordVerification appends a verifier attempt for the run_finished
// event's embedded RunTrace.
func (e *JSONLTraceEmitter) RecordVerification(result types.VerificationResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.verificationResults = append(e.verificationResults, result)
}

// RecordFinalAssistantText stores the run's final assistant text so the
// run_finished event's embedded RunTrace carries it. The loop forwards a
// value already scrubbed and gated by the PhasePostTurn guard.
func (e *JSONLTraceEmitter) RecordFinalAssistantText(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finalAssistantText = text
}

// RecordStopDetails stores the run's final stop details so the
// run_finished event's embedded RunTrace carries them.
func (e *JSONLTraceEmitter) RecordStopDetails(details *types.StopDetails) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopDetails = details
}

// RecordDegradedCapabilities stores the capabilities the run proceeded
// without so the run_finished event's embedded RunTrace carries them.
func (e *JSONLTraceEmitter) RecordDegradedCapabilities(degraded []types.DegradedCapability) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.degradedCapabilities = append([]types.DegradedCapability(nil), degraded...)
}

func (e *JSONLTraceEmitter) RecordCommandOutput(record types.CommandOutputRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	copyRecord := record
	_ = e.writeLineLocked(Event{
		Kind:          EventKindCommandOutputRecord,
		RunID:         e.runIDLocked(record.RunID),
		ParentRunID:   record.ParentRunID,
		CommandOutput: &copyRecord,
	})
}

// RecordSubRunStarted writes a subrun_started line carrying the child's
// config, redacted under the same rule as Start.
func (e *JSONLTraceEmitter) RecordSubRunStarted(runID, parentRunID string, startedAt time.Time, config *types.RunConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ev := Event{
		Kind:        EventKindSubRunStarted,
		RunID:       runID,
		ParentRunID: parentRunID,
		StartedAt:   &startedAt,
	}
	if config != nil {
		redacted := e.redactConfig(config)
		ev.Config = &redacted
	}
	_ = e.writeLineLocked(ev)
}

// RecordSubRunFinished writes a subrun_finished line carrying the
// child's outcome summary.
func (e *JSONLTraceEmitter) RecordSubRunFinished(summary types.SubRunSummary) {
	e.mu.Lock()
	defer e.mu.Unlock()

	completedAt := summary.CompletedAt
	_ = e.writeLineLocked(Event{
		Kind:        EventKindSubRunFinished,
		RunID:       summary.RunID,
		ParentRunID: summary.ParentRunID,
		CompletedAt: &completedAt,
		SubRun:      &summary,
	})
}

// runIDLocked returns the run ID a forwarded record names, or this
// emitter's own run ID for a record produced by the run itself. Must be
// called with e.mu held.
func (e *JSONLTraceEmitter) runIDLocked(recordRunID string) string {
	if recordRunID != "" {
		return recordRunID
	}
	return e.runID
}

func (e *JSONLTraceEmitter) RecordCommandOutputArchive(location string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commandOutputArchive = location
}

// RecordGitHandoff stores the run's git handoff for the RunTrace Finish
// builds.
func (e *JSONLTraceEmitter) RecordGitHandoff(handoff types.GitHandoff) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gitHandoff = &handoff
}

// RecordWorkspaceExport stores the run's workspace export receipt for the
// RunTrace Finish builds.
func (e *JSONLTraceEmitter) RecordWorkspaceExport(receipt types.ExportReceipt) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workspaceExport = &receipt
}

// RecordRunError stores the run's error text for the RunTrace Finish
// builds.
func (e *JSONLTraceEmitter) RecordRunError(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runError = text
}

// RecordStructuredOutput stores the run's validated structured answer for
// the RunTrace Finish builds.
func (e *JSONLTraceEmitter) RecordStructuredOutput(doc json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.structuredOutput = append(json.RawMessage(nil), doc...)
}

// RecordRepositoryGuidance stores the guidance records so the run_finished
// event's embedded RunTrace carries them.
func (e *JSONLTraceEmitter) RecordRepositoryGuidance(records []types.RepositoryGuidanceRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repositoryGuidance = append([]types.RepositoryGuidanceRecord(nil), records...)
}

// Finish builds the canonical RunTrace summary, writes the
// run_finished event, and returns the summary. A run with zero turns
// still produces a valid run_finished line.
func (e *JSONLTraceEmitter) Finish(_ context.Context, outcome string) (*types.RunTrace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	var totalTokens types.TokenUsage
	for _, turn := range e.turns {
		totalTokens.Add(turn.Tokens)
	}

	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if e.config != nil {
		if e.redactionDisabled {
			redactedConfig = *e.config
		} else {
			redactedConfig = e.config.Redact()
		}
	}

	trace := &types.RunTrace{
		ID:                   e.runID,
		Config:               redactedConfig,
		StartedAt:            e.startedAt,
		CompletedAt:          now,
		Turns:                len(e.turns),
		TokenUsage:           totalTokens,
		ToolCalls:            summaries,
		PermissionDenials:    e.permissionDenials,
		VerificationResults:  e.verificationResults,
		Outcome:              outcome,
		StopDetails:          e.stopDetails,
		FinalAssistantText:   e.finalAssistantText,
		HookResults:          e.hookResults,
		CommandOutputArchive: e.commandOutputArchive,
		Git:                  e.gitHandoff,
		WorkspaceExport:      e.workspaceExport,
		Error:                e.runError,
		StructuredOutput:     e.structuredOutput,
		DegradedCapabilities: e.degradedCapabilities,
		RepositoryGuidance:   e.repositoryGuidance,
	}

	ev := Event{
		Kind:        EventKindRunFinished,
		CompletedAt: &now,
		Trace:       trace,
	}
	if err := e.writeLineLocked(ev); err != nil {
		return trace, err
	}
	return trace, nil
}

// Close releases the backing writer when it owns a closable resource
// (e.g. an os.File opened by the factory). NewReader-backed and
// in-memory writers report nil.
func (e *JSONLTraceEmitter) Close() error {
	if e.closer == nil {
		return nil
	}
	return e.closer.Close()
}

// scrubTurnRecord returns a deep-copied TurnRecord with secret-shaped
// substrings replaced by [REDACTED] in every string-valued field that
// can carry untrusted content: message text/tool_use/tool_result
// content (see scrubContentBlocks), ModelOutput text, and each
// ToolCalls[*] Input/Output/Structured. The same tool output reaches
// the trace twice — as ToolCalls[*].Output and as the next turn's
// tool_result Content — and both routes are scrubbed independently.
//
// redactionDisabled is the --debug bypass: it skips every scrub below.
// ContentBlock.ThoughtSignature is dropped rather than scrubbed and is
// not gated by it — that drop is a provider-opacity invariant, not a
// secret-redaction one, so a debug build still never persists it.
func scrubTurnRecord(t types.TurnRecord, redactionDisabled bool) types.TurnRecord {
	out := types.TurnRecord{
		Turn:                t.Turn,
		ModelInput:          scrubModelInput(t.ModelInput, redactionDisabled),
		ModelOutput:         scrubContentBlocks(t.ModelOutput, redactionDisabled),
		ToolCalls:           make([]types.ToolCallRecord, len(t.ToolCalls)),
		RunID:               t.RunID,
		ParentRunID:         t.ParentRunID,
		ResponseModel:       scrubOrPass(t.ResponseModel, redactionDisabled),
		OutputLimitRecovery: t.OutputLimitRecovery,
		StallNudge:          t.StallNudge,
		WindDown:            t.WindDown,
	}
	for i, tc := range t.ToolCalls {
		out.ToolCalls[i] = scrubToolCallRecord(tc, redactionDisabled)
	}
	return out
}

// scrubToolCallRecord returns a copy of tc with its input, output and
// structured payload scrubbed.
func scrubToolCallRecord(tc types.ToolCallRecord, redactionDisabled bool) types.ToolCallRecord {
	return types.ToolCallRecord{
		ID:           tc.ID,
		Name:         tc.Name,
		InternalName: tc.InternalName,
		Input:        scrubRawJSON(tc.Input, redactionDisabled),
		Output:       scrubOrPass(tc.Output, redactionDisabled),
		DurationMs:   tc.DurationMs,
		Success:      tc.Success,
		// The structured payload carries the same untrusted content as
		// Output, so it is scrubbed on the same footing.
		Structured: scrubRawJSON(tc.Structured, redactionDisabled),
		Kind:       tc.Kind,
		IsError:    tc.IsError,
	}
}

// scrubModelInput deep-copies a ModelInput with text content scrubbed.
// Tool definitions and the model string are not scrubbed: they are not
// secrets. Message.ReplayFields is deliberately absent from the rebuilt
// Message — like ContentBlock.ThoughtSignature it is provider-opaque
// round-trip state that must never be persisted (types/messages.go);
// the explicit field list below is the drop mechanism, so a future edit
// that copies the source Message wholesale would regress this.
func scrubModelInput(in types.ModelInput, redactionDisabled bool) types.ModelInput {
	out := types.ModelInput{
		Model:    in.Model,
		Tools:    in.Tools,
		Request:  in.Request,
		Messages: make([]types.Message, len(in.Messages)),
	}
	for i, m := range in.Messages {
		out.Messages[i] = types.Message{
			Role:      m.Role,
			Synthetic: m.Synthetic,
			Origin:    m.Origin,
			Content:   scrubContentBlocks(m.Content, redactionDisabled),
		}
	}
	return out
}

// scrubContentBlocks scrubs text and tool-result content. Tool-use
// blocks carry an Input json.RawMessage that may itself encode arbitrary
// content; scrubRawJSON preserves a valid JSON shape. The Structured
// field on a tool_result block carries untrusted output (including
// MCP-derived) and is scrubbed the same way, mirroring the
// ToolCallRecord.Structured scrub in scrubTurnRecord — the same payload
// reaches the trace via both routes.
func scrubContentBlocks(blocks []types.ContentBlock, redactionDisabled bool) []types.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]types.ContentBlock, len(blocks))
	for i, b := range blocks {
		nb := b
		// ThoughtSignature is provider-opaque state (e.g. Gemini's
		// encrypted chain-of-thought blob) that must never be persisted
		// (types/messages.go); the persisted copy drops it outright,
		// leaving the loop's live blocks untouched. Dropped
		// unconditionally — this is a provider-opacity invariant, not a
		// secret-redaction one, so redactionDisabled does not gate it.
		nb.ThoughtSignature = ""
		if nb.Text != "" {
			nb.Text = scrubOrPass(nb.Text, redactionDisabled)
		}
		// Content is the tool_result text rendering — command output,
		// file excerpts, MCP server responses — the same untrusted
		// surface the ToolCallRecord.Output scrub covers on its other
		// route into the trace.
		if nb.Content != "" {
			nb.Content = scrubOrPass(nb.Content, redactionDisabled)
		}
		if len(nb.Input) > 0 {
			nb.Input = scrubRawJSON(nb.Input, redactionDisabled)
		}
		if len(nb.Structured) > 0 {
			nb.Structured = scrubRawJSON(nb.Structured, redactionDisabled)
		}
		out[i] = nb
	}
	return out
}

// scrubRawJSON scrubs secret-shaped substrings out of a json.RawMessage
// while preserving JSON validity. If the scrubbed payload is no longer
// valid JSON (a regex boundary breaking a JSON literal), the entire
// payload is replaced by a JSON string literal carrying the scrubbed
// text so the on-disk shape stays parseable.
//
// Assumes the caller produced the json.RawMessage with encoding/json's
// default (non-HTML-escaping) marshaller — an HTML-escaping encoder
// would emit `<`, `>`, `&` as \uXXXX sequences that can cause a secret
// regex to miss. Do not pipe HTMLEscape output through this function.
//
// redactionDisabled is checked here rather than at each call site so
// every caller gets the --debug bypass uniformly.
func scrubRawJSON(raw json.RawMessage, redactionDisabled bool) json.RawMessage {
	if redactionDisabled || len(raw) == 0 {
		return raw
	}
	scrubbed := security.Scrub(string(raw))
	if scrubbed == string(raw) {
		return raw
	}
	if json.Valid([]byte(scrubbed)) {
		return json.RawMessage(scrubbed)
	}
	// Wrap the scrubbed text as a JSON string. json.Marshal escapes
	// any embedded quotes/backslashes so the result is always valid.
	wrapped, err := json.Marshal(scrubbed)
	if err != nil {
		// json.Marshal on a string cannot fail in practice; this branch is
		// effectively dead. Fall back to a defensive empty-object literal
		// so the line stays valid JSON.
		slog.Default().Warn("scrubRawJSON string marshal failed; dropping payload to empty object",
			"error", err)

		return json.RawMessage(`{}`)
	}
	return json.RawMessage(wrapped)
}

// redactConfig returns config as persisted: secret references stripped and
// the prompt scrubbed, since it can carry agent-authored text. A debug
// build keeps the config verbatim.
func (e *JSONLTraceEmitter) redactConfig(config *types.RunConfig) types.RunConfig {
	if e.redactionDisabled {
		return *config
	}
	redacted := config.Redact()
	redacted.Prompt = security.Scrub(config.Prompt)
	return redacted
}

// scrubOrPass returns security.Scrub(s), or s verbatim under the --debug
// bypass. Centralised so every scrub call site here and in otel.go reads
// identically rather than hand-rolling the conditional.
func scrubOrPass(s string, redactionDisabled bool) string {
	if redactionDisabled {
		return s
	}
	return security.Scrub(s)
}
