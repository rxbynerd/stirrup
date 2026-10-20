package core

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// TestBuildLoop_MCPUnavailableWarningIsScrubbed makes an MCP server answer
// 401 with a body echoing a credential; the error the factory logs for the
// skipped server must reach stderr redacted.
func TestBuildLoop_MCPUnavailableWarningIsScrubbed(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	const echoedKey = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider := newOpenAIServer(t, nil, nil, nil)
	defer provider.Close()

	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid credential ` + echoedKey + `"}`))
	}))
	defer mcpSrv.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-mcp-scrub",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: provider.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		RuleOfTwo:        disableRuleOfTwo(),
		Tools:            types.ToolsConfig{MCPServers: []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL, Optional: true}}},
		MaxTurns:         2,
		Timeout:          &timeout,
	}

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	must.NoError(err, "os.Pipe")
	captured := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		captured <- b
	}()
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, buildErr := BuildLoopWithTransport(context.Background(), config, tp)
	if loop != nil {
		_ = loop.Close()
	}
	os.Stderr = origStderr
	_ = w.Close()
	stderr := string(<-captured)

	must.NoError(buildErr, "BuildLoopWithTransport")
	must.Contains(stderr, "optional MCP server degraded", "expected the MCP unavailable warning on stderr")
	check.NotContains(stderr, echoedKey, "credential echoed by the MCP server must not reach stderr")
	check.Contains(stderr, "[REDACTED]", "expected the MCP unavailable warning to be redacted")
}
