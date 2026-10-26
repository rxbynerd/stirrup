package provider

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// Output items of a hosted tool search followed by a call to the loaded
// tool, trimmed to the fields the adapter reads.
const (
	responsesBatchSearchCall   = `{"type":"tool_search_call","id":"tsc_1","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["mcp_jira_create_issue"]}}`
	responsesBatchSearchOutput = `{"type":"tool_search_output","id":"tso_1","execution":"server","call_id":null,"status":"completed","tools":[{"type":"namespace","name":"jira","tools":[{"type":"function","name":"mcp_jira_create_issue","defer_loading":true}]}]}`
	responsesBatchLoadedCall   = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp_jira_create_issue","namespace":"jira","arguments":"{\"title\":\"Broken build\"}","status":"completed"}`
)

func responsesBatchResult(items ...string) []byte {
	return []byte(`{"status":"completed","output":[` + strings.Join(items, ",") + `],"usage":{"input_tokens":3,"output_tokens":7}}`)
}

func responsesFabricate(t *testing.T, response []byte) []types.StreamEvent {
	t.Helper()
	ch := make(chan types.StreamEvent, 16)
	fabricateStream(ch, response, "openai-responses")
	close(ch)
	return drain(t, ch)
}

func TestFabricateStream_OpenAIResponsesToolSearch(t *testing.T) {
	must := require.New(t)
	got := responsesFabricate(t, responsesBatchResult(responsesBatchSearchCall, responsesBatchSearchOutput, responsesBatchLoadedCall))

	must.Len(got, 4, "events: %+v", got)
	must.Equal(types.StreamEvent{
		Type:  "tool_search",
		ID:    "tsc_1",
		Name:  "tool_search",
		Input: map[string]any{"paths": []any{"mcp_jira_create_issue"}},
	}, got[0])
	must.Equal(types.StreamEvent{
		Type:           "tool_search_result",
		ID:             "tsc_1",
		ToolReferences: []string{"mcp_jira_create_issue"},
	}, got[1])
	must.Equal("tool_call", got[2].Type)
	must.Equal("call_1", got[2].ID)
	must.Equal(map[string]any{"title": "Broken build"}, got[2].Input)
	must.Equal("message_complete", got[3].Type)
	must.Equal("tool_use", got[3].StopReason)
	must.Equal(7, got[3].OutputTokens)
}

func TestFabricateStream_OpenAIResponsesMalformedToolSearchKeepsTurn(t *testing.T) {
	must := require.New(t)
	got := responsesFabricate(t, responsesBatchResult(
		`{"type":"tool_search_call","status":"completed","arguments":[1,2]}`,
		`{"type":"tool_search_output","status":"completed","tools":{"type":"function"}}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No tool fits."}]}`,
	))

	must.Len(got, 4, "events: %+v", got)
	must.Equal(types.StreamEvent{Type: "tool_search", ID: "tool_search_0", Name: "tool_search"}, got[0])
	must.Equal(types.StreamEvent{Type: "tool_search_result", ID: "tool_search_0"}, got[1])
	must.Equal("text_delta", got[2].Type)
	must.Equal("message_complete", got[3].Type)
	must.Equal("end_turn", got[3].StopReason)
}

// TestFabricateStream_OpenAIResponsesToolSearchMatchesStream pins that a
// batch result with a hosted search yields the events a streamed response
// with the same output items does.
func TestFabricateStream_OpenAIResponsesToolSearchMatchesStream(t *testing.T) {
	must := require.New(t)
	items := []string{responsesBatchSearchCall, responsesBatchSearchOutput, responsesBatchLoadedCall}
	var sse strings.Builder
	for i, item := range items {
		sse.WriteString(makeResponsesEvent("response.output_item.done", fmt.Sprintf(`{"output_index":%d,"item":%s}`, i, item)))
	}
	sse.WriteString(makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[`+strings.Join(items, ",")+`],"usage":{"input_tokens":3,"output_tokens":7}}}`))
	streamed := streamResponsesSSE(t, []byte(sse.String()), nil)
	batched := responsesFabricate(t, responsesBatchResult(items...))

	must.Equal(eventTypes(streamed), eventTypes(batched))
	for i := range streamed {
		t.Run(fmt.Sprintf("event %d %s", i, streamed[i].Type), func(t *testing.T) {
			must := require.New(t)
			s, b := streamed[i], batched[i]
			must.Equal(s.ID, b.ID)
			must.Equal(s.Name, b.Name)
			must.Equal(s.Input, b.Input)
			must.Equal(s.ToolReferences, b.ToolReferences)
			must.Equal(s.StopReason, b.StopReason)
			must.Equal(s.OutputTokens, b.OutputTokens)
		})
	}
}

func TestFabricateStream_OpenAIResponsesToolSearchCapsNamespaceDepth(t *testing.T) {
	must := require.New(t)
	output := `{"type":"tool_search_output","id":"tso_1","status":"completed","tools":` + string(responsesNestedNamespaces(4990)) + `}`

	start := time.Now()
	got := responsesFabricate(t, responsesBatchResult(responsesBatchSearchCall, output))

	must.Less(time.Since(start), time.Second, "deeply nested namespaces must not be re-decoded per level")
	must.Len(got, 3, "events: %+v", got)
	must.Equal("tool_search_result", got[1].Type)
	must.Empty(got[1].ToolReferences, "a function nested past the depth cap is not loaded")
}
