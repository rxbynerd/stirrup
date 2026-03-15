package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rubynerd/stirrup/types"
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
