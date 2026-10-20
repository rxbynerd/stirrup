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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func TestBuildLoop_UsesSelectedProviderAlias(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)

	must.Equal("success", runTrace.Outcome)
	must.Zero(defaultHits.Load(), "default provider should not receive requests")
	must.Equal(int32(1), alternateHits.Load(), "alternate provider should receive one request")
}

func TestBuildLoop_HonorsEditStrategyAndBuiltInSelection(t *testing.T) {
	must := require.New(t)
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "target.txt")
	err := os.WriteFile(targetPath, []byte("hello old world\n"), 0o644)
	must.NoError(err, "write target file")

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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	must.NotNil(loop.Tools.Resolve("search_replace"), "expected search_replace tool to be registered")
	must.Nil(loop.Tools.Resolve("write_file"), "did not expect write_file tool when search-replace strategy is active")
	must.Nil(loop.Tools.Resolve("run_command"), "did not expect run_command tool when it is omitted from tools.builtIn")

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)

	content, err := os.ReadFile(targetPath)
	must.NoError(err, "read target file")
	must.Equal("hello new world\n", string(content))

	must.NotEmpty(requestBodies, "expected at least one provider request")
	must.Contains(requestBodies[0], `"search_replace"`, "expected search_replace tool definition in provider request")
	must.NotContains(requestBodies[0], `"write_file"`, "did not expect write_file tool definition in provider request")
}

func TestBuildToolRegistry_RespectsExecutorCapabilities(t *testing.T) {
	must := require.New(t)
	exec := &registryExecutor{
		caps: executor.ExecutorCapabilities{
			CanRead:  true,
			CanWrite: false,
			CanExec:  false,
		},
	}

	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)

	must.NotNil(registry.Resolve("read_file"), "expected read_file to be registered")
	must.Nil(registry.Resolve("run_command"), "did not expect run_command without exec capability")
	// grep_files's rg fast path gates on CanExec internally and falls back
	// to the native walker, so a CanRead-only executor must still get both
	// grep_files and find_files.
	must.NotNil(registry.Resolve("grep_files"), "expected grep_files to register on a CanRead-only executor")
	must.NotNil(registry.Resolve("find_files"), "expected find_files to register on a CanRead-only executor")
	must.Nil(registry.Resolve("search_files"), "search_files must not be registered (split into grep_files/find_files)")
	must.Nil(registry.Resolve("write_file"), "did not expect write_file without write capability")
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
	// This config is the all-three case the Rule-of-Two validator rejects
	// (default tool list + TEST_*_KEY + allow-all); disable enforcement
	// since this file exercises the factory and loop, not that invariant.
	enforce := false
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
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         2,
		Timeout:          &timeout,
	}
	return config
}

// newOpenAIServer serves one canned SSE response per request, in order. The
// handler runs on the server's goroutine, so it reports failures with assert
// and returns instead of calling require.
func newOpenAIServer(t *testing.T, hits *atomic.Int32, responses []string, requestBodies *[]string) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	var requestIndex atomic.Int32

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if !check.NoError(err, "read request body") {
			return
		}
		if requestBodies != nil {
			*requestBodies = append(*requestBodies, string(bodyBytes))
		}

		idx := int(requestIndex.Add(1) - 1)
		if !check.Less(idx, len(responses), "received unexpected provider request %d", idx) {
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, responses[idx])
	}))
}

func openAIChunk(payload string) string {
	return "data: " + payload + "\n\n"
}

// TestBuildLoop_ResearchModeWebFetchEndToEnd: with deny-side-effects
// active, a research-mode run that calls web_fetch must not record any
// "Permission denied" tool result. The real web_fetch handler is replaced
// with a stub so the test needs no network access; the permission gate
// runs before the handler, so the policy path is still exercised.
func TestBuildLoop_ResearchModeWebFetchEndToEnd(t *testing.T) {
	must := require.New(t)
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
	// Research mode + web_fetch + a TEST_*_KEY APIKeyRef triggers the
	// Rule-of-Two invariant; disable it since this test asserts
	// permission-policy behaviour, not Rule-of-Two.
	enforce := false
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
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         4,
		Timeout:          &timeout,
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	// loop.Tools is the profile Presenter; unwrap to the underlying
	// registry to swap the handler. web_fetch is unaliased under the
	// default profile, so the presented name is unchanged.
	presenter, ok := loop.Tools.(*tool.Presenter)
	must.True(ok, "expected *tool.Presenter, got %T", loop.Tools)
	registry, ok := presenter.Unwrap().(*tool.Registry)
	must.True(ok, "expected *tool.Registry under presenter, got %T", presenter.Unwrap())
	original := registry.Resolve("web_fetch")
	must.NotNil(original, "expected web_fetch tool to be registered in research mode")
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
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)

	var sawWebFetch bool
	var denied []string
	for _, call := range runTrace.ToolCalls {
		if call.Name == "web_fetch" {
			sawWebFetch = true
		}
		if !call.Success && strings.Contains(call.ErrorReason, "Permission denied") {
			denied = append(denied, fmt.Sprintf("%s: %s", call.Name, call.ErrorReason))
		}
	}
	must.Empty(denied, "tool calls must not be denied by the permission policy")
	must.True(sawWebFetch, "expected at least one web_fetch call in run trace")
}

// TestLoop_ReplayProvider_CancelledAfterFirstTurn drives the agentic loop
// with a ReplayProvider (no network) and injects a cancel ControlEvent via
// the transport after the first turn completes. The next turn should be
// aborted and RunTrace.Outcome should be "cancelled".
func TestLoop_ReplayProvider_CancelledAfterFirstTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Two-turn recording: turn 0 calls a read-only tool, turn 1 ends the
	// run; a cancel is injected between them.
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

	// The stock "test_tool" in buildTestLoop returns "tool result". Wrap its
	// handler so the cancel ControlEvent fires once the tool has run, which
	// trips the turn-boundary context check on the next iteration.
	if testTool := loop.Tools.Resolve("test_tool"); testTool != nil {
		prev := testTool.Handler
		testTool.Handler = func(ctx context.Context, input json.RawMessage) (string, error) {
			out, err := prev(ctx, input)
			tr.FireControl(types.ControlEvent{Type: "cancel"})
			return out, err
		}
	}

	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.NotNil(runTrace, "expected non-nil RunTrace")
	check.Equal("cancelled", runTrace.Outcome)

	var doneEvents []types.HarnessEvent
	for _, ev := range tr.Events() {
		if ev.Type == "done" {
			doneEvents = append(doneEvents, ev)
		}
	}
	must.Len(doneEvents, 1, "expected exactly one done event")
	check.Equal("cancelled", doneEvents[0].StopReason)

	check.LessOrEqual(runTrace.Turns, 1, "expected at most one recorded turn")
}
