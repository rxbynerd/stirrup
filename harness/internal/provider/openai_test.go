package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

// makeOpenAIChunk builds a single SSE data line from a raw JSON string.
func makeOpenAIChunk(data string) string {
	return fmt.Sprintf("data: %s\n\n", data)
}

func TestOpenAIAdapter_StreamTextDelta(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":5}}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		assert.True(t, strings.HasSuffix(r.URL.Path, "/chat/completions"), "path %q", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Len(t, events, 3, "events: %+v", events)

	assert.Equal(t, "text_delta", events[0].Type)
	assert.Equal(t, "Hello", events[0].Text)
	assert.Equal(t, "text_delta", events[1].Type)
	assert.Equal(t, " world", events[1].Text)
	assert.Equal(t, "message_complete", events[2].Type)
	assert.Equal(t, "end_turn", events[2].StopReason)
	assert.Equal(t, 5, events[2].OutputTokens)
}

// TestOpenAIAdapter_StreamLongDataLine checks that one SSE data line larger
// than bufio.Scanner's default 64 KB budget streams as ordinary content rather
// than failing with bufio.ErrTooLong. The 200 KB payload is above that default
// and below the 16 MiB maxSSEScannerBuffer cap.
func TestOpenAIAdapter_StreamLongDataLine(t *testing.T) {
	longText := strings.Repeat("a", 200*1024)
	encoded, err := json.Marshal(longText)
	require.NoError(t, err)

	body := strings.Join([]string{
		makeOpenAIChunk(fmt.Sprintf(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, encoded)),
		makeOpenAIChunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Len(t, events, 2)

	assert.Equal(t, "text_delta", events[0].Type)
	assert.Len(t, events[0].Text, len(longText))
	assert.Equal(t, "message_complete", events[1].Type)
	assert.Equal(t, "end_turn", events[1].StopReason)
}

func TestOpenAIAdapter_StreamToolCall(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\"}"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Len(t, events, 2, "events: %+v", events)

	require.Equal(t, "tool_call", events[0].Type)
	assert.Equal(t, "call_abc", events[0].ID)
	assert.Equal(t, "read_file", events[0].Name)
	assert.Equal(t, "main.go", events[0].Input["path"])

	assert.Equal(t, "message_complete", events[1].Type)
	assert.Equal(t, "tool_use", events[1].StopReason)
}

func TestOpenAIAdapter_MultipleToolCalls(t *testing.T) {
	body := strings.Join([]string{
		// Two tool calls started in the same chunk.
		makeOpenAIChunk(`{"id":"chatcmpl-3","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_aaa","type":"function","function":{"name":"read_file","arguments":""}},{"index":1,"id":"call_bbb","type":"function","function":{"name":"write_file","arguments":""}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-3","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"b.go\",\"content\":\"x\"}"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
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

func TestOpenAIAdapter_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("bad-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "Invalid API key")
}

func TestOpenAIAdapter_HTTPErrorNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "500")
}

func TestOpenAIAdapter_HTTPErrorLargeBody(t *testing.T) {
	// Error JSON larger than 4096 bytes should be safely handled by the
	// LimitReader without consuming unbounded memory.
	largeMsg := strings.Repeat("x", 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"%s","type":"invalid_request_error"}}`, largeMsg)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.Error(t, err)
	// With a 4096-byte limit on the body, the JSON won't fully decode,
	// so we should fall back to the generic status-code-only error.
	require.ErrorContains(t, err, "400")
}

func TestOpenAIAdapter_RequestBody(t *testing.T) {
	var received openaiRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&received)
		assert.NoError(t, err, "decode request body")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	tools := []types.ToolDefinition{
		{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
	}

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "gpt-4o",
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
		},
	})
	require.NoError(t, err)
	for range ch {
	}

	assert.True(t, received.Stream, "stream flag")
	assert.Equal(t, "gpt-4o", received.Model)
	assert.Equal(t, 4096, received.MaxTokens)
	assert.Equal(t, quirks.TokenFieldMaxCompletionTokens, received.TokenField, "default token field")
	require.NotNil(t, received.Temperature)
	assert.InDelta(t, 0.5, *received.Temperature, 1e-9)

	// The system prompt is sent as the first message, followed by the user turn.
	require.GreaterOrEqual(t, len(received.Messages), 2)
	assert.Equal(t, "system", received.Messages[0].Role)
	assert.Equal(t, "user", received.Messages[1].Role)

	require.Len(t, received.Tools, 1)
	assert.Equal(t, "function", received.Tools[0].Type)
	assert.Equal(t, "read_file", received.Tools[0].Function.Name)
}

// TestOpenAIAdapter_RawBodyShape captures the raw JSON request body and
// asserts on the exact key set seen by the upstream API. It guards
// against reviving the legacy "max_tokens" field (rejected by reasoning
// models — gpt-5.x / o-series — with HTTP 400) or dropping the
// omitempty on "temperature" (also rejected by reasoning models), and
// pins the unset-vs-explicit-zero semantics on Temperature.
func TestOpenAIAdapter_RawBodyShape(t *testing.T) {
	cases := []struct {
		name              string
		maxTokens         int
		temperature       *float64
		wantTemperature   bool
		wantTempSubstring string
	}{
		{
			name:            "nil temperature omitted",
			maxTokens:       4096,
			temperature:     nil,
			wantTemperature: false,
		},
		{
			name:              "explicit zero temperature serialised",
			maxTokens:         4096,
			temperature:       types.Float64Ptr(0.0),
			wantTemperature:   true,
			wantTempSubstring: `"temperature":0`,
		},
		{
			name:              "non-zero temperature serialised",
			maxTokens:         4096,
			temperature:       types.Float64Ptr(0.7),
			wantTemperature:   true,
			wantTempSubstring: `"temperature":0.7`,
		},
		{
			name:      "zero max_tokens still serialised",
			maxTokens: 0,
			// no omitempty on MaxCompletionTokens; zero must appear on the wire
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// rawBody is set by the handler before the response is sent, and the
			// stream is drained before it is read.
			var rawBody []byte

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				assert.NoError(t, err, "read body")
				rawBody = b
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()

			adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

			// Use gpt-4o so the test exercises the no-quirk default path:
			// the openai-compatible reasoning-class rules (which would
			// suppress temperature) apply only to gpt-5* and o[1-9]-*.
			ch, err := adapter.Stream(context.Background(), types.StreamParams{
				Model:       "gpt-4o",
				MaxTokens:   tc.maxTokens,
				Temperature: tc.temperature,
			})
			require.NoError(t, err)
			for range ch {
			}

			body := string(rawBody)

			assert.Contains(t, body, `"max_completion_tokens"`)
			// Match the bare legacy key, not the new field which contains it
			// as a substring.
			assert.NotContains(t, body, `"max_tokens"`)
			// MaxCompletionTokens has no omitempty, so a zero value must
			// appear on the wire, unlike a nil Temperature.
			if tc.maxTokens == 0 {
				assert.Contains(t, body, `"max_completion_tokens":0`)
			}

			assert.Equal(t, tc.wantTemperature, strings.Contains(body, `"temperature"`), "temperature key presence")
			if tc.wantTempSubstring != "" {
				assert.Contains(t, body, tc.wantTempSubstring)
			}
		})
	}
}

