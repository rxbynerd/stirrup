package zai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/compat/zai"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

const doneSSE = "data: [DONE]\n\n"

// glm47ReasoningSSE is a synthetic GLM-4.7 stream with
// reasoning_content as a sibling of content on each assistant delta;
// the pieces concatenate to "Scanning the request. Choosing an answer."
const glm47ReasoningSSE = "data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Scanning the request. \"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Choosing an answer.\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Done.\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":8}}\n\n" +
	"data: [DONE]\n\n"

// staticBearer mirrors the harness-internal helper of the same name;
// compat/zai lives outside package provider so it can't reach it directly.
func staticBearer(s string) func(context.Context) (string, error) {
	return func(_ context.Context) (string, error) {
		return s, nil
	}
}

// zaiRegistry assembles the quirks registry exactly as core/factory.go
// does for compatProfile=zai-glm.
func zaiRegistry() *quirks.Registry {
	return quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))
}

// zaiTestAdapter builds an adapter that uses zaiRegistry.
func zaiTestAdapter(srvURL string) *provider.OpenAICompatibleAdapter {
	adapter := provider.NewOpenAICompatibleAdapter(
		staticBearer("test-key"),
		srvURL,
		provider.OpenAIAuthConfig{},
		provider.RetryPolicy{},
	)
	adapter.Registry = zaiRegistry()
	return adapter
}

// newSSEServer answers every request with the sse stream. When bodies is
// non-nil, each request body is sent on it. The handler runs on the server's
// goroutine, so it reports failures through assert rather than require.
func newSSEServer(t *testing.T, sse string, bodies chan<- []byte) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bodies != nil {
			b, err := io.ReadAll(r.Body)
			if !check.NoError(err, "read body") {
				return
			}
			bodies <- b
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sse)
	}))
}

// captureRequestBody streams one user turn for model through the Z.ai
// adapter and returns the request body the server received.
func captureRequestBody(t *testing.T, model string, maxTokens int) []byte {
	t.Helper()
	must := require.New(t)
	bodies := make(chan []byte, 1)
	srv := newSSEServer(t, doneSSE, bodies)
	defer srv.Close()

	ch, err := zaiTestAdapter(srv.URL).Stream(context.Background(), types.StreamParams{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  []types.Message{userText("hi")},
	})
	must.NoError(err, "Stream")
	for range ch {
	}
	return <-bodies
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	must := require.New(t)
	var got map[string]any
	must.NoError(json.Unmarshal(body, &got), "body: %s", body)
	return got
}

func userText(text string) types.Message {
	return types.Message{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: text}}}
}

// scanForSecretRefs walks v recursively (maps and slices) and reports
// the first string value containing a secret:// reference, or "" if none.
func scanForSecretRefs(v any) string {
	switch x := v.(type) {
	case string:
		if strings.Contains(x, "secret://") {
			return x
		}
	case map[string]any:
		for _, nv := range x {
			if hit := scanForSecretRefs(nv); hit != "" {
				return hit
			}
		}
	case []any:
		for _, nv := range x {
			if hit := scanForSecretRefs(nv); hit != "" {
				return hit
			}
		}
	}
	return ""
}

// TestZAICompatRules_AppliesTokenFieldAndExtraBody pins the base Z.ai
// rule: the legacy "max_tokens" field and "tool_stream": true.
func TestZAICompatRules_AppliesTokenFieldAndExtraBody(t *testing.T) {
	must := require.New(t)
	body := captureRequestBody(t, "glm-4-plus", 4096)
	got := decodeBody(t, body)

	must.InDelta(4096, got["max_tokens"], 0.5, "legacy max_tokens must carry the caller's value: %s", body)
	must.NotContains(got, "max_completion_tokens", "rule must switch the token field: %s", body)
	must.Contains(got, "tool_stream", "body: %s", body)
	must.Equal(true, got["tool_stream"], "body: %s", body)
}

