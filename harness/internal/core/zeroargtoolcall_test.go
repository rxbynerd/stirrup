package core

// End-to-end cover for zero-argument tool calls. A parameterless call
// carries no arguments on the wire, which decodes to a nil map; if the loop
// stores that as JSON null the schema gate rejects the call and the provider
// rejects the whole conversation when the turn is replayed.

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// initGitRepo makes dir a git repository so git_status has something real to
// report. Returns the symlink-resolved path, which is what the executor sees.
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}

	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		c := osexec.Command("git", args...)
		c.Dir = resolved
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return resolved
}

// TestZeroArgumentToolCall_ExecutesAndReplaysAsObject drives a parameterless
// git_status call through validation and back onto the wire. The tool must
// run, and the replayed assistant turn must carry an object rather than null.
func TestZeroArgumentToolCall_ExecutesAndReplaysAsObject(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	workspace := initGitRepo(t, t.TempDir())

	// Turn 1 opens the call with an empty arguments string and never sends
	// an arguments delta — the shape a model emits for a tool that takes no
	// parameters. Turn 2 is the final answer, and its request body carries
	// the replayed turn 1 assistant message.
	var requestBodies []string
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"git_status","arguments":""}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{"content":"the tree is clean"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, &requestBodies)

	timeout := 30
	enforce := false
	config := &types.RunConfig{
		RunID:            "zero-arg-tool-call",
		Mode:             "execution",
		Prompt:           "Report the working tree state.",
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
		Tools:            types.ToolsConfig{BuiltIn: []string{"git_status"}},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         4,
		Timeout:          &timeout,
	}

	events := &recordingTransport{}
	loop, err := BuildLoopWithTransport(context.Background(), config, events)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer server.Close()
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("expected success, got %q", runTrace.Outcome)
	}

	// Half one: the schema gate accepted the call and the handler ran.
	total, failed := countCalls(runTrace, "git_status")
	if total != 1 || failed != 0 {
		t.Fatalf("git_status calls: total=%d failed=%d, want 1/0", total, failed)
	}
	var sawResult bool
	for _, e := range events.events {
		switch e.Type {
		case "tool_call":
			if got := string(types.NormalizeToolInput(e.Input)); got != "{}" {
				t.Errorf("emitted tool_call input = %q, want {}", e.Input)
			}
		case "tool_result":
			sawResult = true
			if strings.Contains(e.Content, "Invalid input") {
				t.Fatalf("git_status was schema-rejected: %s", e.Content)
			}
			if !strings.Contains(e.Content, "working tree clean") {
				t.Errorf("git_status result = %q, want a clean working tree report", e.Content)
			}
		}
	}
	if !sawResult {
		t.Error("no tool_result event emitted")
	}

	// Half two: the replayed assistant turn must not carry a null input.
	// A null there is what draws the fatal 400 on the next request.
	if len(requestBodies) < 2 {
		t.Fatalf("expected 2 provider requests, got %d", len(requestBodies))
	}
	replay := requestBodies[1]
	if !strings.Contains(replay, `"arguments":"{}"`) {
		t.Errorf("replayed turn missing empty-object arguments:\n%s", replay)
	}
	for _, poison := range []string{`"arguments":"null"`, `"arguments":null`, `"input":null`} {
		if strings.Contains(replay, poison) {
			t.Errorf("replayed turn carries %s:\n%s", poison, replay)
		}
	}
}
