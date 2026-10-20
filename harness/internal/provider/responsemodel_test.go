package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// completedResponseModel returns the ResponseModel of the last
// message_complete event that carries one, and whether every
// message_complete event carries the same value.
func completedResponseModel(t *testing.T, events []types.StreamEvent) (string, bool) {
	t.Helper()
	must := require.New(t)
	var got string
	consistent := true
	completes := 0
	for _, ev := range events {
		if ev.Type != "message_complete" {
			continue
		}
		completes++
		if got != "" && ev.ResponseModel != got {
			consistent = false
		}
		if ev.ResponseModel != "" {
			got = ev.ResponseModel
		}
	}
	must.Positive(completes, "no message_complete event")
	return got, consistent
}

func TestAnthropicAdapter_ResponseModel(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-5-5-20260301","usage":{"input_tokens":4,"output_tokens":1}}}`),
		makeSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`),
		makeSSE("message_stop", `{"type":"message_stop"}`),
	))
	got, _ := completedResponseModel(t, events)
	must.Equal("claude-sonnet-5-5-20260301", got)
}

func TestAnthropicAdapter_ResponseModelAbsent(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("message_start", `not json`),
		makeSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`),
		makeSSE("message_stop", `{"type":"message_stop"}`),
	))
	got, _ := completedResponseModel(t, events)
	must.Empty(got, "message_start is unusable")
}

func TestOpenAIAdapter_ResponseModelOnEveryMessageComplete(t *testing.T) {
	must := require.New(t)
	sse := "data: {\"model\":\"gpt-6-2026-08-01\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-6-2026-08-01\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"model\":\"gpt-6-2026-08-01\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	srv := sseStubServer(t, []byte(sse))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 64})
	must.NoError(err, "Stream")

	got, consistent := completedResponseModel(t, drainStream(t, ch))
	must.Equal("gpt-6-2026-08-01", got)
	must.True(consistent, "message_complete events carry differing ResponseModel values")
}

func TestOpenAIResponsesAdapter_ResponseModel(t *testing.T) {
	for _, event := range []string{"response.completed", "response.incomplete"} {
		t.Run(event, func(t *testing.T) {
			must := require.New(t)
			status := "completed"
			if event == "response.incomplete" {
				status = "incomplete"
			}
			sse := []byte(makeResponsesEvent(event, fmt.Sprintf(`{"response":{"model":"gpt-6-2026-08-01","status":%q,"output":[]}}`, status)))
			events := streamTurn(t, newResponsesScript(t, sse).adapter(nil), responsesReplayModel, replayUserPrompt)
			got, _ := completedResponseModel(t, events)
			must.Equal("gpt-6-2026-08-01", got)
		})
	}
}

func TestGeminiAdapter_ResponseModel(t *testing.T) {
	cases := map[string]string{
		"finish":  `{"modelVersion":"gemini-3.8-pro-001","candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`,
		"blocked": `{"modelVersion":"gemini-3.8-pro-001","promptFeedback":{"blockReason":"SAFETY"}}`,
	}
	for name, chunk := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, makeGeminiData(chunk))
			}))
			defer srv.Close()

			adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
			ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.8-pro"})
			must.NoError(err, "Stream")

			got, _ := completedResponseModel(t, collectEvents(t, ch))
			must.Equal("gemini-3.8-pro-001", got)
		})
	}
}

func TestFabricateStream_ResponseModel(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		response string
	}{
		{"anthropic", "anthropic", `{"model":"claude-sonnet-5-5-20260301","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"output_tokens":1}}`},
		{"openai chat", "openai-compatible", `{"model":"claude-sonnet-5-5-20260301","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`},
		{"openai responses", "openai-responses", `{"model":"claude-sonnet-5-5-20260301","status":"completed","output":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			ch := make(chan types.StreamEvent, 8)
			fabricateStream(ch, []byte(tc.response), tc.provider)
			close(ch)
			var events []types.StreamEvent
			for ev := range ch {
				events = append(events, ev)
			}
			got, _ := completedResponseModel(t, events)
			must.Equal("claude-sonnet-5-5-20260301", got)
		})
	}
}
