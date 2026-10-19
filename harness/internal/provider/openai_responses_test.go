package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// makeResponsesEvent builds a named SSE record terminated by a blank line —
// the wire shape the Responses API streams.
func makeResponsesEvent(name, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", name, data)
}

func TestOpenAIResponsesAdapter_StreamEmitsTextDeltas(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.created", `{"response":{"id":"resp_1","status":"in_progress"}}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"Hello"}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":" world"}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1"}],"usage":{"input_tokens":10,"output_tokens":5}}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		assert.True(t, strings.HasSuffix(r.URL.Path, "/responses"), "path %q", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	require.Len(t, events, 3)
	assert.Equal(t, "text_delta", events[0].Type)
	assert.Equal(t, "Hello", events[0].Text)
	assert.Equal(t, "text_delta", events[1].Type)
	assert.Equal(t, " world", events[1].Text)
	assert.Equal(t, "message_complete", events[2].Type)
	assert.Equal(t, "end_turn", events[2].StopReason)
	assert.Equal(t, 5, events[2].OutputTokens)
}

// responsesWarnStubServer is a minimal Responses stub that returns a single
// response.completed event, used by the tool-choice downgrade warn tests.
func responsesWarnStubServer(t *testing.T) *httptest.Server {
	t.Helper()
	body := makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1"}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOpenAIResponsesAdapter_ToolChoiceNonAuto_WarnsDowngrade: the Responses
// request body carries no tool_choice field, so a non-auto ToolChoice is
// silently downgraded to auto. The warn must fire without leaking message
// content or any secret-derived value.
func TestOpenAIResponsesAdapter_ToolChoiceNonAuto_WarnsDowngrade(t *testing.T) {
	srv := responsesWarnStubServer(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:      "gpt-4.1",
		MaxTokens:  1024,
		ToolChoice: types.ToolChoiceRequired,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "secret-prompt-text"}}},
		},
	})
	require.NoError(t, err)
	collectEvents(t, ch)

	out := buf.String()
	assert.Contains(t, out, "openai-responses tool-choice downgraded to auto")
	assert.Contains(t, out, "gpt-4.1", "warn log missing model identifier")
	assert.NotContains(t, out, "secret-prompt-text", "warn log leaked message content")
}

// TestOpenAIResponsesAdapter_ToolChoiceAuto_NoWarn pins the negative: the
// zero-value ToolChoiceAuto must not emit the downgrade warn.
func TestOpenAIResponsesAdapter_ToolChoiceAuto_NoWarn(t *testing.T) {
	srv := responsesWarnStubServer(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:      "gpt-4.1",
		MaxTokens:  1024,
		ToolChoice: types.ToolChoiceAuto,
	})
	require.NoError(t, err)
	collectEvents(t, ch)

	require.NotContains(t, buf.String(), "tool-choice downgraded", "auto tool-choice must not warn")
}

func TestOpenAIResponsesAdapter_StreamEmitsToolCall(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file"}}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_1","output_index":0,"delta":"{\"path\":"}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_1","output_index":0,"delta":"\"main.go\"}"}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"item_id":"fc_1","output_index":0,"arguments":"{\"path\":\"main.go\"}"}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_2","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","arguments":"{\"path\":\"main.go\"}"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	require.Len(t, events, 2)
	require.Equal(t, "tool_call", events[0].Type)
	assert.Equal(t, "call_abc", events[0].ID)
	assert.Equal(t, "read_file", events[0].Name)
	assert.Equal(t, "main.go", events[0].Input["path"])
	assert.Equal(t, "message_complete", events[1].Type)
	assert.Equal(t, "tool_use", events[1].StopReason)
}

