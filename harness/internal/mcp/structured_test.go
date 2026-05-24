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

// mcpServerReturningCallResult stands up a fake MCP server that advertises a
// single tool and returns the supplied raw JSON object as the tools/call
// result. The raw form lets a test express content shapes (resource links,
// embedded resources, structuredContent) that the production wire types only
// partially decode, exercising the bridge's full preservation path.
func mcpServerReturningCallResult(t *testing.T, toolName, rawCallResult string) *httptest.Server {
	t.Helper()
	tools := []mcpTool{{Name: toolName, Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "tools/list":
			writeJSONRPCResult(w, req.ID, toolsListResult{Tools: tools})
		case "tools/call":
			resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(rawCallResult)}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			writeJSONRPCError(w, req.ID, -32601, "method not found")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// connectAndResolve connects the client to srv and returns the registered
// tool for serverName/toolName.
func connectAndResolve(t *testing.T, srv *httptest.Server, serverName, toolName string) *tool.Tool {
	t.Helper()
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	secrets := &stubSecretStore{secrets: map[string]string{}}
	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: serverName, URI: srv.URL}, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	resolved := registry.Resolve("mcp_" + serverName + "_" + toolName)
	if resolved == nil {
		t.Fatalf("tool mcp_%s_%s not registered", serverName, toolName)
	}
	return resolved
}

// TestMCP_PreservesStructuredContent pins that a tools/call response carrying
// structuredContent is preserved into the #231 envelope under the
// mcp_tool_result kind, with the text content kept as the canonical fallback.
func TestMCP_PreservesStructuredContent(t *testing.T) {
	raw := `{
		"content": [{"type": "text", "text": "42 rows"}],
		"structuredContent": {"rows": 42, "table": "users"}
	}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "query", raw), "db", "query")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "42 rows" {
		t.Errorf("text fallback = %q, want %q", res.Text, "42 rows")
	}
	if res.Kind != kindMCPToolResult {
		t.Errorf("kind = %q, want %q", res.Kind, kindMCPToolResult)
	}
	var env mcpStructuredResult
	if err := json.Unmarshal(res.Structured, &env); err != nil {
		t.Fatalf("decode envelope: %v\nstructured: %s", err, res.Structured)
	}
	var sc map[string]any
	if err := json.Unmarshal(env.StructuredContent, &sc); err != nil {
		t.Fatalf("decode structuredContent: %v", err)
	}
	if sc["rows"] != float64(42) || sc["table"] != "users" {
		t.Errorf("structuredContent = %v, want rows=42 table=users", sc)
	}
}

// TestMCP_MarksNonTextContent pins that resource links, embedded resources,
// and unrecognised content types are represented explicitly in the envelope
// rather than silently dropped, while the text content remains the fallback.
func TestMCP_MarksNonTextContent(t *testing.T) {
	raw := `{
		"content": [
			{"type": "text", "text": "see attached"},
			{"type": "resource_link", "uri": "file:///report.pdf", "name": "report", "mimeType": "application/pdf"},
			{"type": "resource", "resource": {"uri": "file:///note.txt", "mimeType": "text/plain", "text": "inline note"}},
			{"type": "image", "mimeType": "image/png", "data": "BASE64"},
			{"type": "future_kind"}
		]
	}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "fetch", raw), "files", "fetch")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "see attached" {
		t.Errorf("text fallback = %q, want %q", res.Text, "see attached")
	}
	var env mcpStructuredResult
	if err := json.Unmarshal(res.Structured, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	byKind := map[string]mcpNonTextContent{}
	for _, n := range env.NonText {
		byKind[n.Kind] = n
	}
	if len(env.NonText) != 4 {
		t.Fatalf("expected 4 non-text descriptors (link, resource, image, unsupported), got %d: %+v", len(env.NonText), env.NonText)
	}
	if rl, ok := byKind["resource_link"]; !ok || rl.URI != "file:///report.pdf" || rl.Name != "report" {
		t.Errorf("resource_link descriptor wrong/missing: %+v", rl)
	}
	if rs, ok := byKind["resource"]; !ok || rs.URI != "file:///note.txt" || rs.Text != "inline note" {
		t.Errorf("embedded resource descriptor wrong/missing: %+v", rs)
	}
	if img, ok := byKind["image"]; !ok || img.MimeType != "image/png" {
		t.Errorf("image descriptor wrong/missing: %+v", img)
	}
	if un, ok := byKind["unsupported"]; !ok || un.Name != "future_kind" {
		t.Errorf("unsupported descriptor wrong/missing (a new content kind must be marked, not dropped): %+v", un)
	}
}

