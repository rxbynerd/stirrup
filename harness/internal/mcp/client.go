// Package mcp implements a remote-only MCP (Model Context Protocol) client
// that connects to MCP servers via Streamable HTTP transport (JSON-RPC 2.0
// over HTTP POST). The client discovers tools from remote servers and
// registers them into the harness tool registry.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

const maxMCPResponseSize = 10 * 1024 * 1024 // 10 MB

// maxMCPStructuredSize bounds the marshalled mcpStructuredResult envelope
// preserved from a tools/call response; an oversized envelope is dropped
// with a marker rather than retained. See docs/security.md#mcp-result-bounding.
const maxMCPStructuredSize = 256 * 1024

// maxMCPEmbeddedResourceTextLen bounds the inlined text preserved from a
// single embedded-resource content item; the URI remains the durable handle
// for the rest.
const maxMCPEmbeddedResourceTextLen = 8 * 1024

// maxMCPContentItems caps content items processed from one tools/call
// response before the descriptor slice is built, bounding worst-case
// allocation ahead of the size check; overflow is marked, not dropped.
const maxMCPContentItems = 512

// kindMCPToolResult is the ToolResult.Kind for the MCP structured envelope,
// distinct from the built-in kinds since an MCP result mixes structured
// content with typed non-text descriptors.
const kindMCPToolResult = "mcp_tool_result"

// maxMCPToolNameLen caps a remote tool name; it becomes a metric attribute
// value, so an unbounded server-supplied name would blow up cardinality.
const maxMCPToolNameLen = 128

// maxMCPToolsPerServer caps tools registered per server; combined with
// maxMCPToolNameLen this bounds one server's metric-cardinality contribution.
const maxMCPToolsPerServer = 64

// maxMCPSessionIDLen caps the accepted Mcp-Session-Id; it is echoed on every
// later request, so an unbounded value would grow outbound header size
// without limit.
const maxMCPSessionIDLen = 512

// sanitizeMCPToolName truncates a remote tool name to maxMCPToolNameLen
// runes (not bytes, to avoid splitting a multi-byte name mid-codepoint,
// which would produce an invalid OTLP attribute value).
func sanitizeMCPToolName(s string) string {
	runes := []rune(s)
	if len(runes) > maxMCPToolNameLen {
		return string(runes[:maxMCPToolNameLen])
	}
	return s
}

// --- JSON-RPC 2.0 wire types ---

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// --- MCP protocol response types ---

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Annotations carries the server-declared MCP tool annotations (spec
	// 2025-06-18), advisory only — gating always uses the conservative
	// WorkspaceMutating/RequiresApproval defaults below regardless of what a
	// server claims.
	Annotations *types.ToolAnnotations `json:"annotations,omitempty"`
}

type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

// contentItem is one entry in a tools/call result's content array (MCP spec
// 2025-06-18 §server/tools). Only fields the bridge acts on are unmarshalled.
//
//	type=="text"          → Text
//	type=="image"|"audio" → MimeType (the bytes are not inlined)
//	type=="resource_link" → URI / Name / MimeType
//	type=="resource"      → Resource (an embedded resource body)
type contentItem struct {
	Type     string               `json:"type"`
	Text     string               `json:"text"`
	MimeType string               `json:"mimeType"`
	URI      string               `json:"uri"`
	Name     string               `json:"name"`
	Resource *mcpEmbeddedResource `json:"resource"`
}

// mcpEmbeddedResource is the `resource` field of a type=="resource" content
// item. Text/blob bodies are untrusted and potentially large; the bridge
// bounds Text and never inlines Blob, keeping the URI as the durable handle.
type mcpEmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Blob     string `json:"blob"`
}