func TestOpenAIResponsesAdapter_InterleavedToolCallArgumentsStaySeparate(t *testing.T) {
	// Argument-deltas for two concurrent calls arrive interleaved (bbb
	// then aaa); the adapter must emit each tool_call at .done and
	// preserve output_index ordering across the resulting events.
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_aaa","call_id":"call_aaa","name":"read_file"}}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":1,"item":{"type":"function_call","id":"fc_bbb","call_id":"call_bbb","name":"write_file"}}`),

		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_bbb","output_index":1,"delta":"{\"path\":\"b.go\","}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_aaa","output_index":0,"delta":"{\"path\":"}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_bbb","output_index":1,"delta":"\"content\":\"x\"}"}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_aaa","output_index":0,"delta":"\"a.go\"}"}`),

		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"function_call","id":"fc_aaa","call_id":"call_aaa","name":"read_file"}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":{"type":"function_call","id":"fc_bbb","call_id":"call_bbb","name":"write_file"}}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_3","status":"completed","output":[{"type":"function_call","id":"fc_aaa","call_id":"call_aaa","name":"read_file"},{"type":"function_call","id":"fc_bbb","call_id":"call_bbb","name":"write_file"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	var toolCalls []types.StreamEvent
	for _, ev := range events {
		require.NotEqual(t, "error", ev.Type, "unexpected error event: %v", ev.Error)
		if ev.Type == "tool_call" {
			toolCalls = append(toolCalls, ev)
		}
	}
	require.Len(t, toolCalls, 2)

	assert.Equal(t, "call_aaa", toolCalls[0].ID)
	assert.Equal(t, "read_file", toolCalls[0].Name)
	assert.Equal(t, "a.go", toolCalls[0].Input["path"])
	assert.Equal(t, "call_bbb", toolCalls[1].ID)
	assert.Equal(t, "write_file", toolCalls[1].Name)
	assert.Equal(t, "b.go", toolCalls[1].Input["path"])
}

func TestOpenAIResponsesAdapter_DeferredFlushOrderedByOutputIndex(t *testing.T) {
	// If no .done events fire for two concurrent function calls, the
	// adapter must still flush them on response.completed in
	// deterministic output_index ascending order — state map iteration
	// in Go is randomised, so the explicit sort matters.
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":1,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"write_file"}}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read_file"}}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_b","output_index":1,"delta":"{\"path\":\"b\"}"}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_a","output_index":0,"delta":"{\"path\":\"a\"}"}`),

		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read_file"},{"type":"function_call","id":"fc_b","call_id":"call_b","name":"write_file"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	for i := 0; i < 5; i++ { // run a few times because map iteration is randomised
		ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
		require.NoError(t, err, "iter %d", i)
		events := collectEvents(t, ch)

		var calls []types.StreamEvent
		for _, ev := range events {
			if ev.Type == "tool_call" {
				calls = append(calls, ev)
			}
		}
		require.Len(t, calls, 2, "iter %d", i)
		assert.Equal(t, "call_a", calls[0].ID, "iter %d", i)
		assert.Equal(t, "call_b", calls[1].ID, "iter %d", i)
	}
}

func TestOpenAIResponsesAdapter_TextThenToolCall(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"Let me read that file."}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_xyz","name":"read_file"}}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_1","output_index":1,"delta":"{\"path\":\"main.go\"}"}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"item_id":"fc_1","output_index":1}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"message","id":"msg_1"},{"type":"function_call","id":"fc_1","call_id":"call_xyz","name":"read_file"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	require.Len(t, events, 3)
	assert.Equal(t, "text_delta", events[0].Type)
	assert.Equal(t, "tool_call", events[1].Type)
	assert.Equal(t, "message_complete", events[2].Type)
	assert.Equal(t, "tool_use", events[2].StopReason)
}

func TestOpenAIResponsesAdapter_IncompleteMaxTokens(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"hi"}`),
		makeResponsesEvent("response.incomplete", `{"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":1}}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	last := events[len(events)-1]
	assert.Equal(t, "message_complete", last.Type)
	assert.Equal(t, "max_tokens", last.StopReason)
	assert.Equal(t, 1, last.OutputTokens)
}

func TestOpenAIResponsesAdapter_FailedEventEmitsError(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		makeResponsesEvent("response.failed", `{"response":{"status":"failed","error":{"message":"server overloaded","type":"server_error"}}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	last := events[len(events)-1]
	assert.Equal(t, "error", last.Type)
	require.ErrorContains(t, last.Error, "server overloaded")
	// No message_complete should be emitted on a failed response.
	for _, ev := range events {
		assert.NotEqual(t, "message_complete", ev.Type, "unexpected message_complete on failed response: %+v", ev)
	}
}

func TestOpenAIResponsesAdapter_ErrorEventEmitsError(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("error", `{"message":"upstream timeout","type":"timeout","code":"E_TIMEOUT"}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	require.Len(t, events, 1)
	assert.Equal(t, "error", events[0].Type)
	assert.ErrorContains(t, events[0].Error, "upstream timeout")
}

func TestOpenAIResponsesAdapter_MalformedToolArgumentsMarksToolCall(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_bad","name":"read_file"}}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_1","output_index":0,"delta":"{NOT VALID"}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"item_id":"fc_1","output_index":0}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_bad","name":"read_file"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)
	requireMarkedToolCall(t, events, "call_bad", "read_file", `{NOT VALID`)
}

func TestOpenAIResponsesAdapter_ToolArgumentsExceedSizeLimit(t *testing.T) {
	// Build several reasonably-sized deltas whose cumulative size exceeds
	// the openaiMaxToolInputSize cap. The accumulated buffer check should
	// trip on the chunk that crosses the limit, emit an error event, and
	// tear down the stream before any tool_call is produced.
	const chunkSize = 512 * 1024 // 512KB
	chunkText := strings.Repeat("A", chunkSize)
	encoded, _ := json.Marshal(chunkText)
	chunkPayload := fmt.Sprintf(`{"item_id":"fc_1","output_index":0,"delta":%s}`, string(encoded))

	parts := []string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_big","name":"read_file"}}`),
	}
	// 22 chunks of 512KB = 11MB > 10MB cap.
	for i := 0; i < 22; i++ {
		parts = append(parts, makeResponsesEvent("response.function_call_arguments.delta", chunkPayload))
	}
	parts = append(parts,
		makeResponsesEvent("response.function_call_arguments.done", `{"item_id":"fc_1","output_index":0}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
	)
	body := strings.Join(parts, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	foundOversize := false
	for _, ev := range events {
		if ev.Type == "error" && ev.Error != nil && strings.Contains(ev.Error.Error(), "byte limit") {
			foundOversize = true
		}
		assert.NotEqual(t, "tool_call", ev.Type, "tool_call should not be emitted when args exceed cap: %+v", ev)
	}
	assert.True(t, foundOversize, "expected an error event citing the byte limit")
}

func TestOpenAIResponsesAdapter_HTTPErrorReturnsAPIMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("bad-key"), srv.URL, OpenAIAuthConfig{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.ErrorContains(t, err, "Invalid API key")
}

func TestOpenAIResponsesAdapter_HTTPErrorLargeBodyReturnsStatus(t *testing.T) {
	largeMsg := strings.Repeat("x", 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"%s","type":"invalid_request_error"}}`, largeMsg)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("key"), srv.URL, OpenAIAuthConfig{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	// The 4096-byte LimitReader truncates the JSON before it can decode, so
	// we fall back to the generic status-code error. The key assertion is
	// that the call returns cleanly without consuming unbounded memory.
	require.ErrorContains(t, err, "400")
}

func TestOpenAIResponsesAdapter_RequestBody(t *testing.T) {
	var received responsesRequest
	var rawBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		rawBody = buf[:n]
		assert.NoError(t, json.Unmarshal(rawBody, &received), "decode request body")
		assert.True(t, strings.HasSuffix(r.URL.Path, "/responses"), "expected /responses path, got %q", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	tools := []types.ToolDefinition{
		{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
	}

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "gpt-4.1",
		System:      "You are helpful.",
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Tools:       tools,
		Messages: []types.Message{
			{
				Role: "user",
				Content: []types.ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					{Type: "text", Text: "I will read it."},
					{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					{Type: "tool_result", ToolUseID: "call_1", Content: "package main"},
				},
			},
		},
	})
	require.NoError(t, err)
	for range ch {
	}

	assert.True(t, received.Stream, "expected stream=true")
	assert.Equal(t, quirks.StoreFalse, received.StoreMode, "request must set store:false")
	assert.Equal(t, "You are helpful.", received.Instructions)
	assert.Equal(t, "gpt-4.1", received.Model)
	assert.Equal(t, 4096, received.MaxTokens)
	require.NotNil(t, received.Temperature)
	assert.InDelta(t, 0.5, *received.Temperature, 1e-9)

	assert.NotContains(t, string(rawBody), `"messages"`, "Responses API uses 'input'")
	assert.NotContains(t, string(rawBody), `"max_tokens"`, "Responses API uses 'max_output_tokens'")
	assert.Contains(t, string(rawBody), `"store":false`)

	// Input shape: user message, assistant message + function_call, function_call_output.
	require.GreaterOrEqual(t, len(received.Input), 4, "input items: %+v", received.Input)
	assert.Equal(t, "message", received.Input[0].Type)
	assert.Equal(t, "user", received.Input[0].Role)
	if assert.NotEmpty(t, received.Input[0].Content) {
		assert.Equal(t, "input_text", received.Input[0].Content[0].Type)
	}
	assert.Equal(t, "message", received.Input[1].Type)
	assert.Equal(t, "assistant", received.Input[1].Role)
	if assert.NotEmpty(t, received.Input[1].Content) {
		assert.Equal(t, "output_text", received.Input[1].Content[0].Type)
	}
	assert.Equal(t, "function_call", received.Input[2].Type)
	assert.Equal(t, "call_1", received.Input[2].CallID)
	assert.Equal(t, "read_file", received.Input[2].Name)
	assert.Equal(t, "function_call_output", received.Input[3].Type)
	assert.Equal(t, "call_1", received.Input[3].CallID)

	// Tools should use the flatter shape (no nested "function" object).
	require.Len(t, received.Tools, 1)
	assert.Equal(t, "function", received.Tools[0].Type)
	assert.Equal(t, "read_file", received.Tools[0].Name)
	assert.Equal(t, "Read a file", received.Tools[0].Description)
	assert.NotContains(t, string(rawBody), `"function":{"name"`, "Responses API uses a flat tool shape")

	// A non-auto ToolChoice must still omit "tool_choice" from the raw
	// body: the adapter downgrades to auto and never serialises the field.
	t.Run("ToolChoiceRequired", func(t *testing.T) {
		var rawBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
		}))
		defer srv.Close()

		adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

		ch, err := adapter.Stream(context.Background(), types.StreamParams{
			Model:      "gpt-4.1",
			MaxTokens:  4096,
			ToolChoice: types.ToolChoiceRequired,
			Tools:      tools,
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Hello"}}},
			},
		})
		require.NoError(t, err)
		for range ch {
		}

		require.NotContains(t, string(rawBody), `"tool_choice"`, "the Responses adapter downgrades to auto: %s", rawBody)
	})
}

