package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// AssertWireEqual pins the full canonical request body against a committed
// fixture, so a change that adds or drops a top-level key does not pass
// unnoticed; the fixture-authoring procedure is in testdata/quirks/README.md.
//
// Both use a synthetic tool with no Presentation so the fixtures stay stable
// regardless of the examples-folding capability (a tool that carries
// InputExamples would fold them into the schema on these providers).

func contractFixtureTool() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
	}
}

func contractFixtureMessages() []types.Message {
	return []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "weather in Paris?"}}}}
}

// TestAnthropicContract_ToolEnabledRequestBody pins the outbound Anthropic
// Messages request for a tool-enabled, required-tool-choice main-loop turn
// (one carrying a cache key).
func TestAnthropicContract_ToolEnabledRequestBody(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{
		Model:       "claude-sonnet-4-6",
		System:      "You are helpful.",
		Messages:    contractFixtureMessages(),
		Tools:       []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		ToolChoice:  types.ToolChoiceRequired,
		CacheKey:    "contract-cache-key",
	}
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: params.Model})
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	must.NoError(err, "marshal anthropic request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "anthropic", "claude-sonnet-4-6", "request.json"), body)
}

// TestAnthropicContract_ClaudeSonnet5OmitsTemperature pins the outbound
// Claude Sonnet 5 request shape: identical to the claude-sonnet-4-6 fixture
// above except "temperature" is absent, even though the same non-nil
// Temperature is supplied. Claude Sonnet 5 (like Opus 4.7+ and Fable 5 /
// Mythos 5) returns a 400 on a non-default temperature; the
// OmitSamplingParams quirk must suppress the field before it reaches the
// wire.
func TestAnthropicContract_ClaudeSonnet5OmitsTemperature(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{
		Model:       "claude-sonnet-5",
		System:      "You are helpful.",
		Messages:    contractFixtureMessages(),
		Tools:       []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		ToolChoice:  types.ToolChoiceRequired,
		CacheKey:    "contract-cache-key",
	}
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: params.Model})
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	must.NoError(err, "marshal anthropic request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "anthropic", "claude-sonnet-5", "request.json"), body)
}

// TestResponsesContract_ToolEnabledRequestBody pins the outbound OpenAI
// Responses request for a tool-enabled turn, including the typed input-item
// variants and the flat function-tool shape.
func TestResponsesContract_ToolEnabledRequestBody(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{
		Model:       "gpt-4o",
		System:      "You are helpful.",
		Messages:    contractFixtureMessages(),
		Tools:       []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
	}
	q := openAIResponsesQuirks(params.Model)
	req, err := buildResponsesRequest(params, q, nil, "")
	must.NoError(err, "build responses request")
	req.Stream = true
	body, err := json.Marshal(req)
	must.NoError(err, "marshal responses request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-responses", "gpt-4o", "request.json"), body)
}

// TestResponsesContract_GPT56RequestBody pins the outbound Responses request
// for a GPT-5.6 reasoning turn: reasoning.effort projected, temperature
// omitted despite a non-nil value, strict tools with a normalised schema, and
// the encrypted-reasoning include. Documented, not probed.
func TestResponsesContract_GPT56RequestBody(t *testing.T) {
	must := require.New(t)
	params := types.StreamParams{
		Model:           "gpt-5.6-sol",
		System:          "You are helpful.",
		Messages:        contractFixtureMessages(),
		Tools:           []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:       4096,
		Temperature:     types.Float64Ptr(0.5),
		ReasoningEffort: "high",
	}
	q := openAIResponsesQuirks(params.Model)
	req, err := buildResponsesRequest(params, q, nil, "")
	must.NoError(err, "build responses request")
	req.Stream = true
	body, err := json.Marshal(req)
	must.NoError(err, "marshal responses request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-responses", "gpt-5.6-sol", "request.json"), body)
}

// outputSchemaFixtureParams is a judge-shaped turn: no tools, greedy
// temperature, a verdict schema the adapter is asked to enforce.
func outputSchemaFixtureParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:            model,
		System:           "You are a safety classifier.",
		Messages:         contractFixtureMessages(),
		MaxTokens:        1024,
		Temperature:      types.Float64Ptr(0),
		ReasoningSummary: types.ReasoningSummaryOff,
		OutputSchema: &types.OutputSchema{
			Name:   "verdict",
			Schema: json.RawMessage(`{"type":"object","properties":{"nonce":{"type":"string"},"reasoning":{"type":"string"},"verdict":{"type":"string","enum":["allow","deny"]}},"required":["nonce","reasoning","verdict"],"additionalProperties":false}`),
		},
	}
}

// TestAnthropicContract_OutputSchemaRequestBody pins output_config.format
// beside output_config.effort on a Claude Haiku 5.5 judge turn.
func TestAnthropicContract_OutputSchemaRequestBody(t *testing.T) {
	must := require.New(t)
	params := outputSchemaFixtureParams("claude-haiku-5-5")
	params.ReasoningEffort = "low"
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: params.Model})
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	must.NoError(err, "marshal anthropic request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "anthropic", "claude-haiku-5-5", "request.json"), body)
}

// TestOpenAIContract_OutputSchemaRequestBody pins response_format on a
// gpt-5.5 Chat Completions judge turn.
func TestOpenAIContract_OutputSchemaRequestBody(t *testing.T) {
	must := require.New(t)
	params := outputSchemaFixtureParams("gpt-5.5")
	params.ReasoningEffort = "low"
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-compatible", Model: params.Model})
	req, err := buildOpenAIRequest(params, true, q, nil)
	must.NoError(err, "build openai request")
	body, err := json.Marshal(req)
	must.NoError(err, "marshal openai request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-compatible", "gpt-5.5", "request.json"), body)
}

// TestResponsesContract_OutputSchemaRequestBody pins text.format beside
// reasoning.effort on a gpt-5.5 Responses judge turn.
func TestResponsesContract_OutputSchemaRequestBody(t *testing.T) {
	must := require.New(t)
	params := outputSchemaFixtureParams("gpt-5.5")
	params.ReasoningEffort = "low"
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-responses", Model: params.Model})
	req, err := buildResponsesRequest(params, q, nil, "")
	must.NoError(err, "build responses request")
	req.Stream = true
	body, err := json.Marshal(req)
	must.NoError(err, "marshal responses request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-responses", "gpt-5.5", "request.json"), body)
}

// TestGeminiContract_OutputSchemaRequestBody pins the Vertex AI
// generationConfig.responseFormat array beside thinkingConfig on a Gemini
// 3.8 Flash judge turn. No built-in rule grants a Gemini model, so the
// capability is advertised in-test.
func TestGeminiContract_OutputSchemaRequestBody(t *testing.T) {
	must := require.New(t)
	params := outputSchemaFixtureParams("gemini-3.8-flash")
	params.ReasoningEffort = "low"
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: params.Model})
	q.OutputSchema = quirks.OutputSchemaCapability{Supported: true}
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	must.NoError(err, "build gemini request")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "gemini", "gemini-3.8-flash", "request.json"), body)
}
