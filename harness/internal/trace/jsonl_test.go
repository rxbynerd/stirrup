package trace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// readEvents walks a streaming JSONL trace buffer and returns the events
// in order. Used by the JSONL emitter tests to assert on the on-wire
// shape without coupling to the reader package (which lives under
// types/trace and would form a test-only dependency cycle).
func readEvents(t *testing.T, src []byte) []Event {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	var events []Event
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event: %v\n%s", err, line)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan events: %v", err)
	}
	return events
}

// pickEvent returns the first event of the given kind, or fails the
// test. The streaming emitter writes at most one run_started and one
// run_finished per run, so "first" is unambiguous for those kinds.
func pickEvent(t *testing.T, events []Event, kind EventKind) Event {
	t.Helper()
	for _, ev := range events {
		if ev.Kind == kind {
			return ev
		}
	}
	t.Fatalf("no event of kind %q in stream", kind)
	return Event{}
}

func TestJSONLTraceEmitter_FullLifecycle(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	timeout := 300
	config := &types.RunConfig{
		RunID:    "run-123",
		Mode:     "execution",
		MaxTurns: 20,
		Timeout:  &timeout,
		Provider: types.ProviderConfig{
			APIKeyRef: "secret://ANTHROPIC_KEY",
		},
	}

	emitter.Start("run-123", config)

	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		ToolCalls:  2,
		StopReason: "tool_use",
		DurationMs: 1500,
	})
	emitter.RecordTurn(types.TurnTrace{
		Turn:       2,
		Tokens:     types.TokenUsage{Input: 200, Output: 75},
		ToolCalls:  0,
		StopReason: "end_turn",
		DurationMs: 800,
	})

	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "read_file",
		DurationMs: 10,
		Success:    true,
	})
	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "write_file",
		DurationMs: 25,
		Success:    true,
	})
	emitter.RecordPermissionDenial()
	emitter.RecordPermissionDenial()

	trace, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the in-memory trace summary.
	if trace.ID != "run-123" {
		t.Errorf("ID: got %q, want %q", trace.ID, "run-123")
	}
	if trace.Turns != 2 {
		t.Errorf("Turns: got %d, want 2", trace.Turns)
	}
	if trace.TokenUsage.Input != 300 || trace.TokenUsage.Output != 125 {
		t.Errorf("TokenUsage: got %+v, want {300, 125}", trace.TokenUsage)
	}
	if len(trace.ToolCalls) != 2 {
		t.Errorf("ToolCalls: got %d, want 2", len(trace.ToolCalls))
	}
	if trace.Outcome != "success" {
		t.Errorf("Outcome: got %q, want %q", trace.Outcome, "success")
	}
	if trace.PermissionDenials != 2 {
		t.Errorf("PermissionDenials: got %d, want 2", trace.PermissionDenials)
	}
	if trace.Config.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("APIKeyRef should be redacted, got %q", trace.Config.Provider.APIKeyRef)
	}

	// Verify the on-disk stream is well-formed and carries the events
	// the streaming-trace contract promises.
	events := readEvents(t, buf.Bytes())
	if len(events) < 4 {
		t.Fatalf("expected at least 4 events (started, 2 tool_call_record, finished), got %d", len(events))
	}

	started := pickEvent(t, events, EventKindRunStarted)
	if started.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("run_started schemaVersion: got %q, want %q", started.SchemaVersion, CurrentSchemaVersion)
	}
	if started.RunID != "run-123" {
		t.Errorf("run_started runId: got %q, want run-123", started.RunID)
	}
	if started.Config == nil {
		t.Fatal("run_started missing config")
	}
	if started.Config.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("run_started Config.Provider.APIKeyRef not redacted: %q", started.Config.Provider.APIKeyRef)
	}

	var toolCallEvents int
	for _, ev := range events {
		if ev.Kind == EventKindToolCallRecord {
			toolCallEvents++
		}
	}
	if toolCallEvents != 2 {
		t.Errorf("tool_call_record events: got %d, want 2", toolCallEvents)
	}

	finished := pickEvent(t, events, EventKindRunFinished)
	if finished.Trace == nil {
		t.Fatal("run_finished missing embedded trace summary")
	}
	if finished.Trace.ID != "run-123" {
		t.Errorf("run_finished trace ID: got %q, want run-123", finished.Trace.ID)
	}
	if finished.Trace.Outcome != "success" {
		t.Errorf("run_finished outcome: got %q, want success", finished.Trace.Outcome)
	}
	if finished.Trace.PermissionDenials != 2 {
		t.Errorf("run_finished permission denials: got %d, want 2", finished.Trace.PermissionDenials)
	}
}

