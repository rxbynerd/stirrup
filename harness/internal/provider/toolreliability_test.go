package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// These tests pin the wire behaviour of the parallel-tool-call policy and
// input-examples reliability controls per provider, gated on the resolved
// quirks capability. They exercise the builders directly, no live calls.

func demoToolWithExample() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "demo",
		Description: "Demo tool. Example: {\"x\": \"hi\"}",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`),
		Presentation: &types.ToolPresentation{
			InputExamples: []json.RawMessage{json.RawMessage(`{"x":"hi"}`)},
		},
	}
}

func userTurn() []types.Message {
	return []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "go"}}}}
}

// decodeObject unmarshals raw JSON into a string-keyed map, failing the test
// on error. Used to walk a wire body without committing to a typed shape.
func decodeObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m), "raw: %s", raw)
	return m
}

func TestOpenAIChat_NonStrictModel_DisablesParallelAndFoldsExamples(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "gpt-4o", // non-strict
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{demoToolWithExample()},
		MaxTokens:         100,
		ParallelToolCalls: &disable,
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, nil)
	require.NoError(t, err)
	body, err := json.Marshal(req)
	require.NoError(t, err)

	top := decodeObject(t, body)
	require.Equal(t, "false", string(top["parallel_tool_calls"]))

	var tools []json.RawMessage
	require.NoError(t, json.Unmarshal(top["tools"], &tools))
	function := decodeObject(t, decodeObject(t, tools[0])["function"])
	require.Contains(t, decodeObject(t, function["parameters"]), "examples",
		"non-strict tool schema should carry the folded examples keyword: %s", function["parameters"])
}

func TestOpenAIChat_StrictModel_OmitsExamplesAndSetsStrict(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-5", // strict-mode model
		Messages:  userTurn(),
		Tools:     []types.ToolDefinition{demoToolWithExample()},
		MaxTokens: 100,
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, newStrictSchemaCache())
	require.NoError(t, err)
	body, err := json.Marshal(req)
	require.NoError(t, err)

	var tools []json.RawMessage
	require.NoError(t, json.Unmarshal(decodeObject(t, body)["tools"], &tools))
	entry := decodeObject(t, tools[0])
	require.NotEmpty(t, entry["function"], "tool has no function entry: %s", tools[0])
	function := decodeObject(t, entry["function"])
	require.Equal(t, "true", string(function["strict"]), "strict model should emit strict:true")
	// Structured-outputs strict mode rejects `examples`; the description
	// text remains the example carrier.
	require.NotContains(t, decodeObject(t, function["parameters"]), "examples",
		"strict tool schema must not carry examples: %s", function["parameters"])
}

func TestOpenAIChat_UnsetParallel_OmitsField(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-4o",
		Messages:  userTurn(),
		Tools:     []types.ToolDefinition{demoToolWithExample()},
		MaxTokens: 100,
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, nil)
	require.NoError(t, err)
	body, err := json.Marshal(req)
	require.NoError(t, err)

	require.NotContains(t, decodeObject(t, body), "parallel_tool_calls",
		"parallel_tool_calls must be omitted when the caller did not set it")
}

func TestResponses_EnabledParallel_EmitsTrueAndFoldsExamples(t *testing.T) {
	enable := true
	params := types.StreamParams{
		Model:             "gpt-4o",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{demoToolWithExample()},
		MaxTokens:         100,
		ParallelToolCalls: &enable,
	}
	q := quirks.DefaultRegistry().Resolve("openai-responses", params.Model)
	req, err := buildResponsesRequest(params, q, nil, "")
	require.NoError(t, err)
	body, err := json.Marshal(req)
	require.NoError(t, err)

	top := decodeObject(t, body)
	require.Equal(t, "true", string(top["parallel_tool_calls"]))
	var tools []json.RawMessage
	require.NoError(t, json.Unmarshal(top["tools"], &tools))
	// Responses tools are flat: parameters sits directly on the entry.
	require.Contains(t, decodeObject(t, decodeObject(t, tools[0])["parameters"]), "examples",
		"responses tool schema is missing folded examples: %s", tools[0])
}

func TestAnthropic_DisabledParallel_SetsAutoToolChoice(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "claude-sonnet-4-5",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{demoToolWithExample()},
		MaxTokens:         100,
		ParallelToolCalls: &disable,
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	require.NoError(t, err)
	top := decodeObject(t, body)

	// disable_parallel_tool_use rides on a synthesised auto tool_choice.
	toolChoice := decodeObject(t, top["tool_choice"])
	require.Equal(t, `"auto"`, string(toolChoice["type"]))
	require.Equal(t, "true", string(toolChoice["disable_parallel_tool_use"]))

	// examples ride on the native input_examples field; the schema is
	// sent untouched.
	var tools []json.RawMessage
	require.NoError(t, json.Unmarshal(top["tools"], &tools))
	tool := decodeObject(t, tools[0])
	require.JSONEq(t, `[{"x":"hi"}]`, string(tool["input_examples"]))
	require.NotContains(t, decodeObject(t, tool["input_schema"]), "examples",
		"anthropic tool input_schema carries folded examples alongside the native field: %s", tools[0])
}

// TestTranslateToolsAnthropic_ExamplesPlacement pins the three capability
// outcomes: native field, schema fold, or neither. A tool without examples
// never gains an input_examples key.
func TestTranslateToolsAnthropic_ExamplesPlacement(t *testing.T) {
	plain := types.ToolDefinition{
		Name:        "plain",
		Description: "No examples.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	cases := []struct {
		name       string
		capability quirks.ToolExamplesCapability
		want       string
	}{
		{
			name:       "native field",
			capability: quirks.ToolExamplesCapability{Supported: true, Native: true},
			want:       `[{"name":"demo","description":"Demo tool. Example: {\"x\": \"hi\"}","input_schema":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]},"input_examples":[{"x":"hi"}]},{"name":"plain","description":"No examples.","input_schema":{"type":"object"}}]`,
		},
		{
			name:       "schema fold",
			capability: quirks.ToolExamplesCapability{Supported: true},
			want:       `[{"name":"demo","description":"Demo tool. Example: {\"x\": \"hi\"}","input_schema":{"examples":[{"x":"hi"}],"properties":{"x":{"type":"string"}},"required":["x"],"type":"object"}},{"name":"plain","description":"No examples.","input_schema":{"type":"object"}}]`,
		},
		{
			name:       "unsupported",
			capability: quirks.ToolExamplesCapability{},
			want:       `[{"name":"demo","description":"Demo tool. Example: {\"x\": \"hi\"}","input_schema":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}},{"name":"plain","description":"No examples.","input_schema":{"type":"object"}}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := []types.ToolDefinition{demoToolWithExample(), plain}
			got, err := json.Marshal(translateToolsAnthropic(tools, tc.capability))
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
			require.JSONEq(t, `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`,
				string(tools[0].InputSchema), "translation must not mutate the input tool schema")
		})
	}
}

