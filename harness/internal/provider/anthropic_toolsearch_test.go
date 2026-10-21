package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

const anthropicToolSearchTestModel = "claude-sonnet-5-5"

// anthropicToolSearchTools is an eager built-in followed by two deferred MCP tools.
func anthropicToolSearchTools() []types.ToolDefinition {
	return []types.ToolDefinition{
		{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "mcp_github_search_issues", Description: "Search GitHub issues", InputSchema: json.RawMessage(`{"type":"object"}`), Deferred: true},
		{Name: "mcp_jira_create_issue", Description: "Create a Jira issue", InputSchema: json.RawMessage(`{"type":"object"}`), Deferred: true},
	}
}

// anthropicToolSearchHistory is a turn in which the model searched, loaded one
// deferred tool and called it, followed by that call's result.
func anthropicToolSearchHistory(result types.ContentBlock) []types.Message {
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "find the open bug"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "text", Text: "Searching."},
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"github issues"}`)},
			result,
			{Type: "tool_use", ID: "toolu_1", Name: "mcp_github_search_issues", Input: json.RawMessage(`{"q":"bug"}`)},
		}},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "toolu_1", Content: "#42"}}},
	}
}

func anthropicToolSearchLoadedResult(names ...string) types.ContentBlock {
	return types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", ToolReferences: names}
}

func anthropicToolSearchParams(history []types.Message) types.StreamParams {
	return types.StreamParams{
		Model:      anthropicToolSearchTestModel,
		MaxTokens:  1024,
		Tools:      anthropicToolSearchTools(),
		ToolSearch: true,
		Messages:   history,
	}
}

func anthropicToolSearchWireTools(t *testing.T, body []byte) []string {
	t.Helper()
	must := require.New(t)
	var req struct {
		Tools []json.RawMessage `json:"tools"`
	}
	must.NoError(json.Unmarshal(body, &req), "decode request body: %s", body)
	out := make([]string, len(req.Tools))
	for i, tool := range req.Tools {
		out[i] = string(tool)
	}
	return out
}

func anthropicToolSearchBlockStrings(raw []json.RawMessage) []string {
	out := make([]string, len(raw))
	for i, b := range raw {
		out[i] = string(b)
	}
	return out
}

func TestBuildAnthropicRequest_ToolSearch_SearchToolFirstAndDeferredFlagged(t *testing.T) {
	must := require.New(t)
	params := anthropicToolSearchParams(nil)
	body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))

	want := []string{
		`{"type":"tool_search_tool_bm25_20251119","name":"tool_search_tool_bm25"}`,
		`{"name":"read_file","description":"Read a file","input_schema":{"type":"object"}}`,
		`{"name":"mcp_github_search_issues","description":"Search GitHub issues","input_schema":{"type":"object"},"defer_loading":true}`,
		`{"name":"mcp_jira_create_issue","description":"Create a Jira issue","input_schema":{"type":"object"},"defer_loading":true}`,
	}
	must.Equal(want, anthropicToolSearchWireTools(t, body))
}

func TestBuildAnthropicRequest_ToolSearch_InputExamplesSurviveDeferral(t *testing.T) {
	must := require.New(t)
	params := anthropicToolSearchParams(nil)
	params.Tools[1].Presentation = &types.ToolPresentation{
		InputExamples: []json.RawMessage{json.RawMessage(`{"q":"bug"}`)},
	}
	body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))

	must.JSONEq(
		`{"name":"mcp_github_search_issues","description":"Search GitHub issues","input_schema":{"type":"object"},"input_examples":[{"q":"bug"}],"defer_loading":true}`,
		anthropicToolSearchWireTools(t, body)[2],
	)
}

func TestBuildAnthropicRequest_ToolSearch_SearchToolNeverDeferred(t *testing.T) {
	must := require.New(t)
	params := anthropicToolSearchParams(nil)
	for i := range params.Tools {
		params.Tools[i].Deferred = true
	}
	body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))

	tools := anthropicToolSearchWireTools(t, body)
	must.Len(tools, 4)
	must.JSONEq(`{"type":"tool_search_tool_bm25_20251119","name":"tool_search_tool_bm25"}`, tools[0])
	for _, tool := range tools[1:] {
		must.Contains(tool, `"defer_loading":true`)
	}
}

func TestBuildAnthropicRequest_ToolSearch_VariantSelectsSearchTool(t *testing.T) {
	cases := map[string]string{
		"bm25":  `{"type":"tool_search_tool_bm25_20251119","name":"tool_search_tool_bm25"}`,
		"regex": `{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}`,
		"":      `{"type":"tool_search_tool_bm25_20251119","name":"tool_search_tool_bm25"}`,
	}
	for variant, want := range cases {
		t.Run("variant="+variant, func(t *testing.T) {
			must := require.New(t)
			q := quirks.ProviderQuirks{ToolSearch: quirks.ToolSearchCapability{Supported: true, Variant: variant}}
			body := anthropicWireBody(t, anthropicToolSearchParams(nil), true, q)
			must.Equal(want, anthropicToolSearchWireTools(t, body)[0])
		})
	}
}

// TestBuildAnthropicRequest_ToolSearch_GateOffSendsEveryToolLoaded pins
// that without both the loop's request and the model's capability (and at
// least one deferred tool) the request carries no search tool, no
// defer_loading, and none of the recorded search blocks.
func TestBuildAnthropicRequest_ToolSearch_GateOffSendsEveryToolLoaded(t *testing.T) {
	notRequested := anthropicToolSearchParams(anthropicToolSearchHistory(anthropicToolSearchLoadedResult("mcp_github_search_issues")))
	notRequested.ToolSearch = false

	unsupportedModel := anthropicToolSearchParams(anthropicToolSearchHistory(anthropicToolSearchLoadedResult("mcp_github_search_issues")))
	unsupportedModel.Model = "claude-sonnet-4-20250514"

	nothingDeferred := anthropicToolSearchParams(anthropicToolSearchHistory(anthropicToolSearchLoadedResult("mcp_github_search_issues")))
	for i := range nothingDeferred.Tools {
		nothingDeferred.Tools[i].Deferred = false
	}

	cases := map[string]types.StreamParams{
		"not requested":     notRequested,
		"unsupported model": unsupportedModel,
		"nothing deferred":  nothingDeferred,
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
			wire := string(body)
			must.NotContains(wire, "tool_search_tool_bm25")
			must.NotContains(wire, "defer_loading")
			must.NotContains(wire, "server_tool_use")
			must.NotContains(wire, "srvtoolu_1")
			must.Len(anthropicToolSearchWireTools(t, body), 3)
			must.Equal([]string{
				`{"type":"text","text":"Searching."}`,
				`{"type":"tool_use","id":"toolu_1","name":"mcp_github_search_issues","input":{"q":"bug"}}`,
			}, anthropicToolSearchBlockStrings(assistantWireBlocks(t, body, 1)))
		})
	}
}

func TestBuildAnthropicRequest_ToolSearch_GateOffOmitsSearchOnlyTurn(t *testing.T) {
	must := require.New(t)
	history := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "find it"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"x"}`)},
			anthropicToolSearchLoadedResult(),
		}},
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Continue."}}},
	}
	got, err := json.Marshal(translateMessagesAnthropic(history, quirks.StructuredToolResultCapability{}, false))
	must.NoError(err)
	must.JSONEq(`[{"role":"user","content":[{"type":"text","text":"find it"},{"type":"text","text":"Continue."}]}]`, string(got))
}

