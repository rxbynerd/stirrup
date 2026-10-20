package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

const toolSearchModel = "gpt-5.5"

var (
	readFileTool = types.ToolDefinition{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}
	jiraCreateTool = types.ToolDefinition{
		Name:        "mcp_jira_create_issue",
		Description: "Create a Jira issue",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`),
		Deferred:    true,
	}
)

// toolSearchQuirks resolves quirks with hosted tool search on or off and
// strict mode off, so a test pins one concern at a time.
func toolSearchQuirks(supported bool) quirks.ProviderQuirks {
	return quirks.NewRegistry([]quirks.Rule{{
		ProviderType: "openai-responses",
		ModelMatch:   toolSearchModel,
		Description:  "test: hosted tool search",
		LastVerified: quirks.Date("2026-10-20"),
		Apply: func(q *quirks.ProviderQuirks) {
			q.ToolSearch = quirks.ToolSearchCapability{Supported: supported, Variant: "hosted"}
		},
	}}).Resolve("openai-responses", toolSearchModel)
}

func toolSearchParams() types.StreamParams {
	return types.StreamParams{
		Model:      toolSearchModel,
		MaxTokens:  1024,
		ToolSearch: true,
		Tools:      []types.ToolDefinition{readFileTool, jiraCreateTool},
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "file a ticket"}}}},
	}
}

// requestTools marshals the built request and returns its tools array as
// generic JSON values.
func requestTools(t *testing.T, params types.StreamParams, q quirks.ProviderQuirks) []map[string]any {
	t.Helper()
	must := require.New(t)
	req, err := buildResponsesRequest(params, q, nil, "")
	must.NoError(err)
	body, err := json.Marshal(req)
	must.NoError(err)
	var decoded struct {
		Tools []map[string]any `json:"tools"`
	}
	must.NoError(json.Unmarshal(body, &decoded))
	return decoded.Tools
}

func TestBuildResponsesRequest_ToolSearchAppendsHostedEntry(t *testing.T) {
	must := require.New(t)
	tools := requestTools(t, toolSearchParams(), toolSearchQuirks(true))
	must.Len(tools, 3)
	must.Equal(map[string]any{"type": "tool_search"}, tools[2], "hosted search entry carries only its type")
}

func TestBuildResponsesRequest_ToolSearchDefersOnlyDeferredTools(t *testing.T) {
	must := require.New(t)
	tools := requestTools(t, toolSearchParams(), toolSearchQuirks(true))
	must.Len(tools, 3)
	must.Equal("read_file", tools[0]["name"])
	must.NotContains(tools[0], "defer_loading")
	must.Equal("mcp_jira_create_issue", tools[1]["name"])
	must.Equal(true, tools[1]["defer_loading"])
	must.Equal("Create a Jira issue", tools[1]["description"], "a deferred function keeps its description")
	must.Contains(tools[1], "parameters")
}

func TestBuildResponsesRequest_ToolSearchComposesWithStrictMode(t *testing.T) {
	must := require.New(t)
	q := toolSearchQuirks(true)
	q.BehaviourFlags.OpenAI.StrictMode = true
	tools := requestTools(t, toolSearchParams(), q)
	must.Len(tools, 3)
	must.Equal(true, tools[1]["strict"])
	must.Equal(true, tools[1]["defer_loading"])
	params, ok := tools[1]["parameters"].(map[string]any)
	must.True(ok)
	must.Equal(false, params["additionalProperties"], "strict normalisation still applies to a deferred tool")
	must.Equal(map[string]any{"type": "tool_search"}, tools[2])
}

func TestBuildResponsesRequest_ToolSearchGateOff(t *testing.T) {
	cases := []struct {
		name   string
		params func() types.StreamParams
		q      quirks.ProviderQuirks
	}{
		{"quirks do not support it", toolSearchParams, toolSearchQuirks(false)},
		{"loop did not ask for it", func() types.StreamParams {
			p := toolSearchParams()
			p.ToolSearch = false
			return p
		}, toolSearchQuirks(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			tools := requestTools(t, tc.params(), tc.q)
			must.Len(tools, 2, "no hosted search entry")
			for _, tool := range tools {
				must.Equal("function", tool["type"])
				must.NotContains(tool, "defer_loading")
			}
		})
	}
}

func TestBuildResponsesRequest_ToolSearchWithoutToolsSendsNoTools(t *testing.T) {
	must := require.New(t)
	params := toolSearchParams()
	params.Tools = nil
	req, err := buildResponsesRequest(params, toolSearchQuirks(true), nil, "")
	must.NoError(err)
	must.Empty(req.Tools)
}

func TestResponsesTool_RoundTripsHostedSearchEntry(t *testing.T) {
	must := require.New(t)
	req, err := buildResponsesRequest(toolSearchParams(), toolSearchQuirks(true), nil, "")
	must.NoError(err)
	body, err := json.Marshal(req)
	must.NoError(err)
	var back responsesRequest
	must.NoError(json.Unmarshal(body, &back))
	again, err := json.Marshal(back)
	must.NoError(err)
	must.Equal(string(body), string(again))
}
