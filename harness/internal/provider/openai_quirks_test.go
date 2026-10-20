package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// fixtureRoot is the relative path from this _test.go file to the
// per-model fixture directories. Tests run in their package's source
// directory so the path is relative to harness/internal/provider/.
const fixtureRoot = "testdata/quirks/openai-compatible"

// quirksCanonicalParams returns a representative StreamParams whose
// shape exercises the fields driven by the quirks layer (model,
// max_tokens, temperature, messages). The same shape is reused across
// every contract test so a fixture for a different model differs only
// in the parts the rules actually change.
func quirksCanonicalParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:       model,
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
		},
	}
}

// TestOpenAIQuirks_O1Mini_OmitsSamplingParams pins the o-series
// reasoning-class wire shape: max_completion_tokens emitted at the
// canonical key, sampling params (temperature) omitted even when the
// caller supplies a non-nil value. The fixture is the source of truth
// for the expected shape; AssertWireEqual normalises both sides.
func TestOpenAIQuirks_O1Mini_OmitsSamplingParams(t *testing.T) {
	must := require.New(t)
	params := quirksCanonicalParams("o1-mini")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "o1-mini: rule not firing")
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "o1-mini", "request.json"), body)
}

// TestOpenAIQuirks_GPT5Nano_OmitsSamplingParams mirrors the o1-mini
// case for the gpt-5 family. Same canonical-form body; pinned by the
// gpt-5* rule.
func TestOpenAIQuirks_GPT5Nano_OmitsSamplingParams(t *testing.T) {
	must := require.New(t)
	params := quirksCanonicalParams("gpt-5-nano")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "gpt-5-nano: rule not firing")
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "gpt-5-nano", "request.json"), body)
}

// TestOpenAIQuirks_GPT5ChatLatest_KeepsSamplingParams pins the carve-out:
// gpt-5-chat-latest matches both gpt-5* and gpt-5-chat*; the longer glob
// runs last and clears OmitSamplingParams, so temperature is on the wire.
func TestOpenAIQuirks_GPT5ChatLatest_KeepsSamplingParams(t *testing.T) {
	must := require.New(t)
	params := quirksCanonicalParams("gpt-5-chat-latest")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams,
		"gpt-5-chat-latest: carve-out should clear OmitSamplingParams (specificity order broken)")
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "gpt-5-chat-latest", "request.json"), body)
}

// TestOpenAIQuirks_GPT4o_NoQuirksApply pins the negative case: gpt-4o
// matches none of the openai-compatible rules. The wire body is
// therefore the canonical openaiRequest projection: max_completion_tokens,
// temperature transmitted, no extra body fields.
func TestOpenAIQuirks_GPT4o_NoQuirksApply(t *testing.T) {
	must := require.New(t)
	params := quirksCanonicalParams("gpt-4o")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams, "gpt-4o: rule misfire")
	must.Equal(quirks.TokenFieldMaxCompletionTokens, q.BehaviourFlags.OpenAI.TokenField, "gpt-4o token field")
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "gpt-4o", "request.json"), body)
}

// deepseekReplayParams returns the canonical multi-turn tool-calling
// StreamParams for the DeepSeek wire-shape tests: a prior assistant
// turn that called a tool and carries the captured reasoning_content,
// followed by the tool result. The shape is the exact scenario the v4
// thinking-mode guide documents as 400-ing without the replay.
func deepseekReplayParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:       model,
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "list main.go"}}},
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					{Type: "text", Text: "Reading the file."},
					{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
				},
				ReplayFields: map[string]json.RawMessage{
					"reasoning_content": json.RawMessage(`"The user wants the file contents; read_file is the right tool."`),
				},
			},
			{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "package main"}}},
		},
	}
}

// TestOpenAIQuirks_DeepSeekV4Flash_RequestShape is the golden contract
// for the threaded DeepSeek v4 rule: the prior turn's reasoning_content
// is replayed as a top-level key on the assistant wire message, the
// token budget uses the legacy max_tokens key, and the caller-supplied
// temperature is suppressed. The fixture is the source of truth.
func TestOpenAIQuirks_DeepSeekV4Flash_RequestShape(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	params := deepseekReplayParams("deepseek-v4-flash")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "deepseek-v4-flash: rule not firing")
	must.Equal(quirks.TokenFieldMaxTokens, q.BehaviourFlags.OpenAI.TokenField, "deepseek-v4-flash token field")
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "deepseek-v4-flash", "request.json"), body)

	// Belt-and-braces string assertions on the load-bearing keys, so a
	// fixture edit cannot silently weaken the contract.
	s := string(body)
	check.Contains(s, `"max_tokens"`, "body must use the legacy max_tokens key")
	check.NotContains(s, `"max_completion_tokens"`, "body must use the legacy max_tokens key")
	check.NotContains(s, `"temperature"`, "body must omit temperature despite OmitSamplingParams")
	check.Contains(s, `"reasoning_content":"The user wants the file contents; read_file is the right tool."`,
		"body must replay reasoning_content on the assistant message")
}