// TestOpenAIResponsesAdapter_RequestBody_NonAutoToolChoiceOmitsField: even
// when a non-auto ToolChoice is requested, the Responses request body must
// never carry a "tool_choice" field, because the adapter does not support
// it and silently downgrades to auto.
func TestOpenAIResponsesAdapter_RequestBody_NonAutoToolChoiceOmitsField(t *testing.T) {
	var rawBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:      "gpt-4.1",
		MaxTokens:  1024,
		ToolChoice: types.ToolChoiceRequired,
		Tools: []types.ToolDefinition{
			{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		},
	})
	require.NoError(t, err)
	for range ch {
	}

	require.NotContains(t, string(rawBody), `"tool_choice"`, "the Responses adapter downgrades to auto: %s", rawBody)
}

func TestOpenAIResponsesAdapter_RequestBody_EmptyToolResult(t *testing.T) {
	// Drives Stream() with an empty tool_result block and asserts on the
	// raw HTTP body: catches any regression where intermediate processing
	// between translateMessagesResponses and json.Marshal silently elides
	// the "output" key.
	var rawBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		rawBody = buf[:n]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model: "gpt-4.1",
		Messages: []types.Message{
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					{Type: "tool_use", ID: "call_1", Name: "noop", Input: json.RawMessage(`{}`)},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					{Type: "tool_result", ToolUseID: "call_1", Content: ""},
				},
			},
		},
	})
	require.NoError(t, err)
	for range ch {
	}

	require.Contains(t, string(rawBody), `"output":""`, "request body must keep the empty output key: %s", rawBody)
}

// TestOpenAIResponsesAdapter_TemperatureWireShape pins the unset-vs-
// explicit-zero semantics for StreamParams.Temperature: a nil pointer must
// omit the key; an explicit Float64Ptr(0.0) must transmit "temperature":0
// (preserving caller-requested greedy decoding).
func TestOpenAIResponsesAdapter_TemperatureWireShape(t *testing.T) {
	cases := []struct {
		name              string
		temperature       *float64
		wantTemperature   bool
		wantTempSubstring string
	}{
		{name: "nil omitted", temperature: nil, wantTemperature: false},
		{name: "explicit zero serialised", temperature: types.Float64Ptr(0.0), wantTemperature: true, wantTempSubstring: `"temperature":0`},
		{name: "non-zero serialised", temperature: types.Float64Ptr(0.5), wantTemperature: true, wantTempSubstring: `"temperature":0.5`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rawBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rawBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
			}))
			defer srv.Close()

			adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

			ch, err := adapter.Stream(context.Background(), types.StreamParams{
				Model:       "gpt-5.4",
				MaxTokens:   1024,
				Temperature: tc.temperature,
			})
			require.NoError(t, err)
			for range ch {
			}

			body := string(rawBody)
			assert.Equal(t, tc.wantTemperature, strings.Contains(body, `"temperature"`), "temperature key present in body: %s", body)
			if tc.wantTempSubstring != "" {
				assert.Contains(t, body, tc.wantTempSubstring, "body: %s", body)
			}
		})
	}
}

func TestOpenAIResponsesAdapter_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	cancel()

	// An error event after cancellation is optional, so only a completed
	// message is rejected. collectEvents returning shows the channel closed.
	events := collectEvents(t, ch)
	for _, ev := range events {
		assert.NotEqual(t, "message_complete", ev.Type, "unexpected message_complete after cancel: %+v", ev)
	}
}

func TestOpenAIResponsesAdapter_DefaultBaseURL(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter(staticBearer("key"), "", OpenAIAuthConfig{})
	require.Equal(t, openaiDefaultBaseURL, adapter.baseURL)
}

func TestOpenAIResponsesAdapter_TrailingSlashBaseURL(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter(staticBearer("key"), "https://example.com/v1/", OpenAIAuthConfig{})
	require.Equal(t, "https://example.com/v1", adapter.baseURL)
}

func TestOpenAIResponsesAdapter_StreamingClientTimeouts(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), "", OpenAIAuthConfig{})
	assert.Zero(t, adapter.httpClient.Timeout, "a total deadline cuts long streams, which idleTimeoutBody bounds instead")
	tr, ok := adapter.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport")
	assert.NotZero(t, tr.TLSHandshakeTimeout, "TLSHandshakeTimeout should be non-zero")
	assert.NotZero(t, tr.ResponseHeaderTimeout, "ResponseHeaderTimeout should be non-zero")
}

