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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

const maxMCPResponseSize = 10 * 1024 * 1024 // 10 MB

// --- JSON-RPC 2.0 wire types ---

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int64            `json:"id"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *jsonRPCError    `json:"error,omitempty"`
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
}

type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolsCallResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError"`
}

// --- Client ---

// Client is a remote-only MCP client that connects to MCP servers via
// Streamable HTTP transport. It discovers tools and registers them into
// a tool.Registry so they can be invoked by the agentic loop.
type Client struct {
	httpClient *http.Client
	registry   *tool.Registry
	nextID     atomic.Int64

	mu       sync.Mutex
	sessions map[string]*serverSession // keyed by server URI
}

// serverSession holds per-server connection state.
type serverSession struct {
	uri       string
	token     string // bearer token, empty if no auth
	sessionID string // Mcp-Session-Id from server
}

// NewClient creates an MCP client that registers discovered tools into the
// given registry. It uses the provided http.Client for all requests; if nil,
// a default client with a 30-second timeout is used to prevent unbounded
// connections to remote MCP servers.
func NewClient(registry *tool.Registry, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
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
	if config.Name == "" {
		return fmt.Errorf("mcp: server config missing required Name field")
	}
	if config.URI == "" {
		return fmt.Errorf("mcp: server %q missing required URI field", config.Name)
	}

	// Validate URI scheme to prevent SSRF via file://, gopher://, etc.
	parsed, err := url.Parse(config.URI)
	if err != nil {
		return fmt.Errorf("mcp: invalid URI for server %q: %w", config.Name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("mcp: server %q URI scheme %q not allowed (must be http or https)", config.Name, parsed.Scheme)
	}

	// Resolve bearer token if configured.
	var token string
	if config.APIKeyRef != "" {
		var err error
		token, err = secrets.Resolve(ctx, config.APIKeyRef)
		if err != nil {
			return fmt.Errorf("mcp: resolve auth for server %q: %w", config.Name, err)
		}
	}

	sess := &serverSession{
		uri:   config.URI,
		token: token,
	}

	c.mu.Lock()
	c.sessions[config.URI] = sess
	c.mu.Unlock()

	// Discover tools.
	tools, err := c.listTools(ctx, sess)
	if err != nil {
		return fmt.Errorf("mcp: list tools from server %q: %w", config.Name, err)
	}

	// Register each discovered tool.
	for _, mt := range tools {
		c.registerMCPTool(config.Name, sess, mt)
	}

	return nil
}

// Close releases resources held by the client. Currently a no-op since
// Streamable HTTP is stateless on the client side, but provides a clean
// lifecycle boundary for future session teardown (e.g. DELETE request).
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = make(map[string]*serverSession)
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

// callTool sends a tools/call JSON-RPC request and returns the text result.
func (c *Client) callTool(ctx context.Context, sess *serverSession, name string, arguments json.RawMessage) (string, error) {
	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{
		Name:      name,
		Arguments: arguments,
	}

	raw, err := c.call(ctx, sess, "tools/call", params)
	if err != nil {
		return "", err
	}

	var result toolsCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode tools/call result: %w", err)
	}

	if result.IsError {
		// Collect text content items into the error message.
		var texts []string
		for _, item := range result.Content {
			if item.Type == "text" {
				texts = append(texts, item.Text)
			}
		}
		return "", fmt.Errorf("mcp tool error: %s", strings.Join(texts, "; "))
	}

	// Concatenate text content items.
	var texts []string
	for _, item := range result.Content {
		if item.Type == "text" {
			texts = append(texts, item.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
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
	defer resp.Body.Close()

	// Capture session ID from response.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		sess.sessionID = sid
		c.mu.Unlock()
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
func (c *Client) registerMCPTool(serverName string, sess *serverSession, mt mcpTool) {
	prefixedName := fmt.Sprintf("mcp_%s_%s", serverName, mt.Name)
	remoteName := mt.Name
	remoteSess := sess

	c.registry.Register(&tool.Tool{
		Name:        prefixedName,
		Description: mt.Description,
		InputSchema: mt.InputSchema,
		SideEffects: true, // MCP tools default to side-effecting
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			return c.callTool(ctx, remoteSess, remoteName, input)
		},
	})
}
