// Package mcp implements a remote-only MCP (Model Context Protocol) client
// that connects to MCP servers via Streamable HTTP transport (JSON-RPC 2.0
// over HTTP POST). The client discovers tools from remote servers and
// registers them into the harness tool registry.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
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

// maxMCPStructuredSize bounds the byte size of the structured envelope the
// bridge preserves from a single tools/call response (the marshalled
// mcpStructuredResult: structuredContent plus the non-text content
// descriptors). MCP server output is UNTRUSTED — a hostile or misconfigured
// server could return a multi-megabyte structuredContent object (still
// within the 10 MB transport cap) that would then be threaded into every
// subsequent turn's message history AND the persisted trace, multiplying the
// memory and storage cost (CWE-400). When the marshalled envelope exceeds
// this cap the bridge drops the structured payload and keeps only the text
// content, with a marker noting the omission, so the model still receives a
// usable result without the harness retaining an unbounded blob. 256 KB is
// generous for a legitimate structured result while well below the transport
// ceiling.
const maxMCPStructuredSize = 256 * 1024

// maxMCPEmbeddedResourceTextLen bounds the inlined text the bridge preserves
// from a single embedded-resource content item. Embedded resources can carry
// arbitrarily large text/blob bodies; the bridge records a bounded prefix
// plus the resource URI/mime so the model knows the resource exists and can
// re-fetch it, rather than inlining an unbounded (untrusted) body into the
// envelope. The full body is never required for the model to act — the URI
// is the durable handle.
const maxMCPEmbeddedResourceTextLen = 8 * 1024

// maxMCPContentItems caps the number of content items the bridge will
// process from a single tools/call response before building the non-text
// descriptor slice. A hostile 10 MB response can pack ~700K minimal items
// (e.g. {"type":"image"} at ~15 bytes each); iterating them all would grow
// the NonText slice to tens of megabytes (with slice-doubling overhead)
// before the final marshalled-size check fires, a transient spike
// repeatable every tool turn (CWE-789/400). Capping the input count BEFORE
// the loop bounds the allocation up front. 512 is far above any realistic
// tool result while keeping the worst-case descriptor slice small; overflow
// is marked, not silently dropped.
const maxMCPContentItems = 512

// kindMCPToolResult is the ToolResult.Kind discriminator for the structured
// envelope the MCP bridge produces. It is distinct from the built-in kinds
// (command_result, etc.) because an MCP result is a heterogeneous container
// (structuredContent + typed descriptors for non-text content) rather than a
// single built-in shape. Consumers route on this value to know the payload is
// an mcpStructuredResult.
const kindMCPToolResult = "mcp_tool_result"

// maxMCPToolNameLen caps the length of a per-tool name as reported by a
// remote MCP server. mt.Name is taken verbatim from the wire response and
// becomes a metric attribute (`tool.name`) on stirrup.mcp.calls; an
// uncapped value lets a malicious or misconfigured server inject
// arbitrarily long unique attribute values, blowing up cardinality on
// the OTLP exporter (CWE-400). 128 is comfortably above any realistic
// MCP tool name and well below typical metric backend label limits.
const maxMCPToolNameLen = 128

// maxMCPToolsPerServer caps the number of tools a single MCP server may
// register. Combined with maxMCPToolNameLen this bounds the cardinality
// contribution of any one server to (count * length) regardless of how
// the server is misbehaving. Tools beyond the cap are dropped at
// registration time with a structured warning so operators can spot
// misconfigured servers.
const maxMCPToolsPerServer = 64

// maxMCPSessionIDLen caps the length of the Mcp-Session-Id value accepted from
// a server response. The captured value is echoed back verbatim on every
// subsequent request header, so an unbounded server-controlled string would
// let a hostile or buggy server grow the harness's outbound request size
// without limit. A session ID past this cap is rejected (not stored) and the
// prior value is kept. 512 bytes is well above any legitimate opaque token.
const maxMCPSessionIDLen = 512

