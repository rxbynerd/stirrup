package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// JSONLTraceEmitter writes a streaming, line-delimited JSON event log
// for a single harness run. Each call to Start, RecordTurnRecord,
// RecordToolCall (via the inline tool_call_record path), and Finish
// emits one line. Lines are flushed immediately so a SIGKILL leaves a
// parseable partial recording on disk up to the last completed event.
//
// The on-wire shape is documented in events.go. Defence-in-depth
// scrubbing runs over modelOutput, tool call input, and tool call
// output before each line is written; the line never contains raw
// secret material even if the upstream LogScrubber in the agentic loop
// missed a substring.
//
// Concurrency: every public method takes a write lock around the
// underlying io.Writer. The lock is held only for the duration of a
// single Marshal+Write to keep the loop's hot path responsive.
type JSONLTraceEmitter struct {
	writer io.Writer
	closer io.Closer

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
	finalAssistantText   string
	commandOutputArchive string
}

var _ FinalAssistantTextRecorder = (*JSONLTraceEmitter)(nil)
var _ CommandOutputRecorder = (*JSONLTraceEmitter)(nil)
var _ CommandOutputArchiveRecorder = (*JSONLTraceEmitter)(nil)

// NewJSONLTraceEmitter creates a streaming trace emitter that writes to w.
// If w implements io.Closer, Close on the emitter closes it.
func NewJSONLTraceEmitter(w io.Writer) *JSONLTraceEmitter {
	emitter := &JSONLTraceEmitter{writer: w}
	if closer, ok := w.(io.Closer); ok {
		emitter.closer = closer
	}
	return emitter
}

// writeLineLocked marshals e as a single newline-terminated JSON line
// and writes it to the backing writer. Must be called with e.mu held.
//
// Errors from the writer are surfaced to the caller. The emitter has no
// retry / buffering policy: when the file handle is a regular file (the
// production case via factory.go), os.File.Write is effectively atomic
// for the line-sized writes used here and a write failure is unusual.
// When a write does fail the emitter does NOT mutate its in-memory
// accumulators (the line was never observable on disk; the next event
// can still land), and the run's outer error path surfaces the failure.
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
// appear on disk even if a future config field exposes a raw value.
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
	e.finalAssistantText = ""
	e.commandOutputArchive = ""

	startedAt := e.startedAt
	var redacted types.RunConfig
	if config != nil {
		redacted = config.Redact()
	}
	ev := Event{
		Kind:          EventKindRunStarted,
		SchemaVersion: CurrentSchemaVersion,
		RunID:         runID,
		StartedAt:     &startedAt,
		Config:        &redacted,
	}
	// A failure to write the run_started line is non-fatal: the trace
	// will be missing its preamble but the rest of the run still
	// produces records, and Finish surfaces a synthetic run_finished if
	// the writer recovers. The agentic loop has no error channel from
	// Start (the interface returns no error to preserve the existing
	// contract); failure here is rare in practice and the run-level
	// outcome is decided independently of trace durability.
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

	scrubbed := scrubTurnRecord(turn)
	ev := Event{
		Kind:        EventKindTurnRecord,
		Turn:        scrubbed.Turn,
		ModelInput:  &scrubbed.ModelInput,
		ModelOutput: scrubbed.ModelOutput,
		ToolCalls:   scrubbed.ToolCalls,
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
	// Streaming event carries only the summary fields; the full raw
	// input/output is attached at RecordTurnRecord time when the
	// turn's complete tool-call sequence is known. We emit a lean
	// tool_call_record here so live consumers see per-call events
	// before the turn finishes.
	tc := types.ToolCallRecord{
		ID:      call.ID,
		Name:    call.Name,
		Success: call.Success,
		// DurationMs and other counters live on call; for the
		// inline event we keep the schema aligned with the turn-
		// embedded ToolCallRecord shape and surface only fields
		// that are meaningful at this point. Output carries
		// ErrorReason (already scrubbed by the dispatch site) for
		// failed calls so a live consumer sees the failure detail.
		Output:     call.ErrorReason,
		DurationMs: call.DurationMs,
	}
	ev := Event{
		Kind:     EventKindToolCallRecord,
		ToolCall: &tc,
	}
	_ = e.writeLineLocked(ev)
}

// RecordPermissionDenial increments the run-level permission denial count.
func (e *JSONLTraceEmitter) RecordPermissionDenial() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.permissionDenials++
}

