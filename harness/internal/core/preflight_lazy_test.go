package core

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// lazyPreflightConfig is a validator-clean config with lazy loading on, a
// control-plane tool and one MCP server, over a stand-in provider endpoint.
func lazyPreflightConfig(t *testing.T, mcpURL string) *types.RunConfig {
	t.Helper()
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	t.Cleanup(srv.Close)
	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.LazyLoading = types.LazyToolLoadingConfig{Enabled: true}
	cfg.Tools.ControlPlane = []types.ControlPlaneToolConfig{{
		Name:        "lookup_order",
		Description: "Look up a customer order",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpURL}}
	cfg.Transport = types.TransportConfig{Type: "grpc", Address: "localhost:1"}
	return cfg
}

func TestPreflight_LazyToolsStepReportsGroupsAndStrategy(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search", "fetch")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, found := stepStatus(report, "lazy-tools")
	must.True(found, "steps: %+v", report.Steps)
	must.Equal(PreflightOK, status)
	detail, _ := stepDetail(report, "lazy-tools")
	must.Contains(detail, "strategy auto")
	must.Contains(detail, "control plane: 0 eager, 1 deferred")
	must.Contains(detail, `MCP server "docs": 0 eager, 2 deferred`)
	must.Contains(detail, "built-in: ")
	must.Contains(detail, "deferred")
	must.Contains(detail, "openai-compatible/test searches with the harness tool_search (model not in the native tool search allow-list)")
}

func TestPreflight_LazyToolsStepWarnsOnUnknownEager(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)
	cfg.Tools.LazyLoading.Eager = []string{"read_file", "no_such_tool", "mcp_docs_search"}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, found := stepStatus(report, "lazy-tools:eager:no_such_tool")
	must.True(found, "steps: %+v", report.Steps)
	must.Equal(PreflightWarn, status)
	_, found = stepStatus(report, "lazy-tools:eager:mcp_docs_search")
	must.False(found, "an MCP tool the server advertises is a known eager name")
	must.True(report.OK, "a warning does not fail the report")
}

func TestPreflight_LazyToolsStepDoesNotWarnOnMCPEagerNamesWhenMCPIsNotProbed(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)
	cfg.Tools.LazyLoading.Eager = []string{"read_file", "mcp_docs_search", "no_such_tool"}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipMCP: true})
	must.NoError(err)

	_, found := stepStatus(report, "lazy-tools:eager:mcp_docs_search")
	must.False(found, "an unprobed MCP tool cannot be called unknown")
	_, found = stepStatus(report, "lazy-tools:eager:no_such_tool")
	must.True(found)
	detail, _ := stepDetail(report, "lazy-tools")
	must.Contains(detail, "MCP tools are discovered at run start")
	must.Contains(detail, "--no-probe-mcp set")
}

func TestPreflight_LazyToolsStepSkippedWhenDisabled(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)
	cfg.Tools.LazyLoading = types.LazyToolLoadingConfig{}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, found := stepStatus(report, "lazy-tools")
	must.True(found)
	must.Equal(PreflightSkip, status)
}

func TestPreflight_LazyToolsStepNamesHarnessStrategyAsTheReason(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)
	cfg.Tools.LazyLoading.Strategy = types.LazyToolStrategyHarness

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	detail, _ := stepDetail(report, "lazy-tools")
	must.Contains(detail, "strategy harness")
	must.Contains(detail, "searches with the harness tool_search (strategy harness)")
}

func TestPreflight_LazyToolsStepNamesACustomBaseURLAsTheReason(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)
	cfg.Provider = types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://TEST_PREFLIGHT_KEY", BaseURL: "https://gateway.example.com/v1"}
	cfg.ModelRouter = types.ModelRouterConfig{Type: "static", Provider: "openai-responses", Model: "gpt-5.5"}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipProvider: true})
	must.NoError(err)

	detail, _ := stepDetail(report, "lazy-tools")
	must.Contains(detail, "openai-responses/gpt-5.5 searches with the harness tool_search (custom baseUrl)")
}

