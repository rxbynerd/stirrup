package fixtureserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func rpc(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	must := require.New(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	must.NoError(err)
	resp, err := (&http.Client{}).Do(req)
	must.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	must.NoError(err)
	if len(raw) == 0 {
		return resp.StatusCode, nil
	}
	var decoded map[string]any
	must.NoError(json.Unmarshal(raw, &decoded))
	return resp.StatusCode, decoded
}

func startMCP(t *testing.T) *Server {
	t.Helper()
	server, err := Start(types.FixtureServerConfig{Mode: types.FixtureModeMCP})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func TestStartMCP_ToolsList_AdvertisesOneHyphenatedTool(t *testing.T) {
	must := require.New(t)
	status, resp := rpc(t, startMCP(t).URL, `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`)
	must.Equal(http.StatusOK, status)
	must.InDelta(7, resp["id"], 0)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	must.Len(tools, 1)
	must.Equal("read-file-stub", tools[0].(map[string]any)["name"])
}

func TestStartMCP_ToolsCall_ReturnsStubText(t *testing.T) {
	must := require.New(t)
	status, resp := rpc(t, startMCP(t).URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read-file-stub","arguments":{}}}`)
	must.Equal(http.StatusOK, status)
	content := resp["result"].(map[string]any)["content"].([]any)
	must.Equal(map[string]any{"type": "text", "text": MCPToolResult}, content[0])
}

func TestStartMCP_Initialize_ReportsToolsCapability(t *testing.T) {
	must := require.New(t)
	_, resp := rpc(t, startMCP(t).URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	result := resp["result"].(map[string]any)
	must.Equal(mcpProtocolVersion, result["protocolVersion"])
	must.Contains(result["capabilities"], "tools")
}

func TestStartMCP_RejectsUnknownToolAndMethod(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode float64
	}{
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other"}}`, jsonRPCInvalidParams},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, jsonRPCMethodNotFound},
		{"malformed body", `not json`, jsonRPCParseError},
	}
	server := startMCP(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, resp := rpc(t, server.URL, tc.body)
			must.NotContains(resp, "result")
			must.InDelta(tc.wantCode, resp["error"].(map[string]any)["code"], 0)
		})
	}
}

func TestStartMCP_Notification_IsAcknowledgedWithoutBody(t *testing.T) {
	must := require.New(t)
	status, resp := rpc(t, startMCP(t).URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	must.Equal(http.StatusAccepted, status)
	must.Nil(resp)
}

func TestStart_MCP_RejectsStreamFile(t *testing.T) {
	must := require.New(t)
	_, err := Start(types.FixtureServerConfig{Mode: types.FixtureModeMCP, StreamFile: "x.sse"})
	must.ErrorContains(err, "takes no stream file")
}
