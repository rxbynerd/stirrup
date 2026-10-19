package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// TestLoop_MalformedToolArgumentsRecoverEndToEnd drives an
// openai-compatible stream with a trailing-comma argument string through
// the real adapter and loop: the model receives the parse error, its next
// call succeeds, and the replayed history and the trace never carry the
// raw arguments unscrubbed.
func TestLoop_MalformedToolArgumentsRecoverEndToEnd(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	workspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err, "eval symlinks")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o600), "write main.go")

	badArgs, err := json.Marshal(`{"path": "main.go", "token": "` + invalidArgsSecret + `",}`)
	require.NoError(t, err, "marshal arguments")
	var requestBodies []string
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"name":"read_file","arguments":`+string(badArgs)+`}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ok","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"t3","choices":[{"index":0,"delta":{"content":"main.go declares package main"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, &requestBodies)
	defer server.Close()

	timeout := 30
	enforce := false
	config := &types.RunConfig{
		RunID:            "malformed-tool-args",
		Mode:             "execution",
		Prompt:           "Read main.go.",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "search-replace"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         5,
		Timeout:          &timeout,
	}

	events := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), config, events)
	require.NoError(t, err, "BuildLoopWithTransport")
	defer func() { _ = loop.Close() }()
	var traceBuf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&traceBuf, false)

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err, "loop.Run")
	require.Equal(t, "success", runTrace.Outcome)

	require.Len(t, runTrace.ToolCalls, 2, "tool calls in the trace: %+v", runTrace.ToolCalls)
	bad, good := runTrace.ToolCalls[0], runTrace.ToolCalls[1]
	assert.False(t, bad.Success, "first call = %+v", bad)
	assert.Equal(t, string(observability.ToolFailureInvalidArguments), bad.ErrorCategory, "first call = %+v", bad)
	assert.Contains(t, bad.ErrorReason, "raw arguments")
	assert.Contains(t, bad.ErrorReason, `"path": "main.go"`)
	assert.True(t, good.Success, "second call = %+v", good)

	var badResult string
	for _, e := range events.events {
		if e.Type == "tool_result" && e.ToolUseID == "call_bad" {
			badResult = e.Content
		}
	}
	assert.Contains(t, badResult, "were not valid JSON")
	assert.Contains(t, badResult, "at byte offset")

	require.Len(t, requestBodies, 3, "provider requests")
	var replay struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(requestBodies[1]), &replay), "decode second request")
	paired := false
	for i, m := range replay.Messages {
		if m.Role != "assistant" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_bad" {
			continue
		}
		assert.Equal(t, "{}", m.ToolCalls[0].Function.Arguments, "replayed arguments of call_bad")
		if i+1 < len(replay.Messages) && replay.Messages[i+1].Role == "tool" && replay.Messages[i+1].ToolCallID == "call_bad" {
			paired = true
		}
	}
	assert.True(t, paired, "second request does not replay call_bad followed by its tool result:\n%s", requestBodies[1])

	for i, body := range requestBodies {
		assert.NotContains(t, body, invalidArgsSecret, "request %d carries the raw secret", i)
	}
	assert.NotContains(t, traceBuf.String(), invalidArgsSecret, "JSONL trace carries the raw secret")
	assert.Contains(t, traceBuf.String(), "invalid_arguments", "JSONL trace records the invalid_arguments category")
}
