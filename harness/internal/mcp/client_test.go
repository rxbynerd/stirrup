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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	must.NotNil(tl.StructuredHandler, "tool %q has no StructuredHandler", tl.Name)
	return tl.StructuredHandler(context.Background(), input)
}

func TestConnect_ToolDiscovery(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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

	must.NoError(client.Connect(context.Background(), config, secrets))

	defs := registry.List()
	must.Len(defs, 2)
	check.Equal("mcp_docs_search", defs[0].Name)
	check.Equal("mcp_docs_fetch", defs[1].Name)

	resolved := registry.Resolve("mcp_docs_search")
	must.NotNil(resolved, "Resolve returned nil for mcp_docs_search")
	check.True(resolved.WorkspaceMutating, "MCP tools should default to WorkspaceMutating=true")
	check.True(resolved.RequiresApproval, "MCP tools should default to RequiresApproval=true")
}

// TestConnect_ToolAnnotations verifies the bridge parses server-declared MCP
// tool annotations and surfaces them on the registered tool's
// ToolPresentation, while pinning the invariant that the hints are advisory:
// a server asserting readOnlyHint must NOT relax the conservative
// WorkspaceMutating/RequiresApproval gating the harness applies to all remote
// tools.
func TestConnect_ToolAnnotations(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	must.NoError(client.Connect(context.Background(), config, secrets))

	resolved := registry.Resolve("mcp_svc_lookup")
	must.NotNil(resolved, "Resolve returned nil for mcp_svc_lookup")
	check.Equal("svc", resolved.MCPServer, "the tool must record the server that registered it")
	must.NotNil(resolved.Annotations, "server annotations were not carried onto the tool")
	check.Equal("Lookup", resolved.Annotations.Title)
	must.NotNil(resolved.Annotations.ReadOnlyHint)
	check.True(*resolved.Annotations.ReadOnlyHint, "ReadOnlyHint should round-trip as true")

	// The advisory hint must not override the conservative gating defaults.
	check.True(resolved.WorkspaceMutating, "server readOnlyHint must not relax WorkspaceMutating")
	check.True(resolved.RequiresApproval, "server readOnlyHint must not relax RequiresApproval")

	// The annotations also surface on the model-facing Presentation.
	def := resolved.Definition()
	must.NotNil(def.Presentation, "Definition().Presentation is missing")
	must.NotNil(def.Presentation.Annotations, "Definition().Presentation.Annotations is missing")
	check.Equal("Lookup", def.Presentation.Annotations.Title)
}

func TestConnect_ToolCallDispatch(t *testing.T) {
	must := require.New(t)
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

	must.NoError(client.Connect(context.Background(), config, secrets))

	resolved := registry.Resolve("mcp_test_echo")
	must.NotNil(resolved, "tool not found")

	result, err := callMCPText(t, resolved, json.RawMessage(`{}`))
	must.NoError(err)
	must.Equal("result from echo", result)
}

func TestConnect_SessionIDManagement(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	must.NoError(client.Connect(context.Background(), config, secrets))

	// tools/list was request #1 — should have no session ID.
	reqs := log.get()
	must.NotEmpty(reqs)
	check.Empty(reqs[0].SessionID, "first request should have no session ID")

	resolved := registry.Resolve("mcp_sess_ping")
	must.NotNil(resolved, "tool not found")
	_, _ = callMCPText(t, resolved, json.RawMessage(`{}`))

	reqs = log.get()
	must.GreaterOrEqual(len(reqs), 2)
	check.Equal("session-abc-123", reqs[1].SessionID)
}