// toolsCallResult is the tools/call response. StructuredContent is the
// optional typed object a tool returns when it declares an outputSchema
// (MCP spec 2025-06-18), captured as raw JSON.
type toolsCallResult struct {
	Content           []contentItem   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// mcpStructuredResult is the envelope the bridge marshals into
// ToolResult.Structured for an MCP tools/call result: StructuredContent
// carries the server's typed object verbatim (bounded), and NonText
// represents every non-text content item explicitly rather than dropping it.
// Scrubbed and size-bounded before reaching a trace or model history.
type mcpStructuredResult struct {
	// StructuredContent is the server's structuredContent object, preserved
	// verbatim when present and within the size bound. Omitted when the
	// server returned none.
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`

	// NonText lists a typed descriptor for every non-text content item.
	// Empty (omitted) when the result was text-only.
	NonText []mcpNonTextContent `json:"non_text_content,omitempty"`
}

// mcpNonTextContent represents one non-text content item from a tools/call
// result. Image/audio bytes and unbounded resource bodies are never
// forwarded; instead it records Kind, addressing (URI/MimeType/Name), and
// for an embedded text resource a bounded prefix (Text, Truncated when cut).
type mcpNonTextContent struct {
	// Kind is the MCP content type: "image", "audio", "resource_link",
	// "resource", or "unsupported" for a type the bridge does not recognise.
	Kind      string `json:"kind"`
	URI       string `json:"uri,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	Name      string `json:"name,omitempty"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// --- Client ---

// Client is a remote-only MCP client that connects to MCP servers via
// Streamable HTTP transport. It discovers tools and registers them into
// a tool.Registry so they can be invoked by the agentic loop.
type Client struct {
	httpClient *http.Client
	registry   *tool.Registry
	nextID     atomic.Int64

	// Metrics is optional; when set, records stirrup.mcp.calls and
	// stirrup.mcp.duration_ms per invocation. Nil is safe everywhere.
	Metrics *observability.Metrics

	// Logger is optional; when set, emits structured warnings for
	// operator-visible anomalies (e.g. tool-count overflow). Nil is safe.
	Logger *slog.Logger

	mu       sync.Mutex
	sessions map[string]*serverSession // keyed by server URI
}

// warn emits a structured warning when a Logger is wired, and is a no-op
// otherwise. Centralised so call sites do not repeat the nil guard.
func (c *Client) warn(msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.Warn(msg, args...)
	}
}

// validateMCPHost enforces the connect-time trust-model gate: scheme,
// present host, https for non-loopback targets, the shared SSRF guard, and
// the optional AllowedMCPHosts pin. See docs/security.md#mcp-server-trust-model.
func validateMCPHost(config types.MCPServerConfig) error {
	parsed, err := url.Parse(config.URI)
	if err != nil {
		return fmt.Errorf("mcp: invalid URI for server %q: %w", config.Name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("mcp: server %q URI scheme %q not allowed (must be http or https)", config.Name, parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("mcp: server %q URI must include a host", config.Name)
	}

	local := security.IsLoopbackHost(host)
	if parsed.Scheme == "http" && !local {
		return fmt.Errorf("mcp: server %q must use https for remote host %q (http is only permitted for localhost/loopback)", config.Name, host)
	}
	if !local {
		if err := security.ValidatePublicHost(host); err != nil {
			return fmt.Errorf("mcp: server %q: %w", config.Name, err)
		}
	}

	if len(config.AllowedMCPHosts) > 0 {
		want := strings.ToLower(host)
		permitted := false
		for _, h := range config.AllowedMCPHosts {
			if strings.ToLower(strings.TrimSpace(h)) == want {
				permitted = true
				break
			}
		}
		if !permitted {
			return fmt.Errorf("mcp: server %q host %q is not in allowedMCPHosts", config.Name, host)
		}
	}
	return nil
}

// filterAllowedTools applies the per-server AllowedTools allowlist. An empty
// allowlist registers everything; otherwise only listed tool names survive,
// with the rest dropped and a warning logged.
func (c *Client) filterAllowedTools(config types.MCPServerConfig, tools []mcpTool) []mcpTool {
	if len(config.AllowedTools) == 0 {
		return tools
	}
	allowed := make(map[string]bool, len(config.AllowedTools))
	for _, name := range config.AllowedTools {
		allowed[name] = true
	}
	var kept []mcpTool
	for _, mt := range tools {
		if allowed[mt.Name] {
			kept = append(kept, mt)
			continue
		}
		c.warn("mcp tool not in allowlist; rejected at registration",
			"server", config.Name,
			"tool", sanitizeMCPToolName(mt.Name),
		)
	}
	return kept
}

// serverSession holds per-server connection state.
type serverSession struct {
	name string // operator-supplied logical name, for log attribution
	uri  string
	// displayURI is uri with userinfo/query stripped; logs and errors must
	// use it, never uri, since an operator may embed credentials in the URI.
	displayURI string
	token      string // bearer token, empty if no auth
	sessionID  string // Mcp-Session-Id from server
}

const mcpClientTimeout = 30 * time.Second

// NewClient creates an MCP client that registers discovered tools into the
// given registry. If httpClient is nil, a default client with a 30-second
// timeout and DNS-rebinding-safe dialing is used (see
// docs/security.md#ssrf-protection-web_fetch-and-mcp).
func NewClient(registry *tool.Registry, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: mcpClientTimeout,
			Transport: &http.Transport{
				DialContext:           security.LoopbackAwareDialContext(mcpClientTimeout),
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	return &Client{
		httpClient: httpClient,
		registry:   registry,
		sessions:   make(map[string]*serverSession),
	}
}

// Connect connects to a single MCP server: resolves auth credentials,
// discovers tools via tools/list, and registers each tool into the registry.
// The server name from config is used to prefix tool names.
func (c *Client) Connect(ctx context.Context, config types.MCPServerConfig, secrets security.SecretStore) error {
	sess, err := newSession(ctx, config, secrets)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.sessions[config.URI] = sess
	c.mu.Unlock()

	tools, err := c.listTools(ctx, sess)
	if err != nil {
		return fmt.Errorf("mcp: list tools from server %q: %w", config.Name, err)
	}

	// Allowlist filtering happens before the count cap so a hostile server
	// cannot fill the cap with disallowed tools and starve legitimate ones.
	tools = c.filterAllowedTools(config, tools)

	// Caps per-server tool.name cardinality on metric attributes (CWE-400);
	// overflow is dropped with a warning.
	if len(tools) > maxMCPToolsPerServer {
		c.warn("mcp tool count exceeded cap; overflow dropped",
			"server", config.Name,
			"registered", maxMCPToolsPerServer,
			"total", len(tools),
		)
		tools = tools[:maxMCPToolsPerServer]
	}

	for _, mt := range tools {
		c.registerMCPTool(config.Name, sess, mt)
	}

	return nil
}

// Probe performs a read-only reachability and auth handshake against a
// single MCP server for a dry-run preflight: the same tools/list request as
// Connect, but it does NOT register the discovered tools into the registry.
func (c *Client) Probe(ctx context.Context, config types.MCPServerConfig, secrets security.SecretStore) error {
	sess, err := newSession(ctx, config, secrets)
	if err != nil {
		return err
	}
	if _, err := c.listTools(ctx, sess); err != nil {
		return fmt.Errorf("mcp: list tools from server %q: %w", config.Name, err)
	}
	return nil
}

// newSession runs the shared Connect/Probe prologue: validates Name/URI and
// the trust-model host gate, resolves the bearer token, and returns a
// session carrying the logical name for log attribution.
func newSession(ctx context.Context, config types.MCPServerConfig, secrets security.SecretStore) (*serverSession, error) {
	if config.Name == "" {
		return nil, fmt.Errorf("mcp: server config missing required Name field")
	}
	if config.URI == "" {
		return nil, fmt.Errorf("mcp: server %q missing required URI field", config.Name)
	}
	if err := validateMCPHost(config); err != nil {
		return nil, err
	}

	var token string
	if config.APIKeyRef != "" {
		var err error
		token, err = secrets.Resolve(ctx, config.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("mcp: resolve auth for server %q: %w", config.Name, err)
		}
	}

	return &serverSession{
		name:       config.Name,
		uri:        config.URI,
		displayURI: displaySafeURI(config.URI),
		token:      token,
	}, nil
}

// displaySafeURI strips userinfo and the query string from a URI so it can be
// logged or returned in an error without leaking embedded credentials
// (https://user:pass@host or ?api_key=...). On a parse failure it returns a
// fixed sentinel rather than echoing the unparseable input.
func displaySafeURI(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable mcp uri>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// Close releases resources held by the client. Currently a no-op since
// Streamable HTTP is stateless on the client side, but provides a clean
// lifecycle boundary for future session teardown (e.g. DELETE request).
// Returns error to satisfy io.Closer.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = make(map[string]*serverSession)
	return nil
}

// listTools sends a tools/list JSON-RPC request and returns the discovered tools.
func (c *Client) listTools(ctx context.Context, sess *serverSession) ([]mcpTool, error) {
	raw, err := c.call(ctx, sess, "tools/list", struct{}{})
	if err != nil {
		return nil, err
	}

	var result toolsListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode tools/list result: %w", err)
	}

	return result.Tools, nil
}

// callTool sends a tools/call JSON-RPC request and returns the canonical
// text result plus a structured envelope (the marshalled mcpStructuredResult,
// or nil for text-only). It records stirrup.mcp.calls and
// stirrup.mcp.duration_ms when Metrics is set; serverName (not sess.uri) is
// the metric attribute, so dashboards group by operator intent.
//
// The envelope is NOT scrubbed here — that happens at the trace-emission
// boundary (RecordTurnRecord via scrubRawJSON). Size bounding is enforced
// here, at the trust boundary, so an oversized untrusted payload never
// reaches the loop.
func (c *Client) callTool(ctx context.Context, sess *serverSession, serverName, name string, arguments json.RawMessage) (string, json.RawMessage, error) {
	start := time.Now()
	text, structured, err := c.callToolInner(ctx, sess, name, arguments)
	c.recordCall(ctx, serverName, name, err == nil, time.Since(start))
	return text, structured, err
}

// callToolInner is the tools/call body, factored out so callTool's metric
// recording wraps both happy and error paths uniformly. It returns the
// canonical concatenated text, the bounded structured envelope (nil when the
// result is text-only), and an error for transport/protocol/tool failures.
func (c *Client) callToolInner(ctx context.Context, sess *serverSession, name string, arguments json.RawMessage) (string, json.RawMessage, error) {
	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{
		Name:      name,
		Arguments: arguments,
	}

	raw, err := c.call(ctx, sess, "tools/call", params)
	if err != nil {
		return "", nil, err
	}

	var result toolsCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", nil, fmt.Errorf("decode tools/call result: %w", err)
	}

	if result.IsError {
		// Structured content is not preserved on a tool-reported error.
		return "", nil, fmt.Errorf("mcp tool error: %s", strings.Join(textContentItems(result.Content), "; "))
	}

	text := strings.Join(textContentItems(result.Content), "\n")
	structured := buildMCPStructured(result)
	// Treat literal JSON null as absent so it never reaches dispatch as a
	// fabricated structured turn (StructuredHandler keys on len>0).
	if string(structured) == "null" {
		structured = nil
	}
	return text, structured, nil
}

// textContentItems collects the Text of every type=="text" content item, in
// order. This is the canonical text fallback the model always receives,
// independent of structured support.
func textContentItems(items []contentItem) []string {
	var texts []string
	for _, item := range items {
		if item.Type == "text" {
			texts = append(texts, item.Text)
		}
	}
	return texts
}

// buildMCPStructured assembles the bounded mcpStructuredResult envelope from
// a successful tools/call result, or nil when there is nothing structured to
// preserve. Non-text content is always represented, never silently dropped.
func buildMCPStructured(result toolsCallResult) json.RawMessage {
	env := mcpStructuredResult{}

	// Capped before the loop to bound descriptor-slice allocation up front
	// (CWE-789/400); overflow is marked, not silently dropped.
	if len(result.Content) > maxMCPContentItems {
		result.Content = result.Content[:maxMCPContentItems]
		env.NonText = append(env.NonText, mcpNonTextContent{
			Kind: "unsupported",
			Name: "content items truncated: exceeds item-count bound",
		})
	}

	// A literal JSON null is treated as absent: omitempty does not omit a
	// non-nil json.RawMessage, so an unguarded "null" would otherwise
	// serialise onto the wire as a fabricated structured turn.
	switch {
	case len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null":
	case len(result.StructuredContent) <= maxMCPStructuredSize:
		env.StructuredContent = result.StructuredContent
	default:
		// Mark, don't silently drop, so the envelope distinguishes oversized
		// from absent.
		env.NonText = append(env.NonText, mcpNonTextContent{
			Kind: "unsupported",
			Name: "structuredContent dropped: exceeds size bound",
		})
	}

	for _, item := range result.Content {
		switch item.Type {
		case "text":
			// Canonical fallback; handled by textContentItems.
		case "image", "audio":
			env.NonText = append(env.NonText, mcpNonTextContent{
				Kind:     item.Type,
				MimeType: item.MimeType,
			})
		case "resource_link":
			env.NonText = append(env.NonText, mcpNonTextContent{
				Kind:     "resource_link",
				URI:      item.URI,
				MimeType: item.MimeType,
				Name:     item.Name,
			})
		case "resource":
			env.NonText = append(env.NonText, embeddedResourceDescriptor(item.Resource))
		default:
			// Unrecognised type still represented, not dropped.
			env.NonText = append(env.NonText, mcpNonTextContent{
				Kind: "unsupported",
				Name: item.Type,
			})
		}
	}

	if len(env.StructuredContent) == 0 && len(env.NonText) == 0 {
		return nil
	}

	encoded, err := json.Marshal(env)
	if err != nil {
		// Untrusted structuredContent was not valid JSON to re-emit; drop the
		// envelope, text fallback unaffected.
		return nil
	}
	// Final bound check (e.g. many small descriptors adding up): replace with
	// a marker envelope rather than nil, so "too large" stays distinguishable
	// from "no structured content".
	if len(encoded) > maxMCPStructuredSize {
		fallback := mcpStructuredResult{
			NonText: []mcpNonTextContent{{
				Kind: "unsupported",
				Name: "envelope dropped: assembled size exceeds bound",
			}},
		}
		if fb, err := json.Marshal(fallback); err == nil && len(fb) <= maxMCPStructuredSize {
			return fb
		}
		return nil
	}
	return encoded
}

// embeddedResourceDescriptor builds the bounded descriptor for a
// type=="resource" content item: text truncated to
// maxMCPEmbeddedResourceTextLen, binary blobs never inlined.
func embeddedResourceDescriptor(res *mcpEmbeddedResource) mcpNonTextContent {
	d := mcpNonTextContent{Kind: "resource"}
	if res == nil {
		return d
	}
	d.URI = res.URI
	d.MimeType = res.MimeType
	switch {
	case res.Text != "":
		if len(res.Text) > maxMCPEmbeddedResourceTextLen {
			d.Text = res.Text[:maxMCPEmbeddedResourceTextLen]
			d.Truncated = true
		} else {
			d.Text = res.Text
		}
	case res.Blob != "":
		// Never inlined; Truncated signals a blob exists to fetch by URI.
		d.Truncated = true
	}
	return d
}

// recordCall emits the per-invocation MCP counter and duration histogram; a
// nil Metrics short-circuits. A tool-reported error counts as a failed call
// so dashboards distinguish it from transport/protocol errors.
func (c *Client) recordCall(ctx context.Context, serverName, toolName string, success bool, elapsed time.Duration) {
	if c.Metrics == nil {
		return
	}
	c.Metrics.MCPCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String("server.name", serverName),
		attribute.String("tool.name", toolName),
		attribute.Bool("success", success),
	))
	c.Metrics.MCPDuration.Record(ctx, float64(elapsed.Milliseconds()), metric.WithAttributes(
		attribute.String("server.name", serverName),
	))
}

