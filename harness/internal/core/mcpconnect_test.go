package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// mcpToolsServer answers every JSON-RPC request with a tools/list result
// advertising the named tools.
func mcpToolsServer(t *testing.T, toolNames ...string) *httptest.Server {
	t.Helper()
	must := require.New(t)
	tools := make([]map[string]any, 0, len(toolNames))
	for _, name := range toolNames {
		tools = append(tools, map[string]any{
			"name":        name,
			"description": name,
			"inputSchema": map[string]any{"type": "object"},
		})
	}
	result, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": tools}})
	must.NoError(err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// unreachableMCPURL returns the URL of a server that has been shut down,
// so connecting to it fails immediately.
func unreachableMCPURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

func mcpFactoryConfig(t *testing.T, servers ...types.MCPServerConfig) *types.RunConfig {
	t.Helper()
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider := newOpenAIServer(t, nil, nil, nil)
	t.Cleanup(provider.Close)

	timeout := 30
	return &types.RunConfig{
		RunID:            "mcp-required-test",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: provider.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         2,
		Timeout:          &timeout,
		Tools:            types.ToolsConfig{MCPServers: servers},
	}
}

func (tr *recordingTransport) warnings() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []string
	for _, e := range tr.events {
		if e.Type == "warning" {
			out = append(out, e.Message)
		}
	}
	return out
}

func TestBuildLoop_RequiredMCPServerUnreachableFails(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t)})

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	if err == nil {
		_ = loop.Close()
	}
	must.Error(err, "construction should fail for an unreachable required MCP server")
	must.ErrorContains(err, `required MCP server "docs"`, "error should name the server")
	check.Empty(tp.warnings(), "a required-server failure must not be downgraded to a warning")
}

func TestBuildLoop_RequiredMCPServerUnreachableFailsWithExecutorNone(t *testing.T) {
	must := require.New(t)
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t)})
	cfg.Mode = "planning"
	cfg.Executor = types.ExecutorConfig{Type: "none"}
	cfg.Tools.BuiltIn = []string{"web_fetch"}
	cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}

	loop, err := BuildLoopWithTransport(context.Background(), cfg, &recordingTransport{})
	if err == nil {
		_ = loop.Close()
	}
	must.Error(err, "an MCP-only run should fail when its only MCP server is unavailable")
	must.ErrorContains(err, `required MCP server "docs"`)
}

func TestBuildLoop_RequiredMCPServerMissingAllowedToolFails(t *testing.T) {
	must := require.New(t)
	srv := mcpToolsServer(t, "search")
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search", "fetch"},
	})

	loop, err := BuildLoopWithTransport(context.Background(), cfg, &recordingTransport{})
	if err == nil {
		_ = loop.Close()
	}
	must.Error(err, "construction should fail when an allowlisted tool is not advertised")
	t.Run("names the server", func(t *testing.T) {
		must := require.New(t)
		must.ErrorContains(err, `"docs"`)
	})
	t.Run("names the missing tool", func(t *testing.T) {
		must := require.New(t)
		must.ErrorContains(err, "fetch")
	})
}

func TestBuildLoop_OptionalMCPServerUnreachableDegrades(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t), Optional: true})

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	must.NoError(err, "an unreachable optional server must not fail construction")
	defer func() { _ = loop.Close() }()

	must.Len(loop.DegradedCapabilities, 1)
	got := loop.DegradedCapabilities[0]
	check.Equal(types.DegradedCapabilityMCP, got.Kind)
	check.Equal("docs", got.Name)
	check.NotEmpty(got.Error, "the degraded capability should record the failure reason")

	warnings := tp.warnings()
	must.Len(warnings, 1)
	must.Contains(warnings[0], `"docs"`, "the warning should name the server")
}

