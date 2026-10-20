package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// mergedUsage folds every message_complete event through the same
// TokenUsage.MergeEvent the agentic loop uses.
func mergedUsage(t *testing.T, events []types.StreamEvent) types.TokenUsage {
	t.Helper()
	must := require.New(t)
	var got types.TokenUsage
	seen := false
	for _, ev := range events {
		must.NotEqual("error", ev.Type, "unexpected error event: %v", ev.Error)
		if ev.Type != "message_complete" {
			continue
		}
		seen = true
		got.MergeEvent(ev)
	}
	must.True(seen, "no message_complete event in %+v", events)
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

func fabricatedUsage(t *testing.T, response, providerType string) types.TokenUsage {
	t.Helper()
	ch := make(chan types.StreamEvent, 8)
	fabricateStream(ch, []byte(response), providerType)
	close(ch)
	return mergedUsage(t, collectEvents(t, ch))
}

func streamAnthropicUsage(t *testing.T, messageDeltas ...string) types.TokenUsage {
	t.Helper()
	must := require.New(t)
	var lines []string
	for _, md := range messageDeltas {
		lines = append(lines, makeSSE("message_delta", md))
	}
	lines = append(lines, makeSSE("message_stop", `{}`))
	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = serveSSE(t, joinLines(lines...)).URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 1024})
	must.NoError(err)
	return mergedUsage(t, collectEvents(t, ch))
}

