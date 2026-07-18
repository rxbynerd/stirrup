package zai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/compat/zai"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// staticBearer mirrors the harness-internal helper of the same name;
// compat/zai lives outside package provider so it can't reach it directly.
func staticBearer(s string) func(context.Context) (string, error) {
	return func(_ context.Context) (string, error) {
		return s, nil
	}
}

func hasReplayField(q quirks.ProviderQuirks, path string) bool {
	for _, p := range q.ReplayFields {
		if p == path {
			return true
		}
	}
	return false
}

// TestZAICompatRules_AppliesTokenFieldAndExtraBody pins the base
// Z.ai compat rule: legacy "max_tokens" field and "tool_stream": true.
func TestZAICompatRules_AppliesTokenFieldAndExtraBody(t *testing.T) {
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		captured <- b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := provider.NewOpenAICompatibleAdapter(
		staticBearer("test-key"),
		srv.URL,
		provider.OpenAIAuthConfig{},
		provider.RetryPolicy{},
	)
	rules := append(quirks.BuiltinRules(), zai.CompatRules()...)
	adapter.Registry = quirks.NewRegistry(rules)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "glm-4-plus",
		MaxTokens: 4096,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	body := <-captured

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}

	// Legacy token field present (with the caller's value), modern key absent.
	if v, ok := got["max_tokens"]; !ok {
		t.Errorf("Z.ai body missing 'max_tokens' (legacy field required): %s", body)
	} else if n, isNum := v.(float64); !isNum || n != 4096 {
		t.Errorf("Z.ai 'max_tokens' = %v (type %T), want 4096", v, v)
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Errorf("Z.ai body contains 'max_completion_tokens' (rule failed to switch field): %s", body)
	}
	// tool_stream extra body field present and true.
	v, ok := got["tool_stream"]
	if !ok {
		t.Errorf("Z.ai body missing 'tool_stream' extension: %s", body)
	}
	if b, isBool := v.(bool); !isBool || !b {
		t.Errorf("Z.ai 'tool_stream' = %v (type %T), want true (bool)", v, v)
	}
}

// TestZAICompatRules_GLM47AppliesThinkingFamilyBody pins the
// thinking-family request shape: base quirks plus a top-level
// "thinking" object for a glm-4.7 request.
func TestZAICompatRules_GLM47AppliesThinkingFamilyBody(t *testing.T) {
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		captured <- b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := provider.NewOpenAICompatibleAdapter(
		staticBearer("test-key"),
		srv.URL,
		provider.OpenAIAuthConfig{},
		provider.RetryPolicy{},
	)
	rules := append(quirks.BuiltinRules(), zai.CompatRules()...)
	adapter.Registry = quirks.NewRegistry(rules)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "glm-4.7",
		MaxTokens: 4096,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	body := <-captured

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}

	// Base rule still contributes (glm-* matches glm-4.7 too).
	if v, ok := got["max_tokens"]; !ok {
		t.Errorf("glm-4.7 body missing 'max_tokens' (base rule must still fire): %s", body)
	} else if n, isNum := v.(float64); !isNum || n != 4096 {
		t.Errorf("glm-4.7 'max_tokens' = %v (type %T), want 4096", v, v)
	}
	if v, ok := got["tool_stream"]; !ok {
		t.Errorf("glm-4.7 body missing 'tool_stream': %s", body)
	} else if b, isBool := v.(bool); !isBool || !b {
		t.Errorf("glm-4.7 'tool_stream' = %v (type %T), want true", v, v)
	}
	// Thinking-family rule contribution: top-level thinking object.
	thinking, ok := got["thinking"]
	if !ok {
		t.Fatalf("glm-4.7 body missing 'thinking' object: %s", body)
	}
	tm, ok := thinking.(map[string]any)
	if !ok {
		t.Fatalf("glm-4.7 'thinking' = %v (type %T), want object", thinking, thinking)
	}
	if tm["type"] != "enabled" {
		t.Errorf("glm-4.7 thinking.type = %v, want \"enabled\": %s", tm["type"], body)
	}
}

// glm47ReasoningSSE is a synthetic GLM-4.7 stream with
// reasoning_content as a sibling of content on each assistant delta;
// the pieces concatenate to "Scanning the request. Choosing an answer."
const glm47ReasoningSSE = "data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Scanning the request. \"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Choosing an answer.\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Done.\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":8}}\n\n" +
	"data: [DONE]\n\n"