// sanitizeMCPToolName truncates a remote tool name to maxMCPToolNameLen
// runes. Returns the input unchanged when it is already within the cap.
//
// The cap is on runes, not bytes: a byte-slice truncation (s[:128]) could
// split a multi-byte UTF-8 name mid-codepoint and emit an invalid-UTF-8
// string, which then becomes both a registry map key and the `tool.name`
// OTLP attribute on stirrup.mcp.calls — OpenTelemetry SDKs treat invalid
// attribute values as implementation-defined and may reject the export.
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
	// 2025-06-18). types.ToolAnnotations mirrors the MCP object field-for-
	// field, so it doubles as the wire-parse target; absent annotations leave
	// it nil. The harness surfaces these on the tool's ToolPresentation (#222)
	// but does not yet act on them — the conservative WorkspaceMutating/
	// RequiresApproval defaults below still govern gating regardless of what a
	// remote server claims, so a server cannot relax its own gating by
	// asserting readOnlyHint.
	Annotations *types.ToolAnnotations `json:"annotations,omitempty"`
}

type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

// contentItem is one entry in a tools/call result's content array (MCP spec
// 2025-06-18 §server/tools). The harness reads `text` for the canonical
// fallback and the remaining fields to describe non-text content explicitly
// rather than discarding it. Only the fields the bridge acts on are
// unmarshalled; unknown fields are ignored.
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
// item — an inlined resource the server chose to embed in the result. The
// text/blob body is UNTRUSTED and potentially large; the bridge records a
// bounded prefix of Text and never inlines Blob (binary), keeping the URI as
// the durable handle.
type mcpEmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Blob     string `json:"blob"`
}

