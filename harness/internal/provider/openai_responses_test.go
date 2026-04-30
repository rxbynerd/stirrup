package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// makeResponsesEvent builds a named SSE record terminated by a blank line —
// the wire shape the Responses API streams.
func makeResponsesEvent(name, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", name, data)
}

func TestOpenAIResponsesAdapter_StreamTextDelta(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.created", `{"response":{"id":"resp_1","status":"in_progress"}}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"Hello"}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":" world"}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1"}],"usage":{"input_tokens":10,"output_tokens":5}}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("expected Authorization=Bearer test-key, got %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("expected path ending in /responses, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

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

func TestOpenAIResponsesAdapter_StreamToolCall(t *testing.T) {
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
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

func TestOpenAIResponsesAdapter_MultipleToolCalls(t *testing.T) {
	// Two function calls in flight concurrently. Their argument-deltas
	// arrive interleaved (bbb then aaa) but the .done events fire in
	// output_index order, which is OpenAI's natural emission contract.
	// The adapter must emit each tool_call at .done (no later, so the
	// agentic loop can dispatch tools as soon as they're complete) AND
	// preserve the output_index ordering across the resulting events.
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_aaa","call_id":"call_aaa","name":"read_file"}}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":1,"item":{"type":"function_call","id":"fc_bbb","call_id":"call_bbb","name":"write_file"}}`),
		// Interleaved deltas: server is still composing both calls.
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_bbb","output_index":1,"delta":"{\"path\":\"b.go\","}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_aaa","output_index":0,"delta":"{\"path\":"}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_bbb","output_index":1,"delta":"\"content\":\"x\"}"}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_aaa","output_index":0,"delta":"\"a.go\"}"}`),
		// .done events fire in output_index order (the realistic OpenAI
		// contract) so per-call emission already produces deterministic
		// ordering.
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
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
	// Sort by output_index ascending: a then b.
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

func TestOpenAIResponsesAdapter_DeferredFlushOrderedByOutputIndex(t *testing.T) {
	// A defensive case: if no .done events fire for two concurrent
	// function calls (e.g. server abbreviates the stream), the adapter
	// must still flush them on response.completed in deterministic
	// output_index ascending order. State map iteration in Go is
	// randomised, so the explicit sort matters.
	body := strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":1,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"write_file"}}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read_file"}}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_b","output_index":1,"delta":"{\"path\":\"b\"}"}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_a","output_index":0,"delta":"{\"path\":\"a\"}"}`),
		// Note: no .done events. Calls are pending when completion arrives.
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read_file"},{"type":"function_call","id":"fc_b","call_id":"call_b","name":"write_file"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	for i := 0; i < 5; i++ { // run a few times because map iteration is randomised
		ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
		if err != nil {
			t.Fatalf("Stream() error: %v", err)
		}
		events := collectEvents(t, ch)

		var calls []types.StreamEvent
		for _, ev := range events {
			if ev.Type == "tool_call" {
				calls = append(calls, ev)
			}
		}
		if len(calls) != 2 {
			t.Fatalf("iter %d: expected 2 tool_calls, got %d", i, len(calls))
		}
		if calls[0].ID != "call_a" || calls[1].ID != "call_b" {
			t.Errorf("iter %d: order = [%s, %s], want [call_a, call_b]", i, calls[0].ID, calls[1].ID)
		}
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4.1",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" {
		t.Errorf("event[0].Type = %q, want text_delta", events[0].Type)
	}
	if events[1].Type != "tool_call" {
		t.Errorf("event[1].Type = %q, want tool_call", events[1].Type)
	}
	if events[2].Type != "message_complete" || events[2].StopReason != "tool_use" {
		t.Errorf("event[2] = %+v, want message_complete/tool_use", events[2])
	}
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	last := events[len(events)-1]
	if last.Type != "message_complete" || last.StopReason != "max_tokens" {
		t.Errorf("last event = %+v, want message_complete/max_tokens", last)
	}
	if last.OutputTokens != 1 {
		t.Errorf("OutputTokens = %d, want 1", last.OutputTokens)
	}
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	last := events[len(events)-1]
	if last.Type != "error" {
		t.Errorf("last event type = %q, want error", last.Type)
	}
	if last.Error == nil || !strings.Contains(last.Error.Error(), "server overloaded") {
		t.Errorf("expected error to mention server overloaded, got: %v", last.Error)
	}
	// No message_complete should be emitted on a failed response.
	for _, ev := range events {
		if ev.Type == "message_complete" {
			t.Errorf("unexpected message_complete on failed response: %+v", ev)
		}
	}
}

func TestOpenAIResponsesAdapter_ErrorEvent(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("error", `{"message":"upstream timeout","type":"timeout","code":"E_TIMEOUT"}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != "error" {
		t.Errorf("event[0].Type = %q, want error", events[0].Type)
	}
	if events[0].Error == nil || !strings.Contains(events[0].Error.Error(), "upstream timeout") {
		t.Errorf("expected error to mention upstream timeout, got: %v", events[0].Error)
	}
}