// RecordHookExecution appends a lifecycle hook result (issue #461) to
// the in-memory accumulator AND streams an inline hook_record event,
// mirroring RecordToolCall's tool_call_record. OutputTail, Error, and
// Command are all re-scrubbed here as defence-in-depth — the same
// posture RecordTurnRecord applies to tool output — even though
// hook.ExecRunner already scrubs OutputTail before returning the
// types.HookExecution and ValidateRunConfig structurally rejects a
// "secret://" reference in Command before a hook is ever allowed to
// run. Command is scrubbed too rather than trusted verbatim: the
// persisted trace must not depend on that upstream guard being
// airtight (e.g. a RunConfig assembled without going through
// validation).
func (e *JSONLTraceEmitter) RecordHookExecution(exec types.HookExecution) {
	e.mu.Lock()
	defer e.mu.Unlock()

	scrubbed := exec
	scrubbed.Command = security.Scrub(exec.Command)
	scrubbed.OutputTail = security.Scrub(exec.OutputTail)
	scrubbed.Error = security.Scrub(exec.Error)

	e.hookResults = append(e.hookResults, scrubbed)

	ev := Event{
		Kind: EventKindHookRecord,
		Hook: &scrubbed,
	}
	_ = e.writeLineLocked(ev)
}

// RecordFinalAssistantText stores the run's final assistant text so the
// run_finished event's embedded RunTrace carries it. The loop forwards a
// value already scrubbed and gated by the PhasePostTurn guard.
func (e *JSONLTraceEmitter) RecordFinalAssistantText(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finalAssistantText = text
}

func (e *JSONLTraceEmitter) RecordCommandOutput(record types.CommandOutputRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	copyRecord := record
	_ = e.writeLineLocked(Event{Kind: EventKindCommandOutputRecord, CommandOutput: &copyRecord})
}

