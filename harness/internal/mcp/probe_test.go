package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func TestProbe_HandshakeSucceedsWithoutRegistering(t *testing.T) {
	tools := []mcpTool{
		{Name: "search", Description: "Search", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "docs", URI: srv.URL}

	if err := client.Probe(context.Background(), config, secrets); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}

	// A probe must NOT register tools — that is the contract difference
	// from Connect.
	if defs := registry.List(); len(defs) != 0 {
		t.Errorf("Probe registered %d tools; expected 0 (probe must not mutate the registry)", len(defs))
	}
}

func TestProbe_BadScheme(t *testing.T) {
	client := NewClient(tool.NewRegistry(), nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "bad", URI: "file:///etc/passwd"}

	err := client.Probe(context.Background(), config, secrets)
	if err == nil {
		t.Fatal("Probe: expected error for non-http scheme")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error should mention the rejected scheme, got: %v", err)
	}
}

func TestProbe_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	client := NewClient(tool.NewRegistry(), srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "down", URI: srv.URL}

	err := client.Probe(context.Background(), config, secrets)
	if err == nil {
		t.Fatal("Probe: expected error for 5xx server")
	}
	if !strings.Contains(err.Error(), "down") {
		t.Errorf("error should name the server, got: %v", err)
	}
}
