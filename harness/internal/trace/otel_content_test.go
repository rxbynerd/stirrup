package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
// default-off contract (issue #413 AC): with captureContent unset, a
// RecordTurnRecord and a RecordSystemInstructions change nothing — the
// span count and the turn span's attribute set are identical to the
// pre-capture emitter, and no gen_ai content key appears anywhere.
func TestOTelTraceEmitter_RecordTurnRecord_NoOpWithoutCapture(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-no-capture", nil)
	emitter.RecordSystemInstructions("You are a coding agent.")
	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		StopReason: "end_turn",
		DurationMs: 1200,
	})
	// Carries an ID: with capture off this must emit immediately (never
	// buffer) and stay content-free even after the record arrives.
	emitter.RecordToolCall(types.ToolCallTrace{ID: "tu-2", Name: "read_file", DurationMs: 5, Success: true})
	record := captureTurnRecord()
	record.ToolCalls = []types.ToolCallRecord{{
		ID: "tu-2", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`), Output: "package main",
	}}
	emitter.RecordTurnRecord(record)
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans (turn + tool + root) with capture off, got %d", len(spans))
	}
	for _, span := range spans {
		for _, key := range []string{
			genAIInputMessagesKey, genAIOutputMessagesKey, genAISystemInstructionsKey,
			genAIToolCallIDKey, genAIToolCallArgumentsKey, genAIToolCallResultKey,
		} {
			if _, ok := spanAttrString(span, key); ok {
				t.Errorf("span %q carries %q with capture off — content leaked", span.Name, key)
			}
		}
	}
	// The turn span keeps its historical counter attributes.
	turn := findSpan(t, exporter, "turn[1]")
	assertIntAttribute(t, turn, genAIUsageInputTokens, 100)
	assertAttribute(t, turn, genAIOperationNameKey, "chat")
}

// TestOTelTraceEmitter_CaptureContent_EmitsGenAIMessages pins the
// issue #413 Part A acceptance criterion: with the toggle on, the
// turn span carries non-empty gen_ai.input.messages /
// gen_ai.output.messages / gen_ai.system_instructions alongside its
// existing counter attributes, and each value is valid JSON in the
// semconv message shape.
func TestOTelTraceEmitter_CaptureContent_EmitsGenAIMessages(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-capture", nil)
	emitter.RecordSystemInstructions("You are a coding agent operating in planning mode.")
	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		ToolCalls:  1,
		StopReason: "tool_use",
		DurationMs: 1500,
	})
	emitter.RecordTurnRecord(captureTurnRecord())
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Exactly one turn[1] span: the summary and the record merge, they
	// do not produce a counters span plus a content span.
	var turnSpans int
	for _, s := range exporter.GetSpans() {
		if s.Name == "turn[1]" {
			turnSpans++
		}
	}
	if turnSpans != 1 {
		t.Fatalf("expected exactly 1 turn[1] span, got %d", turnSpans)
	}

	turn := findSpan(t, exporter, "turn[1]")

	// Counter attributes survive the merge.
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
			Type    string          `json:"type"`
			Content string          `json:"content"`
			ID      string          `json:"id"`
			Result  string          `json:"result"`
			Name    string          `json:"name"`
			Args    json.RawMessage `json:"arguments"`
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
	if input[1].Parts[0].Type != "tool_call_response" || input[1].Parts[0].ID != "tu-1" || input[1].Parts[0].Result != "main.go\nREADME.md" {
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
// of TestJSONLTraceEmitter_RecordTurnRecord_Scrubs (issue #413 AC):
// secret-shaped substrings in message content, model output, tool
// arguments, or the system prompt must never reach an exported span
// attribute in raw form. The scrubber runs before any attribute is
// built, so the in-memory exporter sees only redacted content.
func TestOTelTraceEmitter_RecordTurnRecord_Scrubs(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-otel-scrub", nil)
	emitter.RecordSystemInstructions("context: the deploy key is sk-ant-api03-sysleak do not reveal it")
	emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 10})
	// A buffered tool call whose record carries secret-shaped arguments
	// and output, so the all-spans scan below covers the execute_tool
	// content attributes too.
	emitter.RecordToolCall(types.ToolCallTrace{ID: "tu-1", Name: "run_command", DurationMs: 5, Success: true})
	emitter.RecordTurnRecord(types.TurnRecord{
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

// TestOTelTraceEmitter_CaptureContent_UnpairedRecordEmitsContentSpan
// pins the fallback for a transcript record with no buffered summary
// (the loop always summarises first, so this is the defensive path for
// a forwarded sub-agent record arriving unpaired): a content-only
// turn[N] span is emitted rather than the transcript being silently
// dropped, with zero duration (wall clock at delivery; no summary
// means no duration to derive timing from) and no counter attributes.
func TestOTelTraceEmitter_CaptureContent_UnpairedRecordEmitsContentSpan(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-unpaired", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 3,
		ModelOutput: []types.ContentBlock{
			{Type: "text", Text: "orphaned transcript"},
		},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	span := findSpan(t, exporter, "turn[3]")
	out, ok := spanAttrString(span, genAIOutputMessagesKey)
	if !ok || !strings.Contains(out, "orphaned transcript") {
		t.Errorf("unpaired record should emit its content, got %q", out)
	}
	if _, ok := spanAttrString(span, genAIInputMessagesKey); ok {
		t.Error("record with no input messages must not carry gen_ai.input.messages")
	}
	// No paired summary → no counter attributes on the fallback span.
	for _, attr := range span.Attributes {
		if string(attr.Key) == genAIUsageInputTokens {
			t.Error("unpaired content span must not carry token counters")
		}
	}
	if !span.EndTime.Equal(span.StartTime) {
		t.Errorf("unpaired span should be zero-duration (start %v, end %v)", span.StartTime, span.EndTime)
	}
}

// TestOTelTraceEmitter_CaptureContent_FlushesUnmatchedTurnAtFinish pins
// the Finish-time flush: a turn whose RecordTurnRecord never arrives
// (the loop's empty-stop-reason error return) still produces its
// counter span — buffering for the merge must never lose a turn.
func TestOTelTraceEmitter_CaptureContent_FlushesUnmatchedTurnAtFinish(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-flush", nil)
	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 42, Output: 7},
		StopReason: "",
		DurationMs: 300,
	})
	if _, err := emitter.Finish(context.Background(), "error"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	turn := findSpan(t, exporter, "turn[1]")
	assertIntAttribute(t, turn, genAIUsageInputTokens, 42)
	if _, ok := spanAttrString(turn, genAIInputMessagesKey); ok {
		t.Error("flushed unmatched turn must not carry content attributes")
	}
}

// TestOTelTraceEmitter_CaptureContent_PairsByRunID pins the forwarded
// sub-agent disambiguation: a child run's turn N (tagged with the
// child's RunID by NestedJSONLEmitter) must merge onto the child's own
// buffered summary, not the parent's same-numbered pending turn.
func TestOTelTraceEmitter_CaptureContent_PairsByRunID(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-parent", nil)
	// Parent's turn 1 is pending (its record arrives last).
	emitter.RecordTurn(types.TurnTrace{
		Turn: 1, Tokens: types.TokenUsage{Input: 1000}, StopReason: "tool_use", DurationMs: 50,
	})
	// Child's turn 1 arrives while the parent's is pending.
	emitter.RecordTurn(types.TurnTrace{
		Turn: 1, RunID: "child-run", ParentRunID: "run-parent",
		Tokens: types.TokenUsage{Input: 77}, StopReason: "end_turn", DurationMs: 20,
	})
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1, RunID: "child-run", ParentRunID: "run-parent",
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "child answer"}},
	})
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn:        1,
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "parent answer"}},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	var childSpan, parentSpan *tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.Name != "turn[1]" {
			continue
		}
		span := s
		out, _ := spanAttrString(span, genAIOutputMessagesKey)
		switch {
		case strings.Contains(out, "child answer"):
			childSpan = &span
		case strings.Contains(out, "parent answer"):
			parentSpan = &span
		}
	}
	if childSpan == nil || parentSpan == nil {
		t.Fatal("expected one turn[1] span with child content and one with parent content")
	}
	// The content landed on the span carrying the matching summary's
	// token counters — proof the merge keyed on RunID, not arrival
	// order.
	assertIntAttribute(t, *childSpan, genAIUsageInputTokens, 77)
	assertIntAttribute(t, *parentSpan, genAIUsageInputTokens, 1000)
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
	emitter.RecordTurn(types.TurnTrace{Turn: 0, StopReason: "tool_use", DurationMs: 10})
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 0,
		ModelInput: types.ModelInput{Messages: []types.Message{{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "the seed prompt"}},
		}}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "intermediate answer"}},
	})

	// A forwarded sub-agent record between the parent's turns: must not
	// leak into the run-level slots.
	emitter.RecordTurn(types.TurnTrace{Turn: 0, RunID: "child-run", ParentRunID: "run-root-io", StopReason: "end_turn", DurationMs: 5})
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 0, RunID: "child-run", ParentRunID: "run-root-io",
		ModelInput: types.ModelInput{Messages: []types.Message{{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "child sub-task"}},
		}}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "child answer"}},
	})

	// Parent turn 1: history embeds the seed; output is the final answer.
	emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 10})
	emitter.RecordTurnRecord(types.TurnRecord{
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

	root := findSpan(t, exporter, "run")

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

// TestOTelTraceEmitter_CaptureContent_ToolSpansCarryIO pins the W3
// acceptance criterion: with capture on, a tool call's counters
// (buffered at RecordToolCall time, timestamps frozen) and its
// arguments/result (delivered by the enclosing turn's record) land on
// one execute_tool span carrying gen_ai.tool.call.{id,arguments,result}.
func TestOTelTraceEmitter_CaptureContent_ToolSpansCarryIO(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-tool-io", nil)
	emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "tool_use", DurationMs: 40})
	emitter.RecordToolCall(types.ToolCallTrace{ID: "tu-9", Name: "read_file", DurationMs: 12, Success: true})

	// Nothing exports until the record arrives: the summary is buffered.
	for _, s := range exporter.GetSpans() {
		if strings.HasPrefix(s.Name, "execute_tool") {
			t.Fatalf("tool span %q exported before its record arrived — buffering broken", s.Name)
		}
	}

	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1,
		ModelOutput: []types.ContentBlock{
			{Type: "tool_use", ID: "tu-9", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
		},
		ToolCalls: []types.ToolCallRecord{{
			ID:         "tu-9",
			Name:       "read_file",
			Input:      json.RawMessage(`{"path":"main.go"}`),
			Output:     "package main",
			DurationMs: 12,
			Success:    true,
		}},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	span := findSpan(t, exporter, "execute_tool read_file")

	// Counters from the buffered summary.
	assertAttribute(t, span, genAIToolNameKey, "read_file")
	assertAttribute(t, span, genAIOperationNameKey, "execute_tool")

	// Content from the record.
	assertAttribute(t, span, genAIToolCallIDKey, "tu-9")
	args, _ := spanAttrString(span, genAIToolCallArgumentsKey)
	if args != `{"path":"main.go"}` {
		t.Errorf("gen_ai.tool.call.arguments = %q, want the call input", args)
	}
	result, _ := spanAttrString(span, genAIToolCallResultKey)
	if result != "package main" {
		t.Errorf("gen_ai.tool.call.result = %q, want the call output", result)
	}

	// Timing frozen at RecordToolCall time: a real 12ms duration, not
	// the zero-duration record-delivery fallback.
	if got := span.EndTime.Sub(span.StartTime); got != 12*time.Millisecond {
		t.Errorf("span duration = %v, want the frozen 12ms from the buffered summary", got)
	}

	// Exactly one execute_tool span: pairing must not double-emit.
	var toolSpans int
	for _, s := range exporter.GetSpans() {
		if strings.HasPrefix(s.Name, "execute_tool") {
			toolSpans++
		}
	}
	if toolSpans != 1 {
		t.Errorf("expected exactly 1 execute_tool span, got %d", toolSpans)
	}
}

// TestOTelTraceEmitter_CaptureContent_EmptyIDToolEmitsImmediately pins
// the un-keyable path: a call without a tool_use ID cannot pair, so it
// emits a plain span at RecordToolCall time, and the record walk skips
// ID-less entries rather than emitting a duplicate content span.
func TestOTelTraceEmitter_CaptureContent_EmptyIDToolEmitsImmediately(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-tool-noid", nil)
	emitter.RecordToolCall(types.ToolCallTrace{Name: "shell", DurationMs: 3, Success: true})

	span := findSpan(t, exporter, "execute_tool shell")
	if _, ok := spanAttrString(span, genAIToolCallArgumentsKey); ok {
		t.Error("immediate-path span must not carry content attributes")
	}

	emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 10})
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn:      1,
		ToolCalls: []types.ToolCallRecord{{Name: "shell", Input: json.RawMessage(`{}`), Output: "ok"}},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	var toolSpans int
	for _, s := range exporter.GetSpans() {
		if strings.HasPrefix(s.Name, "execute_tool") {
			toolSpans++
		}
	}
	if toolSpans != 1 {
		t.Errorf("ID-less call must produce exactly 1 span, got %d", toolSpans)
	}
}

// TestOTelTraceEmitter_CaptureContent_DuplicateToolIDFlushesPrior pins
// the defensive duplicate handling: a second summary under the same
// (RunID, ID) before any record flushes the first as a plain span
// (last-wins, mirroring RecordTurn's duplicate-summary handling), and
// the record then pairs with the survivor.
func TestOTelTraceEmitter_CaptureContent_DuplicateToolIDFlushesPrior(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-tool-dup", nil)
	emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "tool_use", DurationMs: 5})
	emitter.RecordToolCall(types.ToolCallTrace{ID: "tu-dup", Name: "first_call", DurationMs: 1, Success: true})
	emitter.RecordToolCall(types.ToolCallTrace{ID: "tu-dup", Name: "second_call", DurationMs: 2, Success: true})

	// The stale first summary flushed plain.
	flushed := findSpan(t, exporter, "execute_tool first_call")
	if _, ok := spanAttrString(flushed, genAIToolCallResultKey); ok {
		t.Error("flushed duplicate must not carry content")
	}

	emitter.RecordTurnRecord(types.TurnRecord{
		Turn:      1,
		ToolCalls: []types.ToolCallRecord{{ID: "tu-dup", Name: "second_call", Output: "paired"}},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	paired := findSpan(t, exporter, "execute_tool second_call")
	result, _ := spanAttrString(paired, genAIToolCallResultKey)
	if result != "paired" {
		t.Errorf("surviving summary should pair with the record, result = %q", result)
	}
}

// TestOTelTraceEmitter_CaptureContent_FlushesUnmatchedToolAtFinish pins
// the Finish-time flush: a buffered tool call whose turn record never
// arrives (the loop's error returns skip RecordTurnRecord) still
// produces its counter span — buffering must never lose a call.
func TestOTelTraceEmitter_CaptureContent_FlushesUnmatchedToolAtFinish(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-tool-flush", nil)
	emitter.RecordToolCall(types.ToolCallTrace{
		ID: "tu-orphan", Name: "run_command", DurationMs: 7, Success: false,
		ErrorReason: "exit status 1", ErrorCategory: "handler_error",
	})
	if _, err := emitter.Finish(context.Background(), "error"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	span := findSpan(t, exporter, "execute_tool run_command")
	assertAttribute(t, span, errorTypeKey, "handler_error")
	if _, ok := spanAttrString(span, genAIToolCallResultKey); ok {
		t.Error("flushed unmatched tool call must not carry content attributes")
	}
}

// TestOTelTraceEmitter_CaptureContent_UnpairedToolRecordSynthesisesSpan
// pins the inverse fallback: a record entry with no buffered summary (a
// forwarded sub-agent record arriving unpaired) emits a content span
// with counters synthesised from the record — which, unlike the
// unpaired-turn fallback, carries a real duration to derive timing from.
func TestOTelTraceEmitter_CaptureContent_UnpairedToolRecordSynthesisesSpan(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-tool-unpaired", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 2, RunID: "child-run", ParentRunID: "run-tool-unpaired",
		ToolCalls: []types.ToolCallRecord{{
			ID: "tu-x", Name: "write_file", Input: json.RawMessage(`{"path":"a"}`),
			Output: "written", DurationMs: 30, Success: true,
		}},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	span := findSpan(t, exporter, "execute_tool write_file")
	result, _ := spanAttrString(span, genAIToolCallResultKey)
	if result != "written" {
		t.Errorf("synthesised span result = %q, want the record output", result)
	}
	if got := span.EndTime.Sub(span.StartTime); got != 30*time.Millisecond {
		t.Errorf("span duration = %v, want the record's 30ms", got)
	}
}

// TestOTelTraceEmitter_CaptureContent_ToolPairsByRunID pins the
// sub-agent disambiguation on the tool path: a parent call and a
// forwarded child call sharing a tool_use ID value pair independently —
// only the (RunID, ID) composite key keeps them apart.
func TestOTelTraceEmitter_CaptureContent_ToolPairsByRunID(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()

	emitter.Start("run-tool-runs", nil)
	emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "tool_use", DurationMs: 5})
	emitter.RecordToolCall(types.ToolCallTrace{ID: "tu-1", Name: "parent_tool", DurationMs: 1, Success: true})
	emitter.RecordToolCall(types.ToolCallTrace{ID: "tu-1", RunID: "child-run", Name: "child_tool", DurationMs: 2, Success: true})

	// Child record first: must pair with the child's buffered call only.
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn: 1, RunID: "child-run", ParentRunID: "run-tool-runs",
		ToolCalls: []types.ToolCallRecord{{ID: "tu-1", Name: "child_tool", Output: "child result"}},
	})
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn:      1,
		ToolCalls: []types.ToolCallRecord{{ID: "tu-1", Name: "parent_tool", Output: "parent result"}},
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	child := findSpan(t, exporter, "execute_tool child_tool")
	if result, _ := spanAttrString(child, genAIToolCallResultKey); result != "child result" {
		t.Errorf("child span result = %q, want %q", result, "child result")
	}
	parent := findSpan(t, exporter, "execute_tool parent_tool")
	if result, _ := spanAttrString(parent, genAIToolCallResultKey); result != "parent result" {
		t.Errorf("parent span result = %q, want %q", result, "parent result")
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

	root := findSpan(t, exporter, "run")
	for _, key := range []string{genAIInputMessagesKey, genAIOutputMessagesKey, genAISystemInstructionsKey} {
		if val, ok := spanAttrString(root, key); ok {
			t.Errorf("root span carries %q (%q) with no records", key, val)
		}
	}
}
