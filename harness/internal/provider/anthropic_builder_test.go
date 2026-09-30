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
		{
			name: "thinking_replay",
			params: types.StreamParams{
				Model:     "claude-sonnet-5-5",
				MaxTokens: 4096,
				Messages:  thinkingReplayHistory(),
			},
		},
	}
}

// thinkingReplayHistory is a tool-use round trip whose assistant turn
// carries a signed thinking block (empty text, as the API returns by
// default) and a redacted_thinking block around the text and tool call.
func thinkingReplayHistory() []types.Message {
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read main.go"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "thinking", Text: "", ThoughtSignature: "sig-A"},
			{Type: "text", Text: "Reading it."},
			{Type: "redacted_thinking", ThoughtSignature: "data-B"},
			{Type: "tool_use", ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
		}},
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "package main"},
		}},
	}
}

// assistantWireBlocks returns the raw content blocks of messages[idx] in a
// marshalled Anthropic request body.
func assistantWireBlocks(t *testing.T, body []byte, idx int) []json.RawMessage {
	t.Helper()
	var req struct {
		Messages []struct {
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v\nbody: %s", err, body)
	}
	if idx >= len(req.Messages) {
		t.Fatalf("request has %d messages, want index %d\nbody: %s", len(req.Messages), idx, body)
	}
	return req.Messages[idx].Content
}

// TestBuildAnthropicRequest_ThinkingBlockWireShape pins the exact replay
// shape: "thinking" is present even when empty, the signature and redacted
// data sit under their own keys, and block order is preserved.
func TestBuildAnthropicRequest_ThinkingBlockWireShape(t *testing.T) {
	params := types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: thinkingReplayHistory()}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := []string{
		`{"type":"thinking","thinking":"","signature":"sig-A"}`,
		`{"type":"text","text":"Reading it."}`,
		`{"type":"redacted_thinking","data":"data-B"}`,
		`{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"main.go"}}`,
	}
	got := assistantWireBlocks(t, body, 1)
	if len(got) != len(want) {
		t.Fatalf("assistant blocks = %d, want %d\nbody: %s", len(got), len(want), body)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("block %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestBuildAnthropicRequest_ThinkingTextReplayed(t *testing.T) {
	params := types.StreamParams{
		Model:     "claude-opus-5-5",
		MaxTokens: 16,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "thinking", Text: "The user greets me.", ThoughtSignature: "sig"},
				{Type: "text", Text: "Hello."},
			}},
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "again"}}},
		},
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := assistantWireBlocks(t, body, 1)
	if want := `{"type":"thinking","thinking":"The user greets me.","signature":"sig"}`; string(got[0]) != want {
		t.Errorf("thinking block = %s, want %s", got[0], want)
	}
}

