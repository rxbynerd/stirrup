package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func TestProbe_HandshakeSucceedsWithoutRegistering(t *testing.T) {
	must := require.New(t)
	tools := []mcpTool{
		{Name: "search", Description: "Search", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "docs", URI: srv.URL}

	err := client.Probe(context.Background(), config, secrets)
	must.NoError(err)

	// Unlike Connect, a probe must not register tools.
	must.Empty(registry.List(), "probe must not mutate the registry")
}

func TestProbe_RejectsNonHTTPScheme(t *testing.T) {
	must := require.New(t)
	client := NewClient(tool.NewRegistry(), nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "bad", URI: "file:///etc/passwd"}

	err := client.Probe(context.Background(), config, secrets)
	must.Error(err)
	must.ErrorContains(err, "scheme")
}

func TestProbe_FailsOnServerError(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	client := NewClient(tool.NewRegistry(), srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "down", URI: srv.URL}

	err := client.Probe(context.Background(), config, secrets)
	must.Error(err)
	must.ErrorContains(err, "down")
}

func TestProbe_ReportsMissingAllowlistedTool(t *testing.T) {
	must := require.New(t)
	srv, _ := fakeMCPServer(t, []mcpTool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}, "")
	client := NewClient(tool.NewRegistry(), srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Probe(context.Background(), types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search", "fetch"},
	}, secrets)

	var missing *MissingToolsError
	must.ErrorAs(err, &missing)
	must.Equal([]string{"fetch"}, missing.Tools)
}

func TestProbe_AcceptsFullyAdvertisedAllowlist(t *testing.T) {
	must := require.New(t)
	srv, _ := fakeMCPServer(t, []mcpTool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}, "")
	client := NewClient(tool.NewRegistry(), srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Probe(context.Background(), types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search"},
	}, secrets)
	must.NoError(err)
}

func TestProbeTools_ReturnsRegistryNamesWithoutRegistering(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{"type":"object"}`)
	srv, _ := fakeMCPServer(t, []mcpTool{
		{Name: "search", InputSchema: schema},
		{Name: "fetch", InputSchema: schema},
	}, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	names, err := client.ProbeTools(context.Background(), types.MCPServerConfig{Name: "docs", URI: srv.URL}, secrets)

	must.NoError(err)
	must.ElementsMatch([]string{"mcp_docs_search", "mcp_docs_fetch"}, names)
	must.Empty(registry.List())
}

func TestProbeTools_AppliesAllowlistAndReportsMissing(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{"type":"object"}`)
	srv, _ := fakeMCPServer(t, []mcpTool{
		{Name: "search", InputSchema: schema},
		{Name: "fetch", InputSchema: schema},
	}, "")
	client := NewClient(tool.NewRegistry(), srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "docs", URI: srv.URL, AllowedTools: []string{"search", "absent"}}

	names, err := client.ProbeTools(context.Background(), config, secrets)

	var missing *MissingToolsError
	must.ErrorAs(err, &missing)
	must.Equal([]string{"absent"}, missing.Tools)
	must.Equal([]string{"mcp_docs_search"}, names)
}

func TestProbeTools_ReturnsTheNamesConnectRegisters(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{"type":"object"}`)
	advertised := []mcpTool{{Name: strings.Repeat("n", maxMCPToolNameLen+20), InputSchema: schema}}
	for i := range maxMCPToolsPerServer + 10 {
		advertised = append(advertised, mcpTool{Name: fmt.Sprintf("tool_%d", i), InputSchema: schema})
	}
	srv, _ := fakeMCPServer(t, advertised, "")
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "docs", URI: srv.URL}

	connected := tool.NewRegistry()
	must.NoError(NewClient(connected, srv.Client()).Connect(context.Background(), config, secrets))
	registered := make([]string, 0, maxMCPToolsPerServer)
	for _, def := range connected.List() {
		registered = append(registered, def.Name)
	}

	probed, err := NewClient(tool.NewRegistry(), srv.Client()).ProbeTools(context.Background(), config, secrets)

	must.NoError(err)
	must.Len(registered, maxMCPToolsPerServer)
	must.ElementsMatch(registered, probed)
	must.Contains(probed, "mcp_docs_"+strings.Repeat("n", maxMCPToolNameLen), "an over-long name is truncated identically")
}
