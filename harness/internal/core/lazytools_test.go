package core

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

const lazyMCPTool = "mcp_github_create_issue"

// scriptedParamsProvider replays one event script per Stream call and
// records the params of every call.
type scriptedParamsProvider struct {
	mu     sync.Mutex
	calls  [][]types.StreamEvent
	params []types.StreamParams
}

func (p *scriptedParamsProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	events := []types.StreamEvent{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}}
	if i := len(p.params); i < len(p.calls) {
		events = p.calls[i]
	}
	p.params = append(p.params, params)
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (p *scriptedParamsProvider) sent() []types.StreamParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]types.StreamParams(nil), p.params...)
}

// buildLazyTestLoop returns a loop over test_tool, one MCP tool whose
// calls are counted, and tool_search, with lazy loading on.
func buildLazyTestLoop(t *testing.T, prov *scriptedParamsProvider, cfg types.LazyToolLoadingConfig) (*AgenticLoop, *recordingTransport, *recordingTraceEmitter, *int) {
	t.Helper()
	loop := buildTestLoop(nil)
	loop.Provider = prov
	registry := loop.Tools.(*tool.Registry)
	calls := 0
	registry.Register(&tool.Tool{
		Name:              lazyMCPTool,
		Description:       "Open a new GitHub issue",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: true,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			calls++
			return "issue opened", nil
		},
	})
	registry.Register(builtins.ToolSearchTool())
	cfg.Enabled = true
	loop.LazyTools = tool.NewLazyLoader(registry, cfg, tool.LazyLoaderOptions{MCPServers: []string{"github"}})
	tp := &recordingTransport{}
	loop.Transport = tp
	rec := &recordingTraceEmitter{}
	loop.Trace = rec
	return loop, tp, rec, &calls
}

func toolsLoadedEvents(tp *recordingTransport) []types.HarnessEvent {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []types.HarnessEvent
	for _, e := range tp.events {
		if e.Type == "tools_loaded" {
			out = append(out, e)
		}
	}
	return out
}

func toolNames(defs []types.ToolDefinition) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

func toolCall(id, name, input string) types.StreamEvent {
	var m map[string]any
	_ = json.Unmarshal([]byte(input), &m)
	return types.StreamEvent{Type: "tool_call", ID: id, Name: name, Input: m}
}

func TestLoop_LazyTools_HarnessSearchLoadsADeferredTool(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		{toolCall("c0", lazyMCPTool, `{}`), {Type: "message_complete", StopReason: "tool_use"}},
		{toolCall("c1", types.ToolSearchToolName, `{"query":"github issue"}`), {Type: "message_complete", StopReason: "tool_use"}},
		{toolCall("c2", lazyMCPTool, `{}`), {Type: "message_complete", StopReason: "tool_use"}},
	}}
	loop, tp, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	sent := prov.sent()
	must.Len(sent, 4)
	must.Equal([]string{"test_tool", types.ToolSearchToolName}, toolNames(sent[0].Tools))
	must.Equal([]string{"test_tool", types.ToolSearchToolName}, toolNames(sent[1].Tools))
	must.Equal([]string{"test_tool", lazyMCPTool, types.ToolSearchToolName}, toolNames(sent[2].Tools))
	for i, p := range sent {
		must.False(p.ToolSearch, "turn %d", i)
	}

	must.Equal(1, *calls, "the unloaded call is rejected before dispatch")
	turnRecords := rec.turnRecords
	must.Equal(`Tool "mcp_github_create_issue" is not loaded; call tool_search to load it first`, turnRecords[0].ToolCalls[0].Output)
	must.True(turnRecords[0].ToolCalls[0].IsError)
	must.Contains(turnRecords[1].ToolCalls[0].Output, "Loaded 1 tool:\n- mcp_github_create_issue: Open a new GitHub issue")
	must.False(turnRecords[2].ToolCalls[0].IsError)

	loadedEvents := toolsLoadedEvents(tp)
	must.Len(loadedEvents, 1)
	must.Equal(types.ToolSearchToolName, loadedEvents[0].Name)
	must.Equal([]string{lazyMCPTool}, loadedEvents[0].Tools)
	must.JSONEq(`{"query":"github issue"}`, string(loadedEvents[0].Input))
	must.NotNil(loadedEvents[0].Turn)
	must.Equal(1, *loadedEvents[0].Turn)

	turns, _ := rec.snapshot()
	must.Len(turns, 4)
	must.Empty(turns[0].ToolsLoaded)
	must.Equal([]string{lazyMCPTool}, turns[1].ToolsLoaded)
	must.Empty(turns[2].ToolsLoaded)
}

