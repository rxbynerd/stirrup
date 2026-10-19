package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func TestFinalizeCommandOutput_FailClosedOutcomes(t *testing.T) {
	overflowCfg := types.CommandOutputConfig{InlineMaxBytes: 1, PreviewBytesPerStream: 1, MaxBytesPerStream: 1, MaxBytesPerRun: 2}
	roomyCfg := types.CommandOutputConfig{InlineMaxBytes: 10, PreviewBytesPerStream: 2, MaxBytesPerStream: 10, MaxBytesPerRun: 20}

	t.Run("capture failure", func(t *testing.T) {
		store := newCommandOutputStore(t, overflowCfg, filepath.Join(t.TempDir(), "archive.tar.gz"))
		recordOverflowingRun(t, store)
		loop := newFinalizeLoop(store, false)
		require.Equal(t, "command_output_capture_failed", loop.finalizeCommandOutput(context.Background(), "success"))
	})

	t.Run("capture failure never masks a primary failure outcome", func(t *testing.T) {
		store := newCommandOutputStore(t, overflowCfg, filepath.Join(t.TempDir(), "archive.tar.gz"))
		recordOverflowingRun(t, store)
		loop := newFinalizeLoop(store, false)
		require.Equal(t, "timeout", loop.finalizeCommandOutput(context.Background(), "timeout"), "primary timeout outcome should be preserved")
	})

	t.Run("capture failure outranks archive failure", func(t *testing.T) {
		store := newCommandOutputStore(t, overflowCfg, unwritableArchivePath(t))
		recordOverflowingRun(t, store)
		loop := newFinalizeLoop(store, false)
		require.Equal(t, "command_output_capture_failed", loop.finalizeCommandOutput(context.Background(), "success"), "capture failure should outrank archive failure")
	})

	t.Run("best effort posture never claims the outcome", func(t *testing.T) {
		cfg := types.CommandOutputConfig{FailurePosture: types.CommandOutputPostureBestEffort, InlineMaxBytes: 10, PreviewBytesPerStream: 2, MaxBytesPerStream: 10, MaxBytesPerRun: 20}
		store := newCommandOutputStore(t, cfg, unwritableArchivePath(t))
		recordCompletedRun(t, store)
		loop := newFinalizeLoop(store, true)
		require.Equal(t, "success", loop.finalizeCommandOutput(context.Background(), "success"), "archive failure should be ignored under best effort")
	})

	t.Run("archive failure", func(t *testing.T) {
		store := newCommandOutputStore(t, roomyCfg, unwritableArchivePath(t))
		recordCompletedRun(t, store)
		loop := newFinalizeLoop(store, false)
		require.Equal(t, "command_output_archive_failed", loop.finalizeCommandOutput(context.Background(), "success"))
	})
}

// TestFinishWithOutcome_FinalizesCommandOutput covers the early-exit path
// (setup_failed and friends): finalization must run, and a poisoned store
// must not mask the primary failure outcome.
func TestFinishWithOutcome_FinalizesCommandOutput(t *testing.T) {
	cfg := types.CommandOutputConfig{InlineMaxBytes: 1, PreviewBytesPerStream: 1, MaxBytesPerStream: 1, MaxBytesPerRun: 2}
	archive := filepath.Join(t.TempDir(), "archive.tar.gz")
	store := newCommandOutputStore(t, cfg, archive)
	recordOverflowingRun(t, store)

	loop := newFinalizeLoop(store, false)
	loop.Transport = transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	runTrace, _ := loop.finishWithOutcome(context.Background(), &types.RunConfig{}, "setup_failed", errors.New("preRun hook failed"), func() {})
	require.NotNil(t, runTrace)
	require.Equal(t, "setup_failed", runTrace.Outcome, "primary setup_failed outcome should be preserved")

	_, err := os.Stat(archive)
	require.NoError(t, err, "early-exit path must still write the failure archive")
}

// TestBuildCommandOutputStore_ArchivePathDerivation pins the documented
// defaults: a JSONL trace derives an adjacent archive, an explicit local
// archive config wins, and other emitters fall back to a temp location.
func TestBuildCommandOutputStore_ArchivePathDerivation(t *testing.T) {
	dir := t.TempDir()

	t.Run("jsonl trace derives adjacent archive", func(t *testing.T) {
		cfg := &types.RunConfig{RunID: "derive", TraceEmitter: types.TraceEmitterConfig{Type: "jsonl", FilePath: filepath.Join(dir, "run.jsonl")}}
		store, err := buildCommandOutputStore(context.Background(), cfg)
		require.NoError(t, err)
		defer func() { _ = store.Close() }()
		require.Equal(t, filepath.Join(dir, "run.jsonl.command-output.tar.gz"), store.Archive())
	})

	t.Run("explicit local archive wins", func(t *testing.T) {
		cfg := &types.RunConfig{RunID: "derive-explicit", TraceEmitter: types.TraceEmitterConfig{
			Type: "jsonl", FilePath: filepath.Join(dir, "run.jsonl"),
			Archive: &types.TraceArchiveConfig{Type: "local", FilePath: filepath.Join(dir, "custom.tar.gz")},
		}}
		store, err := buildCommandOutputStore(context.Background(), cfg)
		require.NoError(t, err)
		defer func() { _ = store.Close() }()
		require.Equal(t, filepath.Join(dir, "custom.tar.gz"), store.Archive())
	})

	t.Run("other emitters fall back to a temp archive", func(t *testing.T) {
		cfg := &types.RunConfig{RunID: "derive-otel", TraceEmitter: types.TraceEmitterConfig{Type: "otel"}}
		store, err := buildCommandOutputStore(context.Background(), cfg)
		require.NoError(t, err)
		defer func() { _ = store.Close() }()
		got := store.Archive()
		require.NotEmpty(t, got)
		require.NotEqual(t, dir, filepath.Dir(got), "otel emitter should fall back to a temp archive")
	})
}

// newCommandOutputStore returns a store for run "run" that writes its archive to archivePath.
func newCommandOutputStore(t *testing.T, cfg types.CommandOutputConfig, archivePath string) *commandoutput.Store {
	t.Helper()
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: archivePath})
	require.NoError(t, err)
	return store
}

// unwritableArchivePath returns an archive path below a regular file, so the archive cannot be created.
func unwritableArchivePath(t *testing.T) string {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
	return filepath.Join(parent, "archive.tar.gz")
}

// recordOverflowingRun records a cancelled run whose stdout exceeds the per-stream cap.
func recordOverflowingRun(t *testing.T, store *commandoutput.Store) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
	require.NoError(t, err)
	_, _ = capture.Stdout().Write([]byte("too large"))
	captured, _ := capture.Complete(commandoutput.Completion{Cancelled: true})
	require.NoError(t, store.RecordInitial(&captured.Record, "failed"))
}

// recordCompletedRun records a successful run whose output fits inline.
func recordCompletedRun(t *testing.T, store *commandoutput.Store) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
	require.NoError(t, err)
	_, _ = capture.Stdout().Write([]byte("ok"))
	captured, err := capture.Complete(commandoutput.Completion{})
	require.NoError(t, err)
	require.NoError(t, store.RecordInitial(&captured.Record, "ok"))
}

// newFinalizeLoop returns a loop that owns store's command output and discards its trace and logs.
func newFinalizeLoop(store *commandoutput.Store, bestEffort bool) *AgenticLoop {
	emitter := tracepkg.NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	emitter.Start("run", &types.RunConfig{})
	return &AgenticLoop{
		CommandOutput:           store,
		OwnsCommandOutput:       true,
		CommandOutputBestEffort: bestEffort,
		Trace:                   emitter,
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}
