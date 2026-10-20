package trace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

// readEvents walks a streaming JSONL trace buffer and returns the events
// in order. Used by the JSONL emitter tests to assert on the on-wire
// shape without coupling to the reader package (which lives under
// types/trace and would form a test-only dependency cycle).
func readEvents(t *testing.T, src []byte) []Event {
	t.Helper()
	must := require.New(t)
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	var events []Event
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		must.NoError(json.Unmarshal(line, &ev), "unmarshal event\n%s", line)
		events = append(events, ev)
	}
	must.NoError(scanner.Err(), "scan events")
	return events
}

// pickEvent returns the first event of the given kind, or fails the
// test. The streaming emitter writes at most one run_started and one
// run_finished per run, so "first" is unambiguous for those kinds.
func pickEvent(t *testing.T, events []Event, kind EventKind) Event {
	t.Helper()
	must := require.New(t)
	for _, ev := range events {
		if ev.Kind == kind {
			return ev
		}
	}
	must.FailNowf("event not found", "no event of kind %q in stream", kind)
	return Event{}
}

func TestJSONLTraceEmitter_FullLifecycle(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

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
		Tokens:     types.TokenUsage{Input: 100, Output: 50, CacheRead: 60, CacheWrite: 30, Reasoning: 10},
		ToolCalls:  2,
		StopReason: "tool_use",
		DurationMs: 1500,
	})
	emitter.RecordTurn(types.TurnTrace{
		Turn:       2,
		Tokens:     types.TokenUsage{Input: 200, Output: 75, CacheRead: 150, Reasoning: 5},
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
	must.NoError(err)

	check.Equal("run-123", trace.ID)
	check.Equal(2, trace.Turns)
	wantTokens := types.TokenUsage{Input: 300, Output: 125, CacheRead: 210, CacheWrite: 30, Reasoning: 15}
	check.Equal(wantTokens, trace.TokenUsage)
	check.Len(trace.ToolCalls, 2)
	check.Equal("success", trace.Outcome)
	check.Equal(2, trace.PermissionDenials)
	check.Equal("secret://[REDACTED]", trace.Config.Provider.APIKeyRef)

	events := readEvents(t, buf.Bytes())
	must.GreaterOrEqual(len(events), 4, "expected started, 2 tool_call_record and finished events")

	started := pickEvent(t, events, EventKindRunStarted)
	check.Equal(CurrentSchemaVersion, started.SchemaVersion)
	check.Equal("run-123", started.RunID)
	must.NotNil(started.Config, "run_started missing config")
	check.Equal("secret://[REDACTED]", started.Config.Provider.APIKeyRef)

	var toolCallEvents int
	for _, ev := range events {
		if ev.Kind == EventKindToolCallRecord {
			toolCallEvents++
		}
	}
	check.Equal(2, toolCallEvents, "tool_call_record events")

	finished := pickEvent(t, events, EventKindRunFinished)
	must.NotNil(finished.Trace, "run_finished missing embedded trace summary")
	check.Equal("run-123", finished.Trace.ID)
	check.Equal("success", finished.Trace.Outcome)
	check.Equal(2, finished.Trace.PermissionDenials)
	check.Equal(wantTokens, finished.Trace.TokenUsage)
	check.Contains(buf.String(), `"tokenUsage":{"input":300,"output":125,"cacheRead":210,"cacheWrite":30,"reasoning":15}`,
		"run_finished line missing the token usage breakdown")
}

// TestJSONLTraceEmitter_SessionNameRoundTrip pins that a SessionName set
// on the RunConfig flows into both the run_started event's redacted
// config and the run_finished event's embedded trace summary. The eval
// lakehouse and replay tooling rely on this — without it, a run
// labelled --name "nightly-eval" would not be filterable by label in
// downstream analysis.
func TestJSONLTraceEmitter_SessionNameRoundTrip(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

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
	must.NoError(err)
	must.Equal("nightly-eval", tr.Config.SessionName, "returned trace SessionName")

	events := readEvents(t, buf.Bytes())
	started := pickEvent(t, events, EventKindRunStarted)
	must.NotNil(started.Config, "run_started missing config")
	check.Equal("nightly-eval", started.Config.SessionName, "run_started SessionName")

	finished := pickEvent(t, events, EventKindRunFinished)
	must.NotNil(finished.Trace, "run_finished missing embedded trace summary")
	check.Equal("nightly-eval", finished.Trace.Config.SessionName, "run_finished trace SessionName")
}

func TestJSONLTraceEmitter_EmptyRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-empty", nil)

	trace, err := emitter.Finish(context.Background(), "error")
	must.NoError(err)
	check.Zero(trace.Turns)
	check.Equal("error", trace.Outcome)
	check.NotEmpty(buf.Bytes(), "expected output to be written")

	// A nil config is accepted, so callers that fail validation before
	// constructing a full RunConfig can still record telemetry.
	events := readEvents(t, buf.Bytes())
	must.Len(events, 2, "expected run_started and run_finished only")
	check.Equal(EventKindRunStarted, events[0].Kind, "first event")
	check.Equal(EventKindRunFinished, events[len(events)-1].Kind, "last event")
}

