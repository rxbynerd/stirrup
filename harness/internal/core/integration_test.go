package core

import (
	"bytes"
	"context"
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
	defer loop.Close()

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
	defer loop.Close()

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
		fmt.Fprint(w, responses[idx])
	}))
}

func openAIChunk(payload string) string {
	return "data: " + payload + "\n\n"
}
