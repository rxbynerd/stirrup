package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// scriptedStreamExecutor writes fixed stream content and returns result and
// err for every streamed command.
type scriptedStreamExecutor struct {
	mockExecutor
	stdout, stderr string
	result         *executor.ExecResult
	err            error
}

func (e *scriptedStreamExecutor) ExecStream(_ context.Context, _ string, _ time.Duration, stdout, stderr io.Writer) (*executor.ExecResult, error) {
	_, _ = io.WriteString(stdout, e.stdout)
	_, _ = io.WriteString(stderr, e.stderr)
	return e.result, e.err
}

func newTestCaptureStore(t *testing.T, cfg types.CommandOutputConfig) *commandoutput.Store {
	t.Helper()
	must := require.New(t)
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	must.NoError(err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func runCaptured(t *testing.T, exec executor.Executor, cfg types.CommandOutputConfig, maxResultBytes int, ctx context.Context, input string) (tool.StructuredResult, commandResult) {
	t.Helper()
	must := require.New(t)
	store := newTestCaptureStore(t, cfg)
	ctx = tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "cmd"})
	got, err := RunCommandToolWithStore(exec, store, cfg, maxResultBytes).StructuredHandler(ctx, json.RawMessage(input))
	must.NoError(err)
	var structured commandResult
	must.NoError(json.Unmarshal(got.Structured, &structured))
	return got, structured
}

func TestRunCommand_CaptureExecutorErrorReportsNoExitCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *executor.ExecResult
	}{
		{name: "zero result", result: &executor.ExecResult{}},
		{name: "nil result", result: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			exec := &scriptedStreamExecutor{result: tc.result, err: errors.New("exec: container not found")}

			got, structured := runCaptured(t, exec, (types.ToolsConfig{}).EffectiveCommandOutput(), types.DefaultToolMaxResultBytes, context.Background(), `{"command":"true"}`)

			must.True(got.IsError, "an executor failure is a tool error")
			check.Nil(structured.ExitCode, "the command has no exit status")
			check.True(structured.ExecutorError, "the payload must say the executor failed")
			check.Contains(string(got.Structured), `"exit_code":null`)
			check.NotContains(got.Text, "[exit code:", "the text must not render an exit code")
			check.Contains(got.Text, "[executor error: exec: container not found]")
		})
	}
}

func TestRunCommand_CaptureCleanExitReportsExitCode(t *testing.T) {
	must := require.New(t)
	exec := &scriptedStreamExecutor{stdout: "ok", result: &executor.ExecResult{ExitCode: 0}}

	got, structured := runCaptured(t, exec, (types.ToolsConfig{}).EffectiveCommandOutput(), types.DefaultToolMaxResultBytes, context.Background(), `{"command":"true"}`)

	must.False(got.IsError)
	must.Equal(new(0), structured.ExitCode, "a clean exit reports exit code 0")
	must.False(structured.ExecutorError)
	must.Contains(string(got.Structured), `"exit_code":0`)
}

func TestRunCommand_CaptureCancellationAddsMarker(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(300*time.Millisecond, cancel)
	defer timer.Stop()

	got, structured := runCaptured(t, exec, (types.ToolsConfig{}).EffectiveCommandOutput(), types.DefaultToolMaxResultBytes, ctx, `{"command":"printf partial; sleep 30","timeout":60}`)

	must.True(got.IsError, "a cancelled command is a tool error")
	check.Contains(got.Text, "partial", "partial output must be kept")
	check.True(strings.HasSuffix(got.Text, cancelledMarker), "text: %q", got.Text)
	check.True(structured.Cancelled)
	check.False(structured.ExecutorError, "a cancellation is not an executor failure")
	check.Nil(structured.ExitCode, "a cancelled command has no exit status")
}

func TestRunCommandTool_UncapturedCancellationAddsMarker(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exec := &mockExecutor{
		execFunc: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{Stdout: "partial", ExitCode: -1}, fmt.Errorf("command cancelled: %w", context.Canceled)
		},
	}

	got, err := RunCommandTool(exec).StructuredHandler(ctx, json.RawMessage(`{"command":"sleep 30"}`))
	must.NoError(err)

	must.True(got.IsError, "a cancelled command is a tool error")
	check.Equal("partial\n"+cancelledMarker, got.Text, "the -1 the executor reports must not render as an exit code")
	var structured commandResult
	must.NoError(json.Unmarshal(got.Structured, &structured))
	check.True(structured.Cancelled)
	check.Nil(structured.ExitCode)
}

func TestRunCommandTool_UncapturedTimeoutReportsNoExitCode(t *testing.T) {
	must := require.New(t)
	exec := &mockExecutor{
		execFunc: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{Stdout: "partial", ExitCode: -1}, fmt.Errorf("%w after 5s", executor.ErrTimeout)
		},
	}

	got, err := RunCommandTool(exec).StructuredHandler(context.Background(), json.RawMessage(`{"command":"sleep 30","timeout":5}`))
	must.NoError(err)

	must.False(got.IsError, "a timeout is not a tool error")
	must.Equal("partial\n[timed out after 5s]", got.Text, "the -1 the executor reports must not render as an exit code")
}