// TestJSONLTraceEmitter_RecordTurnRecord_Scrubs pins the defence-in-
// depth scrubbing contract: a recorded TurnRecord with secret-shaped
// substrings in the model output, tool input, or tool output never
// reaches disk in raw form. The scrubber runs before the line is
// written, so a SIGKILL between RecordTurnRecord and Finish still
// leaves a scrubbed event on disk.
func TestJSONLTraceEmitter_RecordTurnRecord_Scrubs(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

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
			{Type: "text", Text: "ack, your bearer Bearer ABCDEFG1 is unsafe"},
		},
		ToolCalls: []types.ToolCallRecord{
			{
				Name:   "run_command",
				Input:  json.RawMessage(`{"cmd":"echo sk-ant-api03-leak-leak"}`),
				Output: "stdout: sk-ant-api03-leak\nstderr: ok",
			},
		},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	// Anchor on a known-distinctive secret prefix: if any substring of
	// the form sk-ant-api03-... lands on disk verbatim the scrubber is
	// broken.
	check.NotContains(onDisk, "sk-ant-api03-redactme", "scrubber missed model input secret")
	check.NotContains(onDisk, "sk-ant-api03-leak-leak", "scrubber missed tool input secret")
	check.NotContains(onDisk, "Bearer ABCDEFG1", "scrubber missed bearer token")
	check.Contains(onDisk, "[REDACTED]", "scrubbed trace has no [REDACTED] placeholder")
}

// TestScrubTurnRecord_ToolCallRecord_FieldCompleteness guards
// scrubTurnRecord's field-by-field ToolCallRecord rebuild. A field added
// to types.ToolCallRecord that the rebuild does not copy is silently
// dropped from every persisted trace, so each field must either
// round-trip verbatim or, for Input, Output and Structured, survive
// scrubbing as a non-zero value. The fixture sets every field to a
// non-zero value, so a field left unset cannot pass by comparing
// zero to zero.
func TestScrubTurnRecord_ToolCallRecord_FieldCompleteness(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// scrubbedFields are redacted by scrubTurnRecord, so they are exempt
	// from the verbatim check. Their redaction is covered by
	// TestJSONLTraceEmitter_RecordTurnRecord_Scrubs and the other
	// Scrubs* tests in this file.
	scrubbedFields := map[string]bool{
		"Input":      true,
		"Output":     true,
		"Structured": true,
	}

	in := types.ToolCallRecord{
		ID:           "tool-call-id",
		Name:         "run_command",
		InternalName: "internal_run_command",
		Input:        json.RawMessage(`{"cmd":"ls"}`),
		Output:       "plain output text",
		DurationMs:   1234,
		Success:      true,
		Structured:   json.RawMessage(`{"kind":"file_edit"}`),
		Kind:         "file_edit",
		IsError:      true,
	}

	rtIn := reflect.TypeOf(in)
	inFixtureVal := reflect.ValueOf(in)
	for i := 0; i < rtIn.NumField(); i++ {
		must.False(inFixtureVal.Field(i).IsZero(),
			"fixture field %q is zero; set every ToolCallRecord field in the fixture", rtIn.Field(i).Name)
	}

	turn := types.TurnRecord{Turn: 1, ToolCalls: []types.ToolCallRecord{in}}

	out := scrubTurnRecord(turn, false)
	must.Len(out.ToolCalls, 1)
	got := out.ToolCalls[0]

	inVal := reflect.ValueOf(in)
	gotVal := reflect.ValueOf(got)
	rt := reflect.TypeOf(in)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if scrubbedFields[name] {
			check.False(gotVal.Field(i).IsZero(),
				"field %s was dropped instead of scrubbed", name)
			continue
		}
		check.Equal(inVal.Field(i).Interface(), gotVal.Field(i).Interface(),
			"field %s not copied verbatim by scrubTurnRecord", name)
	}
}