// zaiTestAdapter builds an adapter with a registry assembled exactly
// as core/factory.go does for compatProfile=zai-glm.
func zaiTestAdapter(srvURL string) *provider.OpenAICompatibleAdapter {
	adapter := provider.NewOpenAICompatibleAdapter(
		staticBearer("test-key"),
		srvURL,
		provider.OpenAIAuthConfig{},
		provider.RetryPolicy{},
	)
	adapter.Registry = quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))
	return adapter
}

// TestZAICompatRules_GLM47CapturesReasoningContent pins that the zai
// compat rule registers the reasoning_content path for a GLM model,
// so message_complete.ReplayFields accumulates the concatenated pieces.
func TestZAICompatRules_GLM47CapturesReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, glm47ReasoningSSE)
	}))
	defer srv.Close()

	adapter := zaiTestAdapter(srv.URL)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "glm-4.7",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var complete *types.StreamEvent
	for ev := range ch {
		if ev.Type == "message_complete" {
			e := ev
			complete = &e
		}
	}
	if complete == nil {
		t.Fatal("no message_complete event; GLM stream did not complete")
	}
	want := `"Scanning the request. Choosing an answer."`
	if got := string(complete.ReplayFields["reasoning_content"]); got != want {
		t.Errorf("message_complete ReplayFields[reasoning_content] = %s, want %s "+
			"(zai compat rule must register the reasoning_content path for glm-4.7)", got, want)
	}
}

// TestZAICompatRules_GLM47TwoTurnRoundTrip pins that the zai
// compat-injected reasoning_content path (not just DeepSeek's builtin
// one) closes the loop: turn-1 reasoning_content is captured onto the
// assistant message and threaded back as a top-level key on turn 2.
func TestZAICompatRules_GLM47TwoTurnRoundTrip(t *testing.T) {
	bodies := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		bodies <- b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, glm47ReasoningSSE)
	}))
	defer srv.Close()

	adapter := zaiTestAdapter(srv.URL)

	history := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
	}

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "glm-4.7",
		MaxTokens: 1024,
		Messages:  history,
	})
	if err != nil {
		t.Fatalf("turn-1 Stream: %v", err)
	}
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
	if replay == nil {
		t.Fatal("turn 1 produced no ReplayFields; capture path did not run for glm-4.7")
	}
	<-bodies // discard the turn-1 request body

	history = append(history,
		types.Message{
			Role:         "assistant",
			Content:      []types.ContentBlock{{Type: "text", Text: text.String()}},
			ReplayFields: replay,
		},
		types.Message{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "and now?"}},
		},
	)

	ch2, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "glm-4.7",
		MaxTokens: 1024,
		Messages:  history,
	})
	if err != nil {
		t.Fatalf("turn-2 Stream: %v", err)
	}
	for range ch2 {
	}
	turn2Body := <-bodies

	want := `"reasoning_content":"Scanning the request. Choosing an answer."`
	if !strings.Contains(string(turn2Body), want) {
		t.Errorf("turn-2 wire body missing threaded reasoning_content:\nwant substring: %s\nbody: %s", want, turn2Body)
	}
}

// TestZAICompatRules_ThinkingFamilyResolution pins resolved quirks for
// both thinking-family globs (glm-4.[5-9]* and glm-5*), which share
// applyThinkingFamily.
func TestZAICompatRules_ThinkingFamilyResolution(t *testing.T) {
	reg := quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))

	for _, model := range []string{"glm-4.7", "glm-4.5-air", "glm-5", "glm-5.1"} {
		t.Run(model, func(t *testing.T) {
			q := reg.Resolve("openai-compatible", model)

			// Base quirks still resolve.
			if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxTokens {
				t.Errorf("%s: TokenField = %v, want max_tokens", model, q.BehaviourFlags.OpenAI.TokenField)
			}
			if v, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"]; !ok {
				t.Errorf("%s: tool_stream missing from ExtraBodyFields", model)
			} else if b, isBool := v.(bool); !isBool || !b {
				t.Errorf("%s: tool_stream = %v (type %T), want true", model, v, v)
			}

			// Thinking-family quirks: reasoning_content replay + thinking object.
			if !hasReplayField(q, "reasoning_content") {
				t.Errorf("%s: reasoning_content not in ReplayFields %v", model, q.ReplayFields)
			}
			thinking, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"]
			if !ok {
				t.Fatalf("%s: thinking missing from ExtraBodyFields", model)
			}
			if !reflect.DeepEqual(thinking, map[string]any{"type": "enabled"}) {
				t.Errorf("%s: thinking = %#v, want map[string]any{\"type\":\"enabled\"}", model, thinking)
			}
		})
	}
}

