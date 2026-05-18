package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// responsesBuilderCases enumerates representative StreamParams shapes
// for the builder vs Stream equivalence assertion. The Responses API's
// input[] discriminated union means we want at least one case that
// exercises every variant: message (user/assistant), function_call,
// function_call_output.
func responsesBuilderCases() []struct {
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
			name: "instructions_and_temperature",
			params: types.StreamParams{
				Model:       "gpt-4o",
				System:      "You are helpful.",
				MaxTokens:   2048,
				Temperature: types.Float64Ptr(0.0),
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
		{
			// Pins the "Error: " prefix injection in
			// translateMessagesResponses at the builder level. Covered by
			// openai_responses_test.go through Stream, but the batch path
			// will call the builder directly; the MatchesStream harness
			// extends that coverage here automatically.
			name: "is_error_tool_result",
			params: types.StreamParams{
				Model:     "gpt-4o",
				MaxTokens: 1024,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "run it"}}},
					{Role: "assistant", Content: []types.ContentBlock{
						{Type: "tool_use", ID: "call_1", Name: "run", Input: json.RawMessage(`{}`)},
					}},
					{Role: "user", Content: []types.ContentBlock{
						{Type: "tool_result", ToolUseID: "call_1", Content: "disk full", IsError: true},
					}},
				},
			},
		},
	}
}

// TestBuildResponsesRequest_MatchesStream pins the invariant that
// buildResponsesRequest produces the same wire body the Stream method
// would emit, modulo the Stream field (the helper deliberately leaves
// Stream at its zero value; Stream sets it to true after the call). The
// batch path (phase 6 of #133) reuses the builder and must be
// byte-identical otherwise.
//
// Equivalence is checked by setting Stream=true on the builder output
// before marshalling, mirroring Stream's own pin. This both confirms
// the projection is otherwise complete AND that the wire-toggle
// responsibility lives at the call site (not the helper).
func TestBuildResponsesRequest_MatchesStream(t *testing.T) {
	for _, tc := range responsesBuilderCases() {
		t.Run(tc.name, func(t *testing.T) {
			capturedCh := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
					return
				}
				capturedCh <- b
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				// Minimal completion event so Stream returns without a
				// parser error.
				_, _ = fmt.Fprint(w,
					"event: response.completed\n"+
						`data: {"response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n",
				)
			}))
			defer srv.Close()

			adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

			ch, err := adapter.Stream(context.Background(), tc.params)
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			for range ch {
			}
			captured := <-capturedCh

			built := buildResponsesRequest(tc.params)
			built.Stream = true
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

// TestBuildResponsesRequest_StreamDefaultFalse verifies the helper
// leaves Stream at its zero value AND that the marshalled wire body
// omits the "stream" key entirely (via omitempty on the struct tag).
// The batch path relies on this so a non-streaming submission does
// not send "stream":false — the Anthropic Messages Batches API
// rejects the field outright; the Responses batch endpoint contract
// is unverified, and omission is the safer default.
func TestBuildResponsesRequest_StreamDefaultFalse(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	got := buildResponsesRequest(params)
	if got.Stream != false {
		t.Errorf("builder default: Stream = %v, want false", got.Stream)
	}
	if got.Store != false {
		t.Errorf("builder default: Store = %v, want false", got.Store)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal builder output: %v", err)
	}
	if strings.Contains(string(body), `"stream"`) {
		t.Errorf(`expected "stream" key to be omitted from builder output: %s`, body)
	}
}