func TestOpenAIResponsesAdapter_MalformedToolArguments(t *testing.T) {
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
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
				t.Errorf("expected tool arguments JSON error, got: %v", ev.Error)
			}
		}
	}
	if !foundError {
		t.Error("expected an error event for malformed tool arguments JSON")
	}
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	foundOversize := false
	for _, ev := range events {
		if ev.Type == "error" && ev.Error != nil && strings.Contains(ev.Error.Error(), "byte limit") {
			foundOversize = true
		}
		if ev.Type == "tool_call" {
			t.Errorf("tool_call should not be emitted when args exceed cap: %+v", ev)
		}
	}
	if !foundOversize {
		t.Error("expected an error event citing the byte limit")
	}
}

func TestOpenAIResponsesAdapter_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("bad-key", srv.URL)

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("expected error to contain 'Invalid API key', got: %v", err)
	}
}

func TestOpenAIResponsesAdapter_HTTPErrorLargeBody(t *testing.T) {
	largeMsg := strings.Repeat("x", 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"%s","type":"invalid_request_error"}}`, largeMsg)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("key", srv.URL)

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected error for 400 response, got nil")
	}
	// The 4096-byte LimitReader truncates the JSON before it can decode, so
	// we fall back to the generic status-code error. The key assertion is
	// that the call returns cleanly without consuming unbounded memory.
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected status code in error, got: %v", err)
	}
}

func TestOpenAIResponsesAdapter_RequestBody(t *testing.T) {
	var received responsesRequest
	var rawBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		rawBody = buf[:n]
		if err := json.Unmarshal(rawBody, &received); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("expected /responses path, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)

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
		Temperature: 0.5,
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
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch {
	}

	if !received.Stream {
		t.Error("expected stream=true")
	}
	if received.Store {
		t.Error("expected store=false in request body")
	}
	if received.Instructions != "You are helpful." {
		t.Errorf("instructions = %q, want 'You are helpful.'", received.Instructions)
	}
	if received.Model != "gpt-4.1" {
		t.Errorf("model = %q, want gpt-4.1", received.Model)
	}
	if received.MaxOutputTokens != 4096 {
		t.Errorf("max_output_tokens = %d, want 4096", received.MaxOutputTokens)
	}
	if received.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", received.Temperature)
	}

	// Raw body must NOT contain a Chat Completions-style top-level "messages"
	// field, and must NOT contain a Chat Completions "max_tokens" key.
	if strings.Contains(string(rawBody), `"messages"`) {
		t.Error("request body contains 'messages' field — Responses API uses 'input'")
	}
	if strings.Contains(string(rawBody), `"max_tokens"`) {
		t.Error("request body contains 'max_tokens' — Responses API uses 'max_output_tokens'")
	}
	if !strings.Contains(string(rawBody), `"store":false`) {
		t.Error("request body must contain explicit store:false")
	}

	// Input shape: user message, assistant message + function_call, function_call_output.
	if len(received.Input) < 4 {
		t.Fatalf("expected >=4 input items, got %d: %+v", len(received.Input), received.Input)
	}
	if received.Input[0].Type != "message" || received.Input[0].Role != "user" {
		t.Errorf("input[0] = %+v, want message/user", received.Input[0])
	}
	if len(received.Input[0].Content) == 0 || received.Input[0].Content[0].Type != "input_text" {
		t.Errorf("input[0].content[0].type = %+v, want input_text", received.Input[0].Content)
	}
	if received.Input[1].Type != "message" || received.Input[1].Role != "assistant" {
		t.Errorf("input[1] = %+v, want message/assistant", received.Input[1])
	}
	if len(received.Input[1].Content) == 0 || received.Input[1].Content[0].Type != "output_text" {
		t.Errorf("input[1].content[0].type = %+v, want output_text", received.Input[1].Content)
	}
	if received.Input[2].Type != "function_call" {
		t.Errorf("input[2].Type = %q, want function_call", received.Input[2].Type)
	}
	if received.Input[2].CallID != "call_1" {
		t.Errorf("input[2].CallID = %q, want call_1", received.Input[2].CallID)
	}
	if received.Input[2].Name != "read_file" {
		t.Errorf("input[2].Name = %q, want read_file", received.Input[2].Name)
	}
	if received.Input[3].Type != "function_call_output" {
		t.Errorf("input[3].Type = %q, want function_call_output", received.Input[3].Type)
	}
	if received.Input[3].CallID != "call_1" {
		t.Errorf("input[3].CallID = %q, want call_1", received.Input[3].CallID)
	}

	// Tools should use the flatter shape (no nested "function" object).
	if len(received.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(received.Tools))
	}
	if received.Tools[0].Type != "function" {
		t.Errorf("tools[0].Type = %q, want function", received.Tools[0].Type)
	}
	if received.Tools[0].Name != "read_file" {
		t.Errorf("tools[0].Name = %q, want read_file", received.Tools[0].Name)
	}
	if received.Tools[0].Description != "Read a file" {
		t.Errorf("tools[0].Description = %q, want 'Read a file'", received.Tools[0].Description)
	}
	if strings.Contains(string(rawBody), `"function":{"name"`) {
		t.Error("request body contains nested 'function' object — Responses API uses a flat tool shape")
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	cancel()

	// After cancellation, the channel must close in bounded time. We
	// don't strictly require an error event: the new emit-with-cancel
	// path may discard the trailing error if the consumer was already
	// gone. The contract is that the goroutine must not leak; the
	// closed channel below proves it.
	events := collectEvents(t, ch)
	for _, ev := range events {
		if ev.Type == "message_complete" {
			t.Errorf("unexpected message_complete after cancel: %+v", ev)
		}
	}
}

func TestOpenAIResponsesAdapter_DefaultBaseURL(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("key", "")
	if adapter.baseURL != openaiDefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", adapter.baseURL, openaiDefaultBaseURL)
	}
}

func TestOpenAIResponsesAdapter_TrailingSlashBaseURL(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("key", "https://example.com/v1/")
	if adapter.baseURL != "https://example.com/v1" {
		t.Errorf("baseURL = %q, want https://example.com/v1", adapter.baseURL)
	}
}

func TestOpenAIResponsesAdapter_HasTimeout(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("test-key", "")
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
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	adapter.Metrics = metrics

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"); got != 1 {
		t.Errorf("provider_latency count = %d, want 1", got)
	}
	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"); got != 1 {
		t.Errorf("provider_ttfb count = %d, want 1", got)
	}
	h, ok := providerHistogramFinder(t, reader, "stirrup.harness.provider_latency")
	if !ok || len(h.DataPoints) == 0 {
		t.Fatal("expected provider_latency data point")
	}
	attrs := h.DataPoints[0].Attributes
	if v, ok := attrs.Value("provider.type"); !ok || v.AsString() != "openai-responses" {
		t.Errorf("provider.type = %v ok=%v, want openai-responses", v.AsString(), ok)
	}
	if v, ok := attrs.Value("provider.model"); !ok || v.AsString() != "gpt-4.1" {
		t.Errorf("provider.model = %v ok=%v, want gpt-4.1", v.AsString(), ok)
	}
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
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	adapter := NewOpenAIResponsesAdapter("bad-key", srv.URL)
	adapter.Metrics = metrics

	if _, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024}); err == nil {
		t.Fatal("expected error")
	}
	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"); got != 1 {
		t.Errorf("provider_latency count = %d, want 1", got)
	}
	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"); got != 0 {
		t.Errorf("provider_ttfb count = %d, want 0", got)
	}
}

// --- Translation unit tests ---

func TestTranslateMessagesResponses(t *testing.T) {
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

	result := translateMessagesResponses(messages)

	// user message -> 1 item (input_text)
	// assistant text + tool_use -> 1 message item + 1 function_call item
	// tool_result -> 1 function_call_output item
	if len(result) != 4 {
		t.Fatalf("expected 4 input items, got %d: %+v", len(result), result)
	}
	if result[0].Type != "message" || result[0].Role != "user" {
		t.Errorf("result[0] = %+v, want message/user", result[0])
	}
	if result[0].Content[0].Type != "input_text" {
		t.Errorf("result[0].Content[0].Type = %q, want input_text", result[0].Content[0].Type)
	}
	if result[1].Type != "message" || result[1].Role != "assistant" {
		t.Errorf("result[1] = %+v, want message/assistant", result[1])
	}
	if result[1].Content[0].Type != "output_text" {
		t.Errorf("result[1].Content[0].Type = %q, want output_text", result[1].Content[0].Type)
	}
	if result[2].Type != "function_call" || result[2].CallID != "call_1" || result[2].Name != "read_file" {
		t.Errorf("result[2] = %+v, want function_call(call_1,read_file)", result[2])
	}
	if result[3].Type != "function_call_output" || result[3].CallID != "call_1" || result[3].Output != "package main" {
		t.Errorf("result[3] = %+v, want function_call_output(call_1,package main)", result[3])
	}
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

	result := translateMessagesResponses(messages)
	if len(result) != 1 {
		t.Fatalf("expected 1 item, got %d", len(result))
	}
	if result[0].Type != "function_call_output" {
		t.Errorf("type = %q, want function_call_output", result[0].Type)
	}
	if !strings.HasPrefix(result[0].Output, "Error: ") {
		t.Errorf("expected error-prefixed output, got %q", result[0].Output)
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
	result := translateMessagesResponses(messages)
	if len(result) != 1 || result[0].Type != "function_call" {
		t.Fatalf("expected one function_call item, got %+v", result)
	}
	if result[0].Arguments != "{}" {
		t.Errorf("Arguments = %q, want \"{}\"", result[0].Arguments)
	}
}

func TestTranslateToolsResponses(t *testing.T) {
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
	result := translateToolsResponses(tools)
	if len(result) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result))
	}
	for i, tool := range result {
		if tool.Type != "function" {
			t.Errorf("tools[%d].Type = %q, want function", i, tool.Type)
		}
		if tool.Name == "" {
			t.Errorf("tools[%d].Name is empty", i)
		}
	}
	if result[0].Name != "read_file" || result[1].Name != "write_file" {
		t.Errorf("tool names = [%q, %q], want [read_file, write_file]", result[0].Name, result[1].Name)
	}
}

func TestTranslateToolsResponses_Empty(t *testing.T) {
	if got := translateToolsResponses(nil); got != nil {
		t.Errorf("translateToolsResponses(nil) = %+v, want nil", got)
	}
	if got := translateToolsResponses([]types.ToolDefinition{}); got != nil {
		t.Errorf("translateToolsResponses(empty) = %+v, want nil", got)
	}
}

func TestDeriveStopReason(t *testing.T) {
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
			if got := deriveStopReason(tt.in); got != tt.want {
				t.Errorf("deriveStopReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenAIResponsesAdapter_NoAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`))
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("", srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "local", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch {
	}
}