// TestZAICompatRules_LegacyLineHasNoThinking pins that hyphenated
// legacy GLM ids (glm-4-plus, glm-4-flash, glm-4-air) receive only
// the base quirks, never the thinking-family additions.
func TestZAICompatRules_LegacyLineHasNoThinking(t *testing.T) {
	reg := quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))

	for _, model := range []string{"glm-4-plus", "glm-4-flash", "glm-4-air"} {
		t.Run(model, func(t *testing.T) {
			q := reg.Resolve("openai-compatible", model)

			// Base quirks present.
			if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxTokens {
				t.Errorf("%s: TokenField = %v, want max_tokens", model, q.BehaviourFlags.OpenAI.TokenField)
			}
			if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"]; !ok {
				t.Errorf("%s: tool_stream missing; base rule should fire", model)
			}

			// Thinking quirks ABSENT — the 4.5+ scoping must not leak down.
			if hasReplayField(q, "reasoning_content") {
				t.Errorf("%s: reasoning_content leaked into ReplayFields %v (legacy line has no thinking mode)", model, q.ReplayFields)
			}
			if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"]; ok {
				t.Errorf("%s: thinking leaked into ExtraBodyFields (legacy line has no thinking mode)", model)
			}
		})
	}
}

// TestZAICompatRules_GLM410IsAKnownGap pins a known gap: the
// glm-4.[5-9]* char class stops at 9, so a hypothetical glm-4.10
// receives only the base quirks, no thinking.
func TestZAICompatRules_GLM410IsAKnownGap(t *testing.T) {
	reg := quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))
	q := reg.Resolve("openai-compatible", "glm-4.10")

	// Base rule still fires (glm-* matches glm-4.10).
	if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxTokens {
		t.Errorf("glm-4.10: TokenField = %v, want max_tokens (base rule)", q.BehaviourFlags.OpenAI.TokenField)
	}
	// Thinking quirks ABSENT — the [5-9] class does not reach 10.
	if hasReplayField(q, "reasoning_content") {
		t.Errorf("glm-4.10: reasoning_content present in %v — the [5-9] glob now matches double-digit minors; update the zai.go Rule B comment and this test", q.ReplayFields)
	}
	if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"]; ok {
		t.Errorf("glm-4.10: thinking present — the [5-9] glob now matches double-digit minors; update the zai.go Rule B comment and this test")
	}
}

// TestZAICompatRules_GatewayPrefixResolution pins the OpenRouter
// gateway rule (z-ai/glm-*): legacy max_tokens and reasoning_content
// replay, but tool_stream and thinking absent (vendor extras
// unverified through gateways).
func TestZAICompatRules_GatewayPrefixResolution(t *testing.T) {
	reg := quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))
	q := reg.Resolve("openai-compatible", "z-ai/glm-4.7")

	if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxTokens {
		t.Errorf("z-ai/glm-4.7: TokenField = %v, want max_tokens", q.BehaviourFlags.OpenAI.TokenField)
	}
	if !hasReplayField(q, "reasoning_content") {
		t.Errorf("z-ai/glm-4.7: reasoning_content not in ReplayFields %v", q.ReplayFields)
	}
	// tool_stream and thinking must NOT be present for gateway ids.
	if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"]; ok {
		t.Errorf("z-ai/glm-4.7: tool_stream leaked (vendor extra unverified through gateways)")
	}
	if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"]; ok {
		t.Errorf("z-ai/glm-4.7: thinking leaked (vendor extra unverified through gateways)")
	}
}

// TestZAICompatRules_DoesNotAffectNonGLMModels guards against the
// rules' ModelMatch leaking to non-GLM models on the same registry.
func TestZAICompatRules_DoesNotAffectNonGLMModels(t *testing.T) {
	rules := append(quirks.BuiltinRules(), zai.CompatRules()...)
	reg := quirks.NewRegistry(rules)

	// gpt-4o is not a glm-* model; the rule should not fire.
	q := reg.Resolve("openai-compatible", "gpt-4o")
	if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxCompletionTokens {
		t.Errorf("gpt-4o under zai-registered registry: TokenField = %v, want max_completion_tokens", q.BehaviourFlags.OpenAI.TokenField)
	}
	if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"]; ok {
		t.Errorf("gpt-4o under zai-registered registry: tool_stream leaked into ExtraBodyFields")
	}

	// Sanity-check the rule actually fires for the GLM model.
	q = reg.Resolve("openai-compatible", "glm-4-plus")
	if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxTokens {
		t.Errorf("glm-4-plus: TokenField = %v, want max_tokens (rule should fire)", q.BehaviourFlags.OpenAI.TokenField)
	}
}

