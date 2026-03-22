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

// sseLines builds an SSE stream body from event/data pairs.
func sseLines(events ...string) string {
	return fmt.Sprintf("%s\n", joinLines(events...))
}

func joinLines(lines ...string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func makeSSE(event, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n", event, data)
}

func collectEvents(t *testing.T, ch <-chan types.StreamEvent) []types.StreamEvent {
	t.Helper()
	var events []types.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func TestAnthropicAdapter_StreamTextDelta(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":" world"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("expected x-api-key=test-key, got %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicAPIVersion {
			t.Errorf("expected anthropic-version=%s, got %q", anthropicAPIVersion, got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Expect: text_delta("Hello"), text_delta(" world"), message_complete
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
}

func TestAnthropicAdapter_StreamToolUse(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_123","name":"read_file"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"\"main.go\"}"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
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
	if events[0].ID != "toolu_123" {
		t.Errorf("event[0].ID = %q, want toolu_123", events[0].ID)
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

func TestAnthropicAdapter_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("bad-key")
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
}

func TestAnthropicAdapter_RequestBody(t *testing.T) {
	var received anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "claude-sonnet-4-6",
		System:      "You are helpful.",
		MaxTokens:   4096,
		Temperature: 0.5,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	// Drain the channel.
	for range ch {
	}

	if !received.Stream {
		t.Error("expected stream=true in request body")
	}
	if received.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6", received.Model)
	}
	if received.System != "You are helpful." {
		t.Errorf("system = %q, want 'You are helpful.'", received.System)
	}
	if received.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", received.MaxTokens)
	}
}

func TestSSE_DeltaForUnknownIndex(t *testing.T) {
	// Send a content_block_delta for an index that has no content_block_start.
	// The adapter should skip it silently — no panic, no error event.
	body := joinLines(
		makeSSE("content_block_delta", `{"index":5,"delta":{"type":"text_delta","text":"orphan"}}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Should only get message_complete, no text_delta for the orphan, no error.
	for _, ev := range events {
		if ev.Type == "error" {
			t.Errorf("unexpected error event: %v", ev.Error)
		}
		if ev.Type == "text_delta" {
			t.Errorf("unexpected text_delta event for unknown index: %q", ev.Text)
		}
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (message_complete), got %d: %+v", len(events), events)
	}
	if events[0].Type != "message_complete" {
		t.Errorf("event[0].Type = %q, want message_complete", events[0].Type)
	}
}

func TestSSE_MalformedContentBlockStart(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{not valid json`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
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
		t.Error("expected an error event for malformed content_block_start JSON")
	}
}

func TestSSE_MalformedToolInput(t *testing.T) {
	// Accumulate invalid JSON via input_json_delta, then stop the block.
	// The adapter should emit an error when it tries to unmarshal.
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"read_file"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"INVALID"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
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
			} else if !strings.Contains(ev.Error.Error(), "tool input JSON") {
				t.Errorf("expected error about tool input JSON, got: %v", ev.Error)
			}
		}
	}
	if !foundError {
		t.Error("expected an error event for malformed tool input JSON")
	}
}

func TestSSE_MultipleBlocks(t *testing.T) {
	// Two tool_use blocks at different indices, interleaved.
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_aaa","name":"read_file"}}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"tool_use","id":"toolu_bbb","name":"write_file"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\"}"}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"b.go\",\"content\":\"x\"}"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Expect: tool_call(aaa), tool_call(bbb), message_complete.
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

	// First tool call should be toolu_aaa / read_file.
	if toolCalls[0].ID != "toolu_aaa" {
		t.Errorf("toolCalls[0].ID = %q, want toolu_aaa", toolCalls[0].ID)
	}
	if toolCalls[0].Name != "read_file" {
		t.Errorf("toolCalls[0].Name = %q, want read_file", toolCalls[0].Name)
	}
	if toolCalls[0].Input["path"] != "a.go" {
		t.Errorf("toolCalls[0].Input[path] = %v, want a.go", toolCalls[0].Input["path"])
	}

	// Second tool call should be toolu_bbb / write_file.
	if toolCalls[1].ID != "toolu_bbb" {
		t.Errorf("toolCalls[1].ID = %q, want toolu_bbb", toolCalls[1].ID)
	}
	if toolCalls[1].Name != "write_file" {
		t.Errorf("toolCalls[1].Name = %q, want write_file", toolCalls[1].Name)
	}
	if toolCalls[1].Input["path"] != "b.go" {
		t.Errorf("toolCalls[1].Input[path] = %v, want b.go", toolCalls[1].Input["path"])
	}
}

func TestAnthropicAdapter_ContextCancellation(t *testing.T) {
	// Server that never finishes sending events.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		// Block until the client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter("test-key")
	adapter.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	cancel()

	events := collectEvents(t, ch)
	// Should get an error event from context cancellation.
	if len(events) == 0 {
		t.Fatal("expected at least one event after cancellation")
	}
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Errorf("last event type = %q, want error", last.Type)
	}
}

func TestAnthropicAdapter_HasTimeout(t *testing.T) {
	adapter := NewAnthropicAdapter("test-key")
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
