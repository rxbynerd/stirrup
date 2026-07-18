package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

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
// structuredContent is preserved into the envelope under the mcp_tool_result
// kind, with the text content kept as the canonical fallback.
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
			{"type": "audio", "mimeType": "audio/mpeg", "data": "BASE64"},
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
	if len(env.NonText) != 5 {
		t.Fatalf("expected 5 non-text descriptors (link, resource, image, audio, unsupported), got %d: %+v", len(env.NonText), env.NonText)
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
	if au, ok := byKind["audio"]; !ok || au.MimeType != "audio/mpeg" {
		t.Errorf("audio descriptor wrong/missing (shares the image arm but the discriminator must be 'audio'): %+v", au)
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
// text-only tools/call result produces no structured envelope and no kind.
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

// TestMCP_NullStructuredContentIsAbsent pins that an explicit
// "structuredContent": null does NOT fabricate a structured turn.
func TestMCP_NullStructuredContentIsAbsent(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"x"}],"structuredContent":null}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "nullsc", raw), "srv", "nullsc")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "x" {
		t.Errorf("text = %q, want %q", res.Text, "x")
	}
	if len(res.Structured) != 0 {
		t.Errorf("null structuredContent must produce no envelope, got %s", res.Structured)
	}
	if res.Kind != "" {
		t.Errorf("null structuredContent must produce no kind, got %q", res.Kind)
	}
}

// TestMCP_CapsContentItemCount pins that the content item-count cap fires
// BEFORE the descriptor slice is fully built: the envelope retains at most
// maxMCPContentItems descriptors plus a truncation marker, stays within the
// size bound, and the overflow is marked rather than silently dropped.
func TestMCP_CapsContentItemCount(t *testing.T) {
	items := make([]contentItem, maxMCPContentItems+200)
	for i := range items {
		items[i] = contentItem{Type: "image", MimeType: "image/png"}
	}
	encoded := buildMCPStructured(toolsCallResult{Content: items})
	if len(encoded) == 0 {
		t.Fatal("expected a non-nil envelope")
	}
	if len(encoded) > maxMCPStructuredSize {
		t.Fatalf("envelope %d bytes exceeds bound %d", len(encoded), maxMCPStructuredSize)
	}
	var env mcpStructuredResult
	if err := json.Unmarshal(encoded, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	// maxMCPContentItems image descriptors + 1 truncation marker.
	if len(env.NonText) != maxMCPContentItems+1 {
		t.Fatalf("expected %d descriptors (cap + marker), got %d", maxMCPContentItems+1, len(env.NonText))
	}
	var marked bool
	for _, n := range env.NonText {
		if n.Kind == "unsupported" && strings.Contains(n.Name, "item-count bound") {
			marked = true
		}
	}
	if !marked {
		t.Errorf("expected a truncation marker for the dropped items, got %+v", env.NonText)
	}
}

// TestMCP_OversizedAssembledEnvelopeIsMarked pins that when many
// individually-valid non-text descriptors push the assembled envelope past
// the size bound, the bridge returns a marker envelope rather than silently
// dropping everything.
func TestMCP_OversizedAssembledEnvelopeIsMarked(t *testing.T) {
	// Each resource_link carries a long URI; enough of them (under the item
	// count cap) exceed maxMCPStructuredSize once assembled.
	longURI := "file:///" + strings.Repeat("a", 2048)
	const n = 200 // 200 * ~2KB ~= 400KB > 256KB, but well under maxMCPContentItems
	items := make([]contentItem, n)
	for i := range items {
		items[i] = contentItem{Type: "resource_link", URI: longURI}
	}
	encoded := buildMCPStructured(toolsCallResult{Content: items})
	if len(encoded) == 0 {
		t.Fatal("expected a non-nil marker envelope, got nil")
	}
	if len(encoded) > maxMCPStructuredSize {
		t.Fatalf("marker envelope %d bytes exceeds bound %d", len(encoded), maxMCPStructuredSize)
	}
	var env mcpStructuredResult
	if err := json.Unmarshal(encoded, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.NonText) != 1 || env.NonText[0].Kind != "unsupported" || !strings.Contains(env.NonText[0].Name, "envelope dropped") {
		t.Errorf("expected a single 'envelope dropped' marker, got %+v", env.NonText)
	}
}

// TestMCP_BinaryBlobResourceNotInlined pins that an embedded resource
// carrying a binary blob (not text) is flagged Truncated with no inlined
// body — the bridge never forwards untrusted binary bytes, only the URI
// handle.
func TestMCP_BinaryBlobResourceNotInlined(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"ok"},{"type":"resource","resource":{"uri":"file:///img.png","mimeType":"image/png","blob":"BASE64DATA=="}}]}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "blobres", raw), "files", "blobres")

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
	if d.Kind != "resource" {
		t.Errorf("kind = %q, want resource", d.Kind)
	}
	if !d.Truncated {
		t.Errorf("binary blob resource must be flagged Truncated, got %+v", d)
	}
	if d.Text != "" {
		t.Errorf("binary blob must not be inlined as text, got %q", d.Text)
	}
	if d.URI != "file:///img.png" {
		t.Errorf("URI = %q, want file:///img.png (the durable handle)", d.URI)
	}
}

// TestSanitizeMCPToolName_RuneSafe pins that truncation is rune-safe: a name
// of 65 three-byte runes (195 bytes) truncates to exactly maxMCPToolNameLen
// runes and stays valid UTF-8 — a byte-slice cut would split a codepoint and
// emit an invalid string into the tool.name metric attribute.
func TestSanitizeMCPToolName_RuneSafe(t *testing.T) {
	// "世" is a 3-byte UTF-8 rune.
	long := strings.Repeat("世", maxMCPToolNameLen+20)
	got := sanitizeMCPToolName(long)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitised name is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != maxMCPToolNameLen {
		t.Errorf("rune count = %d, want %d", n, maxMCPToolNameLen)
	}

	// A short name is returned unchanged.
	short := "echo"
	if sanitizeMCPToolName(short) != short {
		t.Errorf("short name was altered: %q", sanitizeMCPToolName(short))
	}

	// An ASCII name exactly at the cap is unchanged.
	atCap := strings.Repeat("a", maxMCPToolNameLen)
	if got := sanitizeMCPToolName(atCap); got != atCap {
		t.Errorf("at-cap name altered: len %d", len(got))
	}
}