// TestConnect_RejectsOversizedSessionID pins the maxMCPSessionIDLen cap: a
// server that returns an Mcp-Session-Id longer than the cap must not have
// that value stored or echoed back.
func TestConnect_RejectsOversizedSessionID(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	oversized := strings.Repeat("x", maxMCPSessionIDLen+1)
	tools := []mcpTool{
		{Name: "ping", Description: "Ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srv, log := fakeMCPServer(t, tools, oversized)
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "sess", URI: srv.URL}

	must.NoError(client.Connect(context.Background(), config, secrets))

	resolved := registry.Resolve("mcp_sess_ping")
	must.NotNil(resolved, "tool not found")
	_, _ = callMCPText(t, resolved, json.RawMessage(`{}`))

	for i, r := range log.get() {
		check.Emptyf(r.SessionID, "request %d carried an oversized session ID", i)
	}
}

func TestConnect_AuthHeader(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	must.NoError(client.Connect(context.Background(), config, secrets))

	reqs := log.get()
	must.NotEmpty(reqs)
	check.Equal("Bearer my-secret-token", reqs[0].Authorization)
}

func TestConnect_NoAuthHeaderWhenNoAPIKeyRef(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tools := []mcpTool{}

	srv, log := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())

	secrets := &stubSecretStore{secrets: map[string]string{}}
	config := types.MCPServerConfig{Name: "noauth", URI: srv.URL}

	must.NoError(client.Connect(context.Background(), config, secrets))

	reqs := log.get()
	check.Empty(reqs[0].Authorization, "Authorization header should be absent")
}

func TestConnect_JSONRPCError(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "JSON-RPC error")
}

func TestConnect_ToolNamePrefixing(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(client.Connect(context.Background(), config, secrets))

	resolved := registry.Resolve("mcp_analytics_run_query")
	must.NotNil(resolved, "expected tool with prefixed name mcp_analytics_run_query")
	check.Equal("Run a query", resolved.Description)
}

func TestConnect_MultipleServers(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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

	must.NoError(client.Connect(context.Background(), configA, secrets), "connect alpha")
	must.NoError(client.Connect(context.Background(), configB, secrets), "connect beta")

	defs := registry.List()
	must.Len(defs, 3)

	check.NotNil(registry.Resolve("mcp_alpha_search"), "missing mcp_alpha_search")
	check.NotNil(registry.Resolve("mcp_beta_search"), "missing mcp_beta_search")
	check.NotNil(registry.Resolve("mcp_beta_index"), "missing mcp_beta_index")
}

func TestConnect_MissingName(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{URI: "http://localhost"}, secrets)
	must.Error(err)
}

func TestConnect_MissingURI(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{Name: "test"}, secrets)
	must.Error(err)
}

func TestConnect_ToolCallError(t *testing.T) {
	must := require.New(t)
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
	must.NoError(client.Connect(context.Background(), config, secrets))

	resolved := registry.Resolve("mcp_errserver_fail")
	must.NotNil(resolved, "tool not found")

	_, err := callMCPText(t, resolved, json.RawMessage(`{}`))
	must.ErrorContains(err, "something went wrong")
}

func TestClient_Close_RemovesSessions(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tools := []mcpTool{
		{Name: "x", Description: "X", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	srv, _ := fakeMCPServer(t, tools, "sid-123")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	config := types.MCPServerConfig{Name: "srv", URI: srv.URL}
	must.NoError(client.Connect(context.Background(), config, secrets))

	_ = client.Close()

	client.mu.Lock()
	n := len(client.sessions)
	client.mu.Unlock()
	check.Zero(n, "sessions remaining after Close")
}

func TestConnect_RejectsFileSchemeURI(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name: "evil",
		URI:  "file:///etc/passwd",
	}, secrets)
	must.ErrorContains(err, "not allowed")
}

func TestConnect_RejectsNoSchemeURI(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name: "bad",
		URI:  "localhost:8080",
	}, secrets)
	must.ErrorContains(err, "not allowed")
}

func TestNewClient_NilHTTPClient_HasTimeout(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	check.NotZero(client.httpClient.Timeout, "default HTTP client should have a non-zero timeout")

	tr, ok := client.httpClient.Transport.(*http.Transport)
	must.True(ok, "expected *http.Transport as default transport")
	check.NotZero(tr.TLSHandshakeTimeout, "TLSHandshakeTimeout should be non-zero")
}

