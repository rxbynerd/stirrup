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
	must := require.New(t)
	re := NewReplayExecutor("/workspace", replayTurns())

	content, err := re.ReadFile(context.Background(), "main.go")
	must.NoError(err)
	must.Equal("package main\n\nfunc main() {}\n", content)
}

func TestReplayExecutor_ReadFile_UnrecordedPathErrors(t *testing.T) {
	must := require.New(t)
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.ReadFile(context.Background(), "nonexistent.go")
	must.Error(err)
}

func TestReplayExecutor_ReadFile_FailedRecordingErrors(t *testing.T) {
	must := require.New(t)
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
	must.Error(err)
}

func TestReplayExecutor_WriteFile_CapturesWrite(t *testing.T) {
	must := require.New(t)
	re := NewReplayExecutor("/workspace", nil)

	err := re.WriteFile(context.Background(), "out.txt", "hello")
	must.NoError(err)

	writes := re.Writes()
	must.Len(writes, 1)
	must.Equal(writeRecord{Path: "out.txt", Content: "hello"}, writes[0])
}

func TestReplayExecutor_ListDirectory_ReturnsRecordedEntries(t *testing.T) {
	must := require.New(t)
	re := NewReplayExecutor("/workspace", replayTurns())

	entries, err := re.ListDirectory(context.Background(), ".")
	must.NoError(err)

	want := []string{"main.go", "go.mod", "README.md"}
	must.Len(entries, len(want))
	for _, e := range entries {
		must.Contains(want, e)
	}
}

func TestReplayExecutor_ListDirectory_UnrecordedDirectoryErrors(t *testing.T) {
	must := require.New(t)
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.ListDirectory(context.Background(), "nonexistent")
	must.Error(err)
}

func TestReplayExecutor_Exec_ReturnsRecordedOutput(t *testing.T) {
	must := require.New(t)
	re := NewReplayExecutor("/workspace", replayTurns())

	result, err := re.Exec(context.Background(), "go test ./...", 0)
	must.NoError(err)
	must.Equal(0, result.ExitCode)
	must.Equal("ok  example.com/pkg 0.003s\n", result.Stdout)
}

func TestReplayExecutor_Exec_UnrecordedCommandErrors(t *testing.T) {
	must := require.New(t)
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.Exec(context.Background(), "unknown-command", 0)
	must.Error(err)
}

func TestReplayExecutor_Exec_FailedRecordingReturnsNonZeroExit(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.NotZero(result.ExitCode)
}

func TestReplayExecutor_ResolvePath_JoinsWorkspace(t *testing.T) {
	must := require.New(t)
	re := NewReplayExecutor("/workspace", nil)

	resolved, err := re.ResolvePath("src/main.go")
	must.NoError(err)
	must.Equal("/workspace/src/main.go", resolved)
}

func TestReplayExecutor_Capabilities_ReadWriteExecWithoutNetwork(t *testing.T) {
	check := assert.New(t)
	re := NewReplayExecutor("/workspace", nil)
	caps := re.Capabilities()

	check.True(caps.CanRead, "CanRead")
	check.True(caps.CanWrite, "CanWrite")
	check.True(caps.CanExec, "CanExec")
	check.False(caps.CanNetwork, "CanNetwork")
}

func TestReplayExecutor_ReadFile_MatchesInputIgnoringJSONWhitespace(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Equal("content", content)
}
