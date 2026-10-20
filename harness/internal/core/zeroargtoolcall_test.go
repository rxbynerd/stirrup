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
	must := require.New(t)

	resolved, err := filepath.EvalSymlinks(dir)
	must.NoError(err, "resolving symlinks for %s", dir)

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
		must.NoError(err, "git %v\n%s", args, out)
	}
	return resolved
}

// TestZeroArgumentToolCall_ExecutesAndReplaysAsObject drives a parameterless
// git_status call through validation and back onto the wire. The tool must
// run, and the replayed assistant turn must carry an object rather than null.
func TestZeroArgumentToolCall_ExecutesAndReplaysAsObject(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer server.Close()
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)

	t.Run("git_status runs without schema rejection", func(t *testing.T) {
		must := require.New(t)
		total, failed := countCalls(runTrace, "git_status")
		must.Equal(1, total, "git_status calls")
		must.Zero(failed, "failed git_status calls")

		var sawResult bool
		for _, e := range events.events {
			switch e.Type {
			case "tool_call":
				must.Equal("{}", string(types.NormalizeToolInput(e.Input)),
					"emitted tool_call input %q", e.Input)
			case "tool_result":
				sawResult = true
				must.NotContains(e.Content, "Invalid input",
					"git_status was schema-rejected: %s", e.Content)
				must.Contains(e.Content, "working tree clean")
			}
		}
		must.True(sawResult, "no tool_result event emitted")
	})

	must.GreaterOrEqual(len(requestBodies), 2, "provider requests")
	replay := requestBodies[1]

	t.Run("replayed turn carries empty object arguments", func(t *testing.T) {
		must := require.New(t)
		must.Contains(replay, `"arguments":"{}"`, "replayed turn:\n%s", replay)
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
			must := require.New(t)
			must.NotContains(replay, tc.token, "replayed turn:\n%s", replay)
		})
	}
}
