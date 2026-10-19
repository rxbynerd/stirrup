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

// responsesBuilderCases enumerates representative StreamParams shapes
// for the builder vs Stream equivalence assertion. The Responses API's
// input[] discriminated union needs at least one case that exercises
// every variant: message (user/assistant), function_call,
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
			// Pins the "Error: " prefix that translateMessagesResponses adds to
			// error tool results, checked at the builder level.
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
		{
			name: "cache_key",
			params: types.StreamParams{
				Model:     "gpt-4o",
				MaxTokens: 1024,
				CacheKey:  "404b0dfface497f076048e07aa412671",
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			},
		},
	}
}

// TestBuildResponsesRequest_MatchesStream pins the invariant that
// buildResponsesRequest produces the same wire body the Stream method
// would emit, modulo the Stream field (the helper deliberately leaves
// Stream at its zero value; Stream sets it to true after the call). The
// batch path reuses the builder and must be byte-identical otherwise.
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
				if !assert.NoError(t, err, "read request body") {
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
			require.NoError(t, err)
			for range ch {
			}
			captured := <-capturedCh

			q := quirks.DefaultRegistry().Resolve("openai-responses", tc.params.Model)
			built, err := buildResponsesRequest(tc.params, q, nil, "")
			require.NoError(t, err)
			built.Stream = true
			builtBytes, err := json.Marshal(built)
			require.NoError(t, err)

			require.Equal(t, string(captured), string(builtBytes), "builder body should match the Stream wire body")
		})
	}
}

// TestResponsesStrictMode_WireBodyShape exercises the Responses API's
// strict-mode wiring through the builder with a synthetic rule, so the
// rewrite's wire shape (`strict: true` on each tool entry, properties
// expanded into a fully-required nullable shape) is pinned independently
// of which built-in globs enable it.
//
// Mirrors TestOpenAIStrictMode_WireBodyShape on the Chat Completions
// side. One test is sufficient because both adapters share the same
// underlying NormalizeStrictSchema and per-adapter cache; the shape
// of the Responses wire entry (flat `name` + `parameters` + `strict`)
// is what differs and is what this test pins.
func TestResponsesStrictMode_WireBodyShape(t *testing.T) {
	q := quirks.NewRegistry([]quirks.Rule{
		{
			ProviderType: "openai-responses",
			ModelMatch:   "gpt-4o-mini",
			Description:  "test: pin strict mode for gpt-4o-mini on openai-responses",
			LastVerified: quirks.Date("2026-05-24"),
			Apply: func(q *quirks.ProviderQuirks) {
				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
	}).Resolve("openai-responses", "gpt-4o-mini")

	params := types.StreamParams{
		Model:     "gpt-4o-mini",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []types.ToolDefinition{
			{
				Name:        "search",
				Description: "search",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path":  {"type": "string"},
						"limit": {"type": "integer"}
					},
					"required": ["path"]
				}`),
			},
		},
	}
	got, err := buildResponsesRequest(params, q, nil, "")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	tool := got.Tools[0]
	require.NotNil(t, tool.Strict, "tool should set strict")
	require.True(t, *tool.Strict, "tool should be strict")

	var params2 map[string]any
	require.NoError(t, json.Unmarshal(tool.Parameters, &params2))
	assert.Equal(t, false, params2["additionalProperties"])
	required, _ := params2["required"].([]any)
	assert.Len(t, required, 2, "required should list both properties")
	props := params2["properties"].(map[string]any)
	// limit was optional → nullable-wrapped.
	limit := props["limit"].(map[string]any)
	limitType, ok := limit["type"].([]any)
	require.True(t, ok, "limit.type should be in array form")
	assert.Contains(t, limitType, "null", "optional limit should be nullable")
}

// TestResponsesStrictMode_BuiltinRules pins which first-party models get an
// explicit strict:true through DefaultRegistry: the gpt-5, gpt-6 and
// o-series families do, and older chat models keep the omitted key.
func TestResponsesStrictMode_BuiltinRules(t *testing.T) {
	cases := []struct {
		model      string
		wantStrict bool
	}{
		{"gpt-5", true},
		// Inferred: gpt-5.4-mini inherits the gpt-5* rule; only gpt-5.4 is
		// documented.
		{"gpt-5.4-mini", true},
		{"gpt-5.6-sol", true},
		{"gpt-6-astra", true},
		{"gpt-6.1-sol", true},
		{"o3", true},
		{"o4-mini", true},
		{"gpt-4.1", false},
		{"gpt-4o", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			params := effortParams(tc.model, "", true)
			q := quirks.DefaultRegistry().Resolve("openai-responses", tc.model)
			req, err := buildResponsesRequest(params, q, nil, "")
			require.NoError(t, err)
			body, err := json.Marshal(req)
			require.NoError(t, err)
			require.Equal(t, tc.wantStrict, strings.Contains(string(body), `"strict":true`),
				"strict emitted in body: %s", body)
			if tc.wantStrict {
				require.Contains(t, string(body), `"additionalProperties":false`, "strict tool schema should be normalised")
			}
		})
	}
}

// TestResponsesRequest_IncludeEncryptedReasoning pins the include key to
// the reasoning families: they request encrypted reasoning for stateless
// replay, while non-reasoning models keep the body without include because
// they may reject the value (unprobed).
func TestResponsesRequest_IncludeEncryptedReasoning(t *testing.T) {
	const include = `"include":["reasoning.encrypted_content"]`
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5", true},
		{"gpt-5.4", true},
		{"gpt-5.6-terra", true},
		{"gpt-6-astra", true},
		{"o3", true},
		{"gpt-5-chat-latest", false},
		{"gpt-4o", false},
		{"gpt-4.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			q := quirks.DefaultRegistry().Resolve("openai-responses", tc.model)
			req, err := buildResponsesRequest(effortParams(tc.model, "", false), q, nil, "")
			require.NoError(t, err)
			body, err := json.Marshal(req)
			require.NoError(t, err)
			require.Equal(t, tc.want, strings.Contains(string(body), include),
				"include emitted in body: %s", body)

			var back responsesRequest
			require.NoError(t, json.Unmarshal(body, &back))
			require.Equal(t, tc.want, back.IncludeEncryptedReasoning)
		})
	}
}

// TestResponsesRequest_UnmarshalInclude pins the test-side decode of the
// include key: only an array naming reasoning.encrypted_content sets the
// flag, and a non-array or non-string value is an error.
func TestResponsesRequest_UnmarshalInclude(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    bool
		wantErr bool
	}{
		{name: "absent", body: `{"model":"gpt-5"}`},
		{name: "null", body: `{"model":"gpt-5","include":null}`},
		{name: "empty", body: `{"model":"gpt-5","include":[]}`},
		{name: "other value only", body: `{"model":"gpt-5","include":["message.output_text.logprobs"]}`},
		{name: "among others", body: `{"model":"gpt-5","include":["message.output_text.logprobs","reasoning.encrypted_content"]}`, want: true},
		{name: "not an array", body: `{"model":"gpt-5","include":"reasoning.encrypted_content"}`, wantErr: true},
		{name: "non-string element", body: `{"model":"gpt-5","include":[1]}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r responsesRequest
			err := json.Unmarshal([]byte(tc.body), &r)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, r.IncludeEncryptedReasoning)
		})
	}
}