func TestNewClient_ExplicitHTTPClient_Preserved(t *testing.T) {
	check := assert.New(t)
	registry := tool.NewRegistry()
	custom := &http.Client{}
	client := NewClient(registry, custom)
	check.Same(custom, client.httpClient, "explicit HTTP client should be preserved, not replaced")
}

// TestMCPCall_RecordsMetrics_Success asserts that a successful tools/call
// records stirrup.mcp.calls (with success=true) and stirrup.mcp.duration
// when the client has a Metrics instance attached.
// attachTestMetrics routes the client's metrics to a manual reader so tests
// can inspect the observations recorded by a call.
func attachTestMetrics(t *testing.T, client *Client) *sdkmetric.ManualReader {
	t.Helper()
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	must.NoError(err)
	client.Metrics = m
	return reader
}

func TestMCPCall_RecordsMetrics_Success(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	tools := []mcpTool{{
		Name:        "echo",
		Description: "Echo tool",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	reader := attachTestMetrics(t, client)

	secrets := &stubSecretStore{secrets: map[string]string{}}
	must.NoError(client.Connect(context.Background(), types.MCPServerConfig{Name: "metrics-srv", URI: srv.URL}, secrets))

	resolved := registry.Resolve("mcp_metrics-srv_echo")
	must.NotNil(resolved, "tool not found")
	_, err := callMCPText(t, resolved, json.RawMessage(`{}`))
	must.NoError(err)

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))

	calls := findInt64Counter(t, rm, "stirrup.mcp.calls")
	check.Equal(int64(1), calls.total, "stirrup.mcp.calls total")
	check.Equal("metrics-srv", calls.attrs["server.name"], "server.name attribute")
	check.Equal("echo", calls.attrs["tool.name"], "tool.name attribute")
	check.Equal("true", calls.attrs["success"], "success attribute")

	duration := findFloat64Histogram(t, rm, "stirrup.mcp.duration")
	check.NotZero(duration.count, "stirrup.mcp.duration observations")
	check.Equal("metrics-srv", duration.attrs["server.name"], "duration server.name attribute")
}

// TestMCPCall_RecordsMetrics_Failure asserts a failed tools/call still
// records the counter with success=false; the success label distinguishes
// an isError=true response from a transport error (also success=false).
func TestMCPCall_RecordsMetrics_Failure(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	reader := attachTestMetrics(t, client)

	secrets := &stubSecretStore{secrets: map[string]string{}}
	must.NoError(client.Connect(context.Background(), types.MCPServerConfig{Name: "boom-srv", URI: srv.URL}, secrets))

	resolved := registry.Resolve("mcp_boom-srv_boom")
	must.NotNil(resolved, "tool not found")
	_, err := callMCPText(t, resolved, json.RawMessage(`{}`))
	must.Error(err, "expected handler error from isError=true response")

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))

	calls := findInt64Counter(t, rm, "stirrup.mcp.calls")
	check.Equal(int64(1), calls.total, "stirrup.mcp.calls total")
	check.Equal("false", calls.attrs["success"], "success attribute")
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
	must := require.New(t)
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			must.True(ok, "metric %q is not a Sum[int64]", name)
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
	must.Failf("metric not found", "%q", name)
	return counterDataPoint{}
}

type histogramDataPoint struct {
	count uint64
	attrs map[string]string
}

func findFloat64Histogram(t *testing.T, rm metricdata.ResourceMetrics, name string) histogramDataPoint {
	t.Helper()
	must := require.New(t)
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			h, ok := mt.Data.(metricdata.Histogram[float64])
			must.True(ok, "metric %q is not a Histogram[float64]", name)
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
	must.Failf("metric not found", "%q", name)
	return histogramDataPoint{}
}

