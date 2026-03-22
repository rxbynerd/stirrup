package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("expected Authorization=Bearer test-key, got %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("expected path ending in /chat/completions, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Expect: text_delta("Hello"), text_delta(" world"), message_complete.
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}

	if events[0].Type != "text_delta" || events[0].Text != "Hello" {
		t.Errorf("event[0] = %+v, want text_delta/Hello", events[0])
	}
	if events[1].Type != "text_delta" || events[1].Text != " world" {
		t.Errorf("event[1] = %+v, want text_delta/ world", events[1])
	}
	if events[2].Type != "message_complete" || events[2].StopReason != "end_turn" {
		t.Errorf("event[2] = %+v, want message_complete/end_turn", events[2])
	}
	if events[2].OutputTokens != 5 {
		t.Errorf("event[2].OutputTokens = %d, want 5", events[2].OutputTokens)
	}
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
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}

	if events[0].Type != "tool_call" {
		t.Fatalf("event[0].Type = %q, want tool_call", events[0].Type)
	}
	if events[0].ID != "call_abc" {
		t.Errorf("event[0].ID = %q, want call_abc", events[0].ID)
	}
	if events[0].Name != "read_file" {
		t.Errorf("event[0].Name = %q, want read_file", events[0].Name)
	}
	if events[0].Input["path"] != "main.go" {
		t.Errorf("event[0].Input[path] = %v, want main.go", events[0].Input["path"])
	}

	if events[1].Type != "message_complete" || events[1].StopReason != "tool_use" {
		t.Errorf("event[1] = %+v, want message_complete/tool_use", events[1])
	}
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
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	var toolCalls []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "error" {
			t.Fatalf("unexpected error event: %v", ev.Error)
		}
		if ev.Type == "tool_call" {
			toolCalls = append(toolCalls, ev)
		}
	}

	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool_call events, got %d", len(toolCalls))
	}

	if toolCalls[0].ID != "call_aaa" || toolCalls[0].Name != "read_file" {
		t.Errorf("toolCalls[0] = %+v, want call_aaa/read_file", toolCalls[0])
	}
	if toolCalls[0].Input["path"] != "a.go" {
		t.Errorf("toolCalls[0].Input[path] = %v, want a.go", toolCalls[0].Input["path"])
	}

	if toolCalls[1].ID != "call_bbb" || toolCalls[1].Name != "write_file" {
		t.Errorf("toolCalls[1] = %+v, want call_bbb/write_file", toolCalls[1])
	}
	if toolCalls[1].Input["path"] != "b.go" {
		t.Errorf("toolCalls[1].Input[path] = %v, want b.go", toolCalls[1].Input["path"])
	}
}

func TestOpenAIAdapter_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("bad-key", srv.URL)

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("expected error to contain 'Invalid API key', got: %v", err)
	}
}

func TestOpenAIAdapter_HTTPErrorNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("key", srv.URL)

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to contain '500', got: %v", err)
	}
}

func TestOpenAIAdapter_RequestBody(t *testing.T) {
	var received openaiRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

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
		Temperature: 0.5,
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
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch {
	}

	if !received.Stream {
		t.Error("expected stream=true in request body")
	}
	if received.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", received.Model)
	}
	if received.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", received.MaxTokens)
	}
	if received.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", received.Temperature)
	}

	// System message should be first.
	if len(received.Messages) < 2 {
		t.Fatalf("expected >= 2 messages, got %d", len(received.Messages))
	}
	if received.Messages[0].Role != "system" {
		t.Errorf("messages[0].Role = %q, want system", received.Messages[0].Role)
	}
	if received.Messages[1].Role != "user" {
		t.Errorf("messages[1].Role = %q, want user", received.Messages[1].Role)
	}

	// Tools should be in OpenAI function format.
	if len(received.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(received.Tools))
	}
	if received.Tools[0].Type != "function" {
		t.Errorf("tools[0].Type = %q, want function", received.Tools[0].Type)
	}
	if received.Tools[0].Function.Name != "read_file" {
		t.Errorf("tools[0].Function.Name = %q, want read_file", received.Tools[0].Function.Name)
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

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	cancel()

	events := collectEvents(t, ch)
	if len(events) == 0 {
		t.Fatal("expected at least one event after cancellation")
	}
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Errorf("last event type = %q, want error", last.Type)
	}
}

