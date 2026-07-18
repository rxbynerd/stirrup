package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
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
			_ = json.Unmarshal(raw, &params)

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
	_ = json.NewEncoder(w).Encode(resp)
}

func writeJSONRPCError(w http.ResponseWriter, id int64, code int, message string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// callMCPText invokes a registered MCP tool's StructuredHandler and returns
// the canonical text fallback. callMCPStructured returns the full
// StructuredResult for tests that assert the structured envelope.
func callMCPText(t *testing.T, tl *tool.Tool, input json.RawMessage) (string, error) {
	t.Helper()
	res, err := callMCPStructured(t, tl, input)
	return res.Text, err
}

func callMCPStructured(t *testing.T, tl *tool.Tool, input json.RawMessage) (tool.StructuredResult, error) {
	t.Helper()
	if tl.StructuredHandler == nil {
		t.Fatalf("tool %q has no StructuredHandler", tl.Name)
	}
	return tl.StructuredHandler(context.Background(), input)
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

	resolved := registry.Resolve("mcp_docs_search")
	if resolved == nil {
		t.Fatal("Resolve returned nil for mcp_docs_search")
	}
	if !resolved.WorkspaceMutating {
		t.Error("MCP tools should default to WorkspaceMutating=true")
	}
	if !resolved.RequiresApproval {
		t.Error("MCP tools should default to RequiresApproval=true")
	}
}

// TestConnect_ToolAnnotations verifies the bridge parses server-declared MCP
// tool annotations and surfaces them on the registered tool's
// ToolPresentation, while pinning the invariant that the hints are advisory:
// a server asserting readOnlyHint must NOT relax the conservative
// WorkspaceMutating/RequiresApproval gating the harness applies to all remote
// tools.
func TestConnect_ToolAnnotations(t *testing.T) {
	readOnly := true
	destructive := false
	tools := []mcpTool{
		{
			Name:        "lookup",
			Description: "Look up a record",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: &types.ToolAnnotations{
				Title:           "Lookup",
				ReadOnlyHint:    &readOnly,
				DestructiveHint: &destructive,
			},
		},
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "svc", URI: srv.URL}

	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolved := registry.Resolve("mcp_svc_lookup")
	if resolved == nil {
		t.Fatal("Resolve returned nil for mcp_svc_lookup")
	}
	if resolved.Annotations == nil {
		t.Fatal("server annotations were not carried onto the tool")
	}
	if resolved.Annotations.Title != "Lookup" {
		t.Errorf("Annotations.Title = %q, want %q", resolved.Annotations.Title, "Lookup")
	}
	if resolved.Annotations.ReadOnlyHint == nil || !*resolved.Annotations.ReadOnlyHint {
		t.Error("ReadOnlyHint should round-trip as true")
	}
	// The advisory hint must not override the conservative gating defaults.
	if !resolved.WorkspaceMutating || !resolved.RequiresApproval {
		t.Error("server readOnlyHint must not relax WorkspaceMutating/RequiresApproval")
	}

	// The annotations also surface on the model-facing Presentation.
	def := resolved.Definition()
	if def.Presentation == nil || def.Presentation.Annotations == nil {
		t.Fatalf("Definition().Presentation.Annotations missing: %+v", def.Presentation)
	}
	if def.Presentation.Annotations.Title != "Lookup" {
		t.Errorf("Presentation Annotations.Title = %q, want %q", def.Presentation.Annotations.Title, "Lookup")
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

	result, err := callMCPText(t, resolved, json.RawMessage(`{}`))
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

	resolved := registry.Resolve("mcp_sess_ping")
	if resolved == nil {
		t.Fatal("tool not found")
	}
	_, _ = callMCPText(t, resolved, json.RawMessage(`{}`))

	reqs = log.get()
	if len(reqs) < 2 {
		t.Fatal("expected at least 2 requests")
	}
	if reqs[1].SessionID != "session-abc-123" {
		t.Errorf("second request session ID = %q, want %q", reqs[1].SessionID, "session-abc-123")
	}
}

// TestConnect_RejectsOversizedSessionID pins the maxMCPSessionIDLen cap: a
// server that returns an Mcp-Session-Id longer than the cap must not have
// that value stored or echoed back.
func TestConnect_RejectsOversizedSessionID(t *testing.T) {
	oversized := strings.Repeat("x", maxMCPSessionIDLen+1)
	tools := []mcpTool{
		{Name: "ping", Description: "Ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srv, log := fakeMCPServer(t, tools, oversized)
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "sess", URI: srv.URL}

	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolved := registry.Resolve("mcp_sess_ping")
	if resolved == nil {
		t.Fatal("tool not found")
	}
	_, _ = callMCPText(t, resolved, json.RawMessage(`{}`))

	for i, r := range log.get() {
		if r.SessionID != "" {
			t.Errorf("request %d carried a session ID %q; oversized ID should have been rejected", i, r.SessionID)
		}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
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
	if got := err.Error(); !strings.Contains(got, "JSON-RPC error") {
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
	// Shared client using the default transport, which can reach both
	// httptest servers regardless of port.
	client := NewClient(registry, &http.Client{Timeout: 30 * time.Second})
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
	tools := []mcpTool{
		{Name: "fail", Description: "Always fails", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

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

	_, err := callMCPText(t, resolved, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error from tool call")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
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

	_ = client.Close()

	client.mu.Lock()
	n := len(client.sessions)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 sessions after Close, got %d", n)
	}
}

func TestConnect_RejectsFileSchemeURI(t *testing.T) {
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name: "evil",
		URI:  "file:///etc/passwd",
	}, secrets)
	if err == nil {
		t.Fatal("expected error for file:// URI scheme")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %q, want it to contain 'not allowed'", err.Error())
	}
}

func TestConnect_RejectsNoSchemeURI(t *testing.T) {
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name: "bad",
		URI:  "localhost:8080",
	}, secrets)
	if err == nil {
		t.Fatal("expected error for URI without http/https scheme")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %q, want it to contain 'not allowed'", err.Error())
	}
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

// TestMCPCall_RecordsMetrics_Success asserts that a successful tools/call
// records stirrup.mcp.calls (with success=true) and stirrup.mcp.duration_ms
// when the client has a Metrics instance attached.
func TestMCPCall_RecordsMetrics_Success(t *testing.T) {
	tools := []mcpTool{{
		Name:        "echo",
		Description: "Echo tool",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	client.Metrics = m

	secrets := &stubSecretStore{secrets: map[string]string{}}
	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: "metrics-srv", URI: srv.URL}, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolved := registry.Resolve("mcp_metrics-srv_echo")
	if resolved == nil {
		t.Fatal("tool not found")
	}
	if _, err := callMCPText(t, resolved, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Handler: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := findInt64Counter(t, rm, "stirrup.mcp.calls")
	if got.total != 1 {
		t.Errorf("stirrup.mcp.calls total = %d, want 1", got.total)
	}
	if got.attrs["server.name"] != "metrics-srv" {
		t.Errorf("server.name = %q, want %q", got.attrs["server.name"], "metrics-srv")
	}
	if got.attrs["tool.name"] != "echo" {
		t.Errorf("tool.name = %q, want %q", got.attrs["tool.name"], "echo")
	}
	if got.attrs["success"] != "true" {
		t.Errorf("success = %q, want %q", got.attrs["success"], "true")
	}

	dur := findFloat64Histogram(t, rm, "stirrup.mcp.duration_ms")
	if dur.count == 0 {
		t.Error("stirrup.mcp.duration_ms recorded no observations")
	}
	if dur.attrs["server.name"] != "metrics-srv" {
		t.Errorf("duration server.name = %q, want %q", dur.attrs["server.name"], "metrics-srv")
	}
}

// TestMCPCall_RecordsMetrics_Failure asserts a failed tools/call still
// records the counter with success=false; the success label distinguishes
// an isError=true response from a transport error (also success=false).
func TestMCPCall_RecordsMetrics_Failure(t *testing.T) {
	tools := []mcpTool{{
		Name:        "boom",
		Description: "Always errors",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "tools/list":
			writeJSONRPCResult(w, req.ID, toolsListResult{Tools: tools})
		case "tools/call":
			writeJSONRPCResult(w, req.ID, toolsCallResult{
				IsError: true,
				Content: []contentItem{{Type: "text", Text: "boom failed"}},
			})
		}
	}))
	t.Cleanup(srv.Close)

	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	client.Metrics = m

	secrets := &stubSecretStore{secrets: map[string]string{}}
	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: "boom-srv", URI: srv.URL}, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolved := registry.Resolve("mcp_boom-srv_boom")
	if resolved == nil {
		t.Fatal("tool not found")
	}
	if _, err := callMCPText(t, resolved, json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected handler error from isError=true response")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := findInt64Counter(t, rm, "stirrup.mcp.calls")
	if got.total != 1 {
		t.Errorf("stirrup.mcp.calls total = %d, want 1", got.total)
	}
	if got.attrs["success"] != "false" {
		t.Errorf("success = %q, want %q", got.attrs["success"], "false")
	}
}

// counterDataPoint is a flattened view of a single int64 counter
// observation. The mcp tests below collect at most one observation per
// metric so we don't need a full per-attribute breakdown.
type counterDataPoint struct {
	total int64
	attrs map[string]string
}

func findInt64Counter(t *testing.T, rm metricdata.ResourceMetrics, name string) counterDataPoint {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is not a Sum[int64]", name)
			}
			if len(sum.DataPoints) == 0 {
				return counterDataPoint{}
			}
			dp := sum.DataPoints[0]
			attrs := make(map[string]string)
			for _, kv := range dp.Attributes.ToSlice() {
				attrs[string(kv.Key)] = kv.Value.String()
			}
			return counterDataPoint{total: dp.Value, attrs: attrs}
		}
	}
	t.Fatalf("metric %q not found", name)
	return counterDataPoint{}
}

type histogramDataPoint struct {
	count uint64
	attrs map[string]string
}

func findFloat64Histogram(t *testing.T, rm metricdata.ResourceMetrics, name string) histogramDataPoint {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			h, ok := mt.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q is not a Histogram[float64]", name)
			}
			if len(h.DataPoints) == 0 {
				return histogramDataPoint{}
			}
			dp := h.DataPoints[0]
			attrs := make(map[string]string)
			for _, kv := range dp.Attributes.ToSlice() {
				attrs[string(kv.Key)] = kv.Value.String()
			}
			return histogramDataPoint{count: dp.Count, attrs: attrs}
		}
	}
	t.Fatalf("metric %q not found", name)
	return histogramDataPoint{}
}