func TestPreflight_LazyToolsStepReportsNothingDeferred(t *testing.T) {
	must := require.New(t)
	cfg := lazyPreflightConfig(t, "")
	cfg.Tools.MCPServers = nil
	cfg.Tools.ControlPlane = nil

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	detail, _ := stepDetail(report, "lazy-tools")
	must.Contains(detail, "nothing is deferred, so every tool is sent")
}

// readOnlyBuiltIns is a built-in list a read-only mode and the api executor
// accept.
var readOnlyBuiltIns = []string{"read_file", "list_directory", "grep_files", "find_files"}

// probedDiscovery runs the dry-run MCP probe the way Preflight does.
func probedDiscovery(t *testing.T, cfg *types.RunConfig) mcpDiscovery {
	t.Helper()
	must := require.New(t)
	secrets, err := security.NewAutoSecretStore(context.Background(), cfg)
	must.NoError(err)
	noStatus := func(string, string) {}
	discovery := preflightMCP(context.Background(), cfg, secrets, PreflightOptions{}, noStatus, noStatus,
		func(step string, err error) { must.Fail("unexpected warning", "%s: %v", step, err) },
		func(step string, err error, _ string) { must.Fail("unexpected failure", "%s: %v", step, err) })
	must.Empty(discovery.incomplete)
	return discovery
}

// visibleIDs returns the sorted internal IDs of every registered tool the
// loader presents.
func visibleIDs(loader *tool.LazyLoader) []string {
	var ids []string
	for _, def := range loader.Visible(true) {
		ids = append(ids, loader.InternalID(def.Name))
	}
	slices.Sort(ids)
	return ids
}

// TestPreflight_LazyToolsMatchesBuiltLoop pins that the dry-run registers the
// tools a real run builds and splits them into the same eager and deferred
// groups, so the report cannot drift from the run it predicts.
func TestPreflight_LazyToolsMatchesBuiltLoop(t *testing.T) {
	fixtures := map[string]func(*testing.T, *types.RunConfig){
		"default eager": func(*testing.T, *types.RunConfig) {},
		"explicit eager": func(_ *testing.T, cfg *types.RunConfig) {
			cfg.Tools.LazyLoading.Eager = []string{"read_file", "mcp_docs_search", "lookup_order"}
		},
		"restricted built-ins": func(_ *testing.T, cfg *types.RunConfig) {
			cfg.Tools.BuiltIn = []string{"read_file", "write_file", "spawn_agent"}
		},
		"toolset profile": func(_ *testing.T, cfg *types.RunConfig) {
			cfg.Tools.Profile = "coding-classic"
		},
		"servers whose names overlap": func(t *testing.T, cfg *types.RunConfig) {
			cfg.Tools.MCPServers = []types.MCPServerConfig{
				{Name: "a", URI: mcpToolsServer(t, "b_fetch").URL},
				{Name: "a_b", URI: mcpToolsServer(t, "search").URL},
			}
		},
		"planning mode": func(_ *testing.T, cfg *types.RunConfig) {
			cfg.Mode = "planning"
			cfg.Tools.BuiltIn = readOnlyBuiltIns
		},
		"api executor": func(_ *testing.T, cfg *types.RunConfig) {
			cfg.Mode = "planning"
			cfg.Tools.BuiltIn = readOnlyBuiltIns
			cfg.Executor = types.ExecutorConfig{
				Type:       "api",
				VcsBackend: &types.VcsBackendConfig{Type: "github", Repo: "rxbynerd/stirrup"},
			}
		},
	}
	for name, mutate := range fixtures {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			mcpSrv := mcpToolsServer(t, "search", "fetch")
			cfg := lazyPreflightConfig(t, mcpSrv.URL)
			mutate(t, cfg)
			must.NoError(types.ValidateRunConfig(cfg))

			loop, err := BuildLoopWithTransport(context.Background(), cfg, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
			must.NoError(err)
			defer func() { _ = loop.Close() }()
			must.NotNil(loop.LazyTools)

			secrets, err := security.NewAutoSecretStore(context.Background(), cfg)
			must.NoError(err)
			execResult := preflightExecutorConstruct(context.Background(), cfg, secrets, nil)
			must.NoError(execResult.err)
			planned, err := preflightLazyLoader(cfg, execResult.exec, probedDiscovery(t, cfg))
			must.NoError(err)

			want := visibleIDs(loop.LazyTools)
			must.NotEmpty(want)
			check.Equal(want, visibleIDs(planned))
			for _, id := range want {
				check.Equal(loop.LazyTools.Deferred(id), planned.Deferred(id), "deferred state of %s", id)
				check.Equal(loop.LazyTools.GroupOf(id), planned.GroupOf(id), "group of %s", id)
			}
			check.Equal(loop.LazyTools.GroupCounts(), planned.GroupCounts())
			check.Equal(loop.LazyTools.UnknownEager(), planned.UnknownEager())
		})
	}
}