// TestMCP_BoundsOversizedStructuredContent pins that a structuredContent
// object larger than the size bound is dropped (with a marker), not retained,
// so an untrusted server cannot blow up memory/trace via a giant payload. The
// text fallback survives intact.
func TestMCP_BoundsOversizedStructuredContent(t *testing.T) {
	huge := strings.Repeat("A", maxMCPStructuredSize+1)
	rawResult, err := json.Marshal(map[string]any{
		"content":           []map[string]any{{"type": "text", "text": "ok"}},
		"structuredContent": map[string]any{"blob": huge},
	})
	if err != nil {
		t.Fatalf("marshal raw result: %v", err)
	}
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "big", string(rawResult)), "hostile", "big")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("text fallback = %q, want %q", res.Text, "ok")
	}
	if len(res.Structured) > maxMCPStructuredSize {
		t.Fatalf("structured envelope %d bytes exceeds bound %d — oversized content not dropped", len(res.Structured), maxMCPStructuredSize)
	}
	var env mcpStructuredResult
	if err := json.Unmarshal(res.Structured, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.StructuredContent) != 0 {
		t.Errorf("oversized structuredContent should have been dropped, got %d bytes", len(env.StructuredContent))
	}
	if len(env.NonText) != 1 || env.NonText[0].Kind != "unsupported" {
		t.Errorf("expected an 'unsupported' marker for the dropped content, got %+v", env.NonText)
	}
}

// TestMCP_BoundsEmbeddedResourceText pins that an oversized embedded-resource
// text body is truncated to the per-resource bound and flagged Truncated,
// rather than inlined whole.
func TestMCP_BoundsEmbeddedResourceText(t *testing.T) {
	huge := strings.Repeat("B", maxMCPEmbeddedResourceTextLen+500)
	rawResult, err := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "ok"},
			{"type": "resource", "resource": map[string]any{"uri": "file:///big.txt", "text": huge}},
		},
	})
	if err != nil {
		t.Fatalf("marshal raw result: %v", err)
	}
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "bigres", string(rawResult)), "hostile", "bigres")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var env mcpStructuredResult
	if err := json.Unmarshal(res.Structured, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.NonText) != 1 {
		t.Fatalf("expected 1 resource descriptor, got %+v", env.NonText)
	}
	d := env.NonText[0]
	if d.Kind != "resource" || !d.Truncated {
		t.Errorf("descriptor = %+v, want kind=resource truncated=true", d)
	}
	if len(d.Text) != maxMCPEmbeddedResourceTextLen {
		t.Errorf("inlined text = %d bytes, want exactly %d (truncated)", len(d.Text), maxMCPEmbeddedResourceTextLen)
	}
}

// TestMCP_TextOnlyResultHasNoStructured pins the no-regression path: a
// text-only tools/call result produces no structured envelope and no kind, so
// the result is byte-identical to the pre-#231 text-only shape downstream.
func TestMCP_TextOnlyResultHasNoStructured(t *testing.T) {
	raw := `{"content": [{"type": "text", "text": "plain"}]}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "echo", raw), "srv", "echo")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "plain" {
		t.Errorf("text = %q, want %q", res.Text, "plain")
	}
	if len(res.Structured) != 0 {
		t.Errorf("text-only result must carry no structured envelope, got %s", res.Structured)
	}
	if res.Kind != "" {
		t.Errorf("text-only result must carry no kind, got %q", res.Kind)
	}
}
