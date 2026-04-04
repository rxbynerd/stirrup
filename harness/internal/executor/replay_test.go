package executor

import (
	"context"
	"encoding/json"
	"testing"

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

func TestReplayExecutor_ReadFile(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	content, err := re.ReadFile(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "package main\n\nfunc main() {}\n" {
		t.Errorf("content = %q, want package main source", content)
	}
}

func TestReplayExecutor_ReadFile_NoMatch(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.ReadFile(context.Background(), "nonexistent.go")
	if err == nil {
		t.Fatal("expected error for unrecorded path, got nil")
	}
}

func TestReplayExecutor_ReadFile_FailedRecording(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for failed recording, got nil")
	}
}

func TestReplayExecutor_WriteFile(t *testing.T) {
	re := NewReplayExecutor("/workspace", nil)

	err := re.WriteFile(context.Background(), "out.txt", "hello")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	writes := re.Writes()
	if len(writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(writes))
	}
	if writes[0].Path != "out.txt" || writes[0].Content != "hello" {
		t.Errorf("write = %+v, want {out.txt, hello}", writes[0])
	}
}

func TestReplayExecutor_ListDirectory(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	entries, err := re.ListDirectory(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}

	want := map[string]bool{"main.go": true, "go.mod": true, "README.md": true}
	if len(entries) != len(want) {
		t.Fatalf("expected %d entries, got %d: %v", len(want), len(entries), entries)
	}
	for _, e := range entries {
		if !want[e] {
			t.Errorf("unexpected entry %q", e)
		}
	}
}

func TestReplayExecutor_ListDirectory_NoMatch(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.ListDirectory(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unrecorded directory, got nil")
	}
}

func TestReplayExecutor_Exec(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	result, err := re.Exec(context.Background(), "go test ./...", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "ok  example.com/pkg 0.003s\n" {
		t.Errorf("Stdout = %q, want test output", result.Stdout)
	}
}

func TestReplayExecutor_Exec_NoMatch(t *testing.T) {
	re := NewReplayExecutor("/workspace", replayTurns())

	_, err := re.Exec(context.Background(), "unknown-command", 0)
	if err == nil {
		t.Fatal("expected error for unrecorded command, got nil")
	}
}

func TestReplayExecutor_Exec_FailedRecording(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Error("expected non-zero exit code for failed recording")
	}
}

func TestReplayExecutor_ResolvePath(t *testing.T) {
	re := NewReplayExecutor("/workspace", nil)

	resolved, err := re.ResolvePath("src/main.go")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if resolved != "/workspace/src/main.go" {
		t.Errorf("resolved = %q, want /workspace/src/main.go", resolved)
	}
}

func TestReplayExecutor_Capabilities(t *testing.T) {
	re := NewReplayExecutor("/workspace", nil)
	caps := re.Capabilities()

	if !caps.CanRead {
		t.Error("expected CanRead = true")
	}
	if !caps.CanWrite {
		t.Error("expected CanWrite = true")
	}
	if !caps.CanExec {
		t.Error("expected CanExec = true")
	}
	if caps.CanNetwork {
		t.Error("expected CanNetwork = false")
	}
}

func TestReplayExecutor_InputWhitespaceNormalization(t *testing.T) {
	// The recording has compact JSON but the lookup constructs it with
	// potentially different whitespace. Verify normalization works.
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
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "content" {
		t.Errorf("content = %q, want %q", content, "content")
	}
}
