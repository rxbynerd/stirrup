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

// staticBearer mirrors the harness-internal helper of the same name
// without crossing the provider package boundary; the compat/zai
// package lives outside `package provider` so it can't reach
// staticBearer directly.
func staticBearer(s string) func(context.Context) (string, error) {
	return func(_ context.Context) (string, error) {
		return s, nil
	}
}

// hasReplayField reports whether path appears in the resolved
// ReplayFields slice.
func hasReplayField(q quirks.ProviderQuirks, path string) bool {
	for _, p := range q.ReplayFields {
		if p == path {
			return true
		}
	}
	return false
}

// TestZAICompatRule_AppliesTokenFieldAndExtraBody pins the two
// divergences the base Z.ai compat rule enforces: TokenFieldMaxTokens
// (so the wire body uses "max_tokens" rather than the modern
// "max_completion_tokens") and the "tool_stream": true extra body
// field. The test injects the rules into a registry, attaches them to
// an OpenAICompatibleAdapter, fires a request against an httptest
// server, and inspects the captured body — the same end-to-end
// path a real Z.ai run takes.
func TestZAICompatRule_AppliesTokenFieldAndExtraBody(t *testing.T) {
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
	// Inject the Z.ai compat rules on top of BuiltinRules so this
	// test mirrors what core/factory.go does for compatProfile=zai-glm.
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

	// Legacy token field present, modern key absent.
	if _, ok := got["max_tokens"]; !ok {
		t.Errorf("Z.ai body missing 'max_tokens' (legacy field required): %s", body)
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

// TestZAICompatRule_GLM47AppliesThinkingFamilyBody is the end-to-end
// twin of the base-rule test for a thinking-family model. It drives a
// glm-4.7 request through the adapter and asserts the wire body carries
// the base quirks (max_tokens, tool_stream) AND the thinking-family
// extra: a top-level "thinking" object == {"type":"enabled"}. The
// reasoning_content half of the round-trip (outbound threading) is
// exercised by the resolution tests below plus the provider package's
// replay_threading_test.go; here we pin the request-shape contribution.
func TestZAICompatRule_GLM47AppliesThinkingFamilyBody(t *testing.T) {
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
	if _, ok := got["max_tokens"]; !ok {
		t.Errorf("glm-4.7 body missing 'max_tokens' (base rule must still fire): %s", body)
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

// TestZAICompatRules_ThinkingFamilyResolution pins the resolved quirks
// for the thinking-family models against a registry assembled exactly
// as the factory does. These are unit-level resolution assertions that
// complement the end-to-end body test above: they confirm the
// reasoning_content ReplayFields entry and the thinking extra body land
// for glm-4.7 and glm-4.5-air.
func TestZAICompatRules_ThinkingFamilyResolution(t *testing.T) {
	reg := quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))

	for _, model := range []string{"glm-4.7", "glm-4.5-air"} {
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

// TestZAICompatRules_LegacyLineHasNoThinking is the negative pin: the
// hyphenated legacy GLM line (glm-4-plus) must receive ONLY the base
// quirks. The glm-4.[5-9]* glob deliberately uses a dot so it does not
// match the hyphenated id — if the scoping ever leaked (e.g. a glob
// widened to glm-4*), this test fails. A non-thinking model receiving
// a "thinking" body or replaying reasoning_content would be a wire
// regression.
func TestZAICompatRules_LegacyLineHasNoThinking(t *testing.T) {
	reg := quirks.NewRegistry(append(quirks.BuiltinRules(), zai.CompatRules()...))
	q := reg.Resolve("openai-compatible", "glm-4-plus")

	// Base quirks present.
	if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxTokens {
		t.Errorf("glm-4-plus: TokenField = %v, want max_tokens", q.BehaviourFlags.OpenAI.TokenField)
	}
	if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"]; !ok {
		t.Errorf("glm-4-plus: tool_stream missing; base rule should fire")
	}

	// Thinking quirks ABSENT — the 4.5+ scoping must not leak down.
	if hasReplayField(q, "reasoning_content") {
		t.Errorf("glm-4-plus: reasoning_content leaked into ReplayFields %v (legacy line has no thinking mode)", q.ReplayFields)
	}
	if _, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"]; ok {
		t.Errorf("glm-4-plus: thinking leaked into ExtraBodyFields (legacy line has no thinking mode)")
	}
}

// TestZAICompatRules_GatewayPrefixResolution pins the OpenRouter
// gateway rule (z-ai/glm-*). The bare glm-* glob cannot match a
// slash-prefixed id (path.Match's `*` does not cross `/`), so the
// gateway rule supplies the portable quirks itself: legacy max_tokens
// and reasoning_content replay. tool_stream and thinking are
// deliberately ABSENT — vendor extras unverified through gateways.
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

// TestZAICompatRule_DoesNotAffectNonGLMModels guards against the
// rules' ModelMatch leaking to non-GLM models served from the same
// adapter. If an operator multiplexes openai-compatible against
// multiple base URLs with the same registry, a non-GLM model
// resolution must keep the modern token field.
func TestZAICompatRule_DoesNotAffectNonGLMModels(t *testing.T) {
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

// TestCompatRulesReplayFieldsSuffix is the compat-package mirror of
// quirks.TestBuiltinRulesReplayFieldsSuffix: every rule in CompatRules()
// whose Apply registers a ReplayFields path must end its Description
// with "(threaded)", because the openai-compatible adapter threads
// those captures back onto subsequent requests. A rule author who adds
// a ReplayFields entry without the suffix (or declares a non-threadable
// multi-segment path) fails here at build time rather than silently
// regressing trace observability. Compat rules are not part of
// BuiltinRules(), so the quirks_test.go version cannot reach them.
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

// TestCompatRuleExtraBodyFieldsNoSecrets is the per-compat-package
// mirror of quirks.TestBuiltinRulesExtraBodyFieldsNoSecrets: it
// materialises each Z.ai rule into a fresh ProviderQuirks and walks
// every ExtraBodyFields value for the secret:// prefix. The map is
// serialised verbatim into the request body, and RunConfig.Redact()
// never reaches inside a quirks rule, so a secret reference embedded
// here would propagate to every wire request without redaction.
//
// The check walks EVERY rule in CompatRules() (resolving each against a
// representative model id) so a future rule that grows a string-valued
// extra body field — not just the original base rule — is covered. The
// pattern is intended for every compat package to copy: each must
// contribute its own version against its own CompatRules, because the
// quirks_test.go version only walks BuiltinRules() and cannot reach
// compat rules that are not part of the registry default.
func TestCompatRuleExtraBodyFieldsNoSecrets(t *testing.T) {
	// Representative model id per rule, in declaration order: the id
	// must match that rule's glob so Resolve materialises its Apply.
	models := []string{"glm-4-plus", "glm-4.7", "glm-5", "z-ai/glm-4.7"}
	rules := zai.CompatRules()
	if len(models) != len(rules) {
		t.Fatalf("models slice (%d) out of sync with CompatRules() (%d); add a representative model for the new rule", len(models), len(rules))
	}
	for i, rule := range rules {
		q := quirks.NewRegistry([]quirks.Rule{rule}).Resolve("openai-compatible", models[i])
		for k, v := range q.BehaviourFlags.OpenAI.ExtraBodyFields {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if strings.Contains(s, "secret://") {
				t.Errorf("Z.ai CompatRules()[%d] (%q) ExtraBodyFields[%q] = %q contains a secret:// reference; "+
					"compat rule values are serialised verbatim and bypass RunConfig.Redact()", i, rule.Description, k, s)
			}
		}
	}
}
