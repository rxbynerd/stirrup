package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

// nativeToolSearch reports a native tool search of the given variant for
// every routed provider and model.
func nativeToolSearch(variant string) ToolSearchSupport {
	return func(string, string) quirks.ToolSearchCapability {
		return quirks.ToolSearchCapability{Supported: true, Variant: variant}
	}
}

func nativeSearchEvents(id, query string, refs ...string) []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "tool_search", ID: id, Name: "tool_search_tool_bm25", Input: map[string]any{"query": query}},
		{Type: "tool_search_result", ID: id, ToolReferences: refs},
	}
}

func turnEvents(stop string, groups ...[]types.StreamEvent) []types.StreamEvent {
	var out []types.StreamEvent
	for _, g := range groups {
		out = append(out, g...)
	}
	return append(out, types.StreamEvent{Type: "message_complete", StopReason: stop})
}

func TestLoop_LazyTools_NativeUnloadedCallNamesTheNativeSearch(t *testing.T) {
	cases := []struct {
		variant string
		want    string
	}{
		{"bm25", `Tool "mcp_github_create_issue" is not loaded; search for it with tool_search_tool_bm25 first`},
		{"regex", `Tool "mcp_github_create_issue" is not loaded; search for it with tool_search_tool_regex first`},
		{"hosted", `Tool "mcp_github_create_issue" is not loaded; search for it with tool_search first`},
	}
	for _, tc := range cases {
		t.Run(tc.variant, func(t *testing.T) {
			must := require.New(t)
			prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
				turnEvents("tool_use", []types.StreamEvent{toolCall("c1", lazyMCPTool, `{}`)}),
			}}
			loop, tp, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
			loop.ToolSearch = nativeToolSearch(tc.variant)

			_, err := loop.Run(context.Background(), buildTestConfig())
			must.NoError(err)
			must.True(prov.sent()[0].ToolSearch)
			must.Zero(*calls)
			must.Empty(toolsLoadedEvents(tp))
			must.Equal(tc.want, rec.turnRecords[0].ToolCalls[0].Output)
			_, toolCalls := rec.snapshot()
			must.Equal(string(observability.ToolFailureToolNotLoaded), toolCalls[0].ErrorCategory)
		})
	}
}

func TestLoop_LazyTools_UnsolicitedSearchResultLoadsNothing(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("tool_use",
			nativeSearchEvents("srvtoolu_1", "github", lazyMCPTool),
			[]types.StreamEvent{toolCall("c1", lazyMCPTool, `{}`)},
		),
	}}
	loop, tp, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{Strategy: types.LazyToolStrategyHarness})
	loop.ToolSearch = nativeToolSearch("bm25")

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.False(prov.sent()[0].ToolSearch)
	must.Zero(loop.LazyTools.LoadedCount())
	must.Zero(*calls)
	must.Empty(toolsLoadedEvents(tp))
	must.Equal(`Tool "mcp_github_create_issue" is not loaded; call tool_search to load it first`, rec.turnRecords[0].ToolCalls[0].Output)
	must.Nil(rec.turnRecords[0].ModelOutput[1].ToolReferences, "an unsolicited result keeps no references")
	must.Nil(rec.turnRecords[0].ToolsLoaded)
}

func TestLoop_LazyTools_NativeSearchKeepsOnlyRegisteredDeferredTools(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("end_turn", nativeSearchEvents("srvtoolu_1", "github", "mcp_github_invented", lazyMCPTool, "test_tool")),
	}}
	loop, tp, rec, _ := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	loop.ToolSearch = nativeToolSearch("bm25")

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal([]string{lazyMCPTool}, rec.turnRecords[0].ModelOutput[1].ToolReferences, "unknown and eager names are dropped")
	must.Equal([]string{lazyMCPTool}, rec.turnRecords[0].ToolsLoaded)
	must.Equal(1, loop.LazyTools.LoadedCount())
	loadedEvents := toolsLoadedEvents(tp)
	must.Len(loadedEvents, 1)
	must.Equal([]string{lazyMCPTool}, loadedEvents[0].Tools)
}

