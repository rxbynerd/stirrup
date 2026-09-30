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

// streamAnthropicSSE serves body as an SSE response and returns every
// StreamEvent the adapter emits for it.
func streamAnthropicSSE(t *testing.T, body string) []types.StreamEvent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-5-5",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	return collectEvents(t, ch)
}

// TestAnthropicAdapter_StreamThinkingBlock pins capture of a thinking block
// ahead of the text and tool call it precedes: text and signature arrive as
// deltas and are emitted as one event when the block stops.
func TestAnthropicAdapter_StreamThinkingBlock(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"check."}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"EqQBCkYIBRgC"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"Reading."}}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("content_block_start", `{"index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{},"caller":{"type":"direct"}}}`),
		makeSSE("content_block_delta", `{"index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"main.go\"}"}}`),
		makeSSE("content_block_stop", `{"index":2}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	wantTypes := []string{"thinking", "text_delta", "tool_call", "message_complete"}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, want)
		}
	}
	if events[0].Text != "Let me check." {
		t.Errorf("thinking text = %q, want %q", events[0].Text, "Let me check.")
	}
	if events[0].ThoughtSignature != "EqQBCkYIBRgC" {
		t.Errorf("thinking signature = %q, want %q", events[0].ThoughtSignature, "EqQBCkYIBRgC")
	}
	if events[2].ThoughtSignature != "" {
		t.Errorf("tool_call carried a signature %q; only thinking events may", events[2].ThoughtSignature)
	}
}

// TestAnthropicAdapter_StreamThinkingOmittedText covers the default display
// on 5.x models: no thinking_delta arrives and only the signature carries
// the content.
func TestAnthropicAdapter_StreamThinkingOmittedText(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"sig-omitted"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if got := events[0]; got.Type != "thinking" || got.Text != "" || got.ThoughtSignature != "sig-omitted" {
		t.Errorf("event[0] = %+v, want thinking with empty text and signature sig-omitted", got)
	}
}

// TestAnthropicAdapter_StreamRedactedThinking pins that a redacted block's
// data, which arrives whole on content_block_start, is emitted verbatim.
func TestAnthropicAdapter_StreamRedactedThinking(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"redacted_thinking","data":"EmwKAhgBEgy3va"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"Done."}}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if got := events[0]; got.Type != "redacted_thinking" || got.ThoughtSignature != "EmwKAhgBEgy3va" || got.Text != "" {
		t.Errorf("event[0] = %+v, want redacted_thinking carrying the data", got)
	}
}

// TestAnthropicAdapter_ThinkingBlockSizeCap mirrors the tool-input cap: a
// thinking block that grows past maxThinkingBlockSize ends the stream with
// an error rather than accumulating without bound.
func TestAnthropicAdapter_ThinkingBlockSizeCap(t *testing.T) {
	huge, err := json.Marshal(strings.Repeat("a", maxThinkingBlockSize+1))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", fmt.Sprintf(`{"index":0,"delta":{"type":"thinking_delta","thinking":%s}}`, huge)),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	gotTypes := make([]string, len(events))
	for i, ev := range events {
		gotTypes[i] = ev.Type
	}
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("event types = %v, want exactly one error event", gotTypes)
	}
	if !strings.Contains(events[0].Error.Error(), "thinking block exceeds") {
		t.Errorf("error = %v, want thinking size-cap error", events[0].Error)
	}
}