func streamResponsesUsage(t *testing.T, terminal string) types.TokenUsage {
	t.Helper()
	must := require.New(t)
	body := makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"ok"}`) + terminal
	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), serveSSE(t, body).URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 1024})
	must.NoError(err)
	return mergedUsage(t, collectEvents(t, ch))
}

func streamGeminiUsage(t *testing.T, chunk string) types.TokenUsage {
	t.Helper()
	must := require.New(t)
	adapter := newGeminiTestAdapter(serveSSE(t, makeGeminiData(chunk)).URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.8-pro"})
	must.NoError(err)
	return mergedUsage(t, collectEvents(t, ch))
}

// bedrockUsageEvents runs a messageStop followed by a metadata event
// carrying usage through the Converse stream consumer.
func bedrockUsageEvents(t *testing.T, usage *brtypes.TokenUsage) []types.StreamEvent {
	t.Helper()
	events := []brtypes.ConverseStreamOutput{
		&brtypes.ConverseStreamOutputMemberMessageStop{
			Value: brtypes.MessageStopEvent{StopReason: brtypes.StopReasonEndTurn},
		},
		&brtypes.ConverseStreamOutputMemberMetadata{
			Value: brtypes.ConverseStreamMetadataEvent{Usage: usage},
		},
	}
	ch := make(chan types.StreamEvent, 8)
	go consumeBedrockStream(context.Background(), newMockEventReader(events, nil), ch)
	return collectEvents(t, ch)
}

func TestSetEventUsage_CapsSubsetsAtTheirTotals(t *testing.T) {
	cases := []struct {
		name string
		in   tokenReport
		want types.TokenUsage
	}{
		{
			name: "cache write capped at the input left after cache read",
			in:   tokenReport{Input: 100, CacheRead: 80, CacheWrite: 50},
			want: types.TokenUsage{Input: 100, CacheRead: 80, CacheWrite: 20},
		},
		{
			name: "cache read capped at input",
			in:   tokenReport{Input: 10, CacheRead: 50, CacheWrite: 5},
			want: types.TokenUsage{Input: 10, CacheRead: 10},
		},
		{
			name: "reasoning capped at output",
			in:   tokenReport{Output: 5, Reasoning: 9},
			want: types.TokenUsage{Output: 5, Reasoning: 5},
		},
		{
			name: "negative counts clamp to zero",
			in:   tokenReport{Input: -1, Output: -1, CacheRead: -1, CacheWrite: -1, Reasoning: -1},
			want: types.TokenUsage{},
		},
		{
			name: "oversized counts clamp to the int32 range",
			in:   tokenReport{Input: math.MaxInt, Output: math.MaxInt, CacheRead: math.MaxInt, Reasoning: math.MaxInt},
			want: types.TokenUsage{Input: maxReportedTokens, Output: maxReportedTokens, CacheRead: maxReportedTokens, Reasoning: maxReportedTokens},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			ev := types.StreamEvent{Type: "message_complete"}
			setEventUsage(&ev, tc.in)
			var got types.TokenUsage
			got.MergeEvent(ev)
			must.Equal(tc.want, got)
		})
	}
}

// maxInt64JSON is the largest count encoding/json decodes into an int.
const maxInt64JSON = "9223372036854775807"

func TestAdapters_ClampHostileUsage(t *testing.T) {
	anthropicHostile := `"usage":{"input_tokens":` + maxInt64JSON + `,"cache_creation_input_tokens":-5,"cache_read_input_tokens":` + maxInt64JSON + `,"output_tokens":-1,"output_tokens_details":{"thinking_tokens":` + maxInt64JSON + `}}`
	cases := []struct {
		name string
		run  func(t *testing.T) types.TokenUsage
		want types.TokenUsage
	}{
		{
			name: "anthropic stream",
			run: func(t *testing.T) types.TokenUsage {
				return streamAnthropicUsage(t, `{"delta":{"stop_reason":"end_turn"},`+anthropicHostile+`}`)
			},
			want: types.TokenUsage{Input: maxReportedTokens, CacheRead: maxReportedTokens},
		},
		{
			name: "anthropic batch",
			run: func(t *testing.T) types.TokenUsage {
				return fabricatedUsage(t, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+anthropicHostile+`}`, "anthropic")
			},
			want: types.TokenUsage{Input: maxReportedTokens, CacheRead: maxReportedTokens},
		},
		{
			name: "openai responses",
			run: func(t *testing.T) types.TokenUsage {
				return streamResponsesUsage(t, makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1"}],"usage":{"input_tokens":-3,"input_tokens_details":{"cached_tokens":`+maxInt64JSON+`,"cache_write_tokens":`+maxInt64JSON+`},"output_tokens":`+maxInt64JSON+`,"output_tokens_details":{"reasoning_tokens":-2}}}}`))
			},
			want: types.TokenUsage{Output: maxReportedTokens},
		},
		{
			name: "openai chat",
			run: func(t *testing.T) types.TokenUsage {
				return streamChatUsage(t, "data: "+`{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":`+maxInt64JSON+`,"prompt_tokens_details":{"cached_tokens":-1},"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":`+maxInt64JSON+`}}}`+"\n\ndata: [DONE]\n\n")
			},
			want: types.TokenUsage{Input: maxReportedTokens, Output: 50, Reasoning: 50},
		},
		{
			name: "gemini",
			run: func(t *testing.T) types.TokenUsage {
				return streamGeminiUsage(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":-10,"cachedContentTokenCount":500,"candidatesTokenCount":`+maxInt64JSON+`,"thoughtsTokenCount":`+maxInt64JSON+`}}`)
			},
			want: types.TokenUsage{Output: maxReportedTokens, Reasoning: maxReportedTokens},
		},
		{
			name: "bedrock",
			run: func(t *testing.T) types.TokenUsage {
				return mergedUsage(t, bedrockUsageEvents(t, &brtypes.TokenUsage{
					InputTokens:           aws.Int32(-5),
					OutputTokens:          aws.Int32(math.MaxInt32),
					CacheReadInputTokens:  aws.Int32(math.MaxInt32),
					CacheWriteInputTokens: aws.Int32(-1),
				}))
			},
			want: types.TokenUsage{Input: maxReportedTokens, Output: maxReportedTokens, CacheRead: maxReportedTokens},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := tc.run(t)
			must.Equal(tc.want, got)
		})
	}
}

// The message_delta fixture mirrors the shape api.anthropic.com returns:
// cumulative usage with separate cache figures and
// output_tokens_details.thinking_tokens.
func TestAnthropicAdapter_ReportsUsageFromMessageDelta(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	got := mergedUsage(t, collectEvents(t, ch))
	want := types.TokenUsage{Input: 4 + 310 + 2162, Output: 941, CacheRead: 2162, CacheWrite: 310, Reasoning: 468}
	must.Equal(want, got)
}

// An Anthropic-compatible endpoint that reports only output_tokens on
// message_delta must leave InputTokens zero so the loop falls back to
// its estimate.
func TestAnthropicAdapter_OutputOnlyUsageLeavesInputUnreported(t *testing.T) {
	must := require.New(t)
	got := streamAnthropicUsage(t, `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`)
	must.Equal(types.TokenUsage{Output: 17}, got)
}

