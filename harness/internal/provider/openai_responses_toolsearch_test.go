package provider

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

const responsesToolSearchModel = "gpt-5.5"

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

// responsesToolSearchQuirks resolves quirks with hosted tool search on or off and
// strict mode off, so a test pins one concern at a time.
func responsesToolSearchQuirks(supported bool) quirks.ProviderQuirks {
	return openAIResponsesQuirksIn(quirks.NewRegistry([]quirks.Rule{{
		ProviderType: "openai-responses",
		ModelMatch:   responsesToolSearchModel,
		Description:  "test: hosted tool search",
		LastVerified: quirks.Date("2026-10-20"),
		Apply: func(q *quirks.ProviderQuirks) {
			q.ToolSearch = quirks.ToolSearchCapability{Supported: supported, Variant: "hosted"}
		},
	}}), responsesToolSearchModel)
}

func responsesToolSearchParams() types.StreamParams {
	return types.StreamParams{
		Model:      responsesToolSearchModel,
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
	tools := requestTools(t, responsesToolSearchParams(), responsesToolSearchQuirks(true))
	must.Len(tools, 3)
	must.Equal(map[string]any{"type": "tool_search"}, tools[2], "hosted search entry carries only its type")
}

func TestBuildResponsesRequest_ToolSearchDefersOnlyDeferredTools(t *testing.T) {
	must := require.New(t)
	tools := requestTools(t, responsesToolSearchParams(), responsesToolSearchQuirks(true))
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
	q := responsesToolSearchQuirks(true)
	q.BehaviourFlags.OpenAI.StrictMode = true
	tools := requestTools(t, responsesToolSearchParams(), q)
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
		{"quirks do not support it", responsesToolSearchParams, responsesToolSearchQuirks(false)},
		{"loop did not ask for it", func() types.StreamParams {
			p := responsesToolSearchParams()
			p.ToolSearch = false
			return p
		}, responsesToolSearchQuirks(true)},
		{"no tool is deferred", func() types.StreamParams {
			p := responsesToolSearchParams()
			p.Tools = p.Tools[:1]
			return p
		}, responsesToolSearchQuirks(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			tools := requestTools(t, tc.params(), tc.q)
			must.NotEmpty(tools)
			for _, tool := range tools {
				must.Equal("function", tool["type"])
				must.NotContains(tool, "defer_loading")
			}
		})
	}
}

