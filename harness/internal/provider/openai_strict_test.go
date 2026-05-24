package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// strictRegistryFor returns a registry that pins StrictMode=true for
// the given model under the openai-compatible provider type, so tests
// can construct adapter wire bodies without depending on any specific
// production rule. Using a synthetic rule avoids re-pinning fixtures
// every time the BuiltinRules() strict-mode surface grows or shrinks.
func strictRegistryFor(model string) *quirks.Registry {
	return quirks.NewRegistry([]quirks.Rule{
		{
			ProviderType: "openai-compatible",
			ModelMatch:   model,
			Description:  "test: pin strict mode for " + model,
			LastVerified: quirks.Date("2026-05-24"),
			Apply: func(q *quirks.ProviderQuirks) {
				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
	})
}

// strictTestSchema is a representative tool input schema with one
// required and two optional fields. Used in multiple tests below to
// drive the strict-mode rewrite.
const strictTestSchema = `{
	"type": "object",
	"properties": {
		"path":  {"type": "string"},
		"limit": {"type": "integer"},
		"depth": {"type": "integer"}
	},
	"required": ["path"]
}`

// TestOpenAIStrictMode_WireBodyShape pins the strict-mode wire shape
// end-to-end: build an OpenAICompatibleAdapter with a quirks rule
// pinning StrictMode for the test model, drive a Stream call, and
// inspect the outbound JSON body.
//
// Assertions:
//   - the tool entry carries `strict: true`
//   - every property in the rewritten schema appears in `required`
//   - the originally-optional fields are nullable (`["type","null"]`)
//   - `additionalProperties` is false at the top-level object
func TestOpenAIStrictMode_WireBodyShape(t *testing.T) {
	capturedBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = strictRegistryFor("gpt-4o-mini")

	params := types.StreamParams{
		Model:     "gpt-4o-mini",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "search",
				Description: "search",
				InputSchema: json.RawMessage(strictTestSchema),
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := adapter.Stream(ctx, params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	body := <-capturedBody

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	function := tool["function"].(map[string]any)
	if function["strict"] != true {
		t.Errorf("function.strict = %v, want true", function["strict"])
	}
	params2 := function["parameters"].(map[string]any)
	if params2["additionalProperties"] != false {
		t.Errorf("params.additionalProperties = %v, want false", params2["additionalProperties"])
	}
	required := params2["required"].([]any)
	if len(required) != 3 {
		t.Errorf("required length = %d, want 3 (every property): %v", len(required), required)
	}
	props := params2["properties"].(map[string]any)
	// path was required → not nullable.
	if _, isArr := props["path"].(map[string]any)["type"].([]any); isArr {
		t.Errorf("path.type should remain a scalar (required), got array")
	}
	// limit + depth optional → nullable-wrapped.
	for _, k := range []string{"limit", "depth"} {
		typ := props[k].(map[string]any)["type"]
		arr, ok := typ.([]any)
		if !ok {
			t.Errorf("%s.type = %v, want array form", k, typ)
			continue
		}
		hasNull := false
		for _, v := range arr {
			if s, ok := v.(string); ok && s == "null" {
				hasNull = true
			}
		}
		if !hasNull {
			t.Errorf("%s.type = %v, want it to contain 'null'", k, typ)
		}
	}
}

// TestOpenAIStrictMode_FailsClosedOnUnsupportedSchema pins design §5:
// when strict-mode normalisation rejects a tool's schema, the Stream
// call returns an error BEFORE any HTTP request is sent. The error
// must name the tool and the offending field path so the operator can
// locate it.
func TestOpenAIStrictMode_FailsClosedOnUnsupportedSchema(t *testing.T) {
	// Whether the HTTP server was reached. Set true by the handler; the
	// test fails if any request lands.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = strictRegistryFor("gpt-4o-mini")

	params := types.StreamParams{
		Model:     "gpt-4o-mini",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "bad_tool",
				Description: "uses oneOf which strict mode cannot express",
				InputSchema: json.RawMessage(`{"oneOf":[{"type":"string"},{"type":"integer"}]}`),
			},
		},
	}
	_, err := adapter.Stream(context.Background(), params)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad_tool") {
		t.Errorf("error %q does not name the tool", err)
	}
	if !strings.Contains(err.Error(), "oneOf") {
		t.Errorf("error %q does not name the offending keyword", err)
	}
	if hit {
		t.Errorf("HTTP server was reached: strict-mode lint should fail before any wire request")
	}
}

// TestOpenAIStrictMode_LintErrorDoesNotLeakDescriptionOrEnum pins the
// privacy contract from #228 §5 for the OpenAI strict-mode path. The
// fail-closed error message must NOT carry the schema's description
// or enum content, even when those fields exist in the rejected
// schema.
func TestOpenAIStrictMode_LintErrorDoesNotLeakDescriptionOrEnum(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), "http://invalid.test", OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = strictRegistryFor("gpt-4o-mini")

	params := types.StreamParams{
		Model:    "gpt-4o-mini",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "secret_tool",
				Description: "uses a typeless optional",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"properties": {
						"x": {
							"enum": ["LEAKABLE-ENUM-VALUE"],
							"description": "LEAKABLE-DESCRIPTION-TEXT"
						}
					}
				}`),
			},
		},
	}
	_, err := adapter.Stream(context.Background(), params)
	if err == nil {
		t.Fatalf("expected error")
	}
	msg := err.Error()
	for _, leak := range []string{"LEAKABLE-ENUM-VALUE", "LEAKABLE-DESCRIPTION-TEXT"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error %q leaks %q", msg, leak)
		}
	}
}

// TestOpenAIStrictMode_CacheHitOnRepeatedTurns pins the per-Stream
// (per-adapter) cache: two Stream calls in the same run with the same
// tool schema should walk the normaliser exactly once.
//
// The Hits/Misses counters on the adapter's strictSchemas cache are
// the cheapest observability point that does not require a global
// counter; the test inspects them directly because the cache is an
// adapter-private field with no public accessor (intentional —
// production code does not need to read these).
func TestOpenAIStrictMode_CacheHitOnRepeatedTurns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = strictRegistryFor("gpt-4o-mini")

	params := types.StreamParams{
		Model:     "gpt-4o-mini",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "search",
				Description: "search",
				InputSchema: json.RawMessage(strictTestSchema),
			},
		},
	}

	// Two consecutive Stream calls in the same adapter lifetime.
	for i := 0; i < 2; i++ {
		ch, err := adapter.Stream(context.Background(), params)
		if err != nil {
			t.Fatalf("Stream %d: %v", i, err)
		}
		for range ch {
		}
	}

	misses := adapter.strictSchemas.Misses.Load()
	hits := adapter.strictSchemas.Hits.Load()
	if misses != 1 {
		t.Errorf("misses = %d, want 1 (one normalisation across two turns)", misses)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1 (second turn hit the cache)", hits)
	}
}

// TestOpenAIStrictMode_CacheKeyIncludesModel pins that a model switch
// inside a single adapter lifetime busts the cache: a dynamic-router
// run that swaps models turn-to-turn must re-normalise per model.
//
// The synthetic registry pins strict-mode for both "gpt-4o-mini" and
// "gpt-5-nano" so both Stream calls hit the normaliser. The cache's
// (model, tool-name, schema-hash) key separates them.
func TestOpenAIStrictMode_CacheKeyIncludesModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = quirks.NewRegistry([]quirks.Rule{
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-*",
			Description:  "test: strict-mode for any gpt model",
			LastVerified: quirks.Date("2026-05-24"),
			Apply: func(q *quirks.ProviderQuirks) {
				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
	})

	toolDef := []types.ToolDefinition{
		{
			Name:        "search",
			Description: "search",
			InputSchema: json.RawMessage(strictTestSchema),
		},
	}

	for _, model := range []string{"gpt-4o-mini", "gpt-5-nano"} {
		params := types.StreamParams{
			Model:    model,
			Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
			Tools:    toolDef,
		}
		ch, err := adapter.Stream(context.Background(), params)
		if err != nil {
			t.Fatalf("Stream %s: %v", model, err)
		}
		for range ch {
		}
	}

	misses := adapter.strictSchemas.Misses.Load()
	if misses != 2 {
		t.Errorf("misses = %d, want 2 (each model normalised separately)", misses)
	}
}

// TestOpenAIStrictMode_DoesNotFireForNonStrictModel pins the negative
// case: a model that does not match any strict-mode rule produces a
// wire body without `strict: true` and without the property-rewrite.
func TestOpenAIStrictMode_DoesNotFireForNonStrictModel(t *testing.T) {
	capturedBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	// Default registry — no rule matches gpt-4o, so strict mode stays off.

	params := types.StreamParams{
		Model:    "gpt-4o",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "search",
				Description: "search",
				InputSchema: json.RawMessage(strictTestSchema),
			},
		},
	}
	ch, err := adapter.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	body := <-capturedBody

	if strings.Contains(string(body), `"strict":true`) {
		t.Errorf("non-strict model: body contains strict=true: %s", body)
	}
	// Misses should be 0 — the cache should not be touched when strict
	// mode is off.
	if misses := adapter.strictSchemas.Misses.Load(); misses != 0 {
		t.Errorf("misses = %d, want 0 (non-strict path should not consult cache)", misses)
	}
}