func TestAnthropicAdapter_ReportsPartialUsage(t *testing.T) {
	cases := []struct {
		name   string
		deltas []string
		want   types.TokenUsage
	}{
		{
			name:   "no usage object",
			deltas: []string{`{"delta":{"stop_reason":"end_turn"}}`},
			want:   types.TokenUsage{},
		},
		{
			name:   "empty output_tokens_details",
			deltas: []string{`{"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":9,"output_tokens_details":{}}}`},
			want:   types.TokenUsage{Input: 20, Output: 9},
		},
		{
			name:   "no output_tokens_details",
			deltas: []string{`{"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"output_tokens":9}}`},
			want:   types.TokenUsage{Input: 20, Output: 9},
		},
		{
			name: "two message_delta events keep the later cumulative usage",
			deltas: []string{
				`{"delta":{"stop_reason":null},"usage":{"input_tokens":20,"cache_read_input_tokens":100,"output_tokens":3}}`,
				`{"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"cache_read_input_tokens":100,"output_tokens":9,"output_tokens_details":{"thinking_tokens":4}}}`,
			},
			want: types.TokenUsage{Input: 120, Output: 9, CacheRead: 100, Reasoning: 4},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := streamAnthropicUsage(t, tc.deltas...)
			must.Equal(tc.want, got)
		})
	}
}

func TestFabricateStream_AnthropicReportsUsage(t *testing.T) {
	must := require.New(t)
	response := `{
		"content": [{"type": "text", "text": "ok"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 522, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 2000,
			"cache_creation": {"ephemeral_5m_input_tokens": 100, "ephemeral_1h_input_tokens": 0},
			"output_tokens": 941, "output_tokens_details": {"thinking_tokens": 468},
			"service_tier": "standard", "inference_geo": "global"}
	}`
	got := fabricatedUsage(t, response, "anthropic")
	want := types.TokenUsage{Input: 2622, Output: 941, CacheRead: 2000, CacheWrite: 100, Reasoning: 468}
	must.Equal(want, got)
}

// In the Responses usage fixtures input_tokens already includes
// cached_tokens and cache_write_tokens.
const responsesUsageFixture = `"usage":{"input_tokens":15000,"input_tokens_details":{"cached_tokens":12000,"cache_write_tokens":3000},"output_tokens":400,"output_tokens_details":{"reasoning_tokens":256},"total_tokens":15400}`

var responsesUsageWant = types.TokenUsage{Input: 15000, Output: 400, CacheRead: 12000, CacheWrite: 3000, Reasoning: 256}

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
			must := require.New(t)
			got := streamResponsesUsage(t, tc.terminal)
			must.Equal(responsesUsageWant, got)
		})
	}
}

func TestOpenAIResponsesAdapter_ReportsPartialUsage(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  types.TokenUsage
	}{
		{
			name:  "no usage",
			usage: ``,
			want:  types.TokenUsage{},
		},
		{
			name:  "no details objects",
			usage: `,"usage":{"input_tokens":300,"output_tokens":40,"total_tokens":340}`,
			want:  types.TokenUsage{Input: 300, Output: 40},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			terminal := makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1"}]`+tc.usage+`}}`)
			got := streamResponsesUsage(t, terminal)
			must.Equal(tc.want, got)
		})
	}
}

func TestFabricateStream_OpenAIResponsesReportsUsage(t *testing.T) {
	must := require.New(t)
	response := `{"status":"completed","output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"ok"}]}],` + responsesUsageFixture + `}`
	got := fabricatedUsage(t, response, "openai-responses")
	must.Equal(responsesUsageWant, got)
}

func streamChatUsage(t *testing.T, sse string) types.TokenUsage {
	t.Helper()
	must := require.New(t)
	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), serveSSE(t, sse).URL, OpenAIAuthConfig{}, RetryPolicy{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-6",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	must.NoError(err)
	return mergedUsage(t, collectEvents(t, ch))
}

// The captured LM Studio Qwen 3.6 stream puts usage (with
// completion_tokens_details.reasoning_tokens) on a trailing
// empty-choices chunk after finish_reason.
func TestOpenAICompatibleAdapter_ReportsTrailingUsageFromCapture(t *testing.T) {
	must := require.New(t)
	sse := string(streamFixtureSSE(t, "testdata/quirks/openai-compatible/qwen3.6-27b/response.sse"))
	got := streamChatUsage(t, sse)
	must.Equal(types.TokenUsage{Input: 16, Output: 42, Reasoning: 31}, got)
}

