package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

var (
	scriptHarnessSearch = []types.StreamEvent{
		toolCall("search-1", types.ToolSearchToolName, `{"query":"github issue"}`),
		{Type: "message_complete", StopReason: "tool_use"},
	}
	scriptCallLazyTool = []types.StreamEvent{
		toolCall("lazy-1", lazyMCPTool, `{}`),
		{Type: "message_complete", StopReason: "tool_use"},
	}
)

// answeringPermissionTransport answers every permission_request with a
// fixed decision and records the tools it was asked about.
type answeringPermissionTransport struct {
	allow    bool
	mu       sync.Mutex
	handlers []func(types.ControlEvent)
	requests []string
}

func (a *answeringPermissionTransport) Emit(event types.HarnessEvent) error {
	if event.Type != "permission_request" {
		return nil
	}
	a.mu.Lock()
	a.requests = append(a.requests, event.ToolName)
	handlers := slices.Clone(a.handlers)
	a.mu.Unlock()
	allow := a.allow
	go func() {
		for _, h := range handlers {
			h(types.ControlEvent{Type: "permission_response", RequestID: event.RequestID, Allowed: &allow, Reason: "operator decision"})
		}
	}()
	return nil
}

func (a *answeringPermissionTransport) OnControl(handler func(types.ControlEvent)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers = append(a.handlers, handler)
}

func (a *answeringPermissionTransport) requested() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// preToolCountingGuard allows everything and records the tools the
// PhasePreTool guard was asked about.
type preToolCountingGuard struct {
	mu    sync.Mutex
	tools []string
}

func (g *preToolCountingGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	if in.Phase == guard.PhasePreTool {
		g.mu.Lock()
		g.tools = append(g.tools, in.ToolName)
		g.mu.Unlock()
	}
	return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "pre-tool-counter"}, nil
}

func (g *preToolCountingGuard) checked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.tools)
}

func TestLoop_LazyTools_LoadedToolIsStillPermissionGated(t *testing.T) {
	t.Run("deny policy", func(t *testing.T) {
		must := require.New(t)
		prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{scriptHarnessSearch, scriptCallLazyTool}}
		loop, _, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
		loop.Permissions = denyPolicy{}

		_, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.True(loop.LazyTools.Loaded(lazyMCPTool))
		must.Zero(*calls)
		must.Equal("Permission denied: test deny", rec.turnRecords[1].ToolCalls[0].Output)
	})

	for _, allow := range []bool{true, false} {
		name := "approval denied"
		wantCalls := 0
		if allow {
			name, wantCalls = "approval granted", 1
		}
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{scriptHarnessSearch, scriptCallLazyTool}}
			loop, _, _, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
			answers := &answeringPermissionTransport{allow: allow}
			loop.Permissions = permission.NewAskUpstreamPolicy(answers, map[string]bool{lazyMCPTool: true}, 5*time.Second)

			_, err := loop.Run(context.Background(), buildTestConfig())
			must.NoError(err)
			must.Equal([]string{lazyMCPTool}, answers.requested())
			must.Equal(wantCalls, *calls)
		})
	}
}

func TestLoop_LazyTools_UnloadedCallSkipsPermissionAndPreToolGuard(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{scriptCallLazyTool, scriptHarnessSearch, scriptCallLazyTool}}
	loop, _, rec, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	answers := &answeringPermissionTransport{allow: true}
	loop.Permissions = permission.NewAskUpstreamPolicy(answers, map[string]bool{lazyMCPTool: true}, 5*time.Second)
	gr := &preToolCountingGuard{}
	loop.GuardRail = gr

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal(`Tool "mcp_github_create_issue" is not loaded; call tool_search to load it first`, rec.turnRecords[0].ToolCalls[0].Output)
	must.Equal([]string{lazyMCPTool}, answers.requested(), "only the loaded call asks for permission")
	must.Equal([]string{types.ToolSearchToolName, lazyMCPTool}, gr.checked(), "only the search and the loaded call reach the pre_tool guard")
	must.Equal(1, *calls)
}

