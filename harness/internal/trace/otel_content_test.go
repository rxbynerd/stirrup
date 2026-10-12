package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// newTestOTelEmitterWithCapture builds an in-memory emitter with
// content capture opted in, mirroring what the factory constructs for
// traceEmitter.captureContent=true. The toggle is passed at
// construction time, like the production constructor — captureContent
// is documented as immutable afterwards (the off-path methods read it
// without the mutex), so the helper must not mutate it on a built
// emitter.
func newTestOTelEmitterWithCapture() (*OTelTraceEmitter, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithResource(observability.BuildResource(observability.ResourceOptions{})),
	)
	return newOTelTraceEmitterForTest(tp, true), exporter
}

// findSpan returns the first exported span with the given name, or
// fails the test.
func findSpan(t *testing.T, exporter *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range exporter.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q span found among %d exported spans", name, len(exporter.GetSpans()))
	return tracetest.SpanStub{}
}

// recordChatSpan drives a chat span the way the loop does for one
// completed request; summary.RunID marks a forwarded sub-agent request.
func recordChatSpan(e *OTelTraceEmitter, summary types.TurnTrace, record types.TurnRecord) {
	chat := e.StartChatSpan(e.RootContext(), ChatSpanStart{Turn: summary.Turn, RunID: summary.RunID, ParentRunID: summary.ParentRunID})
	chat.End(ChatSpanEnd{Summary: &summary, Record: &record})
}

// spanAttrString returns the string value of an attribute, and whether
// the key is present at all.
func spanAttrString(span tracetest.SpanStub, key string) (string, bool) {
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			return attr.Value.AsString(), true
		}
	}
	return "", false
}

// captureTurnRecord is a representative transcript covering every part
// shape the content mapper handles: text input, a tool_result in the
// message history, text output, and a tool_use in the model output.
func captureTurnRecord() types.TurnRecord {
	return types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{
			Model: "claude-sonnet-4-6",
			Messages: []types.Message{
				{
					Role: "user",
					Content: []types.ContentBlock{
						{Type: "text", Text: "list the workspace files"},
					},
				},
				{
					Role: "user",
					Content: []types.ContentBlock{
						{Type: "tool_result", ToolUseID: "tu-1", Content: "main.go\nREADME.md"},
					},
				},
			},
		},
		ModelOutput: []types.ContentBlock{
			{Type: "text", Text: "Reading main.go next."},
			{Type: "tool_use", ID: "tu-2", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
		},
	}
}