func TestOpenAIAdapter_ContextCancellation(t *testing.T) {
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

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	cancel()

	events := collectEvents(t, ch)
	require.NotEmpty(t, events, "expected at least one event after cancellation")
	require.Equal(t, "error", events[len(events)-1].Type)
}

func TestOpenAIAdapter_MalformedChunk(t *testing.T) {
	body := strings.Join([]string{
		"data: {not valid json}\n\n",
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	var errorEvents []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "error" {
			errorEvents = append(errorEvents, ev)
		}
	}
	require.NotEmpty(t, errorEvents, "expected an error event for malformed JSON chunk")
	for _, ev := range errorEvents {
		assert.Error(t, ev.Error, "error event has nil Error field")
	}
}

func TestOpenAIAdapter_MalformedToolArgumentsMarksToolCall(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"chatcmpl-4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"INVALID"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	requireMarkedToolCall(t, events, "call_bad", "read_file", `{"path":INVALID`)
}

// TestOpenAIAdapter_TrailingCommaArgumentsMarkOnlyThatCall pins that a
// malformed call does not stop the calls after it from being emitted.
func TestOpenAIAdapter_TrailingCommaArgumentsMarkOnlyThatCall(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"chatcmpl-5","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"main.go\",}"}},{"index":1,"id":"call_ok","type":"function","function":{"name":"list_directory","arguments":"{\"path\": \".\"}"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	require.NoError(t, err)

	var calls []types.StreamEvent
	for _, ev := range collectEvents(t, ch) {
		require.NotEqual(t, "error", ev.Type, "unexpected error event: %v", ev.Error)
		if ev.Type == "tool_call" {
			calls = append(calls, ev)
		}
	}
	require.Len(t, calls, 2, "tool_call events: %+v", calls)

	require.NotNil(t, calls[0].InvalidInput, "first call should carry its invalid arguments")
	assert.Equal(t, `{"path": "main.go",}`, calls[0].InvalidInput.Raw) //nolint:testifylint // deliberately malformed JSON, compared byte-for-byte

	assert.Nil(t, calls[1].InvalidInput)
	assert.Equal(t, ".", calls[1].Input["path"])
}

func TestOpenAIAdapter_DefaultBaseURL(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter(staticBearer("key"), "", OpenAIAuthConfig{}, RetryPolicy{})
	require.Equal(t, openaiDefaultBaseURL, adapter.baseURL)
}

func TestOpenAIAdapter_TrailingSlashBaseURL(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter(staticBearer("key"), "https://example.com/v1/", OpenAIAuthConfig{}, RetryPolicy{})
	require.Equal(t, "https://example.com/v1", adapter.baseURL)
}

func TestOpenAIAdapter_NoAPIKey(t *testing.T) {
	// Some local endpoints (Ollama, vLLM) don't require an API key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"), "expected no Authorization header")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer(""), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "llama3",
		MaxTokens: 1024,
	})
	require.NoError(t, err)
	for range ch {
	}
}

