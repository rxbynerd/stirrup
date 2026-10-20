package provider

import (
	"encoding/json"
	"strings"
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

// Hosted tool search output items, shaped after the OpenAI tool search
// guide (documented, not probed): call_id is null, the call's arguments
// are a JSON object, and the output lists the loaded definitions.
const (
	toolSearchCallItem   = `{"type":"tool_search_call","id":"tsc_1","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["mcp_jira_create_issue"]}}`
	toolSearchOutputItem = `{"type":"tool_search_output","id":"tso_1","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"mcp_jira_create_issue","description":"Create a Jira issue","defer_loading":true,"parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}]}`
	loadedFunctionCall   = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp_jira_create_issue","namespace":"mcp_jira_create_issue","arguments":"{\"title\":\"Broken build\"}","status":"completed"}`
)

// toolSearchSSE is one response that searches, loads a deferred tool and
// calls it.
func toolSearchSSE() []byte {
	return []byte(strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"tool_search_call","id":"tsc_1","execution":"server","call_id":null,"status":"in_progress","arguments":{}}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+toolSearchCallItem+`}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":1,"item":{"type":"tool_search_output","id":"tso_1","execution":"server","call_id":null,"status":"in_progress","tools":[]}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+toolSearchOutputItem+`}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp_jira_create_issue","namespace":"mcp_jira_create_issue","arguments":""}}`),
		makeResponsesEvent("response.function_call_arguments.delta", `{"item_id":"fc_1","output_index":2,"delta":"{\"title\":\"Broken build\"}"}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":2,"item":`+loadedFunctionCall+`}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[`+toolSearchCallItem+`,`+toolSearchOutputItem+`,`+loadedFunctionCall+`]}}`),
	}, ""))
}

func TestOpenAIResponsesAdapter_StreamEmitsHostedToolSearch(t *testing.T) {
	must := require.New(t)
	events := streamResponsesSSE(t, toolSearchSSE(), nil)

	must.Len(events, 4)
	must.Equal(types.StreamEvent{
		Type:  "tool_search",
		ID:    "tsc_1",
		Name:  "tool_search",
		Input: map[string]any{"paths": []any{"mcp_jira_create_issue"}},
	}, events[0])
	must.Equal(types.StreamEvent{
		Type:           "tool_search_result",
		ID:             "tsc_1",
		ToolReferences: []string{"mcp_jira_create_issue"},
	}, events[1])
	must.Equal("tool_call", events[2].Type)
	must.Equal("call_1", events[2].ID)
	must.Equal("mcp_jira_create_issue", events[2].Name)
	must.Equal(map[string]any{"title": "Broken build"}, events[2].Input)
	must.Equal("message_complete", events[3].Type)
	must.Equal("tool_use", events[3].StopReason)
}

func TestOpenAIResponsesAdapter_ToolSearchPairsOutputsWithCallsInOrder(t *testing.T) {
	must := require.New(t)
	call := func(id string) string {
		return `{"type":"tool_search_call","id":"` + id + `","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["x"]}}`
	}
	output := func(id, name string) string {
		return `{"type":"tool_search_output","id":"` + id + `","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"` + name + `"}]}`
	}
	sse := []byte(strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+call("tsc_a")+`}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call("tsc_b")+`}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":2,"item":`+output("tso_a", "first")+`}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":3,"item":`+output("tso_b", "second")+`}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`),
	}, ""))
	events := streamResponsesSSE(t, sse, nil)

	must.Len(events, 5)
	must.Equal("tsc_a", events[2].ID)
	must.Equal([]string{"first"}, events[2].ToolReferences)
	must.Equal("tsc_b", events[3].ID)
	must.Equal([]string{"second"}, events[3].ToolReferences)
}

func TestOpenAIResponsesAdapter_ToolSearchWithoutItemIDGetsPositionalID(t *testing.T) {
	must := require.New(t)
	sse := []byte(strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":3,"item":{"type":"tool_search_call","execution":"server","call_id":null,"status":"completed","arguments":{}}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":4,"item":{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[]}}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`),
	}, ""))
	events := streamResponsesSSE(t, sse, nil)

	must.Len(events, 3)
	must.Equal("tool_search_3", events[0].ID)
	must.Equal(map[string]any{}, events[0].Input)
	must.Equal("tool_search_3", events[1].ID)
	must.Empty(events[1].ToolReferences, "an empty tools array is a no-match result")
}

func TestOpenAIResponsesAdapter_ToolSearchMalformedPayloadsDoNotFailStream(t *testing.T) {
	cases := []struct {
		name      string
		call      string
		output    string
		wantInput map[string]any
	}{
		{
			name:   "arguments not an object and tools not an array",
			call:   `{"type":"tool_search_call","id":"tsc_1","status":"completed","arguments":[1,2]}`,
			output: `{"type":"tool_search_output","id":"tso_1","status":"completed","tools":{"type":"function"}}`,
		},
		{
			name:      "arguments as a JSON string",
			call:      `{"type":"tool_search_call","id":"tsc_1","status":"completed","arguments":"{\"query\":\"jira\"}"}`,
			output:    `{"type":"tool_search_output","id":"tso_1","status":"completed","tools":[{"type":"mcp","server_label":"jira"}]}`,
			wantInput: map[string]any{"query": "jira"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			sse := []byte(strings.Join([]string{
				makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+tc.call+`}`),
				makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+tc.output+`}`),
				makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[`+tc.call+`,`+tc.output+`]}}`),
			}, ""))
			events := streamResponsesSSE(t, sse, nil)

			must.Len(events, 3)
			must.Equal("tool_search", events[0].Type)
			must.Equal(tc.wantInput, events[0].Input)
			must.Equal("tool_search_result", events[1].Type)
			must.Equal("tsc_1", events[1].ID)
			must.Empty(events[1].ToolReferences)
			must.Equal("message_complete", events[2].Type)
		})
	}
}

func TestResponsesLoadedToolNames_FlattensNamespaces(t *testing.T) {
	must := require.New(t)
	names, err := responsesLoadedToolNames(json.RawMessage(`[
		{"type":"function","name":"get_weather"},
		{"type":"namespace","name":"crm","description":"CRM tools","tools":[
			{"type":"function","name":"list_open_orders","defer_loading":true},
			{"type":"function","name":"get_customer_profile"}
		]},
		{"type":"mcp","server_label":"docs"},
		{"type":"function","name":"get_weather"}
	]`))
	must.NoError(err)
	must.Equal([]string{"get_weather", "list_open_orders", "get_customer_profile"}, names)
}

func TestResponsesOutputItem_FunctionCallArgumentsMustBeAString(t *testing.T) {
	must := require.New(t)
	var item responsesOutputItem
	err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call_1","arguments":{"path":"a.go"}}`), &item)
	must.Error(err, "object arguments are accepted only on tool_search_call items")
}