// Usage attached to the finish chunk itself, with
// prompt_tokens_details.cached_tokens.
func TestOpenAICompatibleAdapter_ReportsUsageOnFinishChunk(t *testing.T) {
	must := require.New(t)
	sse := "data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9000,"prompt_tokens_details":{"cached_tokens":8000},"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":64},"total_tokens":9120}}` + "\n\n" +
		"data: [DONE]\n\n"
	want := types.TokenUsage{Input: 9000, Output: 120, CacheRead: 8000, Reasoning: 64}
	got := streamChatUsage(t, sse)
	must.Equal(want, got)
}

// A server that ignores stream_options.include_usage sends no usage
// block; every count stays zero so the loop keeps its estimate.
func TestOpenAICompatibleAdapter_NoUsageLeavesCountsUnreported(t *testing.T) {
	must := require.New(t)
	sse := "data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	must.Zero(streamChatUsage(t, sse))
}

func TestOpenAICompatibleAdapter_ReportsPartialUsage(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  types.TokenUsage
	}{
		{
			name:  "no details objects",
			usage: `{"prompt_tokens":300,"completion_tokens":40,"total_tokens":340}`,
			want:  types.TokenUsage{Input: 300, Output: 40},
		},
		{
			name:  "null prompt_tokens_details",
			usage: `{"prompt_tokens":300,"prompt_tokens_details":null,"completion_tokens":40,"completion_tokens_details":null}`,
			want:  types.TokenUsage{Input: 300, Output: 40},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			sse := "data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":` + tc.usage + `}` + "\n\n" +
				"data: [DONE]\n\n"
			got := streamChatUsage(t, sse)
			must.Equal(tc.want, got)
		})
	}
}

// A server that attaches usage to the finish chunk and repeats it on a
// trailing chunk gets exactly one usage-bearing message_complete.
func TestOpenAICompatibleAdapter_EmitsUsageOnce(t *testing.T) {
	must := require.New(t)
	usage := `"usage":{"prompt_tokens":300,"completion_tokens":40}`
	sse := "data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],` + usage + `}` + "\n\n" +
		"data: " + `{"id":"c1","choices":[],` + usage + `}` + "\n\n" +
		"data: [DONE]\n\n"
	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), serveSSE(t, sse).URL, OpenAIAuthConfig{}, RetryPolicy{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-6",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	must.NoError(err)
	events := collectEvents(t, ch)
	withUsage := 0
	for _, ev := range events {
		if ev.Type == "message_complete" && ev.InputTokens > 0 {
			withUsage++
		}
	}
	must.Equal(1, withUsage, "usage-bearing message_complete events: %+v", events)
	want := types.TokenUsage{Input: 300, Output: 40}
	must.Equal(want, mergedUsage(t, events))
}

func TestFabricateStream_OpenAIChatReportsUsage(t *testing.T) {
	must := require.New(t)
	response := `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":9000,"prompt_tokens_details":{"cached_tokens":8000},"completion_tokens":120,"completion_tokens_details":{"reasoning_tokens":64}}}`
	want := types.TokenUsage{Input: 9000, Output: 120, CacheRead: 8000, Reasoning: 64}
	got := fabricatedUsage(t, response, "openai-compatible")
	must.Equal(want, got)
}

// Gemini reports thoughts beside, not inside, candidatesTokenCount; the
// adapter adds them into OutputTokens so Reasoning <= Output holds as on
// every other provider. promptTokenCount already includes the cached
// content.
func TestGeminiAdapter_ReportsUsageWithThoughtsInOutput(t *testing.T) {
	must := require.New(t)
	got := streamGeminiUsage(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5000,"cachedContentTokenCount":4096,"candidatesTokenCount":120,"thoughtsTokenCount":900,"totalTokenCount":6020}}`)
	want := types.TokenUsage{Input: 5000, Output: 120 + 900, CacheRead: 4096, Reasoning: 900}
	must.Equal(want, got)
}

func TestGeminiAdapter_ReportsPartialUsage(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  types.TokenUsage
	}{
		{
			name:  "thoughts only",
			usage: `{"promptTokenCount":50,"thoughtsTokenCount":30,"totalTokenCount":80}`,
			want:  types.TokenUsage{Input: 50, Output: 30, Reasoning: 30},
		},
		{
			name:  "cached content only",
			usage: `{"promptTokenCount":5000,"cachedContentTokenCount":4096,"candidatesTokenCount":12,"totalTokenCount":5012}`,
			want:  types.TokenUsage{Input: 5000, Output: 12, CacheRead: 4096},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := streamGeminiUsage(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":`+tc.usage+`}`)
			must.Equal(tc.want, got)
		})
	}
}