// --- B1: backpressure / cancellation ---

// TestOpenAIResponsesAdapter_BackpressureCancellation verifies that the
// streaming goroutine does not leak when the consumer cancels context
// while the producer is still trying to send. Pre-fix, emitEvent did an
// unconditional send; with the channel buffer full, the goroutine would
// block on the send forever because nothing else was draining the
// channel.
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	// Drain a handful of events so the goroutine starts producing, then
	// stop reading and cancel. The goroutine must observe ctx.Done()
	// instead of blocking on the channel.
	for i := 0; i < 4; i++ {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out reading initial events")
		}
	}
	cancel()

	// The channel must close in bounded time. If emitEvent still did an
	// unconditional send, the consumer-cancelled goroutine would never
	// reach the close(ch) defer and this would deadlock.
	closed := make(chan struct{})
	go func() {
		for range ch {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("channel did not close within 3s after cancel — goroutine likely leaked")
	}
}

// --- B2: SSE record size cap ---

// TestOpenAIResponsesAdapter_SSERecordSizeCapEnforced verifies that when
// a single SSE record's accumulated `data:` lines exceed the input cap,
// the adapter emits a bounded error event and tears down the stream
// without materialising the full concatenated payload in memory.
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != "error" {
		t.Errorf("event[0].Type = %q, want error", events[0].Type)
	}
	if events[0].Error == nil || !strings.Contains(events[0].Error.Error(), "byte limit") {
		t.Errorf("expected error citing byte limit, got: %v", events[0].Error)
	}
}