// TestBuildAnthropicRequest_ToolSearch_ReplayWireShape pins the exact
// server_tool_use and tool_search_tool_result shapes replayed in place for
// a successful, an empty and a failed search.
func TestBuildAnthropicRequest_ToolSearch_ReplayWireShape(t *testing.T) {
	const call = `{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"github issues"}}`
	cases := []struct {
		name   string
		result types.ContentBlock
		want   string
	}{
		{
			name:   "loaded",
			result: anthropicToolSearchLoadedResult("mcp_github_search_issues", "mcp_jira_create_issue"),
			want: `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result",` +
				`"tool_references":[{"type":"tool_reference","tool_name":"mcp_github_search_issues"},{"type":"tool_reference","tool_name":"mcp_jira_create_issue"}]}}`,
		},
		{
			name:   "no match",
			result: anthropicToolSearchLoadedResult(),
			want:   `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`,
		},
		{
			name: "error with code and message",
			result: types.ContentBlock{
				Type: "tool_search_result", ToolUseID: "srvtoolu_1", IsError: true,
				Name: "invalid_tool_input", Content: "Query too long: exceeds 500 characters",
			},
			want: `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_result_error",` +
				`"error_code":"invalid_tool_input","error_message":"Query too long: exceeds 500 characters"}}`,
		},
		{
			name:   "error with code only",
			result: types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", IsError: true, Name: "too_many_requests"},
			want:   `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_result_error","error_code":"too_many_requests","error_message":""}}`,
		},
		{
			name:   "error with message only",
			result: types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", IsError: true, Content: "Search timed out"},
			want:   `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_result_error","error_code":"unavailable","error_message":"Search timed out"}}`,
		},
		{
			name:   "error with neither code nor message",
			result: types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", IsError: true},
			want:   `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_result_error","error_code":"unavailable","error_message":""}}`,
		},
		{
			name:   "message colon text is not split",
			result: types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", IsError: true, Name: "unavailable", Content: "upstream: timed out"},
			want:   `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_result_error","error_code":"unavailable","error_message":"upstream: timed out"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			params := anthropicToolSearchParams(anthropicToolSearchHistory(tc.result))
			body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
			must.Equal([]string{
				`{"type":"text","text":"Searching."}`,
				call,
				tc.want,
				`{"type":"tool_use","id":"toolu_1","name":"mcp_github_search_issues","input":{"q":"bug"}}`,
			}, anthropicToolSearchBlockStrings(assistantWireBlocks(t, body, 1)))
		})
	}
}

func TestBuildAnthropicRequest_ToolSearch_ReplaysEmptyInputAsObject(t *testing.T) {
	must := require.New(t)
	history := anthropicToolSearchHistory(anthropicToolSearchLoadedResult())
	history[1].Content[1].Input = nil
	params := anthropicToolSearchParams(history)
	body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
	must.JSONEq(
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{}}`,
		string(assistantWireBlocks(t, body, 1)[1]),
	)
}

// TestBuildAnthropicRequest_ToolSearch_DropsUnreplayableSearchBlocks pins
// that only a matched pair naming an Anthropic search tool is replayed.
func TestBuildAnthropicRequest_ToolSearch_DropsUnreplayableSearchBlocks(t *testing.T) {
	cases := map[string][]types.ContentBlock{
		"unanswered call": {
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25"},
		},
		"orphaned result": {
			anthropicToolSearchLoadedResult("mcp_github_search_issues"),
		},
		"result for another call": {
			{Type: "tool_search", ID: "srvtoolu_2", Name: "tool_search_tool_bm25"},
			anthropicToolSearchLoadedResult("mcp_github_search_issues"),
		},
		"foreign search tool": {
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search"},
			anthropicToolSearchLoadedResult("mcp_github_search_issues"),
		},
	}
	for name, searchBlocks := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			content := append([]types.ContentBlock{{Type: "text", Text: "Searching."}}, searchBlocks...)
			history := []types.Message{
				{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "go"}}},
				{Role: "assistant", Content: content},
			}
			params := anthropicToolSearchParams(history)
			body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
			must.Equal([]string{`{"type":"text","text":"Searching."}`}, anthropicToolSearchBlockStrings(assistantWireBlocks(t, body, 1)))
		})
	}
}