func TestLoop_LazyTools_ToolsLoadedFollowsTheSearchResult(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		{toolCall("c1", types.ToolSearchToolName, `{"query":"github"}`), {Type: "message_complete", StopReason: "tool_use"}},
	}}
	loop, tp, _, _ := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	var order []string
	tp.mu.Lock()
	for _, e := range tp.events {
		switch e.Type {
		case "tool_result", "tools_loaded", "turn_committed":
			order = append(order, e.Type)
		}
	}
	tp.mu.Unlock()
	must.Equal([]string{"tool_result", "tools_loaded", "turn_committed", "turn_committed"}, order)
}

func TestLoop_LazyTools_NativeSearchLoadsFromTheStream(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		{
			{Type: "text_delta", Text: "Looking."},
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: map[string]any{"query": "github"}},
			{Type: "tool_search_result", ID: "srvtoolu_1", ToolReferences: []string{lazyMCPTool}},
			toolCall("c1", lazyMCPTool, `{}`),
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}}
	loop, tp, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	loop.ToolSearch = func(string, string) quirks.ToolSearchCapability {
		return quirks.ToolSearchCapability{Supported: true, Variant: "bm25"}
	}

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	sent := prov.sent()
	must.Len(sent, 2)
	must.True(sent[0].ToolSearch)
	must.Equal([]string{"test_tool", lazyMCPTool}, toolNames(sent[0].Tools), "the harness search tool is not sent beside the native one")
	must.False(sent[0].Tools[0].Deferred)
	must.True(sent[0].Tools[1].Deferred)
	must.True(sent[1].Tools[1].Deferred, "a loaded tool stays deferred on the wire")

	must.Equal(1, *calls, "a tool the native search loaded is dispatched")

	assistant := sent[1].Messages[1]
	must.Equal("assistant", assistant.Role)
	must.Equal([]string{"text", "tool_search", "tool_search_result", "tool_use"}, blockTypes(assistant))
	must.Equal("tool_search_tool_bm25", assistant.Content[1].Name)
	must.JSONEq(`{"query":"github"}`, string(assistant.Content[1].Input))
	must.Equal("srvtoolu_1", assistant.Content[2].ToolUseID)
	must.Equal([]string{lazyMCPTool}, assistant.Content[2].ToolReferences)

	loadedEvents := toolsLoadedEvents(tp)
	must.Len(loadedEvents, 1)
	must.Equal("tool_search_tool_bm25", loadedEvents[0].Name)
	must.Equal([]string{lazyMCPTool}, loadedEvents[0].Tools)
	must.Equal(0, *loadedEvents[0].Turn)

	turns, _ := rec.snapshot()
	must.Equal([]string{lazyMCPTool}, turns[0].ToolsLoaded)
	must.Equal([]string{lazyMCPTool}, rec.turnRecords[0].ModelOutput[2].ToolReferences)
}