func TestPreflight_LazyToolsStepDoesNotClaimNothingIsDeferredWhenMCPIsNotProbed(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)
	cfg.Tools.ControlPlane = nil

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipMCP: true})
	must.NoError(err)

	detail, _ := stepDetail(report, "lazy-tools")
	must.NotContains(detail, "nothing is deferred")
	must.Contains(detail, "decided at run start once MCP tools are known")
	must.Contains(detail, "openai-compatible/test searches with the harness tool_search (model not in the native tool search allow-list)")
}

func TestPreflight_LazyToolsStepWarnsOnEagerNamesOfProbedServersOnly(t *testing.T) {
	must := require.New(t)
	cfg := lazyPreflightConfig(t, "")
	cfg.Tools.MCPServers = []types.MCPServerConfig{
		{Name: "docs", URI: unreachableMCPURL(t), Optional: true},
		{Name: "wiki", URI: mcpToolsServer(t, "search").URL},
	}
	cfg.Tools.LazyLoading.Eager = []string{"mcp_docs_search", "mcp_wiki_search", "mcp_wiki_missing"}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	_, found := stepStatus(report, "lazy-tools:eager:mcp_docs_search")
	must.False(found, "a server that did not answer may advertise the tool")
	_, found = stepStatus(report, "lazy-tools:eager:mcp_wiki_search")
	must.False(found)
	_, found = stepStatus(report, "lazy-tools:eager:mcp_wiki_missing")
	must.True(found, "a probed server's missing tool is unknown")
}

func TestCapabilityOnlyExecutor_RegistersTheToolsAContainerExecutorDoes(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	container := &executor.ContainerExecutor{}
	standIn := capabilityOnlyExecutor{caps: containerToolCapabilities()}

	wantCaps, gotCaps := container.Capabilities(), standIn.Capabilities()
	check.Equal(
		[]bool{wantCaps.CanRead, wantCaps.CanWrite, wantCaps.CanExec},
		[]bool{gotCaps.CanRead, gotCaps.CanWrite, gotCaps.CanExec},
	)

	toolNames := func(exec executor.Executor) []string {
		var names []string
		for _, def := range buildToolRegistry(exec, buildEditStrategy(types.EditStrategyConfig{Type: "whole-file"}), types.ToolsConfig{}, nil).List() {
			names = append(names, def.Name)
		}
		slices.Sort(names)
		return names
	}
	want := toolNames(container)
	must.NotEmpty(want)
	check.Equal(want, toolNames(standIn))
}

func TestPreflight_LazyToolsStepNamesEveryModeSelectionOfAPerModeRouter(t *testing.T) {
	must := require.New(t)
	mcpSrv := mcpToolsServer(t, "search")
	cfg := lazyPreflightConfig(t, mcpSrv.URL)
	cfg.ModelRouter = types.ModelRouterConfig{
		Type:       "per-mode",
		Provider:   "openai-compatible",
		Model:      "test",
		ModeModels: map[string]string{"planning": "openai-compatible/planner"},
	}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	detail, _ := stepDetail(report, "lazy-tools")
	must.Contains(detail, "openai-compatible/test searches with")
	must.Contains(detail, "openai-compatible/planner searches with")
}