// TestZAICompatRules_GLM47AppliesThinkingFamilyBody pins the
// thinking-family request shape: base quirks plus a top-level
// "thinking" object for a glm-4.7 request.
func TestZAICompatRules_GLM47AppliesThinkingFamilyBody(t *testing.T) {
	must := require.New(t)
	body := captureRequestBody(t, "glm-4.7", 4096)
	got := decodeBody(t, body)

	// The base rule still applies because glm-* also matches glm-4.7.
	must.InDelta(4096, got["max_tokens"], 0.5, "base rule must still set max_tokens: %s", body)
	must.Contains(got, "tool_stream", "body: %s", body)
	must.Equal(true, got["tool_stream"], "body: %s", body)

	must.Contains(got, "thinking", "missing thinking object: %s", body)
	thinking, ok := got["thinking"].(map[string]any)
	must.True(ok, "thinking must be an object, got %v", got["thinking"])
	must.Equal("enabled", thinking["type"], "thinking.type: %s", body)
}

// TestZAICompatRules_GLM47CapturesReasoningContent pins that the zai
// compat rule registers the reasoning_content path for a GLM model,
// so message_complete.ReplayFields accumulates the concatenated pieces.
func TestZAICompatRules_GLM47CapturesReasoningContent(t *testing.T) {
	must := require.New(t)
	srv := newSSEServer(t, glm47ReasoningSSE, nil)
	defer srv.Close()

	ch, err := zaiTestAdapter(srv.URL).Stream(context.Background(), types.StreamParams{
		Model:     "glm-4.7",
		MaxTokens: 1024,
		Messages:  []types.Message{userText("hi")},
	})
	must.NoError(err, "Stream")

	var complete *types.StreamEvent
	for ev := range ch {
		if ev.Type == "message_complete" {
			e := ev
			complete = &e
		}
	}
	must.NotNil(complete, "no message_complete event; GLM stream did not complete")
	want := `"Scanning the request. Choosing an answer."`
	must.Equal(want, string(complete.ReplayFields["reasoning_content"]),
		"zai compat rule must register the reasoning_content path for glm-4.7")
}

// TestZAICompatRules_GLM47TwoTurnRoundTrip pins that the zai
// compat-injected reasoning_content path (not just DeepSeek's builtin
// one) closes the loop: turn-1 reasoning_content is captured onto the
// assistant message and threaded back as a top-level key on turn 2.
func TestZAICompatRules_GLM47TwoTurnRoundTrip(t *testing.T) {
	must := require.New(t)
	bodies := make(chan []byte, 2)
	srv := newSSEServer(t, glm47ReasoningSSE, bodies)
	defer srv.Close()

	adapter := zaiTestAdapter(srv.URL)
	history := []types.Message{userText("hi")}

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "glm-4.7",
		MaxTokens: 1024,
		Messages:  history,
	})
	must.NoError(err, "turn-1 Stream")
	var text strings.Builder
	var replay map[string]json.RawMessage
	for ev := range ch {
		switch ev.Type {
		case "text_delta":
			text.WriteString(ev.Text)
		case "message_complete":
			replay = ev.ReplayFields
		}
	}
	must.NotNil(replay, "turn 1 produced no ReplayFields; capture path did not run for glm-4.7")
	<-bodies // discard the turn-1 request body

	history = append(history,
		types.Message{
			Role:         "assistant",
			Content:      []types.ContentBlock{{Type: "text", Text: text.String()}},
			ReplayFields: replay,
		},
		userText("and now?"),
	)

	ch2, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "glm-4.7",
		MaxTokens: 1024,
		Messages:  history,
	})
	must.NoError(err, "turn-2 Stream")
	for range ch2 {
	}
	turn2Body := <-bodies

	want := `"reasoning_content":"Scanning the request. Choosing an answer."`
	must.Contains(string(turn2Body), want, "turn-2 wire body must thread reasoning_content back: %s", turn2Body)
}