func (e *JSONLTraceEmitter) RecordCommandOutputArchive(location string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commandOutputArchive = location
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
		totalTokens.Input += turn.Tokens.Input
		totalTokens.Output += turn.Tokens.Output
	}

	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if e.config != nil {
		redactedConfig = e.config.Redact()
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
		Outcome:              outcome,
		FinalAssistantText:   e.finalAssistantText,
		HookResults:          e.hookResults,
		CommandOutputArchive: e.commandOutputArchive,
	}

	ev := Event{
		Kind:        EventKindRunFinished,
		CompletedAt: &now,
		Trace:       trace,
	}
	if err := e.writeLineLocked(ev); err != nil {
		return nil, err
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
// can carry untrusted content. The defence-in-depth contract: a future
// regression that bypasses the upstream LogScrubber must not leak raw
// secret material into a persisted trace.
//
// Scrubbing surfaces:
//   - turn.ModelInput.Messages[*].Content: text blocks (Text), tool_use
//     Input, and tool_result Content — the message-history route. The
//     same tool output reaches the trace twice: here as the tool_result
//     block's Content on the NEXT turn's input, and below as
//     ToolCalls[*].Output on the turn that ran the tool. Both routes
//     are scrubbed independently.
//   - turn.ModelOutput[*].Text and ToolUseId/Name carriers' inputs.
//   - turn.ToolCalls[*].Input (raw JSON; scrubbed as a string then
//     re-parsed; if the scrubbed string is not valid JSON it is
//     wrapped as a JSON string literal so the on-disk shape stays a
//     valid json.RawMessage).
//   - turn.ToolCalls[*].Output (string).
//   - turn.ToolCalls[*].Structured (raw JSON; scrubbed via scrubRawJSON like
//     the tool-call Input, since the structured envelope carries the same
//     untrusted content as Output).
//
// ContentBlock.ThoughtSignature is not scrubbed but dropped entirely —
// it is provider-opaque and must not be persisted; see scrubContentBlocks.
func scrubTurnRecord(t types.TurnRecord) types.TurnRecord {
	out := types.TurnRecord{
		Turn:        t.Turn,
		ModelInput:  scrubModelInput(t.ModelInput),
		ModelOutput: scrubContentBlocks(t.ModelOutput),
		ToolCalls:   make([]types.ToolCallRecord, len(t.ToolCalls)),
		RunID:       t.RunID,
		ParentRunID: t.ParentRunID,
	}
	for i, tc := range t.ToolCalls {
		out.ToolCalls[i] = types.ToolCallRecord{
			ID:           tc.ID,
			Name:         tc.Name,
			InternalName: tc.InternalName,
			Input:        scrubRawJSON(tc.Input),
			Output:       security.Scrub(tc.Output),
			DurationMs:   tc.DurationMs,
			Success:      tc.Success,
			// The structured payload (issue #231) carries the same
			// untrusted content as Output — a command transcript or file
			// excerpt that can hold credentials — so it is scrubbed on the
			// same footing. scrubRawJSON preserves a valid json.RawMessage
			// shape on disk even when a secret straddles a JSON token.
			Structured: scrubRawJSON(tc.Structured),
			Kind:       tc.Kind,
			IsError:    tc.IsError,
		}
	}
	return out
}

// scrubModelInput deep-copies a ModelInput with text content scrubbed.
// Tool definitions and the model string are not scrubbed: tool schemas
// are constants compiled into the harness, and the model identifier
// (e.g. "claude-3-5-sonnet-latest") is not a secret.
//
// Message.ReplayFields is deliberately absent from the rebuilt Message:
// like ContentBlock.ThoughtSignature it is provider-opaque round-trip
// state the harness must never persist verbatim (see the field contract
// in types/messages.go), and the scrubber cannot inspect an opaque
// value. The explicit field list below is the drop mechanism — a future
// edit that copies the source Message wholesale would regress this.
func scrubModelInput(in types.ModelInput) types.ModelInput {
	out := types.ModelInput{
		Model:    in.Model,
		Tools:    in.Tools,
		Messages: make([]types.Message, len(in.Messages)),
	}
	for i, m := range in.Messages {
		out.Messages[i] = types.Message{
			Role:      m.Role,
			Synthetic: m.Synthetic,
			Content:   scrubContentBlocks(m.Content),
		}
	}
	return out
}

// scrubContentBlocks scrubs text and tool-result content. Tool-use
// blocks carry an Input json.RawMessage that may itself encode arbitrary
// content; we re-use scrubRawJSON to preserve a valid JSON shape.
//
// The Structured field on a tool_result block carries the issue #231 typed
// envelope and, for MCP-derived results, untrusted server output. It is
// scrubbed on the same footing as Input via scrubRawJSON so a secret-shaped
// substring in a structured payload (built-in OR MCP) cannot reach the
// persisted trace unscrubbed. This mirrors the ToolCallRecord.Structured
// scrub in scrubTurnRecord; both surfaces are scrubbed because the same
// payload reaches the trace via two routes (the turn record AND the next
// turn's model input message history).
func scrubContentBlocks(blocks []types.ContentBlock) []types.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]types.ContentBlock, len(blocks))
	for i, b := range blocks {
		nb := b
		// ThoughtSignature is provider-opaque state (Gemini's encrypted
		// chain-of-thought blob) the harness must round-trip on the live
		// message history but must never log or persist verbatim — see
		// the field contract in types/messages.go. It is opaque by
		// definition, so the scrubber cannot inspect it; the persisted
		// copy drops it outright. The live blocks the loop holds are
		// untouched.
		nb.ThoughtSignature = ""
		if nb.Text != "" {
			nb.Text = security.Scrub(nb.Text)
		}
		// Content is the tool_result text rendering — command output,
		// file excerpts, MCP server responses — exactly the untrusted
		// surface the ToolCallRecord.Output scrub covers on its other
		// route into the trace. Found via the OTel content-capture
		// scrub test (#413): this field previously reached the
		// defence-in-depth layer unscrubbed.
		if nb.Content != "" {
			nb.Content = security.Scrub(nb.Content)
		}
		if len(nb.Input) > 0 {
			nb.Input = scrubRawJSON(nb.Input)
		}
		if len(nb.Structured) > 0 {
			nb.Structured = scrubRawJSON(nb.Structured)
		}
		out[i] = nb
	}
	return out
}

// scrubRawJSON scrubs secret-shaped substrings out of a json.RawMessage
// while preserving JSON validity. The scrubber operates on the string
// form of the message and re-validates the result. If the scrubbed
// payload is no longer valid JSON (an extreme corner case: a regex
// boundary breaks a JSON literal), the entire payload is replaced by a
// JSON string literal carrying the scrubbed text so the on-disk shape
// stays parseable.
//
// Assumes the caller produced the json.RawMessage with encoding/json's default
// (non-HTML-escaping) marshaller. An HTML-escaping encoder (json.HTMLEscape, or
// json.Encoder without SetEscapeHTML(false)) would emit `<`, `>`, `&` and
// U+2028/U+2029 as \uXXXX sequences in the raw byte stream, which can cause a
// secret regex anchored on those characters to miss. Do not pipe HTMLEscape
// output through this function.
func scrubRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
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
		// json.Marshal on a string cannot fail in practice, so this branch
		// is effectively dead. Warn if it is ever reached: it would mean an
		// invariant about encoding/json broke, and the empty-object fallback
		// silently discards the scrubbed tool payload from the recording.
		slog.Default().Warn("scrubRawJSON string marshal failed; dropping payload to empty object",
			"error", err)
		// Fall back to a defensive empty-object literal so the line stays valid.
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(wrapped)
}