func TestLoop_LazyTools_FailedNativeSearchLoadsNothing(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		{
			{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: map[string]any{"query": ""}},
			{Type: "tool_search_result", ID: "srvtoolu_1", Name: "invalid_tool_input", Text: "query is empty"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	loop, tp, rec, _ := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	loop.ToolSearch = func(string, string) quirks.ToolSearchCapability {
		return quirks.ToolSearchCapability{Supported: true}
	}

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Empty(toolsLoadedEvents(tp))
	result := rec.turnRecords[0].ModelOutput[1]
	must.True(result.IsError)
	must.Equal("invalid_tool_input", result.Name)
	must.Equal("query is empty", result.Content)
	must.Zero(loop.LazyTools.LoadedCount())
}

func TestToolSearchBlock_RecordsTheSearchOutcome(t *testing.T) {
	cases := []struct {
		name  string
		event types.StreamEvent
		want  types.ContentBlock
	}{
		{
			name:  "code and message",
			event: types.StreamEvent{Type: "tool_search_result", ID: "s1", Name: "invalid_tool_input", Text: "query is empty", IsError: true},
			want:  types.ContentBlock{Type: "tool_search_result", ToolUseID: "s1", Name: "invalid_tool_input", Content: "query is empty", IsError: true},
		},
		{
			name:  "code only",
			event: types.StreamEvent{Type: "tool_search_result", ID: "s1", Name: "unavailable", IsError: true},
			want:  types.ContentBlock{Type: "tool_search_result", ToolUseID: "s1", Name: "unavailable", IsError: true},
		},
		{
			name:  "message only",
			event: types.StreamEvent{Type: "tool_search_result", ID: "s1", Text: "search failed", IsError: true},
			want:  types.ContentBlock{Type: "tool_search_result", ToolUseID: "s1", Content: "search failed", IsError: true},
		},
		{
			name:  "neither code nor message",
			event: types.StreamEvent{Type: "tool_search_result", ID: "s1", IsError: true},
			want:  types.ContentBlock{Type: "tool_search_result", ToolUseID: "s1", IsError: true},
		},
		{
			name:  "code without the error flag",
			event: types.StreamEvent{Type: "tool_search_result", ID: "s1", Name: "too_many_requests"},
			want:  types.ContentBlock{Type: "tool_search_result", ToolUseID: "s1", Name: "too_many_requests", IsError: true},
		},
		{
			name:  "success",
			event: types.StreamEvent{Type: "tool_search_result", ID: "s1", ToolReferences: []string{lazyMCPTool}},
			want:  types.ContentBlock{Type: "tool_search_result", ToolUseID: "s1", ToolReferences: []string{lazyMCPTool}},
		},
		{
			name:  "search call",
			event: types.StreamEvent{Type: "tool_search", ID: "s1", Name: "tool_search_tool_bm25", Input: map[string]any{"query": "github"}},
			want:  types.ContentBlock{Type: "tool_search", ID: "s1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"github"}`)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, toolSearchBlock(tc.event))
		})
	}
}

func TestLoop_LazyTools_ToolSearchFollowsCapabilityAndStrategy(t *testing.T) {
	cases := []struct {
		name      string
		cfg       types.LazyToolLoadingConfig
		supported bool
		native    bool
	}{
		{"auto with native search", types.LazyToolLoadingConfig{}, true, true},
		{"auto without native search", types.LazyToolLoadingConfig{}, false, false},
		{"harness strategy", types.LazyToolLoadingConfig{Strategy: types.LazyToolStrategyHarness}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			prov := &scriptedParamsProvider{}
			loop, _, _, _ := buildLazyTestLoop(t, prov, tc.cfg)
			var asked []string
			loop.ToolSearch = func(provider, model string) quirks.ToolSearchCapability {
				asked = append(asked, provider+"/"+model)
				return quirks.ToolSearchCapability{Supported: tc.supported}
			}
			_, err := loop.Run(context.Background(), buildTestConfig())
			must.NoError(err)
			sent := prov.sent()[0]
			must.Equal(tc.native, sent.ToolSearch)
			must.Equal(!tc.native, toolNames(sent.Tools)[len(sent.Tools)-1] == types.ToolSearchToolName)
			must.Equal([]string{"anthropic/claude-sonnet-4-6"}, asked)
		})
	}
}

func TestLoop_LazyTools_NothingDeferredIsANoOp(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	registry := loop.Tools.(*tool.Registry)
	registry.Register(builtins.ToolSearchTool())
	loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{})
	loop.ToolSearch = func(string, string) quirks.ToolSearchCapability {
		return quirks.ToolSearchCapability{Supported: true}
	}

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	sent := prov.sent()[0]
	must.False(sent.ToolSearch)
	must.Equal([]string{"test_tool"}, toolNames(sent.Tools))
}

func TestLoop_LazyTools_WarnsAboutUnknownEagerNamesAfterRunStarted(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{}
	loop, tp, _, _ := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{Eager: []string{"test_tool", "no_such_tool"}})

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	tp.mu.Lock()
	defer tp.mu.Unlock()
	started, warned := -1, -1
	for i, e := range tp.events {
		switch {
		case e.Type == "run_started":
			started = i
		case e.Type == "warning" && e.Message == `tools.lazyLoading.eager names "no_such_tool", which is not a registered tool; it is ignored`:
			warned = i
		}
	}
	must.GreaterOrEqual(started, 0)
	must.Greater(warned, started)
}

func TestStartLazyTools_ResetsUnlessHistoryContinues(t *testing.T) {
	must := require.New(t)
	loop, _, _, _ := buildLazyTestLoop(t, &scriptedParamsProvider{}, types.LazyToolLoadingConfig{})
	loop.LazyTools.Load([]string{lazyMCPTool})

	loop.startLazyTools(context.Background(), true)
	must.True(loop.LazyTools.Loaded(lazyMCPTool))
	loop.startLazyTools(context.Background(), false)
	must.False(loop.LazyTools.Loaded(lazyMCPTool))
}