// TestZAICompatRules_ThinkingFamilyResolution pins resolved quirks for
// both thinking-family globs (glm-4.[5-9]* and glm-5*), which share
// applyThinkingFamily.
func TestZAICompatRules_ThinkingFamilyResolution(t *testing.T) {
	reg := zaiRegistry()

	for _, model := range []string{"glm-4.7", "glm-4.5-air", "glm-5", "glm-5.1", "glm-5.3", "glm-5.3-flash"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := reg.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: model})
			openAI := q.BehaviourFlags.OpenAI

			// Base quirks still resolve.
			must.Equal(quirks.TokenFieldMaxTokens, openAI.TokenField)
			must.Contains(openAI.ExtraBodyFields, "tool_stream")
			must.Equal(true, openAI.ExtraBodyFields["tool_stream"])

			// Thinking-family quirks: reasoning_content replay and the thinking object.
			must.Contains(q.ReplayFields, "reasoning_content")
			must.Contains(openAI.ExtraBodyFields, "thinking")
			must.Equal(map[string]any{"type": "enabled"}, openAI.ExtraBodyFields["thinking"])
		})
	}
}

// TestZAICompatRules_FollowLastVerifiedConvention applies the builtin
// rules' LastVerified checks to the compat profile.
func TestZAICompatRules_FollowLastVerifiedConvention(t *testing.T) {
	for _, rule := range zai.CompatRules() {
		t.Run(rule.Description, func(t *testing.T) {
			check := assert.New(t)
			check.Empty(quirks.LastVerifiedProblems(rule, time.Now()))
		})
	}
}

// TestZAIGatewayGLM53_ReasoningEffortOnTheWire pins that a listed effort
// reaches the gateway request and an unlisted one fails before send, since
// GLM-5.3 would otherwise map it to max.
func TestZAIGatewayGLM53_ReasoningEffortOnTheWire(t *testing.T) {
	t.Run("listed level is sent", func(t *testing.T) {
		must := require.New(t)
		bodies := make(chan []byte, 1)
		srv := newSSEServer(t, doneSSE, bodies)
		defer srv.Close()
		ch, err := zaiTestAdapter(srv.URL).Stream(context.Background(), types.StreamParams{
			Model:           "z-ai/glm-5.3",
			MaxTokens:       256,
			ReasoningEffort: "high",
			Messages:        []types.Message{userText("hi")},
		})
		must.NoError(err)
		for range ch {
		}
		must.Equal("high", decodeBody(t, <-bodies)["reasoning_effort"])
	})
	t.Run("unlisted level fails before send", func(t *testing.T) {
		must := require.New(t)
		bodies := make(chan []byte, 1)
		srv := newSSEServer(t, doneSSE, bodies)
		defer srv.Close()
		_, err := zaiTestAdapter(srv.URL).Stream(context.Background(), types.StreamParams{
			Model:           "z-ai/glm-5.3",
			MaxTokens:       256,
			ReasoningEffort: "medium",
			Messages:        []types.Message{userText("hi")},
		})
		must.ErrorContains(err, `reasoningEffort "medium" is not supported`)
		must.Empty(bodies, "no request may reach the server")
	})
}

// TestZAICompatRules_GLM53ReasoningEffort pins the GLM-5.3 effort
// allow-list, first-party and through a gateway prefix. GLM-5.3 maps any
// unlisted value to max, so "medium" must be rejected up front rather
// than forwarded; earlier GLM-5 releases have no probed effort control
// and send nothing.
func TestZAICompatRules_GLM53ReasoningEffort(t *testing.T) {
	reg := zaiRegistry()
	cases := map[string][]string{
		"glm-5.3":            {"low", "high", "max"},
		"glm-5.3-flash":      {"low", "high", "max"},
		"glm-5.3-flashx":     {"low", "high", "max"},
		"z-ai/glm-5.3":       {"low", "high", "max"},
		"z-ai/glm-5.3-flash": {"low", "high", "max"},
		"z-ai/glm-4.7":       {},
		"glm-5.1":            {},
		"glm-4.7":            {},
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			got := reg.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: model}).BehaviourFlags.OpenAI.ReasoningEffortLevels
			must.Equal(want, got)
		})
	}
}

