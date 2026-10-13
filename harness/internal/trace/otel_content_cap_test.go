package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/types"
)

func textMessage(role, text string) types.Message {
	return types.Message{Role: role, Content: []types.ContentBlock{{Type: "text", Text: text}}}
}

func attrMapOf(attrs []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func TestAppendContentAttribute_UnderCapUnchanged(t *testing.T) {
	value := genAIInputMessagesJSON([]types.Message{textMessage("user", "hello")})
	for _, maxBytes := range []int{0, len(value)} {
		got := attrMapOf(appendContentAttribute(nil, genAIInputMessagesKey, value, maxBytes))
		if got[genAIInputMessagesKey].AsString() != value {
			t.Errorf("maxBytes %d: value changed to %q", maxBytes, got[genAIInputMessagesKey].AsString())
		}
		if _, ok := got[contentTruncatedKey(genAIInputMessagesKey)]; ok {
			t.Errorf("maxBytes %d: truncation marker set on an uncapped value", maxBytes)
		}
	}
}

func TestAppendContentAttribute_MarksTruncation(t *testing.T) {
	value := genAIInputMessagesJSON([]types.Message{textMessage("user", strings.Repeat("x", 600))})
	got := attrMapOf(appendContentAttribute(nil, genAIInputMessagesKey, value, 256))
	if !got["stirrup.gen_ai.input.messages.truncated"].AsBool() {
		t.Errorf("expected stirrup.gen_ai.input.messages.truncated=true, got %v", got)
	}
	if n := len(got[genAIInputMessagesKey].AsString()); n > 256 {
		t.Errorf("capped value is %d bytes, want <= 256", n)
	}
}

func TestCapContentValue_InputMessagesKeepNewest(t *testing.T) {
	msgs := []types.Message{
		textMessage("user", "first "+strings.Repeat("a", 200)),
		textMessage("assistant", "second "+strings.Repeat("b", 200)),
		textMessage("user", "third "+strings.Repeat("c", 200)),
	}
	value := genAIInputMessagesJSON(msgs)
	capped := capContentValue(genAIInputMessagesKey, value, 600)

	var got []genAIMessage
	if err := json.Unmarshal([]byte(capped), &got); err != nil {
		t.Fatalf("capped input messages are not valid JSON: %v\n%s", err, capped)
	}
	if len(capped) > 600 {
		t.Errorf("capped value is %d bytes, want <= 600", len(capped))
	}
	if len(got) != 2 || !strings.HasPrefix(got[0].Parts[0].Content, "second") || !strings.HasPrefix(got[1].Parts[0].Content, "third") {
		t.Errorf("expected the two newest messages in order, got %s", capped)
	}
}

func TestCapContentValue_OutputMessagesKeepLeadingParts(t *testing.T) {
	value := genAIOutputMessagesJSON([]types.ContentBlock{
		{Type: "text", Text: "lead " + strings.Repeat("a", 200)},
		{Type: "tool_use", ID: "tu-1", Name: "read_file", Input: json.RawMessage(`{"path":"` + strings.Repeat("p", 300) + `"}`)},
	}, "tool_use")
	capped := capContentValue(genAIOutputMessagesKey, value, 300)

	var got []genAIMessage
	if err := json.Unmarshal([]byte(capped), &got); err != nil {
		t.Fatalf("capped output messages are not valid JSON: %v\n%s", err, capped)
	}
	if len(capped) > 300 {
		t.Errorf("capped value is %d bytes, want <= 300", len(capped))
	}
	if len(got) != 1 || got[0].FinishReason != "tool_use" || len(got[0].Parts) != 1 || got[0].Parts[0].Type != "text" {
		t.Errorf("expected the assistant message with only its leading text part, got %s", capped)
	}
}

func TestCapContentValue_ShrinksOversizedSinglePart(t *testing.T) {
	// Multi-byte runes and characters JSON escapes, so the cut must land
	// on a rune boundary and account for escaping.
	text := strings.Repeat("é<\"\n", 400)
	value := genAISystemInstructionsJSON(text)
	capped := capContentValue(genAISystemInstructionsKey, value, 256)

	var got []genAIPart
	if err := json.Unmarshal([]byte(capped), &got); err != nil {
		t.Fatalf("capped system instructions are not valid JSON: %v\n%s", err, capped)
	}
	// One repeated unit encodes to 12 bytes, so a tight cut lands within
	// that of the cap.
	if len(capped) > 256 || len(capped) < 256-12 {
		t.Errorf("capped value is %d bytes, want within 12 bytes below 256", len(capped))
	}
	if len(got) != 1 || got[0].Type != "text" || got[0].Content == "" || !strings.HasPrefix(text, got[0].Content) {
		t.Errorf("expected one text part holding a prefix of the prompt, got %s", capped)
	}
	if !utf8.ValidString(got[0].Content) {
		t.Errorf("shortened text is not valid UTF-8: %q", got[0].Content)
	}
}

func TestCapContentValue_ShrinksOversizedToolResponseInNewestMessage(t *testing.T) {
	value := genAIInputMessagesJSON([]types.Message{
		textMessage("user", "list files"),
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "tu-1", Content: strings.Repeat("r", 2000)}}},
	})
	capped := capContentValue(genAIInputMessagesKey, value, 300)

	var got []genAIMessage
	if err := json.Unmarshal([]byte(capped), &got); err != nil {
		t.Fatalf("capped input messages are not valid JSON: %v\n%s", err, capped)
	}
	if len(capped) > 300 {
		t.Errorf("capped value is %d bytes, want <= 300", len(capped))
	}
	if len(got) != 1 || len(got[0].Parts) != 1 || got[0].Parts[0].Type != "tool_call_response" ||
		got[0].Parts[0].ID != "tu-1" || got[0].Parts[0].Response == nil || *got[0].Parts[0].Response == "" {
		t.Errorf("expected the newest message with a shortened tool response, got %s", capped)
	}
}

