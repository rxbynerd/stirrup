package executor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func replayTurns() []types.TurnRecord {
	return []types.TurnRecord{
		{
			Turn: 1,
			ToolCalls: []types.ToolCallRecord{
				{
					ID:      "tc_1",
					Name:    "read_file",
					Input:   json.RawMessage(`{"path":"main.go"}`),
					Output:  "package main\n\nfunc main() {}\n",
					Success: true,
				},
				{
					ID:      "tc_2",
					Name:    "list_directory",
					Input:   json.RawMessage(`{"path":"."}`),
					Output:  "main.go\ngo.mod\nREADME.md",
					Success: true,
				},
				{
					ID:      "tc_3",
					Name:    "run_command",
					Input:   json.RawMessage(`{"command":"go test ./..."}`),
					Output:  "ok  example.com/pkg 0.003s\n",
					Success: true,
				},
			},
		},
	}
}

func TestReplayExecutor_ReadFile_ReturnsRecordedOutput(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	content, err := re.ReadFile(context.Background(), "main.go")
	require.NoError(t, err)
	require.Equal(t, "package main\n\nfunc main() {}\n", content)
}

func TestReplayExecutor_ReadFile_UnrecordedPathErrors(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.ReadFile(context.Background(), "nonexistent.go")
	require.Error(t, err)
}

func TestReplayExecutor_ReadFile_FailedRecordingErrors(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ToolCalls: []types.ToolCallRecord{
				{
					Name:    "read_file",
					Input:   json.RawMessage(`{"path":"missing.txt"}`),
					Output:  "file not found",
					Success: false,
				},
			},
		},
	}
	re := NewReplayExecutor("/workspace", turns)

	_, err := re.ReadFile(context.Background(), "missing.txt")
	require.Error(t, err)
}

func TestReplayExecutor_WriteFile_CapturesWrite(t *testing.T) {
	re := NewReplayExecutor("/workspace", nil)

	err := re.WriteFile(context.Background(), "out.txt", "hello")
	require.NoError(t, err)

	writes := re.Writes()
	require.Len(t, writes, 1)
	require.Equal(t, writeRecord{Path: "out.txt", Content: "hello"}, writes[0])
}

func TestReplayExecutor_ListDirectory_ReturnsRecordedEntries(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	entries, err := re.ListDirectory(context.Background(), ".")
	require.NoError(t, err)

	want := []string{"main.go", "go.mod", "README.md"}
	require.Len(t, entries, len(want))
	for _, e := range entries {
		require.Contains(t, want, e)
	}
}

func TestReplayExecutor_ListDirectory_UnrecordedDirectoryErrors(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.ListDirectory(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestReplayExecutor_Exec_ReturnsRecordedOutput(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	result, err := re.Exec(context.Background(), "go test ./...", 0)
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode)
	require.Equal(t, "ok  example.com/pkg 0.003s\n", result.Stdout)
}

func TestReplayExecutor_Exec_UnrecordedCommandErrors(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.Exec(context.Background(), "unknown-command", 0)
	require.Error(t, err)
}

func TestReplayExecutor_Exec_FailedRecordingReturnsNonZeroExit(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ToolCalls: []types.ToolCallRecord{
				{
					Name:    "run_command",
					Input:   json.RawMessage(`{"command":"go test ./..."}`),
					Output:  "FAIL example.com/pkg\n",
					Success: false,
				},
			},
		},
	}
	re := NewReplayExecutor("/workspace", turns)

	result, err := re.Exec(context.Background(), "go test ./...", 0)
	require.NoError(t, err)
	require.NotZero(t, result.ExitCode)
}

func TestReplayExecutor_ResolvePath_JoinsWorkspace(t *testing.T) {
	re := NewReplayExecutor("/workspace", nil)

	resolved, err := re.ResolvePath("src/main.go")
	require.NoError(t, err)
	require.Equal(t, "/workspace/src/main.go", resolved)
}

func TestReplayExecutor_Capabilities_ReadWriteExecWithoutNetwork(t *testing.T) {
	re := NewReplayExecutor("/workspace", nil)
	caps := re.Capabilities()

	assert.True(t, caps.CanRead, "CanRead")
	assert.True(t, caps.CanWrite, "CanWrite")
	assert.True(t, caps.CanExec, "CanExec")
	assert.False(t, caps.CanNetwork, "CanNetwork")
}

func TestReplayExecutor_ReadFile_MatchesInputIgnoringJSONWhitespace(t *testing.T) {
	// Recorded input uses spacing that differs from the compact form the lookup builds.
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ToolCalls: []types.ToolCallRecord{
				{
					Name:    "read_file",
					Input:   json.RawMessage(`{ "path" : "spaced.go" }`),
					Output:  "content",
					Success: true,
				},
			},
		},
	}
	re := NewReplayExecutor("/workspace", turns)

	content, err := re.ReadFile(context.Background(), "spaced.go")
	require.NoError(t, err)
	require.Equal(t, "content", content)
}
