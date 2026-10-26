package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

const lazySecondMCPTool = "mcp_github_list_issues"

// buildTwoDeferredLazyTestLoop is buildLazyTestLoop with a second
// deferred MCP tool and a big_tool whose output fills a small context
// window, so a request can un-defer one tool while another stays deferred.
func buildTwoDeferredLazyTestLoop(t *testing.T, prov *scriptedParamsProvider) (*AgenticLoop, *recordingTransport) {
	t.Helper()
	loop, tp, _, _ := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	registry := loop.Tools.(*tool.Registry)
	registry.Register(&tool.Tool{
		Name:        lazySecondMCPTool,
		Description: "List GitHub issues",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "no issues", nil
		},
	})
	registry.Register(&tool.Tool{
		Name:        "big_tool",
		Description: "Returns a large output",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 40_000), nil
		},
	})
	loop.LazyTools = tool.NewLazyLoader(registry,
		types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"test_tool", "big_tool"}},
		tool.LazyLoaderOptions{MCPServers: []string{"github"}})
	loop.ToolSearch = nativeToolSearch("bm25")
	return loop, tp
}

func toolDefinitionNamed(t *testing.T, defs []types.ToolDefinition, name string) types.ToolDefinition {
	t.Helper()
	for _, d := range defs {
		if d.Name == name {
			return d
		}
	}
	require.Failf(t, "tool not sent", "%s is not in %v", name, toolNames(defs))
	return types.ToolDefinition{}
}

func hasToolSearchResult(messages []types.Message) bool {
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "tool_search_result" {
				return true
			}
		}
	}
	return false
}

func TestLoop_NativeTurnUndefersLoadedToolWhoseReferenceWasTrimmed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("tool_use",
			nativeSearchEvents("srvtoolu_1", "github", lazyMCPTool),
			[]types.StreamEvent{toolCall("c1", lazyMCPTool, `{}`)}),
		turnEvents("tool_use", []types.StreamEvent{toolCall("c2", "big_tool", `{}`)}),
	}}
	loop, _ := buildTwoDeferredLazyTestLoop(t, prov)
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 2000

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)
	sent := prov.sent()
	must.Len(sent, 3)

	must.True(hasToolSearchResult(sent[1].Messages))
	kept := toolDefinitionNamed(t, sent[1].Tools, lazyMCPTool)
	check.True(kept.Deferred, "a tool whose search result is in the request stays deferred")
	check.Equal("mcp_github", kept.Namespace)

	must.False(hasToolSearchResult(sent[2].Messages), "the sliding window drops the search exchange")
	trimmed := toolDefinitionNamed(t, sent[2].Tools, lazyMCPTool)
	check.False(trimmed.Deferred, "a loaded tool without a search result in the request is sent in full")
	check.Empty(trimmed.Namespace)
	check.Empty(trimmed.NamespaceDescription)
	check.True(toolDefinitionNamed(t, sent[2].Tools, lazySecondMCPTool).Deferred, "an unloaded tool stays deferred")
	check.True(sent[2].ToolSearch)
	check.True(loop.LazyTools.Deferred(lazyMCPTool), "the loader's deferred set is unchanged")
	check.True(loop.LazyTools.Loaded(lazyMCPTool))
}

func TestLoop_NativeTurnKeepsReferencedToolDeferred(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("tool_use",
			nativeSearchEvents("srvtoolu_1", "github", lazyMCPTool),
			[]types.StreamEvent{toolCall("c1", lazyMCPTool, `{}`)}),
		turnEvents("tool_use", []types.StreamEvent{toolCall("c2", "test_tool", `{}`)}),
	}}
	loop, _ := buildTwoDeferredLazyTestLoop(t, prov)

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	sent := prov.sent()
	must.Len(sent, 3)
	for i := 1; i < len(sent); i++ {
		def := toolDefinitionNamed(t, sent[i].Tools, lazyMCPTool)
		check.True(def.Deferred, "request %d", i)
		check.Equal("mcp_github", def.Namespace, "request %d", i)
		check.Equal(sent[0].Tools, sent[i].Tools, "request %d keeps the first request's tool list", i)
	}
}

// harnessThenNativeRouter routes turn 0 to a provider without native
// search and every later turn to the default provider.
type harnessThenNativeRouter struct{}

