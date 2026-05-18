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

// anthropicBuilderCases enumerates representative StreamParams shapes for
// the builder vs Stream equivalence assertion. Each case exercises a
// different combination of fields the helper has to project: minimal,
// system prompt, multi-turn with tool_use round-trip, explicit
// temperature, multiple tools.
func anthropicBuilderCases() []struct {
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
				Model:     "claude-sonnet-4-6",
				MaxTokens: 1024,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			},
		},
		{
			name: "system_and_temperature",
			params: types.StreamParams{
				Model:       "claude-sonnet-4-6",
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
				Model:     "claude-sonnet-4-6",
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
						{Type: "tool_use", ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
					}},
					{Role: "user", Content: []types.ContentBlock{
						{Type: "tool_result", ToolUseID: "toolu_1", Content: "package main"},
					}},
				},
			},
		},
	}
}

// TestBuildAnthropicRequest_MatchesStream pins the invariant that
// buildAnthropicRequest produces the same wire body the Stream method
// would emit. The batch path (phase 2 of #133) reuses the builder and
// must be byte-identical to streaming for the same StreamParams modulo
// the stream toggle.
func TestBuildAnthropicRequest_MatchesStream(t *testing.T) {
	for _, tc := range anthropicBuilderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var captured []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				captured = b
				// Minimal valid stream so Stream returns without surfacing
				// a parser error on the response side.
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`)+makeSSE("message_stop", `{}`))
			}))
			defer srv.Close()

			adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
			adapter.baseURL = srv.URL

			ch, err := adapter.Stream(context.Background(), tc.params)
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			for range ch {
			}

			built := buildAnthropicRequest(tc.params, true)
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

// TestBuildAnthropicRequest_StreamFlag verifies the stream argument
// drives the wire field, so a future batch caller passing false produces
// a body with "stream":false. Pinning this here prevents the helper
// from regressing to a hard-coded true once the batch path lands.
func TestBuildAnthropicRequest_StreamFlag(t *testing.T) {
	params := types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	if got := buildAnthropicRequest(params, true).Stream; got != true {
		t.Errorf("stream=true argument: got Stream=%v, want true", got)
	}
	if got := buildAnthropicRequest(params, false).Stream; got != false {
		t.Errorf("stream=false argument: got Stream=%v, want false", got)
	}
}

// TestBuildAnthropicRequest_ThoughtSignatureDropped pins the cross-provider
// leakage invariant from issue #194 at the builder level. The Stream-path
// equivalent (TestAnthropic_ThoughtSignatureNotLeakedToAnthropicAPI in
// anthropic_test.go) covers translateMessagesAnthropic transitively through
// Stream; the phase-2 batch caller will invoke buildAnthropicRequest
// directly, bypassing Stream entirely. If translateMessagesAnthropic were
// accidentally refactored to embed types.ContentBlock instead of the
// local anthropicContentBlock wire type, the Stream-level test would still
// pass while the batch path silently forwarded Vertex's encrypted
// chain-of-thought blob to Anthropic. Asserting the builder output here
// closes that structural gap.
func TestBuildAnthropicRequest_ThoughtSignatureDropped(t *testing.T) {
	const sig = "AY89SIGBLOB=="
	params := types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read it"}}},
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					{
						Type:             "tool_use",
						ID:               "toolu_1",
						Name:             "read_file",
						Input:            json.RawMessage(`{"path":"main.go"}`),
						ThoughtSignature: sig,
					},
				},
			},
		},
	}
	body, err := json.Marshal(buildAnthropicRequest(params, false))
	if err != nil {
		t.Fatalf("marshal builder output: %v", err)
	}
	if strings.Contains(string(body), "thought_signature") {
		t.Errorf("builder output contains \"thought_signature\" — Gemini-private state leaked to Anthropic batch path.\nbody = %s", body)
	}
	if strings.Contains(string(body), sig) {
		t.Errorf("builder output contains the signature value %q.\nbody = %s", sig, body)
	}
}