// TestOpenAIQuirks_DeepSeekReasoner_RequestShape mirrors the v4-flash
// golden for the retiring deepseek-reasoner alias (a v4-flash thinking
// alias until 2026-07-24): the rule's behaviour flags must produce the
// same wire divergences — legacy max_tokens, suppressed temperature,
// reasoning_content replayed on the assistant message. The fixture pins
// the current shape for the alias's remaining operation; remove it
// together with the rule after the sunset.
func TestOpenAIQuirks_DeepSeekReasoner_RequestShape(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	params := deepseekReplayParams("deepseek-reasoner")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "deepseek-reasoner: rule not firing")
	must.Equal(quirks.TokenFieldMaxTokens, q.BehaviourFlags.OpenAI.TokenField, "deepseek-reasoner token field")
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "deepseek-reasoner", "request.json"), body)

	s := string(body)
	check.Contains(s, `"max_tokens"`, "body must use the legacy max_tokens key")
	check.NotContains(s, `"max_completion_tokens"`, "body must use the legacy max_tokens key")
	check.NotContains(s, `"temperature"`, "body must omit temperature despite OmitSamplingParams")
	check.Contains(s, `"reasoning_content"`, "body must replay reasoning_content on the assistant message")
}

// TestOpenAIQuirks_DeepSeekV4Flash_SSEParse drives the deepseek-v4-flash
// response fixture through the production Stream path: the SSE parse
// must capture every reasoning_content delta and the message_complete
// event must carry the flattened (concatenated) value. Together with
// the request.json golden this closes the fixture round-trip for the
// explicit flash id — every other model fixture directory carries both
// halves.
func TestOpenAIQuirks_DeepSeekV4Flash_SSEParse(t *testing.T) {
	must := require.New(t)
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4-flash/response.sse"))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-v4-flash",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	must.NoError(err, "Stream")
	var complete *types.StreamEvent
	for _, ev := range drainStream(t, ch) {
		if ev.Type == "message_complete" {
			evCopy := ev
			complete = &evCopy
		}
	}
	must.NotNil(complete, "no message_complete event")
	must.NotNil(complete.ReplayFields, "message_complete carries no ReplayFields for deepseek-v4-flash")
	want := `"Scanning the request. Choosing an answer."`
	must.Equal(want, string(complete.ReplayFields["reasoning_content"]), "ReplayFields[reasoning_content]")
}

// TestOpenAIQuirks_Qwen36_SSEParse_TrailingUsage pins streaming
// output-token accounting. Qwen 3.6 via LM Studio (like OpenAI and
// vLLM) reports usage only when stream_options.include_usage is set, and
// delivers it in a trailing chunk whose choices array is EMPTY — arriving
// after the finish_reason chunk that already produced message_complete.
// The adapter must surface that count. The assertions aggregate across
// every message_complete exactly as streamEventsToResult does (last
// non-empty stop reason, last non-zero output tokens) so the test mirrors
// the real consumer. It also pins the deliberate decision NOT to capture
// Qwen's reasoning_content: unlike DeepSeek/GLM there is no replay rule,
// so no ReplayFields state may leak onto the completed message.
func TestOpenAIQuirks_Qwen36_SSEParse_TrailingUsage(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/qwen3.6-27b/response.sse"))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "qwen/qwen3.6-27b",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	must.NoError(err, "Stream")

	var (
		sawComplete  bool
		stopReason   string
		outputTokens int
		replay       map[string]json.RawMessage
		text         string
	)
	for _, ev := range drainStream(t, ch) {
		switch ev.Type {
		case "text_delta":
			text += ev.Text
		case "message_complete":
			sawComplete = true
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
			if ev.OutputTokens > 0 {
				outputTokens = ev.OutputTokens
			}
			if len(ev.ReplayFields) > 0 {
				replay = ev.ReplayFields
			}
		}
	}
	must.True(sawComplete, "no message_complete event")
	check.Equal(mapFinishReason("stop"), stopReason, "StopReason")
	check.Equal(42, outputTokens, "OutputTokens should come from the trailing choices-empty usage chunk")
	check.Nil(replay, "Qwen 3.6 must not capture reasoning_content as ReplayFields")
	check.Equal("\n\nHello!", text, "content should exclude reasoning_content")
}