// anthropicToolSearchSSE is a streamed turn in which the model searches, the search
// result arrives inline on content_block_start, and the model calls the
// loaded tool.
func anthropicToolSearchSSE(resultContent string) string {
	return joinLines(
		makeSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Searching."}}`),
		makeSSE("content_block_stop", `{"type":"content_block_stop","index":0}`),
		makeSSE("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25"}}`),
		makeSSE("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"github "}}`),
		makeSSE("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"issues\",\"limit\":3}"}}`),
		makeSSE("content_block_stop", `{"type":"content_block_stop","index":1}`),
		makeSSE("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":`+resultContent+`}}`),
		makeSSE("content_block_stop", `{"type":"content_block_stop","index":2}`),
		makeSSE("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"mcp_github_search_issues","input":{}}}`),
		makeSSE("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"bug\"}"}}`),
		makeSSE("content_block_stop", `{"type":"content_block_stop","index":3}`),
		makeSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`),
		makeSSE("message_stop", `{"type":"message_stop"}`),
	)
}

// TestAnthropicAdapter_ToolSearchStream_RoundTrips streams each documented
// result form, checks the events, and replays the persisted blocks: the
// server_tool_use and tool_search_tool_result blocks go back as the API sent
// them, except that a failed search with no error code replays with the
// fallback code.
func TestAnthropicAdapter_ToolSearchStream_RoundTrips(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		wantEvent  types.StreamEvent
		wantBlock  types.ContentBlock
		wantReplay string
	}{
		{
			name:       "loaded",
			content:    `{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"mcp_github_search_issues"}]}`,
			wantEvent:  types.StreamEvent{ToolReferences: []string{"mcp_github_search_issues"}},
			wantBlock:  types.ContentBlock{ToolReferences: []string{"mcp_github_search_issues"}},
			wantReplay: `{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"mcp_github_search_issues"}]}`,
		},
		{
			name:       "no match",
			content:    `{"type":"tool_search_tool_search_result","tool_references":[]}`,
			wantReplay: `{"type":"tool_search_tool_search_result","tool_references":[]}`,
		},
		{
			name:       "error with code and message",
			content:    `{"type":"tool_search_tool_result_error","error_code":"too_many_requests","error_message":"Rate limit exceeded"}`,
			wantEvent:  types.StreamEvent{IsError: true, Name: "too_many_requests", Text: "Rate limit exceeded"},
			wantBlock:  types.ContentBlock{IsError: true, Name: "too_many_requests", Content: "Rate limit exceeded"},
			wantReplay: `{"type":"tool_search_tool_result_error","error_code":"too_many_requests","error_message":"Rate limit exceeded"}`,
		},
		{
			name:       "error with code only",
			content:    `{"type":"tool_search_tool_result_error","error_code":"execution_time_exceeded"}`,
			wantEvent:  types.StreamEvent{IsError: true, Name: "execution_time_exceeded"},
			wantBlock:  types.ContentBlock{IsError: true, Name: "execution_time_exceeded"},
			wantReplay: `{"type":"tool_search_tool_result_error","error_code":"execution_time_exceeded","error_message":""}`,
		},
		{
			name:       "error with message only",
			content:    `{"type":"tool_search_tool_result_error","error_message":"Search timed out"}`,
			wantEvent:  types.StreamEvent{IsError: true, Text: "Search timed out"},
			wantBlock:  types.ContentBlock{IsError: true, Content: "Search timed out"},
			wantReplay: `{"type":"tool_search_tool_result_error","error_code":"unavailable","error_message":"Search timed out"}`,
		},
		{
			name:       "error with neither code nor message",
			content:    `{"type":"tool_search_tool_result_error"}`,
			wantEvent:  types.StreamEvent{IsError: true},
			wantBlock:  types.ContentBlock{IsError: true},
			wantReplay: `{"type":"tool_search_tool_result_error","error_code":"unavailable","error_message":""}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			events := streamAnthropicSSE(t, anthropicToolSearchSSE(tc.content))
			must.Equal([]string{"text_delta", "tool_search", "tool_search_result", "tool_call", "message_complete"}, eventTypes(events))

			search := events[1]
			check.Equal("srvtoolu_1", search.ID)
			check.Equal("tool_search_tool_bm25", search.Name)
			check.Equal(map[string]any{"query": "github issues", "limit": float64(3)}, search.Input)

			wantEvent := tc.wantEvent
			wantEvent.Type, wantEvent.ID = "tool_search_result", "srvtoolu_1"
			check.Equal(wantEvent, events[2])
			check.Equal("tool_use", events[4].StopReason)

			wantBlock := tc.wantBlock
			wantBlock.Type, wantBlock.ToolUseID = "tool_search_result", "srvtoolu_1"
			history := []types.Message{
				{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "find the open bug"}}},
				{Role: "assistant", Content: []types.ContentBlock{
					{Type: "text", Text: "Searching."},
					{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"github issues","limit":3}`)},
					wantBlock,
					{Type: "tool_use", ID: "toolu_1", Name: "mcp_github_search_issues", Input: json.RawMessage(`{"q":"bug"}`)},
				}},
			}
			params := anthropicToolSearchParams(history)
			body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
			got := assistantWireBlocks(t, body, 1)
			must.Len(got, 4)
			must.JSONEq(`{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"github issues","limit":3}}`, string(got[1]))
			must.JSONEq(`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":`+tc.wantReplay+`}`, string(got[2]))
		})
	}
}

