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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		{
			name:   "tool_search_replay",
			params: toolSearchParams(toolSearchHistory(loadedResult("mcp_github_search_issues"))),
		},
		{
			name: "tool_search_requested_on_unsupported_model",
			params: func() types.StreamParams {
				p := toolSearchParams(toolSearchHistory(loadedResult("mcp_github_search_issues")))
				p.Model = "claude-sonnet-4-20250514"
				return p
			}(),
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
	must := require.New(t)
	var req struct {
		Messages []struct {
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	must.NoError(json.Unmarshal(body, &req), "decode request body: %s", body)
	must.Less(idx, len(req.Messages), "request body: %s", body)
	return req.Messages[idx].Content
}

// anthropicWireBody marshals the request that buildAnthropicRequest builds for params.
func anthropicWireBody(t *testing.T, params types.StreamParams, stream bool, q quirks.ProviderQuirks) []byte {
	t.Helper()
	must := require.New(t)
	body, err := json.Marshal(buildAnthropicRequest(params, stream, q))
	must.NoError(err, "marshal request")
	return body
}

// TestBuildAnthropicRequest_ThinkingBlockWireShape pins the exact replay
// shape: "thinking" is present even when empty, the signature and redacted
// data sit under their own keys, and block order is preserved.
func TestBuildAnthropicRequest_ThinkingBlockWireShape(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: thinkingReplayHistory()}
	body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))

	want := []string{
		`{"type":"thinking","thinking":"","signature":"sig-A"}`,
		`{"type":"text","text":"Reading it."}`,
		`{"type":"redacted_thinking","data":"data-B"}`,
		`{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"main.go"}}`,
	}
	raw := assistantWireBlocks(t, body, 1)
	got := make([]string, len(raw))
	for i, block := range raw {
		got[i] = string(block)
	}
	must.Equal(want, got, "body: %s", body)
}

func TestBuildAnthropicRequest_ThinkingTextReplayed(t *testing.T) {
	must := require.New(t)
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
	body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
	got := assistantWireBlocks(t, body, 1)
	must.NotEmpty(got, "assistant turn has no wire blocks")
	must.JSONEq(`{"type":"thinking","thinking":"The user greets me.","signature":"sig"}`, string(got[0]))
}

// TestBuildAnthropicRequest_UnsignedThinkingDropped pins that a thinking or
// redacted_thinking block with no signature never reaches the wire (a
// recording-seeded history carries none), leaving the body byte-identical
// to the same history without those blocks.
func TestBuildAnthropicRequest_UnsignedThinkingDropped(t *testing.T) {
	must := require.New(t)
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
	gotBody := anthropicWireBody(t, types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: withUnsigned}, true, q)
	wantBody := anthropicWireBody(t, types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: without}, true, q)
	must.Equal(string(wantBody), string(gotBody), "unsigned thinking blocks leaked onto the wire")
}

// thinkingOnlyTurnHistory has a middle assistant turn holding nothing but
// a thinking block, as a turn that stops at max_tokens while thinking
// leaves it.
func thinkingOnlyTurnHistory(signature string) []types.Message {
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Solve it."}}},
		{Role: "assistant", Content: []types.ContentBlock{{Type: "thinking", ThoughtSignature: signature}}},
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Continue."}}},
	}
}

func wireMessages(t *testing.T, history []types.Message) string {
	t.Helper()
	must := require.New(t)
	q := quirks.DefaultRegistry().Resolve("anthropic", "claude-sonnet-5-5")
	body := anthropicWireBody(t, types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: history}, true, q)
	var req struct {
		Messages json.RawMessage `json:"messages"`
	}
	must.NoError(json.Unmarshal(body, &req))
	return string(req.Messages)
}

// TestBuildAnthropicRequest_ThinkingOnlyTurnOmitted pins that an assistant
// turn emptied by the loop's strip or by the unsigned-block drop never
// reaches the wire as an empty content array: it is omitted and the user
// turns on either side are joined into one.
func TestBuildAnthropicRequest_ThinkingOnlyTurnOmitted(t *testing.T) {
	want := `[{"role":"user","content":[{"type":"text","text":"Solve it."},{"type":"text","text":"Continue."}]}]`
	cases := map[string][]types.Message{
		"stripped": types.StripThinkingBlocks(thinkingOnlyTurnHistory("sig-A")),
		"unsigned": thinkingOnlyTurnHistory(""),
	}
	for name, history := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(want, wireMessages(t, history))
		})
	}

	t.Run("signed", func(t *testing.T) {
		must := require.New(t)
		wantSigned := `[{"role":"user","content":[{"type":"text","text":"Solve it."}]},` +
			`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig-A"}]},` +
			`{"role":"user","content":[{"type":"text","text":"Continue."}]}]`
		must.Equal(wantSigned, wireMessages(t, thinkingOnlyTurnHistory("sig-A")))
	})
}