func TestOpenAIResponsesAdapter_RecordsLatencyAndTTFB(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"Hi"}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"message","id":"msg_1"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	prov := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(prov)
	require.NoError(t, err)

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.Metrics = metrics

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	for range ch {
	}

	assert.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "provider_latency count")
	assert.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count")
	assertFirstChunkSeries(t, reader, adapter, "openai", "gpt-4.1")

	h, ok := providerHistogramFinder(t, reader, "stirrup.harness.provider_latency")
	require.True(t, ok, "provider_latency histogram is missing")
	require.NotEmpty(t, h.DataPoints, "provider_latency has no data points")
	attrs := h.DataPoints[0].Attributes

	providerType, ok := attrs.Value("provider.type")
	assert.True(t, ok, "provider.type attribute is missing")
	assert.Equal(t, "openai-responses", providerType.AsString())
	model, ok := attrs.Value("provider.model")
	assert.True(t, ok, "provider.model attribute is missing")
	assert.Equal(t, "gpt-4.1", model.AsString())
}

func TestOpenAIResponsesAdapter_RecordsLatencyOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	prov := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(prov)
	require.NoError(t, err)

	adapter := NewOpenAIResponsesAdapter(staticBearer("bad-key"), srv.URL, OpenAIAuthConfig{})
	adapter.Metrics = metrics

	_, err = adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.Error(t, err)

	assert.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "provider_latency count")
	assert.EqualValues(t, 0, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count")
}

// --- Translation unit tests ---

func TestTranslateMessagesResponses_MapsTextToolUseAndToolResult(t *testing.T) {
	messages := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "text", Text: "Hello"},
			},
		},
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "text", Text: "Let me check."},
				{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Content: "package main"},
			},
		},
	}

	result := translateMessagesResponses(messages, "")

	// user message -> 1 item (input_text)
	// assistant text + tool_use -> 1 message item + 1 function_call item
	// tool_result -> 1 function_call_output item
	require.Len(t, result, 4, "input items: %+v", result)
	assert.Equal(t, "message", result[0].Type)
	assert.Equal(t, "user", result[0].Role)
	require.NotEmpty(t, result[0].Content)
	assert.Equal(t, "input_text", result[0].Content[0].Type)
	assert.Equal(t, "message", result[1].Type)
	assert.Equal(t, "assistant", result[1].Role)
	require.NotEmpty(t, result[1].Content)
	assert.Equal(t, "output_text", result[1].Content[0].Type)
	assert.Equal(t, "function_call", result[2].Type)
	assert.Equal(t, "call_1", result[2].CallID)
	assert.Equal(t, "read_file", result[2].Name)
	assert.Equal(t, "function_call_output", result[3].Type)
	assert.Equal(t, "call_1", result[3].CallID)
	assert.Equal(t, "package main", result[3].Output)

	// Marshal-roundtrip check: a future omitempty regression on the Output
	// field would be invisible to the struct comparison above.
	raw, err := json.Marshal(result[3])
	require.NoError(t, err, "marshal result[3]")
	assert.Contains(t, string(raw), `"output":"package main"`, "marshalled result[3]: %s", raw)
}

func TestTranslateMessagesResponses_ErrorToolResult(t *testing.T) {
	messages := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Content: "file not found", IsError: true},
			},
		},
	}

	result := translateMessagesResponses(messages, "")
	require.Len(t, result, 1)
	assert.Equal(t, "function_call_output", result[0].Type)
	assert.True(t, strings.HasPrefix(result[0].Output, "Error: "), "expected error-prefixed output, got %q", result[0].Output)

	// Marshal-roundtrip check: an omitempty regression on Output would not
	// trip the struct assertions above for non-empty payloads, but the wire
	// shape is what the API actually validates.
	raw, err := json.Marshal(result[0])
	require.NoError(t, err, "marshal result[0]")
	assert.Contains(t, string(raw), `"output":"Error: file not found"`, "marshalled result[0]: %s", raw)
}

func TestTranslateMessagesResponses_EmptyToolResultContent(t *testing.T) {
	// An empty tool_result must marshal with an explicit empty output key,
	// because the Responses API rejects a function_call_output without one.
	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_1", Content: ""},
		}},
	}
	result := translateMessagesResponses(messages, "")
	require.Len(t, result, 1)
	assert.Equal(t, "function_call_output", result[0].Type)
	assert.Equal(t, "call_1", result[0].CallID)
	assert.Empty(t, result[0].Output)

	raw, err := json.Marshal(result[0])
	require.NoError(t, err, "marshal result[0]")
	assert.Contains(t, string(raw), `"output":""`, "marshalled JSON: %s", raw)
}

func TestTranslateMessagesResponses_EmptyErrorToolResultContent(t *testing.T) {
	// An error tool_result with empty content still has a non-empty output
	// ("Error: "), so its wire shape is checked separately from the empty case.
	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_1", Content: "", IsError: true},
		}},
	}
	result := translateMessagesResponses(messages, "")
	require.Len(t, result, 1)
	assert.Equal(t, "function_call_output", result[0].Type)
	assert.Equal(t, "Error: ", result[0].Output)

	raw, err := json.Marshal(result[0])
	require.NoError(t, err, "marshal result[0]")
	assert.Contains(t, string(raw), `"output":"Error: "`, "marshalled JSON: %s", raw)
}

func TestTranslateMessagesResponses_MultipleEmptyToolResultContents(t *testing.T) {
	// Every empty tool_result in a multi-block user message must marshal with
	// an explicit empty output key, not only the first.
	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_1", Content: ""},
			{Type: "tool_result", ToolUseID: "call_2", Content: ""},
		}},
	}
	result := translateMessagesResponses(messages, "")
	require.Len(t, result, 2)

	wantIDs := []string{"call_1", "call_2"}
	for i, item := range result {
		assert.Equal(t, "function_call_output", item.Type, "result[%d].Type", i)
		assert.Equal(t, wantIDs[i], item.CallID, "result[%d].CallID", i)
		assert.Empty(t, item.Output, "result[%d].Output", i)

		raw, err := json.Marshal(item)
		require.NoError(t, err, "marshal result[%d]", i)
		assert.Contains(t, string(raw), `"output":""`, "marshalled result[%d]: %s", i, raw)
	}
}