// TestOTelTraceEmitter_RecordTurnRecord_NoOpWithoutCapture pins the
// default-off contract: with captureContent unset, a
// RecordTurnRecord and a RecordSystemInstructions change nothing — the
// span count and the turn span's attribute set are identical to the
// pre-capture emitter, and no gen_ai content key appears anywhere.
func TestOTelTraceEmitter_RecordTurnRecord_NoOpWithoutCapture(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-no-capture", nil)
	emitter.RecordSystemInstructions("You are a coding agent.")
	summary := types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		StopReason: "end_turn",
		DurationMs: 1200,
	}
	record := captureTurnRecord()
	record.ToolCalls = []types.ToolCallRecord{{
		ID: "tu-2", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`), Output: "package main",
	}}
	recordChatSpan(emitter, summary, record)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-2", Name: "read_file", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{ID: "tu-2", Name: "read_file", DurationMs: 5, Success: true},
		Record:  &record.ToolCalls[0],
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans (chat + tool + root) with capture off, got %d", len(spans))
	}
	for _, span := range spans {
		for _, key := range []string{
			genAIInputMessagesKey, genAIOutputMessagesKey, genAISystemInstructionsKey,
			genAIToolCallArgumentsKey, genAIToolCallResultKey,
		} {
			if _, ok := spanAttrString(span, key); ok {
				t.Errorf("span %q carries %q with capture off — content leaked", span.Name, key)
			}
		}
	}
	// The call ID is a non-sensitive correlation key and is emitted
	// regardless of capture.
	assertAttribute(t, findSpan(t, exporter, "execute_tool read_file"), genAIToolCallIDKey, "tu-2")
	// The chat span keeps its counter attributes.
	turn := findSpan(t, exporter, "chat")
	assertIntAttribute(t, turn, genAIUsageInputTokens, 100)
	assertAttribute(t, turn, genAIOperationNameKey, "chat")
}

// TestOTelTraceEmitter_CaptureContent_EmitsGenAIMessages pins that
// with the toggle on, the chat span carries non-empty gen_ai.input.messages /
// gen_ai.output.messages / gen_ai.system_instructions alongside its
// existing counter attributes, and each value is valid JSON in the
// semconv message shape.
func TestOTelTraceEmitter_CaptureContent_EmitsGenAIMessages(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-capture", nil)
	emitter.RecordSystemInstructions("You are a coding agent operating in planning mode.")
	recordChatSpan(emitter, types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		ToolCalls:  1,
		StopReason: "tool_use",
		DurationMs: 1500,
	}, captureTurnRecord())
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	turn := findSpan(t, exporter, "chat")

	// Counter attributes sit beside the content.
	assertIntAttribute(t, turn, genAIUsageInputTokens, 100)
	assertIntAttribute(t, turn, genAIUsageOutputTokens, 50)
	assertAttribute(t, turn, genAIOperationNameKey, "chat")

	// gen_ai.input.messages: both history messages, in order, with the
	// schema part shapes.
	inputJSON, ok := spanAttrString(turn, genAIInputMessagesKey)
	if !ok || inputJSON == "" {
		t.Fatal("gen_ai.input.messages missing or empty")
	}
	var input []struct {
		Role  string `json:"role"`
		Parts []struct {
			Type     string          `json:"type"`
			Content  string          `json:"content"`
			ID       string          `json:"id"`
			Response string          `json:"response"`
			Name     string          `json:"name"`
			Args     json.RawMessage `json:"arguments"`
		} `json:"parts"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		t.Fatalf("gen_ai.input.messages is not valid JSON: %v\n%s", err, inputJSON)
	}
	if len(input) != 2 || input[0].Role != "user" {
		t.Fatalf("unexpected input messages shape: %s", inputJSON)
	}
	if input[0].Parts[0].Type != "text" || input[0].Parts[0].Content != "list the workspace files" {
		t.Errorf("first input part wrong: %+v", input[0].Parts[0])
	}
	if input[1].Parts[0].Type != "tool_call_response" || input[1].Parts[0].ID != "tu-1" || input[1].Parts[0].Response != "main.go\nREADME.md" {
		t.Errorf("tool_call_response part wrong: %+v", input[1].Parts[0])
	}

	// gen_ai.output.messages: one assistant message with text +
	// tool_call parts and the summary's stop reason as finish_reason.
	outputJSON, ok := spanAttrString(turn, genAIOutputMessagesKey)
	if !ok || outputJSON == "" {
		t.Fatal("gen_ai.output.messages missing or empty")
	}
	var output []struct {
		Role  string `json:"role"`
		Parts []struct {
			Type string          `json:"type"`
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
		} `json:"parts"`
		FinishReason string `json:"finish_reason"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &output); err != nil {
		t.Fatalf("gen_ai.output.messages is not valid JSON: %v\n%s", err, outputJSON)
	}
	if len(output) != 1 || output[0].Role != "assistant" || output[0].FinishReason != "tool_use" {
		t.Fatalf("unexpected output messages shape: %s", outputJSON)
	}
	if len(output[0].Parts) != 2 || output[0].Parts[1].Type != "tool_call" || output[0].Parts[1].Name != "read_file" {
		t.Errorf("tool_call part wrong: %s", outputJSON)
	}
	if string(output[0].Parts[1].Args) != `{"path":"main.go"}` {
		t.Errorf("tool_call arguments wrong: %s", output[0].Parts[1].Args)
	}

	// gen_ai.system_instructions: single text part with the recorded
	// prompt.
	sysJSON, ok := spanAttrString(turn, genAISystemInstructionsKey)
	if !ok || sysJSON == "" {
		t.Fatal("gen_ai.system_instructions missing or empty")
	}
	var sys []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(sysJSON), &sys); err != nil {
		t.Fatalf("gen_ai.system_instructions is not valid JSON: %v\n%s", err, sysJSON)
	}
	if len(sys) != 1 || sys[0].Type != "text" || !strings.Contains(sys[0].Content, "planning mode") {
		t.Errorf("unexpected system instructions shape: %s", sysJSON)
	}
}

// TestOTelTraceEmitter_RecordTurnRecord_Scrubs is the OTel counterpart
// of TestJSONLTraceEmitter_RecordTurnRecord_Scrubs:
// secret-shaped substrings in message content, model output, tool
// arguments, or the system prompt must never reach an exported span
// attribute in raw form. The scrubber runs before any attribute is
// built, so the in-memory exporter sees only redacted content.
func TestOTelTraceEmitter_RecordTurnRecord_Scrubs(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-otel-scrub", nil)
	emitter.RecordSystemInstructions("context: the deploy key is sk-ant-api03-sysleak do not reveal it")
	record := types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{
			Model: "claude-3-5-sonnet-latest",
			Messages: []types.Message{
				{
					Role: "user",
					Content: []types.ContentBlock{
						{Type: "text", Text: "here is my key sk-ant-api03-redactme please"},
						{Type: "tool_result", ToolUseID: "tu-0", Content: "stdout: sk-ant-api03-leak\nstderr: ok"},
					},
				},
			},
		},
		ModelOutput: []types.ContentBlock{
			{Type: "text", Text: "ack, your bearer Bearer ABCDEFG is unsafe"},
			{Type: "tool_use", ID: "tu-1", Name: "run_command", Input: json.RawMessage(`{"cmd":"echo sk-ant-api03-leak-leak"}`)},
		},
		ToolCalls: []types.ToolCallRecord{{
			ID:     "tu-1",
			Name:   "run_command",
			Input:  json.RawMessage(`{"cmd":"echo sk-ant-api03-leak-leak"}`),
			Output: "stdout contains sk-ant-api03-leak",
		}},
	}
	recordChatSpan(emitter, types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 10}, record)
	// A tool call whose record carries secret-shaped arguments and output,
	// so the all-spans scan below covers the execute_tool content
	// attributes too.
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-1", Name: "run_command", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{ID: "tu-1", Name: "run_command", DurationMs: 5, Success: true},
		Record:  &record.ToolCalls[0],
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	var sawRedacted bool
	for _, span := range exporter.GetSpans() {
		for _, attr := range span.Attributes {
			value := attr.Value.String()
			for _, secret := range []string{
				"sk-ant-api03-redactme",
				"sk-ant-api03-leak",
				"sk-ant-api03-sysleak",
				"Bearer ABCDEFG",
			} {
				if strings.Contains(value, secret) {
					t.Errorf("scrubber missed %q in span %q attribute %q:\n%s",
						secret, span.Name, attr.Key, value)
				}
			}
			if strings.Contains(value, "[REDACTED]") {
				sawRedacted = true
			}
		}
	}
	if !sawRedacted {
		t.Error("expected at least one [REDACTED] placeholder proving the scrubber ran")
	}
}

// TestOTelTraceEmitter_CaptureContent_RootSpanIO pins the run-level
// content surface: the root span carries the first parent turn's input
// (the seed prompt), the last parent turn's output (the final assistant
// message), and the system instructions — and forwarded sub-agent
// records contribute to none of them. Backends derive their trace-level
// input/output views from the root span, so a regression here empties
// those panels while leaving every turn span intact.
func TestOTelTraceEmitter_CaptureContent_RootSpanIO(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-root-io", nil)
	emitter.RecordSystemInstructions("You are a coding agent.")

	// Parent turn 0: the seed prompt and an intermediate answer.
	recordChatSpan(emitter, types.TurnTrace{Turn: 0, StopReason: "tool_use", DurationMs: 10}, types.TurnRecord{
		Turn: 0,
		ModelInput: types.ModelInput{Messages: []types.Message{{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "the seed prompt"}},
		}}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "intermediate answer"}},
	})

	// A forwarded sub-agent record between the parent's turns: must not
	// leak into the run-level slots.
	recordChatSpan(emitter, types.TurnTrace{Turn: 0, RunID: "child-run", ParentRunID: "run-root-io", StopReason: "end_turn", DurationMs: 5}, types.TurnRecord{
		Turn: 0, RunID: "child-run", ParentRunID: "run-root-io",
		ModelInput: types.ModelInput{Messages: []types.Message{{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "child sub-task"}},
		}}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "child answer"}},
	})

	// Parent turn 1: history embeds the seed; output is the final answer.
	recordChatSpan(emitter, types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 10}, types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{Messages: []types.Message{{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "the seed prompt"}},
		}, {
			Role:    "assistant",
			Content: []types.ContentBlock{{Type: "text", Text: "intermediate answer"}},
		}}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "the final answer"}},
	})

	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	root := findSpan(t, exporter, "invoke_agent stirrup")

	input, ok := spanAttrString(root, genAIInputMessagesKey)
	if !ok {
		t.Fatal("root span missing gen_ai.input.messages")
	}
	if !strings.Contains(input, "the seed prompt") {
		t.Errorf("root input should carry the seed prompt, got %q", input)
	}
	if strings.Contains(input, "intermediate answer") {
		t.Errorf("root input must be turn 0's input (set-once), not a later turn's history: %q", input)
	}

	output, ok := spanAttrString(root, genAIOutputMessagesKey)
	if !ok {
		t.Fatal("root span missing gen_ai.output.messages")
	}
	if !strings.Contains(output, "the final answer") {
		t.Errorf("root output should carry the last parent turn's output, got %q", output)
	}

	for _, val := range []string{input, output} {
		if strings.Contains(val, "child") {
			t.Errorf("forwarded sub-agent content leaked into root span: %q", val)
		}
	}

	system, ok := spanAttrString(root, genAISystemInstructionsKey)
	if !ok || !strings.Contains(system, "You are a coding agent.") {
		t.Errorf("root span system instructions: got %q (present=%v)", system, ok)
	}
}

// TestOTelTraceEmitter_CaptureContent_RootSpanIOAbsentWithoutRecords
// pins the degraded shape: a run that produced no transcript records
// (every loop error path) finishes with a bare root span — no content
// keys, never empty-string attributes.
func TestOTelTraceEmitter_CaptureContent_RootSpanIOAbsentWithoutRecords(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-root-bare", nil)
	emitter.RecordTurn(types.TurnTrace{Turn: 0, StopReason: "", DurationMs: 5})
	if _, err := emitter.Finish(context.Background(), "error"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	root := findSpan(t, exporter, "invoke_agent stirrup")
	for _, key := range []string{genAIInputMessagesKey, genAIOutputMessagesKey, genAISystemInstructionsKey} {
		if val, ok := spanAttrString(root, key); ok {
			t.Errorf("root span carries %q (%q) with no records", key, val)
		}
	}
}

// TestGenAIParts_DropsThinkingBlocks pins that thinking and
// redacted_thinking blocks never reach span content: they have no GenAI
// part shape and their signatures are provider-opaque.
func TestGenAIParts_DropsThinkingBlocks(t *testing.T) {
	parts := genAIParts([]types.ContentBlock{
		{Type: "thinking", Text: "private plan", ThoughtSignature: "sig-blob"},
		{Type: "text", Text: "answer"},
		{Type: "redacted_thinking", ThoughtSignature: "redacted-blob"},
	})
	if len(parts) != 1 || parts[0].Type != "text" || parts[0].Content != "answer" {
		t.Fatalf("parts = %+v, want only the text part", parts)
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leaked := range []string{"private plan", "sig-blob", "redacted-blob"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("span content carries %q: %s", leaked, raw)
		}
	}
}

// TestGenAIParts_ToolResponseKeyAlwaysPresent pins that a
// tool_call_response part carries the schema-required "response" key,
// including when the tool produced no output.
func TestGenAIParts_ToolResponseKeyAlwaysPresent(t *testing.T) {
	for name, content := range map[string]string{"output": "ok", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			parts := genAIParts([]types.ContentBlock{
				{Type: "tool_result", ToolUseID: "tu-1", Content: content},
			})
			raw, err := json.Marshal(parts)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded []map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got, ok := decoded[0]["response"]; !ok || got != content {
				t.Errorf("response = %v (present=%v), want %q", got, ok, content)
			}
			if _, ok := decoded[0]["result"]; ok {
				t.Errorf("legacy result key present: %s", raw)
			}
		})
	}
}

func countSpanAttr(span tracetest.SpanStub, key string) int {
	n := 0
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			n++
		}
	}
	return n
}

// TestOTelTraceEmitter_ToolCallID_EmittedOnce pins that the execute_tool
// span sets gen_ai.tool.call.id exactly once when the call has an ID,
// with capture on or off, and omits it otherwise.
func TestOTelTraceEmitter_ToolCallID_EmittedOnce(t *testing.T) {
	const id = "tu-path"
	cases := []struct {
		name    string
		capture bool
		id      string
	}{
		{"capture off", false, id},
		{"capture on with record", true, id},
		{"without id", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emitter, exporter := newTestOTelEmitter()
			if tc.capture {
				emitter, exporter = newTestOTelEmitterWithCapture()
			}
			emitter.Start("run-id-paths", nil)
			emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: tc.id, Name: "tool", Known: true}).End(ToolSpanEnd{
				Summary: &types.ToolCallTrace{ID: tc.id, Name: "tool", Success: true},
				Record:  &types.ToolCallRecord{ID: tc.id, Name: "tool", Output: "ok"},
			})
			if _, err := emitter.Finish(context.Background(), "success"); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			span := findSpan(t, exporter, "execute_tool tool")
			wantCount := 1
			if tc.id == "" {
				wantCount = 0
			}
			if got := countSpanAttr(span, genAIToolCallIDKey); got != wantCount {
				t.Fatalf("%s emitted %d times, want %d", genAIToolCallIDKey, got, wantCount)
			}
			if got, _ := spanAttrString(span, genAIToolCallIDKey); got != tc.id {
				t.Errorf("%s = %q, want %q", genAIToolCallIDKey, got, tc.id)
			}
		})
	}
}

// TestOTelTraceEmitter_ToolCallID_Scrubbed pins that a secret-shaped call
// ID is scrubbed on the span unless redaction is disabled.
func TestOTelTraceEmitter_ToolCallID_Scrubbed(t *testing.T) {
	const secretID = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("redactionDisabled=%v", disabled), func(t *testing.T) {
			emitter, exporter := newTestOTelEmitter()
			emitter.redactionDisabled = disabled
			emitter.Start("run-id-scrub", nil)
			emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: secretID, Name: "tool", Known: true}).End(ToolSpanEnd{
				Summary: &types.ToolCallTrace{ID: secretID, Name: "tool", Success: true},
			})
			if _, err := emitter.Finish(context.Background(), "success"); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			want := "[REDACTED]"
			if disabled {
				want = secretID
			}
			got, _ := spanAttrString(findSpan(t, exporter, "execute_tool tool"), genAIToolCallIDKey)
			if got != want {
				t.Errorf("%s = %q, want %q", genAIToolCallIDKey, got, want)
			}
		})
	}
}
