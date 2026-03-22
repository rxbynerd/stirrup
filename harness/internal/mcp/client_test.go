package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// stubSecretStore is a test double for security.SecretStore.
type stubSecretStore struct {
	secrets map[string]string
}

func (s *stubSecretStore) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := s.secrets[ref]
	if !ok {
		return "", fmt.Errorf("secret not found: %q", ref)
	}
	return v, nil
}

// fakeMCPServer creates an httptest.Server that speaks JSON-RPC 2.0 for
// tools/list and tools/call. It returns the server and a pointer to the
// recorded request headers for assertions.
func fakeMCPServer(t *testing.T, tools []mcpTool, sessionID string) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.requests = append(log.requests, capturedRequest{
			Authorization: r.Header.Get("Authorization"),
			SessionID:     r.Header.Get("Mcp-Session-Id"),
		})
		log.mu.Unlock()

		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Set session ID header if configured.
		if sessionID != "" {
			w.Header().Set("Mcp-Session-Id", sessionID)
		}

		switch req.Method {
		case "tools/list":
			result := toolsListResult{Tools: tools}
			writeJSONRPCResult(w, req.ID, result)

		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			raw, _ := json.Marshal(req.Params)
			json.Unmarshal(raw, &params)

			result := toolsCallResult{
				Content: []contentItem{{Type: "text", Text: "result from " + params.Name}},
			}
			writeJSONRPCResult(w, req.ID, result)

		default:
			writeJSONRPCError(w, req.ID, -32601, "method not found")
		}
	}))

	t.Cleanup(srv.Close)
	return srv, log
}

type capturedRequest struct {
	Authorization string
	SessionID     string
}

type requestLog struct {
	mu       sync.Mutex
	requests []capturedRequest
}

func (l *requestLog) get() []capturedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]capturedRequest, len(l.requests))
	copy(out, l.requests)
	return out
}