// A prompt Vertex blocks is still billed; its usage rides on the
// promptFeedback chunk.
func TestGeminiAdapter_ReportsUsageOnBlockedPrompt(t *testing.T) {
	must := require.New(t)
	got := streamGeminiUsage(t, `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":812,"totalTokenCount":812}}`)
	must.Equal(types.TokenUsage{Input: 812}, got)
}

// Bedrock delivers usage on the metadata event after messageStop.
func TestBedrock_ReportsUsageFromMetadata(t *testing.T) {
	cases := []struct {
		name  string
		usage *brtypes.TokenUsage
		want  types.TokenUsage
	}{
		{
			name: "input derived from totalTokens when inputTokens excludes cache",
			usage: &brtypes.TokenUsage{
				InputTokens:           aws.Int32(12),
				OutputTokens:          aws.Int32(150),
				TotalTokens:           aws.Int32(3200),
				CacheReadInputTokens:  aws.Int32(2900),
				CacheWriteInputTokens: aws.Int32(138),
			},
			want: types.TokenUsage{Input: 3050, Output: 150, CacheRead: 2900, CacheWrite: 138},
		},
		{
			name: "input derived from totalTokens when inputTokens includes cache",
			usage: &brtypes.TokenUsage{
				InputTokens:           aws.Int32(3050),
				OutputTokens:          aws.Int32(150),
				TotalTokens:           aws.Int32(3200),
				CacheReadInputTokens:  aws.Int32(2900),
				CacheWriteInputTokens: aws.Int32(138),
			},
			want: types.TokenUsage{Input: 3050, Output: 150, CacheRead: 2900, CacheWrite: 138},
		},
		{
			name: "without totalTokens the cache figures are added to inputTokens",
			usage: &brtypes.TokenUsage{
				InputTokens:           aws.Int32(12),
				OutputTokens:          aws.Int32(150),
				CacheReadInputTokens:  aws.Int32(2900),
				CacheWriteInputTokens: aws.Int32(138),
			},
			want: types.TokenUsage{Input: 3050, Output: 150, CacheRead: 2900, CacheWrite: 138},
		},
		{
			name: "nil cache pointers leave input at inputTokens",
			usage: &brtypes.TokenUsage{
				InputTokens:  aws.Int32(812),
				OutputTokens: aws.Int32(40),
			},
			want: types.TokenUsage{Input: 812, Output: 40},
		},
		{
			name:  "nil inputTokens leaves input unreported",
			usage: &brtypes.TokenUsage{OutputTokens: aws.Int32(40)},
			want:  types.TokenUsage{Output: 40},
		},
		{
			name: "totalTokens below outputTokens leaves input unreported",
			usage: &brtypes.TokenUsage{
				InputTokens:  aws.Int32(812),
				OutputTokens: aws.Int32(40),
				TotalTokens:  aws.Int32(10),
			},
			want: types.TokenUsage{Output: 40},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := mergedUsage(t, bedrockUsageEvents(t, tc.usage))
			must.Equal(tc.want, got)
		})
	}
}

func TestBedrock_MetadataWithoutUsageEmitsNoMessageComplete(t *testing.T) {
	must := require.New(t)
	events := bedrockUsageEvents(t, nil)
	completes := 0
	for _, ev := range events {
		if ev.Type == "message_complete" {
			completes++
		}
	}
	must.Equal(1, completes, "want one message_complete from messageStop: %+v", events)
	must.Zero(mergedUsage(t, events))
}

func TestFabricateStream_NoUsageLeavesCountsUnreported(t *testing.T) {
	cases := []struct {
		providerType string
		response     string
	}{
		{"anthropic", `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`},
		{"openai-compatible", `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
		{"openai-responses", `{"status":"completed","output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"ok"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.providerType, func(t *testing.T) {
			must := require.New(t)
			must.Zero(fabricatedUsage(t, tc.response, tc.providerType))
		})
	}
}
