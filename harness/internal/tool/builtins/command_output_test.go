package builtins

import (
	"context"
	"encoding/json"
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
	run := RunCommandToolWithStore(exec, store, cfg)
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

	reader := ReadCommandOutputTool(store, exec)
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
	run := RunCommandToolWithStore(exec, store, cfg)
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
	runResult, err := RunCommandToolWithStore(replay, store, types.CommandOutputConfig{}).StructuredHandler(context.Background(), runInput)
	if err != nil {
		t.Fatal(err)
	}
	if runResult.Text != "recorded initial preview" {
		t.Fatalf("run replay=%q", runResult.Text)
	}
	readResult, err := ReadCommandOutputTool(store, replay).StructuredHandler(context.Background(), readInput)
	if err != nil {
		t.Fatal(err)
	}
	if readResult.Text != "recorded read chunk" {
		t.Fatalf("read replay=%q", readResult.Text)
	}
}