func TestTranslateMessages_MapsRolesAndToolCalls(t *testing.T) {
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

	result := translateMessages("System prompt", messages, nil)

	// system + user + assistant + tool = 4 messages.
	require.Len(t, result, 4)

	assert.Equal(t, "system", result[0].Role)
	assert.Equal(t, "user", result[1].Role)
	assert.Equal(t, "assistant", result[2].Role)
	require.Len(t, result[2].ToolCalls, 1)
	assert.Equal(t, "call_1", result[2].ToolCalls[0].ID)
	assert.Equal(t, "read_file", result[2].ToolCalls[0].Function.Name)
	assert.Equal(t, "tool", result[3].Role)
	assert.Equal(t, "call_1", result[3].ToolCallID)
}

func TestTranslateMessages_ErrorResultPrefixesContent(t *testing.T) {
	messages := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Content: "file not found", IsError: true},
			},
		},
	}

	result := translateMessages("", messages, nil)

	require.Len(t, result, 1)
	assert.Equal(t, "tool", result[0].Role)
	content, ok := result[0].Content.(string)
	require.True(t, ok, "result[0].Content type = %T", result[0].Content)
	assert.True(t, strings.HasPrefix(content, "Error: "), "expected error-prefixed content, got %q", content)
}

func TestMapFinishReason_TranslatesProviderValues(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "stop maps to end_turn", input: "stop", want: "end_turn"},
		{name: "tool_calls maps to tool_use", input: "tool_calls", want: "tool_use"},
		{name: "length maps to max_tokens", input: "length", want: "max_tokens"},
		{name: "content_filter passes through", input: "content_filter", want: "content_filter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, mapFinishReason(tt.input))
		})
	}
}

func TestOpenAIAdapter_TextThenToolCall(t *testing.T) {
	// Model emits some text reasoning, then a tool call — a common pattern.
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"chatcmpl-5","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me read that file."},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_xyz","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"chatcmpl-5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	// text_delta, tool_call, message_complete.
	require.Len(t, events, 3, "events: %+v", events)
	assert.Equal(t, "text_delta", events[0].Type)
	assert.Equal(t, "tool_call", events[1].Type)
	assert.Equal(t, "message_complete", events[2].Type)
}

// A bearer closure error must surface from Stream without a network
// round-trip. Otherwise a half-built request would mask the original
// credential failure behind an HTTP error.
func TestOpenAIAdapter_BearerClosureErrorFailsBeforeRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		assert.Fail(t, "openai-compatible adapter should not hit the network when the bearer closure errors")
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(erroringBearer("federation: STS returned 401"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 16,
	})
	require.Error(t, err, "expected error from bearer closure failure")
	assert.ErrorContains(t, err, "STS returned 401", "error should preserve closure cause")
}

func TestOpenAIAdapter_StreamingClientTimeouts(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), "", OpenAIAuthConfig{}, RetryPolicy{})
	require.Zero(t, adapter.httpClient.Timeout, "a total deadline cuts long streams; idleTimeoutBody bounds them instead")
	tr, ok := adapter.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport")
	assert.NotZero(t, tr.TLSHandshakeTimeout)
	assert.NotZero(t, tr.ResponseHeaderTimeout)
}