// TestJSONLTraceEmitter_RecordTurnRecord_DropsThoughtSignature pins the
// ThoughtSignature persistence ban: the field is provider-opaque state
// (Gemini's encrypted chain-of-thought) whose contract in
// types/messages.go forbids logging it verbatim. Because it is opaque,
// the scrubber cannot redact within it — the persisted copy must drop
// it outright, on both the model-output route and the message-history
// route of the next turn.
func TestJSONLTraceEmitter_RecordTurnRecord_DropsThoughtSignature(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	const signature = "opaque-thought-signature-blob-do-not-persist"
	emitter.Start("run-thought-signature", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 2,
		ModelInput: types.ModelInput{
			Model: "gemini-3-pro",
			Messages: []types.Message{
				{
					Role: "assistant",
					Content: []types.ContentBlock{
						{Type: "text", Text: "prior turn", ThoughtSignature: signature},
					},
				},
			},
		},
		ModelOutput: []types.ContentBlock{
			{Type: "text", Text: "answer", ThoughtSignature: signature},
		},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	check.NotContains(onDisk, signature, "ThoughtSignature survived into the persisted trace")
	check.NotContains(onDisk, "thought_signature", "thought_signature key should be omitted after drop")
}

// TestJSONLTraceEmitter_RecordTurnRecord_DropsThinkingSignatures extends the
// persistence ban to Anthropic thinking blocks: the signature and the
// redacted data are both opaque, so the recording keeps each block's type
// and (scrubbed) text but never the blob.
func TestJSONLTraceEmitter_RecordTurnRecord_DropsThinkingSignatures(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	const signature = "anthropic-thinking-signature-do-not-persist"
	const redacted = "anthropic-redacted-data-do-not-persist"
	thinking := []types.ContentBlock{
		{Type: "thinking", Text: "plan", ThoughtSignature: signature},
		{Type: "redacted_thinking", ThoughtSignature: redacted},
		{Type: "text", Text: "answer"},
	}
	emitter.Start("run-thinking-signature", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{
			Model:    "claude-sonnet-5-5",
			Messages: []types.Message{{Role: "assistant", Content: thinking}},
		},
		ModelOutput: thinking,
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	for _, secret := range []string{signature, redacted} {
		check.NotContains(onDisk, secret, "thinking blob survived into the persisted trace")
	}
	check.Contains(onDisk, `"type":"thinking","text":"plan"`, "thinking block type missing from the recording")
	check.Contains(onDisk, `"type":"redacted_thinking"`, "redacted_thinking block type missing from the recording")
}

// TestJSONLTraceEmitter_RecordTurnRecord_DropsMessageReplayFields mirrors
// the ThoughtSignature drop test for the message-level
// Message.ReplayFields carrier (the quirks ReplayFields round-trip
// state, e.g. DeepSeek's reasoning_content). Same contract, same
// mechanism: the value is provider-opaque so the scrubber cannot redact
// within it, and scrubModelInput's explicit field list drops it from
// the persisted message history outright.
func TestJSONLTraceEmitter_RecordTurnRecord_DropsMessageReplayFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	const replayValue = "opaque-reasoning-content-do-not-persist"
	emitter.Start("run-replay-fields", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 2,
		ModelInput: types.ModelInput{
			Model: "deepseek-v4-flash",
			Messages: []types.Message{
				{
					Role: "assistant",
					Content: []types.ContentBlock{
						{Type: "text", Text: "prior turn"},
					},
					ReplayFields: map[string]json.RawMessage{
						"reasoning_content": json.RawMessage(`"` + replayValue + `"`),
					},
				},
			},
		},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	check.NotContains(onDisk, replayValue, "Message.ReplayFields value survived into the persisted trace")
	check.NotContains(onDisk, "replay_fields", "replay_fields key should be omitted after drop")
}

// TestJSONLTraceEmitter_RecordTurnRecord_DropsResponsesOutputReplay pins the
// same drop for the OpenAI Responses adapter's stored output items, whose
// reasoning items carry encrypted_content: the whole array stays out of the
// persisted trace.
func TestJSONLTraceEmitter_RecordTurnRecord_DropsResponsesOutputReplay(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	const encrypted = "gAAAAAB-encrypted-reasoning-do-not-persist"
	emitter.Start("run-responses-replay", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 2,
		ModelInput: types.ModelInput{
			Model: "gpt-5.6-sol",
			Messages: []types.Message{
				{
					Role:    "assistant",
					Content: []types.ContentBlock{{Type: "text", Text: "prior turn"}},
					ReplayFields: map[string]json.RawMessage{
						"openai_responses.output": json.RawMessage(`[{"type":"reasoning","id":"rs_trace","summary":[],"encrypted_content":"` + encrypted + `"},{"type":"message","id":"msg_trace","role":"assistant","content":[{"type":"output_text","text":"prior turn"}]}]`),
					},
				},
			},
		},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	for _, leak := range []string{encrypted, "rs_trace", "msg_trace", "replay_fields"} {
		check.NotContains(onDisk, leak, "persisted trace contains leaked replay content")
	}
}

// TestJSONLTraceEmitter_RecordTurnRecord_ScrubsToolResultContent pins
// the scrub of ContentBlock.Content — the tool_result text rendering
// that rides the message history into the next turn's ModelInput. The
// same payload is scrubbed as ToolCallRecord.Output on its other route
// into the trace; this covers the message-history route.
func TestJSONLTraceEmitter_RecordTurnRecord_ScrubsToolResultContent(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-scrub-tool-result", nil)
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
							ToolUseID: "tu-1",
							Content:   "API_KEY=sk-ant-api03-toolresultleak loaded",
						},
					},
				},
			},
		},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	check.NotContains(onDisk, "sk-ant-api03-toolresultleak", "scrubber missed secret in tool_result content")
	check.Contains(onDisk, "[REDACTED]", "tool_result scrub did not run")
}

