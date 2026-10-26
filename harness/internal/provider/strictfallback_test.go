package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// optionalAnyOfSchema is the shape FastMCP and Pydantic emit for an
// Optional[int] parameter: an anyOf with a null branch, which strict mode
// cannot express.
const optionalAnyOfSchema = `{"type":"object","properties":{"limit":{"anyOf":[{"type":"integer"},{"type":"null"}]}}}`

const strictFallbackWarnMsg = "strict-mode schema lint rejected tool; sending it with strict:false"

func strictFallbackTools() []types.ToolDefinition {
	return []types.ToolDefinition{
		{Name: "search", Description: "search", InputSchema: json.RawMessage(strictTestSchema)},
		{Name: "mcp_lookup", Description: "lookup", InputSchema: json.RawMessage(optionalAnyOfSchema)},
	}
}

// strictFallbackWarns returns the strict-fallback WARN records in the
// JSON log output.
func strictFallbackWarns(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if rec["msg"] == strictFallbackWarnMsg {
			out = append(out, rec)
		}
	}
	return out
}

// bodyCapturingServer records every request body and answers 500, which
// ends the stream without a response parser being involved.
func bodyCapturingServer(t *testing.T) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

// wireTool is one tools[] entry as sent, flattened across the Chat
// Completions (nested under "function") and Responses (flat) shapes.
type wireTool struct {
	Strict     any
	HasStrict  bool
	Parameters map[string]any
}

func wireTools(t *testing.T, body []byte) map[string]wireTool {
	t.Helper()
	var req struct {
		Tools []map[string]any `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(body, &req))
	out := map[string]wireTool{}
	for _, tool := range req.Tools {
		def := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			def = fn
		}
		name, _ := def["name"].(string)
		params, _ := def["parameters"].(map[string]any)
		strict, has := def["strict"]
		out[name] = wireTool{Strict: strict, HasStrict: has, Parameters: params}
	}
	return out
}

func TestTranslateTools_NonNormalisableSchemaFallsBackToNonStrict(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tools, fallbacks, _, err := translateTools(strictFallbackTools(), true, false, "gpt-5", newStrictSchemaCache())
	must.NoError(err)
	must.Len(tools, 2)

	check.True(*tools[0].Function.Strict, "clean tool stays strict")
	check.JSONEq(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":["integer","null"]},"depth":{"type":["integer","null"]}},"required":["depth","limit","path"],"additionalProperties":false}`,
		string(tools[0].Function.Parameters))

	must.NotNil(tools[1].Function.Strict, "fallback tool carries an explicit strict flag")
	check.False(*tools[1].Function.Strict)
	check.JSONEq(optionalAnyOfSchema, string(tools[1].Function.Parameters), "fallback tool keeps its original schema")

	must.Len(fallbacks, 1)
	check.Equal("mcp_lookup", fallbacks[0].tool)
	check.Equal("properties.limit", fallbacks[0].path)
}

func TestTranslateToolsResponses_NonNormalisableSchemaFallsBackToNonStrict(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tools, fallbacks, _, err := translateToolsResponses(strictFallbackTools(), true, false, false, "gpt-5", newStrictSchemaCache())
	must.NoError(err)
	must.Len(tools, 2)

	must.NotNil(tools[0].Strict)
	check.True(*tools[0].Strict, "clean tool stays strict")

	must.NotNil(tools[1].Strict, "an omitted strict means strict-when-possible on this surface, so false is explicit")
	check.False(*tools[1].Strict)
	check.JSONEq(optionalAnyOfSchema, string(tools[1].Parameters), "fallback tool keeps its original schema")

	must.Len(fallbacks, 1)
	check.Equal("mcp_lookup", fallbacks[0].tool)
	check.Equal("properties.limit", fallbacks[0].path)
}

func TestTranslateTools_FallbackToolFoldsExamplesIntoSchema(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tools := strictFallbackTools()
	tools[1].Presentation = &types.ToolPresentation{InputExamples: []json.RawMessage{json.RawMessage(`{"limit":3}`)}}

	chat, _, _, err := translateTools(tools, true, true, "gpt-5", nil)
	must.NoError(err)
	check.Contains(string(chat[1].Function.Parameters), `"examples"`, "fallback tool is non-strict, so examples are folded")
	check.NotContains(string(chat[0].Function.Parameters), `"examples"`)

	responses, _, _, err := translateToolsResponses(tools, true, true, false, "gpt-5", nil)
	must.NoError(err)
	check.Contains(string(responses[1].Parameters), `"examples"`)
}