func TestOpenAIAdapter_RecordsLatencyAndTTFB(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(provider)
	require.NoError(t, err, "NewMetricsForTesting")

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Metrics = metrics

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err, "Stream")
	for range ch {
	}

	assert.Equal(t, uint64(1), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "provider_latency count")
	assert.Equal(t, uint64(1), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count")
	assertFirstChunkSeries(t, reader, adapter, "openai-compatible", "gpt-4o")

	h, ok := providerHistogramFinder(t, reader, "stirrup.harness.provider_latency")
	require.True(t, ok, "expected provider_latency histogram")
	require.NotEmpty(t, h.DataPoints, "expected provider_latency data point")
	attrs := h.DataPoints[0].Attributes
	providerType, ok := attrs.Value("provider.type")
	assert.True(t, ok, "provider.type attribute")
	assert.Equal(t, "openai-compatible", providerType.AsString(), "provider.type")
	providerModel, ok := attrs.Value("provider.model")
	assert.True(t, ok, "provider.model attribute")
	assert.Equal(t, "gpt-4o", providerModel.AsString(), "provider.model")
}

func TestOpenAIAdapter_RecordsLatencyOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(provider)
	require.NoError(t, err, "NewMetricsForTesting")

	adapter := NewOpenAICompatibleAdapter(staticBearer("bad-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Metrics = metrics

	_, err = adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	require.Error(t, err)
	assert.Equal(t, uint64(1), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "provider_latency count")
	assert.Equal(t, uint64(0), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count")
}

// TestOpenAIAdapter_AzureKeyHeaderAndQueryParam is the Azure OpenAI key-auth
// regression guard for the Chat Completions adapter. It verifies that
// providing OpenAIAuthConfig{APIKeyHeader: "api-key", QueryParams: {...}}
// causes the request to:
//   - hit /openai/v1/chat/completions?api-version=preview (path appended,
//     query string preserved);
//   - carry header "api-key: <KEY>"; and
//   - omit the "Authorization" header entirely (no fallback Bearer).
func TestOpenAIAdapter_AzureKeyHeaderAndQueryParam(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/openai/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "preview", r.URL.Query().Get("api-version"))
		assert.Equal(t, "AZURE-KEY", r.Header.Get("api-key"))
		assert.Empty(t, r.Header.Get("Authorization"), "Authorization header should be omitted")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("AZURE-KEY"), srv.URL+"/openai/v1", OpenAIAuthConfig{
		APIKeyHeader: "api-key",
		QueryParams:  map[string]string{"api-version": "preview"},
	}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.NoError(t, err)
	for range ch { //nolint:revive // drain stream
	}
}

// TestOpenAIAdapter_QueryParamsOverrideBaseURL pins the documented precedence
// rule: when both BaseURL's query string and OpenAIAuthConfig.QueryParams
// supply the same key, QueryParams wins. Other keys present in BaseURL are
// preserved in the final URL.
func TestOpenAIAdapter_QueryParamsOverrideBaseURL(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		assert.Equal(t, "preview", q.Get("api-version"), "QueryParams should override BaseURL")
		assert.Equal(t, "gpt4-prod", q.Get("deployment-id"), "BaseURL key should survive")
		// Exactly one value per key, with no duplicate-key trail.
		assert.Len(t, q["api-version"], 1, "api-version values")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("KEY"), srv.URL+"/v1?api-version=2024-10-01-preview&deployment-id=gpt4-prod", OpenAIAuthConfig{
		QueryParams: map[string]string{"api-version": "preview"},
	}, RetryPolicy{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	require.NoError(t, err)
	for range ch { //nolint:revive // drain stream
	}
}

// TestComposeOpenAIURL_TrimsAndAppends covers the URL-building helper in
// isolation: trailing slashes are trimmed, the path is appended, and
// duplicate keys collapse to a single QueryParams-controlled value.
func TestComposeOpenAIURL_TrimsAndAppends(t *testing.T) {
	got, err := composeOpenAIURL("https://api.example.com/openai/v1/", "/chat/completions", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.com/openai/v1/chat/completions", got)

	got, err = composeOpenAIURL("https://api.example.com/v1?api-version=stable", "/responses", map[string]string{"api-version": "preview"})
	require.NoError(t, err)
	// QueryParams should override the BaseURL-encoded api-version.
	assert.Contains(t, got, "api-version=preview")
	assert.NotContains(t, got, "api-version=stable")
	assert.Contains(t, got, "/v1/responses")
}

// TestOpenAIAdapter_HTTPDoErrorDoesNotContainURL asserts the *url.Error
// unwrapping documented in docs/security.md: it seeds an unmistakable
// secret into QueryParams and asserts it is absent from the returned
// error string, and that the error no longer wraps a *url.Error.
func TestOpenAIAdapter_HTTPDoErrorDoesNotContainURL(t *testing.T) {
	// httptest.Server with a Hijack handler that closes the underlying
	// TCP connection mid-request. http.Client.Do returns a *url.Error
	// wrapping the transport-layer failure with the request URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !assert.True(t, ok, "ResponseWriter does not support Hijack") {
			return
		}
		conn, _, err := hj.Hijack()
		if !assert.NoError(t, err, "hijack") {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	// An opaque, non-pattern secret: security.Scrub would NOT catch this,
	// which is exactly why the leak must be closed at the source rather than
	// relying on downstream scrubbing.
	const sensitive = "supersecretsignature"
	queryParams := map[string]string{
		"api-version": "preview",
		"sig":         sensitive,
	}

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{
		QueryParams: queryParams,
	}, RetryPolicy{})

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	require.Error(t, err, "expected an error from connection-close")

	// The credentialed URL must not survive into the error chain.
	var urlErr *url.Error
	require.NotErrorAs(t, err, &urlErr, "error still wraps *url.Error (URL leaks)")
	rawMessage := err.Error()
	assert.NotContains(t, rawMessage, sensitive, "error leaked the sig QueryParams secret")
	assert.NotContains(t, rawMessage, "api-version=preview", "error still embeds the encoded request URL")
}

// testRetryPolicy returns a RetryPolicy tuned for unit tests: small
// delays keep the test wall-clock short while still exercising the
// helper's sleep + budget paths.
func testRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     2,
		InitialDelay:    time.Millisecond,
		MaxDelay:        5 * time.Millisecond,
		WallClockBudget: 200 * time.Millisecond,
	}
}

