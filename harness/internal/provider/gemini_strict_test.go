package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// geminiParams returns single-turn stream params for model with the given tools.
func geminiParams(model string, tools ...types.ToolDefinition) types.StreamParams {
	return types.StreamParams{
		Model:    model,
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:    tools,
	}
}

// TestGeminiSchemaLint_FailsClosedOnUnsupportedFeature pins that
// BuildGenerateContentRequest errors before marshalling when a tool's
// schema uses a JSON Schema keyword the resolved quirks list as
// unsupported; the error names the tool and offending field path.
func TestGeminiSchemaLint_FailsClosedOnUnsupportedFeature(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-3-pro")
	require.NotEmpty(t, q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures,
		"gemini-3 rule should pin SchemaUnsupportedFeatures")

	params := geminiParams("gemini-3-pro", types.ToolDefinition{
		Name:        "name_check",
		Description: "uses pattern which gemini-3 rejects",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","pattern":"^[a-z]+$"}},"required":["name"]}`),
	})

	_, _, err := BuildGenerateContentRequest(params, nil, q)
	require.Error(t, err)

	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "names the tool", want: "name_check"},
		{name: "names the offending keyword", want: "pattern"},
		{name: "names the offending field path", want: "properties.name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestGeminiSchemaLint_PassesCleanSchema pins that a schema using only
// Gemini-supported keywords clears the lint and proceeds to ConvertSchema.
func TestGeminiSchemaLint_PassesCleanSchema(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-3-pro")
	params := geminiParams("gemini-3-pro", types.ToolDefinition{
		Name:        "search",
		Description: "clean schema",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	})

	body, _, err := BuildGenerateContentRequest(params, nil, q)
	require.NoError(t, err)
	require.Contains(t, string(body), "search", "body should reference the tool")
}

// TestGeminiSchemaLint_IsNoOpForGemini25 pins that gemini-2.5, having no
// SchemaUnsupportedFeatures rule, accepts a schema with `pattern`.
func TestGeminiSchemaLint_IsNoOpForGemini25(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
	require.Empty(t, q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures,
		"gemini-2.5 should not pin SchemaUnsupportedFeatures")

	params := geminiParams("gemini-2.5-pro", types.ToolDefinition{
		Name:        "with_pattern",
		Description: "schema uses pattern",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","pattern":"^[a-z]+$"}},"required":["name"]}`),
	})

	_, _, err := BuildGenerateContentRequest(params, nil, q)
	require.NoError(t, err, "gemini-2.5 should accept schema with pattern (no lint rule)")
}
