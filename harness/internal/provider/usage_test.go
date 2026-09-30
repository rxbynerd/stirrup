package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// reportedUsage is the token-count projection of a message_complete event.
type reportedUsage struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int
}

func usageOf(ev types.StreamEvent) reportedUsage {
	return reportedUsage{
		Input:      ev.InputTokens,
		Output:     ev.OutputTokens,
		CacheRead:  ev.CacheReadTokens,
		CacheWrite: ev.CacheWriteTokens,
		Reasoning:  ev.ReasoningTokens,
	}
}

// mergedUsage folds every message_complete event the way the agentic
// loop does: each non-zero count wins, so adapters that split usage
// across a stop event and a trailing usage-only event are covered.
func mergedUsage(t *testing.T, events []types.StreamEvent) reportedUsage {
	t.Helper()
	var got reportedUsage
	seen := false
	for _, ev := range events {
		if ev.Type == "error" {
			t.Fatalf("unexpected error event: %v", ev.Error)
		}
		if ev.Type != "message_complete" {
			continue
		}
		seen = true
		u := usageOf(ev)
		for _, f := range []struct {
			dst *int
			v   int
		}{
			{&got.Input, u.Input},
			{&got.Output, u.Output},
			{&got.CacheRead, u.CacheRead},
			{&got.CacheWrite, u.CacheWrite},
			{&got.Reasoning, u.Reasoning},
		} {
			if f.v > 0 {
				*f.dst = f.v
			}
		}
	}
	if !seen {
		t.Fatalf("no message_complete event in %+v", events)
	}
	return got
}

func serveSSE(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fabricatedUsage(t *testing.T, response, providerType string) reportedUsage {
	t.Helper()
	ch := make(chan types.StreamEvent, 8)
	fabricateStream(ch, []byte(response), providerType)
	close(ch)
	return mergedUsage(t, collectEvents(t, ch))
}

// The message_delta fixture mirrors the shape api.anthropic.com returns
// (probed 2026-09-30): cumulative usage with separate cache figures and
// output_tokens_details.thinking_tokens.
func TestAnthropicAdapter_ReportsUsageFromMessageDelta(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"ok"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null,"stop_details":null,"container":null},"usage":{"input_tokens":4,"cache_creation_input_tokens":310,"cache_read_input_tokens":2162,"output_tokens":941,"output_tokens_details":{"thinking_tokens":468}}}`),
		makeSSE("message_stop", `{}`),
	)
	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = serveSSE(t, body).URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	got := mergedUsage(t, collectEvents(t, ch))
	want := reportedUsage{Input: 4 + 310 + 2162, Output: 941, CacheRead: 2162, CacheWrite: 310, Reasoning: 468}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// An Anthropic-compatible endpoint that reports only output_tokens on
// message_delta must leave InputTokens zero so the loop falls back to
// its estimate.
func TestAnthropicAdapter_OutputOnlyUsageLeavesInputUnreported(t *testing.T) {
	body := joinLines(
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`),
		makeSSE("message_stop", `{}`),
	)
	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = serveSSE(t, body).URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	if got, want := mergedUsage(t, collectEvents(t, ch)), (reportedUsage{Output: 17}); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestFabricateStream_AnthropicReportsUsage(t *testing.T) {
	response := `{
		"content": [{"type": "text", "text": "ok"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 522, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 2000,
			"cache_creation": {"ephemeral_5m_input_tokens": 100, "ephemeral_1h_input_tokens": 0},
			"output_tokens": 941, "output_tokens_details": {"thinking_tokens": 468},
			"service_tier": "standard", "inference_geo": "global"}
	}`
	got := fabricatedUsage(t, response, "anthropic")
	want := reportedUsage{Input: 2622, Output: 941, CacheRead: 2000, CacheWrite: 100, Reasoning: 468}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// The Responses usage fixtures follow the documented shape (documented,
// not probed): input_tokens already includes cached_tokens and
// cache_write_tokens.
const responsesUsageFixture = `"usage":{"input_tokens":15000,"input_tokens_details":{"cached_tokens":12000,"cache_write_tokens":3000},"output_tokens":400,"output_tokens_details":{"reasoning_tokens":256},"total_tokens":15400}`

var responsesUsageWant = reportedUsage{Input: 15000, Output: 400, CacheRead: 12000, CacheWrite: 3000, Reasoning: 256}

func TestOpenAIResponsesAdapter_ReportsUsage(t *testing.T) {
	cases := []struct {
		name     string
		terminal string
	}{
		{
			name:     "completed",
			terminal: makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1"}],`+responsesUsageFixture+`}}`),
		},
		{
			name:     "incomplete",
			terminal: makeResponsesEvent("response.incomplete", `{"response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","id":"msg_1"}],`+responsesUsageFixture+`}}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"ok"}`) + tc.terminal
			adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), serveSSE(t, body).URL, OpenAIAuthConfig{})

			ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 1024})
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			if got := mergedUsage(t, collectEvents(t, ch)); got != responsesUsageWant {
				t.Errorf("usage = %+v, want %+v", got, responsesUsageWant)
			}
		})
	}
}

func TestFabricateStream_OpenAIResponsesReportsUsage(t *testing.T) {
	response := `{"status":"completed","output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"ok"}]}],` + responsesUsageFixture + `}`
	if got := fabricatedUsage(t, response, "openai-responses"); got != responsesUsageWant {
		t.Errorf("usage = %+v, want %+v", got, responsesUsageWant)
	}
}

func streamChatUsage(t *testing.T, sse string) reportedUsage {
	t.Helper()
	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), serveSSE(t, sse).URL, OpenAIAuthConfig{}, RetryPolicy{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-6",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	return mergedUsage(t, collectEvents(t, ch))
}

// The captured LM Studio Qwen 3.6 stream puts usage (with
// completion_tokens_details.reasoning_tokens) on a trailing
// empty-choices chunk after finish_reason.
func TestOpenAICompatibleAdapter_ReportsTrailingUsageFromCapture(t *testing.T) {
	sse := string(streamFixtureSSE(t, "testdata/quirks/openai-compatible/qwen3.6-27b/response.sse"))
	if got, want := streamChatUsage(t, sse), (reportedUsage{Input: 16, Output: 42, Reasoning: 31}); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// Usage attached to the finish chunk itself, with the documented OpenAI
// prompt_tokens_details cache figures (documented, not probed).
func TestOpenAICompatibleAdapter_ReportsUsageOnFinishChunk(t *testing.T) {
	sse := "data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9000,"prompt_tokens_details":{"cached_tokens":8000,"cache_write_tokens":500},"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":64},"total_tokens":9120}}` + "\n\n" +
		"data: [DONE]\n\n"
	want := reportedUsage{Input: 9000, Output: 120, CacheRead: 8000, CacheWrite: 500, Reasoning: 64}
	if got := streamChatUsage(t, sse); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// A server that ignores stream_options.include_usage sends no usage
// block; every count stays zero so the loop keeps its estimate.
func TestOpenAICompatibleAdapter_NoUsageLeavesCountsUnreported(t *testing.T) {
	sse := "data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	if got := streamChatUsage(t, sse); got != (reportedUsage{}) {
		t.Errorf("usage = %+v, want all zero", got)
	}
}

func TestFabricateStream_OpenAIChatReportsUsage(t *testing.T) {
	response := `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":9000,"prompt_tokens_details":{"cached_tokens":8000,"cache_write_tokens":500},"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":64}}}`
	want := reportedUsage{Input: 9000, Output: 120, CacheRead: 8000, CacheWrite: 500, Reasoning: 64}
	if got := fabricatedUsage(t, response, "openai-compatible"); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}