func TestTranslateMessagesResponses_AssistantToolUseEmptyInput(t *testing.T) {
	// An assistant tool_use block with empty Input should serialise as
	// "{}", not "" — a missing Arguments field would be ambiguous.
	messages := []types.Message{
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "noop", Input: nil},
			},
		},
	}
	result := translateMessagesResponses(messages, "")
	require.Len(t, result, 1, "result: %+v", result)
	require.Equal(t, "function_call", result[0].Type)
	require.Equal(t, "{}", result[0].Arguments)
}

// A message item must emit only the keys valid for the message variant,
// because stricter validators reject unknown parameters such as
// input[N].output with HTTP 400.
func TestResponsesInputMarshal_MessageOmitsOutput(t *testing.T) {
	item := responsesInput{
		Type: "message",
		Role: "user",
		Content: []responsesContentBlock{
			{Type: "input_text", Text: "hello"},
		},
	}
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	wire := string(raw)
	assert.NotContains(t, wire, `"output"`, "message item must not emit an output key")
	assert.NotContains(t, wire, `"call_id"`, "message item must not emit a call_id key")
	assert.NotContains(t, wire, `"name"`, "message item must not emit a name key")
	assert.NotContains(t, wire, `"arguments"`, "message item must not emit an arguments key")
	assert.Contains(t, wire, `"type":"message"`, "message item must emit type=message")
	assert.Contains(t, wire, `"role":"user"`, "message item must emit role")
}

// TestResponsesInputMarshal_FunctionCallOmitsOutput: on the function_call
// variant only type/call_id/name/arguments are valid, so the spurious
// "output" key (and "role"/"content") must not leak across the
// discriminant.
func TestResponsesInputMarshal_FunctionCallOmitsOutput(t *testing.T) {
	item := responsesInput{
		Type:      "function_call",
		CallID:    "call_abc",
		Name:      "read_file",
		Arguments: `{"path":"main.go"}`,
	}
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	wire := string(raw)
	assert.NotContains(t, wire, `"output"`, "function_call item must not emit an output key")
	assert.NotContains(t, wire, `"role"`, "function_call item must not emit a role key")
	assert.NotContains(t, wire, `"content"`, "function_call item must not emit a content key")
	assert.Contains(t, wire, `"type":"function_call"`, "function_call item must emit type=function_call")
	assert.Contains(t, wire, `"call_id":"call_abc"`, "function_call item must emit call_id")
}

// A function_call_output item must emit "output":"" for an empty result,
// because the Responses API rejects a missing output with HTTP 400.
func TestResponsesInputMarshal_FunctionCallOutputKeepsEmptyOutput(t *testing.T) {
	item := responsesInput{
		Type:   "function_call_output",
		CallID: "call_1",
		Output: "",
	}
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	wire := string(raw)
	assert.Contains(t, wire, `"output":""`, "function_call_output must emit an empty output")
	assert.NotContains(t, wire, `"role"`, "function_call_output must not emit a role key")
	assert.NotContains(t, wire, `"content"`, "function_call_output must not emit a content key")
	assert.NotContains(t, wire, `"name"`, "function_call_output must not emit a name key")
	assert.NotContains(t, wire, `"arguments"`, "function_call_output must not emit an arguments key")
	assert.Contains(t, wire, `"type":"function_call_output"`, "function_call_output must emit type=function_call_output")
	assert.Contains(t, wire, `"call_id":"call_1"`, "function_call_output must emit call_id")
}

// A function_call_output item carrying a result must send that result
// verbatim under the output key.
func TestResponsesInputMarshal_FunctionCallOutputNonEmpty(t *testing.T) {
	item := responsesInput{
		Type:   "function_call_output",
		CallID: "call_1",
		Output: "some result",
	}
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	wire := string(raw)
	assert.Contains(t, wire, `"output":"some result"`, "function_call_output must carry a non-empty output verbatim")
	assert.Contains(t, wire, `"type":"function_call_output"`, "function_call_output must emit type=function_call_output")
	assert.Contains(t, wire, `"call_id":"call_1"`, "function_call_output must emit call_id")
}

// Marshalling an unrecognised type must fail, so a type added without a
// wire variant cannot be serialised silently.
func TestResponsesInputMarshal_UnknownTypeErrors(t *testing.T) {
	_, err := json.Marshal(responsesInput{Type: "reasoning"})
	require.Error(t, err)
}

// TestResponsesInput_RoundTripPreservesVariantFields checks that marshalling
// and then unmarshalling returns the original value for every field valid on
// each variant.
func TestResponsesInput_RoundTripPreservesVariantFields(t *testing.T) {
	cases := []struct {
		name string
		in   responsesInput
	}{
		{
			name: "message",
			in: responsesInput{
				Type: "message",
				Role: "user",
				Content: []responsesContentBlock{
					{Type: "input_text", Text: "hello"},
				},
			},
		},
		{
			name: "function_call",
			in: responsesInput{
				Type:      "function_call",
				CallID:    "call_abc",
				Name:      "read_file",
				Arguments: `{"path":"main.go"}`,
			},
		},
		{
			name: "function_call_output",
			in: responsesInput{
				Type:   "function_call_output",
				CallID: "call_abc",
				Output: "file contents",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.in)
			require.NoError(t, err)
			var got responsesInput
			require.NoError(t, json.Unmarshal(raw, &got))

			require.Equal(t, tc.in.Type, got.Type)
			switch tc.in.Type {
			case "message":
				require.Equal(t, tc.in.Role, got.Role)
				require.Len(t, got.Content, len(tc.in.Content))
				for i := range tc.in.Content {
					require.Equal(t, tc.in.Content[i], got.Content[i], "Content[%d]", i)
				}
			case "function_call":
				require.Equal(t, tc.in.CallID, got.CallID)
				require.Equal(t, tc.in.Name, got.Name)
				require.Equal(t, tc.in.Arguments, got.Arguments)
			case "function_call_output":
				require.Equal(t, tc.in.CallID, got.CallID)
				require.Equal(t, tc.in.Output, got.Output)
			}
		})
	}
}