// TestOpenAIQuirks_Qwen36_RequestShape pins that Qwen 3.6 needs no
// wire-shape quirks: the request carries the modern max_completion_tokens
// key (not legacy max_tokens), opts into streaming usage, and never
// threads reasoning_content (no replay rule resolves for it).
func TestOpenAIQuirks_Qwen36_RequestShape(t *testing.T) {
	for _, model := range []string{"qwen/qwen3.6-27b", "qwen3.6-35b-a3b-mlx"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := quirks.DefaultRegistry().Resolve("openai-compatible", model)
			must.Empty(q.ReplayFields, "expected no ReplayFields")
			must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams,
				"Qwen recommends sampling params; OmitSamplingParams must stay false")
			params := quirksCanonicalParams(model)
			req, err := buildOpenAIRequest(params, true, q, nil)
			must.NoError(err, "build")
			body, err := json.Marshal(req)
			must.NoError(err, "marshal")
			s := string(body)
			must.Contains(s, `"max_completion_tokens"`)
			must.NotContains(s, `"max_tokens"`, "body must not use legacy max_tokens")
			must.Contains(s, `"stream_options":{"include_usage":true}`, "streaming body must opt into usage reporting")
			must.NotContains(s, "reasoning_content", "body must not thread reasoning_content")
		})
	}
}

// TestOpenAIQuirks_NoReplayRule_OmitsReplayState pins the negative half
// of the threading contract: the same message history sent to a model
// whose resolved quirks register no ReplayFields emits NOTHING extra —
// the registry, not the persisted message, decides what is replayed.
func TestOpenAIQuirks_NoReplayRule_OmitsReplayState(t *testing.T) {
	must := require.New(t)
	params := deepseekReplayParams("gpt-4o")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	must.Empty(q.ReplayFields, "gpt-4o: expected no ReplayFields")
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal")
	must.NotContains(string(body), "reasoning_content", "gpt-4o body leaked reasoning_content with no rule")
}

// TestNoRegressionMaxCompletionTokensDefault: the default
// openai-compatible body MUST emit max_completion_tokens, not the legacy
// max_tokens, regardless of whether a quirk rule fired, guarding against
// a future rule accidentally restoring the legacy field for non-Z.ai
// providers. Reasoning-class models additionally assert temperature is
// absent from the wire body despite a non-nil caller-supplied value.
func TestNoRegressionMaxCompletionTokensDefault(t *testing.T) {
	models := []string{
		"gpt-4o",            // no rule
		"gpt-3.5-turbo",     // no rule
		"o1-mini",           // reasoning-class rule applies
		"gpt-5-nano",        // gpt-5* rule applies
		"gpt-5-chat-latest", // gpt-5-chat* carve-out applies
		"claude-3-opus",     // wrong-provider sanity check
		"random-model-xyz",  // forward-compatible no-rule sanity
	}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			params := quirksCanonicalParams(model)
			q := quirks.DefaultRegistry().Resolve("openai-compatible", model)
			req, err := buildOpenAIRequest(params, true, q, nil)
			must.NoError(err, "build")
			body, err := json.Marshal(req)
			must.NoError(err, "marshal")
			s := string(body)
			must.Contains(s, `"max_completion_tokens"`)
			must.NotContains(s, `"max_tokens"`, "body must not use the legacy max_tokens key")
			if q.BehaviourFlags.OpenAI.OmitSamplingParams {
				must.NotContains(s, `"temperature"`, "suppressed sampling params must not reach the wire")
			}
		})
	}
}

