package builtins

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunCommand_SpillsLargeOutputAndReadsBack(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	cfg := types.CommandOutputConfig{InlineMaxBytes: 5, PreviewBytesPerStream: 3, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	run := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes)

	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", Turn: 1, ToolUseID: "inline"})
	inline, err := run.StructuredHandler(ctx, json.RawMessage(`{"command":"printf 12345"}`))
	require.NoError(t, err)
	require.Equal(t, "12345", inline.Text, "output within the inline limit is returned as text")

	ctx = tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", Turn: 2, ToolUseID: "spill"})
	spilled, err := run.StructuredHandler(ctx, json.RawMessage(`{"command":"printf 123456; printf abcdef >&2"}`))
	require.NoError(t, err)
	require.Contains(t, spilled.Text, "command output spilled")
	require.Contains(t, spilled.Text, "stirrup://command-output/")
	var result commandResult
	require.NoError(t, json.Unmarshal(spilled.Structured, &result))
	require.True(t, result.Spilled)
	require.Equal(t, "456", result.Stdout, "preview keeps the last bytes of stdout")
	require.Equal(t, "def", result.Stderr, "preview keeps the last bytes of stderr")

	reader := ReadCommandOutputTool(store, exec, types.DefaultToolMaxResultBytes)
	readCtx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", Turn: 3, ToolUseID: "reader"})
	input, _ := json.Marshal(map[string]any{"ref": result.StdoutRef, "offset": 1, "limit": 3})
	chunk, err := reader.StructuredHandler(readCtx, input)
	require.NoError(t, err)
	require.Contains(t, chunk.Text, `"content":"234"`)

	_, err = store.Finalize(context.Background())
	require.NoError(t, err)
	require.FileExists(t, store.Archive())
}

func TestRunCommand_TimeoutReturnsPartialOutputAsError(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	cfg := types.CommandOutputConfig{InlineMaxBytes: 32 << 10, PreviewBytesPerStream: 1024, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "run-timeout", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	run := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes)

	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run-timeout", ToolUseID: "timeout"})
	result, err := run.StructuredHandler(ctx, json.RawMessage(`{"command":"printf partial; sleep 5","timeout":1}`))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "partial")

	var structured commandResult
	require.NoError(t, json.Unmarshal(result.Structured, &structured))
	require.True(t, structured.TimedOut)
	require.True(t, structured.CaptureComplete)
}

func TestCommandOutputReplay_RecordedResultsReturnedVerbatim(t *testing.T) {
	runInput := json.RawMessage(`{"command":"big-output","timeout":30}`)
	readInput := json.RawMessage(`{"ref":"stirrup://command-output/a/b/stdout","offset":0}`)
	replay := executor.NewReplayExecutor(t.TempDir(), []types.TurnRecord{{ToolCalls: []types.ToolCallRecord{
		{Name: "run_command", Input: runInput, Output: "recorded initial preview", Success: true, Kind: kindCommandResult, Structured: json.RawMessage(`{"spilled":true}`)},
		{Name: "read_command_output", Input: readInput, Output: "recorded read chunk", Success: true, Kind: kindCommandOutputChunk},
	}}})
	store, err := commandoutput.New(commandoutput.Options{RunID: "replay", Config: types.CommandOutputConfig{}, ArchivePath: filepath.Join(t.TempDir(), "unused.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	runResult, err := RunCommandToolWithStore(replay, store, types.CommandOutputConfig{}, types.DefaultToolMaxResultBytes).StructuredHandler(context.Background(), runInput)
	require.NoError(t, err)
	require.Equal(t, "recorded initial preview", runResult.Text)

	readResult, err := ReadCommandOutputTool(store, replay, types.DefaultToolMaxResultBytes).StructuredHandler(context.Background(), readInput)
	require.NoError(t, err)
	require.Equal(t, "recorded read chunk", readResult.Text)
}

// On the capture path the timeout marker must appear in Text. A reader of
// Text alone otherwise cannot tell a timed-out partial transcript from a
// clean exit.
func TestRunCommand_CaptureTimeoutAddsTimedOutMarker(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	cfg := types.CommandOutputConfig{InlineMaxBytes: 1 << 20, PreviewBytesPerStream: 1 << 10, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "timeout", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "timeout", Turn: 1, ToolUseID: "slow"})
	got, err := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes).StructuredHandler(ctx, json.RawMessage(`{"command":"printf partial; sleep 30","timeout":1}`))
	require.NoError(t, err)
	require.Contains(t, got.Text, "partial")
	require.Contains(t, got.Text, "[timed out after 1s]")

	var result commandResult
	require.NoError(t, json.Unmarshal(got.Structured, &result))
	require.True(t, result.TimedOut)
	require.Equal(t, 1, result.TimeoutSeconds)
}

// TestRunCommand_ArchiveContainsOnlyScrubbedBytes drives the production
// path — a real shell command through the local executor into the store —
// and checks the archive on disk, not only the model-visible text.
func TestRunCommand_ArchiveContainsOnlyScrubbedBytes(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	cfg := types.CommandOutputConfig{InlineMaxBytes: 32 << 10, PreviewBytesPerStream: 1024, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	archive := filepath.Join(t.TempDir(), "archive.tar.gz")
	store, err := commandoutput.New(commandoutput.Options{RunID: "scrub", Config: cfg, ArchivePath: archive})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	secret := "gh" + "p_runCommandFixture0123456789"
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "scrub", ToolUseID: "cmd"})
	input, _ := json.Marshal(map[string]any{"command": "printf 'token=" + secret + "\\n'"})
	result, err := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes).StructuredHandler(ctx, input)
	require.NoError(t, err)
	require.NotContains(t, result.Text, secret)
	require.Contains(t, result.Text, "[REDACTED]")

	_, err = store.Finalize(context.Background())
	require.NoError(t, err)
	f, err := os.Open(store.Archive())
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)

	members := tar.NewReader(gz)
	redacted := false
	for {
		header, err := members.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		body, err := io.ReadAll(members)
		require.NoError(t, err)
		require.NotContains(t, string(body), secret, "archive member %s holds the secret", header.Name)
		redacted = redacted || strings.Contains(string(body), "[REDACTED]")
	}
	require.True(t, redacted, "no archive member carries the redaction, so the assertion proved nothing")
}

