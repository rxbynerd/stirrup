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

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
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
			capturedCh := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
					return
				}
				capturedCh <- b
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
			captured := <-capturedCh

			q := quirks.DefaultRegistry().Resolve("anthropic", tc.params.Model)
			built := buildAnthropicRequest(tc.params, true, q)
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
//
// The marshalled-body assertions catch the failure mode the struct-level
// checks miss: if anthropicRequest.Stream were tagged omitempty, the
// struct check would still pass while "stream":false silently disappeared
// from the wire body. Anthropic's tag intentionally lacks omitempty, so
// false must serialise as "stream":false — pin both.
func TestBuildAnthropicRequest_StreamFlag(t *testing.T) {
	params := types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	if got := buildAnthropicRequest(params, true, q).Stream; got != true {
		t.Errorf("stream=true argument: got Stream=%v, want true", got)
	}
	if got := buildAnthropicRequest(params, false, q).Stream; got != false {
		t.Errorf("stream=false argument: got Stream=%v, want false", got)
	}
	trueBody, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal stream=true body: %v", err)
	}
	if !strings.Contains(string(trueBody), `"stream":true`) {
		t.Errorf(`expected "stream":true in stream=true body: %s`, trueBody)
	}
	falseBody, err := json.Marshal(buildAnthropicRequest(params, false, q))
	if err != nil {
		t.Fatalf("marshal stream=false body: %v", err)
	}
	if !strings.Contains(string(falseBody), `"stream":false`) {
		t.Errorf(`expected "stream":false in stream=false body: %s`, falseBody)
	}
}

// TestBuildAnthropicRequest_ToolChoice pins the tool_choice projection
// gated on the resolved capability. Anthropic's base rule advertises
// auto/any/tool (no native none), so:
//   - ToolChoiceAuto (zero) and ToolChoiceNone emit no tool_choice field;
//   - ToolChoiceRequired emits {"type":"any"};
//   - ToolChoiceTool with a name emits {"type":"tool","name":...};
//   - ToolChoiceTool with no name degrades to auto (no field).
func TestBuildAnthropicRequest_ToolChoice(t *testing.T) {
	base := types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 256,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", base.Model)

	cases := []struct {
		name       string
		choice     types.ToolChoiceMode
		toolName   string
		wantSubstr string // empty means "no tool_choice field"
	}{
		{"auto omits field", types.ToolChoiceAuto, "", ""},
		{"none omits field (no native none)", types.ToolChoiceNone, "", ""},
		{"required emits any", types.ToolChoiceRequired, "", `"tool_choice":{"type":"any"}`},
		{"tool emits named", types.ToolChoiceTool, "read_file", `"tool_choice":{"type":"tool","name":"read_file"}`},
		{"tool without name degrades to auto", types.ToolChoiceTool, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := base
			params.ToolChoice = tc.choice
			params.ToolChoiceName = tc.toolName
			body, err := json.Marshal(buildAnthropicRequest(params, true, q))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.wantSubstr == "" {
				if strings.Contains(string(body), "tool_choice") {
					t.Errorf("expected no tool_choice field, got body: %s", body)
				}
				return
			}
			if !strings.Contains(string(body), tc.wantSubstr) {
				t.Errorf("expected %s in body, got: %s", tc.wantSubstr, body)
			}
		})
	}
}

// TestBuildAnthropicRequest_ToolChoiceUnsupportedCapability pins the
// graceful no-op: when the resolved capability advertises no support
// (zero-value quirks), no tool_choice field is emitted even for a
// ToolChoiceRequired request. The escalation chunk's prompt fallback
// covers this case, not the adapter.
func TestBuildAnthropicRequest_ToolChoiceUnsupportedCapability(t *testing.T) {
	params := types.StreamParams{
		Model:      "claude-sonnet-4-6",
		MaxTokens:  256,
		ToolChoice: types.ToolChoiceRequired,
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	body, err := json.Marshal(buildAnthropicRequest(params, true, quirks.ProviderQuirks{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "tool_choice") {
		t.Errorf("zero-value capability must emit no tool_choice field, got: %s", body)
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
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, false, q))
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