// TestResponsesStrictMode_StreamSendsNormalisedSchema pins the strict wire
// shape through Stream on gpt-5.6: strict:true on the tool and the schema
// rewritten for strict mode.
func TestResponsesStrictMode_StreamSendsNormalisedSchema(t *testing.T) {
	params := effortParams("gpt-5.6-sol", "", false)
	params.Tools = []types.ToolDefinition{{
		Name:        "search",
		Description: "Search files",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"limit":{"type":"integer"}},"required":["pattern"]}`),
	}}
	script := newResponsesScript(t)
	ch, err := script.adapter(nil).Stream(context.Background(), params)
	require.NoError(t, err)
	collectEvents(t, ch)
	var req struct {
		Tools []struct {
			Strict     bool           `json:"strict"`
			Parameters map[string]any `json:"parameters"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(script.lastBody(t), &req))
	require.Len(t, req.Tools, 1)
	require.True(t, req.Tools[0].Strict, "tool should be strict")

	schema := req.Tools[0].Parameters
	assert.Equal(t, false, schema["additionalProperties"])
	required, _ := schema["required"].([]any)
	assert.Len(t, required, 2, "required should list both properties")
	limit, _ := schema["properties"].(map[string]any)["limit"].(map[string]any)
	limitTypes, ok := limit["type"].([]any)
	require.True(t, ok, "limit.type should be in array form")
	assert.Len(t, limitTypes, 2, "optional limit should be made nullable")
}