func TestBuildResponsesRequest_ToolSearchWithoutToolsSendsNoTools(t *testing.T) {
	must := require.New(t)
	params := responsesToolSearchParams()
	params.Tools = nil
	req, err := buildResponsesRequest(params, responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	must.Empty(req.Tools)
}

func TestResponsesTool_RoundTripsHostedSearchEntry(t *testing.T) {
	must := require.New(t)
	req, err := buildResponsesRequest(responsesToolSearchParams(), responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	body, err := json.Marshal(req)
	must.NoError(err)
	var back responsesRequest
	must.NoError(json.Unmarshal(body, &back))
	again, err := json.Marshal(back)
	must.NoError(err)
	must.Equal(string(body), string(again))
}

// Hosted tool search output items trimmed to the fields the adapter reads:
// call_id is null, the call's arguments are a JSON object, and the output
// lists the loaded definitions.
const (
	toolSearchCallItem   = `{"type":"tool_search_call","id":"tsc_1","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["mcp_jira_create_issue"]}}`
	toolSearchOutputItem = `{"type":"tool_search_output","id":"tso_1","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"mcp_jira_create_issue","description":"Create a Jira issue","defer_loading":true,"parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}]}`
	loadedFunctionCall   = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp_jira_create_issue","namespace":"mcp_jira_create_issue","arguments":"{\"title\":\"Broken build\"}","status":"completed"}`
)

// responsesToolSearchSSE is one response that searches, loads a deferred tool and
// calls it.
func responsesToolSearchSSE() []byte {
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
	events := streamResponsesSSE(t, responsesToolSearchSSE(), nil)

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

// responsesNestedNamespaces returns a tools array whose single namespace
// entry nests another namespace depth-1 more times, with one function at
// the innermost level.
func responsesNestedNamespaces(depth int) json.RawMessage {
	var b strings.Builder
	for range depth {
		b.WriteString(`[{"type":"namespace","name":"n","tools":`)
	}
	b.WriteString(`[{"type":"function","name":"deep"}]`)
	for range depth {
		b.WriteString(`}]`)
	}
	return json.RawMessage(b.String())
}

func TestResponsesLoadedToolNames_CapsNamespaceDepth(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"function at depth 1", `[{"type":"function","name":"a"}]`, []string{"a"}},
		{"function in a namespace at depth 2", `[{"type":"namespace","tools":[{"type":"function","name":"b"}]}]`, []string{"b"}},
		{"function in a namespace nested at depth 3", `[{"type":"namespace","tools":[{"type":"namespace","tools":[{"type":"function","name":"c"}]},{"type":"function","name":"d"}]}]`, []string{"d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			names, err := responsesLoadedToolNames(json.RawMessage(tc.raw))
			must.NoError(err)
			must.Equal(tc.want, names)
		})
	}
}

func TestResponsesLoadedToolNames_DeepNestingIsLinear(t *testing.T) {
	must := require.New(t)
	raw := responsesNestedNamespaces(4990)
	start := time.Now()
	names, err := responsesLoadedToolNames(raw)
	must.NoError(err)
	must.Empty(names)
	must.Less(time.Since(start), time.Second, "deeply nested namespaces must not be re-decoded per level")
}

func TestResponsesOutputItem_FunctionCallArgumentsMustBeAString(t *testing.T) {
	must := require.New(t)
	var item responsesOutputItem
	err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call_1","arguments":{"path":"a.go"}}`), &item)
	must.Error(err, "object arguments are accepted only on tool_search_call items")
}

// responsesToolSearchTurnBlocks is the assistant content the loop persists for
// responsesToolSearchSSE.
func responsesToolSearchTurnBlocks() []types.ContentBlock {
	return []types.ContentBlock{
		{Type: "tool_search", ID: "tsc_1", Name: "tool_search", Input: json.RawMessage(`{"paths":["mcp_jira_create_issue"]}`)},
		{Type: "tool_search_result", ToolUseID: "tsc_1", ToolReferences: []string{"mcp_jira_create_issue"}},
		{Type: "tool_use", ID: "call_1", Name: "mcp_jira_create_issue", Input: json.RawMessage(`{"title":"Broken build"}`)},
	}
}

func responsesToolSearchHistory(blocks []types.ContentBlock) []types.Message {
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "file a ticket"}}},
		{Role: "assistant", Content: blocks},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "PROJ-1"}}},
	}
}