// --- H1: latency recorded across context cancellation ---

// TestOpenAIResponsesAdapter_LatencyRecordedAfterContextCancel verifies
// that provider_latency is recorded even when the caller cancels their
// context after Stream() returns. Pre-fix, the deferred recordLatency
// call ran on the (now-cancelled) caller context, and OTel exporters
// that drop measurements on cancelled contexts would silently lose the
// measurement.
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
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	adapter.Metrics = metrics

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Cancel BEFORE draining the channel — exposes the case where the
	// caller's ctx is dead by the time the goroutine reaches the
	// deferred recordLatency call.
	cancel()
	for range ch {
	}

	// Give the goroutine a moment to record latency.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"); got == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("provider_latency count = %d, want 1 (latency must be recorded against background ctx)",
		providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"))
}

// --- H2: malformed terminal event emits error ---

// TestOpenAIResponsesAdapter_MalformedCompletedEvent verifies that a
// malformed JSON payload on response.completed surfaces as an error
// event rather than silently dropping the terminal message_complete.
func TestOpenAIResponsesAdapter_MalformedCompletedEvent(t *testing.T) {
	body := makeResponsesEvent("response.completed", `NOT_VALID_JSON`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != "error" {
		t.Errorf("event[0].Type = %q, want error", events[0].Type)
	}
	if events[0].Error == nil || !strings.Contains(events[0].Error.Error(), "parse response.completed") {
		t.Errorf("expected error mentioning parse response.completed, got: %v", events[0].Error)
	}
	for _, ev := range events {
		if ev.Type == "message_complete" {
			t.Errorf("message_complete must not be emitted on malformed completed: %+v", ev)
		}
	}
}

// TestOpenAIResponsesAdapter_MalformedDispatchEvents covers parse-error
// branches across every dispatch case that captures the unmarshal
// error. Each variant must emit an error event and terminate the stream.
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
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := makeResponsesEvent(tc.eventName, "{NOT JSON")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, body)
			}))
			defer srv.Close()

			adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
			ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			events := collectEvents(t, ch)
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
			}
			if events[0].Type != "error" {
				t.Errorf("event[0].Type = %q, want error", events[0].Type)
			}
			if events[0].Error == nil || !strings.Contains(events[0].Error.Error(), tc.errSubstr) {
				t.Errorf("expected error mentioning %q, got: %v", tc.errSubstr, events[0].Error)
			}
		})
	}
}