// Unmarshalling an unrecognised type must fail rather than silently
// zeroing the item. This is the inverse of the marshal-side guard in
// TestResponsesInputMarshal_UnknownTypeErrors.
func TestResponsesInputUnmarshal_UnknownTypeErrors(t *testing.T) {
	var item responsesInput
	err := json.Unmarshal([]byte(`{"type":"reasoning"}`), &item)
	require.Error(t, err)
}

func TestTranslateToolsResponses_MapsFunctionTools(t *testing.T) {
	tools := []types.ToolDefinition{
		{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		{
			Name:        "write_file",
			Description: "Write a file",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}
	result, err := translateToolsResponses(tools, false, false, "gpt-4o", nil)
	require.NoError(t, err)
	require.Len(t, result, 2)
	for i, tool := range result {
		require.Equal(t, "function", tool.Type, "tools[%d].Type", i)
		require.NotEmpty(t, tool.Name, "tools[%d].Name", i)
	}
	require.Equal(t, "read_file", result[0].Name)
	require.Equal(t, "write_file", result[1].Name)
}

func TestTranslateToolsResponses_NilOrEmptyReturnsNil(t *testing.T) {
	tests := []struct {
		name  string
		tools []types.ToolDefinition
	}{
		{name: "nil", tools: nil},
		{name: "empty", tools: []types.ToolDefinition{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := translateToolsResponses(tt.tools, false, false, "gpt-4o", nil)
			require.NoError(t, err)
			require.Nil(t, got)
		})
	}
}

func TestDeriveStopReason_MapsStatusToStopReason(t *testing.T) {
	tests := []struct {
		name string
		in   responsesResponse
		want string
	}{
		{
			name: "completed text only",
			in:   responsesResponse{Status: "completed", Output: []responsesOutputItem{{Type: "message"}}},
			want: "end_turn",
		},
		{
			name: "completed with function call",
			in:   responsesResponse{Status: "completed", Output: []responsesOutputItem{{Type: "function_call"}}},
			want: "tool_use",
		},
		{
			name: "incomplete due to max_output_tokens",
			in: responsesResponse{Status: "incomplete", IncompleteDetails: &struct {
				Reason string `json:"reason"`
			}{Reason: "max_output_tokens"}},
			want: "max_tokens",
		},
		{
			name: "incomplete with other reason",
			in: responsesResponse{Status: "incomplete", IncompleteDetails: &struct {
				Reason string `json:"reason"`
			}{Reason: "content_filter"}},
			want: "content_filter",
		},
		{
			name: "incomplete with no reason",
			in:   responsesResponse{Status: "incomplete"},
			want: "incomplete",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, deriveStopReason(tt.in))
		})
	}
}

// An empty API key must result in no Authorization header on the request.
func TestOpenAIResponsesAdapter_EmptyKeyOmitsAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"), "expected no Authorization header")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer(""), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "local", MaxTokens: 1024})
	require.NoError(t, err)
	for range ch {
	}
}

// The stream goroutine must exit and close the channel when the consumer
// cancels its context while the producer is blocked on a full channel.
func TestOpenAIResponsesAdapter_BackpressureCancellation(t *testing.T) {
	// Continuously stream small SSE records as fast as possible so the
	// channel buffer fills before the consumer gets a chance to drain.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, err := fmt.Fprint(w, makeResponsesEvent("response.output_text.delta",
				`{"item_id":"msg_1","output_index":0,"delta":"x"}`))
			if err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)

	// Drain a handful of events so the goroutine starts producing, then
	// stop reading and cancel. The goroutine must observe ctx.Done()
	// instead of blocking on the channel.
	for i := 0; i < 4; i++ {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			require.Fail(t, "timed out reading initial events")
		}
	}
	cancel()

	// A send that ignores ctx would block the goroutine before its
	// close(ch) defer, so the channel would never close.
	closed := make(chan struct{})
	go func() {
		for range ch {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		require.Fail(t, "channel did not close within 3s after cancel; the stream goroutine may have leaked")
	}
}

// A single SSE record whose accumulated data lines exceed the input cap
// must produce one bounded error event and end the stream.
func TestOpenAIResponsesAdapter_SSERecordSizeCapEnforced(t *testing.T) {
	// Two data: lines, each just under 6MB, totalling > 10MB.
	const half = 6 * 1024 * 1024
	bigChunk := strings.Repeat("z", half)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\n")
		_, _ = fmt.Fprintf(w, "data: %s\n", bigChunk)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", bigChunk)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	require.Len(t, events, 1, "events: %+v", events)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "byte limit")
}

// provider_latency must be recorded even when the caller cancels its
// context before draining the stream.
func TestOpenAIResponsesAdapter_LatencyRecordedAfterContextCancel(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"hi"}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"message","id":"msg_1"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	prov := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = prov.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(prov)
	require.NoError(t, err)

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.Metrics = metrics

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	// Cancel before draining so the latency recording happens after the
	// caller's context is already done.
	cancel()
	for range ch {
	}

	// The goroutine records latency asynchronously, so poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency") == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, uint64(1), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"),
		"latency must be recorded against a background context")
}

// A malformed response.completed payload must surface as an error event and
// must not emit message_complete.
func TestOpenAIResponsesAdapter_MalformedCompletedEvent(t *testing.T) {
	body := makeResponsesEvent("response.completed", `NOT_VALID_JSON`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	require.Len(t, events, 1, "events: %+v", events)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "parse response.completed")
	for _, ev := range events {
		require.NotEqual(t, "message_complete", ev.Type, "message_complete must not be emitted on malformed completed: %+v", ev)
	}
}

// Each event type that decodes its payload must emit one error event for a
// malformed payload and end the stream.
func TestOpenAIResponsesAdapter_MalformedDispatchEvents(t *testing.T) {
	cases := []struct {
		name      string
		eventName string
		errSubstr string
	}{
		{"output_item.added", "response.output_item.added", "parse output_item.added"},
		{"output_text.delta", "response.output_text.delta", "parse output_text.delta"},
		{"function_call_arguments.delta", "response.function_call_arguments.delta", "parse function_call_arguments.delta"},
		{"function_call_arguments.done", "response.function_call_arguments.done", "parse function_call_arguments.done"},
		{"output_item.done", "response.output_item.done", "parse output_item.done"},
		{"response.incomplete", "response.incomplete", "parse response.incomplete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := makeResponsesEvent(tc.eventName, "{NOT JSON")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, body)
			}))
			defer srv.Close()

			adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
			ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
			require.NoError(t, err)
			events := collectEvents(t, ch)

			require.Len(t, events, 1, "events: %+v", events)
			require.Equal(t, "error", events[0].Type)
			require.ErrorContains(t, events[0].Error, tc.errSubstr)
		})
	}
}