func TestLoadSearchedTools_CapsReferencesPerResult(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(nil)
	registry := loop.Tools.(*tool.Registry)
	refs := make([]string, maxSearchReferences+44)
	for i := range refs {
		refs[i] = fmt.Sprintf("mcp_github_tool_%03d", i)
		registry.Register(&tool.Tool{
			Name:        refs[i],
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler:     func(context.Context, json.RawMessage) (string, error) { return "", nil },
		})
	}
	registry.Register(builtins.ToolSearchTool(types.LazyToolLoadingConfig{}))
	loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{})
	blocks := []types.ContentBlock{
		{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25"},
		{Type: "tool_search_result", ToolUseID: "srvtoolu_1", ToolReferences: refs},
	}

	loaded := loop.loadSearchedTools(context.Background(), blocks, true)
	must.Len(blocks[1].ToolReferences, maxSearchReferences)
	must.Equal(refs[:maxSearchReferences], blocks[1].ToolReferences)
	must.Equal(refs[:maxSearchReferences], loaded)
	must.Equal(maxSearchReferences, loop.LazyTools.LoadedCount())
}

func TestLoadSearchedTools_OrphanResultLoadsUnderTheSameRules(t *testing.T) {
	must := require.New(t)
	loop, tp, _, _ := buildLazyTestLoop(t, &scriptedParamsProvider{}, types.LazyToolLoadingConfig{})
	blocks := []types.ContentBlock{
		{Type: "tool_search_result", ToolUseID: "srvtoolu_orphan", ToolReferences: []string{lazyMCPTool, "mcp_github_invented"}},
	}

	must.NotPanics(func() { loop.loadSearchedTools(context.Background(), blocks, true) })
	must.Equal([]string{lazyMCPTool}, blocks[0].ToolReferences)
	loadedEvents := toolsLoadedEvents(tp)
	must.Len(loadedEvents, 1)
	must.Empty(loadedEvents[0].Name)
	must.Equal(types.ToolsLoadedSourceNative, loadedEvents[0].Source)
}

func TestLoop_LazyTools_RepeatedNativeSearchReportsNothingNew(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("tool_use",
			nativeSearchEvents("srvtoolu_1", "github", lazyMCPTool),
			[]types.StreamEvent{toolCall("c1", lazyMCPTool, `{}`)},
		),
		turnEvents("tool_use",
			nativeSearchEvents("srvtoolu_2", "github issue", lazyMCPTool),
			[]types.StreamEvent{toolCall("c2", lazyMCPTool, `{}`)},
		),
	}}
	loop, tp, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	loop.ToolSearch = nativeToolSearch("bm25")

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal(2, *calls)
	must.Len(toolsLoadedEvents(tp), 1)
	must.Equal([]string{lazyMCPTool}, rec.turnRecords[1].ModelOutput[1].ToolReferences, "the repeat keeps its reference for replay")
	must.Nil(rec.turnRecords[1].ToolsLoaded)
}

func TestLoop_LazyTools_SameResponseOrderDecidesWhetherACallLoads(t *testing.T) {
	t.Run("search before the call", func(t *testing.T) {
		must := require.New(t)
		prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
			turnEvents("tool_use", []types.StreamEvent{
				toolCall("c1", types.ToolSearchToolName, `{"query":"github issue"}`),
				toolCall("c2", lazyMCPTool, `{}`),
			}),
		}}
		loop, tp, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})

		_, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.Equal(1, *calls)
		must.False(rec.turnRecords[0].ToolCalls[1].IsError)
		must.Len(toolsLoadedEvents(tp), 1)
		turns, _ := rec.snapshot()
		must.Equal([]string{lazyMCPTool}, turns[0].ToolsLoaded)
	})

	t.Run("call before the search", func(t *testing.T) {
		must := require.New(t)
		prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
			turnEvents("tool_use", []types.StreamEvent{
				toolCall("c1", lazyMCPTool, `{}`),
				toolCall("c2", types.ToolSearchToolName, `{"query":"github issue"}`),
			}),
		}}
		loop, _, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})

		_, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.Zero(*calls)
		must.Equal(`Tool "mcp_github_create_issue" is not loaded; call tool_search to load it first`, rec.turnRecords[0].ToolCalls[0].Output)
		must.True(loop.LazyTools.Loaded(lazyMCPTool), "the later search still loads the tool")
	})
}

