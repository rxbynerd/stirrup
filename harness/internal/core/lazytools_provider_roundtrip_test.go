package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

const (
	roundTripResponsesModel = "gpt-5.5"
	roundTripJiraTool       = "mcp_jira_create_issue"

	roundTripSearchCall   = `{"type":"tool_search_call","id":"tsc_1","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["mcp_jira_create_issue"]}}`
	roundTripSearchOutput = `{"type":"tool_search_output","id":"tso_1","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"mcp_jira_create_issue","description":"Create a Jira issue","defer_loading":true,"parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}]}`
	roundTripFunctionCall = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp_jira_create_issue","arguments":"{\"title\":\"Broken build\"}","status":"completed"}`
)

// responsesSearchTurnSSE is one Responses turn that runs the hosted tool
// search, loads the Jira tool and calls it.
func responsesSearchTurnSSE() string {
	return responsesSSEEvent("response.output_item.done", `{"output_index":0,"item":`+roundTripSearchCall+`}`) +
		responsesSSEEvent("response.output_item.done", `{"output_index":1,"item":`+roundTripSearchOutput+`}`) +
		responsesSSEEvent("response.output_item.done", `{"output_index":2,"item":`+roundTripFunctionCall+`}`) +
		responsesSSEEvent("response.completed", `{"response":{"status":"completed","output":[`+roundTripSearchCall+`,`+roundTripSearchOutput+`,`+roundTripFunctionCall+`]}}`)
}

// requestInputItems decodes the input items of a recorded request body.
func requestInputItems(t *testing.T, body string) []map[string]any {
	t.Helper()
	must := require.New(t)
	var req struct {
		Input []map[string]any `json:"input"`
	}
	must.NoError(json.Unmarshal([]byte(body), &req))
	return req.Input
}

// searchItems returns the tool search items of a request input, without
// the item IDs a reconstruction cannot know.
func searchItems(items []map[string]any) []map[string]any {
	var out []map[string]any
	for _, item := range items {
		if item["type"] == "tool_search_call" || item["type"] == "tool_search_output" {
			delete(item, "id")
			out = append(out, item)
		}
	}
	return out
}

// loadedToolNames returns the tool names a tool_search_output item lists;
// a rebuilt item carries the request's own definitions of them.
func loadedToolNames(item map[string]any) []string {
	var names []string
	tools, _ := item["tools"].([]any)
	for _, t := range tools {
		if def, ok := t.(map[string]any); ok {
			names = append(names, fmt.Sprint(def["name"]))
		}
	}
	return names
}

func originalSearchItems(t *testing.T) []map[string]any {
	t.Helper()
	must := require.New(t)
	var call, output map[string]any
	must.NoError(json.Unmarshal([]byte(roundTripSearchCall), &call))
	must.NoError(json.Unmarshal([]byte(roundTripSearchOutput), &output))
	return searchItems([]map[string]any{call, output})
}

// TestLoop_LazyTools_ResponsesHostedSearchRoundTrips streams a hosted tool
// search through the real OpenAI Responses adapter and the loop's own
// block conversion, then pins that the search items reach the next
// request: verbatim through the adapter's stored output items, and
// rebuilt from the persisted blocks alone.
func TestLoop_LazyTools_ResponsesHostedSearchRoundTrips(t *testing.T) {
	must := require.New(t)
	server := newResponsesTurnServer(t, responsesSearchTurnSSE(), responsesFinalTurnSSE(), responsesFinalTurnSSE())
	adapter := server.adapter()

	loop := buildTestLoop(nil)
	loop.Provider = adapter
	registry := loop.Tools.(*tool.Registry)
	calls := 0
	registry.Register(&tool.Tool{
		Name:        roundTripJiraTool,
		Description: "Create a Jira issue",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			calls++
			return "created", nil
		},
	})
	registry.Register(builtins.ToolSearchTool(types.LazyToolLoadingConfig{}))
	loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{MCPServers: []string{"jira"}})
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://TEST"}
	config.ModelRouter = types.ModelRouterConfig{Type: "static", Provider: "openai-responses", Model: roundTripResponsesModel}
	loop.ToolSearch = buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
	must.True(loop.ToolSearch("", roundTripResponsesModel).Supported, "the fixture model must have hosted tool search")
	loop.Router = router.NewStaticRouter("openai-responses", roundTripResponsesModel)
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal(1, calls, "the tool the hosted search loaded is dispatched")

	blocks := rec.turnRecords[0].ModelOutput
	must.Equal(types.ContentBlock{Type: "tool_search", ID: "tsc_1", Name: "tool_search", Input: json.RawMessage(`{"paths":["mcp_jira_create_issue"]}`)}, blocks[0])
	must.Equal(types.ContentBlock{Type: "tool_search_result", ToolUseID: "tsc_1", ToolReferences: []string{roundTripJiraTool}}, blocks[1])

	want := originalSearchItems(t)
	must.Equal(want, searchItems(requestInputItems(t, server.requests()[1])), "the next request replays the search")

	history := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "file a ticket"}}},
		{Role: "assistant", Content: blocks},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "created"}}},
	}
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:      roundTripResponsesModel,
		MaxTokens:  1024,
		ToolSearch: true,
		Tools:      loop.LazyTools.Visible(true),
		Messages:   history,
	})
	must.NoError(err)
	for range ch {
	}
	rebuilt := requestInputItems(t, server.requests()[2])
	rebuiltSearch := searchItems(rebuilt)
	must.Len(rebuiltSearch, 2)
	must.Equal(want[0], rebuiltSearch[0], "the persisted blocks alone rebuild the search call")
	must.Equal(loadedToolNames(want[1]), loadedToolNames(rebuiltSearch[1]), "the rebuilt output loads the same tools")
	var order []string
	for _, item := range rebuilt {
		order = append(order, fmt.Sprint(item["type"]))
	}
	must.Equal("message tool_search_call tool_search_output function_call function_call_output", strings.Join(order, " "))
}