// An empty item_id falls back to keying the call by its output index.
func TestCallKey_FallbackToIndex(t *testing.T) {
	require.Equal(t, "idx:3", callKey("", 3))
	require.Equal(t, "real_id", callKey("real_id", 3))
}

// An arguments delta that arrives before its output_item.added must create
// the call state lazily, and the tool_call must still be emitted at .done.
func TestOpenAIResponsesAdapter_DeltaBeforeAdded(t *testing.T) {
	body := strings.Join([]string{
		// No output_item.added: the state is created on the first delta.
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_lazy","output_index":0,"delta":"{\"k\":1}"}`),
		// The done event echoes the call metadata, giving the call its ID and name.
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"function_call","id":"fc_lazy","call_id":"call_lazy","name":"noop"}}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"function_call","id":"fc_lazy","call_id":"call_lazy","name":"noop"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	var toolCall *types.StreamEvent
	for i := range events {
		if events[i].Type == "tool_call" {
			toolCall = &events[i]
			break
		}
	}
	require.NotNil(t, toolCall, "expected a tool_call event, got: %+v", events)
	require.Equal(t, "call_lazy", toolCall.ID)
	require.Equal(t, "noop", toolCall.Name)
	require.InDelta(t, 1, toolCall.Input["k"], 0)
}

// When a delta and its done event carry no item_id, the call is keyed by
// output index and must still produce a tool_call with the parsed arguments.
func TestOpenAIResponsesAdapter_DeltaBeforeAddedNoItemIDFallback(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.function_call_arguments.delta", `{"output_index":2,"delta":"{\"a\":\"b\"}"}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"output_index":2,"arguments":"{\"a\":\"b\"}"}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	var toolCalls []types.StreamEvent
	for _, ev := range events {
		require.NotEqual(t, "error", ev.Type, "unexpected error event: %v", ev.Error)
		if ev.Type == "tool_call" {
			toolCalls = append(toolCalls, ev)
		}
	}
	require.NotEmpty(t, toolCalls, "expected tool_call event, got: %+v", events)
	for _, call := range toolCalls {
		require.Equal(t, "b", call.Input["a"])
	}
}

// TestDeriveStopReason_DefaultUnknownStatus exercises the default branch
// of deriveStopReason — an unknown Responses status (e.g. cancelled)
// passes through verbatim so the agentic loop can surface it as the
// run outcome.
func TestDeriveStopReason_DefaultUnknownStatus(t *testing.T) {
	tests := []struct {
		name string
		resp responsesResponse
		want string
	}{
		{
			name: "unknown status passes through verbatim",
			resp: responsesResponse{Status: "cancelled"},
			want: "cancelled",
		},
		{
			name: "empty status with a function call is tool_use",
			resp: responsesResponse{Output: []responsesOutputItem{{Type: "function_call"}}},
			want: "tool_use",
		},
		{
			name: "empty status with only a message is end_turn",
			resp: responsesResponse{},
			want: "end_turn",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, deriveStopReason(tt.resp))
		})
	}
}

// TestDeriveStopReason_MaxTokensSpelling exercises the second OR arm
// of the max_tokens detection (`max_tokens` literal, not
// `max_output_tokens`).
func TestDeriveStopReason_MaxTokensSpelling(t *testing.T) {
	in := responsesResponse{
		Status: "incomplete",
		IncompleteDetails: &struct {
			Reason string `json:"reason"`
		}{Reason: "max_tokens"},
	}
	require.Equal(t, "max_tokens", deriveStopReason(in))
}

