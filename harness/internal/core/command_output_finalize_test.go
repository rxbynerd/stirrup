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

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
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
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
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
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
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
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
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

	t.Run("bestEffort never claims the outcome", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := types.CommandOutputConfig{FailurePosture: types.CommandOutputPostureBestEffort, InlineMaxBytes: 10, PreviewBytesPerStream: 2, MaxBytesPerStream: 10, MaxBytesPerRun: 20}
		store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(parent, "archive.tar.gz")})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
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
		loop := &AgenticLoop{CommandOutput: store, OwnsCommandOutput: true, CommandOutputBestEffort: true, Trace: emitter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := loop.finalizeCommandOutput(context.Background(), "success"); got != "success" {
			t.Fatalf("outcome=%q, want archive failure ignored under bestEffort", got)
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
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
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
		if got := loop.finalizeCommandOutput(context.Background(), "success"); got != "command_output_archive_failed" {
			t.Fatalf("outcome=%q", got)
		}
	})
}

// TestFinishWithOutcomeFinalizesCommandOutput covers the early-exit path
// (setup_failed and friends): finalization must run, and a poisoned store
// must not mask the primary failure outcome.
func TestFinishWithOutcomeFinalizesCommandOutput(t *testing.T) {
	cfg := types.CommandOutputConfig{InlineMaxBytes: 1, PreviewBytesPerStream: 1, MaxBytesPerStream: 1, MaxBytesPerRun: 2}
	archive := filepath.Join(t.TempDir(), "archive.tar.gz")
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: archive})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run", ToolUseID: "tool"}), cancel)
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
	loop := &AgenticLoop{
		CommandOutput:     store,
		OwnsCommandOutput: true,
		Trace:             emitter,
		Transport:         transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	runTrace, _ := loop.finishWithOutcome(context.Background(), "setup_failed", errors.New("preRun hook failed"))
	if runTrace == nil || runTrace.Outcome != "setup_failed" {
		t.Fatalf("trace=%+v, want the primary setup_failed outcome preserved", runTrace)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("early-exit path must still write the failure archive: %v", err)
	}
}

// TestBuildCommandOutputStore_ArchivePathDerivation pins the documented
// defaults: a JSONL trace derives an adjacent archive, an explicit local
// archive config wins, and other emitters fall back to a temp location.
func TestBuildCommandOutputStore_ArchivePathDerivation(t *testing.T) {
	dir := t.TempDir()

	jsonlCfg := &types.RunConfig{RunID: "derive", TraceEmitter: types.TraceEmitterConfig{Type: "jsonl", FilePath: filepath.Join(dir, "run.jsonl")}}
	store, err := buildCommandOutputStore(context.Background(), jsonlCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if got, want := store.Archive(), filepath.Join(dir, "run.jsonl.command-output.tar.gz"); got != want {
		t.Fatalf("jsonl-adjacent archive: got %q want %q", got, want)
	}

	explicit := &types.RunConfig{RunID: "derive-explicit", TraceEmitter: types.TraceEmitterConfig{
		Type: "jsonl", FilePath: filepath.Join(dir, "run.jsonl"),
		Archive: &types.TraceArchiveConfig{Type: "local", FilePath: filepath.Join(dir, "custom.tar.gz")},
	}}
	store2, err := buildCommandOutputStore(context.Background(), explicit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store2.Close() }()
	if got, want := store2.Archive(), filepath.Join(dir, "custom.tar.gz"); got != want {
		t.Fatalf("explicit local archive: got %q want %q", got, want)
	}

	otel := &types.RunConfig{RunID: "derive-otel", TraceEmitter: types.TraceEmitterConfig{Type: "otel"}}
	store3, err := buildCommandOutputStore(context.Background(), otel)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store3.Close() }()
	if got := store3.Archive(); got == "" || filepath.Dir(got) == dir {
		t.Fatalf("otel emitter should fall back to a temp archive, got %q", got)
	}
}
