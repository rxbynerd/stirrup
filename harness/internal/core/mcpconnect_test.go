package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// mcpToolsServer answers every JSON-RPC request with a tools/list result
// advertising the named tools.
func mcpToolsServer(t *testing.T, toolNames ...string) *httptest.Server {
	t.Helper()
	tools := make([]map[string]any, 0, len(toolNames))
	for _, name := range toolNames {
		tools = append(tools, map[string]any{
			"name":        name,
			"description": name,
			"inputSchema": map[string]any{"type": "object"},
		})
	}
	result, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": tools}})
	if err != nil {
		t.Fatal(err)
	}
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
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t)})

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	if err == nil {
		_ = loop.Close()
		t.Fatal("expected construction to fail for an unreachable required MCP server")
	}
	if !strings.Contains(err.Error(), `required MCP server "docs"`) {
		t.Errorf("error should name the server, got: %v", err)
	}
	if w := tp.warnings(); len(w) != 0 {
		t.Errorf("a required-server failure must not be downgraded to a warning, got %v", w)
	}
}

func TestBuildLoop_RequiredMCPServerUnreachableFailsWithExecutorNone(t *testing.T) {
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t)})
	cfg.Mode = "planning"
	cfg.Executor = types.ExecutorConfig{Type: "none"}
	cfg.Tools.BuiltIn = []string{"web_fetch"}
	cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}

	loop, err := BuildLoopWithTransport(context.Background(), cfg, &recordingTransport{})
	if err == nil {
		_ = loop.Close()
		t.Fatal("expected an MCP-only run to fail when its only MCP server is unavailable")
	}
	if !strings.Contains(err.Error(), `required MCP server "docs"`) {
		t.Errorf("error should name the server, got: %v", err)
	}
}

func TestBuildLoop_RequiredMCPServerMissingAllowedToolFails(t *testing.T) {
	srv := mcpToolsServer(t, "search")
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search", "fetch"},
	})

	loop, err := BuildLoopWithTransport(context.Background(), cfg, &recordingTransport{})
	if err == nil {
		_ = loop.Close()
		t.Fatal("expected construction to fail when an allowlisted tool is not advertised")
	}
	for _, want := range []string{`"docs"`, "fetch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}

func TestBuildLoop_OptionalMCPServerUnreachableDegrades(t *testing.T) {
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t), Optional: true})

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	if err != nil {
		t.Fatalf("an unreachable optional server must not fail construction: %v", err)
	}
	defer func() { _ = loop.Close() }()

	if len(loop.DegradedCapabilities) != 1 {
		t.Fatalf("DegradedCapabilities = %+v, want one entry", loop.DegradedCapabilities)
	}
	got := loop.DegradedCapabilities[0]
	if got.Kind != types.DegradedCapabilityMCP || got.Name != "docs" || got.Error == "" {
		t.Errorf("DegradedCapabilities[0] = %+v, want kind=mcp name=docs with an error", got)
	}
	warnings := tp.warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"docs"`) {
		t.Errorf("warnings = %v, want one naming the server", warnings)
	}
}

func TestBuildLoop_OptionalMCPServerMissingAllowedToolKeepsAdvertisedTools(t *testing.T) {
	srv := mcpToolsServer(t, "search", "other")
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search", "fetch"},
		Optional:     true,
	})

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	if err != nil {
		t.Fatalf("a partial optional server must not fail construction: %v", err)
	}
	defer func() { _ = loop.Close() }()

	if loop.Tools.Resolve("mcp_docs_search") == nil {
		t.Error("the advertised allowlisted tool should be registered")
	}
	if loop.Tools.Resolve("mcp_docs_other") != nil {
		t.Error("a tool outside the allowlist must not be registered")
	}
	if len(loop.DegradedCapabilities) != 1 || !strings.Contains(loop.DegradedCapabilities[0].Error, "fetch") {
		t.Errorf("DegradedCapabilities = %+v, want one entry naming the missing tool", loop.DegradedCapabilities)
	}
	if len(tp.warnings()) != 1 {
		t.Errorf("warnings = %v, want one", tp.warnings())
	}
}

func TestBuildLoop_HealthyMCPServerBehavesAsBefore(t *testing.T) {
	srv := mcpToolsServer(t, "search", "fetch")
	cfg := mcpFactoryConfig(t,
		types.MCPServerConfig{Name: "docs", URI: srv.URL, AllowedTools: []string{"search", "fetch"}},
		types.MCPServerConfig{Name: "extra", URI: srv.URL, Optional: true},
	)

	tp := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), cfg, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	if loop.Tools.Resolve("mcp_docs_search") == nil || loop.Tools.Resolve("mcp_docs_fetch") == nil {
		t.Error("allowlisted tools should be registered")
	}
	if len(loop.DegradedCapabilities) != 0 {
		t.Errorf("DegradedCapabilities = %+v, want none", loop.DegradedCapabilities)
	}
	if w := tp.warnings(); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}

func TestRun_DegradedCapabilitiesReachRunTrace(t *testing.T) {
	cfg := mcpFactoryConfig(t, types.MCPServerConfig{Name: "docs", URI: unreachableMCPURL(t), Optional: true})
	provider := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"r-1","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer provider.Close()
	cfg.Provider.BaseURL = provider.URL

	loop, err := BuildLoopWithTransport(context.Background(), cfg, &recordingTransport{})
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if len(runTrace.DegradedCapabilities) != 1 || runTrace.DegradedCapabilities[0].Name != "docs" {
		t.Errorf("RunTrace.DegradedCapabilities = %+v, want the skipped docs server", runTrace.DegradedCapabilities)
	}
}

func TestBoundDegradedError_ScrubsAndTruncates(t *testing.T) {
	long := strings.Repeat("x", maxDegradedErrorBytes*2)
	got := boundDegradedError(errString(long))
	if len(got) > maxDegradedErrorBytes+len("... [truncated by harness]") {
		t.Errorf("len(got) = %d, want bounded near %d", len(got), maxDegradedErrorBytes)
	}
	if !strings.HasSuffix(got, "[truncated by harness]") {
		t.Errorf("a truncated error should carry the truncation marker, got suffix %q", got[len(got)-30:])
	}
}

func TestBoundDegradedError_ScrubsSecrets(t *testing.T) {
	const secret = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	got := boundDegradedError(errString("upstream rejected " + secret))
	if strings.Contains(got, secret) {
		t.Errorf("degraded error leaked a secret: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