func TestRunCommand_CaptureInlineOutputSpillsWhenOverMaxResultBytes(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const maxResultBytes = 2048
	cfg := (types.ToolsConfig{}).EffectiveCommandOutput()
	exec := &scriptedStreamExecutor{stdout: strings.Repeat("o", 8<<10) + "TAIL", result: &executor.ExecResult{ExitCode: 3}}

	got, structured := runCaptured(t, exec, cfg, maxResultBytes, context.Background(), `{"command":"build"}`)

	must.Less(int64(8<<10), cfg.InlineMaxBytes, "the output must be within inlineMaxBytes for this test")
	check.LessOrEqual(len(got.Text), maxResultBytes, "text must fit tools.maxResultBytes")
	check.True(structured.Spilled, "output that does not fit must spill")
	check.Contains(got.Text, "exit_code=3 ")
	check.Contains(got.Text, "TAIL", "the stdout tail must survive the shrink")
	check.True(strings.HasSuffix(structured.Stdout, "TAIL"))
	check.Contains(got.Text, "stdout_tail:\n"+structured.Stdout+"\n", "the payload preview must match the text preview")
}

func TestRunCommand_CaptureSpilledPreviewsShrinkToMaxResultBytes(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const maxResultBytes = 4096
	cfg := types.CommandOutputConfig{InlineMaxBytes: 16, PreviewBytesPerStream: 64 << 10, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	exec := &scriptedStreamExecutor{
		stdout: strings.Repeat("o", 100<<10) + "OUT-TAIL",
		stderr: strings.Repeat("e", 100<<10) + "ERR-TAIL",
		result: &executor.ExecResult{},
		err:    errors.New("exec: stream reset " + strings.Repeat("x", 16<<10)),
	}

	got, structured := runCaptured(t, exec, cfg, maxResultBytes, context.Background(), `{"command":"build"}`)

	must.True(got.IsError)
	check.LessOrEqual(len(got.Text), maxResultBytes, "text must fit tools.maxResultBytes")
	check.Contains(got.Text, "exit_code=none ", "a failed executor reports no exit code")
	check.Contains(got.Text, "OUT-TAIL\n")
	check.Contains(got.Text, "ERR-TAIL\n")
	check.Contains(got.Text, "[executor error: exec: stream reset ")
	check.Contains(got.Text, "stdout_tail:\n"+structured.Stdout+"\n", "the payload preview must match the text preview")
	check.Contains(got.Text, "stderr_tail:\n"+structured.Stderr+"\n", "the payload preview must match the text preview")
	check.Less(len(structured.Stdout), 4096, "previews must shrink below previewBytesPerStream")
}

func TestStreamTail_StartsOnRuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		n       int64
		want    string
	}{
		{name: "cut inside a character", content: "a€b", n: 3, want: "b"},
		{name: "content starts mid-character", content: "€b"[1:], n: 64, want: "b"},
		{name: "whole content", content: "a€b", n: 64, want: "a€b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, streamTail(tc.content, tc.n))
		})
	}
}

func TestRunCommand_CaptureExecutorErrorAtMinimumBudgetKeepsReferencesAndLabel(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const maxResultBytes = 1024
	cfg := types.CommandOutputConfig{InlineMaxBytes: 16, PreviewBytesPerStream: 64 << 10, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	exec := &scriptedStreamExecutor{
		stdout: strings.Repeat("o", 64<<10),
		stderr: strings.Repeat("e", 64<<10),
		result: &executor.ExecResult{},
		err:    errors.New("exec: stream reset " + strings.Repeat("x", 4<<10)),
	}

	got, _ := runCaptured(t, exec, cfg, maxResultBytes, context.Background(), `{"command":"build"}`)

	must.True(got.IsError)
	check.LessOrEqual(len(got.Text), maxResultBytes, "text must fit tools.maxResultBytes")
	check.Contains(got.Text, "stdout_ref=", "the model must be able to page stdout")
	check.Contains(got.Text, "stderr_ref=", "the model must be able to page stderr")
	check.Contains(got.Text, "[executor error: exec: stream reset ", "the failure label must survive")
}

type recordingRecorder struct{ records []types.CommandOutputRecord }

func (r *recordingRecorder) RecordCommandOutput(rec types.CommandOutputRecord) {
	r.records = append(r.records, rec)
}

func TestRunCommand_CaptureRecordMatchesStructuredExit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		result       *executor.ExecResult
		err          error
		wantExit     *int
		wantExecutor bool
	}{
		{name: "clean exit", result: &executor.ExecResult{ExitCode: 3}, wantExit: new(3)},
		{name: "executor error", result: &executor.ExecResult{}, err: errors.New("exec: container not found"), wantExecutor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cfg := (types.ToolsConfig{}).EffectiveCommandOutput()
			store := newTestCaptureStore(t, cfg)
			recorder := &recordingRecorder{}
			store.SetRecorder(recorder)
			exec := &scriptedStreamExecutor{result: tc.result, err: tc.err}
			ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", ToolUseID: "cmd"})

			_, err := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes).StructuredHandler(ctx, json.RawMessage(`{"command":"true"}`))

			must.NoError(err)
			must.Len(recorder.records, 1)
			check.Equal(tc.wantExit, recorder.records[0].ExitCode, "the record's exit code must match the structured result")
			check.Equal(tc.wantExecutor, recorder.records[0].ExecutorError)
		})
	}
}
