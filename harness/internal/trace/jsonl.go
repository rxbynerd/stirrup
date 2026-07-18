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

	mu        sync.Mutex
	runID     string
	config    *types.RunConfig
	startedAt time.Time

	// Accumulators retained for the run_finished summary. The streaming
	// event log carries the full transcript, but Finish still produces a
	// canonical RunTrace (token totals, tool call summaries, outcome)
	// for the in-process caller (harness factory, eval runner) that
	// reads it directly without re-parsing the file.
	turns              []types.TurnTrace
	toolCalls          []types.ToolCallTrace
	permissionDenials  int
	hookResults        []types.HookExecution
	finalAssistantText string
}

var _ FinalAssistantTextRecorder = (*JSONLTraceEmitter)(nil)

// NewJSONLTraceEmitter creates a streaming trace emitter that writes to w.
// If w implements io.Closer, Close on the emitter closes it.
func NewJSONLTraceEmitter(w io.Writer) *JSONLTraceEmitter {
	emitter := &JSONLTraceEmitter{writer: w}
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
	// produces records. Start has no error channel to preserve the
	// existing interface contract.
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

// RecordHookExecution appends a lifecycle hook result to the in-memory
// accumulator AND streams an inline hook_record event, mirroring
// RecordToolCall's tool_call_record. OutputTail, Error, and Command are
// all re-scrubbed here as defence-in-depth — the persisted trace must
// not depend on upstream scrubbing/validation guards being airtight.
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
		ID:                 e.runID,
		Config:             redactedConfig,
		StartedAt:          e.startedAt,
		CompletedAt:        now,
		Turns:              len(e.turns),
		TokenUsage:         totalTokens,
		ToolCalls:          summaries,
		PermissionDenials:  e.permissionDenials,
		Outcome:            outcome,
		FinalAssistantText: e.finalAssistantText,
		HookResults:        e.hookResults,
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
// can carry untrusted content: message text/tool_use/tool_result
// content (see scrubContentBlocks), ModelOutput text, and each
// ToolCalls[*] Input/Output/Structured. The same tool output reaches
// the trace twice — as ToolCalls[*].Output and as the next turn's
// tool_result Content — and both routes are scrubbed independently.
// ContentBlock.ThoughtSignature is dropped rather than scrubbed; it is
// provider-opaque and must not be persisted.
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
			// The structured payload carries the same untrusted content
			// as Output, so it is scrubbed on the same footing.
			Structured: scrubRawJSON(tc.Structured),
			Kind:       tc.Kind,
		}
	}
	return out
}

// scrubModelInput deep-copies a ModelInput with text content scrubbed.
// Tool definitions and the model string are not scrubbed: they are not
// secrets. Message.ReplayFields is deliberately absent from the rebuilt
// Message — like ContentBlock.ThoughtSignature it is provider-opaque
// round-trip state that must never be persisted (types/messages.go);
// the explicit field list below is the drop mechanism, so a future edit
// that copies the source Message wholesale would regress this.
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
// content; scrubRawJSON preserves a valid JSON shape. The Structured
// field on a tool_result block carries untrusted output (including
// MCP-derived) and is scrubbed the same way, mirroring the
// ToolCallRecord.Structured scrub in scrubTurnRecord — the same payload
// reaches the trace via both routes.
func scrubContentBlocks(blocks []types.ContentBlock) []types.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]types.ContentBlock, len(blocks))
	for i, b := range blocks {
		nb := b
		// ThoughtSignature is provider-opaque state (e.g. Gemini's
		// encrypted chain-of-thought blob) that must never be persisted
		// (types/messages.go); the persisted copy drops it outright,
		// leaving the loop's live blocks untouched.
		nb.ThoughtSignature = ""
		if nb.Text != "" {
			nb.Text = security.Scrub(nb.Text)
		}
		// Content is the tool_result text rendering — command output,
		// file excerpts, MCP server responses — the same untrusted
		// surface the ToolCallRecord.Output scrub covers on its other
		// route into the trace.
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
// while preserving JSON validity. If the scrubbed payload is no longer
// valid JSON (a regex boundary breaking a JSON literal), the entire
// payload is replaced by a JSON string literal carrying the scrubbed
// text so the on-disk shape stays parseable.
//
// Assumes the caller produced the json.RawMessage with encoding/json's
// default (non-HTML-escaping) marshaller — an HTML-escaping encoder
// would emit `<`, `>`, `&` as \uXXXX sequences that can cause a secret
// regex to miss. Do not pipe HTMLEscape output through this function.
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
		// json.Marshal on a string cannot fail in practice; this branch is
		// effectively dead. Fall back to a defensive empty-object literal
		// so the line stays valid JSON.
		slog.Default().Warn("scrubRawJSON string marshal failed; dropping payload to empty object",
			"error", err)

		return json.RawMessage(`{}`)
	}
	return json.RawMessage(wrapped)
}