// TestOpenAIAdapter_RetriesOnce429ThenSucceeds verifies the end-to-end
// retry path: a single 429 is retried once, the second attempt
// succeeds, and provider_retry_attempt fires once while rate_limited
// does not fire (the final response was a 200).
func TestOpenAIAdapter_RetriesOnce429ThenSucceeds(t *testing.T) {
	body := strings.Join([]string{
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	}, "")

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, testRetryPolicy())
	adapter.Tracer = tracer

	ctx, span := tracer.Start(context.Background(), "test")

	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	require.NoError(t, err, "Stream")
	events := collectEvents(t, ch)
	span.End()

	assert.Equal(t, int32(2), calls.Load(), "server requests")
	require.Len(t, events, 2, "expected text_delta and message_complete, got %+v", events)
	assert.Equal(t, "text_delta", events[0].Type)
	assert.Equal(t, "Hi", events[0].Text)
	assert.Equal(t, "message_complete", events[1].Type)
	assert.Equal(t, "end_turn", events[1].StopReason)

	stubs := exporter.GetSpans()
	require.NotEmpty(t, stubs, "expected at least one finished span")
	var retryAttempts, rateLimited int
	for _, s := range stubs {
		for _, ev := range s.Events {
			switch ev.Name {
			case "provider_retry_attempt":
				retryAttempts++
			case "rate_limited":
				rateLimited++
			}
		}
	}
	assert.Equal(t, 1, retryAttempts, "provider_retry_attempt events")
	assert.Equal(t, 0, rateLimited, "rate_limited events on terminal success")
}