// TestJSONLTraceEmitter_RecordTurnRecord_ScrubsStructured pins the
// requirement that the structured tool-result payload is scrubbed on the same
// footing as the text Output: a command transcript or file excerpt carried in
// ToolCallRecord.Structured must never reach disk with a secret in the clear.
func TestJSONLTraceEmitter_RecordTurnRecord_ScrubsStructured(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

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
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	check.NotContains(onDisk, "sk-ant-api03-structuredleak", "scrubber missed secret in structured payload")
	check.Contains(onDisk, "[REDACTED]", "structured scrub did not run")
	check.Contains(onDisk, `"structured"`, "structured field missing from the trace")
	// scrubRawJSON matches secrets in the raw JSON bytes. The persisted
	// JSON must not contain \u escapes, which would split a secret's bytes.
	check.NotContains(onDisk, `\u`, "persisted JSON contains a \\u escape")
}

// TestJSONLTraceEmitter_RecordTurnRecord_ScrubsContentBlockStructured pins the
// requirement that a structured tool-result envelope carried on a
// message-history ContentBlock is scrubbed before persistence. This is the
// route MCP-derived structured content (untrusted server output) takes into the
// trace: it lands on the tool_result block of the NEXT turn's ModelInput, not
// only on the ToolCallRecord. A secret-shaped substring inside that block's
// Structured payload must not reach disk in the clear.
func TestJSONLTraceEmitter_RecordTurnRecord_ScrubsContentBlockStructured(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

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
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	onDisk := buf.String()
	check.NotContains(onDisk, "sk-ant-api03-cbleak", "scrubber missed secret in content-block structured payload")
	check.Contains(onDisk, "[REDACTED]", "content-block structured scrub did not run")
	// The structured field must survive as parseable JSON, not a mangled
	// fragment: scrubRawJSON preserves a valid shape.
	check.Contains(onDisk, `"structured"`, "content-block structured field should survive scrubbing")
}

// TestJSONLTraceEmitter_RecordTurnRecord_PreservesSynthetic pins the requirement
// that scrubModelInput forwards the Synthetic and Origin markers to the on-disk trace so the
// replay/mining toolchain can distinguish harness-injected turns from genuine
// user content.
func TestJSONLTraceEmitter_RecordTurnRecord_PreservesSynthetic(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

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
				{
					Role:      "user",
					Synthetic: true,
					Origin:    types.MessageOriginVerifierFeedback,
					Content:   []types.ContentBlock{{Type: "text", Text: "tests failed"}},
				},
			},
		},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	turnEv := pickEvent(t, readEvents(t, buf.Bytes()), EventKindTurnRecord)
	must.NotNil(turnEv.ModelInput, "turn_record has no modelInput")
	msgs := turnEv.ModelInput.Messages
	must.Len(msgs, 3)
	check.False(msgs[0].Synthetic, "first message should not be synthetic")
	check.True(msgs[1].Synthetic, "second message should keep Synthetic:true through scrubModelInput")
	check.Empty(msgs[1].Origin, "escalation nudge should not gain an origin")
	check.Equal(types.MessageOriginVerifierFeedback, msgs[2].Origin, "verifier feedback origin should survive scrubModelInput")

	check.Contains(buf.String(), `"synthetic":true`, "on-disk JSON should carry synthetic:true")
}

// TestJSONLTraceEmitter_RecordTurnRecord_PreservesOutputLimitRecovery pins
// that a turn the loop continued past an output-limit stop is marked on
// its turn_record line, and that other turns omit the field.
func TestJSONLTraceEmitter_RecordTurnRecord_PreservesOutputLimitRecovery(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-olr", nil)
	emitter.RecordTurnRecord(types.TurnRecord{Turn: 0, OutputLimitRecovery: true})
	emitter.RecordTurnRecord(types.TurnRecord{Turn: 1})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	var flags []bool
	for _, ev := range readEvents(t, buf.Bytes()) {
		if ev.Kind == EventKindTurnRecord {
			flags = append(flags, ev.OutputLimitRecovery)
		}
	}
	check.Equal([]bool{true, false}, flags, "turn_record outputLimitRecovery")
	check.Equal(1, strings.Count(buf.String(), `"outputLimitRecovery":true`), "on-disk outputLimitRecovery:true count")
}

// TestJSONLTraceEmitter_RecordTurnRecord_PreservesStallNudge pins that a
// turn followed by a stall nudge is marked on its turn_record line, and
// that other turns omit the field.
func TestJSONLTraceEmitter_RecordTurnRecord_PreservesStallNudge(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-nudge", nil)
	emitter.RecordTurnRecord(types.TurnRecord{Turn: 0})
	emitter.RecordTurnRecord(types.TurnRecord{Turn: 1, StallNudge: true})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	var flags []bool
	for _, ev := range readEvents(t, buf.Bytes()) {
		if ev.Kind == EventKindTurnRecord {
			flags = append(flags, ev.StallNudge)
		}
	}
	check.Equal([]bool{false, true}, flags, "turn_record stallNudge")
	check.Equal(1, strings.Count(buf.String(), `"stallNudge":true`), "on-disk stallNudge:true count")
}