func TestBuildLoop_OptionalMCPServerMissingAllowedToolKeepsAdvertisedTools(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := mcpToolsServer(t, "search", "other")
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search", "fetch"},
		Optional:     true,
	})

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	must.NoError(err, "a partial optional server must not fail construction")
	defer func() { _ = loop.Close() }()

	check.NotNil(loop.Tools.Resolve("mcp_docs_search"), "the advertised allowlisted tool should be registered")
	check.Nil(loop.Tools.Resolve("mcp_docs_other"), "a tool outside the allowlist must not be registered")
	must.Len(loop.DegradedCapabilities, 1)
	check.Contains(loop.DegradedCapabilities[0].Error, "fetch", "the degraded reason should name the missing tool")
	check.Len(tp.warnings(), 1, "a partial optional server should emit one warning")
}

func TestBuildLoop_HealthyMCPServerRegistersToolsWithoutDegrading(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := mcpToolsServer(t, "search", "fetch")
	cfg := mcpFactoryConfig(t,
		types.MCPServerConfig{Name: "docs", URI: srv.URL, AllowedTools: []string{"search", "fetch"}},
		types.MCPServerConfig{Name: "extra", URI: srv.URL, Optional: true},
	)

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	check.NotNil(loop.Tools.Resolve("mcp_docs_search"), "the search tool should be registered")
	check.NotNil(loop.Tools.Resolve("mcp_docs_fetch"), "the fetch tool should be registered")
	check.Empty(loop.DegradedCapabilities)
	check.Empty(tp.warnings())
}

func TestRun_DegradedCapabilitiesReachRunTrace(t *testing.T) {
	must := require.New(t)
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t), Optional: true})
	provider := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"r-1","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer provider.Close()
	cfg.Provider.BaseURL = provider.URL

	loop, err := BuildLoopWithTransport(context.Background(), cfg, &recordingTransport{})
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), cfg)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.Len(runTrace.DegradedCapabilities, 1)
	must.Equal("docs", runTrace.DegradedCapabilities[0].Name, "the skipped docs server should be reported")
}

func TestBoundDegradedError_ScrubsAndTruncates(t *testing.T) {
	check := assert.New(t)
	long := strings.Repeat("x", maxDegradedErrorBytes*2)
	got := boundDegradedError(errString(long))
	check.LessOrEqual(len(got), maxDegradedErrorBytes+len("... [truncated by harness]"),
		"a truncated error should stay near %d bytes", maxDegradedErrorBytes)
	check.True(strings.HasSuffix(got, "[truncated by harness]"),
		"a truncated error should end with the truncation marker")
}

func TestBoundDegradedError_ScrubsSecrets(t *testing.T) {
	must := require.New(t)
	const secret = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	got := boundDegradedError(errString("upstream rejected " + secret))
	must.NotContains(got, secret, "a degraded error should not leak a secret")
}

type errString string

func (e errString) Error() string { return string(e) }

// leakyMCPServer answers every request with a JSON-RPC error whose message
// is long and carries a credential.
func leakyMCPServer(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	must := require.New(t)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]any{"code": -32000, "message": secret + " " + strings.Repeat("x", maxDegradedErrorBytes*4)},
	})
	must.NoError(err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBuildLoop_RequiredMCPServerErrorIsScrubbedAndBounded(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const secret = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	const prefix = `required MCP server "docs" unavailable: `
	srv := leakyMCPServer(t, secret)
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: srv.URL})

	loop, err := BuildLoopWithTransport(context.Background(), cfg, &recordingTransport{})
	if err == nil {
		_ = loop.Close()
	}
	must.Error(err, "construction should fail for a required MCP server returning an error")

	text := err.Error()
	check.NotContains(text, secret, "the required-server error should not leak a secret")
	must.True(strings.HasPrefix(text, prefix), "error should start with %q, got %q", prefix, text)
	reason := text[len(prefix):]
	must.LessOrEqual(len(reason), maxDegradedErrorBytes+len("... [truncated by harness]"),
		"the reason should be bounded near %d bytes", maxDegradedErrorBytes)
}
