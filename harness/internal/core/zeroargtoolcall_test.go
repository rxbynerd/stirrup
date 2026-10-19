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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// initGitRepo makes dir a git repository so git_status has something real to
// report. Returns the symlink-resolved path, which is what the executor sees.
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err, "resolving symlinks for %s", dir)

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
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v\n%s", args, out)
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
	require.NoError(t, err)
	defer server.Close()
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)

	t.Run("git_status runs without schema rejection", func(t *testing.T) {
		total, failed := countCalls(runTrace, "git_status")
		require.Equal(t, 1, total, "git_status calls")
		require.Zero(t, failed, "failed git_status calls")

		var sawResult bool
		for _, e := range events.events {
			switch e.Type {
			case "tool_call":
				require.Equal(t, "{}", string(types.NormalizeToolInput(e.Input)),
					"emitted tool_call input %q", e.Input)
			case "tool_result":
				sawResult = true
				require.NotContains(t, e.Content, "Invalid input",
					"git_status was schema-rejected: %s", e.Content)
				require.Contains(t, e.Content, "working tree clean")
			}
		}
		require.True(t, sawResult, "no tool_result event emitted")
	})

	require.GreaterOrEqual(t, len(requestBodies), 2, "provider requests")
	replay := requestBodies[1]

	t.Run("replayed turn carries empty object arguments", func(t *testing.T) {
		require.Contains(t, replay, `"arguments":"{}"`, "replayed turn:\n%s", replay)
	})

	// A null input in the replayed turn is what draws the fatal 400 on the next request.
	nullForms := []struct {
		name  string
		token string
	}{
		{name: "no quoted null arguments", token: `"arguments":"null"`},
		{name: "no bare null arguments", token: `"arguments":null`},
		{name: "no null input", token: `"input":null`},
	}
	for _, tc := range nullForms {
		t.Run(tc.name, func(t *testing.T) {
			require.NotContains(t, replay, tc.token, "replayed turn:\n%s", replay)
		})
	}
}