func TestOpenAIAdapter_MalformedChunk(t *testing.T) {
	body := strings.Join([]string{
		"data: {not valid json}\n\n",
		"data: [DONE]\n\n",
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	foundError := false
	for _, ev := range events {
		if ev.Type == "error" {
			foundError = true
			if ev.Error == nil {
				t.Error("error event has nil Error field")
			}
		}
	}
	if !foundError {
		t.Error("expected an error event for malformed JSON chunk")
	}
}

func TestOpenAIAdapter_MalformedToolArguments(t *testing.T) {
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
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	foundError := false
	for _, ev := range events {
		if ev.Type == "error" {
			foundError = true
			if ev.Error == nil {
				t.Error("error event has nil Error field")
			} else if !strings.Contains(ev.Error.Error(), "tool arguments JSON") {
				t.Errorf("expected error about tool arguments JSON, got: %v", ev.Error)
			}
		}
	}
	if !foundError {
		t.Error("expected an error event for malformed tool arguments JSON")
	}
}

func TestOpenAIAdapter_DefaultBaseURL(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter("key", "")
	if adapter.baseURL != openaiDefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", adapter.baseURL, openaiDefaultBaseURL)
	}
}

func TestOpenAIAdapter_TrailingSlashBaseURL(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter("key", "https://example.com/v1/")
	if adapter.baseURL != "https://example.com/v1" {
		t.Errorf("baseURL = %q, want https://example.com/v1", adapter.baseURL)
	}
}

func TestOpenAIAdapter_NoAPIKey(t *testing.T) {
	// Some local endpoints (Ollama, vLLM) don't require an API key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "llama3",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch {
	}
}

func TestTranslateMessages(t *testing.T) {
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

	result := translateMessages("System prompt", messages)

	// system + user + assistant + tool = 4 messages.
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d: %+v", len(result), result)
	}

	if result[0].Role != "system" {
		t.Errorf("result[0].Role = %q, want system", result[0].Role)
	}
	if result[1].Role != "user" {
		t.Errorf("result[1].Role = %q, want user", result[1].Role)
	}
	if result[2].Role != "assistant" {
		t.Errorf("result[2].Role = %q, want assistant", result[2].Role)
	}
	if len(result[2].ToolCalls) != 1 {
		t.Fatalf("result[2].ToolCalls length = %d, want 1", len(result[2].ToolCalls))
	}
	if result[2].ToolCalls[0].ID != "call_1" {
		t.Errorf("result[2].ToolCalls[0].ID = %q, want call_1", result[2].ToolCalls[0].ID)
	}
	if result[2].ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("result[2].ToolCalls[0].Function.Name = %q, want read_file", result[2].ToolCalls[0].Function.Name)
	}
	if result[3].Role != "tool" {
		t.Errorf("result[3].Role = %q, want tool", result[3].Role)
	}
	if result[3].ToolCallID != "call_1" {
		t.Errorf("result[3].ToolCallID = %q, want call_1", result[3].ToolCallID)
	}
}

func TestTranslateMessages_ErrorToolResult(t *testing.T) {
	messages := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Content: "file not found", IsError: true},
			},
		},
	}

	result := translateMessages("", messages)

	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Role != "tool" {
		t.Errorf("result[0].Role = %q, want tool", result[0].Role)
	}
	content, ok := result[0].Content.(string)
	if !ok {
		t.Fatalf("result[0].Content is not a string: %T", result[0].Content)
	}
	if !strings.HasPrefix(content, "Error: ") {
		t.Errorf("expected error-prefixed content, got %q", content)
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "content_filter"},
	}
	for _, tt := range tests {
		got := mapFinishReason(tt.input)
		if got != tt.want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
		}
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
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// text_delta, tool_call, message_complete.
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" {
		t.Errorf("event[0].Type = %q, want text_delta", events[0].Type)
	}
	if events[1].Type != "tool_call" {
		t.Errorf("event[1].Type = %q, want tool_call", events[1].Type)
	}
	if events[2].Type != "message_complete" {
		t.Errorf("event[2].Type = %q, want message_complete", events[2].Type)
	}
}

func TestOpenAIAdapter_HasTimeout(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter("test-key", "")
	if adapter.httpClient.Timeout == 0 {
		t.Error("HTTP client should have a non-zero timeout")
	}
	tr, ok := adapter.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("TLSHandshakeTimeout should be non-zero")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("ResponseHeaderTimeout should be non-zero")
	}
}
