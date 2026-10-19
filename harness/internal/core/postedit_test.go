package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// TestAttachPostEditChecks_EveryEditToolRunsChecks pins that each
// workspace-writing edit tool name the strategies register runs the
// configured checks after a successful edit, and not after a failed one.
func TestAttachPostEditChecks_EveryEditToolRunsChecks(t *testing.T) {
	cases := []struct {
		name     string
		strategy edit.EditStrategy
		input    string
		bad      string
	}{
		{"write_file", edit.NewWholeFileStrategy(), `{"path":"f.go","content":"package p\n"}`, `{"path":"","content":"x"}`},
		{"search_replace", edit.NewSearchReplaceStrategy(), `{"path":"f.go","old_string":"package p","new_string":"package q"}`, `{"path":"f.go","old_string":"absent","new_string":"x"}`},
		{"apply_diff", edit.NewUdiffStrategy(0), `{"path":"f.go","diff":"--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-package p\n+package q\n"}`, `{"path":"f.go","diff":"not a diff"}`},
		{"edit_file", edit.NewMultiStrategy(0), `{"path":"f.go","operation":"replace","old_string":"package p","new_string":"package q"}`, `{"path":"f.go","operation":"replace","old_string":"absent","new_string":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(workspace, "f.go"), []byte("package p\n"), 0o644))
			exec, err := executor.NewLocalExecutor(workspace)
			require.NoError(t, err)

			registry := buildToolRegistry(exec, tc.strategy, types.ToolsConfig{}, nil)
			checker := attachPostEditChecks(registry, tc.strategy.ToolDefinition().Name, exec, []types.PostEditCheckConfig{
				{Glob: "*.go", Command: "echo checked {path} >> checks.log; echo diagnostic-line; exit 3"},
			}, slog.New(slog.DiscardHandler))
			require.NotNil(t, checker, "attachPostEditChecks should return a checker")

			editTool := registry.Resolve(tc.name)
			require.NotNil(t, editTool, "edit tool should be registered")

			out, err := editTool.Handler(context.Background(), json.RawMessage(tc.input))
			require.NoError(t, err)
			require.Contains(t, out, "exited with code 3", "tool result lacks check diagnostics:\n%s", out)
			require.Contains(t, out, "diagnostic-line", "tool result lacks check diagnostics:\n%s", out)

			_, err = editTool.Handler(context.Background(), json.RawMessage(tc.bad))
			require.Error(t, err)

			log, err := os.ReadFile(filepath.Join(workspace, "checks.log"))
			require.NoError(t, err)
			require.Equal(t, "checked f.go\n", string(log), "checks should run once, for the successful edit only")
		})
	}
}

func TestAttachPostEditChecks_NoChecksLeavesHandlerUntouched(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	es := edit.NewWholeFileStrategy()
	registry := buildToolRegistry(exec, es, types.ToolsConfig{}, nil)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	checker := attachPostEditChecks(registry, es.ToolDefinition().Name, exec, nil, logger)
	require.Nil(t, checker, "no checker should be attached without checks")

	out, err := registry.Resolve("write_file").Handler(context.Background(), json.RawMessage(`{"path":"a.go","content":"x"}`))
	require.NoError(t, err)
	require.Equal(t, "Successfully edited a.go", out)

	noEdit := buildToolRegistry(exec, es, types.ToolsConfig{BuiltIn: []string{"read_file"}}, nil)
	assert.Empty(t, logs.String(), "unconfigured checks should not log")

	checker = attachPostEditChecks(noEdit, es.ToolDefinition().Name, exec, []types.PostEditCheckConfig{{Glob: "*", Command: "true"}}, logger)
	require.Nil(t, checker, "no checker should be attached when no edit tool is registered")
	require.Contains(t, logs.String(), "no edit tool is registered", "missing warning for inert tools.postEdit; logs: %s", logs.String())
}

// TestBuildLoop_PostEditDiagnosticsReachModelAndTrace drives a full run:
// the model writes a file, the failing check's fenced diagnostics are
// sent back to the model in the next request, and the run trace records
// the execution.
func TestBuildLoop_PostEditDiagnosticsReachModelAndTrace(t *testing.T) {
	workspace := t.TempDir()
	var requestBodies []string
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"pe-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"main.go\",\"content\":\"package main\\n\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"pe-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"pe-2","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"pe-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, &requestBodies)
	defer server.Close()

	config := buildOpenAIConfig(t, server.URL)
	config.Executor.Workspace = workspace
	config.MaxTurns = 4
	config.Tools = types.ToolsConfig{
		BuiltIn:  []string{"write_file"},
		PostEdit: []types.PostEditCheckConfig{{Glob: "*.go", Command: "echo undefined: frobnicate in {path} AKIAABCDEFGHIJKLMNOP; exit 1"}},
	}

	t.Setenv("TEST_OPENAI_KEY", "test-key")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.FileExists(t, filepath.Join(workspace, "main.go"), "edit should be applied")

	require.Len(t, requestBodies, 2)
	followUp := requestBodies[1]
	for _, tc := range []struct {
		name string
		want string
	}{
		{"includes check output", "undefined: frobnicate in main.go"},
		{"fences check output as diagnostics", "POST_EDIT_DIAGNOSTICS_"},
		{"includes check exit code", "exited with code 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, followUp, tc.want)
		})
	}
	require.NotContains(t, followUp, "AKIAABCDEFGHIJKLMNOP", "unscrubbed secret reached the model")

	require.Len(t, runTrace.PostEditChecks, 1)
	rec := runTrace.PostEditChecks[0]
	require.Equal(t, "write_file", rec.Tool)
	require.Equal(t, "main.go", rec.Path)
	require.Equal(t, 1, rec.ExitCode)
	require.Equal(t, "call_1", rec.ToolUseID)
}