// TestOpenAIRequest_MarshalJSON_ExtraBodyFieldCollision pins the two
// error-return arms in MarshalJSON that guard against a rule author
// bypassing the canonical field set via ExtraBodyFields. Without
// these guards, a rule could side-channel a value for a canonical
// field (e.g. force temperature back on after suppression, or replace
// the model name) by writing to ExtraBodyFields instead of the
// canonical OpenAIBehaviourFlags slots. Each sub-case asserts the
// error fires and that the message contains a substring pinning the
// guard's identity so a refactor cannot silently weaken the check.
func TestOpenAIRequest_MarshalJSON_ExtraBodyFieldCollision(t *testing.T) {
	const wantSubstring = "collides with canonical request field"
	cases := []struct {
		name string
		req  openaiRequest
	}{
		{
			name: "model-key collision (already-emitted canonical field)",
			req: openaiRequest{
				Model:           "gpt-4o",
				MaxTokens:       100,
				ExtraBodyFields: map[string]any{"model": "evil-override"},
			},
		},
		{
			name: "temperature collision against canonical guard when suppressed",
			req: openaiRequest{
				Model:              "o1-mini",
				MaxTokens:          100,
				OmitSamplingParams: true,
				ExtraBodyFields:    map[string]any{"temperature": 0.5},
			},
		},
		{
			name: "max_completion_tokens collision (canonical token field)",
			req: openaiRequest{
				Model:           "gpt-4o",
				MaxTokens:       100,
				ExtraBodyFields: map[string]any{"max_completion_tokens": 99},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := json.Marshal(tc.req)
			must.ErrorContains(err, wantSubstring)
		})
	}
}

// TestOpenAIRequest_UnmarshalJSON_TokenFieldsAndExtras covers the decode
// paths that MarshalJSON's tests would not exercise: the legacy max_tokens
// branch (Z.ai's wire shape), the extras catch-all that preserves
// non-canonical top-level keys for round-trip stability, and the rejection
// of requests that carry both token keys.
func TestOpenAIRequest_UnmarshalJSON_TokenFieldsAndExtras(t *testing.T) {
	t.Run("legacy max_tokens decodes to TokenFieldMaxTokens", func(t *testing.T) {
		must := require.New(t)
		raw := []byte(`{"model":"glm-4-plus","messages":[],"stream":true,"max_tokens":4096}`)
		var req openaiRequest
		must.NoError(json.Unmarshal(raw, &req))
		must.Equal(quirks.TokenFieldMaxTokens, req.TokenField)
		must.Equal(4096, req.MaxTokens)
	})

	t.Run("modern max_completion_tokens decodes to TokenFieldMaxCompletionTokens", func(t *testing.T) {
		must := require.New(t)
		raw := []byte(`{"model":"o1-mini","messages":[],"stream":true,"max_completion_tokens":2048}`)
		var req openaiRequest
		must.NoError(json.Unmarshal(raw, &req))
		must.Equal(quirks.TokenFieldMaxCompletionTokens, req.TokenField)
		must.Equal(2048, req.MaxTokens)
	})

	t.Run("non-canonical top-level key surfaces in ExtraBodyFields", func(t *testing.T) {
		must := require.New(t)
		raw := []byte(`{"model":"glm-4-plus","messages":[],"stream":true,"max_tokens":4096,"tool_stream":true}`)
		var req openaiRequest
		must.NoError(json.Unmarshal(raw, &req))
		must.Equal(true, req.ExtraBodyFields["tool_stream"])
	})

	t.Run("dual token keys are rejected", func(t *testing.T) {
		must := require.New(t)
		raw := []byte(`{"model":"gpt-4o","messages":[],"stream":true,"max_completion_tokens":100,"max_tokens":200}`)
		var req openaiRequest
		err := json.Unmarshal(raw, &req)
		must.ErrorContains(err, "both max_completion_tokens and max_tokens present")
	})

	t.Run("round-trip with ExtraBodyFields preserves shape", func(t *testing.T) {
		must := require.New(t)
		original := openaiRequest{
			Model:           "glm-4-plus",
			Messages:        []openaiMessage{{Role: "user", Content: "hi"}},
			MaxTokens:       4096,
			Stream:          true,
			TokenField:      quirks.TokenFieldMaxTokens,
			ExtraBodyFields: map[string]any{"tool_stream": true},
		}
		out, err := json.Marshal(original)
		must.NoError(err, "Marshal")
		var round openaiRequest
		must.NoError(json.Unmarshal(out, &round), "Unmarshal")
		must.Equal(original.Model, round.Model)
		must.Equal(original.MaxTokens, round.MaxTokens)
		must.Equal(original.TokenField, round.TokenField)
		must.Equal(original.Stream, round.Stream)
		must.Equal(true, round.ExtraBodyFields["tool_stream"])
	})

	t.Run("tool_choice string round-trips out of ExtraBodyFields", func(t *testing.T) {
		must := require.New(t)
		// A string tool_choice must decode into r.ToolChoice, not leak
		// into ExtraBodyFields (which would trip the marshal-side
		// canonical-field collision guard on the next encode).
		original := openaiRequest{
			Model:      "gpt-4o",
			Messages:   []openaiMessage{{Role: "user", Content: "hi"}},
			MaxTokens:  256,
			Stream:     true,
			ToolChoice: "required",
		}
		out, err := json.Marshal(original)
		must.NoError(err, "Marshal")
		var round openaiRequest
		must.NoError(json.Unmarshal(out, &round), "Unmarshal")
		must.NotContains(round.ExtraBodyFields, "tool_choice")
		must.Equal("required", round.ToolChoice)
		// Re-marshalling the decoded request must reproduce the field
		// stably (the canonical guard must not reject it).
		out2, err := json.Marshal(round)
		must.NoError(err, "re-Marshal")
		must.Contains(string(out2), `"tool_choice":"required"`)
	})

	t.Run("tool_choice typed named object round-trips with deterministic order", func(t *testing.T) {
		must := require.New(t)
		original := openaiRequest{
			Model:     "gpt-4o",
			Messages:  []openaiMessage{{Role: "user", Content: "hi"}},
			MaxTokens: 256,
			Stream:    true,
			ToolChoice: openAINamedToolChoice{
				Type:     "function",
				Function: openAINamedToolChoiceFunc{Name: "read_file"},
			},
		}
		out, err := json.Marshal(original)
		must.NoError(err, "Marshal")
		// The typed struct pins key order: "type" before "function".
		must.Contains(string(out), `"tool_choice":{"type":"function","function":{"name":"read_file"}}`)
		var round openaiRequest
		must.NoError(json.Unmarshal(out, &round), "Unmarshal")
		must.NotContains(round.ExtraBodyFields, "tool_choice")
		must.NotNil(round.ToolChoice, "decoded ToolChoice should hold the function object")
	})
}

