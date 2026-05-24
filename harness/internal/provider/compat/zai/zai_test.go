package zai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestZAICompatRule_AppliesTokenFieldAndExtraBody pins the two
// divergences the Z.ai compat profile enforces: TokenFieldMaxTokens
// (so the wire body uses "max_tokens" rather than the modern
// "max_completion_tokens") and the "tool_stream": true extra body
// field. The test injects the rule into a registry, attaches it to
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
	// Inject the Z.ai compat rule on top of BuiltinRules so this
	// test mirrors what core/factory.go does for compatProfile=zai-glm.
	rules := append(quirks.BuiltinRules(), zai.CompatRule())
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

// TestZAICompatRule_DoesNotAffectNonGLMModels guards against the
// rule's ModelMatch leaking to non-GLM models served from the same
// adapter. If an operator multiplexes openai-compatible against
// multiple base URLs with the same registry, a non-GLM model
// resolution must keep the modern token field.
func TestZAICompatRule_DoesNotAffectNonGLMModels(t *testing.T) {
	rules := append(quirks.BuiltinRules(), zai.CompatRule())
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