// TestOpenAIAdapter_429ExhaustedSurfacesTerminalError verifies that
// when every attempt returns 429, the adapter exhausts retries and the
// rate_limited span event fires exactly once, on the terminal attempt
// only (kept off intermediate retries to avoid double-counting against
// alerts keyed on it).
func TestOpenAIAdapter_429ExhaustedSurfacesTerminalError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"still rate limited"}}`)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(mp)
	require.NoError(t, err, "NewMetricsForTesting")

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, testRetryPolicy())
	adapter.Tracer = tracer
	adapter.Metrics = metrics

	ctx, span := tracer.Start(context.Background(), "test")

	_, err = adapter.Stream(ctx, types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	span.End()
	require.Error(t, err, "expected terminal error after retry exhaustion")
	// Pin the full prefix so log scrapers that pattern-match on the
	// error string break loudly if the wrap format changes. The message
	// comes straight from the final 429 response, with no transport wrap.
	wantPrefix := "openai API returned status 429: still rate limited"
	assert.True(t, strings.HasPrefix(err.Error(), wantPrefix), "error = %q, want prefix %q", err.Error(), wantPrefix)
	assert.Equal(t, int32(2), calls.Load(), "server requests with MaxAttempts=2")

	stubs := exporter.GetSpans()
	require.NotEmpty(t, stubs, "expected at least one finished span")
	var retryAttempts, rateLimited int
	for _, s := range stubs {
		for _, ev := range s.Events {
			switch ev.Name {
			case "provider_retry_attempt":
				retryAttempts++
			case "rate_limited":
				rateLimited++
			}
		}
	}
	// One intermediate retry event (attempt 1 → 2) and one terminal
	// rate_limited event on the final 429.
	assert.Equal(t, 1, retryAttempts, "provider_retry_attempt events")
	assert.Equal(t, 1, rateLimited, "rate_limited events on terminal 429")
	assert.GreaterOrEqual(t, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), uint64(1), "a terminal 429 must still record provider latency")
}

// TestOpenAIAdapter_429BudgetExhaustedSurfacesTerminalError covers the
// `retryOutcomeBudgetExhausted` branch: exhaustion via WallClockBudget
// rather than MaxAttempts. Uses a deterministic Retry-After-Ms hint
// (rather than backoff) so the delay reliably exceeds the budget
// despite full-jitter randomness.
func TestOpenAIAdapter_429BudgetExhaustedSurfacesTerminalError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After-Ms", "100")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"budget should run out"}}`)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	// Allow up to 10 attempts but cap the wall-clock at 5ms so the budget
	// is the binding constraint. The server's 100ms Retry-After-Ms hint
	// (capped at MaxDelay=100ms) guarantees the first retry's sleep would
	// exceed the budget.
	budgetPolicy := RetryPolicy{
		MaxAttempts:     10,
		InitialDelay:    50 * time.Millisecond,
		MaxDelay:        100 * time.Millisecond,
		WallClockBudget: 5 * time.Millisecond,
	}
	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, budgetPolicy)
	adapter.Tracer = tracer

	ctx, span := tracer.Start(context.Background(), "test")
	_, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	span.End()
	require.Error(t, err, "expected terminal error after budget exhaustion")
	require.ErrorContains(t, err, "openai API returned status 429")

	// Budget exhaustion before any retry sleep means only the first
	// attempt reaches the server.
	assert.Equal(t, int32(1), calls.Load(), "server requests: budget should cut before retry")

	stubs := exporter.GetSpans()
	require.NotEmpty(t, stubs, "expected at least one finished span")
	var rateLimited int
	for _, s := range stubs {
		for _, ev := range s.Events {
			if ev.Name == "rate_limited" {
				rateLimited++
			}
		}
	}
	assert.Equal(t, 1, rateLimited, "rate_limited events on terminal 429 from budget exhaustion")
}

// TestOpenAIAdapter_TerminalNon429WithTracer_SetsStatusCode covers a
// terminal non-429 error response with a tracer set. The span must carry
// the response status in http.status_code, and rate_limited must not fire.
func TestOpenAIAdapter_TerminalNon429WithTracer_SetsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	adapter := NewOpenAICompatibleAdapter(staticBearer("bad-key"), srv.URL, OpenAIAuthConfig{}, testRetryPolicy())
	adapter.Tracer = tracer

	ctx, span := tracer.Start(context.Background(), "test")
	_, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	span.End()
	require.Error(t, err, "expected error for 401")

	stubs := exporter.GetSpans()
	require.NotEmpty(t, stubs, "expected at least one finished span")
	var statusCodes []int64
	var eventNames []string
	for _, s := range stubs {
		for _, kv := range s.Attributes {
			if string(kv.Key) == "http.status_code" {
				statusCodes = append(statusCodes, kv.Value.AsInt64())
			}
		}
		for _, ev := range s.Events {
			eventNames = append(eventNames, ev.Name)
		}
	}
	require.NotEmpty(t, statusCodes, "expected http.status_code attribute on the span")
	for _, code := range statusCodes {
		assert.Equal(t, int64(http.StatusUnauthorized), code, "http.status_code")
	}
	assert.NotContains(t, eventNames, "rate_limited", "rate_limited must not fire on a non-429 terminal status")
}