// TestRegisterMCPTool_TruncatesLongNames pins the metric-cardinality bound
// (CWE-400): a server-advertised tool name of arbitrary length must be
// truncated before it flows into the registry name or the `tool.name`
// metric attribute.
func TestRegisterMCPTool_TruncatesLongNames(t *testing.T) {
	longName := strings.Repeat("a", maxMCPToolNameLen+50)
	tools := []mcpTool{{
		Name:        longName,
		Description: "Tool with absurd name",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: "long", URI: srv.URL}, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Registry stores the prefixed, sanitised name. The remote-side name
	// portion of the registered tool must be exactly maxMCPToolNameLen.
	wantSuffix := strings.Repeat("a", maxMCPToolNameLen)
	wantRegistered := "mcp_long_" + wantSuffix
	if registry.Resolve(wantRegistered) == nil {
		defs := registry.List()
		got := make([]string, 0, len(defs))
		for _, d := range defs {
			got = append(got, d.Name)
		}
		t.Fatalf("expected registered tool %q, got names %v", wantRegistered, got)
	}
}

// TestRegisterMCPTool_RecordsTruncatedToolName extends the truncation
// test to the metrics path: a recorded mcp.calls observation must carry
// the truncated tool name so downstream cardinality is bounded even
// when a hostile MCP server returns a 4 KB name.
func TestRegisterMCPTool_RecordsTruncatedToolName(t *testing.T) {
	longName := strings.Repeat("a", maxMCPToolNameLen+50)
	tools := []mcpTool{{
		Name:        longName,
		Description: "Tool with absurd name",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	client.Metrics = m

	secrets := &stubSecretStore{secrets: map[string]string{}}
	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: "long-srv", URI: srv.URL}, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	wantSuffix := strings.Repeat("a", maxMCPToolNameLen)
	resolved := registry.Resolve("mcp_long-srv_" + wantSuffix)
	if resolved == nil {
		t.Fatal("registered tool not found")
	}
	if _, err := callMCPText(t, resolved, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Handler: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := findInt64Counter(t, rm, "stirrup.mcp.calls")
	gotName := got.attrs["tool.name"]
	if len(gotName) != maxMCPToolNameLen {
		t.Errorf("tool.name length = %d, want %d", len(gotName), maxMCPToolNameLen)
	}
	if gotName != wantSuffix {
		t.Errorf("tool.name = %q, want %q", gotName, wantSuffix)
	}
}

// TestConnect_CapsToolsPerServer asserts the per-server tool count cap
// applied at Connect time: the first maxMCPToolsPerServer entries are
// registered and the rest are dropped.
func TestConnect_CapsToolsPerServer(t *testing.T) {
	overflow := maxMCPToolsPerServer + 25
	tools := make([]mcpTool, overflow)
	for i := 0; i < overflow; i++ {
		tools[i] = mcpTool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "Generated",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: "flood", URI: srv.URL}, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got := len(registry.List())
	if got != maxMCPToolsPerServer {
		t.Errorf("registered tool count = %d, want %d (cap)", got, maxMCPToolsPerServer)
	}
}

// TestConnect_AllowedToolsFiltersAdvertised verifies the per-server
// allowlist: a server advertising a tool not in AllowedTools has that tool
// rejected at registration, while allowlisted tools register.
func TestConnect_AllowedToolsFiltersAdvertised(t *testing.T) {
	tools := []mcpTool{
		{Name: "search", Description: "ok", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "exfiltrate", Description: "evil", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fetch", Description: "ok", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	config := types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search", "fetch"},
	}
	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if registry.Resolve("mcp_docs_search") == nil {
		t.Error("allowlisted tool 'search' should be registered")
	}
	if registry.Resolve("mcp_docs_fetch") == nil {
		t.Error("allowlisted tool 'fetch' should be registered")
	}
	if registry.Resolve("mcp_docs_exfiltrate") != nil {
		t.Error("tool 'exfiltrate' is not in the allowlist and must be rejected")
	}
	if got := len(registry.List()); got != 2 {
		t.Errorf("registered tool count = %d, want 2", got)
	}
}

// TestConnect_EmptyAllowedToolsRegistersAll pins the backward-compatible
// default: an unset AllowedTools registers every advertised tool.
func TestConnect_EmptyAllowedToolsRegistersAll(t *testing.T) {
	tools := []mcpTool{
		{Name: "a", Description: "", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", Description: "", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	config := types.MCPServerConfig{Name: "srv", URI: srv.URL}
	if err := client.Connect(context.Background(), config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := len(registry.List()); got != 2 {
		t.Errorf("registered tool count = %d, want 2 (unset allowlist registers all)", got)
	}
}

// TestConnect_RejectsNonHTTPSRemote verifies a remote (non-loopback) server
// reached over plain http is refused — credentials and tool-call payloads
// must not travel in clear.
func TestConnect_RejectsNonHTTPSRemote(t *testing.T) {
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name: "remote",
		URI:  "http://mcp.example.com/rpc",
	}, secrets)
	if err == nil {
		t.Fatal("expected error for non-https remote URI")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error = %q, want it to mention https", err.Error())
	}
}

// TestConnect_RejectsPrivateIPURI verifies the connect-time SSRF guard refuses
// a server URI whose host is a non-loopback private address, reusing the
// shared web_fetch validator (security.ValidatePublicHost). 169.254.169.254 is
// the cloud metadata endpoint; 10.0.0.1 is RFC1918.
func TestConnect_RejectsPrivateIPURI(t *testing.T) {
	cases := []struct{ name, uri string }{
		{"link_local_metadata", "https://169.254.169.254/latest/meta-data"},
		{"rfc1918", "https://10.0.0.1/rpc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := tool.NewRegistry()
			client := NewClient(registry, nil)
			secrets := &stubSecretStore{secrets: map[string]string{}}

			err := client.Connect(context.Background(), types.MCPServerConfig{
				Name: "ssrf",
				URI:  tc.uri,
			}, secrets)
			if err == nil {
				t.Fatalf("expected SSRF rejection for %q", tc.uri)
			}
			if !strings.Contains(err.Error(), "private host") {
				t.Errorf("error = %q, want it to mention 'private host'", err.Error())
			}
		})
	}
}

// TestConnect_AllowedMCPHostsPin verifies AllowedMCPHosts rejects a URI whose
// host is not pinned and admits one that is.
func TestConnect_AllowedMCPHostsPin(t *testing.T) {
	srv, _ := fakeMCPServer(t, []mcpTool{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	// A pin that does not include the server's loopback host must reject.
	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name:            "pinned",
		URI:             srv.URL,
		AllowedMCPHosts: []string{"only.example.com"},
	}, secrets)
	if err == nil {
		t.Fatal("expected rejection: host not in allowedMCPHosts")
	}
	if !strings.Contains(err.Error(), "allowedMCPHosts") {
		t.Errorf("error = %q, want it to mention allowedMCPHosts", err.Error())
	}

	// A pin that includes the actual host (127.0.0.1) must admit.
	host := mustHost(t, srv.URL)
	registry2 := tool.NewRegistry()
	client2 := NewClient(registry2, srv.Client())
	if err := client2.Connect(context.Background(), types.MCPServerConfig{
		Name:            "pinned",
		URI:             srv.URL,
		AllowedMCPHosts: []string{host},
	}, secrets); err != nil {
		t.Fatalf("Connect with matching pin: %v", err)
	}
	if registry2.Resolve("mcp_pinned_a") == nil {
		t.Error("tool should register when host is pinned")
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}

// TestConnect_LocalhostNameDefaultTransport drives a full tool call to an
// http://localhost server through the DEFAULT transport
// (LoopbackAwareDialContext): a loopback NAME (which net.ParseIP cannot
// recognise as loopback) must be admitted at dial time, not refused by the
// SSRF guard. Reaching the registered tool's result is the proof the dial
// succeeded.
func TestConnect_LocalhostNameDefaultTransport(t *testing.T) {
	tools := []mcpTool{{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "tools/list":
			writeJSONRPCResult(w, req.ID, toolsListResult{Tools: tools})
		case "tools/call":
			writeJSONRPCResult(w, req.ID, toolsCallResult{Content: []contentItem{{Type: "text", Text: "pong"}}})
		}
	}))
	t.Cleanup(srv.Close)

	// httptest binds 127.0.0.1; rewrite to the localhost NAME so the dial
	// exercises the name path rather than the IP-literal path.
	uri := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	registry := tool.NewRegistry()
	client := NewClient(registry, nil) // default transport: LoopbackAwareDialContext
	secrets := &stubSecretStore{secrets: map[string]string{}}

	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: "local", URI: uri}, secrets); err != nil {
		t.Fatalf("Connect to %s: %v", uri, err)
	}
	resolved := registry.Resolve("mcp_local_ping")
	if resolved == nil {
		t.Fatal("tool not registered")
	}
	out, err := resolved.StructuredHandler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("tool call through loopback-name dial failed: %v", err)
	}
	if out.Text != "pong" {
		t.Fatalf("tool result = %q, want pong", out.Text)
	}
}