func TestAnthropicAdapter_ToolSearchStream_UsesStartInputWithoutDeltas(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_regex","input":{"pattern":"weather"}}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	))
	must.Equal([]string{"tool_search", "message_complete"}, eventTypes(events))
	must.Equal("tool_search_tool_regex", events[0].Name)
	must.Equal(map[string]any{"pattern": "weather"}, events[0].Input)
}

func TestAnthropicAdapter_ToolSearchStream_IgnoresOtherServerTools(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"x\"}"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	))
	must.Equal([]string{"message_complete"}, eventTypes(events))
}

func TestAnthropicAdapter_ToolSearchStream_UnknownResultLoadsNothing(t *testing.T) {
	for name, content := range map[string]string{
		"unknown type": `{"type":"tool_search_tool_future_result","tools":["x"]}`,
		"not object":   `"text"`,
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			events := streamAnthropicSSE(t, joinLines(
				makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":`+content+`}}`),
				makeSSE("content_block_stop", `{"index":0}`),
				makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
				makeSSE("message_stop", `{}`),
			))
			must.Equal([]string{"tool_search_result", "message_complete"}, eventTypes(events))
			must.Equal(types.StreamEvent{Type: "tool_search_result", ID: "srvtoolu_1"}, events[0])
		})
	}
}