// TestJSONLTraceEmitter_SessionNameRoundTrip pins that a SessionName set
// on the RunConfig flows into both the run_started event's redacted
// config and the run_finished event's embedded trace summary. The eval
// lakehouse and replay tooling rely on this — without it, a run
// labelled --name "nightly-eval" would not be filterable by label in
// downstream analysis.
func TestJSONLTraceEmitter_SessionNameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	timeout := 60
	config := &types.RunConfig{
		RunID:       "run-session",
		Mode:        "execution",
		SessionName: "nightly-eval",
		MaxTurns:    5,
		Timeout:     &timeout,
		Provider:    types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://X"},
	}
	emitter.Start("run-session", config)
	tr, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if tr.Config.SessionName != "nightly-eval" {
		t.Errorf("returned trace SessionName: got %q, want nightly-eval", tr.Config.SessionName)
	}

	events := readEvents(t, buf.Bytes())
	started := pickEvent(t, events, EventKindRunStarted)
	if started.Config == nil || started.Config.SessionName != "nightly-eval" {
		t.Errorf("run_started SessionName: got %+v, want nightly-eval", started.Config)
	}
	finished := pickEvent(t, events, EventKindRunFinished)
	if finished.Trace == nil || finished.Trace.Config.SessionName != "nightly-eval" {
		t.Errorf("run_finished trace SessionName: got %+v, want nightly-eval", finished.Trace)
	}
}

func TestJSONLTraceEmitter_EmptyRun(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	emitter.Start("run-empty", nil)

	trace, err := emitter.Finish(context.Background(), "error")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if trace.Turns != 0 {
		t.Errorf("Turns: got %d, want 0", trace.Turns)
	}
	if trace.Outcome != "error" {
		t.Errorf("Outcome: got %q, want %q", trace.Outcome, "error")
	}
	if buf.Len() == 0 {
		t.Error("expected output to be written")
	}

	// An empty run still produces a valid two-line stream: run_started
	// (with a nil config — the trace_emitter accepts a nil config so
	// callers that fail validation before constructing a full RunConfig
	// can still record telemetry) followed by run_finished.
	events := readEvents(t, buf.Bytes())
	if len(events) != 2 {
		t.Fatalf("empty run events: got %d, want 2 (started + finished)", len(events))
	}
	if events[0].Kind != EventKindRunStarted {
		t.Errorf("first event: got %q, want run_started", events[0].Kind)
	}
	if events[len(events)-1].Kind != EventKindRunFinished {
		t.Errorf("last event: got %q, want run_finished", events[len(events)-1].Kind)
	}
}