// TestJSONLTraceEmitter_RecordTurnRecord_PreservesWindDown pins that the
// turn carrying the wind-down instruction names its trigger on its
// turn_record line, and that other turns omit the field.
func TestJSONLTraceEmitter_RecordTurnRecord_PreservesWindDown(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-wind-down", nil)
	emitter.RecordTurnRecord(types.TurnRecord{Turn: 0})
	emitter.RecordTurnRecord(types.TurnRecord{Turn: 1, WindDown: "time"})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	var triggers []string
	for _, ev := range readEvents(t, buf.Bytes()) {
		if ev.Kind == EventKindTurnRecord {
			triggers = append(triggers, ev.WindDown)
		}
	}
	check.Equal([]string{"", "time"}, triggers, "turn_record windDown")
	check.Equal(1, strings.Count(buf.String(), `"windDown"`), "on-disk windDown count")
}

func TestJSONLTraceEmitter_PreservesLazyToolLoadingFields(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-lazy-tools", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 0,
		ModelInput: types.ModelInput{Tools: []types.ToolDefinition{
			{Name: "read_file", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_github_create_issue", InputSchema: json.RawMessage(`{}`), Deferred: true},
		}},
		ModelOutput: []types.ContentBlock{
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"github"}`)},
			{Type: "tool_search_result", ToolUseID: "srvtoolu_1", ToolReferences: []string{"mcp_github_create_issue"}},
			{Type: "tool_search", ID: "srvtoolu_2", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":""}`)},
			{Type: "tool_search_result", ToolUseID: "srvtoolu_2", Name: "invalid_tool_input", Content: "query is empty", IsError: true},
		},
		ToolsLoaded: []string{"mcp_github_create_issue"},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	events := readEvents(t, buf.Bytes())
	record := pickEvent(t, events, EventKindTurnRecord)
	must.False(record.ModelInput.Tools[0].Deferred)
	must.True(record.ModelInput.Tools[1].Deferred)
	must.Equal("tool_search", record.ModelOutput[0].Type)
	must.JSONEq(`{"query":"github"}`, string(record.ModelOutput[0].Input))
	must.Equal("srvtoolu_1", record.ModelOutput[1].ToolUseID)
	must.Equal([]string{"mcp_github_create_issue"}, record.ModelOutput[1].ToolReferences)
	must.Equal(types.ContentBlock{
		Type: "tool_search_result", ToolUseID: "srvtoolu_2", Name: "invalid_tool_input", Content: "query is empty", IsError: true,
	}, record.ModelOutput[3])
	must.Equal([]string{"mcp_github_create_issue"}, record.ToolsLoaded)
}