// TestCompatRulesReplayFieldsSuffix mirrors
// quirks.TestBuiltinRulesReplayFieldsSuffix for CompatRules(), which
// that test cannot reach: every rule registering a ReplayFields path
// must end its Description with "(threaded)" and use a threadable
// single-segment path.
func TestCompatRulesReplayFieldsSuffix(t *testing.T) {
	const threadedSuffix = "(threaded)"
	for i, rule := range zai.CompatRules() {
		if rule.Apply == nil {
			continue
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
			continue
		}
		if rule.ProviderType != "openai-compatible" {
			t.Errorf("CompatRules()[%d] (%q): unexpected ProviderType %q for a ReplayFields rule", i, rule.Description, rule.ProviderType)
			continue
		}
		if !strings.HasSuffix(rule.Description, threadedSuffix) {
			t.Errorf("CompatRules()[%d] (%q): openai-compatible ReplayFields rule description must end with %q", i, rule.Description, threadedSuffix)
		}
		for _, path := range q.ReplayFields {
			segments, err := quirks.ParseReplayPath(path)
			if err != nil {
				t.Errorf("CompatRules()[%d] (%q): ReplayFields path %q is invalid: %v", i, rule.Description, path, err)
				continue
			}
			if len(segments) != 1 || segments[0].IsArray {
				t.Errorf("CompatRules()[%d] (%q): ReplayFields path %q is not threadable (must be a single non-array segment)", i, rule.Description, path)
			}
		}
	}
}

// TestCompatRulesValidate mirrors quirks.TestBuiltinRulesValidate for
// CompatRules(): every rule must carry a non-empty Description, a
// non-zero LastVerified, and a non-nil Apply.
func TestCompatRulesValidate(t *testing.T) {
	for i, rule := range zai.CompatRules() {
		if rule.Description == "" {
			t.Errorf("CompatRules()[%d]: Description is required", i)
		}
		if rule.LastVerified.IsZero() {
			t.Errorf("CompatRules()[%d] (%q): LastVerified is required", i, rule.Description)
		}
		if rule.Apply == nil {
			t.Errorf("CompatRules()[%d] (%q): Apply is required", i, rule.Description)
		}
		if rule.ProviderType != "openai-compatible" {
			t.Errorf("CompatRules()[%d] (%q): ProviderType = %q, want openai-compatible (Z.ai is a compat profile on that adapter)", i, rule.Description, rule.ProviderType)
		}
	}
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

// TestCompatRuleExtraBodyFieldsNoSecrets mirrors
// quirks.TestBuiltinRulesExtraBodyFieldsNoSecrets for CompatRules():
// ExtraBodyFields is serialised verbatim and RunConfig.Redact() never
// reaches inside a quirks rule, so no value may contain a secret:// ref.
func TestCompatRuleExtraBodyFieldsNoSecrets(t *testing.T) {
	// Representative model id per rule, in declaration order, so
	// Resolve materialises each rule's Apply.
	models := []string{"glm-4-plus", "glm-4.7", "glm-5", "z-ai/glm-4.7"}
	rules := zai.CompatRules()
	if len(models) != len(rules) {
		t.Fatalf("models slice (%d) out of sync with CompatRules() (%d); add a representative model for the new rule", len(models), len(rules))
	}
	for i, rule := range rules {
		q := quirks.NewRegistry([]quirks.Rule{rule}).Resolve("openai-compatible", models[i])
		for k, v := range q.BehaviourFlags.OpenAI.ExtraBodyFields {
			if hit := scanForSecretRefs(v); hit != "" {
				t.Errorf("Z.ai CompatRules()[%d] (%q) ExtraBodyFields[%q] contains a secret:// reference (%q); "+
					"compat rule values are serialised verbatim and bypass RunConfig.Redact()", i, rule.Description, k, hit)
			}
		}
	}
}
