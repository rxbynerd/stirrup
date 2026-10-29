package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// streamOpenAICompatible drives sse through the production Stream path for
// model and returns every event.
func streamOpenAICompatible(t *testing.T, model string, sse []byte) []types.StreamEvent {
	t.Helper()
	must := require.New(t)
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
	must.NoError(err, "Stream")
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
func TestOpenAICompatible_ReasoningContent_StreamsAsThinkingDelta(t *testing.T) {
	events := streamOpenAICompatible(t, "qwen/qwen3.6-27b",
		streamFixtureSSE(t, "testdata/quirks/openai-compatible/qwen3.6-27b/response.sse"))
	thinking, text := splitDeltas(events)

	t.Run("thinking deltas", func(t *testing.T) {
		must := require.New(t)
		must.Equal("The user| wants a greeting.", strings.Join(thinking, "|"))
	})
	t.Run("answer text", func(t *testing.T) {
		must := require.New(t)
		must.Equal("\n\nHello!", text)
	})
	t.Run("no replay fields", func(t *testing.T) {
		must := require.New(t)
		for _, ev := range events {
			if ev.Type == "message_complete" {
				must.Empty(ev.ReplayFields)
			}
		}
	})
}

// A replay rule captures reasoning_content for the next turn; the same
// fragments still stream as thinking_delta.
func TestOpenAICompatible_ReasoningContent_StreamsBesideReplayCapture(t *testing.T) {
	events := streamOpenAICompatible(t, "deepseek-v4-flash",
		streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4-flash/response.sse"))
	thinking, text := splitDeltas(events)

	t.Run("thinking deltas", func(t *testing.T) {
		must := require.New(t)
		must.Equal("Scanning the request. |Choosing an answer.", strings.Join(thinking, "|"))
	})
	t.Run("answer text", func(t *testing.T) {
		must := require.New(t)
		must.Equal("Done.", text)
	})
	t.Run("replay capture", func(t *testing.T) {
		must := require.New(t)
		var replayed string
		for _, ev := range events {
			if ev.Type == "message_complete" && ev.ReplayFields != nil {
				replayed = string(ev.ReplayFields["reasoning_content"])
			}
		}
		must.Equal(`"Scanning the request. Choosing an answer."`, replayed)
	})
}

// A non-string reasoning_content must not fail the chunk or leak into the
// answer, and a delta carrying both fields yields reasoning first.
func TestOpenAICompatible_ReasoningContent_NonStringIgnoredReasoningFirst(t *testing.T) {
	must := require.New(t)
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
	must.Equal("thinking_delta:plan,text_delta:answer", strings.Join(order, ","))
}

// OpenRouter streams reasoning as a string in delta.reasoning (beside a
// reasoning_details array) and never as reasoning_content; it reaches the
// transport as thinking_delta and never as answer text.
func TestOpenAICompatible_ReasoningField_StreamsAsThinkingDelta(t *testing.T) {
	events := streamOpenAICompatible(t, "deepseek/deepseek-v4.1-flash-1017",
		streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek/deepseek-v4.1-flash-1017/response.sse"))
	thinking, text := splitDeltas(events)

	t.Run("thinking deltas", func(t *testing.T) {
		must := require.New(t)
		must.Equal("We| need answer. Need reason|. Anagram| S|PAIR -> Paris. European capital. Need| get weather via| tool. Use get|_weather city Paris|.",
			strings.Join(thinking, "|"))
	})
	t.Run("answer text carries no reasoning", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(text, "Anagram")
	})
}

// The same delta.reasoning shape arrives for GLM through OpenRouter, beside
// a tool call in the same stream.
func TestOpenAICompatible_ReasoningField_GLMThroughOpenRouter(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	events := streamOpenAICompatible(t, "z-ai/glm-5.3",
		streamFixtureSSE(t, "testdata/quirks/openai-compatible/z-ai/glm-5.3/response.sse"))
	thinking, text := splitDeltas(events)

	check.Equal(`An|agram of "|SPAIR" →| PARIS.| Paris is a capital| (|of| France).`, strings.Join(thinking, "|"))
	check.Equal("Rearranging the letters S, P, A, I, R gives PARIS — the capital of France.", text)
	var calls []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "tool_call" {
			calls = append(calls, ev)
		}
	}
	must.Len(calls, 1, "tool calls")
	check.Equal("get_weather", calls[0].Name)
	check.Equal(map[string]any{"city": "Paris"}, calls[0].Input)
}

// reasoning_content wins when a delta carries both fields, so a server that
// echoes the text under both names is shown once; an empty or non-string
// reasoning_content falls back to reasoning, and a non-string reasoning is
// ignored.
func TestOpenAICompatible_ReasoningField_PrecedenceAndFallback(t *testing.T) {
	must := require.New(t)
	sse := []byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"both","reasoning":"both"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":"","reasoning":"fallback-empty"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":null,"reasoning":"fallback-null"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":{"unexpected":"object"},"reasoning":"fallback-shape"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning":{"unexpected":"object"}}}]}

data: {"choices":[{"index":0,"delta":{"reasoning":null,"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`)
	events := streamOpenAICompatible(t, "some-gateway-model", sse)

	thinking, text := splitDeltas(events)
	must.Equal("both|fallback-empty|fallback-null|fallback-shape", strings.Join(thinking, "|"))
	must.Equal("answer", text)
}
