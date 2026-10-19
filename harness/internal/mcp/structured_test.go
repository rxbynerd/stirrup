package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

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
	err := client.Connect(context.Background(), types.MCPServerConfig{Name: serverName, URI: srv.URL}, secrets)
	require.NoError(t, err)
	resolved := registry.Resolve("mcp_" + serverName + "_" + toolName)
	require.NotNil(t, resolved, "tool mcp_%s_%s not registered", serverName, toolName)
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
	require.NoError(t, err)
	require.Equal(t, "42 rows", res.Text, "text fallback")
	require.Equal(t, kindMCPToolResult, res.Kind)

	var env mcpStructuredResult
	require.NoError(t, json.Unmarshal(res.Structured, &env), "decode envelope")
	var sc map[string]any
	require.NoError(t, json.Unmarshal(env.StructuredContent, &sc), "decode structuredContent")
	require.InDelta(t, 42.0, sc["rows"], 0)
	require.Equal(t, "users", sc["table"])
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
	require.NoError(t, err)
	require.Equal(t, "see attached", res.Text, "text fallback")

	var env mcpStructuredResult
	require.NoError(t, json.Unmarshal(res.Structured, &env), "decode envelope")
	require.Len(t, env.NonText, 5, "expected descriptors for link, resource, image, audio and unsupported: %+v", env.NonText)

	byKind := map[string]mcpNonTextContent{}
	for _, n := range env.NonText {
		byKind[n.Kind] = n
	}
	t.Run("resource link", func(t *testing.T) {
		rl, ok := byKind["resource_link"]
		require.True(t, ok, "descriptor missing")
		require.Equal(t, "file:///report.pdf", rl.URI)
		require.Equal(t, "report", rl.Name)
	})
	t.Run("embedded resource", func(t *testing.T) {
		rs, ok := byKind["resource"]
		require.True(t, ok, "descriptor missing")
		require.Equal(t, "file:///note.txt", rs.URI)
		require.Equal(t, "inline note", rs.Text)
	})
	t.Run("image", func(t *testing.T) {
		img, ok := byKind["image"]
		require.True(t, ok, "descriptor missing")
		require.Equal(t, "image/png", img.MimeType)
	})
	t.Run("audio", func(t *testing.T) {
		// Audio shares the image arm, so its discriminator must still be 'audio'.
		au, ok := byKind["audio"]
		require.True(t, ok, "descriptor missing")
		require.Equal(t, "audio/mpeg", au.MimeType)
	})
	t.Run("unsupported content type", func(t *testing.T) {
		un, ok := byKind["unsupported"]
		require.True(t, ok, "a new content kind must be marked, not dropped")
		require.Equal(t, "future_kind", un.Name)
	})
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
	require.NoError(t, err)
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "big", string(rawResult)), "hostile", "big")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, "ok", res.Text, "text fallback")
	require.LessOrEqual(t, len(res.Structured), maxMCPStructuredSize, "oversized content not dropped")

	var env mcpStructuredResult
	require.NoError(t, json.Unmarshal(res.Structured, &env), "decode envelope")
	require.Empty(t, env.StructuredContent, "oversized structuredContent should have been dropped")
	require.Len(t, env.NonText, 1, "expected an 'unsupported' marker for the dropped content: %+v", env.NonText)
	require.Equal(t, "unsupported", env.NonText[0].Kind)
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
	require.NoError(t, err)
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "bigres", string(rawResult)), "hostile", "bigres")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	require.NoError(t, err)

	var env mcpStructuredResult
	require.NoError(t, json.Unmarshal(res.Structured, &env), "decode envelope")
	require.Len(t, env.NonText, 1, "expected 1 resource descriptor: %+v", env.NonText)
	d := env.NonText[0]
	require.Equal(t, "resource", d.Kind)
	require.True(t, d.Truncated, "descriptor %+v should be truncated", d)
	require.Len(t, d.Text, maxMCPEmbeddedResourceTextLen, "inlined text should be truncated to the per-resource bound")
}