// TestZAICompatRules_LegacyLineHasNoThinking pins that hyphenated
// legacy GLM ids (glm-4-plus, glm-4-flash, glm-4-air) receive only
// the base quirks, never the thinking-family additions.
func TestZAICompatRules_LegacyLineHasNoThinking(t *testing.T) {
	reg := zaiRegistry()

	for _, model := range []string{"glm-4-plus", "glm-4-flash", "glm-4-air"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := reg.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: model})
			openAI := q.BehaviourFlags.OpenAI

			// Base quirks present.
			must.Equal(quirks.TokenFieldMaxTokens, openAI.TokenField)
			must.Contains(openAI.ExtraBodyFields, "tool_stream", "base rule should fire")

			// The 4.5+ thinking scoping must not leak down to the legacy line.
			must.NotContains(q.ReplayFields, "reasoning_content", "legacy line has no thinking mode")
			must.NotContains(openAI.ExtraBodyFields, "thinking", "legacy line has no thinking mode")
		})
	}
}

// TestZAICompatRules_GLM410IsAKnownGap pins a known gap: the
// glm-4.[5-9]* char class stops at 9, so a hypothetical glm-4.10
// receives only the base quirks, no thinking.
func TestZAICompatRules_GLM410IsAKnownGap(t *testing.T) {
	must := require.New(t)
	q := zaiRegistry().Resolve(quirks.Target{ProviderType: "openai-compatible", Model: "glm-4.10"})
	openAI := q.BehaviourFlags.OpenAI

	// Base rule still fires (glm-* matches glm-4.10).
	must.Equal(quirks.TokenFieldMaxTokens, openAI.TokenField, "base rule must still fire for glm-4.10")

	// The [5-9] class does not reach 10.
	const gapMsg = "the [5-9] glob now matches double-digit minors; update the zai.go Rule B comment and this test"
	must.NotContains(q.ReplayFields, "reasoning_content", gapMsg)
	must.NotContains(openAI.ExtraBodyFields, "thinking", gapMsg)
}

// TestZAICompatRules_GatewayPrefixResolution pins the OpenRouter
// gateway rule (z-ai/glm-*): legacy max_tokens and no replay, since
// OpenRouter streams the reasoning as delta.reasoning, never
// reasoning_content; tool_stream and thinking stay absent (vendor
// extras unverified through gateways).
func TestZAICompatRules_GatewayPrefixResolution(t *testing.T) {
	must := require.New(t)
	q := zaiRegistry().Resolve(quirks.Target{ProviderType: "openai-compatible", Model: "z-ai/glm-5.3"})
	openAI := q.BehaviourFlags.OpenAI

	must.Equal(quirks.TokenFieldMaxTokens, openAI.TokenField)
	must.Empty(q.ReplayFields)

	// Vendor extras are unverified through gateways, so they must not be sent.
	must.NotContains(openAI.ExtraBodyFields, "tool_stream")
	must.NotContains(openAI.ExtraBodyFields, "thinking")
}

// TestZAICompatRules_DoesNotAffectNonGLMModels guards against the
// rules' ModelMatch leaking to non-GLM models on the same registry.
func TestZAICompatRules_DoesNotAffectNonGLMModels(t *testing.T) {
	reg := zaiRegistry()

	t.Run("gpt-4o is unaffected", func(t *testing.T) {
		must := require.New(t)
		q := reg.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: "gpt-4o"})
		must.Equal(quirks.TokenFieldMaxCompletionTokens, q.BehaviourFlags.OpenAI.TokenField)
		must.NotContains(q.BehaviourFlags.OpenAI.ExtraBodyFields, "tool_stream")
	})

	t.Run("glm-4-plus gets the rule", func(t *testing.T) {
		must := require.New(t)
		q := reg.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: "glm-4-plus"})
		must.Equal(quirks.TokenFieldMaxTokens, q.BehaviourFlags.OpenAI.TokenField)
	})
}