func TestJSONLTraceEmitter_ScrubsToolSearchErrorMessage(t *testing.T) {
	must := require.New(t)
	const secret = "sk-ant-api03-toolsearchleak"
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-lazy-tools-scrub", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		ModelOutput: []types.ContentBlock{
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"` + secret + `"}`)},
			{Type: "tool_search_result", ToolUseID: "srvtoolu_1", Name: "invalid_tool_input", Content: "bad query " + secret, IsError: true},
		},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	must.NotContains(buf.String(), secret)
	record := pickEvent(t, readEvents(t, buf.Bytes()), EventKindTurnRecord)
	must.Equal("invalid_tool_input", record.ModelOutput[1].Name)
	must.True(record.ModelOutput[1].IsError)
}

// TestJSONLTraceEmitter_PartialStream_ParseableBeforeFinish pins the SIGKILL-safety property:
// when a run is interrupted between RecordTurnRecord and Finish, the
// on-disk file is still parseable up to the last completed event.
// A bytes.Buffer is used to simulate the file; in production the
// emitter writes to an os.File whose os.Write calls are line-flushed
// by the kernel for sub-PIPE_BUF writes.
func TestJSONLTraceEmitter_PartialStream_ParseableBeforeFinish(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)

	emitter.Start("run-partial", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ModelOutput: []types.ContentBlock{
			{Type: "text", Text: "first turn"},
		},
	})
	// Simulate a SIGKILL: no Finish call.
	events := readEvents(t, buf.Bytes())
	must.Len(events, 2, "expected run_started and turn_record before Finish")
	check.Equal(EventKindRunStarted, events[0].Kind, "first event kind")
	check.Equal(EventKindTurnRecord, events[1].Kind, "second event kind")
	check.Equal(1, events[1].Turn, "turn_record turn")
}

// TestJSONLTraceEmitter_ImplementsHookRecorder is a compile-time
// satisfaction guard for the optional-capability interface.
func TestJSONLTraceEmitter_ImplementsHookRecorder(t *testing.T) {
	var _ HookRecorder = (*JSONLTraceEmitter)(nil)
}

// TestJSONLTraceEmitter_RecordHookExecution_StreamsAndAccumulates pins
// the dual write RecordHookExecution performs: an inline hook_record
// event per call (so a live consumer sees each hook as it lands,
// mirroring tool_call_record) AND accumulation into RunTrace.HookResults
// at Finish, in call order.
func TestJSONLTraceEmitter_RecordHookExecution_StreamsAndAccumulates(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-hooks", nil)

	emitter.RecordHookExecution(context.Background(), types.HookExecution{
		Phase: "preRun", Index: 0, Name: "clone", Command: "git clone . .", ExitCode: 0, DurationMs: 10,
	})
	emitter.RecordHookExecution(context.Background(), types.HookExecution{
		Phase: "postRun", Index: 0, Name: "smoke-test", Command: "test -f marker", ExitCode: 0, DurationMs: 5,
	})

	trace, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	must.Len(trace.HookResults, 2)
	check.Equal("preRun", trace.HookResults[0].Phase, "HookResults[0] phase")
	check.Equal("clone", trace.HookResults[0].Name, "HookResults[0] name")
	check.Equal("postRun", trace.HookResults[1].Phase, "HookResults[1] phase")
	check.Equal("smoke-test", trace.HookResults[1].Name, "HookResults[1] name")

	events := readEvents(t, buf.Bytes())
	var hookEvents []Event
	for _, ev := range events {
		if ev.Kind == EventKindHookRecord {
			hookEvents = append(hookEvents, ev)
		}
	}
	must.Len(hookEvents, 2)
	must.NotNil(hookEvents[0].Hook, "hook_record[0] has no hook payload")
	check.Equal("clone", hookEvents[0].Hook.Name, "hook_record[0] name")
	must.NotNil(hookEvents[1].Hook, "hook_record[1] has no hook payload")
	check.Equal("smoke-test", hookEvents[1].Hook.Name, "hook_record[1] name")
}

// TestJSONLTraceEmitter_RecordHookExecution_Scrubs pins the
// defence-in-depth scrub RecordHookExecution applies to Command,
// OutputTail, and Error: even though hook.ExecRunner already scrubs
// OutputTail before constructing the types.HookExecution and
// ValidateRunConfig structurally rejects a "secret://" reference in
// Command, the emitter re-scrubs all three fields before they reach
// disk, the same posture RecordTurnRecord applies to tool output. A
// Command with no secret-shaped literal survives intact.
func TestJSONLTraceEmitter_RecordHookExecution_Scrubs(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-hook-scrub", nil)

	emitter.RecordHookExecution(context.Background(), types.HookExecution{
		Phase:      "preRun",
		Index:      0,
		Command:    "curl -H \"Authorization: Bearer $TOKEN\"", // no secret-shaped literal
		OutputTail: "response included sk-ant-api03-leak in the body",
		Error:      "request failed, token sk-ant-api03-leak rejected",
	})
	trace, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	check.NotContains(buf.String(), "sk-ant-api03-leak", "scrubber missed hook secret in on-disk trace")
	hook := trace.HookResults[0]
	check.NotContains(hook.OutputTail, "sk-ant-api03-leak", "HookResults[0] OutputTail not scrubbed")
	check.NotContains(hook.Error, "sk-ant-api03-leak", "HookResults[0] Error not scrubbed")
	check.Equal(`curl -H "Authorization: Bearer $TOKEN"`, hook.Command, "a command without a secret-shaped literal must survive intact")
}

// TestJSONLTraceEmitter_RecordHookExecution_ScrubsCaseVariedSecretRef
// pins the belt-and-braces contract explicitly: even if a case-varied
// "secret://" reference somehow reached RecordHookExecution — bypassing
// ValidateRunConfig's case-insensitive rejection, e.g. a RunConfig
// assembled programmatically without going through validation — the
// trace emitter's own scrub still redacts it before it lands on disk or
// in RunTrace.HookResults. The persisted trace must not depend solely
// on the upstream guard being airtight.
func TestJSONLTraceEmitter_RecordHookExecution_ScrubsCaseVariedSecretRef(t *testing.T) {
	cases := []struct {
		name    string
		command string
	}{
		{"uppercase scheme", "echo SECRET://API_KEY"},
		{"titlecase scheme", "echo Secret://API_KEY"},
		{"mixedcase scheme embedded", "curl -H \"Authorization: Bearer $(cat sEcReT://mixed_case)\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer
			emitter := NewJSONLTraceEmitter(&buf, false)
			emitter.Start("run-hook-case-bypass", nil)

			emitter.RecordHookExecution(context.Background(), types.HookExecution{
				Phase:   "preRun",
				Index:   0,
				Command: tc.command,
			})
			trace, err := emitter.Finish(context.Background(), "success")
			must.NoError(err)

			onDisk := buf.String()
			must.NotContains(strings.ToLower(onDisk), "secret://", "scrubber missed case-varied secret:// reference in on-disk trace")
			must.NotContains(strings.ToLower(trace.HookResults[0].Command), "secret://", "HookResults[0].Command not scrubbed")
		})
	}
}

// TestJSONLTraceEmitter_NoHooks_LeavesHookResultsEmpty pins that a run
// with no lifecycle hooks configured produces no hook_record events and
// a nil/empty HookResults, so the byte-for-byte-unchanged guarantee for
// a hookless run extends to the JSONL trace shape.
func TestJSONLTraceEmitter_NoHooks_LeavesHookResultsEmpty(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-no-hooks", nil)
	trace, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
	check.Empty(trace.HookResults, "a hookless run should have no hook results")
	check.NotContains(buf.String(), string(EventKindHookRecord), "a hookless run should write no hook_record events")
}

// TestScrubRawJSON_WrapsWhenScrubBreaksValidity exercises the branch where the
// scrubbed byte stream is no longer valid JSON. The "[REDACTED]" placeholder is
// a bare token, so a secret that was sitting as an unquoted JSON value leaves
// the document unparseable after replacement. scrubRawJSON must then re-wrap
// the scrubbed text as a JSON string literal so the on-disk line stays valid,
// rather than emitting raw broken JSON.
func TestScrubRawJSON_WrapsWhenScrubBreaksValidity(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// secret:// matches the secret_ref pattern; sitting as an unquoted value it
	// is replaced by the bare "[REDACTED]" token, breaking JSON validity.
	in := json.RawMessage(`{"a":secret://leaked}`)
	out := scrubRawJSON(in, false)

	must.True(json.Valid(out), "scrubRawJSON output is not valid JSON: %s", out)
	// The wrap path encodes the scrubbed text as a JSON string. Decoding it
	// back must yield the scrubbed-but-invalid intermediate, with the secret
	// gone.
	var decoded string
	must.NoError(json.Unmarshal(out, &decoded), "wrapped output should decode as a JSON string (out=%s)", out)
	check.NotContains(decoded, "secret://leaked", "secret survived scrub")
	check.Contains(decoded, "[REDACTED]", "expected redaction marker in wrapped text")
}

