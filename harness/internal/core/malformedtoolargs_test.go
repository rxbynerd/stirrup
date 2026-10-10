package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	badArgs, err := json.Marshal(`{"path": "main.go", "token": "` + invalidArgsSecret + `",}`)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
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
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()
	var traceBuf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&traceBuf, false)

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", runTrace.Outcome)
	}

	if len(runTrace.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls in the trace, want 2: %+v", len(runTrace.ToolCalls), runTrace.ToolCalls)
	}
	bad, good := runTrace.ToolCalls[0], runTrace.ToolCalls[1]
	if bad.Success || bad.ErrorCategory != string(observability.ToolFailureInvalidArguments) {
		t.Errorf("first call = %+v, want a failed invalid_arguments call", bad)
	}
	if !strings.Contains(bad.ErrorReason, "raw arguments") || !strings.Contains(bad.ErrorReason, `"path": "main.go"`) {
		t.Errorf("ErrorReason = %q, want the scrubbed raw arguments", bad.ErrorReason)
	}
	if !good.Success {
		t.Errorf("second call = %+v, want success", good)
	}

	var badResult string
	for _, e := range events.events {
		if e.Type == "tool_result" && e.ToolUseID == "call_bad" {
			badResult = e.Content
		}
	}
	if !strings.Contains(badResult, "were not valid JSON") || !strings.Contains(badResult, "at byte offset") {
		t.Errorf("tool_result for call_bad = %q, want the parse error", badResult)
	}

	if len(requestBodies) != 3 {
		t.Fatalf("got %d provider requests, want 3", len(requestBodies))
	}
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
	if err := json.Unmarshal([]byte(requestBodies[1]), &replay); err != nil {
		t.Fatalf("decode second request: %v", err)
	}
	paired := false
	for i, m := range replay.Messages {
		if m.Role != "assistant" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_bad" {
			continue
		}
		if got := m.ToolCalls[0].Function.Arguments; got != "{}" {
			t.Errorf("replayed arguments = %q, want {}", got)
		}
		if i+1 < len(replay.Messages) && replay.Messages[i+1].Role == "tool" && replay.Messages[i+1].ToolCallID == "call_bad" {
			paired = true
		}
	}
	if !paired {
		t.Errorf("second request does not replay call_bad followed by its tool result:\n%s", requestBodies[1])
	}

	for i, body := range requestBodies {
		if strings.Contains(body, invalidArgsSecret) {
			t.Errorf("request %d carries the raw secret", i)
		}
	}
	if strings.Contains(traceBuf.String(), invalidArgsSecret) {
		t.Error("JSONL trace carries the raw secret")
	}
	if !strings.Contains(traceBuf.String(), "invalid_arguments") {
		t.Error("JSONL trace does not record the invalid_arguments category")
	}
}