// toolsCallResult is the tools/call response. StructuredContent is the
// optional typed object an MCP tool returns when it declares an outputSchema
// (MCP spec 2025-06-18). It is captured as raw JSON and preserved into the
// B1 envelope. Meta is ignored beyond decoding tolerance.
type toolsCallResult struct {
	Content           []contentItem   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// mcpStructuredResult is the typed envelope the bridge marshals into
// ToolResult.Structured for an MCP tools/call result (issue #231 B2). It is a
// concrete Go struct, not a map[string]any (wave-2 design D13): StructuredContent
// carries the server's typed object verbatim (bounded), and NonText explicitly
// describes every content item the harness does not inline as text, so a
// resource link / embedded resource / image is REPRESENTED rather than silently
// dropped. The whole envelope is scrubbed and size-bounded before it reaches a
// trace or the model history (see callToolStructured).
type mcpStructuredResult struct {
	// StructuredContent is the server's structuredContent object, preserved
	// verbatim when present and within the size bound. Omitted when the
	// server returned none.
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`

	// NonText lists a typed descriptor for every non-text content item.
	// Empty (omitted) when the result was text-only.
	NonText []mcpNonTextContent `json:"non_text_content,omitempty"`
}

// mcpNonTextContent is the explicit, marked representation of one non-text
// content item from a tools/call result. The harness does not forward image
// or audio bytes or unbounded embedded-resource bodies; instead it records
// what the item was (Kind), its addressing (URI/MimeType/Name), and — for an
// embedded text resource — a bounded prefix (Text, with Truncated set when
// the body was longer). This is the "represent-or-mark, never silently drop"
// contract from issue #231.
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

	// Metrics is optional. When set, every MCP tool invocation records
	// stirrup.mcp.calls and stirrup.mcp.duration_ms with attributes
	// (server.name, tool.name, success). A nil Metrics is safe at every
	// call site — callers (including the factory) wire this after
	// construction. Field-injected to avoid breaking existing NewClient
	// callers.
	Metrics *observability.Metrics

	// Logger is optional. When set, the client emits structured warnings for
	// operator-visible anomalies — notably a server advertising more tools
	// than maxMCPToolsPerServer (the overflow is dropped). A nil Logger is
	// safe everywhere; the helper guards it. Field-injected for the same
	// backwards-compatibility reason as Metrics.
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

// validateMCPHost enforces the connect-time trust-model gate on a server
// URI: a parseable http/https scheme, a present host, https for any remote
// (non-loopback) target, the shared SSRF guard against private/reserved
// addresses, and the optional AllowedMCPHosts pin. It mirrors the static
// checks in types.ValidateRunConfig but also runs the resolving SSRF guard,
// because a config that validated at parse time can still point at a name
// that resolves to a private address — the same reuse web_fetch relies on.
//
// Loopback http URIs are permitted for local development; the resolving SSRF
// guard is therefore skipped only for a loopback IP literal or a
// localhost name, never for a remote host.
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

// filterAllowedTools applies the per-server AllowedTools allowlist to the set
// of advertised tools. An empty allowlist returns the input unchanged
// (register everything). Otherwise only tools whose server-reported name is
// listed survive; the rest are dropped with a warning so a server advertising
// tools beyond its trust grant is visible to operators.
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
	name      string // operator-supplied logical name, for log attribution
	uri       string
	token     string // bearer token, empty if no auth
	sessionID string // Mcp-Session-Id from server
}

const mcpClientTimeout = 30 * time.Second

// NewClient creates an MCP client that registers discovered tools into the
// given registry. It uses the provided http.Client for all requests; if nil,
// a default client with a 30-second timeout is used to prevent unbounded
// connections to remote MCP servers.
//
// The default client's transport re-validates every dialled host through the
// shared SSRF guard (security.LoopbackAwareDialContext), so a server whose DNS
// record flips to a non-loopback private/reserved address between the
// connect-time check and the actual dial is still refused — DNS-rebinding
// protection consistent with web_fetch. Loopback targets are permitted because
// a remote http:// URI is already rejected upstream (ValidateRunConfig /
// validateMCPHost) and a localhost MCP server is a supported local-dev case.
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

	// Discover tools.
	tools, err := c.listTools(ctx, sess)
	if err != nil {
		return fmt.Errorf("mcp: list tools from server %q: %w", config.Name, err)
	}

	// Apply the per-server tool allowlist BEFORE the cardinality cap so a
	// hostile server cannot fill the cap with disallowed tools and starve
	// the legitimate ones. An empty AllowedTools registers everything
	// (backward-compatible); a non-empty list drops any advertised tool not
	// named, with a warning so operators can spot a server advertising more
	// than it was trusted for.
	tools = c.filterAllowedTools(config, tools)

	// Cap the number of tools we register per server. A misconfigured or
	// hostile MCP server could otherwise expose thousands of unique
	// `tool.name` metric attribute values via stirrup.mcp.calls and
	// permission decisions, causing cardinality explosion in the OTLP
	// exporter (CWE-400). Drop the overflow with a warning so operators
	// can investigate; the first maxMCPToolsPerServer tools are
	// still usable.
	if len(tools) > maxMCPToolsPerServer {
		c.warn("mcp tool count exceeded cap; overflow dropped",
			"server", config.Name,
			"registered", maxMCPToolsPerServer,
			"total", len(tools),
		)
		tools = tools[:maxMCPToolsPerServer]
	}

	// Register each discovered tool.
	for _, mt := range tools {
		c.registerMCPTool(config.Name, sess, mt)
	}

	return nil
}

// Probe performs a read-only reachability and authentication handshake
// against a single MCP server for a dry-run preflight: it validates the
// URI shape, resolves the configured bearer token, and issues the same
// tools/list request Connect uses — but it does NOT register the
// discovered tools into the registry. This keeps the probe a pure
// side-effect-free check that can run before (or instead of) the real
// Connect, and avoids mutating a registry a non-dry-run path may also be
// populating.
//
// A returned error names the server so the preflight step can point the
// operator at the specific misconfigured entry.
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

// newSession runs the shared Connect/Probe prologue: it validates the
// required Name/URI fields and the trust-model host gate (scheme, https,
// SSRF, allowedMCPHosts), resolves the bearer token, and returns a session
// carrying the logical name for log attribution. Factoring it here keeps the
// two entry points from drifting in their gating.
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

	return &serverSession{name: config.Name, uri: config.URI, token: token}, nil
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
// text result plus a structured envelope (issue #231 B2). It records
// stirrup.mcp.calls and stirrup.mcp.duration_ms when Metrics is set. The
// serverName argument is the operator-supplied logical name (used for the
// server.name attribute) — distinct from sess.uri so dashboards group by
// intent rather than transport target.
//
// The returned structured envelope is the marshalled mcpStructuredResult or
// nil for a text-only result. It is NOT scrubbed here: scrubbing is applied at
// the trace-emission boundary, when the trace emitter's RecordTurnRecord runs
// the payload through scrubRawJSON before writing each line (the value flows
// back through tool.StructuredResult → dispatch → RecordTurnRecord). Size
// bounding, however, is enforced here at the trust boundary so an oversized
// untrusted payload is never materialised into the loop in the first place.
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
		// A tool-reported error surfaces as a Go error built from the text
		// content; structured content on an error result is not preserved
		// (the dispatch error path carries no structured payload).
		return "", nil, fmt.Errorf("mcp tool error: %s", strings.Join(textContentItems(result.Content), "; "))
	}

	text := strings.Join(textContentItems(result.Content), "\n")
	structured := buildMCPStructured(result)
	// Defence-in-depth: treat a literal JSON null envelope as absent so a
	// "null" payload can never reach the dispatch path and fabricate a
	// structured turn (the StructuredHandler keys on len(structured) > 0).
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

// buildMCPStructured assembles the bounded mcpStructuredResult envelope from a
// successful tools/call result and returns its marshalled form, or nil when
// there is nothing structured to preserve. It enforces the untrusted-input
// size bounds (CWE-400): structuredContent over maxMCPStructuredSize is
// dropped (with a marker), embedded-resource text is truncated to
// maxMCPEmbeddedResourceTextLen, and image/audio bytes are never inlined.
// Non-text content is always represented (never silently discarded) so the
// model knows the item existed even when the bytes are not forwarded.
func buildMCPStructured(result toolsCallResult) json.RawMessage {
	env := mcpStructuredResult{}

	// Cap the content item count BEFORE the loop so a hostile response cannot
	// force an unbounded descriptor-slice allocation ahead of the final
	// marshalled-size check (CWE-789/400). The overflow is marked, not
	// silently dropped, so an oversized server is visible in the envelope.
	if len(result.Content) > maxMCPContentItems {
		result.Content = result.Content[:maxMCPContentItems]
		env.NonText = append(env.NonText, mcpNonTextContent{
			Kind: "unsupported",
			Name: "content items truncated: exceeds item-count bound",
		})
	}

	// structuredContent: preserve verbatim when present, non-null, and within
	// bound. An explicit JSON null is treated as ABSENT — a non-nil
	// json.RawMessage("null") would otherwise pass the len>0 guard and, since
	// omitempty does not omit a non-nil slice, serialise "structured_content":null
	// onto the wire, fabricating a structured turn for a server that signalled
	// no structured content.
	switch {
	case len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null":
		// Absent: nothing to preserve.
	case len(result.StructuredContent) <= maxMCPStructuredSize:
		env.StructuredContent = result.StructuredContent
	default:
		// Mark the omission so a reader of the envelope (and the model,
		// via the descriptor) can tell the server returned structured
		// content that was too large to retain rather than that there
		// was none.
		env.NonText = append(env.NonText, mcpNonTextContent{
			Kind: "unsupported",
			Name: "structuredContent dropped: exceeds size bound",
		})
	}

	// Non-text content items: represent each explicitly.
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
			// An unrecognised content type is still represented, not dropped,
			// so a future MCP content kind surfaces as a visible marker.
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
		// A marshal failure means the (untrusted) structuredContent was not
		// valid JSON to re-emit; drop the structured envelope entirely rather
		// than risk an invalid payload. The text fallback is unaffected.
		return nil
	}
	// Final defence: if the assembled envelope still exceeds the bound (e.g. a
	// flood of non-text descriptors that individually passed), replace it with
	// a marker envelope rather than silently returning nil — the
	// represent-or-mark contract means a reader can distinguish "no structured
	// content" from "envelope too large". The text content survives either way.
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
// type=="resource" content item. The URI/mime are the durable handle; the
// inlined text body is truncated to maxMCPEmbeddedResourceTextLen and binary
// blobs are never inlined (only noted via Truncated when present).
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
		// Binary body: never inlined. Mark that a blob exists so the model
		// knows to fetch the resource by URI if it needs the bytes.
		d.Truncated = true
	}
	return d
}

// recordCall emits the per-invocation MCP counter and duration histogram.
// A nil Metrics short-circuits — every call site assumes Metrics is
// optional. Note: the issue's attribute set for mcp.calls also includes
// `success`; an mcp tool error returned by the server is treated as a
// failed call here so dashboards can distinguish transport/protocol
// errors from successful responses.
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

	// Send bearer token if configured.
	if sess.token != "" {
		req.Header.Set("Authorization", "Bearer "+sess.token)
	}

	// Send session ID if we have one from a prior response.
	c.mu.Lock()
	sessionID := sess.sessionID
	c.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to %s: %w", sess.uri, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Capture session ID from response. Reject an over-cap value rather than
	// store it: the ID is echoed back on every later request, so an unbounded
	// server-controlled string would inflate outbound headers indefinitely.
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
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, sess.uri, string(respBody))
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
// and registers it in the registry. Tool names are prefixed with the server
// name to avoid collisions: "mcp_{serverName}_{toolName}".
//
// mt.Name is sanitised via sanitizeMCPToolName so a remote server that
// returns an absurdly long name cannot inject high-cardinality strings
// into the metric attribute stream (`tool.name` on stirrup.mcp.calls and
// stirrup.permission.decisions). The sanitised form is used both for
// registration and for outbound metric attribution.
func (c *Client) registerMCPTool(serverName string, sess *serverSession, mt mcpTool) {
	remoteName := sanitizeMCPToolName(mt.Name)
	prefixedName := fmt.Sprintf("mcp_%s_%s", serverName, remoteName)
	remoteSess := sess
	// Capture serverName by value so each handler reports the correct
	// operator-supplied name — independent of any later registration on
	// the same client.
	remoteServerName := serverName

	c.registry.Register(&tool.Tool{
		Name:        prefixedName,
		Description: mt.Description,
		InputSchema: mt.InputSchema,
		// Carry the server-declared annotations through to the tool's
		// ToolPresentation (#222). They are advisory metadata only — the
		// conservative WorkspaceMutating/RequiresApproval defaults below are
		// what govern gating, never these hints.
		Annotations: mt.Annotations,
		// MCP tools are remote and opaque to the harness: we cannot
		// statically tell whether they mutate the workspace, so be
		// conservative and assume they do. They also require upstream
		// approval for the same reason — the operator should be in the
		// loop on remote tool execution.
		WorkspaceMutating: true,
		RequiresApproval:  true,
		// StructuredHandler (not Handler) so the bridge can preserve
		// structuredContent and non-text descriptors into the #231 envelope.
		// The canonical text is always populated as the fallback, so a
		// resolution against a provider with no structured capability still
		// sends exactly the text the plain Handler would have returned. The
		// returned Structured payload is bounded at the trust boundary
		// (callTool) and scrubbed downstream by the dispatch path, never
		// here — MCP output is untrusted and must not bypass the shared
		// scrub on its way to a trace.
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