func (harnessThenNativeRouter) Select(_ context.Context, rc router.RouterContext) router.ModelSelection {
	if rc.Turn == 0 {
		return router.ModelSelection{Provider: "chat", Model: "gpt-4o"}
	}
	return router.ModelSelection{Model: "claude-sonnet-4-6"}
}

func TestLoop_RouterSwitchHarnessLoadedToolSentUndeferredOnNativeTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	primary := &scriptedParamsProvider{}
	routed := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("tool_use", []types.StreamEvent{toolCall("c1", types.ToolSearchToolName, `{"query":"create","limit":1}`)}),
	}}
	loop, tp := buildTwoDeferredLazyTestLoop(t, primary)
	loop.Router = harnessThenNativeRouter{}
	loop.Providers = map[string]provider.ProviderAdapter{"chat": routed}
	loop.ToolSearch = func(key, _ string) quirks.ToolSearchCapability {
		return quirks.ToolSearchCapability{Supported: key != "chat", Variant: "bm25"}
	}
	config := buildTestConfig()
	config.Providers = map[string]types.ProviderConfig{"chat": {Type: "openai", APIKeyRef: "secret://TEST"}}

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)
	loaded := toolsLoadedEvents(tp)
	must.Len(loaded, 1)
	must.Equal(types.ToolsLoadedSourceHarness, loaded[0].Source)
	must.Equal([]string{lazyMCPTool}, loaded[0].Tools)

	sent := primary.sent()
	must.Len(sent, 1)
	check.True(sent[0].ToolSearch)
	check.NotContains(toolNames(sent[0].Tools), types.ToolSearchToolName)
	check.False(toolDefinitionNamed(t, sent[0].Tools, lazyMCPTool).Deferred, "the harness search left no native reference to expand the tool")
	check.True(toolDefinitionNamed(t, sent[0].Tools, lazySecondMCPTool).Deferred)
}

func TestLoop_PauseContinuationDiscardedAtOutputLimitUndefersTheToolsItLoaded(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("pause_turn", []types.StreamEvent{pendingSearchEvent("srvtoolu_1", "github")}),
		turnEvents("max_tokens", []types.StreamEvent{{Type: "tool_search_result", ID: "srvtoolu_1", ToolReferences: []string{lazyMCPTool}}}),
	}}
	loop, tp := buildTwoDeferredLazyTestLoop(t, prov)

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Len(toolsLoadedEvents(tp), 1, "the discarded continuation still loaded the tool")
	sent := prov.sent()
	must.Len(sent, 3)
	check.True(toolDefinitionNamed(t, sent[1].Tools, lazyMCPTool).Deferred, "the continuation request comes before the load")

	must.False(hasToolSearchResult(sent[2].Messages), "output-limit recovery discards the continuation and its search result")
	check.False(toolDefinitionNamed(t, sent[2].Tools, lazyMCPTool).Deferred)
	check.True(toolDefinitionNamed(t, sent[2].Tools, lazySecondMCPTool).Deferred)
}

func TestLoop_NativeTurnWithEveryDeferredToolUndeferredStillAsksForTheSearch(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("tool_use",
			nativeSearchEvents("srvtoolu_1", "github", lazyMCPTool, lazySecondMCPTool),
			[]types.StreamEvent{toolCall("c1", lazyMCPTool, `{}`)}),
		turnEvents("tool_use", []types.StreamEvent{toolCall("c2", "big_tool", `{}`)}),
	}}
	loop, _ := buildTwoDeferredLazyTestLoop(t, prov)
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 2000

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)
	sent := prov.sent()
	must.Len(sent, 3)
	must.False(hasToolSearchResult(sent[2].Messages))
	for _, def := range sent[2].Tools {
		check.False(def.Deferred, "%s is deferred", def.Name)
	}
	check.True(sent[2].ToolSearch, "the request still asks for the native search; the adapter sends it only beside a deferred tool")
	check.True(loop.LazyTools.UsesNative(true), "un-deferring one request leaves the loader on the native search")
}