// call performs a single JSON-RPC 2.0 request over HTTP POST. It handles
// session ID tracking and bearer auth.
func (c *Client) call(ctx context.Context, sess *serverSession, method string, params interface{}) (json.RawMessage, error) {
	id := c.nextID.Add(1)

	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sess.uri, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if sess.token != "" {
		req.Header.Set("Authorization", "Bearer "+sess.token)
	}

	c.mu.Lock()
	sessionID := sess.sessionID
	c.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// *url.Error embeds the full request URL (Go redacts only userinfo,
		// not query strings) in its message; unwrap to the transport cause so
		// the error doesn't leak a credentialed URI (CWE-532).
		cause := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			cause = urlErr.Err
		}
		return nil, fmt.Errorf("HTTP request to %s: %w", sess.displayURI, cause)
	}
	defer func() { _ = resp.Body.Close() }()

	// An over-cap session ID is rejected, not stored: it is echoed on every
	// later request, so an unbounded value would inflate outbound headers.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		if len(sid) > maxMCPSessionIDLen {
			c.warn("mcp session ID exceeded cap; ignoring",
				"server", sess.name, "len", len(sid), "cap", maxMCPSessionIDLen)
		} else {
			c.mu.Lock()
			sess.sessionID = sid
			c.mu.Unlock()
		}
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, sess.displayURI, string(respBody))
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode JSON-RPC response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}

	return rpcResp.Result, nil
}