// TestBuildAnthropicRequest_MatchesStream pins the invariant that
// buildAnthropicRequest produces the same wire body the Stream method
// would emit. The batch path reuses the builder and must be
// byte-identical to streaming for the same StreamParams modulo the
// stream toggle.
func TestBuildAnthropicRequest_MatchesStream(t *testing.T) {
	for _, tc := range anthropicBuilderCases() {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			capturedCh := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The handler runs on the server goroutine, so it reports with assert.
				b, err := io.ReadAll(r.Body)
				if !check.NoError(err, "read body") {
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
			must.NoError(err, "Stream()")
			for range ch {
			}
			captured := <-capturedCh

			built := anthropicWireBody(t, tc.params, true, quirks.DefaultRegistry().Resolve("anthropic", tc.params.Model))
			must.Equal(string(captured), string(built), "builder body must match the Stream wire body")
		})
	}
}

// TestBuildAnthropicRequest_StreamFlag checks that the stream argument drives
// the wire field. Both the struct value and the marshalled body are checked,
// because "stream":false is only visible on the wire when the field has no
// omitempty tag.
func TestBuildAnthropicRequest_StreamFlag(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	must.True(buildAnthropicRequest(params, true, q).Stream, "stream=true argument")
	must.False(buildAnthropicRequest(params, false, q).Stream, "stream=false argument")
	must.Contains(string(anthropicWireBody(t, params, true, q)), `"stream":true`)
	must.Contains(string(anthropicWireBody(t, params, false, q)), `"stream":false`)
}

// TestBuildAnthropicRequest_ToolChoiceByMode pins the tool_choice projection
// gated on the resolved capability. Anthropic's base rule advertises
// auto/any/tool (no native none), so:
//   - ToolChoiceAuto (zero) and ToolChoiceNone emit no tool_choice field;
//   - ToolChoiceRequired emits {"type":"any"};
//   - ToolChoiceTool with a name emits {"type":"tool","name":...};
//   - ToolChoiceTool with no name, an invalid name, or an over-length
//     name degrades to auto (no field).
func TestBuildAnthropicRequest_ToolChoiceByMode(t *testing.T) {
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
			must := require.New(t)
			params := base
			params.ToolChoice = tc.choice
			params.ToolChoiceName = tc.toolName
			body := anthropicWireBody(t, params, true, q)
			if tc.wantSubstr == "" {
				must.NotContains(string(body), "tool_choice")
				return
			}
			must.Contains(string(body), tc.wantSubstr)
		})
	}
}