func TestBuildResponsesRequest_ToolSearchReconstructsSearchItems(t *testing.T) {
	must := require.New(t)
	params := responsesToolSearchParams()
	params.Messages = responsesToolSearchHistory(responsesToolSearchTurnBlocks())
	req, err := buildResponsesRequest(params, responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	input, err := json.Marshal(req.Input)
	must.NoError(err)

	must.JSONEq(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"file a ticket"}]},
		{"type":"tool_search_call","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["mcp_jira_create_issue"]}},
		{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[
			{"type":"function","name":"mcp_jira_create_issue","description":"Create a Jira issue","defer_loading":true,
			 "parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}
		]},
		{"type":"function_call","call_id":"call_1","name":"mcp_jira_create_issue","namespace":"mcp_jira_create_issue","arguments":"{\"title\":\"Broken build\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"PROJ-1"}
	]`, string(input))
}

// namespacedToolSearchParams is responsesToolSearchParams with the Jira
// tool grouped under the mcp_jira namespace, as the lazy loader sends an
// MCP server's deferred tools.
func namespacedToolSearchParams() types.StreamParams {
	params := responsesToolSearchParams()
	jira := jiraCreateTool
	jira.Namespace = "mcp_jira"
	jira.NamespaceDescription = `Tools from MCP server "jira".`
	params.Tools = []types.ToolDefinition{readFileTool, jira}
	return params
}

// requestFunctionCall returns the single function_call input item of a
// built request.
func requestFunctionCall(t *testing.T, params types.StreamParams, q quirks.ProviderQuirks) map[string]any {
	t.Helper()
	must := require.New(t)
	req, err := buildResponsesRequest(params, q, nil, "")
	must.NoError(err)
	body, err := json.Marshal(req)
	must.NoError(err)
	var calls []map[string]any
	for _, raw := range requestInput(t, body) {
		var item map[string]any
		must.NoError(json.Unmarshal(raw, &item))
		if item["type"] == "function_call" {
			calls = append(calls, item)
		}
	}
	must.Len(calls, 1)
	return calls[0]
}

func TestResponsesReconstruct_FunctionCallCarriesNamespace(t *testing.T) {
	readFileBlocks := func() []types.ContentBlock {
		b := responsesToolSearchTurnBlocks()
		b[2] = types.ContentBlock{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)}
		return b
	}
	callOnlyBlocks := func(name string) func() []types.ContentBlock {
		return func() []types.ContentBlock {
			return []types.ContentBlock{{Type: "tool_use", ID: "call_1", Name: name, Input: json.RawMessage(`{}`)}}
		}
	}
	withEagerMember := func() types.StreamParams {
		params := namespacedToolSearchParams()
		member := jiraCreateTool
		member.Name = "mcp_jira_list_projects"
		member.Namespace = "mcp_jira"
		member.NamespaceDescription = `Tools from MCP server "jira".`
		member.Deferred = false
		params.Tools = append(params.Tools, member)
		return params
	}
	cases := []struct {
		name   string
		params func() types.StreamParams
		blocks func() []types.ContentBlock
		q      quirks.ProviderQuirks
		want   any
	}{
		{"ungrouped deferred function names itself", responsesToolSearchParams, responsesToolSearchTurnBlocks, responsesToolSearchQuirks(true), "mcp_jira_create_issue"},
		{"namespace member names its namespace", namespacedToolSearchParams, responsesToolSearchTurnBlocks, responsesToolSearchQuirks(true), "mcp_jira"},
		{"eager function stays in the default namespace", responsesToolSearchParams, readFileBlocks, responsesToolSearchQuirks(true), nil},
		{"request without hosted search sends no namespace", namespacedToolSearchParams, responsesToolSearchTurnBlocks, responsesToolSearchQuirks(false), nil},
		{"namespace member called without a search pair names its namespace", namespacedToolSearchParams, callOnlyBlocks("mcp_jira_create_issue"), responsesToolSearchQuirks(true), "mcp_jira"},
		{"un-deferred namespace member names its namespace", withEagerMember, callOnlyBlocks("mcp_jira_list_projects"), responsesToolSearchQuirks(true), "mcp_jira"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			params := tc.params()
			params.Messages = responsesToolSearchHistory(tc.blocks())
			call := requestFunctionCall(t, params, tc.q)
			check.Equal(tc.want, call["namespace"])
		})
	}
}

func TestResponsesReconstruct_UndeferredFlatToolAfterRebuiltOutputKeepsNoNamespace(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	params := namespacedToolSearchParams()
	flat := jiraCreateTool
	flat.Name = "mcp_jira_list_projects"
	flat.Deferred = false
	params.Tools = append(params.Tools, flat)
	params.Messages = responsesToolSearchHistory(append(responsesToolSearchTurnBlocks(),
		types.ContentBlock{Type: "tool_use", ID: "call_2", Name: "mcp_jira_list_projects", Input: json.RawMessage(`{}`)}))

	req, err := buildResponsesRequest(params, responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	body, err := json.Marshal(req)
	must.NoError(err)

	var outputs []string
	namespaces := map[string]any{}
	for _, raw := range requestInput(t, body) {
		var item map[string]any
		must.NoError(json.Unmarshal(raw, &item))
		switch item["type"] {
		case "tool_search_output":
			outputs = append(outputs, string(raw))
		case "function_call":
			name, ok := item["name"].(string)
			must.True(ok)
			namespaces[name] = item["namespace"]
		}
	}
	must.Len(outputs, 1)
	check.NotContains(outputs[0], "mcp_jira_list_projects", "an un-deferred tool is never part of a rebuilt output")
	check.Equal("mcp_jira", namespaces["mcp_jira_create_issue"])
	check.Contains(namespaces, "mcp_jira_list_projects")
	check.Nil(namespaces["mcp_jira_list_projects"])
}

func TestResponsesReconstruct_ToolSearchOutputUsesNamespace(t *testing.T) {
	must := require.New(t)
	params := namespacedToolSearchParams()
	blocks := responsesToolSearchTurnBlocks()
	blocks[0].Input = json.RawMessage(`{"paths":["mcp_jira"]}`)
	params.Messages = responsesToolSearchHistory(blocks)
	req, err := buildResponsesRequest(params, responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	input, err := json.Marshal(req.Input)
	must.NoError(err)

	must.JSONEq(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"file a ticket"}]},
		{"type":"tool_search_call","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["mcp_jira"]}},
		{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[
			{"type":"namespace","name":"mcp_jira","description":"Tools from MCP server \"jira\".","tools":[
				{"type":"function","name":"mcp_jira_create_issue","description":"Create a Jira issue","defer_loading":true,
				 "parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}
			]}
		]},
		{"type":"function_call","call_id":"call_1","name":"mcp_jira_create_issue","namespace":"mcp_jira","arguments":"{\"title\":\"Broken build\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"PROJ-1"}
	]`, string(input))
}