// TestScrubRawJSON_PreservesValidJSON guards the common path: a secret embedded
// in a well-formed JSON string is scrubbed in place and the result stays valid
// JSON without the string-wrap fallback.
func TestScrubRawJSON_PreservesValidJSON(t *testing.T) {
	must := require.New(t)
	in := json.RawMessage(`{"token":"ghp_deadbeefcafef00d"}`)
	out := scrubRawJSON(in, false)

	must.True(json.Valid(out), "scrubRawJSON output is not valid JSON: %s", out)
	var decoded map[string]string
	must.NoError(json.Unmarshal(out, &decoded), "scrubbed output should decode as an object (out=%s)", out)
	must.Equal("[REDACTED]", decoded["token"], "token field")
}

// TestScrubContentBlocks_DecodesFencedSecret pins that a secret the loop
// fenced for the model is redacted in the persisted copy: the fence
// payload is decoded, scrubbed and re-encoded rather than stored as
// base64 of the key.
func TestScrubContentBlocks_DecodesFencedSecret(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const key = "sk-ant-api03-QUJDZGVmR0hJSktMbW5vcFFSU1RVdld4eXow"
	encoded := base64.StdEncoding.EncodeToString([]byte(key))
	fence := func(s string) string { return security.Spotlight(s) }
	structured, err := json.Marshal(map[string]string{"text": "x " + fence(key)})
	must.NoError(err)
	turn := types.TurnRecord{ModelInput: types.ModelInput{Messages: []types.Message{{
		Role: "user",
		Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "a", Content: "prefix " + fence(key) + " suffix"},
			{Type: "tool_result", ToolUseID: "b", Content: fence("whole block with " + key)},
			{Type: "tool_result", ToolUseID: "c", Content: "ok", Structured: structured},
			{Type: "tool_result", ToolUseID: "d", Content: "benign " + fence("plain words")},
			{Type: "tool_result", ToolUseID: "e", Content: "notes mention " + security.SpotlightOpenTag + " tags.\nTOKEN=" + fence(key) + "\n"},
		},
	}}}}
	got := scrubTurnRecord(turn, false).ModelInput.Messages[0].Content
	all, _ := json.Marshal(got)
	must.NotContains(string(all), key, "persisted blocks carry the key")
	must.NotContains(string(all), encoded, "persisted blocks carry the key's base64")
	check.Equal("prefix "+fence("[REDACTED]")+" suffix", got[0].Content, "span fence")
	check.Equal(fence("whole block with [REDACTED]"), got[1].Content, "whole-block fence")
	check.Equal("benign "+fence("plain words"), got[3].Content, "benign block should be unchanged")
}