func TestLoop_LazyTools_InterleavedNativeSearchesReportSeparately(t *testing.T) {
	must := require.New(t)
	const second = "mcp_github_add_comment"
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("end_turn", []types.StreamEvent{
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: map[string]any{"query": "create issue"}},
			{Type: "tool_search", ID: "srvtoolu_2", Name: "tool_search_tool_bm25", Input: map[string]any{"query": "comment"}},
			{Type: "tool_search_result", ID: "srvtoolu_2", ToolReferences: []string{second}},
			{Type: "tool_search_result", ID: "srvtoolu_1", ToolReferences: []string{lazyMCPTool}},
		}),
	}}
	loop, tp, _, _ := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        second,
		Description: "Comment on a GitHub issue",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     func(context.Context, json.RawMessage) (string, error) { return "", nil },
	})
	loop.LazyTools = tool.NewLazyLoader(loop.Tools, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{MCPServers: []string{"github"}})
	loop.ToolSearch = nativeToolSearch("bm25")

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	loadedEvents := toolsLoadedEvents(tp)
	must.Len(loadedEvents, 2)
	must.Equal([]string{second}, loadedEvents[0].Tools)
	must.JSONEq(`{"query":"comment"}`, string(loadedEvents[0].Input))
	must.Equal([]string{lazyMCPTool}, loadedEvents[1].Tools)
	must.JSONEq(`{"query":"create issue"}`, string(loadedEvents[1].Input))
	for _, ev := range loadedEvents {
		must.Equal("tool_search_tool_bm25", ev.Name)
	}
}

func TestToolFailureMetrics_UnloadedToolKeepsItsRegisteredName(t *testing.T) {
	must := require.New(t)
	deferred := mutatingTool()
	deferred.Name = lazyMCPTool
	loop, reader := buildMetricsHarness(t, []*tool.Tool{trivialTool(), deferred}, nil, nil, nil)
	loop.LazyTools = tool.NewLazyLoader(loop.Tools, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{})
	cfg := &types.RunConfig{RunID: "test-run", Mode: "execution", ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1}}

	_, _, outcome := loop.planAndDispatch(context.Background(), cfg, []types.ToolCall{
		{ID: "tc1", Name: lazyMCPTool, Input: json.RawMessage(`{}`)},
		{ID: "tc2", Name: "mcp_github_invented", Input: json.RawMessage(`{}`)},
	}, &stallDetector{}, "anthropic", "claude-sonnet-4-6")
	must.Empty(outcome)

	failures := collectFailures(t, reader)
	must.Len(failures, 2)
	byCategory := map[string]string{}
	for _, f := range failures {
		byCategory[f.category] = f.tool
	}
	must.Equal(map[string]string{
		string(observability.ToolFailureToolNotLoaded): lazyMCPTool,
		string(observability.ToolFailureUnknownTool):   unknownToolMetricName,
	}, byCategory)
}

func TestEstimateLoadedToolDefinitionTokens_CountsLoadedDeferredTools(t *testing.T) {
	must := require.New(t)
	loop, _, _, _ := buildLazyTestLoop(t, &scriptedParamsProvider{}, types.LazyToolLoadingConfig{})
	sized := func(name string, deferred bool) types.ToolDefinition {
		return types.ToolDefinition{Name: name, Description: string(make([]byte, 400)), InputSchema: json.RawMessage(`{"type":"object"}`), Deferred: deferred}
	}
	eager := sized("test_tool", false)
	loadedLater := sized(lazyMCPTool, true)
	neverLoaded := sized("mcp_github_other", true)
	native := []types.ToolDefinition{eager, loadedLater, neverLoaded}
	eagerOnly := estimateToolDefinitionTokens([]types.ToolDefinition{eager})

	must.Equal(eagerOnly, estimateLoadedToolDefinitionTokens(native, loop.deferredToolLoaded), "nothing is loaded yet")
	loop.LazyTools.Load([]string{lazyMCPTool})
	withLoaded := estimateToolDefinitionTokens([]types.ToolDefinition{eager, sized(lazyMCPTool, false)})
	must.Equal(withLoaded, estimateLoadedToolDefinitionTokens(native, loop.deferredToolLoaded))
	must.Equal(eagerOnly, estimateToolDefinitionTokens(native), "without a loaded predicate no deferred definition counts")

	harness := loop.LazyTools.Visible(false)
	must.Equal(estimateToolDefinitionTokens(harness), estimateLoadedToolDefinitionTokens(harness, loop.deferredToolLoaded),
		"the harness strategy sends loaded tools without the flag")
}
