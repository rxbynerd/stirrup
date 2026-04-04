// Package executor — replay.go implements a deterministic Executor that
// replays recorded tool outputs for eval testing without real file I/O.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// ReplayExecutor replays recorded tool call outputs. It indexes recordings by
// (toolName, canonicalInput) so that lookups match the same calls the model
// made during the original run.
type ReplayExecutor struct {
	workspace  string
	recordings map[string]types.ToolCallRecord // key: "toolName|canonicalInput"
	writes     []writeRecord
	mu         sync.Mutex
}

// writeRecord tracks a file write for later verification.
type writeRecord struct {
	Path    string
	Content string
}

// NewReplayExecutor creates a ReplayExecutor from recorded turns. The workspace
// parameter sets the virtual workspace root used by ResolvePath.
func NewReplayExecutor(workspace string, turns []types.TurnRecord) *ReplayExecutor {
	recordings := make(map[string]types.ToolCallRecord)
	for _, turn := range turns {
		for _, tc := range turn.ToolCalls {
			key := recordingKey(tc.Name, tc.Input)
			recordings[key] = tc
		}
	}
	return &ReplayExecutor{
		workspace:  workspace,
		recordings: recordings,
	}
}

// ReadFile looks up a matching read_file tool call record and returns its output.
func (re *ReplayExecutor) ReadFile(_ context.Context, path string) (string, error) {
	input := mustMarshal(map[string]string{"path": path})
	rec, ok := re.lookup("read_file", input)
	if !ok {
		return "", fmt.Errorf("replay executor: no recorded read_file call for path %q", path)
	}
	if !rec.Success {
		return "", fmt.Errorf("replay executor: recorded read_file for %q was an error: %s", path, rec.Output)
	}
	return rec.Output, nil
}

// WriteFile records the write for later verification. Always succeeds.
func (re *ReplayExecutor) WriteFile(_ context.Context, path string, content string) error {
	re.mu.Lock()
	defer re.mu.Unlock()
	re.writes = append(re.writes, writeRecord{Path: path, Content: content})
	return nil
}

// ListDirectory looks up a matching list_directory tool call record.
func (re *ReplayExecutor) ListDirectory(_ context.Context, path string) ([]string, error) {
	input := mustMarshal(map[string]string{"path": path})
	rec, ok := re.lookup("list_directory", input)
	if !ok {
		return nil, fmt.Errorf("replay executor: no recorded list_directory call for path %q", path)
	}
	if !rec.Success {
		return nil, fmt.Errorf("replay executor: recorded list_directory for %q was an error: %s", path, rec.Output)
	}
	entries := strings.Split(rec.Output, "\n")
	// Filter out empty strings from trailing newlines.
	var result []string
	for _, e := range entries {
		if e != "" {
			result = append(result, e)
		}
	}
	return result, nil
}

// Exec looks up a matching run_command tool call record and parses an ExecResult.
// The recorded output is used as stdout; the exit code defaults to 0 for
// successful recordings and 1 for failed ones.
func (re *ReplayExecutor) Exec(_ context.Context, command string, _ time.Duration) (*ExecResult, error) {
	input := mustMarshal(map[string]string{"command": command})
	rec, ok := re.lookup("run_command", input)
	if !ok {
		return nil, fmt.Errorf("replay executor: no recorded run_command call for command %q", command)
	}

	exitCode := 0
	if !rec.Success {
		exitCode = 1
	}

	// Try to extract exit code from output if it follows the pattern
	// "exit code: N\n..." used by some recorders.
	stdout := rec.Output
	if strings.HasPrefix(stdout, "exit code: ") {
		if idx := strings.Index(stdout, "\n"); idx != -1 {
			codeStr := strings.TrimPrefix(stdout[:idx], "exit code: ")
			if code, err := strconv.Atoi(codeStr); err == nil {
				exitCode = code
				stdout = stdout[idx+1:]
			}
		}
	}

	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout,
	}, nil
}

// ResolvePath returns the path joined with the workspace root.
func (re *ReplayExecutor) ResolvePath(relativePath string) (string, error) {
	return filepath.Join(re.workspace, relativePath), nil
}

// Capabilities returns full capabilities since the replay executor simulates
// a fully capable environment.
func (re *ReplayExecutor) Capabilities() ExecutorCapabilities {
	return ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   true,
		CanExec:    true,
		CanNetwork: false,
		MaxTimeout: 5 * time.Minute,
	}
}

// Writes returns all file writes recorded during the replay, useful for
// verifying that the harness attempted the expected writes.
func (re *ReplayExecutor) Writes() []writeRecord {
	re.mu.Lock()
	defer re.mu.Unlock()
	result := make([]writeRecord, len(re.writes))
	copy(result, re.writes)
	return result
}

// lookup finds a recorded tool call by name and canonical input JSON.
func (re *ReplayExecutor) lookup(toolName string, input json.RawMessage) (types.ToolCallRecord, bool) {
	key := recordingKey(toolName, input)
	rec, ok := re.recordings[key]
	return rec, ok
}

// recordingKey produces a canonical lookup key from a tool name and its input
// JSON. The input is re-marshalled through json.Compact to normalize whitespace.
func recordingKey(toolName string, input json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, input); err != nil {
		// If the input isn't valid JSON, use it as-is for the key.
		return toolName + "|" + string(input)
	}
	return toolName + "|" + compact.String()
}

// mustMarshal marshals v to JSON, panicking on error (only used for
// simple map literals that are guaranteed to succeed).
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustMarshal: %v", err))
	}
	return data
}