func TestResponsesReconstruct_ToolSearchOutputGroupsMembersOnce(t *testing.T) {
	must := require.New(t)
	search := newResponsesToolSearch([]responsesTool{
		{Type: "function", Name: "read_file"},
		{Type: responsesNamespaceType, Name: "mcp_jira", Description: "Jira", Tools: []responsesTool{
			{Type: "function", Name: "mcp_jira_create_issue"},
			{Type: "function", Name: "mcp_jira_get_issue"},
		}},
		{Type: "function", Name: "list_directory"},
	})
	loaded, ok := search.loaded([]string{"mcp_jira_get_issue", "list_directory", "mcp_jira_create_issue"})
	must.True(ok)
	must.Len(loaded, 2)
	must.Equal(responsesNamespaceType, loaded[0].Type)
	must.Equal("mcp_jira", loaded[0].Name)
	must.Equal("Jira", loaded[0].Description)
	must.Len(loaded[0].Tools, 2)
	must.Equal("mcp_jira_get_issue", loaded[0].Tools[0].Name)
	must.Equal("mcp_jira_create_issue", loaded[0].Tools[1].Name)
	must.Equal("list_directory", loaded[1].Name)
	for _, def := range []responsesTool{loaded[0].Tools[0], loaded[0].Tools[1], loaded[1]} {
		must.NotNil(def.DeferLoading)
		must.True(*def.DeferLoading, "%s is flagged defer_loading", def.Name)
	}
}

