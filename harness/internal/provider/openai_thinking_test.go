package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// streamOpenAICompatible drives sse through the production Stream path for
// model and returns every event.
func streamOpenAICompatible(t *testing.T, model string, sse []byte) []types.StreamEvent {
	t.Helper()
	srv := sseStubServer(t, sse)
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     model,
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return drainStream(t, ch)
}

// splitDeltas returns the thinking_delta fragments and the concatenated
// text_delta text.
func splitDeltas(events []types.StreamEvent) (thinking []string, text string) {
	var b strings.Builder
	for _, ev := range events {
		switch ev.Type {
		case "thinking_delta":
			thinking = append(thinking, ev.Text)
		case "text_delta":
			b.WriteString(ev.Text)
		}
	}
	return thinking, b.String()
}

// Qwen 3.6 has no replay rule, so reasoning_content reaches the transport
// only as thinking_delta and never as answer text.
func TestOpenAICompatible_ReasoningContentStreamsAsThinkingDelta(t *testing.T) {
	events := streamOpenAICompatible(t, "qwen/qwen3.6-27b",
		streamFixtureSSE(t, "testdata/quirks/openai-compatible/qwen3.6-27b/response.sse"))

	thinking, text := splitDeltas(events)
	if got, want := strings.Join(thinking, "|"), "The user| wants a greeting."; got != want {
		t.Errorf("thinking_delta fragments = %q, want %q", got, want)
	}
	if text != "\n\nHello!" {
		t.Errorf("answer text = %q, want %q", text, "\n\nHello!")
	}
	for _, ev := range events {
		if ev.Type == "message_complete" && len(ev.ReplayFields) > 0 {
			t.Errorf("unexpected ReplayFields on message_complete: %v", ev.ReplayFields)
		}
	}
}

// A replay rule captures reasoning_content for the next turn; the same
// fragments still stream as thinking_delta.
func TestOpenAICompatible_ReasoningContentStreamsBesideReplayCapture(t *testing.T) {
	events := streamOpenAICompatible(t, "deepseek-v4-flash",
		streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4-flash/response.sse"))

	thinking, text := splitDeltas(events)
	if got, want := strings.Join(thinking, "|"), "Scanning the request. |Choosing an answer."; got != want {
		t.Errorf("thinking_delta fragments = %q, want %q", got, want)
	}
	if text != "Done." {
		t.Errorf("answer text = %q, want %q", text, "Done.")
	}
	var replayed string
	for _, ev := range events {
		if ev.Type == "message_complete" && ev.ReplayFields != nil {
			replayed = string(ev.ReplayFields["reasoning_content"])
		}
	}
	if want := `"Scanning the request. Choosing an answer."`; replayed != want {
		t.Errorf("ReplayFields[reasoning_content] = %s, want %s", replayed, want)
	}
}

// A non-string reasoning_content must not fail the chunk or leak into the
// answer, and a delta carrying both fields yields reasoning first.
func TestOpenAICompatible_ReasoningContentShapes(t *testing.T) {
	sse := []byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":null}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":{"unexpected":"object"}}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":["unexpected"]}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":"plan","content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`)
	events := streamOpenAICompatible(t, "some-local-model", sse)

	var order []string
	for _, ev := range events {
		if ev.Type == "thinking_delta" || ev.Type == "text_delta" {
			order = append(order, ev.Type+":"+ev.Text)
		}
	}
	if got, want := strings.Join(order, ","), "thinking_delta:plan,text_delta:answer"; got != want {
		t.Errorf("delta order = %q, want %q", got, want)
	}
}
