package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	cases := []struct {
		name     string
		maxBytes int
	}{
		{name: "zero limit", maxBytes: 0},
		{name: "limit equal to value length", maxBytes: len(value)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attrMapOf(appendContentAttribute(nil, genAIInputMessagesKey, value, tc.maxBytes))
			require.Equal(t, value, got[genAIInputMessagesKey].AsString())
			require.NotContains(t, got, contentTruncatedKey(genAIInputMessagesKey))
		})
	}
}

func TestAppendContentAttribute_MarksTruncation(t *testing.T) {
	value := genAIInputMessagesJSON([]types.Message{textMessage("user", strings.Repeat("x", 600))})
	got := attrMapOf(appendContentAttribute(nil, genAIInputMessagesKey, value, 256))
	assert.True(t, got["stirrup.gen_ai.input.messages.truncated"].AsBool(), "attributes: %v", got)
	assert.LessOrEqual(t, len(got[genAIInputMessagesKey].AsString()), 256)
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
	require.NoError(t, json.Unmarshal([]byte(capped), &got), "capped input messages are not valid JSON: %s", capped)
	assert.LessOrEqual(t, len(capped), 600)
	require.Len(t, got, 2, "expected the two newest messages, got %s", capped)
	assert.True(t, strings.HasPrefix(got[0].Parts[0].Content, "second"), "got %s", capped)
	assert.True(t, strings.HasPrefix(got[1].Parts[0].Content, "third"), "got %s", capped)
}

func TestCapContentValue_OutputMessagesKeepLeadingParts(t *testing.T) {
	value := genAIOutputMessagesJSON([]types.ContentBlock{
		{Type: "text", Text: "lead " + strings.Repeat("a", 200)},
		{Type: "tool_use", ID: "tu-1", Name: "read_file", Input: json.RawMessage(`{"path":"` + strings.Repeat("p", 300) + `"}`)},
	}, "tool_use")
	capped := capContentValue(genAIOutputMessagesKey, value, 300)

	var got []genAIMessage
	require.NoError(t, json.Unmarshal([]byte(capped), &got), "capped output messages are not valid JSON: %s", capped)
	assert.LessOrEqual(t, len(capped), 300)
	require.Len(t, got, 1, "expected the assistant message only, got %s", capped)
	assert.Equal(t, "tool_use", got[0].FinishReason, "got %s", capped)
	require.Len(t, got[0].Parts, 1, "expected only the leading text part, got %s", capped)
	assert.Equal(t, "text", got[0].Parts[0].Type, "got %s", capped)
}

func TestCapContentValue_ShrinksOversizedSinglePart(t *testing.T) {
	// Multi-byte runes and characters JSON escapes, so the cut must land
	// on a rune boundary and account for escaping.
	text := strings.Repeat("é<\"\n", 400)
	value := genAISystemInstructionsJSON(text)
	capped := capContentValue(genAISystemInstructionsKey, value, 256)

	var got []genAIPart
	require.NoError(t, json.Unmarshal([]byte(capped), &got), "capped system instructions are not valid JSON: %s", capped)
	// One repeated unit encodes to 12 bytes, so a tight cut lands within
	// that of the cap.
	assert.GreaterOrEqual(t, len(capped), 256-12)
	assert.LessOrEqual(t, len(capped), 256)
	require.Len(t, got, 1, "got %s", capped)
	assert.Equal(t, "text", got[0].Type, "got %s", capped)
	assert.NotEmpty(t, got[0].Content, "got %s", capped)
	assert.True(t, strings.HasPrefix(text, got[0].Content), "expected a prefix of the prompt, got %s", capped)
	assert.True(t, utf8.ValidString(got[0].Content), "shortened text is not valid UTF-8: %q", got[0].Content)
}

func TestCapContentValue_ShrinksOversizedToolResponseInNewestMessage(t *testing.T) {
	value := genAIInputMessagesJSON([]types.Message{
		textMessage("user", "list files"),
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "tu-1", Content: strings.Repeat("r", 2000)}}},
	})
	capped := capContentValue(genAIInputMessagesKey, value, 300)

	var got []genAIMessage
	require.NoError(t, json.Unmarshal([]byte(capped), &got), "capped input messages are not valid JSON: %s", capped)
	assert.LessOrEqual(t, len(capped), 300)
	require.Len(t, got, 1, "expected the newest message only, got %s", capped)
	require.Len(t, got[0].Parts, 1, "got %s", capped)
	part := got[0].Parts[0]
	assert.Equal(t, "tool_call_response", part.Type, "got %s", capped)
	assert.Equal(t, "tu-1", part.ID, "got %s", capped)
	require.NotNil(t, part.Response, "got %s", capped)
	assert.NotEmpty(t, *part.Response, "got %s", capped)
}

func TestCapContentValue_ToolArgumentsBecomeJSONString(t *testing.T) {
	args := `{"path":"` + strings.Repeat("p", 600) + `"}`
	capped := capContentValue(genAIToolCallArgumentsKey, args, 256)

	var got string
	require.NoError(t, json.Unmarshal([]byte(capped), &got), "capped arguments are not a JSON string: %s", capped)
	assert.LessOrEqual(t, len(capped), 256)
	assert.NotEmpty(t, got)
	assert.True(t, strings.HasPrefix(args, got), "expected a prefix of the arguments, got %q", got)
}

func TestCapContentValue_ToolResultTruncatedOnRuneBoundary(t *testing.T) {
	result := strings.Repeat("ü", 300)
	capped := capContentValue(genAIToolCallResultKey, result, 257)
	require.LessOrEqual(t, len(capped), 257)
	require.True(t, utf8.ValidString(capped), "capped tool result is not valid UTF-8: %q", capped)
	require.True(t, strings.HasPrefix(result, capped), "capped tool result is not a prefix of the input: %q", capped)
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
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	chat := findSpan(t, exporter, "chat")
	for _, key := range []string{genAIInputMessagesKey, genAIOutputMessagesKey, genAISystemInstructionsKey} {
		assertCappedJSON(t, chat, key, maxBytes)
	}
	toolSpan := findSpan(t, exporter, "execute_tool read_file")
	assertCappedJSON(t, toolSpan, genAIToolCallArgumentsKey, maxBytes)
	toolResult, _ := spanAttrString(toolSpan, genAIToolCallResultKey)
	assert.LessOrEqual(t, len(toolResult), maxBytes)
	root := findSpan(t, exporter, "invoke_agent stirrup")
	assertCappedJSON(t, root, genAIInputMessagesKey, maxBytes)
}

func assertCappedJSON(t *testing.T, span tracetest.SpanStub, key string, maxBytes int) {
	t.Helper()
	v, ok := spanAttrString(span, key)
	require.True(t, ok, "%s: %s missing", span.Name, key)
	assert.LessOrEqual(t, len(v), maxBytes, "%s: %s", span.Name, key)
	assert.True(t, json.Valid([]byte(v)), "%s: %s is not valid JSON: %s", span.Name, key, v)
	found := false
	for _, attr := range span.Attributes {
		if string(attr.Key) == contentTruncatedKey(key) && attr.Value.AsBool() {
			found = true
		}
	}
	assert.True(t, found, "%s: %s missing its truncation marker", span.Name, key)
}