func TestUndeferUnreferenced_CountsOnlyAnsweredSearchesByTheTurnsTool(t *testing.T) {
	bm25 := lazySearch{native: true, tool: "tool_search_tool_bm25"}
	search := types.ContentBlock{Type: "tool_search", ID: "s1", Name: "tool_search_tool_bm25"}
	result := types.ContentBlock{Type: "tool_search_result", ToolUseID: "s1", ToolReferences: []string{lazyMCPTool}}
	assistant := func(blocks ...types.ContentBlock) []types.Message {
		return []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "task"}}}, {Role: "assistant", Content: blocks}}
	}
	failed := result
	failed.IsError = true
	failed.ToolReferences = nil
	foreign := search
	foreign.Name = "tool_search"
	cases := []struct {
		name     string
		search   lazySearch
		prepared []types.Message
		deferred bool
	}{
		{"answered search keeps it deferred", bm25, assistant(search, result), true},
		{"no search sends it in full", bm25, assistant(), false},
		{"failed search sends it in full", bm25, assistant(search, failed), false},
		{"result without its call sends it in full", bm25, assistant(result), false},
		{"another provider's search sends it in full", bm25, assistant(foreign, result), false},
		{"call and result in different messages sends it in full", bm25, append(assistant(search), types.Message{Role: "assistant", Content: []types.ContentBlock{result}}), false},
		{"unanswered search at the end keeps it deferred", bm25, assistant(search), true},
		{"unanswered search before an answered one keeps it deferred", bm25, assistant(search, types.ContentBlock{Type: "tool_search", ID: "s2", Name: "tool_search_tool_bm25"}, result), true},
		{"unanswered search of another provider's tool sends it in full", bm25, assistant(foreign), false},
		{"harness search turn is unchanged", lazySearch{}, assistant(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			loop, _, _, _ := buildLazyTestLoop(t, &scriptedParamsProvider{}, types.LazyToolLoadingConfig{})
			loop.LazyTools.Load([]string{lazyMCPTool})
			defs := loop.LazyTools.Visible(tc.search.native)
			before := append([]types.ToolDefinition(nil), defs...)

			got := loop.undeferUnreferenced(defs, tc.search, tc.prepared)

			must.Equal(before, defs, "the input slice is not modified")
			if !tc.search.native {
				must.Equal(defs, got)
				return
			}
			must.Equal(tc.deferred, toolDefinitionNamed(t, got, lazyMCPTool).Deferred)
		})
	}
}

func TestUndeferUnreferenced_LeavesUnloadedToolsDeferred(t *testing.T) {
	must := require.New(t)
	loop, _, _, _ := buildLazyTestLoop(t, &scriptedParamsProvider{}, types.LazyToolLoadingConfig{})
	defs := loop.LazyTools.Visible(true)

	got := loop.undeferUnreferenced(defs, lazySearch{native: true, tool: "tool_search_tool_bm25"}, nil)

	must.True(toolDefinitionNamed(t, got, lazyMCPTool).Deferred)
}

func TestLoop_ForcedCompactionRetryUndefersLoadedToolWhoseReferenceWasTrimmed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &overflowScriptProvider{script: []overflowStep{
		{events: turnEvents("tool_use",
			nativeSearchEvents("srvtoolu_1", "github", lazyMCPTool),
			[]types.StreamEvent{toolCall("c1", "test_tool", `{}`)})},
		{events: turnEvents("tool_use", []types.StreamEvent{toolCall("c2", "big_tool", `{}`)})},
		{events: turnEvents("tool_use", []types.StreamEvent{toolCall("c3", "big_tool", `{}`)})},
		{err: promptTooLong()},
	}}
	loop, _ := buildTwoDeferredLazyTestLoop(t, &scriptedParamsProvider{})
	loop.Provider = prov

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Len(prov.requests, 5, "three tool turns, the overflow and its retry")

	rejected, retried := prov.requests[3], prov.requests[4]
	must.True(hasToolSearchResult(rejected.Messages))
	check.True(toolDefinitionNamed(t, rejected.Tools, lazyMCPTool).Deferred)
	must.False(hasToolSearchResult(retried.Messages), "the forced compaction drops the search exchange")
	check.False(toolDefinitionNamed(t, retried.Tools, lazyMCPTool).Deferred)
	check.True(toolDefinitionNamed(t, retried.Tools, lazySecondMCPTool).Deferred)
}
