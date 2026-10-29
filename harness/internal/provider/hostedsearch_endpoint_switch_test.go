package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// A router switch mid-run from a provider declared "openai" to a "generic"
// endpoint hands the generic adapter a history that holds hosted search
// blocks and stored hosted search output items. These tests pin that the
// generic adapter's request body carries none of them.

// hostedSearchHistory is a transcript whose assistant turn searched, loaded
// a deferred function and called it, stored as origin's replay items.
func hostedSearchHistory(t *testing.T, origin string) []types.Message {
	t.Helper()
	turn := storedTurn(t, origin, responsesToolSearchTurnBlocks(), toolSearchCallItem, toolSearchOutputItem, loadedFunctionCall)
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "file a ticket"}}},
		turn,
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "PROJ-1"}}},
	}
}

func TestOpenAIResponsesAdapter_GenericEndpointDropsHostedSearchHistory(t *testing.T) {
	// The loop offers no native search to a generic endpoint, so its tools
	// arrive eager and without ToolSearch.
	eagerTools := []types.ToolDefinition{readFileTool, jiraCreateTool}
	eagerTools[1].Deferred = false

	tests := []struct {
		name         string
		endpoint     string
		toolSearch   bool
		tools        []types.ToolDefinition
		matchOrigin  bool
		wantHostedIn bool
	}{
		{"openai endpoint replays hosted search items", types.EndpointProfileOpenAI, true, []types.ToolDefinition{readFileTool, jiraCreateTool}, true, true},
		{"generic endpoint on the origin drops them", types.EndpointProfileGeneric, false, eagerTools, true, false},
		{"generic endpoint off the origin drops them", types.EndpointProfileGeneric, false, eagerTools, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			script := newResponsesScript(t)
			adapter := script.adapter(nil)
			adapter.EndpointProfile = tc.endpoint

			origin := responsesReplayOrigin(responsesToolSearchModel, "http://other.example/v1")
			if tc.matchOrigin {
				origin = responsesReplayOrigin(responsesToolSearchModel, script.srv.URL)
			}
			ch, err := adapter.Stream(context.Background(), types.StreamParams{
				Model:      responsesToolSearchModel,
				MaxTokens:  1024,
				ToolSearch: tc.toolSearch,
				Tools:      tc.tools,
				Messages:   hostedSearchHistory(t, origin),
			})
			must.NoError(err, "Stream")
			drainStream(t, ch)

			body := string(script.lastBody(t))
			for _, marker := range []string{`"tool_search_call"`, `"tool_search_output"`, `"defer_loading"`, `"type":"tool_search"`, `"namespace"`, `"execution"`} {
				if tc.wantHostedIn {
					check.Contains(body, marker, "the openai endpoint should receive hosted search wire shapes")
				} else {
					check.NotContains(body, marker, "a generic endpoint must not receive hosted search wire shapes")
				}
			}
			check.Contains(body, `"call_id":"call_1"`, "the function call and its output stay paired")
			check.Contains(body, `"PROJ-1"`)
		})
	}
}

func TestOpenAICompatibleAdapter_HostedSearchHistoryStaysOffChatCompletions(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		check.NoError(err, "read request body")
		captured = body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w,
			makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	must.Equal(types.EndpointProfileGeneric, adapter.EndpointProfile)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     responsesToolSearchModel,
		MaxTokens: 1024,
		Tools:     []types.ToolDefinition{readFileTool},
		Messages:  hostedSearchHistory(t, responsesReplayOrigin(responsesToolSearchModel, srv.URL)),
	})
	must.NoError(err, "Stream")
	drainStream(t, ch)

	body := string(captured)
	for _, marker := range []string{"tool_search", "defer_loading", "openai_responses", "encrypted_content"} {
		check.NotContains(body, marker)
	}
	check.Contains(body, `"tool_call_id":"call_1"`, "the function call and its output stay paired")
}
