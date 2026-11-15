package core

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

// goneSandboxExecutor fails every streamed command the way a pod-exec
// executor does once the sandbox container has been killed: a zero-value
// result, nothing on either stream, and an error.
type goneSandboxExecutor struct {
	executor.Executor
}

func (goneSandboxExecutor) ExecStream(context.Context, string, time.Duration, io.Writer, io.Writer) (*executor.ExecResult, error) {
	return &executor.ExecResult{}, errors.New(`exec: unable to upgrade connection: container not found ("agent")`)
}

func TestLoop_RunCommandExecutorErrorReachesProvider(t *testing.T) {
	must := require.New(t)
	h := newOutputLimitHarness(
		[]types.StreamEvent{
			{Type: "tool_call", ID: "tc_1", Name: "run_command", Input: map[string]any{"command": "echo still here"}},
			stopWith("tool_use"),
		},
		scriptEndTurn,
	)
	cfg := (types.ToolsConfig{}).EffectiveCommandOutput()
	store, err := commandoutput.New(commandoutput.Options{RunID: "test-run-1", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	must.NoError(err)
	t.Cleanup(func() { _ = store.Close() })
	h.loop.Tools.(*tool.Registry).Register(builtins.RunCommandToolWithStore(goneSandboxExecutor{}, store, cfg, types.DefaultToolMaxResultBytes))

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	requests := h.provider.params()
	must.Len(requests, 2)

	var results []types.ContentBlock
	for _, m := range requests[1].Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == "tc_1" {
				results = append(results, b)
			}
		}
	}
	must.Len(results, 1)
	must.True(results[0].IsError)
	must.Equal(`[executor error: exec: unable to upgrade connection: container not found ("agent")]`, results[0].Content)
}