// TestOpenAIResponsesAdapter_IncompleteMaxTokensAlternateSpelling
// covers the same alternate spelling path inside the SSE dispatch for
// response.incomplete.
func TestOpenAIResponsesAdapter_IncompleteMaxTokensAlternateSpelling(t *testing.T) {
	body := makeResponsesEvent("response.incomplete",
		`{"response":{"status":"incomplete","incomplete_details":{"reason":"max_tokens"}}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "message_complete", last.Type)
	require.Equal(t, "max_tokens", last.StopReason)
}

// TestOpenAIResponsesAdapter_SSEParser_ToleratesCommentsAndTrailingRecord
// checks that the SSE parser skips comment lines (`:` prefix), accepts
// header lines without a trailing space (`event:`/`data:`), and flushes
// a trailing record at EOF without a terminating blank line.
func TestOpenAIResponsesAdapter_SSEParser_ToleratesCommentsAndTrailingRecord(t *testing.T) {
	// Note: no terminating blank line on the last record — relies on
	// the EOF-flush path.
	body := ":heartbeat\n" +
		":another comment\n" +
		"event:response.output_item.added\n" +
		"data:{\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\"}}\n" +
		"\n" +
		"event:response.output_text.delta\n" +
		"data:{\"item_id\":\"msg_1\",\"output_index\":0,\"delta\":\"hi\"}\n" +
		"\n" +
		// Trailing record without final blank line — must still flush.
		"event:response.completed\n" +
		"data:{\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\"}]}}\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	require.Len(t, events, 2, "expected text_delta then message_complete, got %+v", events)
	require.Equal(t, "text_delta", events[0].Type)
	require.Equal(t, "hi", events[0].Text)
	require.Equal(t, "message_complete", events[1].Type)
	require.Equal(t, "end_turn", events[1].StopReason)
}

// TestOpenAIResponsesAdapter_ConnectionRefused_ReturnsRequestError covers
// the connection-level failure path: the server is closed before the
// request is sent.
func TestOpenAIResponsesAdapter_ConnectionRefused_ReturnsRequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), addr, OpenAIAuthConfig{})
	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.ErrorContains(t, err, "execute request")
}

// TestOpenAIResponsesAdapter_5xxWithoutBody_ReturnsStatusCode covers the
// 5xx fallback path where the body is absent or unparseable: the adapter
// returns the generic status-code error rather than crashing.
func TestOpenAIResponsesAdapter_5xxWithoutBody_ReturnsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.ErrorContains(t, err, "503")
}

// TestOpenAIResponsesAdapter_RateLimit_RecordsRetryAfterSpanEvent checks
// that the 429 branch attaches a rate_limited span event carrying the
// Retry-After header.
func TestOpenAIResponsesAdapter_RateLimit_RecordsRetryAfterSpanEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.Tracer = tracer

	// We need the span to be in ctx so SpanFromContext finds it.
	ctx, span := tracer.Start(context.Background(), "test")

	_, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.Error(t, err)
	span.End()

	stubs := exporter.GetSpans()
	require.NotEmpty(t, stubs, "expected at least one finished span")
	var retryAfters []string
	for _, s := range stubs {
		for _, ev := range s.Events {
			if ev.Name != "rate_limited" {
				continue
			}
			retryAfters = append(retryAfters, spanEventAttribute(ev, "retry_after"))
		}
	}
	require.NotEmpty(t, retryAfters, "expected a rate_limited span event")
	for _, got := range retryAfters {
		require.Equal(t, "42", got)
	}
}

// spanEventAttribute returns the string value of the named attribute on a
// span event, or "" when the attribute is absent.
func spanEventAttribute(ev sdktrace.Event, key string) string {
	for _, attr := range ev.Attributes {
		if string(attr.Key) == key {
			return attr.Value.AsString()
		}
	}
	return ""
}

// Note: the dispatch-level arg cap on `.function_call_arguments.done` and
// `.output_item.done` is defensive only — the same byte limit is enforced
// earlier by the SSE scanner's per-line cap and the dataParts aggregate
// cap. Kept for defense-in-depth against a future relaxation of the
// scanner cap; not exercised by a dedicated test since it is unreachable
// with the current configuration.

// TestOpenAIResponsesAdapter_UnknownEvent_RecordsSpanEvent checks that an
// unknown SSE event type is ignored and recorded as a span event for
// production observability.
func TestOpenAIResponsesAdapter_UnknownEvent_RecordsSpanEvent(t *testing.T) {
	body := strings.Join([]string{
		// Unknown event type — must be ignored but tagged on the span.
		makeResponsesEvent("response.reasoning_summary.delta", `{"text":"thinking..."}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.Tracer = tracer

	ctx, span := tracer.Start(context.Background(), "test")
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	require.NoError(t, err)
	for range ch {
	}
	span.End()

	var eventTypes []string
	for _, s := range exporter.GetSpans() {
		for _, ev := range s.Events {
			if ev.Name != "openai_responses.unknown_sse_event" {
				continue
			}
			eventTypes = append(eventTypes, spanEventAttribute(ev, "event.type"))
		}
	}
	require.NotEmpty(t, eventTypes, "expected an openai_responses.unknown_sse_event span event")
	for _, typ := range eventTypes {
		require.Equal(t, "response.reasoning_summary.delta", typ)
	}
}

// TestTranslateMessagesResponses_UserTextAndToolResultOrder pins the
// emission ordering for a user message containing
// [text, tool_result, text]. The current contract is: function_call_output
// items emit first (in their document order); user text is batched into
// a single trailing input_text item. See the comment in
// translateMessagesResponses for rationale.
func TestTranslateMessagesResponses_UserTextAndToolResultOrder(t *testing.T) {
	messages := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "text", Text: "Before. "},
				{Type: "tool_result", ToolUseID: "call_1", Content: "first result"},
				{Type: "text", Text: "After."},
			},
		},
	}
	result := translateMessagesResponses(messages, "")
	require.Len(t, result, 2, "got %+v", result)
	require.Equal(t, "function_call_output", result[0].Type)
	require.Equal(t, "message", result[1].Type)
	require.Equal(t, "user", result[1].Role)
	require.NotEmpty(t, result[1].Content)
	require.Equal(t, "Before. After.", result[1].Content[0].Text)
}

// TestOpenAIResponsesAdapter_AzureKeyHeaderAndQueryParam is the Azure OpenAI
// regression guard for the Responses adapter: provider.type
// "openai-responses" with apiKeyHeader: "api-key" and queryParams:
// {"api-version": "preview"} produces a request hitting
// /openai/v1/responses?api-version=preview, carrying header api-key: <KEY>,
// and NO Authorization header (Bearer fallback must not engage).
func TestOpenAIResponsesAdapter_AzureKeyHeaderAndQueryParam(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.created", `{"response":{"id":"resp_1","status":"in_progress"}}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`),
	}, "")

	// The handler runs on the server goroutine, so it reports with assert.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/openai/v1/responses", r.URL.Path)
		assert.Equal(t, "preview", r.URL.Query().Get("api-version"))
		assert.Equal(t, "AZURE-KEY", r.Header.Get("api-key"))
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("AZURE-KEY"), srv.URL+"/openai/v1", OpenAIAuthConfig{
		APIKeyHeader: "api-key",
		QueryParams:  map[string]string{"api-version": "preview"},
	})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	require.NoError(t, err)
	for range ch { //nolint:revive // drain stream
	}
}

// TestOpenAIResponsesAdapter_QueryParamsOverrideBaseURL pins the precedence
// rule for the Responses adapter: when BaseURL already encodes a query key,
// QueryParams entries with the same key win, while unrelated BaseURL keys
// survive untouched. Mirror of the Chat Completions equivalent.
func TestOpenAIResponsesAdapter_QueryParamsOverrideBaseURL(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`),
	}, "")

	// The handler runs on the server goroutine, so it reports with assert.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		assert.Equal(t, "preview", q.Get("api-version"), "QueryParams should override BaseURL")
		assert.Equal(t, "azure", q.Get("flavour"), "BaseURL key should survive")
		assert.Len(t, q["api-version"], 1, "api-version should appear exactly once")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("KEY"), srv.URL+"/openai/v1?api-version=2024-10-01-preview&flavour=azure", OpenAIAuthConfig{
		QueryParams: map[string]string{"api-version": "preview"},
	})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	require.NoError(t, err)
	for range ch { //nolint:revive // drain stream
	}
}

// TestOpenAIResponsesAdapter_BearerClosureError exercises the
// resolveBearer error branch on the Responses adapter — same shape
// as the openai-compatible test, distinct adapter implementation.
func TestOpenAIResponsesAdapter_BearerClosureError(t *testing.T) {
	// The handler runs on the server goroutine, so it reports with assert.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		assert.Fail(t, "openai-responses adapter should not have hit the network when the bearer closure errors")
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(erroringBearer("federation: STS returned 401"), srv.URL, OpenAIAuthConfig{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 16,
	})
	require.ErrorContains(t, err, "STS returned 401", "error should preserve closure cause")
}