func writeJSONRPCResult(w http.ResponseWriter, id int64, result interface{}) {
	raw, _ := json.Marshal(result)
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: raw}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func writeJSONRPCError(w http.ResponseWriter, id int64, code int, message string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func TestConnect_ToolDiscovery(t *testing.T) {
	tools := []mcpTool{
		{
			Name:        "search",
			Description: "Search documents",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
		},
		{
			Name:        "fetch",
			Description: "Fetch a URL",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}}}`),
		},
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "docs", URI: srv.URL}

	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Verify tools are registered with prefixed names.
	defs := registry.List()
	if len(defs) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(defs))
	}

	if defs[0].Name != "mcp_docs_search" {
		t.Errorf("tool[0].Name = %q, want %q", defs[0].Name, "mcp_docs_search")
	}
	if defs[1].Name != "mcp_docs_fetch" {
		t.Errorf("tool[1].Name = %q, want %q", defs[1].Name, "mcp_docs_fetch")
	}

	// Verify tool has side effects.
	resolved := registry.Resolve("mcp_docs_search")
	if resolved == nil {
		t.Fatal("Resolve returned nil for mcp_docs_search")
	}
	if !resolved.SideEffects {
		t.Error("MCP tools should default to SideEffects=true")
	}
}

func TestConnect_ToolCallDispatch(t *testing.T) {
	tools := []mcpTool{
		{
			Name:        "echo",
			Description: "Echo tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "test", URI: srv.URL}

	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolved := registry.Resolve("mcp_test_echo")
	if resolved == nil {
		t.Fatal("tool not found")
	}

	result, err := resolved.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if result != "result from echo" {
		t.Errorf("result = %q, want %q", result, "result from echo")
	}
}

func TestConnect_SessionIDManagement(t *testing.T) {
	tools := []mcpTool{
		{
			Name:        "ping",
			Description: "Ping",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}

	srv, log := fakeMCPServer(t, tools, "session-abc-123")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "sess", URI: srv.URL}

	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// tools/list was request #1 — should have no session ID.
	reqs := log.get()
	if len(reqs) < 1 {
		t.Fatal("expected at least 1 request")
	}
	if reqs[0].SessionID != "" {
		t.Errorf("first request should have no session ID, got %q", reqs[0].SessionID)
	}

	// Now call a tool — should send the session ID we got back.
	resolved := registry.Resolve("mcp_sess_ping")
	if resolved == nil {
		t.Fatal("tool not found")
	}
	resolved.Handler(context.Background(), json.RawMessage(`{}`))

	reqs = log.get()
	if len(reqs) < 2 {
		t.Fatal("expected at least 2 requests")
	}
	if reqs[1].SessionID != "session-abc-123" {
		t.Errorf("second request session ID = %q, want %q", reqs[1].SessionID, "session-abc-123")
	}
}

func TestConnect_AuthHeader(t *testing.T) {
	tools := []mcpTool{}

	srv, log := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{
		"secret://MCP_TOKEN": "my-secret-token",
	}}
	config := types.MCPServerConfig{
		Name:      "authed",
		URI:       srv.URL,
		APIKeyRef: "secret://MCP_TOKEN",
	}

	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	reqs := log.get()
	if len(reqs) < 1 {
		t.Fatal("expected at least 1 request")
	}
	want := "Bearer my-secret-token"
	if reqs[0].Authorization != want {
		t.Errorf("Authorization = %q, want %q", reqs[0].Authorization, want)
	}
}

func TestConnect_NoAuthHeaderWhenNoAPIKeyRef(t *testing.T) {
	tools := []mcpTool{}

	srv, log := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "noauth", URI: srv.URL}

	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	reqs := log.get()
	if reqs[0].Authorization != "" {
		t.Errorf("Authorization should be empty, got %q", reqs[0].Authorization)
	}
}

func TestConnect_JSONRPCError(t *testing.T) {
	// Create a server that returns an error for tools/list.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		writeJSONRPCError(w, req.ID, -32600, "invalid request")
	}))
	t.Cleanup(srv.Close)

	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "broken", URI: srv.URL}

	err := client.Connect(context.Background(), config, secrets)
	if err == nil {
		t.Fatal("expected error from Connect")
	}
	if got := err.Error(); !contains(got, "JSON-RPC error") {
		t.Errorf("error = %q, want it to contain 'JSON-RPC error'", got)
	}
}

func TestConnect_ToolNamePrefixing(t *testing.T) {
	tools := []mcpTool{
		{
			Name:        "run_query",
			Description: "Run a query",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	config := types.MCPServerConfig{Name: "analytics", URI: srv.URL}
	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolved := registry.Resolve("mcp_analytics_run_query")
	if resolved == nil {
		t.Fatal("expected tool with prefixed name mcp_analytics_run_query")
	}
	if resolved.Description != "Run a query" {
		t.Errorf("Description = %q, want %q", resolved.Description, "Run a query")
	}
}

func TestConnect_MultipleServers(t *testing.T) {
	toolsA := []mcpTool{
		{Name: "search", Description: "Search A", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	toolsB := []mcpTool{
		{Name: "search", Description: "Search B", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "index", Description: "Index B", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srvA, _ := fakeMCPServer(t, toolsA, "")
	srvB, _ := fakeMCPServer(t, toolsB, "")

	registry := tool.NewRegistry()
	// Both servers share an http client that can reach both test servers.
	// Since httptest servers use different ports, we need to use the default
	// transport which can reach any local address.
	client := NewClient(registry, &http.Client{})
	secrets := &stubSecretStore{secrets: map[string]string{}}

	configA := types.MCPServerConfig{Name: "alpha", URI: srvA.URL}
	configB := types.MCPServerConfig{Name: "beta", URI: srvB.URL}

	if err := client.Connect(context.Background(), configA, secrets); err != nil {
		t.Fatalf("Connect alpha: %v", err)
	}
	if err := client.Connect(context.Background(), configB, secrets); err != nil {
		t.Fatalf("Connect beta: %v", err)
	}

	defs := registry.List()
	if len(defs) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(defs))
	}

	// Both "search" tools should be registered with distinct prefixed names.
	if registry.Resolve("mcp_alpha_search") == nil {
		t.Error("missing mcp_alpha_search")
	}
	if registry.Resolve("mcp_beta_search") == nil {
		t.Error("missing mcp_beta_search")
	}
	if registry.Resolve("mcp_beta_index") == nil {
		t.Error("missing mcp_beta_index")
	}
}

func TestConnect_MissingName(t *testing.T) {
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{URI: "http://localhost"}, secrets)
	if err == nil {
		t.Fatal("expected error for missing Name")
	}
}

func TestConnect_MissingURI(t *testing.T) {
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{Name: "test"}, secrets)
	if err == nil {
		t.Fatal("expected error for missing URI")
	}
}

func TestConnect_ToolCallError(t *testing.T) {
	// Server returns isError: true in tools/call response.
	tools := []mcpTool{
		{Name: "fail", Description: "Always fails", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		json.NewDecoder(r.Body).Decode(&req)

		switch req.Method {
		case "tools/list":
			result := toolsListResult{Tools: tools}
			writeJSONRPCResult(w, req.ID, result)
		case "tools/call":
			result := toolsCallResult{
				Content: []contentItem{{Type: "text", Text: "something went wrong"}},
				IsError: true,
			}
			writeJSONRPCResult(w, req.ID, result)
		}
	}))
	t.Cleanup(srv.Close)

	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	config := types.MCPServerConfig{Name: "errserver", URI: srv.URL}
	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolved := registry.Resolve("mcp_errserver_fail")
	if resolved == nil {
		t.Fatal("tool not found")
	}

	_, err := resolved.Handler(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error from tool call")
	}
	if !contains(err.Error(), "something went wrong") {
		t.Errorf("error = %q, want it to contain 'something went wrong'", err.Error())
	}
}

func TestClose(t *testing.T) {
	tools := []mcpTool{
		{Name: "x", Description: "X", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	srv, _ := fakeMCPServer(t, tools, "sid-123")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	config := types.MCPServerConfig{Name: "srv", URI: srv.URL}
	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	client.Close()

	client.mu.Lock()
	n := len(client.sessions)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 sessions after Close, got %d", n)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestNewClient_NilHTTPClient_HasTimeout(t *testing.T) {
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	if client.httpClient.Timeout == 0 {
		t.Error("default HTTP client should have a non-zero timeout")
	}
	tr, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport as default transport")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("TLSHandshakeTimeout should be non-zero")
	}
}

func TestNewClient_ExplicitHTTPClient_Preserved(t *testing.T) {
	registry := tool.NewRegistry()
	custom := &http.Client{}
	client := NewClient(registry, custom)
	if client.httpClient != custom {
		t.Error("explicit HTTP client should be preserved, not replaced")
	}
}