func TestTranslateToolsResponses_GroupsDeferredToolsByNamespace(t *testing.T) {
	must := require.New(t)
	params := namespacedToolSearchParams()
	jiraGet := jiraCreateTool
	jiraGet.Name = "mcp_jira_get_issue"
	jiraGet.Namespace = "mcp_jira"
	jiraGet.NamespaceDescription = `Tools from MCP server "jira".`
	cp := jiraCreateTool
	cp.Name = "approve_deploy"
	cp.Namespace = "control_plane"
	cp.NamespaceDescription = "Tools provided by the control plane."
	builtIn := jiraCreateTool
	builtIn.Name = "list_directory"
	params.Tools = append(params.Tools, cp, jiraGet, builtIn)

	tools := requestTools(t, params, responsesToolSearchQuirks(true))
	must.Len(tools, 5)
	must.Equal("read_file", tools[0]["name"])
	must.Equal("namespace", tools[1]["type"])
	must.Equal("mcp_jira", tools[1]["name"])
	must.Equal(`Tools from MCP server "jira".`, tools[1]["description"])
	must.NotContains(tools[1], "parameters")
	members, ok := tools[1]["tools"].([]any)
	must.True(ok)
	must.Len(members, 2)
	for i, want := range []string{"mcp_jira_create_issue", "mcp_jira_get_issue"} {
		member, ok := members[i].(map[string]any)
		must.True(ok)
		must.Equal(want, member["name"], "members keep their full presented names")
		must.Equal(true, member["defer_loading"])
		must.Contains(member, "parameters")
	}
	must.Equal("namespace", tools[2]["type"])
	must.Equal("control_plane", tools[2]["name"])
	must.Equal("function", tools[3]["type"])
	must.Equal("list_directory", tools[3]["name"], "a deferred tool without a namespace stays ungrouped")
	must.Equal(true, tools[3]["defer_loading"])
	must.Equal(map[string]any{"type": "tool_search"}, tools[4])
}

func TestTranslateToolsResponses_UndeferredMemberStaysInItsNamespace(t *testing.T) {
	must := require.New(t)
	params := namespacedToolSearchParams()
	eager := jiraCreateTool
	eager.Name = "mcp_jira_list_projects"
	eager.Namespace = "mcp_jira"
	eager.NamespaceDescription = `Tools from MCP server "jira".`
	eager.Deferred = false
	params.Tools = append(params.Tools, eager)

	tools := requestTools(t, params, responsesToolSearchQuirks(true))
	must.Len(tools, 3)
	must.Equal("namespace", tools[1]["type"])
	members, ok := tools[1]["tools"].([]any)
	must.True(ok)
	must.Len(members, 2)
	deferred, ok := members[0].(map[string]any)
	must.True(ok)
	must.Equal(true, deferred["defer_loading"])
	undeferred, ok := members[1].(map[string]any)
	must.True(ok)
	must.Equal("mcp_jira_list_projects", undeferred["name"])
	must.NotContains(undeferred, "defer_loading")
}

func TestTranslateToolsResponses_NoNamespaceWithoutToolSearch(t *testing.T) {
	must := require.New(t)
	tools := requestTools(t, namespacedToolSearchParams(), responsesToolSearchQuirks(false))
	must.Len(tools, 2)
	for _, tool := range tools {
		must.Equal("function", tool["type"])
		must.NotContains(tool, "defer_loading")
	}
	must.Equal("mcp_jira_create_issue", tools[1]["name"])
}

func TestTranslateToolsResponses_NamespaceNamedLikeAFunctionStaysUngrouped(t *testing.T) {
	must := require.New(t)
	params := namespacedToolSearchParams()
	clash := readFileTool
	clash.Name = "mcp_jira"
	params.Tools = append(params.Tools, clash)

	tools := requestTools(t, params, responsesToolSearchQuirks(true))
	must.Len(tools, 4)
	for _, tool := range tools[:3] {
		must.NotEqual("namespace", tool["type"])
	}
	must.Equal("mcp_jira_create_issue", tools[1]["name"])
	must.Equal("mcp_jira", tools[2]["name"])
}