// TestMCP_TextOnlyResultHasNoStructured pins the no-regression path: a
// text-only tools/call result produces no structured envelope and no kind.
func TestMCP_TextOnlyResultHasNoStructured(t *testing.T) {
	raw := `{"content": [{"type": "text", "text": "plain"}]}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "echo", raw), "srv", "echo")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, "plain", res.Text)
	require.Empty(t, res.Structured, "text-only result must carry no structured envelope")
	require.Empty(t, res.Kind, "text-only result must carry no kind")
}

// TestMCP_NullStructuredContentIsAbsent pins that an explicit
// "structuredContent": null does NOT fabricate a structured turn.
func TestMCP_NullStructuredContentIsAbsent(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"x"}],"structuredContent":null}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "nullsc", raw), "srv", "nullsc")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, "x", res.Text)
	require.Empty(t, res.Structured, "null structuredContent must produce no envelope")
	require.Empty(t, res.Kind, "null structuredContent must produce no kind")
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
	require.NotEmpty(t, encoded, "expected a non-nil envelope")
	require.LessOrEqual(t, len(encoded), maxMCPStructuredSize)

	var env mcpStructuredResult
	require.NoError(t, json.Unmarshal(encoded, &env), "decode envelope")
	require.Len(t, env.NonText, maxMCPContentItems+1, "expected the item cap plus one truncation marker")

	var marked bool
	for _, n := range env.NonText {
		if n.Kind == "unsupported" && strings.Contains(n.Name, "item-count bound") {
			marked = true
		}
	}
	require.True(t, marked, "expected a truncation marker for the dropped items, got %+v", env.NonText)
}

// TestMCP_OversizedAssembledEnvelopeIsMarked pins that when many
// individually-valid non-text descriptors push the assembled envelope past
// the size bound, the bridge returns a marker envelope rather than silently
// dropping everything.
func TestMCP_OversizedAssembledEnvelopeIsMarked(t *testing.T) {
	longURI := "file:///" + strings.Repeat("a", 2048)
	// 200 links of about 2 KiB each exceed maxMCPStructuredSize and stay under maxMCPContentItems.
	const n = 200
	items := make([]contentItem, n)
	for i := range items {
		items[i] = contentItem{Type: "resource_link", URI: longURI}
	}
	encoded := buildMCPStructured(toolsCallResult{Content: items})
	require.NotEmpty(t, encoded, "expected a non-nil marker envelope")
	require.LessOrEqual(t, len(encoded), maxMCPStructuredSize, "marker envelope")

	var env mcpStructuredResult
	require.NoError(t, json.Unmarshal(encoded, &env), "decode envelope")
	require.Len(t, env.NonText, 1, "expected a single 'envelope dropped' marker, got %+v", env.NonText)
	require.Equal(t, "unsupported", env.NonText[0].Kind)
	require.Contains(t, env.NonText[0].Name, "envelope dropped")
}

// TestMCP_BinaryBlobResourceNotInlined pins that an embedded resource
// carrying a binary blob (not text) is flagged Truncated with no inlined
// body — the bridge never forwards untrusted binary bytes, only the URI
// handle.
func TestMCP_BinaryBlobResourceNotInlined(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"ok"},{"type":"resource","resource":{"uri":"file:///img.png","mimeType":"image/png","blob":"BASE64DATA=="}}]}`
	resolved := connectAndResolve(t, mcpServerReturningCallResult(t, "blobres", raw), "files", "blobres")

	res, err := callMCPStructured(t, resolved, json.RawMessage(`{}`))
	require.NoError(t, err)

	var env mcpStructuredResult
	require.NoError(t, json.Unmarshal(res.Structured, &env), "decode envelope")
	require.Len(t, env.NonText, 1, "expected 1 resource descriptor: %+v", env.NonText)
	d := env.NonText[0]
	require.Equal(t, "resource", d.Kind)
	require.True(t, d.Truncated, "binary blob resource must be flagged Truncated: %+v", d)
	require.Empty(t, d.Text, "binary blob must not be inlined as text")
	require.Equal(t, "file:///img.png", d.URI, "the URI is the durable handle")
}

// TestSanitizeMCPToolName_TruncatesOnRuneBoundary pins that truncation counts
// runes, not bytes: a name of multi-byte runes truncates to exactly
// maxMCPToolNameLen runes and stays valid UTF-8. A byte-slice cut would split
// a codepoint and emit an invalid string into the tool.name metric attribute.
func TestSanitizeMCPToolName_TruncatesOnRuneBoundary(t *testing.T) {
	t.Run("long name truncates to the rune cap", func(t *testing.T) {
		long := strings.Repeat("世", maxMCPToolNameLen+20)
		got := sanitizeMCPToolName(long)
		require.True(t, utf8.ValidString(got), "sanitised name is not valid UTF-8: %q", got)
		require.Equal(t, maxMCPToolNameLen, utf8.RuneCountInString(got))
	})

	t.Run("short name unchanged", func(t *testing.T) {
		require.Equal(t, "echo", sanitizeMCPToolName("echo"))
	})

	t.Run("name at cap unchanged", func(t *testing.T) {
		atCap := strings.Repeat("a", maxMCPToolNameLen)
		require.Equal(t, atCap, sanitizeMCPToolName(atCap))
	})
}
