package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
