package fixtureserver

import (
	"encoding/json"
	"io"
	"net/http"
)

const (
	// MCPToolName is the one tool the MCP stub advertises. The hyphens make
	// the harness's registry name (mcp_<server>_read-file-stub) one that
	// some providers reject, so a run exercises provider-facing name
	// normalization.
	MCPToolName = "read-file-stub"

	// MCPToolResult is the text the stub returns from every tools/call.
	MCPToolResult = "stub file contents"

	mcpProtocolVersion = "2025-06-18"

	jsonRPCParseError     = -32700
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
)

type mcpRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// newMCPHandler serves the minimal Streamable-HTTP MCP surface: initialize,
// tools/list and tools/call. The harness client currently sends only
// tools/list and tools/call. A notification, which has no id, is
// acknowledged with 202.
func newMCPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			http.Error(w, "read request body", http.StatusBadRequest)
			return
		}
		var req mcpRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeMCP(w, mcpResponse{Error: &mcpError{Code: jsonRPCParseError, Message: "parse error"}})
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result, rpcErr := serveMCP(req)
		writeMCP(w, mcpResponse{ID: req.ID, Result: result, Error: rpcErr})
	})
	return mux
}

func serveMCP(req mcpRequest) (any, *mcpError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "eval-fixture-mcp", "version": "0"},
		}, nil
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        MCPToolName,
			"description": "Returns fixed stub text for the eval fixture MCP server.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}, nil
	case "tools/call":
		var params struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.Name != MCPToolName {
			return nil, &mcpError{Code: jsonRPCInvalidParams, Message: "unknown tool"}
		}
		return map[string]any{"content": []any{map[string]string{"type": "text", "text": MCPToolResult}}}, nil
	default:
		return nil, &mcpError{Code: jsonRPCMethodNotFound, Message: "method not found"}
	}
}

func writeMCP(w http.ResponseWriter, resp mcpResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