// TestJSONLTraceEmitter_RecordTurnRecord_Scrubs pins the defence-in-
// depth scrubbing contract: a recorded TurnRecord with secret-shaped
// substrings in the model output, tool input, or tool output never
// reaches disk in raw form. The scrubber runs before the line is
// written, so a SIGKILL between RecordTurnRecord and Finish still
// leaves a scrubbed event on disk.
func TestJSONLTraceEmitter_RecordTurnRecord_Scrubs(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	emitter.Start("run-scrub", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{
			Model: "claude-3-5-sonnet-latest",
			Messages: []types.Message{
				{
					Role: "user",
					Content: []types.ContentBlock{
						{Type: "text", Text: "here is my key sk-ant-api03-redactme please"},
					},
				},
			},
		},
		ModelOutput: []types.ContentBlock{
			{Type: "text", Text: "ack, your bearer Bearer ABCDEFG is unsafe"},
		},
		ToolCalls: []types.ToolCallRecord{
			{
				Name:   "run_command",
				Input:  json.RawMessage(`{"cmd":"echo sk-ant-api03-leak-leak"}`),
				Output: "stdout: sk-ant-api03-leak\nstderr: ok",
			},
		},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	on_disk := buf.String()
	// Anchor on a known-distinctive secret prefix: if any substring of
	// the form sk-ant-api03-... lands on disk verbatim the scrubber is
	// broken.
	if strings.Contains(on_disk, "sk-ant-api03-redactme") {
		t.Errorf("scrubber missed model input secret in on-disk trace:\n%s", on_disk)
	}
	if strings.Contains(on_disk, "sk-ant-api03-leak-leak") {
		t.Errorf("scrubber missed tool input secret in on-disk trace:\n%s", on_disk)
	}
	if strings.Contains(on_disk, "Bearer ABCDEFG") {
		t.Errorf("scrubber missed bearer token in on-disk trace:\n%s", on_disk)
	}
	// At least one [REDACTED] marker proves the scrubber ran.
	if !strings.Contains(on_disk, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder in scrubbed trace, got:\n%s", on_disk)
	}
}

// TestJSONLTraceEmitter_RecordTurnRecord_ScrubsStructured pins the issue #231
// requirement that the structured tool-result payload is scrubbed on the same
// footing as the text Output: a command transcript or file excerpt carried in
// ToolCallRecord.Structured must never reach disk with a secret in the clear.
func TestJSONLTraceEmitter_RecordTurnRecord_ScrubsStructured(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	emitter.Start("run-scrub-structured", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ToolCalls: []types.ToolCallRecord{
			{
				Name:    "run_command",
				Input:   json.RawMessage(`{"command":"cat .env"}`),
				Output:  "redacted in text already",
				Success: true,
				Structured: json.RawMessage(
					`{"stdout":"API_KEY=sk-ant-api03-structuredleak","stderr":"","exit_code":0}`,
				),
			},
		},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	onDisk := buf.String()
	if strings.Contains(onDisk, "sk-ant-api03-structuredleak") {
		t.Errorf("scrubber missed secret in structured payload on disk:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder proving the structured scrub ran, got:\n%s", onDisk)
	}
	// The on-disk structured payload must remain a parseable JSON object,
	// not a mangled fragment — scrubRawJSON preserves a valid shape.
	if !strings.Contains(onDisk, `"structured"`) {
		t.Errorf("expected the structured field to survive scrubbing in the trace, got:\n%s", onDisk)
	}
	// R3 guard: scrubRawJSON scrubs the raw byte stream and assumes a
	// non-HTML-escaping marshaller. None of this fixture's strings contain
	// HTML-special chars, so no \uXXXX escapes must appear on disk; if a
	// future change pipes HTMLEscape output through scrubRawJSON this fails,
	// flagging that secret regexes could miss escaped bytes.
	if strings.Contains(onDisk, `\u`) {
		t.Errorf("unexpected \\u escape on disk — an HTML-escaping encoder would defeat raw-byte scrubbing:\n%s", onDisk)
	}
}

// TestJSONLTraceEmitter_RecordTurnRecord_ScrubsContentBlockStructured pins the
// issue #231 B2 requirement that a structured tool-result envelope carried on a
// message-history ContentBlock is scrubbed before persistence. This is the
// route MCP-derived structured content (untrusted server output) takes into the
// trace: it lands on the tool_result block of the NEXT turn's ModelInput, not
// only on the ToolCallRecord. A secret-shaped substring inside that block's
// Structured payload must not reach disk in the clear.
func TestJSONLTraceEmitter_RecordTurnRecord_ScrubsContentBlockStructured(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	emitter.Start("run-scrub-cb-structured", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 2,
		ModelInput: types.ModelInput{
			Model: "claude-3-5-sonnet-latest",
			Messages: []types.Message{
				{
					Role: "user",
					Content: []types.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "call_1",
							Content:   "ok",
							Kind:      "mcp_tool_result",
							Structured: json.RawMessage(
								`{"structured_content":{"token":"sk-ant-api03-cbleak"}}`,
							),
						},
					},
				},
			},
		},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	onDisk := buf.String()
	if strings.Contains(onDisk, "sk-ant-api03-cbleak") {
		t.Errorf("scrubber missed secret in content-block structured payload on disk:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder proving the content-block structured scrub ran, got:\n%s", onDisk)
	}
	// The structured field must survive as parseable JSON, not a mangled
	// fragment — scrubRawJSON preserves a valid shape.
	if !strings.Contains(onDisk, `"structured"`) {
		t.Errorf("expected the content-block structured field to survive scrubbing, got:\n%s", onDisk)
	}
}

// TestJSONLTraceEmitter_RecordTurnRecord_PreservesSynthetic pins the requirement
// that scrubModelInput forwards the Synthetic marker to the on-disk trace so the
// replay/mining toolchain can distinguish harness-injected turns from genuine
// user content (#340).
func TestJSONLTraceEmitter_RecordTurnRecord_PreservesSynthetic(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	emitter.Start("run-synthetic", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{
			Model: "test-model",
			Messages: []types.Message{
				{
					Role:    "user",
					Content: []types.ContentBlock{{Type: "text", Text: "real user prompt"}},
				},
				{
					Role:      "user",
					Synthetic: true,
					Content:   []types.ContentBlock{{Type: "text", Text: "escalation nudge"}},
				},
			},
		},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	events := readEvents(t, buf.Bytes())
	var turnEv Event
	for _, ev := range events {
		if ev.Kind == EventKindTurnRecord {
			turnEv = ev
			break
		}
	}
	if turnEv.ModelInput == nil {
		t.Fatal("turn_record has no modelInput")
	}
	msgs := turnEv.ModelInput.Messages
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Synthetic {
		t.Error("first message should not be synthetic")
	}
	if !msgs[1].Synthetic {
		t.Error("second message should have Synthetic:true preserved through scrubModelInput")
	}

	// Also verify the on-disk JSON contains the synthetic field explicitly.
	onDisk := buf.String()
	if !strings.Contains(onDisk, `"synthetic":true`) {
		t.Errorf("expected on-disk JSON to contain synthetic:true, got:\n%s", onDisk)
	}
}

// TestJSONLTraceEmitter_PartialStream pins the SIGKILL-safety property:
// when a run is interrupted between RecordTurnRecord and Finish, the
// on-disk file is still parseable up to the last completed event.
// A bytes.Buffer is used to simulate the file; in production the
// emitter writes to an os.File whose os.Write calls are line-flushed
// by the kernel for sub-PIPE_BUF writes.
func TestJSONLTraceEmitter_PartialStream(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)

	emitter.Start("run-partial", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ModelOutput: []types.ContentBlock{
			{Type: "text", Text: "first turn"},
		},
	})
	// Simulate a SIGKILL: no Finish call.
	events := readEvents(t, buf.Bytes())
	if len(events) != 2 {
		t.Fatalf("partial stream events: got %d, want 2 (started + turn_record)", len(events))
	}
	if events[0].Kind != EventKindRunStarted {
		t.Errorf("first event: got %q, want run_started", events[0].Kind)
	}
	if events[1].Kind != EventKindTurnRecord {
		t.Errorf("second event: got %q, want turn_record", events[1].Kind)
	}
	if events[1].Turn != 1 {
		t.Errorf("turn_record Turn: got %d, want 1", events[1].Turn)
	}
}

