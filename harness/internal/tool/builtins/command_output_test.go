package builtins

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunCommandInlineBoundaryAndSpillRead(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := types.CommandOutputConfig{InlineMaxBytes: 5, PreviewBytesPerStream: 3, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	run := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes)
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", Turn: 1, ToolUseID: "inline"})
	inline, err := run.StructuredHandler(ctx, json.RawMessage(`{"command":"printf 12345"}`))
	if err != nil {
		t.Fatal(err)
	}
	if inline.Text != "12345" {
		t.Fatalf("inline=%q", inline.Text)
	}

	ctx = tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", Turn: 2, ToolUseID: "spill"})
	spilled, err := run.StructuredHandler(ctx, json.RawMessage(`{"command":"printf 123456; printf abcdef >&2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spilled.Text, "command output spilled") || !strings.Contains(spilled.Text, "stirrup://command-output/") {
		t.Fatalf("spill=%q", spilled.Text)
	}
	var result commandResult
	if err := json.Unmarshal(spilled.Structured, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Spilled || result.Stdout != "456" || result.Stderr != "def" {
		t.Fatalf("structured=%+v", result)
	}

	reader := ReadCommandOutputTool(store, exec, types.DefaultToolMaxResultBytes)
	readCtx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", Turn: 3, ToolUseID: "reader"})
	input, _ := json.Marshal(map[string]any{"ref": result.StdoutRef, "offset": 1, "limit": 3})
	chunk, err := reader.StructuredHandler(readCtx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chunk.Text, `"content":"234"`) {
		t.Fatalf("chunk=%s", chunk.Text)
	}
	if _, err := store.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Archive()); err != nil {
		t.Fatal(err)
	}
}

func TestRunCommandTimeoutReturnsCapturedErrorResult(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := types.CommandOutputConfig{InlineMaxBytes: 32 << 10, PreviewBytesPerStream: 1024, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "run-timeout", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	run := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes)
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run-timeout", ToolUseID: "timeout"})
	input := json.RawMessage([]byte("{\"command\":\"printf partial; sleep 5\",\"timeout\":1}"))
	result, err := run.StructuredHandler(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Text, "partial") {
		t.Fatalf("result=%+v", result)
	}
	var structured commandResult
	if err := json.Unmarshal(result.Structured, &structured); err != nil {
		t.Fatal(err)
	}
	if !structured.TimedOut || !structured.CaptureComplete {
		t.Fatalf("structured=%+v", structured)
	}
}

func TestCommandOutputReplayUsesRecordedModelVisibleResultsVerbatim(t *testing.T) {
	runInput := json.RawMessage([]byte("{\"command\":\"big-output\",\"timeout\":30}"))
	readInput := json.RawMessage([]byte("{\"ref\":\"stirrup://command-output/a/b/stdout\",\"offset\":0}"))
	replay := executor.NewReplayExecutor(t.TempDir(), []types.TurnRecord{{ToolCalls: []types.ToolCallRecord{
		{Name: "run_command", Input: runInput, Output: "recorded initial preview", Success: true, Kind: kindCommandResult, Structured: json.RawMessage([]byte("{\"spilled\":true}"))},
		{Name: "read_command_output", Input: readInput, Output: "recorded read chunk", Success: true, Kind: kindCommandOutputChunk},
	}}})
	store, err := commandoutput.New(commandoutput.Options{RunID: "replay", Config: types.CommandOutputConfig{}, ArchivePath: filepath.Join(t.TempDir(), "unused.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	runResult, err := RunCommandToolWithStore(replay, store, types.CommandOutputConfig{}, types.DefaultToolMaxResultBytes).StructuredHandler(context.Background(), runInput)
	if err != nil {
		t.Fatal(err)
	}
	if runResult.Text != "recorded initial preview" {
		t.Fatalf("run replay=%q", runResult.Text)
	}
	readResult, err := ReadCommandOutputTool(store, replay, types.DefaultToolMaxResultBytes).StructuredHandler(context.Background(), readInput)
	if err != nil {
		t.Fatal(err)
	}
	if readResult.Text != "recorded read chunk" {
		t.Fatalf("read replay=%q", readResult.Text)
	}
}

// TestRunCommandCaptureTimeoutKeepsSoftOutcomeMarker pins #489's contract
// on the capture path. buildCommandResult carries the "[timed out after
// Ns]" marker on the store-less legacy path, but a real run always goes
// through ExecStream, where the marker has to come from
// formatCapturedCommand instead — and only the spilled rendering states
// timed_out explicitly. Without the inline branch appending it, a model
// reading only Text sees a bare partial transcript indistinguishable from
// a clean exit.
func TestRunCommandCaptureTimeoutKeepsSoftOutcomeMarker(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := types.CommandOutputConfig{InlineMaxBytes: 1 << 20, PreviewBytesPerStream: 1 << 10, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "timeout", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "timeout", Turn: 1, ToolUseID: "slow"})
	got, err := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes).StructuredHandler(ctx, json.RawMessage(`{"command":"printf partial; sleep 30","timeout":1}`))
	if err != nil {
		t.Fatalf("unexpected hard error for a timeout: %v", err)
	}
	if !strings.Contains(got.Text, "partial") {
		t.Errorf("Text = %q, want the partial stdout preserved", got.Text)
	}
	if !strings.Contains(got.Text, "[timed out after 1s]") {
		t.Errorf("Text = %q, want an explicit timed-out marker", got.Text)
	}
	var result commandResult
	if err := json.Unmarshal(got.Structured, &result); err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.TimeoutSeconds != 1 {
		t.Errorf("structured = %+v, want TimedOut with TimeoutSeconds=1", result)
	}
}

// TestRunCommandSpoolsScrubbedBytesForRealCommand drives the production
// path — a real shell command through the local executor into the store —
// and checks the archive on disk, not only the model-visible text.
func TestRunCommandSpoolsScrubbedBytesForRealCommand(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := types.CommandOutputConfig{InlineMaxBytes: 32 << 10, PreviewBytesPerStream: 1024, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	archive := filepath.Join(t.TempDir(), "archive.tar.gz")
	store, err := commandoutput.New(commandoutput.Options{RunID: "scrub", Config: cfg, ArchivePath: archive})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	secret := "gh" + "p_runCommandFixture0123456789"
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "scrub", ToolUseID: "cmd"})
	input, _ := json.Marshal(map[string]any{"command": "printf 'token=" + secret + "\\n'"})
	result, err := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes).StructuredHandler(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, secret) || !strings.Contains(result.Text, "[REDACTED]") {
		t.Fatalf("model-visible result=%q", result.Text)
	}
	if _, err := store.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(store.Archive())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	members := tar.NewReader(gz)
	redacted := false
	for {
		header, err := members.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(members)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("secret reached archive member %s", header.Name)
		}
		redacted = redacted || strings.Contains(string(body), "[REDACTED]")
	}
	if !redacted {
		t.Fatal("no archive member carries the redaction, so the assertion proved nothing")
	}
}

func TestReadCommandOutputFitsWithinMaxResultBytes(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := types.CommandOutputConfig{InlineMaxBytes: 16, PreviewBytesPerStream: 16, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	run := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes)
	if !run.SelfBoundedResult {
		t.Fatal("run_command with a capture store must be self-bounded")
	}
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", ToolUseID: "spill"})
	// '<' JSON-encodes to six bytes, so the marshalled chunk is several
	// times larger than the raw byte range.
	spilled, err := run.StructuredHandler(ctx, json.RawMessage(`{"command":"head -c 131072 /dev/zero | tr '\\0' '<'"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result commandResult
	if err := json.Unmarshal(spilled.Structured, &result); err != nil {
		t.Fatal(err)
	}

	const maxBytes = 2048
	reader := ReadCommandOutputTool(store, exec, maxBytes)
	if !reader.SelfBoundedResult {
		t.Fatal("read_command_output must be self-bounded")
	}
	offset := int64(0)
	for reads := 0; ; reads++ {
		if reads > 2000 {
			t.Fatal("pagination did not reach EOF")
		}
		readCtx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", ToolUseID: "reader"})
		input, _ := json.Marshal(map[string]any{"ref": result.StdoutRef, "offset": offset, "limit": commandoutput.ReadMaxBytes})
		chunk, err := reader.StructuredHandler(readCtx, input)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Text) > maxBytes {
			t.Fatalf("chunk text is %d bytes, want <= %d", len(chunk.Text), maxBytes)
		}
		var payload commandOutputChunkResult
		if err := json.Unmarshal(chunk.Structured, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Offset != offset || payload.EndOffset-payload.Offset != int64(len(payload.Content)) {
			t.Fatalf("range [%d,%d) does not match %d content bytes at offset %d", payload.Offset, payload.EndOffset, len(payload.Content), offset)
		}
		if payload.EndOffset <= offset {
			t.Fatalf("read made no progress at offset %d: %s", offset, chunk.Text)
		}
		offset = payload.EndOffset
		if payload.EOF {
			break
		}
	}
	if offset != 131072 {
		t.Fatalf("paginated %d bytes, want 131072", offset)
	}
}