func TestTranslateTools_MalformedSchemaStillFails(t *testing.T) {
	must := require.New(t)
	cache := newStrictSchemaCache()
	bad := []types.ToolDefinition{{Name: "broken", InputSchema: json.RawMessage(`{"type":`)}}

	_, _, _, err := translateTools(bad, true, false, "gpt-5", cache)
	must.ErrorContains(err, "broken")
	_, _, _, err = translateToolsResponses(bad, true, false, false, "gpt-5", cache)
	must.ErrorContains(err, "broken")
	must.Zero(cache.Misses.Load(), "a schema that does not parse is not cached")
}

func TestTranslateTools_StructurallyInvalidSchemaStillFails(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"properties not an object", `{"type":"object","properties":["a"]}`},
		{"required not an array", `{"type":"object","properties":{"a":{"type":"string"}},"required":"a"}`},
		{"required element not a string", `{"type":"object","properties":{"a":{"type":"string"}},"required":[1]}`},
		{"type neither string nor array", `{"type":"object","properties":{"a":{"type":5}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cache := newStrictSchemaCache()
			bad := []types.ToolDefinition{{Name: "broken", InputSchema: json.RawMessage(tc.schema)}}

			_, _, _, err := translateTools(bad, true, false, "gpt-5", cache)
			must.ErrorContains(err, "broken")
			_, _, _, err = translateToolsResponses(bad, true, false, false, "gpt-5", cache)
			must.ErrorContains(err, "broken")
			must.Zero(cache.Misses.Load(), "a malformed schema is not cached")
		})
	}
}

func TestStrictFallback_ConcurrentFirstMissReportsOnce(t *testing.T) {
	cases := []struct {
		name      string
		translate func(cache *strictSchemaCache) ([]*strictSchemaError, error)
	}{
		{"chat", func(cache *strictSchemaCache) ([]*strictSchemaError, error) {
			_, fallbacks, _, err := translateTools(strictFallbackTools(), true, false, "gpt-5", cache)
			return fallbacks, err
		}},
		{"responses", func(cache *strictSchemaCache) ([]*strictSchemaError, error) {
			_, fallbacks, _, err := translateToolsResponses(strictFallbackTools(), true, false, false, "gpt-5", cache)
			return fallbacks, err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cache := newStrictSchemaCache()
			const workers = 16
			reported := make([]int, workers)
			errs := make([]error, workers)
			var wg sync.WaitGroup
			for i := range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var fallbacks []*strictSchemaError
					fallbacks, errs[i] = tc.translate(cache)
					reported[i] = len(fallbacks)
				}()
			}
			wg.Wait()

			total := 0
			for i := range workers {
				must.NoError(errs[i])
				total += reported[i]
			}
			check.Equal(1, total, "exactly one concurrent caller reports the first miss")
			check.Equal(uint64(2), cache.Misses.Load(), "one miss per tool")
		})
	}
}

func TestStrictFallback_WarnTruncatesLongSchemaPath(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var logs bytes.Buffer
	longPath := "properties." + strings.Repeat("é", 1000)

	warnStrictFallbacks(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), "openai-compatible", "gpt-5",
		[]*strictSchemaError{{tool: "t", path: longPath, reason: "strict mode does not support the \"anyOf\" keyword"}})

	warns := strictFallbackWarns(t, &logs)
	must.Len(warns, 1)
	path, _ := warns[0]["schema.path"].(string)
	check.Len([]rune(path), maxStrictWarnPathRunes+len("..."), "path is cut to the rune bound plus the ellipsis")
	check.True(strings.HasSuffix(path, "..."))
}

func TestStrictFallback_SentAsNonStrictOnBothSurfaces(t *testing.T) {
	cases := []struct {
		name string
		new  func(url string, logger *slog.Logger) ProviderAdapter
	}{
		{"chat gpt-5", func(url string, logger *slog.Logger) ProviderAdapter {
			a := NewOpenAICompatibleAdapter(staticBearer("k"), url, OpenAIAuthConfig{}, RetryPolicy{})
			a.Logger = logger
			return a
		}},
		{"responses gpt-5", func(url string, logger *slog.Logger) ProviderAdapter {
			a := NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
			a.Logger = logger
			return a
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			srv, bodies := bodyCapturingServer(t)
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))

			params := effortParams("gpt-5", "", false)
			params.Tools = strictFallbackTools()
			_, _ = tc.new(srv.URL, logger).Stream(context.Background(), params)

			sent := bodies()
			must.Len(sent, 1, "the request is sent rather than failed before send")
			wire := wireTools(t, sent[0])
			must.Len(wire, 2)
			check.Equal(true, wire["search"].Strict, "clean tool is strict")
			check.Equal(false, wire["mcp_lookup"].Strict, "non-normalisable tool is sent with strict:false")
			check.True(wire["mcp_lookup"].HasStrict, "strict:false is explicit")
			anyOf := wire["mcp_lookup"].Parameters["properties"].(map[string]any)["limit"].(map[string]any)
			check.Contains(anyOf, "anyOf", "the original schema is sent")

			warns := strictFallbackWarns(t, &logs)
			must.Len(warns, 1)
			check.Equal("WARN", warns[0]["level"])
			check.Equal("mcp_lookup", warns[0]["tool.name"])
			check.Equal("properties.limit", warns[0]["schema.path"])
			check.Equal("gpt-5", warns[0]["provider.model"])
			check.Contains(warns[0]["reason"], "anyOf")
		})
	}
}

func TestStrictFallback_WarnsOncePerTool(t *testing.T) {
	cases := []struct {
		name string
		new  func(url string, logger *slog.Logger) (ProviderAdapter, *strictSchemaCache)
	}{
		{"chat", func(url string, logger *slog.Logger) (ProviderAdapter, *strictSchemaCache) {
			a := NewOpenAICompatibleAdapter(staticBearer("k"), url, OpenAIAuthConfig{}, RetryPolicy{})
			a.Logger = logger
			return a, a.strictSchemas
		}},
		{"responses", func(url string, logger *slog.Logger) (ProviderAdapter, *strictSchemaCache) {
			a := NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
			a.Logger = logger
			return a, a.strictSchemas
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			srv, bodies := bodyCapturingServer(t)
			var logs bytes.Buffer
			adapter, cache := tc.new(srv.URL, slog.New(slog.NewJSONHandler(&logs, nil)))

			params := effortParams("gpt-5", "", false)
			params.Tools = strictFallbackTools()
			for range 3 {
				_, _ = adapter.Stream(context.Background(), params)
			}

			must.Len(bodies(), 3)
			check.Len(strictFallbackWarns(t, &logs), 1, "one WARN per tool across turns")
			check.Equal(uint64(2), cache.Misses.Load(), "the fallback is recorded in the cache beside the clean tool")
			check.Equal(uint64(4), cache.Hits.Load(), "later turns hit the cache for both tools")

			wire := wireTools(t, bodies()[2])
			check.Equal(false, wire["mcp_lookup"].Strict, "later turns keep sending the fallback tool non-strict")
		})
	}
}

func TestStrictFallback_WarnsAgainWhenSchemaChanges(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cache := newStrictSchemaCache()
	tools := strictFallbackTools()

	_, first, _, err := translateTools(tools, true, false, "gpt-5", cache)
	must.NoError(err)
	must.Len(first, 1)

	tools[1].InputSchema = json.RawMessage(`{"type":"object","properties":{"limit":{"oneOf":[{"type":"integer"}]}}}`)
	_, second, _, err := translateTools(tools, true, false, "gpt-5", cache)
	must.NoError(err)
	check.Len(second, 1, "a changed schema is a new cache key and is reported again")
}

func TestStrictFallback_WarnDoesNotLeakDescriptionOrEnum(t *testing.T) {
	must := require.New(t)
	srv, _ := bodyCapturingServer(t)
	var logs bytes.Buffer
	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = slog.New(slog.NewJSONHandler(&logs, nil))

	params := effortParams("gpt-5", "", false)
	params.Tools = []types.ToolDefinition{{
		Name:        "secret_tool",
		Description: "uses a typeless optional",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"enum":["LEAKABLE-ENUM-VALUE"],"description":"LEAKABLE-DESCRIPTION-TEXT"}}}`),
	}}
	_, _ = adapter.Stream(context.Background(), params)

	must.Len(strictFallbackWarns(t, &logs), 1)
	for _, leak := range []string{"LEAKABLE-ENUM-VALUE", "LEAKABLE-DESCRIPTION-TEXT"} {
		must.NotContains(logs.String(), leak)
	}
}