func TestReadCommandOutput_PaginationStaysWithinMaxResultBytes(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	cfg := types.CommandOutputConfig{InlineMaxBytes: 16, PreviewBytesPerStream: 16, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	run := RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes)
	require.True(t, run.SelfBoundedResult, "run_command with a capture store must be self-bounded")

	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", ToolUseID: "spill"})
	// '<' JSON-encodes to six bytes, so the marshalled chunk is several
	// times larger than the raw byte range.
	spilled, err := run.StructuredHandler(ctx, json.RawMessage(`{"command":"head -c 131072 /dev/zero | tr '\\0' '<'"}`))
	require.NoError(t, err)
	var result commandResult
	require.NoError(t, json.Unmarshal(spilled.Structured, &result))

	const maxBytes = 2048
	reader := ReadCommandOutputTool(store, exec, maxBytes)
	require.True(t, reader.SelfBoundedResult, "read_command_output must be self-bounded")
	offset := int64(0)
	for reads := 0; ; reads++ {
		require.LessOrEqual(t, reads, 2000, "pagination did not reach EOF")
		readCtx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run", ToolUseID: "reader"})
		input, _ := json.Marshal(map[string]any{"ref": result.StdoutRef, "offset": offset, "limit": commandoutput.ReadMaxBytes})
		chunk, err := reader.StructuredHandler(readCtx, input)
		require.NoError(t, err)
		require.LessOrEqual(t, len(chunk.Text), maxBytes)

		var payload commandOutputChunkResult
		require.NoError(t, json.Unmarshal(chunk.Structured, &payload))
		require.Equal(t, offset, payload.Offset, "chunk does not start at the requested offset")
		require.Equal(t, int64(len(payload.Content)), payload.EndOffset-payload.Offset, "range does not match content bytes")
		require.Greater(t, payload.EndOffset, offset, "read made no progress: %s", chunk.Text)

		offset = payload.EndOffset
		if payload.EOF {
			break
		}
	}
	require.Equal(t, int64(131072), offset)
}

// TestFitCommandOutputChunk_CutsOnRuneBoundary checks that shortening a
// chunk to fit the result budget never splits a multi-byte character.
func TestFitCommandOutputChunk_CutsOnRuneBoundary(t *testing.T) {
	content := []byte(strings.Repeat("€", 4000))
	for _, budget := range []int{1 << 10, 1<<10 + 1, 1<<10 + 2, 4 << 10} {
		t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
			read := commandoutput.ReadResult{Bytes: content, End: int64(len(content)), EOF: true, Stream: "stdout", Content: types.CommandOutputStreamRecord{ScrubbedBytes: int64(len(content))}}
			got, structured, err := fitCommandOutputChunk("stirrup://command-output/x", read, budget)
			require.NoError(t, err)
			require.LessOrEqual(t, len(structured), budget)
			require.True(t, utf8.Valid(got.Bytes))
			require.False(t, strings.ContainsRune(string(structured), utf8.RuneError), "chunk was cut inside a character")
			require.NotContains(t, string(structured), `\ufffd`, "chunk was cut inside a character")
			require.False(t, got.EOF, "range not shortened")
			require.Equal(t, int64(len(got.Bytes)), got.End)
		})
	}
}
