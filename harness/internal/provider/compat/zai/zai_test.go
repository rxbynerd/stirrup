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

// TestZAICompatRule_GLM47AppliesThinkingFamilyBody is the end-to-end
// twin of the base-rule test for a thinking-family model. It drives a
// glm-4.7 request through the adapter and asserts the wire body carries
// the base quirks (max_tokens, tool_stream) AND the thinking-family
// extra: a top-level "thinking" object == {"type":"enabled"}. The
// reasoning_content half of the round-trip (outbound threading) is
// exercised by the resolution tests below plus the provider package's
// replay_threading_test.go; here we pin the request-shape contribution.
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

// glm47ReasoningSSE is a synthetic GLM-4.7 streaming response that
// mirrors the deepseek-v4-flash fixture's wire shape exactly:
// reasoning_content is streamed as a sibling of content on each
// assistant delta, the docs assert GLM uses the identical
// delta.reasoning_content shape. The two reasoning pieces concatenate
// to "Scanning the request. Choosing an answer." — the value the
// capture-and-flatten path must surface on message_complete.
const glm47ReasoningSSE = "data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Scanning the request. \"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Choosing an answer.\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Done.\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"glm-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":8}}\n\n" +
	"data: [DONE]\n\n"

// zaiTestAdapter builds an OpenAICompatibleAdapter pointed at srvURL
// with a registry assembled exactly as core/factory.go does for
// compatProfile=zai-glm (BuiltinRules + zai.CompatRules()).
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

// TestZAICompatRules_GLM47CapturesReasoningContent is the GLM-scoped
// parse-side capture test. It drives adapter.Stream() with Model:
// "glm-4.7" against a server emitting the GLM reasoning SSE shape and
// asserts message_complete.ReplayFields["reasoning_content"] accumulates
// to the concatenated pieces. The generic deepseek capture test proves
// the walker; this proves the zai compat rule actually registers the
// reasoning_content path so the walker runs for a GLM model.
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

// TestZAICompatRules_GLM47TwoTurnRoundTrip is the GLM-scoped end-to-end
// round-trip — the test the spec deviation in review wave 1 flagged as
// missing. The existing replay_threading_test.go TwoTurnRoundTrip
// resolves from DefaultRegistry() (BuiltinRules), so it proves
// DeepSeek's BUILTIN reasoning_content path, not the zai COMPAT-INJECTED
// path. Here the registry is assembled the way the factory does for
// compatProfile=zai-glm, so the assertion is that the compat injection
// closes the loop: turn 1 streams a glm-4.7 response carrying
// reasoning_content, the captured state is attached to the assistant
// message exactly as the agentic loop does, and the turn-2 request body
// must carry reasoning_content as a top-level assistant-message key.
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

	// Turn 1: stream and assemble the assistant message the way
	// streamEventsToResult + appendAssistantContent do — text deltas
	// concatenate into one block; message_complete supplies the replay
	// state.
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

	// Turn 2: the request built from the updated history must thread the
	// captured value back onto the assistant wire message.
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

// TestZAICompatRules_ThinkingFamilyResolution pins the resolved quirks
// for the thinking-family models against a registry assembled exactly
// as the factory does. These are unit-level resolution assertions that
// complement the end-to-end body test above: they confirm the
// reasoning_content ReplayFields entry and the thinking extra body land
// for both glob families — Rule B (glm-4.[5-9]*: glm-4.7, glm-4.5-air)
// and Rule C (glm-5*: glm-5, glm-5.1). Rule C shares applyThinkingFamily
// with Rule B, so covering it here catches a future divergence in the
// glm-5 line that the shared helper alone would not surface.
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

// TestZAICompatRules_LegacyLineHasNoThinking is the negative pin: every
// hyphenated legacy GLM id (glm-4-plus, glm-4-flash, glm-4-air) must
// receive ONLY the base quirks. The glm-4.[5-9]* glob deliberately uses
// a dot so it does not match the hyphenated ids — if the scoping ever
// leaked (e.g. a glob widened to glm-4*), this test fails for whichever
// id regressed. A non-thinking model receiving a "thinking" body or
// replaying reasoning_content would be a wire regression.
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

// TestZAICompatRules_GLM410IsAKnownGap pins a documented limitation
// rather than a desired behaviour: the glm-4.[5-9]* char class stops at
// 9, so a future glm-4.10 (if Z.ai ever ships double-digit minors)
// would NOT match Rule B and would receive only the base quirks — no
// thinking, no reasoning_content replay. This is the conservative
// default (do not speculatively widen the glob), and the test makes the
// gap visible: when glm-4.10 becomes real, this test starts failing the
// day the rule is fixed, signalling the comment in zai.go must be
// updated alongside it.
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

// TestCompatRulesValidate is the per-compat-package mirror of
// quirks.TestBuiltinRulesValidate: every rule in CompatRules() must
// carry a non-empty Description (operators read it in the introspection
// subcommand), a non-zero LastVerified (the staleness signal), and a
// non-nil Apply (a rule that does nothing is a registration bug). The
// quirks_test.go version walks only BuiltinRules() and cannot reach the
// operator-gated compat rules, so each compat package owns this check.
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
// the first string value containing a secret:// reference, or "" if
// none. ExtraBodyFields is serialised verbatim into the request body
// and RunConfig.Redact() never reaches inside a quirks rule, so any
// nested string carrying a secret reference would leak unredacted — the
// thinking object's {"type":"enabled"} map is the first nested value, so
// the walk must descend, not just check top-level strings.
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

// TestCompatRuleExtraBodyFieldsNoSecrets is the per-compat-package
// mirror of quirks.TestBuiltinRulesExtraBodyFieldsNoSecrets: it
// materialises each Z.ai rule into a fresh ProviderQuirks and walks
// every ExtraBodyFields value for the secret:// prefix. The map is
// serialised verbatim into the request body, and RunConfig.Redact()
// never reaches inside a quirks rule, so a secret reference embedded
// here would propagate to every wire request without redaction.
//
// The check walks EVERY rule in CompatRules() (resolving each against a
// representative model id) and recurses into nested maps/slices so the
// thinking object's contents are covered, not just top-level strings.
// The pattern is intended for every compat package to copy: each must
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
			if hit := scanForSecretRefs(v); hit != "" {
				t.Errorf("Z.ai CompatRules()[%d] (%q) ExtraBodyFields[%q] contains a secret:// reference (%q); "+
					"compat rule values are serialised verbatim and bypass RunConfig.Redact()", i, rule.Description, k, hit)
			}
		}
	}
}
