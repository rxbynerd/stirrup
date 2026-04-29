package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func TestBuildLoop_UsesSelectedProviderAlias(t *testing.T) {
	var defaultHits atomic.Int32
	defaultServer := newOpenAIServer(t, &defaultHits, []string{
		openAIChunk(`{"id":"default","choices":[{"index":0,"delta":{"content":"default"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"default","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer defaultServer.Close()

	var alternateHits atomic.Int32
	alternateServer := newOpenAIServer(t, &alternateHits, []string{
		openAIChunk(`{"id":"alternate","choices":[{"index":0,"delta":{"content":"alternate"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"alternate","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer alternateServer.Close()

	config := buildOpenAIConfig(t, defaultServer.URL)
	config.Providers = map[string]types.ProviderConfig{
		"alternate": {
			Type:      "openai-compatible",
			APIKeyRef: "secret://TEST_OPENAI_KEY",
			BaseURL:   alternateServer.URL,
		},
	}
	config.ModelRouter = types.ModelRouterConfig{
		Type:     "static",
		Provider: "alternate",
		Model:    "gpt-4o-mini",
	}

	t.Setenv("TEST_OPENAI_KEY", "test-key")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runTrace.Outcome != "success" {
		t.Fatalf("expected success, got %q", runTrace.Outcome)
	}
	if defaultHits.Load() != 0 {
		t.Fatalf("default provider was called %d times; expected 0", defaultHits.Load())
	}
	if alternateHits.Load() != 1 {
		t.Fatalf("alternate provider was called %d times; expected 1", alternateHits.Load())
	}
}

func TestBuildLoop_HonorsEditStrategyAndBuiltInSelection(t *testing.T) {
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(targetPath, []byte("hello old world\n"), 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}

	var requestBodies []string
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"edit-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search_replace","arguments":"{\"path\":\"target.txt\",\"old_string\":\"old\",\"new_string\":\"new\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"edit-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"edit-2","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"edit-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, &requestBodies)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:    "integration-edit",
		Mode:     "execution",
		Prompt:   "Update the file.",
		Provider: types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: "openai-compatible",
			Model:    "gpt-4o-mini",
		},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "search-replace"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"search_replace"}},
		MaxTurns:         4,
		Timeout:          &timeout,
	}

	t.Setenv("TEST_OPENAI_KEY", "test-key")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	if loop.Tools.Resolve("search_replace") == nil {
		t.Fatal("expected search_replace tool to be registered")
	}
	if loop.Tools.Resolve("write_file") != nil {
		t.Fatal("did not expect write_file tool when search-replace strategy is active")
	}
	if loop.Tools.Resolve("run_command") != nil {
		t.Fatal("did not expect run_command tool when it is omitted from tools.builtIn")
	}

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("expected success, got %q", runTrace.Outcome)
	}

	content, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if string(content) != "hello new world\n" {
		t.Fatalf("unexpected file content: %q", string(content))
	}
	if len(requestBodies) == 0 {
		t.Fatal("expected at least one provider request")
	}
	if !strings.Contains(requestBodies[0], `"search_replace"`) {
		t.Fatalf("expected search_replace tool definition in provider request, got %s", requestBodies[0])
	}
	if strings.Contains(requestBodies[0], `"write_file"`) {
		t.Fatalf("did not expect write_file tool definition in provider request, got %s", requestBodies[0])
	}
}

func TestBuildToolRegistry_RespectsExecutorCapabilities(t *testing.T) {
	exec := &registryExecutor{
		caps: executor.ExecutorCapabilities{
			CanRead:  true,
			CanWrite: false,
			CanExec:  false,
		},
	}

	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{})

	if registry.Resolve("read_file") == nil {
		t.Fatal("expected read_file to be registered")
	}
	if registry.Resolve("run_command") != nil {
		t.Fatal("did not expect run_command without exec capability")
	}
	if registry.Resolve("search_files") != nil {
		t.Fatal("did not expect search_files without exec capability")
	}
	if registry.Resolve("write_file") != nil {
		t.Fatal("did not expect write_file without write capability")
	}
}

type registryExecutor struct {
	caps executor.ExecutorCapabilities
}

func (e *registryExecutor) ReadFile(context.Context, string) (string, error)        { return "", nil }
func (e *registryExecutor) WriteFile(context.Context, string, string) error         { return nil }
func (e *registryExecutor) ListDirectory(context.Context, string) ([]string, error) { return nil, nil }
func (e *registryExecutor) Exec(context.Context, string, time.Duration) (*executor.ExecResult, error) {
	return nil, nil
}
func (e *registryExecutor) ResolvePath(path string) (string, error) { return path, nil }
func (e *registryExecutor) Capabilities() executor.ExecutorCapabilities {
	return e.caps
}

func buildOpenAIConfig(t *testing.T, baseURL string) *types.RunConfig {
	t.Helper()
	timeout := 30
	workspace := t.TempDir()
	config := &types.RunConfig{
		RunID:    "integration-provider",
		Mode:     "execution",
		Prompt:   "Say hello.",
		Provider: types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: baseURL},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: "openai-compatible",
			Model:    "gpt-4o-mini",
		},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
		Timeout:          &timeout,
	}
	return config
}