func TestAnthropic_EnabledParallel_OmitsToolChoice(t *testing.T) {
	enable := true
	params := types.StreamParams{
		Model:             "claude-sonnet-4-5",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{demoToolWithExample()},
		MaxTokens:         100,
		ParallelToolCalls: &enable, // parallel is the default; nothing to emit
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	require.NoError(t, err)

	require.NotContains(t, decodeObject(t, body), "tool_choice",
		"enabling parallel (the Anthropic default) must not synthesise a tool_choice object")
}

func TestAnthropic_RequiredChoice_CarriesDisableParallel(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "claude-sonnet-4-5",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{demoToolWithExample()},
		MaxTokens:         100,
		ToolChoice:        types.ToolChoiceRequired,
		ParallelToolCalls: &disable,
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	require.NoError(t, err)

	toolChoice := decodeObject(t, decodeObject(t, body)["tool_choice"])
	require.Equal(t, `"any"`, string(toolChoice["type"]), "required maps to the any tool_choice type")
	require.Equal(t, "true", string(toolChoice["disable_parallel_tool_use"]),
		"required tool_choice should still carry disable_parallel_tool_use")
}

func TestGemini_ParallelAndExamples_AreNotEmitted(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "gemini-2.5-pro",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{demoToolWithExample()},
		MaxTokens:         100,
		ParallelToolCalls: &disable, // unsupported on Gemini → no-op
	}
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	require.NoError(t, err)
	top := decodeObject(t, body)

	// Gemini has no native parallel-tool control in either casing.
	require.NotContains(t, top, "parallel_tool_calls")
	require.NotContains(t, top, "parallelToolCalls")

	// Gemini's ToolExamples capability is zero, so examples never fold into
	// the function-declaration schema.
	var tools []json.RawMessage
	require.NoError(t, json.Unmarshal(top["tools"], &tools))
	declarations := decodeObject(t, tools[0])
	var functionDecls []json.RawMessage
	require.NoError(t, json.Unmarshal(declarations["functionDeclarations"], &functionDecls))
	schema := decodeObject(t, functionDecls[0])["parameters"]
	require.NotContains(t, decodeObject(t, schema), "examples", "Gemini tool schema must not carry examples: %s", schema)
}