// credentialedURI rewrites base (e.g. an httptest server URL) to embed
// userinfo and a secret query parameter, matching the CWE-532 leak scenario
// of an operator putting credentials directly in the MCP server URI.
func credentialedURI(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base URL %q: %v", base, err)
	}
	u.User = url.UserPassword("user", "pass")
	u.RawQuery = "api_key=supersecret"
	return u.String()
}

// assertNoSecret fails if either the embedded credential or the secret query
// value appears in s.
func assertNoSecret(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, "user:pass") {
		t.Errorf("%s leaked userinfo: %q", where, s)
	}
	if strings.Contains(s, "supersecret") {
		t.Errorf("%s leaked query secret: %q", where, s)
	}
}

// TestCall_NonOKError_DoesNotLeakCredentials drives the non-200 error site in
// call(): the returned error must report a display-safe URI, never the raw
// credentialed one.
func TestCall_NonOKError_DoesNotLeakCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is sad", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	uri := credentialedURI(t, srv.URL)
	sess, err := newSession(context.Background(),
		types.MCPServerConfig{Name: "leaky", URI: uri},
		&stubSecretStore{secrets: map[string]string{}})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if strings.Contains(sess.displayURI, "user:pass") || strings.Contains(sess.displayURI, "supersecret") {
		t.Fatalf("displayURI itself leaked credentials: %q", sess.displayURI)
	}

	client := NewClient(tool.NewRegistry(), srv.Client())
	_, err = client.call(context.Background(), sess, "tools/list", nil)
	if err == nil {
		t.Fatal("expected error from non-200 response")
	}
	assertNoSecret(t, "non-200 error", err.Error())
}