// TestRegisterMCPTool_TruncatesLongNames pins the metric-cardinality bound
// (CWE-400): a server-advertised tool name of arbitrary length must be
// truncated before it flows into the registry name or the `tool.name`
// metric attribute.
func TestRegisterMCPTool_TruncatesLongNames(t *testing.T) {
	must := require.New(t)
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
	must.NoError(client.Connect(context.Background(), types.MCPServerConfig{Name: "long", URI: srv.URL}, secrets))

	// Registry stores the prefixed, sanitised name. The remote-side name
	// portion of the registered tool must be exactly maxMCPToolNameLen.
	wantSuffix := strings.Repeat("a", maxMCPToolNameLen)
	wantRegistered := "mcp_long_" + wantSuffix
	names := make([]string, 0, len(registry.List()))
	for _, d := range registry.List() {
		names = append(names, d.Name)
	}
	must.NotNil(registry.Resolve(wantRegistered), "expected registered tool %q, got names %v", wantRegistered, names)
}

// TestRegisterMCPTool_RecordsTruncatedToolName extends the truncation
// test to the metrics path: a recorded mcp.calls observation must carry
// the truncated tool name so downstream cardinality is bounded even
// when a hostile MCP server returns a 4 KB name.
func TestRegisterMCPTool_RecordsTruncatedToolName(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	longName := strings.Repeat("a", maxMCPToolNameLen+50)
	tools := []mcpTool{{
		Name:        longName,
		Description: "Tool with absurd name",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	reader := attachTestMetrics(t, client)

	secrets := &stubSecretStore{secrets: map[string]string{}}
	must.NoError(client.Connect(context.Background(), types.MCPServerConfig{Name: "long-srv", URI: srv.URL}, secrets))

	wantSuffix := strings.Repeat("a", maxMCPToolNameLen)
	resolved := registry.Resolve("mcp_long-srv_" + wantSuffix)
	must.NotNil(resolved, "registered tool not found")
	_, err := callMCPText(t, resolved, json.RawMessage(`{}`))
	must.NoError(err)

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))

	calls := findInt64Counter(t, rm, "stirrup.mcp.calls")
	gotName := calls.attrs["tool.name"]
	check.Len(gotName, maxMCPToolNameLen, "tool.name length")
	check.Equal(wantSuffix, gotName, "tool.name attribute")
}

// TestConnect_CapsToolsPerServer asserts the per-server tool count cap
// applied at Connect time: the first maxMCPToolsPerServer entries are
// registered and the rest are dropped.
func TestConnect_CapsToolsPerServer(t *testing.T) {
	must := require.New(t)
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
	must.NoError(client.Connect(context.Background(), types.MCPServerConfig{Name: "flood", URI: srv.URL}, secrets))

	must.Len(registry.List(), maxMCPToolsPerServer, "registered tools should be capped at maxMCPToolsPerServer")
}

// TestConnect_AllowedToolsFiltersAdvertised verifies the per-server
// allowlist: a server advertising a tool not in AllowedTools has that tool
// rejected at registration, while allowlisted tools register.
func TestConnect_AllowedToolsFiltersAdvertised(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(client.Connect(context.Background(), config, secrets))

	check.NotNil(registry.Resolve("mcp_docs_search"), "allowlisted tool search should be registered")
	check.NotNil(registry.Resolve("mcp_docs_fetch"), "allowlisted tool fetch should be registered")
	check.Nil(registry.Resolve("mcp_docs_exfiltrate"), "tool exfiltrate is not in the allowlist and must be rejected")
	check.Len(registry.List(), 2, "registered tool count")
}

// TestConnect_EmptyAllowedToolsRegistersAll pins the backward-compatible
// default: an unset AllowedTools registers every advertised tool.
func TestConnect_EmptyAllowedToolsRegistersAll(t *testing.T) {
	must := require.New(t)
	tools := []mcpTool{
		{Name: "a", Description: "", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", Description: "", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	config := types.MCPServerConfig{Name: "srv", URI: srv.URL}
	must.NoError(client.Connect(context.Background(), config, secrets))
	must.Len(registry.List(), 2, "an unset allowlist should register every advertised tool")
}

// TestConnect_RejectsNonHTTPSRemote verifies a remote (non-loopback) server
// reached over plain http is refused — credentials and tool-call payloads
// must not travel in clear.
func TestConnect_RejectsNonHTTPSRemote(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	client := NewClient(registry, nil)
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name: "remote",
		URI:  "http://mcp.example.com/rpc",
	}, secrets)
	must.Error(err, "expected error for non-https remote URI")
	must.ErrorContains(err, "https")
}