// TestCompatRules_ReplayFieldsAreThreadable mirrors
// quirks.TestBuiltinRulesReplayFieldsSuffix for CompatRules(), which
// that test cannot reach: every rule registering a ReplayFields path
// must end its Description with "(threaded)" and use a threadable
// single-segment path.
func TestCompatRules_ReplayFieldsAreThreadable(t *testing.T) {
	const threadedSuffix = "(threaded)"
	for _, rule := range zai.CompatRules() {
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			if rule.Apply == nil {
				return
			}
			q := quirks.ProviderQuirks{
				FieldRenames:   map[string]string{},
				OmitFields:     []string{},
				ValueOverrides: map[string]quirks.Value{},
				EnumCoercions:  map[string]map[string]string{},
				ReplayFields:   []string{},
				BehaviourFlags: quirks.ProviderBehaviourFlags{OpenAI: quirks.OpenAIBehaviourFlags{ExtraBodyFields: map[string]any{}}},
			}
			rule.Apply(&q)
			if len(q.ReplayFields) == 0 {
				return
			}

			must.Equal("openai-compatible", rule.ProviderType, "ReplayFields rules must target the openai-compatible adapter")
			must.True(strings.HasSuffix(rule.Description, threadedSuffix), "description must end with %q", threadedSuffix)
			for _, path := range q.ReplayFields {
				segments, err := quirks.ParseReplayPath(path)
				must.NoError(err, "ReplayFields path %q", path)
				must.Len(segments, 1, "ReplayFields path %q must be a single segment", path)
				must.False(segments[0].IsArray, "ReplayFields path %q must not be an array segment", path)
			}
		})
	}
}

// TestCompatRules_RulesHaveRequiredMetadata mirrors
// quirks.TestBuiltinRulesValidate for CompatRules(): every rule must carry
// a non-empty Description, a non-zero LastVerified, and a non-nil Apply.
func TestCompatRules_RulesHaveRequiredMetadata(t *testing.T) {
	for _, rule := range zai.CompatRules() {
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			must.NotEmpty(rule.Description, "Description is required")
			must.False(rule.LastVerified.IsZero(), "LastVerified is required")
			must.NotNil(rule.Apply, "Apply is required")
			must.Equal("openai-compatible", rule.ProviderType, "Z.ai is a compat profile on the openai-compatible adapter")
		})
	}
}

// TestCompatRules_ExtraBodyFieldsHaveNoSecretRefs mirrors
// quirks.TestBuiltinRulesExtraBodyFieldsNoSecrets for CompatRules():
// ExtraBodyFields is serialised verbatim and RunConfig.Redact() never
// reaches inside a quirks rule, so no value may contain a secret:// ref.
func TestCompatRules_ExtraBodyFieldsHaveNoSecretRefs(t *testing.T) {
	must := require.New(t)
	// Representative model id per rule, in declaration order, so
	// Resolve materialises each rule's Apply.
	models := []string{"glm-4-plus", "glm-4.7", "glm-5", "glm-5.3", "z-ai/glm-4.7", "z-ai/glm-5.3"}
	rules := zai.CompatRules()
	must.Len(models, len(rules), "add a representative model for each new CompatRules() entry")
	for i, rule := range rules {
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			q := quirks.NewRegistry([]quirks.Rule{rule}).Resolve(quirks.Target{ProviderType: "openai-compatible", Model: models[i]})
			for k, v := range q.BehaviourFlags.OpenAI.ExtraBodyFields {
				must.Empty(scanForSecretRefs(v),
					"ExtraBodyFields[%q] contains a secret:// reference; compat rule values are serialised verbatim and bypass RunConfig.Redact()", k)
			}
		})
	}
}