func TestCapContentValue_ToolArgumentsBecomeJSONString(t *testing.T) {
	args := `{"path":"` + strings.Repeat("p", 600) + `"}`
	capped := capContentValue(genAIToolCallArgumentsKey, args, 256)

	var got string
	if err := json.Unmarshal([]byte(capped), &got); err != nil {
		t.Fatalf("capped arguments are not a JSON string: %v\n%s", err, capped)
	}
	if len(capped) > 256 {
		t.Errorf("capped value is %d bytes, want <= 256", len(capped))
	}
	if !strings.HasPrefix(args, got) || got == "" {
		t.Errorf("expected a prefix of the arguments, got %q", got)
	}
}

func TestCapContentValue_ToolResultTruncatedOnRuneBoundary(t *testing.T) {
	result := strings.Repeat("ü", 300)
	capped := capContentValue(genAIToolCallResultKey, result, 257)
	if len(capped) > 257 || !utf8.ValidString(capped) || !strings.HasPrefix(result, capped) {
		t.Errorf("tool result not truncated to a valid UTF-8 prefix within 257 bytes: %d bytes", len(capped))
	}
}

// TestOTelTraceEmitter_CaptureContentMaxBytes_CapsSpanAttributes drives a
// chat span and a tool span through an emitter built with a cap and
// checks every content attribute is within the cap, valid, and marked.
func TestOTelTraceEmitter_CaptureContentMaxBytes_CapsSpanAttributes(t *testing.T) {
	const maxBytes = 256
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	emitter := newOTelTraceEmitter(tp, true, maxBytes, false)

	emitter.Start("run-cap", nil)
	emitter.RecordSystemInstructions(strings.Repeat("system prompt ", 100))
	big := strings.Repeat("content ", 100)
	recordChatSpan(emitter, types.TurnTrace{Turn: 1, StopReason: "end_turn"}, types.TurnRecord{
		Turn: 1,
		ModelInput: types.ModelInput{Messages: []types.Message{
			textMessage("user", big),
			textMessage("assistant", big),
			textMessage("user", "short follow-up"),
		}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: big}},
	})
	tool := emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-1", Name: "read_file", Known: true})
	tool.End(ToolSpanEnd{Record: &types.ToolCallRecord{
		ID:     "tu-1",
		Input:  json.RawMessage(`{"path":"` + strings.Repeat("p", 400) + `"}`),
		Output: big,
	}})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	chat := findSpan(t, exporter, "chat")
	for _, key := range []string{genAIInputMessagesKey, genAIOutputMessagesKey, genAISystemInstructionsKey} {
		assertCappedJSON(t, chat, key, maxBytes)
	}
	toolSpan := findSpan(t, exporter, "execute_tool read_file")
	assertCappedJSON(t, toolSpan, genAIToolCallArgumentsKey, maxBytes)
	if v, _ := spanAttrString(toolSpan, genAIToolCallResultKey); len(v) > maxBytes {
		t.Errorf("tool result is %d bytes, want <= %d", len(v), maxBytes)
	}
	root := findSpan(t, exporter, "invoke_agent stirrup")
	assertCappedJSON(t, root, genAIInputMessagesKey, maxBytes)
}

func assertCappedJSON(t *testing.T, span tracetest.SpanStub, key string, maxBytes int) {
	t.Helper()
	v, ok := spanAttrString(span, key)
	if !ok {
		t.Errorf("%s: %s missing", span.Name, key)
		return
	}
	if len(v) > maxBytes {
		t.Errorf("%s: %s is %d bytes, want <= %d", span.Name, key, len(v), maxBytes)
	}
	if !json.Valid([]byte(v)) {
		t.Errorf("%s: %s is not valid JSON: %s", span.Name, key, v)
	}
	found := false
	for _, attr := range span.Attributes {
		if string(attr.Key) == contentTruncatedKey(key) && attr.Value.AsBool() {
			found = true
		}
	}
	if !found {
		t.Errorf("%s: %s missing its truncation marker", span.Name, key)
	}
}