// TestConnect_RejectsPrivateIPURI verifies the connect-time SSRF guard refuses
// a server URI whose host is a non-loopback private address, reusing the
// shared web_fetch validator (security.ValidatePublicHost). 169.254.169.254 is
// the cloud metadata endpoint; 10.0.0.1 is RFC1918.
func TestConnect_RejectsPrivateIPURI(t *testing.T) {
	cases := []struct{ name, uri string }{
		{"link-local metadata address", "https://169.254.169.254/latest/meta-data"},
		{"rfc1918 address", "https://10.0.0.1/rpc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			registry := tool.NewRegistry()
			client := NewClient(registry, nil)
			secrets := &stubSecretStore{secrets: map[string]string{}}

			err := client.Connect(context.Background(), types.MCPServerConfig{
				Name: "ssrf",
				URI:  tc.uri,
			}, secrets)
			must.Error(err, "expected SSRF rejection for %q", tc.uri)
			must.ErrorContains(err, "private host")
		})
	}
}

// TestConnect_AllowedMCPHostsPin verifies AllowedMCPHosts rejects a URI whose
// host is not pinned and admits one that is.
func TestConnect_AllowedMCPHostsPin(t *testing.T) {
	srv, _ := fakeMCPServer(t, []mcpTool{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}, "")
	secrets := &stubSecretStore{secrets: map[string]string{}}

	t.Run("rejects host outside the pin", func(t *testing.T) {
		must := require.New(t)
		client := NewClient(tool.NewRegistry(), srv.Client())
		err := client.Connect(context.Background(), types.MCPServerConfig{
			Name:            "pinned",
			URI:             srv.URL,
			AllowedMCPHosts: []string{"only.example.com"},
		}, secrets)
		must.Error(err, "expected rejection: host not in allowedMcpHosts")
		must.ErrorContains(err, "allowedMcpHosts")
	})

	t.Run("admits the pinned host", func(t *testing.T) {
		must := require.New(t)
		registry := tool.NewRegistry()
		client := NewClient(registry, srv.Client())
		host := mustHost(t, srv.URL)
		must.NoError(client.Connect(context.Background(), types.MCPServerConfig{
			Name:            "pinned",
			URI:             srv.URL,
			AllowedMCPHosts: []string{host},
		}, secrets))
		must.NotNil(registry.Resolve("mcp_pinned_a"), "tool should register when host is pinned")
	})
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	must := require.New(t)
	u, err := url.Parse(raw)
	must.NoError(err, "parse %q", raw)
	return u.Hostname()
}

// TestConnect_LocalhostNameDefaultTransport drives a full tool call to an
// http://localhost server through the DEFAULT transport
// (LoopbackAwareDialContext): a loopback NAME (which net.ParseIP cannot
// recognise as loopback) must be admitted at dial time, not refused by the
// SSRF guard. Reaching the registered tool's result is the proof the dial
// succeeded.
func TestConnect_LocalhostNameDefaultTransport(t *testing.T) {
	must := require.New(t)
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

	must.NoError(client.Connect(context.Background(), types.MCPServerConfig{Name: "local", URI: uri}, secrets), "connect to %s", uri)
	resolved := registry.Resolve("mcp_local_ping")
	must.NotNil(resolved, "tool not registered")
	out, err := resolved.StructuredHandler(context.Background(), json.RawMessage(`{}`))
	must.NoError(err, "tool call through loopback-name dial")
	must.Equal("pong", out.Text)
}

// credentialedURI rewrites base (e.g. an httptest server URL) to embed
// userinfo and a secret query parameter, matching the CWE-532 leak scenario
// of an operator putting credentials directly in the MCP server URI.
func credentialedURI(t *testing.T, base string) string {
	t.Helper()
	must := require.New(t)
	u, err := url.Parse(base)
	must.NoError(err, "parse base URL %q", base)
	u.User = url.UserPassword("user", "pass")
	u.RawQuery = "api_key=supersecret"
	return u.String()
}

