package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NoError(t, err)
	for range ch {
	}
	body := <-capturedBody

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))
	tools := req["tools"].([]any)
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	function := tool["function"].(map[string]any)
	assert.Equal(t, true, function["strict"], "tool should be strict")

	schema := function["parameters"].(map[string]any)
	assert.Equal(t, false, schema["additionalProperties"], "top-level object should close additional properties")
	required := schema["required"].([]any)
	assert.Len(t, required, 3, "every property should be required: %v", required)

	props := schema["properties"].(map[string]any)
	_, pathIsArray := props["path"].(map[string]any)["type"].([]any)
	assert.False(t, pathIsArray, "required path should keep a scalar type")

	for _, field := range []string{"limit", "depth"} {
		t.Run(field+" is nullable", func(t *testing.T) {
			typ := props[field].(map[string]any)["type"]
			typeList, ok := typ.([]any)
			require.True(t, ok, "optional %s should use array type form, got %v", field, typ)
			require.Contains(t, typeList, "null")
		})
	}
}

// TestOpenAIStrictMode_FailsClosedOnUnsupportedSchema pins the
// fail-closed contract: when strict-mode normalisation rejects a
// tool's schema, the Stream call returns an error BEFORE any HTTP
// request is sent. The error must name the tool and the offending
// field path so the operator can locate it.
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
	require.Error(t, err)
	require.ErrorContains(t, err, "bad_tool", "error should name the tool")
	require.ErrorContains(t, err, "oneOf", "error should name the offending keyword")
	assert.False(t, hit, "strict-mode lint should fail before any wire request")
}

// TestOpenAIStrictMode_LintErrorDoesNotLeakDescriptionOrEnum pins the
// privacy contract for the OpenAI strict-mode path: the fail-closed
// error message must NOT carry the schema's description or enum
// content, even when those fields exist in the rejected schema.
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
	require.Error(t, err)
	msg := err.Error()

	for _, tc := range []struct {
		name string
		leak string
	}{
		{name: "enum value", leak: "LEAKABLE-ENUM-VALUE"},
		{name: "description", leak: "LEAKABLE-DESCRIPTION-TEXT"},
	} {
		t.Run("does not leak "+tc.name, func(t *testing.T) {
			require.NotContains(t, msg, tc.leak)
		})
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
		require.NoError(t, err, "Stream %d", i)
		for range ch {
		}
	}

	misses := adapter.strictSchemas.Misses.Load()
	hits := adapter.strictSchemas.Hits.Load()
	assert.EqualValues(t, 1, misses, "one normalisation across two turns")
	assert.EqualValues(t, 1, hits, "second turn should hit the cache")
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
		require.NoError(t, err, "Stream %s", model)
		for range ch {
		}
	}

	misses := adapter.strictSchemas.Misses.Load()
	assert.EqualValues(t, 2, misses, "each model should be normalised separately")
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
	require.NoError(t, err)
	for range ch {
	}
	body := <-capturedBody

	assert.NotContains(t, string(body), `"strict":true`, "non-strict model should not set strict: %s", body)
	// The cache should not be touched when strict mode is off.
	misses := adapter.strictSchemas.Misses.Load()
	assert.Zero(t, misses, "non-strict path should not consult the cache")
}

// TestOpenAIStrictMode_ErrorsAreNotCached pins the documented contract
// that normalisation failures bypass the cache entirely. A schema whose
// strict-mode lint fails (here: contains `oneOf`) must re-surface the
// error on every Stream call so an operator sees the rejection in logs
// at every turn rather than once at first occurrence. Neither Hits nor
// Misses moves on a failing call: Hits requires a lookup-hit (the
// failing schema is never stored, so subsequent lookups also miss),
// and Misses is only incremented on a successful normalisation.
func TestOpenAIStrictMode_ErrorsAreNotCached(t *testing.T) {
	// HTTP server must NOT be reached — strict-mode lint fails before
	// any wire request is issued, on every turn.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = strictRegistryFor("gpt-4o-mini")

	params := types.StreamParams{
		Model:    "gpt-4o-mini",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "bad_tool",
				Description: "uses oneOf which strict mode cannot express",
				InputSchema: json.RawMessage(`{"oneOf":[{"type":"string"},{"type":"integer"}]}`),
			},
		},
	}

	// Two consecutive Stream calls with the same failing schema.
	// Both must surface the lint error rather than the second observing
	// a cached failure.
	for i := 0; i < 2; i++ {
		_, err := adapter.Stream(context.Background(), params)
		require.Error(t, err, "Stream %d", i)
		require.ErrorContains(t, err, "oneOf", "Stream %d should name the offending keyword", i)
	}
	assert.False(t, hit, "strict-mode lint should fail before any wire request")
	// Both counters must stay at zero: the failing schema is never
	// stored (so no subsequent lookup-hit is possible), and Misses
	// only moves on a successful normalisation.
	hits := adapter.strictSchemas.Hits.Load()
	assert.Zero(t, hits, "failing schema must not produce cache hits")
	misses := adapter.strictSchemas.Misses.Load()
	assert.Zero(t, misses, "failing schema must not be memoised")
}

// TestOpenAIStrictMode_ConcurrentFirstMiss pins the singleflight
// behaviour of the strict-schema cache: N goroutines calling Stream
// simultaneously on the same adapter, same model, same schema produce
// exactly one Misses increment and N-1 Hits, rather than each
// goroutine independently observing the initial miss and double-
// counting it. Runs with -race to also catch a regression in the lock
// ordering inside computeAndStore.
func TestOpenAIStrictMode_ConcurrentFirstMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = strictRegistryFor("gpt-4o-mini")

	params := types.StreamParams{
		Model:    "gpt-4o-mini",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "search",
				Description: "search",
				InputSchema: json.RawMessage(strictTestSchema),
			},
		},
	}

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			ch, err := adapter.Stream(context.Background(), params)
			if !assert.NoError(t, err) {
				return
			}
			for range ch {
			}
		}()
	}
	// Release all goroutines at once to maximise the chance of a
	// concurrent first-miss against the empty cache.
	close(start)
	wg.Wait()

	misses := adapter.strictSchemas.Misses.Load()
	hits := adapter.strictSchemas.Hits.Load()
	assert.EqualValues(t, 1, misses, "one logical first-miss across %d goroutines", N)
	assert.EqualValues(t, N-1, hits, "all but the first-miss goroutine should observe the cached entry")
}