func TestEstimateToolDefinitionTokens_SkipsDeferredDefinitions(t *testing.T) {
	must := require.New(t)
	eager := types.ToolDefinition{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}
	deferred := types.ToolDefinition{Name: "mcp_big", Description: string(bytes.Repeat([]byte("x"), 4000)), Deferred: true}
	must.Equal(estimateToolDefinitionTokens([]types.ToolDefinition{eager}), estimateToolDefinitionTokens([]types.ToolDefinition{eager, deferred}))
}

func TestPrepareSubAgent_ChildGetsAFreshLazyLoader(t *testing.T) {
	must := require.New(t)
	parent := buildSubAgentTestLoop(&mockProvider{})
	registry := parent.Tools.(*tool.Registry)
	registry.Register(&tool.Tool{
		Name:              lazyMCPTool,
		Description:       "Open a new GitHub issue",
		InputSchema:       json.RawMessage(`{"type":"object"}`),
		WorkspaceMutating: true,
		Handler:           func(context.Context, json.RawMessage) (string, error) { return "", nil },
	})
	registry.Register(builtins.ToolSearchTool())
	parent.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{MCPServers: []string{"github"}})
	parent.LazyTools.Load([]string{lazyMCPTool})
	parent.ToolSearch = func(string, string) quirks.ToolSearchCapability { return quirks.ToolSearchCapability{} }
	parentConfig := buildTestConfig()

	t.Run("execution child", func(t *testing.T) {
		must := require.New(t)
		run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "subtask", Mode: "execution"})
		must.NoError(err)
		child := run.loop.LazyTools
		must.NotNil(child)
		must.NotSame(parent.LazyTools, child)
		must.NotNil(run.loop.ToolSearch)
		must.True(child.Unloaded(lazyMCPTool), "the child starts with nothing loaded")
		must.Equal([]string{"test_tool", "read_file", types.ToolSearchToolName}, toolNames(child.Visible(false)))
		must.NotNil(run.loop.Tools.Resolve(lazyMCPTool), "the child registry keeps every tool")
	})

	t.Run("read-only child defers nothing", func(t *testing.T) {
		must := require.New(t)
		run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "subtask", Mode: "research"})
		must.NoError(err)
		child := run.loop.LazyTools
		must.NotNil(child)
		must.False(child.HasDeferred())
		must.Equal([]string{"test_tool", "read_file"}, toolNames(child.Visible(false)))
	})

	must.True(parent.LazyTools.Loaded(lazyMCPTool), "spawning leaves the parent's loaded set alone")
}

func TestBuildLoop_LazyLoadingWiresLoaderAndToolSearch(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"}
	config.Executor = types.ExecutorConfig{Type: "local", Workspace: t.TempDir()}
	config.Tools = types.ToolsConfig{
		BuiltIn:     []string{"read_file", "run_command"},
		Profile:     "coding-classic",
		LazyLoading: types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file"}},
	}
	config.RuleOfTwo = disableRuleOfTwo()
	must.NoError(types.ValidateRunConfig(config), "ValidateRunConfig")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err, "BuildLoopWithTransport")
	defer func() { _ = loop.Close() }()

	must.NotNil(loop.LazyTools)
	must.NotNil(loop.Tools.Resolve(types.ToolSearchToolName))
	must.True(loop.LazyTools.Deferred("run_command"))
	must.Equal("run_command", loop.LazyTools.InternalID("bash"))
	must.True(loop.ToolSearch("", "claude-sonnet-4-6").Supported)
	must.False(loop.ToolSearch("", "claude-sonnet-4-20250514").Supported)
}

func TestBuildLoop_LazyLoadingOffRegistersNoToolSearch(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"}
	config.Executor = types.ExecutorConfig{Type: "local", Workspace: t.TempDir()}
	config.Tools = types.ToolsConfig{BuiltIn: []string{"read_file"}}
	config.RuleOfTwo = disableRuleOfTwo()
	must.NoError(types.ValidateRunConfig(config), "ValidateRunConfig")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err, "BuildLoopWithTransport")
	defer func() { _ = loop.Close() }()

	must.Nil(loop.LazyTools)
	must.Nil(loop.ToolSearch)
	must.Nil(loop.Tools.Resolve(types.ToolSearchToolName))
}