// TestAnthropicAdapter_ToolSearchStream_EnforcesSizeLimits pins that the
// tool input cap bounds both a streamed search input and an inline result.
func TestAnthropicAdapter_ToolSearchStream_EnforcesSizeLimits(t *testing.T) {
	t.Run("streamed input", func(t *testing.T) {
		must := require.New(t)
		chunk := strings.Repeat("a", maxToolInputSize/2+1)
		events := streamAnthropicSSE(t, joinLines(
			makeSSE("content_block_start", `{"index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25"}}`),
			makeSSE("content_block_delta", fmt.Sprintf(`{"index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, chunk)),
			makeSSE("content_block_delta", fmt.Sprintf(`{"index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, chunk)),
			makeSSE("content_block_stop", `{"index":0}`),
		))
		must.Equal([]string{"error"}, eventTypes(events))
		must.ErrorContains(events[0].Error, "tool input exceeds")
	})
	t.Run("inline result", func(t *testing.T) {
		must := require.New(t)
		name := strings.Repeat("a", maxToolInputSize)
		events := streamAnthropicSSE(t, joinLines(
			makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"`+name+`"}]}}}`),
			makeSSE("content_block_stop", `{"index":0}`),
		))
		must.Equal([]string{"error"}, eventTypes(events))
		must.ErrorContains(events[0].Error, "tool_search_tool_result block exceeds")
	})
}

// TestBuildAnthropicRequest_ToolSearch_ForeignSearchEmptiesMessage pins that
// a message holding only another provider's search is omitted and the turns
// either side of it are joined.
func TestBuildAnthropicRequest_ToolSearch_ForeignSearchEmptiesMessage(t *testing.T) {
	must := require.New(t)
	history := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "find it"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "tool_search", ID: "call_1", Name: "tool_search", Input: json.RawMessage(`{"paths":["mcp"]}`)},
			{Type: "tool_search_result", ToolUseID: "call_1", ToolReferences: []string{"mcp_github_search_issues"}},
		}},
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Continue."}}},
	}
	got, err := json.Marshal(translateMessagesAnthropic(history, quirks.StructuredToolResultCapability{}, true))
	must.NoError(err)
	must.JSONEq(`[{"role":"user","content":[{"type":"text","text":"find it"},{"type":"text","text":"Continue."}]}]`, string(got))
}

func TestAnthropicAdapter_ToolSearchUnsupported_LogsAndSendsEveryToolLoaded(t *testing.T) {
	cases := map[string]struct {
		model     string
		wantDebug int
		wantTools int
	}{
		"unsupported model": {model: "claude-sonnet-4-20250514", wantDebug: 1, wantTools: 3},
		"supported model":   {model: anthropicToolSearchTestModel, wantDebug: 0, wantTools: 4},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			var rawBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rawBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
			}))
			t.Cleanup(srv.Close)

			var buf bytes.Buffer
			adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
			adapter.baseURL = srv.URL
			adapter.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			params := anthropicToolSearchParams(nil)
			params.Model = tc.model
			ch, err := adapter.Stream(context.Background(), params)
			must.NoError(err)
			for range ch {
			}

			var records []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var rec map[string]any
				must.NoError(json.Unmarshal([]byte(line), &rec))
				if rec["msg"] == "anthropic tool search not supported; sending every tool loaded" {
					records = append(records, rec)
				}
			}
			must.Len(records, tc.wantDebug)
			for _, rec := range records {
				must.Equal("anthropic", rec["provider.type"])
				must.Equal(tc.model, rec["provider.model"])
			}
			must.Len(anthropicToolSearchWireTools(t, rawBody), tc.wantTools)
			if tc.wantDebug > 0 {
				must.NotContains(string(rawBody), "defer_loading")
				must.NotContains(string(rawBody), "tool_search_tool_bm25")
			}
		})
	}
}

func TestBuildAnthropicRequest_ToolSearch_NothingDeferredSendsNoSearchTool(t *testing.T) {
	must := require.New(t)
	params := anthropicToolSearchParams(nil)
	for i := range params.Tools {
		params.Tools[i].Deferred = false
	}
	body := anthropicWireBody(t, params, true, quirks.DefaultRegistry().Resolve("anthropic", params.Model))
	must.Len(anthropicToolSearchWireTools(t, body), 3)
	must.NotContains(string(body), "defer_loading")
	must.NotContains(string(body), "tool_search_tool_bm25")
}

func TestAnthropicAdapter_ToolSearchStream_UnrecognisedResultWarns(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ev := parseAnthropicToolSearchResult(context.Background(), logger, "srvtoolu_1", json.RawMessage(`{"type":"tool_search_tool_future_result"}`))

	must.Equal(types.StreamEvent{Type: "tool_search_result", ID: "srvtoolu_1"}, ev)
	var rec map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &rec))
	must.Equal("WARN", rec["level"])
	must.Equal("anthropic tool search result not recognised", rec["msg"])
}