// quirksLogStubServer returns an httptest server that drains the
// request body and returns an empty [DONE] SSE stream. Used by the
// logging tests so the Stream call completes the full warn/debug
// path without touching a real OpenAI endpoint.
func quirksLogStubServer(t *testing.T) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		check.NoError(err, "read body")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestOpenAIAdapter_OmitSamplingParams_WarnsWithRuleDescription:
// when OmitSamplingParams suppresses a caller-supplied non-nil
// Temperature, the warn log must fire, name the rule that caused the
// suppression, and NOT include the suppressed value itself (a caller
// value must not propagate into any log sink that captures warn records).
func TestOpenAIAdapter_OmitSamplingParams_WarnsWithRuleDescription(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := quirksLogStubServer(t)
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "o1-mini",
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	must.NoError(err)
	for range ch {
	}

	logOutput := buf.String()
	check.Contains(logOutput, "openai quirks suppressed caller temperature")
	// The reasoning-class rule description must appear in the warn
	// record so the operator can trace which rule fired. The exact
	// substring is the rule's Description field — if the rule text
	// changes the substring below must change with it.
	const wantRule = "OpenAI reasoning-class"
	check.Contains(logOutput, wantRule)
	// The suppressed value must NOT appear in the log. The number
	// "0.5" alone could collide with a metric value or a duration, so
	// pin against the named attribute key as well.
	check.NotContains(logOutput, "temperature.suppressed", "the suppressed temperature must not leak into the warn record")
}

// TestLogReplayFieldsCapture_NonStringValueLogsLengthOnly exercises the non-string
// fallback branch in logReplayFieldsCapture's per-value switch: the
// default arm runs json.Marshal on the value to compute its length
// contribution. Current ReplayFields rules only register string-typed
// fields, but CaptureReplayFields can return any JSON value type, and
// the helper is the safety gate for future rules — the log line must be
// emitted with the field name and length metadata, and never the
// literal value.
func TestLogReplayFieldsCapture_NonStringValueLogsLengthOnly(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Use a float64 value that, when JSON-marshalled, yields a
	// distinctive substring ("123.45"). The default branch in the
	// type switch is the one that fires here — strings short-circuit
	// on the typed case before reaching the marshal path.
	capture := map[string][]any{
		"reasoning_content": {float64(123.45)},
	}
	logReplayFieldsCapture(context.Background(), logger, "openai-compatible", "deepseek-reasoner", capture)

	out := buf.String()
	must.Contains(out, "quirks replay fields captured")
	check.Contains(out, "reasoning_content")
	check.Contains(out, `"count":1`)
	// json.Marshal(float64(123.45)) = "123.45" (6 bytes). The
	// total_len must equal 6. Pin it exactly to catch any future
	// regression that decides to skip the default branch.
	check.Contains(out, `"total_len":6`)
	// The value itself must NOT appear in the log under any
	// representation. "123.45" is the canonical JSON form and the
	// one the marshal path would emit if a future refactor swapped
	// length-reporting for value-reporting.
	check.NotContains(out, "123.45", "length-only invariant: the non-string value must not be logged")
}