func TestBuildLoop_LazyLoadingInReadOnlyModeCannotSurfaceMutatingTools(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")
	config := buildTestConfig()
	config.Mode = "planning"
	config.Provider = types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"}
	config.Executor = types.ExecutorConfig{Type: "local", Workspace: t.TempDir()}
	config.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	config.Tools = types.ToolsConfig{
		BuiltIn:     []string{"read_file", "grep_files"},
		LazyLoading: types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file"}, MaxResults: types.MaxLazyToolSearchResults},
	}
	config.RuleOfTwo = disableRuleOfTwo()
	must.NoError(types.ValidateRunConfig(config), "ValidateRunConfig")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err, "BuildLoopWithTransport")
	defer func() { _ = loop.Close() }()

	found := loop.LazyTools.Search("write edit file run command shell replace diff apply search", 0)
	must.NotEmpty(found, "the search reaches the read-only tools it may load")
	for _, name := range []string{"write_file", "edit_file", "run_command", "search_replace", "apply_diff"} {
		must.Nil(loop.Tools.Resolve(name), "%s is registered in a read-only run", name)
		must.False(loop.LazyTools.Deferred(name), "%s is deferred in a read-only run", name)
		for _, m := range found {
			must.NotEqual(name, m.ID, "tool_search surfaced %s", name)
		}
	}
}

func TestRunFollowUpLoop_LazyTools_LoadedSetFollowsTheHistory(t *testing.T) {
	cases := []struct {
		history    string
		wantLoaded bool
	}{
		{types.FollowUpHistoryContinue, true},
		{types.FollowUpHistoryReset, false},
	}
	for _, tc := range cases {
		t.Run(tc.history, func(t *testing.T) {
			must := require.New(t)
			prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptHarnessSearch, scriptEndTurn, scriptCallLazyTool, scriptEndTurn}}
			loop, tr := buildUserInputTestLoop(prov)
			registry := loop.Tools.(*tool.Registry)
			calls := 0
			registry.Register(&tool.Tool{
				Name:        lazyMCPTool,
				Description: "Open a new GitHub issue",
				InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
				Handler: func(context.Context, json.RawMessage) (string, error) {
					calls++
					return "issue opened", nil
				},
			})
			registry.Register(builtins.ToolSearchTool(types.LazyToolLoadingConfig{}))
			loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{})
			config := buildTestConfig()
			config.FollowUpHistory = tc.history

			followUpSession(t, loop, tr, config, "now open the issue")

			sent := prov.params()
			must.Len(sent, 4)
			must.Equal(tc.wantLoaded, slices.Contains(toolNames(sent[2].Tools), lazyMCPTool), "the follow-up's first request")
			if tc.wantLoaded {
				must.Equal(1, calls)
				return
			}
			must.Zero(calls)
			last := sent[3].Messages[len(sent[3].Messages)-1]
			must.Equal(`Tool "mcp_github_create_issue" is not loaded; call tool_search to load it first`, last.Content[0].Content)
		})
	}

	t.Run("second run on the loop", func(t *testing.T) {
		must := require.New(t)
		prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{scriptHarnessSearch, scriptEndTurn, scriptCallLazyTool, scriptEndTurn}}
		loop, _, _, calls := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})

		_, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.True(loop.LazyTools.Loaded(lazyMCPTool))
		_, err = loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.NotContains(toolNames(prov.sent()[2].Tools), lazyMCPTool)
		must.Zero(*calls)
	})
}

// routeByTurn routes turn 0 to the primary provider and every later turn
// to the provider key "chat".
type routeByTurn struct{}

func (routeByTurn) Select(_ context.Context, rc router.RouterContext) router.ModelSelection {
	if rc.Turn == 0 {
		return router.ModelSelection{Model: "claude-sonnet-4-6"}
	}
	return router.ModelSelection{Provider: "chat", Model: "gpt-4o"}
}

func TestLoop_LazyTools_RoutedProviderDecidesTheSearchPerTurn(t *testing.T) {
	must := require.New(t)
	primary := &scriptedParamsProvider{calls: [][]types.StreamEvent{scriptCallTestTool}}
	routed := &scriptedParamsProvider{}
	loop, _, _, _ := buildLazyTestLoop(t, primary, types.LazyToolLoadingConfig{})
	loop.Router = routeByTurn{}
	loop.Providers = map[string]provider.ProviderAdapter{"chat": routed}
	config := buildTestConfig()
	config.Providers = map[string]types.ProviderConfig{"chat": {Type: "openai", APIKeyRef: "secret://TEST"}}
	resolve := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
	var asked []string
	loop.ToolSearch = func(key, model string) quirks.ToolSearchCapability {
		asked = append(asked, key+"/"+model)
		return resolve(key, model)
	}

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal([]string{"/claude-sonnet-4-6", "chat/gpt-4o"}, asked)

	first := primary.sent()[0]
	must.True(first.ToolSearch)
	must.NotContains(toolNames(first.Tools), types.ToolSearchToolName)

	second := routed.sent()[0]
	must.False(second.ToolSearch, "a provider without native search gets the harness search")
	must.Equal([]string{"test_tool", types.ToolSearchToolName}, toolNames(second.Tools))
	for _, def := range second.Tools {
		must.False(def.Deferred, "%s is flagged deferred for a provider without native search", def.Name)
	}
}