// TestStrictMode_FailsClosedAlikeOnBothSurfaces pins that a tool schema
// the strict rewriter cannot express (anyOf here, as an MCP server might
// supply) stops a strict-mode request before any bytes are sent, with the
// same lint error on Chat Completions gpt-5 and Responses gpt-5.x.
func TestStrictMode_FailsClosedAlikeOnBothSurfaces(t *testing.T) {
	const lintFailure = "strict-mode schema lint failed"
	params := func(model string) types.StreamParams {
		p := effortParams(model, "", false)
		p.Tools = []types.ToolDefinition{{
			Name:        "mcp_lookup",
			Description: "Look up a record",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"required":["id"]}`),
		}}
		return p
	}
	cases := []struct {
		name  string
		model string
		new   func(url string) ProviderAdapter
	}{
		{"chat gpt-5", "gpt-5", func(url string) ProviderAdapter {
			return NewOpenAICompatibleAdapter(staticBearer("k"), url, OpenAIAuthConfig{}, RetryPolicy{})
		}},
		{"responses gpt-5", "gpt-5", func(url string) ProviderAdapter {
			return NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
		}},
		{"responses gpt-5.6", "gpt-5.6-sol", func(url string) ProviderAdapter {
			return NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
		}},
	}
	var lintErrs []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := countingServer(t)
			_, err := tc.new(srv.URL).Stream(context.Background(), params(tc.model))
			require.ErrorContains(t, err, lintFailure)
			msg := err.Error()
			lintErrs = append(lintErrs, msg[strings.Index(msg, lintFailure):])
			require.Zero(t, hits.Load(), "server should receive no requests")
		})
	}
	require.Len(t, lintErrs, len(cases))
	for _, e := range lintErrs[1:] {
		assert.Equal(t, lintErrs[0], e, "lint errors should match across surfaces")
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
	q := quirks.DefaultRegistry().Resolve("openai-responses", params.Model)
	got, err := buildResponsesRequest(params, q, nil, "")
	require.NoError(t, err)
	require.False(t, got.Stream, "builder should leave Stream unset")
	require.Equal(t, quirks.StoreFalse, got.StoreMode, "builder should default StoreMode to StoreFalse")

	body, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(body), `"stream"`, "builder output should omit the stream key")
}

// TestBuildResponsesRequest_PromptCacheKey pins the prompt_cache_key
// projection: the last key of the body when the resolved flag is on and
// StreamParams.CacheKey is set, and otherwise absent with the rest of the
// body unchanged. The Chat Completions adapter never sends the key, since
// compatible servers may reject unknown fields.
func TestBuildResponsesRequest_PromptCacheKey(t *testing.T) {
	const key = "404b0dfface497f076048e07aa412671"
	base := types.StreamParams{
		Model:     "gpt-4o",
		System:    "sys",
		MaxTokens: 256,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	withKey := base
	withKey.CacheKey = key

	marshal := func(t *testing.T, params types.StreamParams, q quirks.ProviderQuirks) []byte {
		t.Helper()
		req, err := buildResponsesRequest(params, q, nil, "")
		require.NoError(t, err)
		body, err := json.Marshal(req)
		require.NoError(t, err)
		return body
	}
	flagOn := quirks.DefaultRegistry().Resolve("openai-responses", base.Model)
	flagOff := quirks.NewRegistry(nil).Resolve("openai-responses", base.Model)
	noKey := marshal(t, base, flagOn)

	t.Run("flag on with key appends prompt_cache_key", func(t *testing.T) {
		body := marshal(t, withKey, flagOn)
		want := strings.TrimSuffix(string(noKey), "}") + `,"prompt_cache_key":"` + key + `"}`
		require.Equal(t, want, string(body))

		var back responsesRequest
		require.NoError(t, json.Unmarshal(body, &back))
		require.Equal(t, key, back.PromptCacheKey)
	})
	t.Run("flag on without key omits it", func(t *testing.T) {
		require.NotContains(t, string(noKey), "prompt_cache_key")
	})
	t.Run("flag off with key omits it", func(t *testing.T) {
		body := marshal(t, withKey, flagOff)
		require.Equal(t, marshal(t, base, flagOff), body, "flag-off body should ignore CacheKey")
	})
	t.Run("chat completions never sends it", func(t *testing.T) {
		q := quirks.DefaultRegistry().Resolve("openai-compatible", withKey.Model)
		req, err := buildOpenAIRequest(withKey, true, q, nil)
		require.NoError(t, err)
		body, err := json.Marshal(req)
		require.NoError(t, err)
		require.NotContains(t, string(body), "prompt_cache_key")
		require.NotContains(t, string(body), key)
	})
}
