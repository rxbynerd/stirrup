package provider

import (
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// These tests close the #224 gap where the Anthropic and OpenAI Responses
// adapters had no golden request fixture — their request shape was only
// validated field-by-field inline, so a change that added or dropped a
// top-level key could pass unnoticed. AssertWireEqual pins the full canonical
// body against the committed fixture; the fixture-authoring procedure is in
// testdata/quirks/README.md.
//
// Both use a synthetic tool with no Presentation so the fixtures stay stable
// regardless of the #222 examples-folding capability (a tool that carries
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
// Messages request for a tool-enabled, required-tool-choice turn.
func TestAnthropicContract_ToolEnabledRequestBody(t *testing.T) {
	params := types.StreamParams{
		Model:       "claude-sonnet-4-6",
		System:      "You are helpful.",
		Messages:    contractFixtureMessages(),
		Tools:       []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		ToolChoice:  types.ToolChoiceRequired,
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
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
	params := types.StreamParams{
		Model:       "claude-sonnet-5",
		System:      "You are helpful.",
		Messages:    contractFixtureMessages(),
		Tools:       []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		ToolChoice:  types.ToolChoiceRequired,
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "anthropic", "claude-sonnet-5", "request.json"), body)
}

// TestResponsesContract_ToolEnabledRequestBody pins the outbound OpenAI
// Responses request for a tool-enabled turn, including the typed input-item
// variants (#199) and the flat function-tool shape.
func TestResponsesContract_ToolEnabledRequestBody(t *testing.T) {
	params := types.StreamParams{
		Model:       "gpt-4o",
		System:      "You are helpful.",
		Messages:    contractFixtureMessages(),
		Tools:       []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
	}
	q := quirks.DefaultRegistry().Resolve("openai-responses", params.Model)
	req, err := buildResponsesRequest(params, q, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-responses", "gpt-4o", "request.json"), body)
}