// TestBuildAnthropicRequest_ToolChoiceUnsupportedCapability pins the
// graceful no-op: when the resolved capability advertises no support
// (zero-value quirks), no tool_choice field is emitted even for a
// ToolChoiceRequired request.
func TestBuildAnthropicRequest_ToolChoiceUnsupportedCapability(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{
		Model:      "claude-sonnet-4-6",
		MaxTokens:  256,
		ToolChoice: types.ToolChoiceRequired,
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	body := anthropicWireBody(t, params, true, quirks.ProviderQuirks{})
	must.NotContains(string(body), "tool_choice", "zero-value capability must emit no tool_choice field")
}

// TestBuildAnthropicRequest_ToolChoice_PartialCapability exercises the
// per-mode guard branch that the full-support builtin rule never
// reaches: a capability with Supported=true but Required=false must omit
// the field rather than fail open and emit {"type":"any"}.
func TestBuildAnthropicRequest_ToolChoice_PartialCapability(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{
		Model:      "claude-sonnet-4-6",
		MaxTokens:  256,
		ToolChoice: types.ToolChoiceRequired,
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	q := quirks.ProviderQuirks{ToolChoice: quirks.ToolChoiceCapability{Supported: true, Required: false, NamedTool: true}}
	body := anthropicWireBody(t, params, true, q)
	must.NotContains(string(body), "tool_choice", "Required=false capability must emit no tool_choice field")
}

// TestBuildAnthropicRequest_ThoughtSignatureDropped pins that a Gemini thought
// signature on a tool_use block never reaches the body buildAnthropicRequest
// returns. The Stream-level counterpart is
// TestAnthropic_ThoughtSignatureNotLeakedToAnthropicAPI.
func TestBuildAnthropicRequest_ThoughtSignatureDropped(t *testing.T) {
	must := require.New(t)
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
	body := anthropicWireBody(t, params, false, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
	must.NotContains(string(body), "thought_signature", "Gemini-private state leaked into the Anthropic body")
	must.NotContains(string(body), sig, "thought signature value leaked into the Anthropic body")
}

// TestBuildAnthropicRequest_OutputConfigEffort pins the output_config.effort
// projection: emitted only when the resolved model advertises the level,
// and never for a model with an empty allow-list (Haiku 4.5 rejects the
// key outright).
func TestBuildAnthropicRequest_OutputConfigEffort(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		effort     string
		wantSubstr string // empty means "no output_config field"
	}{
		{"opus 5.5 emits high", "claude-opus-5-5", "high", `"output_config":{"effort":"high"}`},
		{"sonnet 5.5 emits low", "claude-sonnet-5-5", "low", `"output_config":{"effort":"low"}`},
		{"uppercase effort is lowercased", "claude-fable-5-1", "MEDIUM", `"output_config":{"effort":"medium"}`},
		{"opus 4.6 emits high", "claude-opus-4-6", "high", `"output_config":{"effort":"high"}`},
		{"empty effort omits field", "claude-opus-5-5", "", ""},
		{"haiku 4.5 omits field", "claude-haiku-4-5-20251001", "high", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			params := types.StreamParams{
				Model:           tc.model,
				MaxTokens:       256,
				ReasoningEffort: tc.effort,
				Messages:        []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
			}
			body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", tc.model))
			if tc.wantSubstr == "" {
				must.NotContains(string(body), "output_config")
				return
			}
			must.Contains(string(body), tc.wantSubstr)
		})
	}
}

// TestAnthropicStream_RejectsUnsupportedEffortBeforeSend pins the
// fail-closed path: a level outside the model's allow-list is a config
// error returned from Stream, and no request reaches the API. "minimal"
// is the provider-neutral level no Claude model accepts.
func TestAnthropicStream_RejectsUnsupportedEffortBeforeSend(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, `reasoningEffort "minimal" is not supported`)
	must.Zero(hits, "server received requests")
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
		{"required degrades to auto without a field", types.ToolChoiceRequired, "", nil, ""},
		{"named tool degrades to auto without a field", types.ToolChoiceTool, "read_file", nil, ""},
		{"required with parallel disabled keeps an auto object", types.ToolChoiceRequired, "", &disable, `"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			params := types.StreamParams{
				Model:             "claude-sonnet-5-5",
				MaxTokens:         256,
				ToolChoice:        tc.choice,
				ToolChoiceName:    tc.toolName,
				ParallelToolCalls: tc.parallel,
				Tools:             []types.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
				Messages:          []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
			}
			body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
			if tc.want == "" {
				must.NotContains(string(body), "tool_choice")
				return
			}
			must.Contains(string(body), tc.want)
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
			must := require.New(t)
			params := types.StreamParams{
				Model:     "claude-sonnet-4-6",
				System:    tc.system,
				MaxTokens: 256,
				Messages:  msgs,
				CacheKey:  tc.cacheKey,
			}
			body := anthropicWireBody(t, params, true, tc.q)
			must.Equal(tc.want, string(body))
		})
	}
}

// TestBuildAnthropicRequest_PromptCaching_EnabledOnEveryModel pins that the
// registry turns caching on for every Anthropic model, including Haiku 4.5
// whose 4,096-token minimum most prompts miss: the API leaves a short
// prompt uncached rather than rejecting the breakpoint.
func TestBuildAnthropicRequest_PromptCaching_EnabledOnEveryModel(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5-20251001", "claude-sonnet-4-6", "claude-sonnet-5-5", "claude-opus-5-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			params := types.StreamParams{
				Model:     model,
				System:    "sys",
				MaxTokens: 256,
				Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
				CacheKey:  "k",
			}
			q := quirks.DefaultRegistry().Resolve("anthropic", model)
			body, err := json.Marshal(buildAnthropicRequest(params, true, q))
			must.NoError(err)
			for _, want := range []string{
				`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}]`,
				`"cache_control":{"type":"ephemeral"},"stream":true`,
			} {
				must.Contains(string(body), want)
			}
		})
	}
}

// TestBuildAnthropicRequest_PromptCaching_IgnoresToolChoice pins that a
// forced tool_choice turn leaves both breakpoints byte-identical to an
// auto turn, so escalation changes only the tool_choice field.
func TestBuildAnthropicRequest_PromptCaching_IgnoresToolChoice(t *testing.T) {
	must := require.New(t)
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
		must.NoError(err)
		var fields map[string]json.RawMessage
		must.NoError(json.Unmarshal(body, &fields))
		return fields
	}
	auto := decode(types.ToolChoiceAuto)
	required := decode(types.ToolChoiceRequired)

	must.Contains(auto, "system")
	must.Contains(auto, "cache_control")
	must.NotContains(auto, "tool_choice")
	must.JSONEq(`{"type":"any"}`, string(required["tool_choice"]))

	delete(required, "tool_choice")
	must.Len(required, len(auto), "key sets differ beyond tool_choice")
	for key, want := range auto {
		t.Run(key, func(t *testing.T) {
			must := require.New(t)
			must.Equal(string(want), string(required[key]))
		})
	}
}