func newOpenAIServer(t *testing.T, hits *atomic.Int32, responses []string, requestBodies *[]string) *httptest.Server {
	t.Helper()
	var requestIndex atomic.Int32

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if requestBodies != nil {
			*requestBodies = append(*requestBodies, string(bodyBytes))
		}

		idx := int(requestIndex.Add(1) - 1)
		if idx >= len(responses) {
			t.Fatalf("received unexpected provider request %d", idx)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, responses[idx])
	}))
}

func openAIChunk(payload string) string {
	return "data: " + payload + "\n\n"
}

// TestBuildLoop_ResearchModeWebFetchEndToEnd is the WP1 end-to-end
// regression. With deny-side-effects active, a research-mode run that
// asks the model to call web_fetch must not record any
// "Permission denied" tool result. Before the WP1 fix the conflated
// SideEffects flag caused every web_fetch to be denied.
//
// We replace the real web_fetch handler with a stub after the loop is
// built so the test does not need network access; the permission gate
// runs *before* the handler, so this still exercises the policy path.
func TestBuildLoop_ResearchModeWebFetchEndToEnd(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	server := newOpenAIServer(t, nil, []string{
		// Turn 1: model requests web_fetch.
		openAIChunk(`{"id":"rs-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://example.com/\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"rs-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		// Turn 2: model produces final answer.
		openAIChunk(`{"id":"rs-2","choices":[{"index":0,"delta":{"content":"summary"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"rs-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "integration-research-webfetch",
		Mode:             "research",
		Prompt:           "Look up example.com",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
		MaxTurns:         4,
		Timeout:          &timeout,
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	// Replace web_fetch with a stub so the test does not depend on
	// network access. The permission policy runs *before* the handler,
	// so this still exercises the WP1 gating path.
	registry, ok := loop.Tools.(*tool.Registry)
	if !ok {
		t.Fatalf("expected *tool.Registry, got %T", loop.Tools)
	}
	original := registry.Resolve("web_fetch")
	if original == nil {
		t.Fatal("expected web_fetch tool to be registered in research mode")
	}
	registry.Register(&tool.Tool{
		Name:              original.Name,
		Description:       original.Description,
		InputSchema:       original.InputSchema,
		WorkspaceMutating: original.WorkspaceMutating,
		RequiresApproval:  original.RequiresApproval,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "stubbed body", nil
		},
	})

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runTrace.Outcome != "success" {
		t.Fatalf("expected research run to succeed, got outcome %q", runTrace.Outcome)
	}
	// At least one web_fetch call should have been recorded, and none
	// of the recorded calls should report a permission denial.
	var sawWebFetch bool
	for _, call := range runTrace.ToolCalls {
		if call.Name == "web_fetch" {
			sawWebFetch = true
		}
		if !call.Success && strings.Contains(call.ErrorReason, "Permission denied") {
			t.Errorf("tool call %q was denied by permission policy: %s", call.Name, call.ErrorReason)
		}
	}
	if !sawWebFetch {
		t.Fatal("expected at least one web_fetch call in run trace")
	}
}

// TestLoop_ReplayProvider_CancelledAfterFirstTurn drives the agentic loop
// with a ReplayProvider (no network) and injects a cancel ControlEvent via
// the transport after the first turn completes. The next turn should be
// aborted and RunTrace.Outcome should be "cancelled".
func TestLoop_ReplayProvider_CancelledAfterFirstTurn(t *testing.T) {
	// Two-turn recording: turn 0 calls a read-only tool, turn 1 ends the
	// run. We inject a cancel between them.
	turns := []types.TurnRecord{
		{
			Turn: 0,
			ModelOutput: []types.ContentBlock{
				{
					Type:  "tool_use",
					ID:    "tc_1",
					Name:  "test_tool",
					Input: json.RawMessage(`{}`),
				},
			},
		},
		{
			Turn: 1,
			ModelOutput: []types.ContentBlock{
				{Type: "text", Text: "All done."},
			},
		},
	}

	rp := provider.NewReplayProvider(turns)
	tr := &cancellableTransport{}

	loop := buildTestLoop(nil)
	loop.Provider = rp
	loop.Transport = tr

	// The stock "test_tool" in buildTestLoop returns "tool result". Hook the
	// tool handler so that after it runs, we fire the cancel ControlEvent —
	// this causes the outer turn-boundary ctx check to trip on the next
	// iteration.
	originalRegistry := loop.Tools
	_ = originalRegistry
	fireCancel := func() {
		tr.FireControl(types.ControlEvent{Type: "cancel"})
	}
	// Replace the tool handler to fire cancel on completion.
	if tool := loop.Tools.Resolve("test_tool"); tool != nil {
		prev := tool.Handler
		tool.Handler = func(ctx context.Context, input json.RawMessage) (string, error) {
			out, err := prev(ctx, input)
			fireCancel()
			return out, err
		}
	}

	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace")
	}
	if runTrace.Outcome != "cancelled" {
		t.Errorf("expected outcome 'cancelled', got %q", runTrace.Outcome)
	}

	// Sanity: the replay provider should have been consumed at most once.
	// The second turn must not have been executed.
	if runTrace.Turns > 1 {
		t.Errorf("expected at most 1 recorded turn, got %d", runTrace.Turns)
	}
}