// --- H3: callKey fallback ---

// TestCallKey_FallbackToIndex verifies the idx: fallback when item_id
// is empty (some partner gateways omit the ID).
func TestCallKey_FallbackToIndex(t *testing.T) {
	if got := callKey("", 3); got != "idx:3" {
		t.Errorf("callKey(\"\", 3) = %q, want idx:3", got)
	}
	if got := callKey("real_id", 3); got != "real_id" {
		t.Errorf("callKey(real_id, 3) = %q, want real_id", got)
	}
}

// TestOpenAIResponsesAdapter_DeltaBeforeAdded verifies the lazy state
// creation when an arguments delta arrives before its corresponding
// output_item.added (defensive for partner gateways that abbreviate the
// stream). The adapter must accumulate the delta and still emit the
// tool_call when the .done event fires.
func TestOpenAIResponsesAdapter_DeltaBeforeAdded(t *testing.T) {
	body := strings.Join([]string{
		// No output_item.added — straight to delta. The state should be
		// created on first delta.
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_lazy","output_index":0,"delta":"{\"k\":1}"}`),
		// done event echoes the call metadata so we have a callID/name.
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"function_call","id":"fc_lazy","call_id":"call_lazy","name":"noop"}}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"function_call","id":"fc_lazy","call_id":"call_lazy","name":"noop"}]}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	var toolCall *types.StreamEvent
	for i := range events {
		if events[i].Type == "tool_call" {
			toolCall = &events[i]
			break
		}
	}
	if toolCall == nil {
		t.Fatalf("expected a tool_call event, got: %+v", events)
	}
	if toolCall.ID != "call_lazy" || toolCall.Name != "noop" {
		t.Errorf("toolCall = %+v, want call_lazy/noop", toolCall)
	}
	if v, _ := toolCall.Input["k"].(float64); v != 1 {
		t.Errorf("toolCall.Input[k] = %v, want 1", toolCall.Input["k"])
	}
}

// TestOpenAIResponsesAdapter_DeltaBeforeAddedNoItemIDFallback exercises
// the callKey fallback path inline in dispatchEvent: the delta arrives
// without an item_id, so the call must be keyed on idx:N and still
// produce a tool_call when .done arrives (with item_id, but matching
// the same output_index — the .done path also falls back).
func TestOpenAIResponsesAdapter_DeltaBeforeAddedNoItemIDFallback(t *testing.T) {
	body := strings.Join([]string{
		makeResponsesEvent("response.function_call_arguments.delta", `{"output_index":2,"delta":"{\"a\":\"b\"}"}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"output_index":2,"arguments":"{\"a\":\"b\"}"}`),
		// Note: no item_id, no echoed call_id/name. The .done event
		// state still flushes a tool_call with empty ID/Name. We assert
		// that no error event fires and the args parse cleanly.
		makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
	}, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	var sawToolCall bool
	for _, ev := range events {
		if ev.Type == "error" {
			t.Errorf("unexpected error event: %v", ev.Error)
		}
		if ev.Type == "tool_call" {
			sawToolCall = true
			if v, _ := ev.Input["a"].(string); v != "b" {
				t.Errorf("Input[a] = %v, want b", ev.Input["a"])
			}
		}
	}
	if !sawToolCall {
		t.Errorf("expected tool_call event, got: %+v", events)
	}
}

// TestDeriveStopReason_DefaultUnknownStatus exercises the default branch
// of deriveStopReason — an unknown Responses status (e.g. cancelled)
// passes through verbatim so the agentic loop can surface it as the
// run outcome.
func TestDeriveStopReason_DefaultUnknownStatus(t *testing.T) {
	got := deriveStopReason(responsesResponse{Status: "cancelled"})
	if got != "cancelled" {
		t.Errorf("deriveStopReason(cancelled) = %q, want cancelled", got)
	}
	// Default branch with no status and a function_call → tool_use.
	got = deriveStopReason(responsesResponse{Status: "", Output: []responsesOutputItem{{Type: "function_call"}}})
	if got != "tool_use" {
		t.Errorf("deriveStopReason(empty,function_call) = %q, want tool_use", got)
	}
	// Default branch with no status and only message → end_turn.
	got = deriveStopReason(responsesResponse{Status: ""})
	if got != "end_turn" {
		t.Errorf("deriveStopReason(empty) = %q, want end_turn", got)
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
	if got := deriveStopReason(in); got != "max_tokens" {
		t.Errorf("deriveStopReason(max_tokens spelling) = %q, want max_tokens", got)
	}
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)
	if len(events) == 0 {
		t.Fatalf("expected at least 1 event")
	}
	last := events[len(events)-1]
	if last.Type != "message_complete" || last.StopReason != "max_tokens" {
		t.Errorf("last = %+v, want message_complete/max_tokens", last)
	}
}

// TestOpenAIResponsesAdapter_SSEParserDefensiveBranches exercises the
// SSE parser's tolerant branches: comment lines (`:` prefix), header
// lines without trailing space (`event:`/`data:`), and trailing record
// flushed at EOF without a terminating blank line.
func TestOpenAIResponsesAdapter_SSEParserDefensiveBranches(t *testing.T) {
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	if len(events) != 2 {
		t.Fatalf("expected 2 events (text_delta, message_complete), got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" || events[0].Text != "hi" {
		t.Errorf("event[0] = %+v, want text_delta/hi", events[0])
	}
	if events[1].Type != "message_complete" || events[1].StopReason != "end_turn" {
		t.Errorf("event[1] = %+v, want message_complete/end_turn", events[1])
	}
}

// TestOpenAIResponsesAdapter_NetworkError exercises the httpClient.Do
// connection-level failure path (server closed before request).
func TestOpenAIResponsesAdapter_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // close before any request fires.

	adapter := NewOpenAIResponsesAdapter("test-key", addr)
	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
	if !strings.Contains(err.Error(), "execute request") {
		t.Errorf("expected execute-request error, got: %v", err)
	}
}

// TestOpenAIResponsesAdapter_5xxNoBody exercises the 5xx fallback path
// where the body is absent or unparseable: the adapter returns the
// generic status-code error rather than crashing.
func TestOpenAIResponsesAdapter_5xxNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected error for 503, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected status code in error, got: %v", err)
	}
}

// TestOpenAIResponsesAdapter_429RetryAfterSpanEvent verifies the 429
// rate-limit branch attaches a span event with the Retry-After header.
func TestOpenAIResponsesAdapter_429RetryAfterSpanEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	adapter.Tracer = tracer

	// We need the span to be in ctx so SpanFromContext finds it.
	ctx, span := tracer.Start(context.Background(), "test")

	_, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
	span.End()

	stubs := exporter.GetSpans()
	if len(stubs) == 0 {
		t.Fatal("expected at least one finished span")
	}
	var found bool
	for _, s := range stubs {
		for _, ev := range s.Events {
			if ev.Name == "rate_limited" {
				found = true
				var retryAfter string
				for _, attr := range ev.Attributes {
					if string(attr.Key) == "retry_after" {
						retryAfter = attr.Value.AsString()
					}
				}
				if retryAfter != "42" {
					t.Errorf("rate_limited.retry_after = %q, want 42", retryAfter)
				}
			}
		}
	}
	if !found {
		t.Error("expected a rate_limited span event")
	}
}

// Note: the dispatch-level arg cap on `.function_call_arguments.done`
// (lines 559–563) and `.output_item.done` (lines 601–606) is defensive
// only. The same byte limit is enforced by the SSE scanner's per-line
// cap and by the B2 dataParts aggregate cap, both of which trip before
// dispatch is reached. The brief flagged these as coverage gaps; they
// are unreachable in production with the current cap configuration. We
// keep the checks for defense-in-depth (so any future relaxation of the
// scanner cap does not silently uncap tool arguments) but do not
// fabricate tests for unreachable paths.

// TestOpenAIResponsesAdapter_UnknownEventSpanEvent verifies that an
// unknown SSE event type emits a span event for production
// observability (M1).
func TestOpenAIResponsesAdapter_UnknownEventSpanEvent(t *testing.T) {
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

	adapter := NewOpenAIResponsesAdapter("test-key", srv.URL)
	adapter.Tracer = tracer

	ctx, span := tracer.Start(context.Background(), "test")
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	span.End()

	stubs := exporter.GetSpans()
	var foundUnknown bool
	for _, s := range stubs {
		for _, ev := range s.Events {
			if ev.Name == "openai_responses.unknown_sse_event" {
				foundUnknown = true
				var typ string
				for _, attr := range ev.Attributes {
					if string(attr.Key) == "event.type" {
						typ = attr.Value.AsString()
					}
				}
				if typ != "response.reasoning_summary.delta" {
					t.Errorf("event.type = %q, want response.reasoning_summary.delta", typ)
				}
			}
		}
	}
	if !foundUnknown {
		t.Error("expected an openai_responses.unknown_sse_event span event")
	}
}

// TestTranslateMessagesResponses_UserTextAndToolResultOrder pins the
// emission ordering for a user message containing
// [text, tool_result, text]. The current contract is: function_call_output
// items emit first (in their document order); user text is batched into
// a single trailing input_text item. See the comment in
// translateMessagesResponses for rationale (M4).
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
	result := translateMessagesResponses(messages)
	if len(result) != 2 {
		t.Fatalf("expected 2 items, got %d: %+v", len(result), result)
	}
	if result[0].Type != "function_call_output" {
		t.Errorf("result[0].Type = %q, want function_call_output", result[0].Type)
	}
	if result[1].Type != "message" || result[1].Role != "user" {
		t.Errorf("result[1] = %+v, want message/user", result[1])
	}
	if len(result[1].Content) == 0 || result[1].Content[0].Text != "Before. After." {
		t.Errorf("result[1].Content = %+v, want concatenated 'Before. After.'", result[1].Content)
	}
}
