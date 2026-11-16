package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

func toollessParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:      model,
		System:     "Classify the submission.",
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Buy now!"}}}},
		MaxTokens:  1024,
		ToolChoice: types.ToolChoiceAuto,
	}
}

func TestBuildGenerateContentRequest_NoToolsOmitsToolFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	body, _, err := BuildGenerateContentRequest(toollessParams("gemini-2.5-pro"), nil, quirks.ProviderQuirks{})
	must.NoError(err)

	var fields map[string]any
	must.NoError(json.Unmarshal(body, &fields))
	check.NotContains(fields, "tools")
	check.NotContains(fields, "toolConfig")
}

func TestBuildResponsesRequest_NoToolsOmitsToolFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	req, err := buildResponsesRequest(toollessParams("gpt-4o"), openAIResponsesQuirks("gpt-4o"), nil, "")
	must.NoError(err)
	req.Stream = true
	body, err := json.Marshal(req)
	must.NoError(err)

	var fields map[string]any
	must.NoError(json.Unmarshal(body, &fields))
	check.NotContains(fields, "tools")
	check.NotContains(fields, "tool_choice")
	check.NotContains(fields, "parallel_tool_calls")
}

func TestBuildConverseStreamInput_NoToolsOmitsToolConfig(t *testing.T) {
	must := require.New(t)
	input, err := buildConverseStreamInput(toollessParams("anthropic.claude-sonnet-4-6-v1"), quirks.ProviderQuirks{})
	must.NoError(err)
	must.Nil(input.ToolConfig)
}