// TestScrubRawJSON_WrapsWhenScrubBreaksValidity exercises the branch where the
// scrubbed byte stream is no longer valid JSON. The "[REDACTED]" placeholder is
// a bare token, so a secret that was sitting as an unquoted JSON value leaves
// the document unparseable after replacement. scrubRawJSON must then re-wrap
// the scrubbed text as a JSON string literal so the on-disk line stays valid,
// rather than emitting raw broken JSON.
func TestScrubRawJSON_WrapsWhenScrubBreaksValidity(t *testing.T) {
	// secret:// matches the secret_ref pattern; sitting as an unquoted value it
	// is replaced by the bare "[REDACTED]" token, breaking JSON validity.
	in := json.RawMessage(`{"a":secret://leaked}`)
	out := scrubRawJSON(in)

	if !json.Valid(out) {
		t.Fatalf("scrubRawJSON output is not valid JSON: %s", out)
	}
	// The wrap path encodes the scrubbed text as a JSON string. Decoding it
	// back must yield the scrubbed-but-invalid intermediate, with the secret
	// gone.
	var decoded string
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("wrapped output should decode as a JSON string: %v (out=%s)", err, out)
	}
	if strings.Contains(decoded, "secret://leaked") {
		t.Errorf("secret survived scrub: %q", decoded)
	}
	if !strings.Contains(decoded, "[REDACTED]") {
		t.Errorf("expected redaction marker in wrapped text, got %q", decoded)
	}
}

// TestScrubRawJSON_PreservesValidJSON guards the common path: a secret embedded
// in a well-formed JSON string is scrubbed in place and the result stays valid
// JSON without the string-wrap fallback.
func TestScrubRawJSON_PreservesValidJSON(t *testing.T) {
	in := json.RawMessage(`{"token":"ghp_deadbeefcafef00d"}`)
	out := scrubRawJSON(in)

	if !json.Valid(out) {
		t.Fatalf("scrubRawJSON output is not valid JSON: %s", out)
	}
	var decoded map[string]string
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("scrubbed output should decode as an object: %v (out=%s)", err, out)
	}
	if decoded["token"] != "[REDACTED]" {
		t.Errorf("token field = %q, want [REDACTED]", decoded["token"])
	}
}