// assertNoSecret fails if either the embedded credential or the secret query
// value appears in s.
func assertNoSecret(t *testing.T, where, s string) {
	t.Helper()
	check := assert.New(t)
	check.NotContains(s, "user:pass", "%s leaked userinfo", where)
	check.NotContains(s, "supersecret", "%s leaked query secret", where)
}

// TestCall_NonOKError_DoesNotLeakCredentials drives the non-200 error site in
// call(): the returned error must report a display-safe URI, never the raw
// credentialed one.
func TestCall_NonOKError_DoesNotLeakCredentials(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is sad", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	uri := credentialedURI(t, srv.URL)
	sess, err := newSession(context.Background(),
		types.MCPServerConfig{Name: "leaky", URI: uri},
		&stubSecretStore{secrets: map[string]string{}})
	must.NoError(err)
	must.NotContains(sess.displayURI, "user:pass", "displayURI leaked userinfo")
	must.NotContains(sess.displayURI, "supersecret", "displayURI leaked query secret")

	client := NewClient(tool.NewRegistry(), srv.Client())
	_, err = client.call(context.Background(), sess, "tools/list", nil)
	must.Error(err, "expected error from non-200 response")
	assertNoSecret(t, "non-200 error", err.Error())
}

// TestCall_TransportError_DoesNotLeakCredentials drives the transport-error
// site in call(): a dial failure against a credentialed loopback URI must not
// surface the credentials.
func TestCall_TransportError_DoesNotLeakCredentials(t *testing.T) {
	must := require.New(t)
	// Bind then immediately close a listener to obtain a port that refuses
	// connections, keeping the host loopback so the SSRF dial guard allows it.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	uri := credentialedURI(t, closedURL)
	sess, err := newSession(context.Background(),
		types.MCPServerConfig{Name: "leaky", URI: uri},
		&stubSecretStore{secrets: map[string]string{}})
	must.NoError(err)

	client := NewClient(tool.NewRegistry(), nil) // default transport: real dial
	_, err = client.call(context.Background(), sess, "tools/list", nil)
	must.Error(err, "expected transport error from closed port")
	assertNoSecret(t, "transport error", err.Error())
}

func TestDisplaySafeURI_StripsUserinfoQueryAndFragment(t *testing.T) {
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
			check := assert.New(t)
			got := displaySafeURI(tt.in)
			check.Equal(tt.want, got, "displaySafeURI(%q)", tt.in)
			assertNoSecret(t, "displaySafeURI", got)
		})
	}
}

func TestConnect_MissingAllowlistedTool_ReturnsMissingToolsError(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	tools := []mcpTool{
		{Name: "search", Description: "Search", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "extra", Description: "Extra", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	err := client.Connect(context.Background(), types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search", "fetch", "fetch", "lookup"},
	}, secrets)

	var missing *MissingToolsError
	must.ErrorAs(err, &missing)
	check.Equal("docs", missing.Server)
	check.Equal("fetch,lookup", strings.Join(missing.Tools, ","), "missing tools in allowlist order, deduplicated")
	must.ErrorContains(err, `"docs"`)
	must.ErrorContains(err, "fetch, lookup")
	check.NotNil(registry.Resolve("mcp_docs_search"), "advertised allowlisted tool should still be registered")
	check.Len(registry.List(), 1, "only the advertised allowlisted tool should be registered")
}

func TestConnect_AllowlistFullyAdvertised_ConnectsCleanly(t *testing.T) {
	must := require.New(t)
	tools := []mcpTool{
		{Name: "search", Description: "Search", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "extra", Description: "Extra", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	srv, _ := fakeMCPServer(t, tools, "")
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}

	must.NoError(client.Connect(context.Background(), types.MCPServerConfig{
		Name:         "docs",
		URI:          srv.URL,
		AllowedTools: []string{"search"},
	}, secrets))
	must.Len(registry.List(), 1)
}