func TestResponsesTool_RoundTripsNamespaceEntry(t *testing.T) {
	must := require.New(t)
	req, err := buildResponsesRequest(namespacedToolSearchParams(), responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	body, err := json.Marshal(req)
	must.NoError(err)
	var back responsesRequest
	must.NoError(json.Unmarshal(body, &back))
	again, err := json.Marshal(back)
	must.NoError(err)
	must.Equal(string(body), string(again))
}

func TestBuildResponsesRequest_ToolSearchReconstructsEmptyResult(t *testing.T) {
	must := require.New(t)
	params := responsesToolSearchParams()
	params.Messages = responsesToolSearchHistory([]types.ContentBlock{
		{Type: "tool_search", ID: "tsc_1", Name: "tool_search", Input: json.RawMessage(`{"paths":["nothing"]}`)},
		{Type: "tool_search_result", ToolUseID: "tsc_1"},
		{Type: "text", Text: "No matching tool."},
	})
	req, err := buildResponsesRequest(params, responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	input, err := json.Marshal(req.Input)
	must.NoError(err)
	must.Contains(string(input), `{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[]}`)
}

func TestBuildResponsesRequest_ToolSearchDropsUnreconstructableSearches(t *testing.T) {
	blocks := responsesToolSearchTurnBlocks
	cases := []struct {
		name   string
		blocks func() []types.ContentBlock
		q      quirks.ProviderQuirks
		params func(types.StreamParams) types.StreamParams
	}{
		{name: "quirks do not support tool search", blocks: blocks, q: responsesToolSearchQuirks(false)},
		{name: "loop did not ask for tool search", blocks: blocks, q: responsesToolSearchQuirks(true),
			params: func(p types.StreamParams) types.StreamParams { p.ToolSearch = false; return p }},
		{name: "loaded tool missing from the request", blocks: blocks, q: responsesToolSearchQuirks(true),
			params: func(p types.StreamParams) types.StreamParams { p.Tools = p.Tools[:1]; return p }},
		{name: "failed search with code and message", q: responsesToolSearchQuirks(true), blocks: func() []types.ContentBlock {
			b := blocks()
			b[1] = types.ContentBlock{Type: "tool_search_result", ToolUseID: "tsc_1", IsError: true, Name: "unavailable", Content: "search failed"}
			return b
		}},
		{name: "failed search with neither code nor message", q: responsesToolSearchQuirks(true), blocks: func() []types.ContentBlock {
			b := blocks()
			b[1] = types.ContentBlock{Type: "tool_search_result", ToolUseID: "tsc_1", IsError: true}
			return b
		}},
		{name: "search recorded by another provider", q: responsesToolSearchQuirks(true), blocks: func() []types.ContentBlock {
			return responsesAnthropicSearchBlocks("srvtoolu_1")
		}},
		{name: "result without its call", q: responsesToolSearchQuirks(true), blocks: func() []types.ContentBlock { return blocks()[1:] }},
		{name: "call without its result", q: responsesToolSearchQuirks(true), blocks: func() []types.ContentBlock {
			b := blocks()
			return []types.ContentBlock{b[0], b[2]}
		}},
		{name: "repeated call id", q: responsesToolSearchQuirks(true), blocks: func() []types.ContentBlock {
			b := blocks()
			return []types.ContentBlock{b[0], b[0], b[1], b[2]}
		}},
		{name: "search input not JSON", q: responsesToolSearchQuirks(true), blocks: func() []types.ContentBlock {
			b := blocks()
			b[0].Input = json.RawMessage(`{"paths":`)
			return b
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			params := responsesToolSearchParams()
			params.Messages = responsesToolSearchHistory(tc.blocks())
			if tc.params != nil {
				params = tc.params(params)
			}
			req, err := buildResponsesRequest(params, tc.q, nil, "")
			must.NoError(err)
			body, err := json.Marshal(req)
			must.NoError(err)

			var kinds []string
			for _, raw := range requestInput(t, body) {
				var item struct {
					Type string `json:"type"`
				}
				must.NoError(json.Unmarshal(raw, &item))
				kinds = append(kinds, item.Type)
			}
			must.Equal([]string{"message", "function_call", "function_call_output"}, kinds)
		})
	}
}

// responsesAnthropicSearchBlocks is a search pair as the Anthropic adapter
// records it, followed by the call that uses the loaded tool.
func responsesAnthropicSearchBlocks(id string) []types.ContentBlock {
	return []types.ContentBlock{
		{Type: "tool_search", ID: id, Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"jira","limit":5}`)},
		{Type: "tool_search_result", ToolUseID: id, ToolReferences: []string{"mcp_jira_create_issue"}},
		{Type: "tool_use", ID: "call_1", Name: "mcp_jira_create_issue", Input: json.RawMessage(`{"title":"Broken build"}`)},
	}
}

func TestBuildResponsesRequest_ToolSearchRebuildsOnlyOwnSearchesFromMixedHistory(t *testing.T) {
	must := require.New(t)
	params := responsesToolSearchParams()
	blocks := responsesAnthropicSearchBlocks("srvtoolu_1")
	blocks = append(blocks, responsesToolSearchTurnBlocks()[:2]...)
	params.Messages = responsesToolSearchHistory(blocks)
	req, err := buildResponsesRequest(params, responsesToolSearchQuirks(true), nil, "")
	must.NoError(err)
	body, err := json.Marshal(req)
	must.NoError(err)

	var calls, outputs int
	for _, raw := range requestInput(t, body) {
		var item struct {
			Type string `json:"type"`
		}
		must.NoError(json.Unmarshal(raw, &item))
		switch item.Type {
		case "tool_search_call":
			calls++
		case "tool_search_output":
			outputs++
		}
	}
	must.Equal(1, calls, "only the hosted search call is rebuilt")
	must.Equal(1, outputs, "only the hosted search output is rebuilt")
	must.NotContains(string(body), "tool_search_tool_bm25")
	must.NotContains(string(body), "srvtoolu_1")
	must.Contains(string(body), `"arguments":{"paths":["mcp_jira_create_issue"]}`)
}

// TestOpenAIResponsesAdapter_ToolSearchReplaysItemsInPlace streams a turn
// that searches and calls a loaded tool, then pins that the next request
// replays the stored search items verbatim ahead of the function call.
func TestOpenAIResponsesAdapter_ToolSearchReplaysItemsInPlace(t *testing.T) {
	must := require.New(t)
	script := newResponsesScript(t, responsesToolSearchSSE())
	adapter := script.adapter(nil)
	params := responsesToolSearchParams()
	params.Model = responsesReplayModel

	ch, err := adapter.Stream(t.Context(), params)
	must.NoError(err)
	events := drainStream(t, ch)
	assistant := assistantFromEvents(events)
	must.Equal(responsesToolSearchTurnBlocks(), assistant.Content)
	stored := storedReplayItems(t, messageComplete(t, events))
	must.Len(stored, 3)

	params.Messages = responsesToolSearchHistory(assistant.Content)
	params.Messages[1].ReplayFields = assistant.ReplayFields
	ch, err = adapter.Stream(t.Context(), params)
	must.NoError(err)
	drainStream(t, ch)

	input := requestInput(t, script.lastBody(t))
	must.Len(input, 5)
	for i, raw := range stored {
		must.True(jsonValuesEqual(t, raw, input[i+1]), "input item %d is not the stored item: %s", i+1, input[i+1])
	}
	assertCallsPaired(t, input)
}

// TestOpenAIResponsesAdapter_ToolSearchReplayNeedsHostedSearch pins that a
// stored turn holding search items is reconstructed, without them, when
// the next request has no hosted search.
func TestOpenAIResponsesAdapter_ToolSearchReplayNeedsHostedSearch(t *testing.T) {
	must := require.New(t)
	script := newResponsesScript(t, responsesToolSearchSSE())
	adapter := script.adapter(nil)
	params := responsesToolSearchParams()
	params.Model = responsesReplayModel

	ch, err := adapter.Stream(t.Context(), params)
	must.NoError(err)
	assistant := assistantFromEvents(drainStream(t, ch))

	params.ToolSearch = false
	params.Messages = responsesToolSearchHistory(assistant.Content)
	params.Messages[1].ReplayFields = assistant.ReplayFields
	ch, err = adapter.Stream(t.Context(), params)
	must.NoError(err)
	drainStream(t, ch)

	body := string(script.lastBody(t))
	must.NotContains(body, "tool_search")
	must.NotContains(body, `"id":`)
}

func TestResponsesInput_ToolSearchItemsRoundTrip(t *testing.T) {
	truthy := true
	cases := []responsesInput{
		{Type: "tool_search_call", SearchArguments: json.RawMessage(`{"paths":["crm"]}`)},
		{Type: "tool_search_output", LoadedTools: []responsesTool{{
			Type: "function", Name: "list_open_orders", Parameters: json.RawMessage(`{"type":"object"}`), DeferLoading: &truthy,
		}}},
		{Type: "tool_search_output", LoadedTools: []responsesTool{}},
		{Type: "tool_search_output", LoadedTools: []responsesTool{{
			Type: responsesNamespaceType, Name: "crm", Description: "CRM tools", Tools: []responsesTool{{
				Type: "function", Name: "list_open_orders", Parameters: json.RawMessage(`{"type":"object"}`), DeferLoading: &truthy,
			}},
		}}},
		{Type: "function_call", CallID: "call_1", Name: "list_open_orders", Arguments: "{}", Namespace: "crm"},
	}
	for _, in := range cases {
		t.Run(in.Type, func(t *testing.T) {
			must := require.New(t)
			body, err := json.Marshal(in)
			must.NoError(err)
			var back responsesInput
			must.NoError(json.Unmarshal(body, &back))
			must.Equal(in, back)
		})
	}
}

// hostedSearchEntryCount counts the hosted tool_search entries in a
// Responses request body's tools array.
func hostedSearchEntryCount(t *testing.T, body []byte) int {
	t.Helper()
	must := require.New(t)
	var decoded struct {
		Tools []map[string]any `json:"tools"`
	}
	must.NoError(json.Unmarshal(body, &decoded))
	n := 0
	for _, tool := range decoded.Tools {
		if tool["type"] == responsesToolSearchType {
			n++
		}
	}
	return n
}

func TestOpenAIResponsesAdapter_HostedSearchEntryOnlyForOpenAIEndpointProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		want    int
	}{
		{"openai", types.EndpointProfileOpenAI, 1},
		{"generic", types.EndpointProfileGeneric, 0},
		{"azure-openai", types.EndpointProfileAzureOpenAI, 0},
		{"bedrock-mantle", types.EndpointProfileBedrockMantle, 0},
		{"unstated", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			script := newResponsesScript(t)
			adapter := script.adapter(nil)
			adapter.EndpointProfile = tc.profile

			ch, err := adapter.Stream(t.Context(), responsesToolSearchParams())
			must.NoError(err)
			drainStream(t, ch)

			body := script.lastBody(t)
			must.Equal(tc.want, hostedSearchEntryCount(t, body))
			if tc.want == 0 {
				must.NotContains(string(body), "defer_loading", "no tool is deferred without a hosted search")
			}
		})
	}
}

func TestOpenAIResponsesAdapter_DebugLogCarriesEndpointProfile(t *testing.T) {
	must := require.New(t)
	script := newResponsesScript(t)
	var buf bytes.Buffer
	adapter := script.adapter(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	adapter.EndpointProfile = types.EndpointProfileGeneric

	ch, err := adapter.Stream(t.Context(), responsesToolSearchParams())
	must.NoError(err)
	drainStream(t, ch)

	must.Contains(buf.String(), "openai-responses quirks resolved")
	must.Contains(buf.String(), `"provider.endpoint_profile":"generic"`)
}

func TestBatchAdapter_ResponsesHostedSearchFollowsEndpointProfileArgument(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inner    ProviderAdapter
		endpoint string
		want     int
	}{
		{"openai", NewOpenAIResponsesAdapter(staticBearer("k"), "http://unused.invalid", OpenAIAuthConfig{}), types.EndpointProfileOpenAI, 1},
		{"generic", NewOpenAIResponsesAdapter(staticBearer("k"), "http://unused.invalid", OpenAIAuthConfig{}), types.EndpointProfileGeneric, 0},
		{"unstated with a wrapped inner adapter", &fakeAdapter{}, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			a := NewBatchAdapter(tc.inner, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-responses", tc.endpoint, "run-test")

			body, _, err := a.marshalRequestBody(t.Context(), responsesToolSearchParams())
			must.NoError(err)

			must.Equal(tc.want, hostedSearchEntryCount(t, body))
		})
	}
}