// registerMCPTool creates a Tool backed by a remote MCP tools/call invocation
// and registers it in the registry as "mcp_{serverName}_{toolName}".
// mt.Name is sanitised so a server cannot inject high-cardinality tool.name
// metric values.
func (c *Client) registerMCPTool(serverName string, sess *serverSession, mt mcpTool) {
	remoteName := sanitizeMCPToolName(mt.Name)
	prefixedName := fmt.Sprintf("mcp_%s_%s", serverName, remoteName)
	remoteSess := sess
	// Captured by value so the handler closure is independent of later
	// registrations on the same client.
	remoteServerName := serverName

	c.registry.Register(&tool.Tool{
		Name:        prefixedName,
		Description: mt.Description,
		InputSchema: mt.InputSchema,
		// Advisory only; gating always uses the conservative defaults below.
		Annotations: mt.Annotations,
		// MCP tools are opaque to the harness; assume mutation and require
		// approval.
		WorkspaceMutating: true,
		RequiresApproval:  true,
		// StructuredHandler (not Handler) so structuredContent/non-text
		// descriptors survive; bounded in callTool and scrubbed downstream,
		// never here.
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			text, structured, err := c.callTool(ctx, remoteSess, remoteServerName, remoteName, input)
			if err != nil {
				return tool.StructuredResult{}, err
			}
			res := tool.StructuredResult{Text: text}
			if len(structured) > 0 {
				res.Structured = structured
				res.Kind = kindMCPToolResult
			}
			return res, nil
		},
	})
}
