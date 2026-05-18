package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// openaiBuilderCases enumerates representative StreamParams shapes for
// the builder vs Stream equivalence assertion. Each case exercises a
// different combination of fields the helper has to project: minimal,
// system+temperature, multi-turn with tool round-trip, multiple tools.
func openaiBuilderCases() []struct {
	name   string
	params types.StreamParams
} {
	return []struct {
		name   string
		params types.StreamParams
	}{
		{
			name: "minimal",
			params: types.StreamParams{
				Model:     "gpt-4o",
				MaxTokens: 1024,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			},
		},
		{
			name: "system_and_temperature",
			params: types.StreamParams{
				Model:       "gpt-4o",
				System:      "You are helpful.",
				MaxTokens:   2048,
				Temperature: types.Float64Ptr(0.5),
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
				},
			},
		},
		{
			name: "tools_and_multi_turn",
			params: types.StreamParams{
				Model:     "gpt-4o",
				System:    "Use tools when needed.",
				MaxTokens: 4096,
				Tools: []types.ToolDefinition{
					{
						Name:        "read_file",
						Description: "Read a file from disk",
						InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
					},
					{
						Name:        "write_file",
						Description: "Write a file to disk",
						InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
					},
				},
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read main.go"}}},
					{Role: "assistant", Content: []types.ContentBlock{
						{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
					}},
					{Role: "user", Content: []types.ContentBlock{
						{Type: "tool_result", ToolUseID: "call_1", Content: "package main"},
					}},
				},
			},
		},
	}
}

// TestBuildOpenAIRequest_MatchesStream pins the invariant that
// buildOpenAIRequest produces the same wire body the Stream method would
// emit. The batch path (phase 6 of #133) reuses the builder and must be
// byte-identical to streaming for the same StreamParams modulo the
// stream toggle.
func TestBuildOpenAIRequest_MatchesStream(t *testing.T) {
	for _, tc := range openaiBuilderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var captured []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				captured = b
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				// Minimal valid chunk + DONE so Stream's SSE consumer
				// terminates cleanly.
				_, _ = fmt.Fprint(w,
					makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
						"data: [DONE]\n\n",
				)
			}))
			defer srv.Close()

			adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

			ch, err := adapter.Stream(context.Background(), tc.params)
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			for range ch {
			}

			built := buildOpenAIRequest(tc.params, true)
			builtBytes, err := json.Marshal(built)
			if err != nil {
				t.Fatalf("marshal builder output: %v", err)
			}

			if string(builtBytes) != string(captured) {
				t.Errorf("builder vs Stream body mismatch\n builder: %s\n stream:  %s", builtBytes, captured)
			}
		})
	}
}

// TestBuildOpenAIRequest_StreamFlag verifies the stream argument drives
// the wire field, so a future batch caller passing false produces a body
// with "stream":false. Pinning this here prevents the helper from
// regressing to a hard-coded true once the batch path lands.
func TestBuildOpenAIRequest_StreamFlag(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	if got := buildOpenAIRequest(params, true).Stream; got != true {
		t.Errorf("stream=true argument: got Stream=%v, want true", got)
	}
	if got := buildOpenAIRequest(params, false).Stream; got != false {
		t.Errorf("stream=false argument: got Stream=%v, want false", got)
	}
}