func TestLoop_LazyTools_NativeSearchUnderAToolsetProfile(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(nil)
	registry := loop.Tools.(*tool.Registry)
	registry.Register(&tool.Tool{
		Name:              "run_command",
		Description:       "Run a shell command",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		WorkspaceMutating: true,
		Handler:           func(context.Context, json.RawMessage) (string, error) { return "", nil },
	})
	registry.Register(builtins.ToolSearchTool(types.LazyToolLoadingConfig{}))
	profile, ok := tool.ProfileFor("coding-classic")
	must.True(ok)
	presenter, err := tool.NewPresenter(registry, profile)
	must.NoError(err)
	loop.Tools = presenter
	loop.LazyTools = tool.NewLazyLoader(presenter, types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"test_tool"}}, tool.LazyLoaderOptions{})
	loop.ToolSearch = nativeToolSearch("bm25")
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("tool_use", nativeSearchEvents("srvtoolu_1", "shell", "bash"), []types.StreamEvent{toolCall("c1", "test_tool", `{}`)}),
	}}
	loop.Provider = prov
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	_, err = loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.True(loop.LazyTools.Loaded("run_command"))
	must.Equal([]string{"run_command"}, rec.turnRecords[0].ModelOutput[1].ToolReferences, "the record keeps the internal ID")
	must.Equal([]string{"run_command"}, rec.turnRecords[0].ToolsLoaded)

	sent := prov.sent()
	must.Len(sent, 2)
	replayed := sent[1].Messages[1].Content[1]
	must.Equal("tool_search_result", replayed.Type)
	must.Equal([]string{"bash"}, replayed.ToolReferences, "the next request names the tool as the profile presents it")
	must.Contains(toolNames(sent[1].Tools), "bash")
}

func TestBuildLoop_LazyLoadingDefersControlPlaneTools(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"}
	config.Executor = types.ExecutorConfig{Type: "local", Workspace: t.TempDir()}
	config.Transport = types.TransportConfig{Type: "grpc"}
	config.Tools = controlPlaneToolsConfig()
	config.Tools.LazyLoading = types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness}
	config.RuleOfTwo = disableRuleOfTwo()
	must.NoError(types.ValidateRunConfig(config), "ValidateRunConfig")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err, "BuildLoopWithTransport")
	defer func() { _ = loop.Close() }()

	must.True(loop.LazyTools.Deferred("search_memory"))
	must.True(loop.LazyTools.Deferred("save_memory"))
	must.False(loop.LazyTools.Deferred("web_fetch"))
	visible := loop.LazyTools.Visible(false)
	search := visible[len(visible)-1]
	must.Equal(types.ToolSearchToolName, search.Name)
	must.Contains(search.Description, "2 tools provided by the control plane")
}

func TestSubAgent_LazyTools_ChildSearchLeavesTheParentAlone(t *testing.T) {
	must := require.New(t)
	parent := buildSubAgentTestLoop(&mockProvider{})
	registry := parent.Tools.(*tool.Registry)
	registerLazyMCPTool(registry, lazyMCPTool)
	registry.Register(builtins.ToolSearchTool(types.LazyToolLoadingConfig{}))
	parent.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{MCPServers: []string{"github"}})
	parentTransport := &recordingTransport{}
	parent.Transport = parentTransport

	run, err := prepareSubAgent(context.Background(), parent, buildTestConfig(), SubAgentConfig{Prompt: "open an issue", Mode: "execution"})
	must.NoError(err)
	run.loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{scriptHarnessSearch, spawnTextTurn("done")}}

	_, err = run.loop.Run(context.Background(), run.config)
	must.NoError(err)
	must.True(run.loop.LazyTools.Loaded(lazyMCPTool), "the child loaded the tool")
	must.False(parent.LazyTools.Loaded(lazyMCPTool), "the parent's loaded set is unchanged")
	must.Zero(parent.LazyTools.LoadedCount())
	must.Empty(toolsLoadedEvents(parentTransport), "the parent's transport reports no child load")
}