// TestBuildAnthropicRequest_UnsignedThinkingDropped pins that a thinking or
// redacted_thinking block with no signature never reaches the wire (a
// recording-seeded history carries none), leaving the body byte-identical
// to the same history without those blocks.
func TestBuildAnthropicRequest_UnsignedThinkingDropped(t *testing.T) {
	withUnsigned := thinkingReplayHistory()
	withUnsigned[1].Content = []types.ContentBlock{
		{Type: "thinking", Text: "reasoning"},
		{Type: "text", Text: "Reading it."},
		{Type: "redacted_thinking"},
		{Type: "tool_use", ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
	}
	without := thinkingReplayHistory()
	without[1].Content = []types.ContentBlock{
		{Type: "text", Text: "Reading it."},
		{Type: "tool_use", ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
	}

	q := quirks.DefaultRegistry().Resolve("anthropic", "claude-sonnet-5-5")
	gotBody, err := json.Marshal(buildAnthropicRequest(types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: withUnsigned}, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantBody, err := json.Marshal(buildAnthropicRequest(types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: without}, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(gotBody) != string(wantBody) {
		t.Errorf("unsigned thinking blocks leaked onto the wire\n got: %s\nwant: %s", gotBody, wantBody)
	}
}

// TestBuildAnthropicRequest_MatchesStream pins the invariant that
// buildAnthropicRequest produces the same wire body the Stream method
// would emit. The batch path reuses the builder and must be
// byte-identical to streaming for the same StreamParams modulo the
// stream toggle.
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
//   - ToolChoiceTool with no name, an invalid name, or an over-length
//     name degrades to auto (no field).
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
		{"tool with invalid name degrades to auto", types.ToolChoiceTool, "bad name!", ""},
		{"tool with over-length name degrades to auto", types.ToolChoiceTool, strings.Repeat("a", 65), ""},
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

// TestBuildAnthropicRequest_ToolChoice_PartialCapability exercises the
// per-mode guard branch that the full-support builtin rule never
// reaches: a capability with Supported=true but Required=false must omit
// the field rather than fail open and emit {"type":"any"}.
func TestBuildAnthropicRequest_ToolChoice_PartialCapability(t *testing.T) {
	params := types.StreamParams{
		Model:      "claude-sonnet-4-6",
		MaxTokens:  256,
		ToolChoice: types.ToolChoiceRequired,
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	q := quirks.ProviderQuirks{ToolChoice: quirks.ToolChoiceCapability{Supported: true, Required: false, NamedTool: true}}
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "tool_choice") {
		t.Errorf("Required=false capability must emit no tool_choice field, got: %s", body)
	}
}

// TestBuildAnthropicRequest_ThoughtSignatureDropped pins the cross-provider
// leakage invariant at the builder level: the batch path invokes
// buildAnthropicRequest directly, bypassing the Stream-level equivalent
// (TestAnthropic_ThoughtSignatureNotLeakedToAnthropicAPI in
// anthropic_test.go), so a refactor of translateMessagesAnthropic that
// dropped the local anthropicContentBlock wire type could silently forward
// Vertex's encrypted chain-of-thought blob to Anthropic without either test
// catching it.
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

// TestBuildAnthropicRequest_Effort pins the output_config.effort
// projection: emitted only when the resolved model advertises the level,
// and never for a model with an empty allow-list (Haiku 4.5 rejects the
// key outright).
func TestBuildAnthropicRequest_Effort(t *testing.T) {
	cases := []struct {
		model      string
		effort     string
		wantSubstr string // empty means "no output_config field"
	}{
		{"claude-opus-5-5", "high", `"output_config":{"effort":"high"}`},
		{"claude-sonnet-5-5", "low", `"output_config":{"effort":"low"}`},
		{"claude-fable-5-1", "MEDIUM", `"output_config":{"effort":"medium"}`},
		{"claude-opus-4-6", "high", `"output_config":{"effort":"high"}`},
		{"claude-opus-5-5", "", ""},
		{"claude-haiku-4-5-20251001", "high", ""},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			params := types.StreamParams{
				Model:           tc.model,
				MaxTokens:       256,
				ReasoningEffort: tc.effort,
				Messages:        []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
			}
			q := quirks.DefaultRegistry().Resolve("anthropic", tc.model)
			body, err := json.Marshal(buildAnthropicRequest(params, true, q))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.wantSubstr == "" {
				if strings.Contains(string(body), "output_config") {
					t.Errorf("expected no output_config field, got body: %s", body)
				}
				return
			}
			if !strings.Contains(string(body), tc.wantSubstr) {
				t.Errorf("expected %s in body, got: %s", tc.wantSubstr, body)
			}
		})
	}
}

// TestAnthropicStream_RejectsUnsupportedEffortBeforeSend pins the
// fail-closed path: a level outside the model's allow-list is a config
// error returned from Stream, and no request reaches the API. "minimal"
// is the provider-neutral level no Claude model accepts.
func TestAnthropicStream_RejectsUnsupportedEffortBeforeSend(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:           "claude-opus-5-5",
		MaxTokens:       256,
		ReasoningEffort: "minimal",
		Messages:        []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err == nil || !strings.Contains(err.Error(), `reasoningEffort "minimal" is not supported`) {
		t.Fatalf("Stream() error = %v, want unsupported-effort error", err)
	}
	if hits != 0 {
		t.Errorf("server received %d requests, want 0", hits)
	}
}

// TestBuildAnthropicRequest_ForcedToolChoiceDegradesToAuto pins the wire
// shape for models that reject tool_choice "any"/"tool": a forced request
// emits no tool_choice, and a parallel-disable still rides on an auto
// object, the one forced-choice-free shape those models accept.
func TestBuildAnthropicRequest_ForcedToolChoiceDegradesToAuto(t *testing.T) {
	disable := false
	cases := []struct {
		name     string
		choice   types.ToolChoiceMode
		toolName string
		parallel *bool
		want     string // empty means "no tool_choice field"
	}{
		{"required", types.ToolChoiceRequired, "", nil, ""},
		{"named tool", types.ToolChoiceTool, "read_file", nil, ""},
		{"required with parallel disabled", types.ToolChoiceRequired, "", &disable, `"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := types.StreamParams{
				Model:             "claude-sonnet-5-5",
				MaxTokens:         256,
				ToolChoice:        tc.choice,
				ToolChoiceName:    tc.toolName,
				ParallelToolCalls: tc.parallel,
				Tools:             []types.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
				Messages:          []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
			}
			q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
			body, err := json.Marshal(buildAnthropicRequest(params, true, q))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.want == "" {
				if strings.Contains(string(body), "tool_choice") {
					t.Errorf("expected no tool_choice field, got body: %s", body)
				}
				return
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("expected %s in body, got: %s", tc.want, body)
			}
		})
	}
}

// TestBuildAnthropicRequest_PromptCaching pins the system/cache_control
// shapes byte-for-byte across the flag and the cache key. The system text
// carries HTML-significant characters so the flag-off case also proves the
// string form escapes exactly as a plain string field does.
func TestBuildAnthropicRequest_PromptCaching(t *testing.T) {
	msgs := []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}}
	cachingOn := quirks.ProviderQuirks{}
	cachingOn.BehaviourFlags.Anthropic.PromptCaching = true

	cases := []struct {
		name     string
		system   string
		cacheKey string
		q        quirks.ProviderQuirks
		want     string
	}{
		{
			name:     "flag off sends a string system and no cache_control",
			system:   "Use <b> & tools.",
			cacheKey: "k",
			q:        quirks.ProviderQuirks{},
			want:     `{"model":"claude-sonnet-4-6","system":"Use \u003cb\u003e \u0026 tools.","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"max_tokens":256,"stream":true}`,
		},
		{
			name:     "flag off with empty system omits both keys",
			system:   "",
			cacheKey: "k",
			q:        quirks.ProviderQuirks{},
			want:     `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"max_tokens":256,"stream":true}`,
		},
		{
			name:     "flag on without a cache key sends only the system breakpoint",
			system:   "Use <b> & tools.",
			cacheKey: "",
			q:        cachingOn,
			want:     `{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"Use \u003cb\u003e \u0026 tools.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"max_tokens":256,"stream":true}`,
		},
		{
			name:     "flag on with a cache key sends the system breakpoint and top-level cache_control",
			system:   "Use <b> & tools.",
			cacheKey: "k",
			q:        cachingOn,
			want:     `{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"Use \u003cb\u003e \u0026 tools.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"max_tokens":256,"cache_control":{"type":"ephemeral"},"stream":true}`,
		},
		{
			name:     "flag on with empty system and a cache key sends only top-level cache_control",
			system:   "",
			cacheKey: "k",
			q:        cachingOn,
			want:     `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"max_tokens":256,"cache_control":{"type":"ephemeral"},"stream":true}`,
		},
		{
			name:     "flag on with empty system and no cache key sends no cache_control",
			system:   "",
			cacheKey: "",
			q:        cachingOn,
			want:     `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"max_tokens":256,"stream":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := types.StreamParams{
				Model:     "claude-sonnet-4-6",
				System:    tc.system,
				MaxTokens: 256,
				Messages:  msgs,
				CacheKey:  tc.cacheKey,
			}
			body, err := json.Marshal(buildAnthropicRequest(params, true, tc.q))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(body) != tc.want {
				t.Errorf("body mismatch\n got:  %s\n want: %s", body, tc.want)
			}
		})
	}
}

// TestBuildAnthropicRequest_PromptCachingOnEveryModel pins that the
// registry turns caching on for every Anthropic model, including Haiku 4.5
// whose 4,096-token minimum most prompts miss: the API leaves a short
// prompt uncached rather than rejecting the breakpoint.
func TestBuildAnthropicRequest_PromptCachingOnEveryModel(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5-20251001", "claude-sonnet-4-6", "claude-sonnet-5-5", "claude-opus-5-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			params := types.StreamParams{
				Model:     model,
				System:    "sys",
				MaxTokens: 256,
				Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
				CacheKey:  "k",
			}
			q := quirks.DefaultRegistry().Resolve("anthropic", model)
			body, err := json.Marshal(buildAnthropicRequest(params, true, q))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, want := range []string{
				`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}]`,
				`"cache_control":{"type":"ephemeral"},"stream":true`,
			} {
				if !strings.Contains(string(body), want) {
					t.Errorf("expected %s in body, got: %s", want, body)
				}
			}
		})
	}
}

// TestBuildAnthropicRequest_PromptCachingIgnoresToolChoice pins that a
// forced tool_choice turn leaves both breakpoints byte-identical to an
// auto turn, so escalation changes only the tool_choice field.
func TestBuildAnthropicRequest_PromptCachingIgnoresToolChoice(t *testing.T) {
	params := types.StreamParams{
		Model:     "claude-sonnet-4-6",
		System:    "sys",
		MaxTokens: 256,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		CacheKey: "k",
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)

	decode := func(choice types.ToolChoiceMode) map[string]json.RawMessage {
		t.Helper()
		p := params
		p.ToolChoice = choice
		body, err := json.Marshal(buildAnthropicRequest(p, true, q))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return fields
	}
	auto := decode(types.ToolChoiceAuto)
	required := decode(types.ToolChoiceRequired)

	for _, key := range []string{"system", "cache_control"} {
		if _, ok := auto[key]; !ok {
			t.Fatalf("auto body missing %q", key)
		}
	}
	if _, ok := auto["tool_choice"]; ok {
		t.Errorf("auto body carries tool_choice: %s", auto["tool_choice"])
	}
	if got := string(required["tool_choice"]); got != `{"type":"any"}` {
		t.Errorf("required tool_choice = %s, want {\"type\":\"any\"}", got)
	}
	delete(required, "tool_choice")
	if len(required) != len(auto) {
		t.Fatalf("key sets differ beyond tool_choice: auto %d keys, required %d keys", len(auto), len(required))
	}
	for key, want := range auto {
		if got := required[key]; string(got) != string(want) {
			t.Errorf("%s differs between auto and required:\n auto:     %s\n required: %s", key, want, got)
		}
	}
}
