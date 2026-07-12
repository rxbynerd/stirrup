package core

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

func TestFinalizeCommandOutputFailClosedOutcomes(t *testing.T) {
	t.Run("capture failure", func(t *testing.T) {
		cfg := types.CommandOutputConfig{InlineMaxBytes: 1, PreviewBytesPerStream: 1, MaxBytesPerStream: 1, MaxBytesPerRun: 2}
		store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(commandoutput.WithCallContext(ctx, commandoutput.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = capture.Stdout().Write([]byte("too large"))
		captured, _ := capture.Complete(commandoutput.Completion{Cancelled: true})
		if err := store.RecordInitial(&captured.Record, "failed"); err != nil {
			t.Fatal(err)
		}
		emitter := tracepkg.NewJSONLTraceEmitter(&bytes.Buffer{})
		emitter.Start("run", &types.RunConfig{})
		loop := &AgenticLoop{CommandOutput: store, OwnsCommandOutput: true, Trace: emitter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := loop.finalizeCommandOutput(context.Background(), "success"); got != "command_output_capture_failed" {
			t.Fatalf("outcome=%q", got)
		}
	})

	t.Run("capture failure never masks a primary failure outcome", func(t *testing.T) {
		cfg := types.CommandOutputConfig{InlineMaxBytes: 1, PreviewBytesPerStream: 1, MaxBytesPerStream: 1, MaxBytesPerRun: 2}
		store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(commandoutput.WithCallContext(ctx, commandoutput.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = capture.Stdout().Write([]byte("too large"))
		captured, _ := capture.Complete(commandoutput.Completion{Cancelled: true})
		if err := store.RecordInitial(&captured.Record, "failed"); err != nil {
			t.Fatal(err)
		}
		emitter := tracepkg.NewJSONLTraceEmitter(&bytes.Buffer{})
		emitter.Start("run", &types.RunConfig{})
		loop := &AgenticLoop{CommandOutput: store, OwnsCommandOutput: true, Trace: emitter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := loop.finalizeCommandOutput(context.Background(), "timeout"); got != "timeout" {
			t.Fatalf("outcome=%q, want the primary timeout preserved", got)
		}
	})

	t.Run("capture failure outranks archive failure", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := types.CommandOutputConfig{InlineMaxBytes: 1, PreviewBytesPerStream: 1, MaxBytesPerStream: 1, MaxBytesPerRun: 2}
		store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(parent, "archive.tar.gz")})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(commandoutput.WithCallContext(ctx, commandoutput.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = capture.Stdout().Write([]byte("too large"))
		captured, _ := capture.Complete(commandoutput.Completion{Cancelled: true})
		if err := store.RecordInitial(&captured.Record, "failed"); err != nil {
			t.Fatal(err)
		}
		emitter := tracepkg.NewJSONLTraceEmitter(&bytes.Buffer{})
		emitter.Start("run", &types.RunConfig{})
		loop := &AgenticLoop{CommandOutput: store, OwnsCommandOutput: true, Trace: emitter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := loop.finalizeCommandOutput(context.Background(), "success"); got != "command_output_capture_failed" {
			t.Fatalf("outcome=%q, want capture failure to outrank archive failure", got)
		}
	})

	t.Run("archive failure", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := types.CommandOutputConfig{InlineMaxBytes: 10, PreviewBytesPerStream: 2, MaxBytesPerStream: 10, MaxBytesPerRun: 20}
		store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(parent, "archive.tar.gz")})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(commandoutput.WithCallContext(ctx, commandoutput.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = capture.Stdout().Write([]byte("ok"))
		captured, err := capture.Complete(commandoutput.Completion{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordInitial(&captured.Record, "ok"); err != nil {
			t.Fatal(err)
		}
		emitter := tracepkg.NewJSONLTraceEmitter(&bytes.Buffer{})
		emitter.Start("run", &types.RunConfig{})
		loop := &AgenticLoop{CommandOutput: store, OwnsCommandOutput: true, Trace: emitter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := loop.finalizeCommandOutput(context.Background(), "success"); got != "trace_archive_failed" {
			t.Fatalf("outcome=%q", got)
		}
	})
}