// TestOpenAIChunkChoice_UnmarshalJSON_DeltaStates covers the custom
// decoder's three observable states: a chunk with no delta key (the false branch of the
// len(helper.Delta) > 0 guard), a chunk with a malformed delta value
// (the json.Unmarshal error return), and a chunk with a valid delta (the
// happy path that populates both RawDelta and the typed Delta).
func TestOpenAIChunkChoice_UnmarshalJSON_DeltaStates(t *testing.T) {
	t.Run("no delta key", func(t *testing.T) {
		must := require.New(t)
		raw := []byte(`{"index":0,"finish_reason":"stop"}`)
		var c openaiChunkChoice
		must.NoError(json.Unmarshal(raw, &c))
		must.Nil(c.RawDelta, "RawDelta must stay nil when no delta key is present")
		// Delta must remain the zero value: no Role, no Content, no
		// ToolCalls.
		must.Empty(c.Delta.Role)
		must.Nil(c.Delta.Content)
		must.Empty(c.Delta.ToolCalls)
		must.Equal(0, c.Index)
		must.NotNil(c.FinishReason)
		must.Equal("stop", *c.FinishReason)
	})
	t.Run("malformed delta", func(t *testing.T) {
		must := require.New(t)
		// A non-object delta cannot decode into openaiDelta (which is
		// a struct); the typed Unmarshal returns a UnmarshalTypeError
		// that the custom UnmarshalJSON wraps under
		// "openaiChunkChoice.delta:". The error must surface.
		raw := []byte(`{"index":0,"delta":"not-an-object"}`)
		var c openaiChunkChoice
		err := json.Unmarshal(raw, &c)
		must.Error(err)
		must.ErrorContains(err, "openaiChunkChoice.delta")
	})
	t.Run("valid delta", func(t *testing.T) {
		must := require.New(t)
		raw := []byte(`{"index":0,"delta":{"role":"assistant","content":"hello"}}`)
		var c openaiChunkChoice
		must.NoError(json.Unmarshal(raw, &c))
		must.Equal("assistant", c.Delta.Role)
		must.NotNil(c.Delta.Content)
		must.Equal("hello", *c.Delta.Content)
		// RawDelta must hold the raw bytes of the delta object so a
		// ReplayFields walker can descend without coupling to the
		// typed struct. The bytes are not whitespace-normalised; an
		// exact match is the strongest assertion.
		const wantRaw = `{"role":"assistant","content":"hello"}`
		must.Equal(wantRaw, string(c.RawDelta)) //nolint:testifylint // exact bytes are the contract
	})
}

// TestOpenAIAdapter_DebugLogListsAppliedRules pins the per-Stream debug
// line: it fires at the top of every Stream call and lists the
// descriptions of the rules that contributed to the resolution. An empty
// rule list is still logged, so a grep for the line never misses a
// resolution.
func TestOpenAIAdapter_DebugLogListsAppliedRules(t *testing.T) {
	srv := quirksLogStubServer(t)
	defer srv.Close()

	cases := []struct {
		name  string
		model string
		// Substrings the debug record must contain. Empty means no
		// rule fired — the debug record fires anyway with `rules:[]`.
		wantRuleSubstrings []string
	}{
		{
			name:               "reasoning-class rule fires",
			model:              "o1-mini",
			wantRuleSubstrings: []string{"OpenAI reasoning-class"},
		},
		{
			name:               "no rule fires for gpt-4o",
			model:              "gpt-4o",
			wantRuleSubstrings: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
			adapter.Logger = logger

			ch, err := adapter.Stream(context.Background(), types.StreamParams{
				Model:     tc.model,
				MaxTokens: 4096,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			})
			must.NoError(err)
			for range ch {
			}

			logOutput := buf.String()
			must.Contains(logOutput, "openai quirks resolved")
			for _, want := range tc.wantRuleSubstrings {
				must.Contains(logOutput, want)
			}
		})
	}
}