// TestCall_TransportError_DoesNotLeakCredentials drives the transport-error
// site in call(): a dial failure against a credentialed loopback URI must not
// surface the credentials.
func TestCall_TransportError_DoesNotLeakCredentials(t *testing.T) {
	// Bind then immediately close a listener to obtain a port that refuses
	// connections, keeping the host loopback so the SSRF dial guard allows it.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	uri := credentialedURI(t, closedURL)
	sess, err := newSession(context.Background(),
		types.MCPServerConfig{Name: "leaky", URI: uri},
		&stubSecretStore{secrets: map[string]string{}})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}

	client := NewClient(tool.NewRegistry(), nil) // default transport: real dial
	_, err = client.call(context.Background(), sess, "tools/list", nil)
	if err == nil {
		t.Fatal("expected transport error from closed port")
	}
	assertNoSecret(t, "transport error", err.Error())
}

func TestDisplaySafeURI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"userinfo and query", "https://user:pass@host.example/path?api_key=supersecret", "https://host.example/path"},
		{"query only", "https://host.example/mcp?token=abc", "https://host.example/mcp"},
		{"fragment", "https://host.example/mcp#frag", "https://host.example/mcp"},
		{"clean uri unchanged", "https://host.example/mcp", "https://host.example/mcp"},
		{"unparseable", "ht!tp://\x7f", "<unparseable mcp uri>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := displaySafeURI(tt.in)
			if got != tt.want {
				t.Errorf("displaySafeURI(%q) = %q, want %q", tt.in, got, tt.want)
			}
			assertNoSecret(t, "displaySafeURI", got)
		})
	}
}