// TestScrubRawJSON_PEMKeyAsLineArray covers read_file's structured shape:
// a key held one line per array element is redacted as one match and the
// payload keeps its object shape rather than taking the string-wrap path.
func TestScrubRawJSON_PEMKeyAsLineArray(t *testing.T) {
	must := require.New(t)
	body := []string{
		"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun",
		"VTLw7onLRnrq0/IzW7yWR7QkrmBL7jTKEn5u+qKhbwKfBstIs+bMY2Zkp18gnTxK",
	}
	lines := append(append([]string{"x", "-----BEGIN RSA PRIVATE KEY-----"}, body...), "-----END RSA PRIVATE KEY-----", "y")
	in, err := json.Marshal(map[string][]string{"lines": lines})
	must.NoError(err)
	out := scrubRawJSON(in, false)
	var decoded struct{ Lines []string }
	must.NoError(json.Unmarshal(out, &decoded), "scrubbed output should decode as an object (out=%s)", out)
	must.Equal([]string{"x", "[REDACTED]", "y"}, decoded.Lines)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestJSONLTraceEmitter_FinishWriteFailureReturnsTrace(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	emitter := NewJSONLTraceEmitter(failingWriter{}, false)
	emitter.Start("run-w", nil)
	emitter.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 10, Output: 5}})

	tr, err := emitter.Finish(context.Background(), "timeout")
	must.ErrorContains(err, "disk full")
	must.NotNil(tr, "Finish should return the assembled trace alongside the write error")
	check.Equal("run-w", tr.ID)
	check.Equal("timeout", tr.Outcome)
	check.Equal(1, tr.Turns)
}

func TestJSONLTraceEmitter_RunStartedCarriesHarnessVersion(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-version", nil)

	started := pickEvent(t, readEvents(t, buf.Bytes()), EventKindRunStarted)
	must.Equal(version.Full(), started.HarnessVersion)
}

func TestJSONLTraceEmitter_RecordSystemInstructions_WritesScrubbedPromptAndDigest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-prompt", nil)

	prompt := "You are an agent.\nDeploy with key sk-ant-api03-promptsecret please."
	emitter.RecordSystemInstructions(prompt)

	must.NotContains(buf.String(), "sk-ant-api03-promptsecret", "system prompt secret reached the trace")
	ev := pickEvent(t, readEvents(t, buf.Bytes()), EventKindSystemPrompt)
	check.Equal("run-prompt", ev.RunID)
	check.Contains(ev.SystemPrompt, "[REDACTED]", "system prompt should have the secret redacted")
	check.True(strings.HasPrefix(ev.SystemPrompt, "You are an agent."), "system prompt should keep its leading text")
	sum := sha256.Sum256([]byte(prompt))
	check.Equal(hex.EncodeToString(sum[:]), ev.SystemPromptSHA256, "digest of the prompt as sent")
}

func TestJSONLTraceEmitter_RecordSystemInstructions_RedactionDisabledWritesVerbatim(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, true)
	emitter.Start("run-debug", nil)

	prompt := "key sk-ant-api03-promptsecret"
	emitter.RecordSystemInstructions(prompt)

	ev := pickEvent(t, readEvents(t, buf.Bytes()), EventKindSystemPrompt)
	must.Equal(prompt, ev.SystemPrompt, "system prompt should be written verbatim under --debug")
}

func TestJSONLTraceEmitter_RecordTurnRecord_PreservesRequestParamsAndResponseModel(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-request", nil)

	want := types.RequestParams{
		Temperature:     types.Float64Ptr(0),
		MaxTokens:       4096,
		ReasoningEffort: "high",
		ToolChoice:      types.ToolChoiceRequired,
		CacheKey:        "0123456789abcdef",
	}
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn:          1,
		ModelInput:    types.ModelInput{Model: "m", Request: &want},
		ResponseModel: "m-20260101",
	})

	ev := pickEvent(t, readEvents(t, buf.Bytes()), EventKindTurnRecord)
	check.Equal("m-20260101", ev.ResponseModel)
	must.NotNil(ev.ModelInput, "turn_record dropped the model input")
	must.NotNil(ev.ModelInput.Request, "turn_record dropped the request params")
	must.Equal(want, *ev.ModelInput.Request)
	check.Contains(buf.String(), `"toolChoice":"required"`, "wire form should carry toolChoice")
	check.Contains(buf.String(), `"temperature":0`, "wire form should carry an explicit zero temperature")
}

func TestJSONLTraceEmitter_CorrelationScrubbedInPersistedConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	key := "sk-ant-api03-" + strings.Repeat("B", 40)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	config := &types.RunConfig{
		RunID: "run-corr",
		Mode:  "execution",
		Correlation: &types.CorrelationConfig{
			TaskID: "task-7",
			URL:    "https://cp.example.com/runs/42?token=" + key,
			Labels: map[string]string{"agent_profile": key},
		},
	}
	emitter.Start("run-corr", config)
	trace, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	check.NotContains(buf.String(), key, "JSONL output should not carry the unscrubbed key")
	started := pickEvent(t, readEvents(t, buf.Bytes()), EventKindRunStarted)
	must.NotNil(started.Config, "run_started has no config")
	must.NotNil(started.Config.Correlation, "run_started config lost the correlation block")
	got := started.Config.Correlation
	check.Equal("task-7", got.TaskID, "run_started correlation task ID")
	check.Equal("https://cp.example.com/runs/42?token=[REDACTED]", got.URL, "run_started correlation URL")
	check.NotContains(trace.Config.Correlation.Labels["agent_profile"], key, "RunTrace correlation label should be scrubbed")
	check.Contains(config.Correlation.URL, key, "scrubbing must not reach the live config")
}
