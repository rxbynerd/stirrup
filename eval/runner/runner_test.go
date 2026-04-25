package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestRunSuite_EmptySuite(t *testing.T) {
	_, err := RunSuite(context.Background(), types.EvalSuite{ID: "s1"}, RunConfig{})
	if err == nil {
		t.Fatal("expected error for empty suite")
	}
	if err.Error() != "suite must contain at least one task" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunSuite_EmptyID(t *testing.T) {
	suite := types.EvalSuite{
		Tasks: []types.EvalTask{{ID: "t1", Prompt: "hi"}},
	}
	_, err := RunSuite(context.Background(), suite, RunConfig{})
	if err == nil {
		t.Fatal("expected error for empty suite ID")
	}
	if err.Error() != "suite ID is required" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunSuite_DryRun(t *testing.T) {
	suite := types.EvalSuite{
		ID: "test-suite",
		Tasks: []types.EvalTask{
			{ID: "task-1", Prompt: "do something"},
			{ID: "task-2", Prompt: "do something else"},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.SuiteID != "test-suite" {
		t.Errorf("SuiteID = %q, want %q", result.SuiteID, "test-suite")
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(result.Tasks))
	}
	for _, tr := range result.Tasks {
		if tr.Outcome != "pass" {
			t.Errorf("task %s: outcome = %q, want %q", tr.TaskID, tr.Outcome, "pass")
		}
		if !tr.JudgeVerdict.Passed {
			t.Errorf("task %s: verdict not passed in dry run", tr.TaskID)
		}
	}
	if result.PassRate != 1.0 {
		t.Errorf("PassRate = %f, want 1.0", result.PassRate)
	}
}

func TestRunSuite_WithFakeHarness(t *testing.T) {
	// Create a fake harness script that writes a trace file.
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")

	// The fake harness reads --trace from args and writes a JSONL trace there.
	// The first argument is the "harness" subcommand, which we skip.
	script := `#!/bin/sh
shift
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":3,"cost":0.05,"outcome":"success"}' > "$TRACE"
fi
`
	if err := os.WriteFile(harnessPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a workspace with a file for the judge to check.
	workspaceContent := t.TempDir()
	targetFile := filepath.Join(workspaceContent, "output.txt")
	if err := os.WriteFile(targetFile, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	suite := types.EvalSuite{
		ID: "harness-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "task-file-exists",
				Prompt: "create output.txt",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"output.txt"},
				},
			},
		},
	}

	outputDir := t.TempDir()
	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		OutputDir:   outputDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The fake harness runs in a temp dir, so the workspace won't have output.txt.
	// The task will produce a result (possibly error/fail) but should not crash.
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	// We mainly verify the runner doesn't panic and produces a result.
	t.Logf("task outcome: %s, verdict: %s", result.Tasks[0].Outcome, result.Tasks[0].JudgeVerdict.Reason)
}

func TestReplayRecording_Passing(t *testing.T) {
	workspace := t.TempDir()
	// Create the file the judge will look for.
	if err := os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	recording := types.RunRecording{
		RunID: "replay-1",
		FinalOutcome: types.RunTrace{
			ID:    "trace-1",
			Turns: 2,
		},
	}

	task := types.EvalTask{
		ID: "replay-task",
		Judge: types.EvalJudge{
			Type:  "file-exists",
			Paths: []string{"result.txt"},
		},
	}

	result, err := ReplayRecording(context.Background(), recording, task, workspace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Outcome != "pass" {
		t.Errorf("outcome = %q, want %q", result.Outcome, "pass")
	}
	if !result.JudgeVerdict.Passed {
		t.Error("expected verdict to pass")
	}
	if result.Trace == nil {
		t.Fatal("expected trace to be set")
	}
}

func TestReplayRecording_Failing(t *testing.T) {
	workspace := t.TempDir()
	// Do NOT create the file — the judge should fail.

	recording := types.RunRecording{
		RunID: "replay-2",
		FinalOutcome: types.RunTrace{
			ID:    "trace-2",
			Turns: 1,
		},
	}

	task := types.EvalTask{
		ID: "replay-fail-task",
		Judge: types.EvalJudge{
			Type:  "file-exists",
			Paths: []string{"missing.txt"},
		},
	}

	result, err := ReplayRecording(context.Background(), recording, task, workspace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Outcome != "fail" {
		t.Errorf("outcome = %q, want %q", result.Outcome, "fail")
	}
	if result.JudgeVerdict.Passed {
		t.Error("expected verdict to fail")
	}
}

func TestParseTraceFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid trace", func(t *testing.T) {
		trace := types.RunTrace{ID: "t1", Turns: 5}
		data, _ := json.Marshal(trace)
		path := filepath.Join(dir, "valid.jsonl")
		_ = os.WriteFile(path, append([]byte("ignored first line\n"), append(data, '\n')...), 0o644)

		got, err := parseTraceFile(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != "t1" {
			t.Errorf("ID = %q, want %q", got.ID, "t1")
		}
		if got.Turns != 5 {
			t.Errorf("Turns = %d, want 5", got.Turns)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(dir, "empty.jsonl")
		_ = os.WriteFile(path, []byte(""), 0o644)

		_, err := parseTraceFile(path)
		if err == nil {
			t.Fatal("expected error for empty trace file")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := parseTraceFile(filepath.Join(dir, "nonexistent.jsonl"))
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}

func TestValidateSuite(t *testing.T) {
	tests := []struct {
		name    string
		suite   types.EvalSuite
		wantErr string
	}{
		{
			name:    "empty ID",
			suite:   types.EvalSuite{Tasks: []types.EvalTask{{ID: "t1"}}},
			wantErr: "suite ID is required",
		},
		{
			name:    "no tasks",
			suite:   types.EvalSuite{ID: "s1"},
			wantErr: "suite must contain at least one task",
		},
		{
			name:  "valid",
			suite: types.EvalSuite{ID: "s1", Tasks: []types.EvalTask{{ID: "t1"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSuite(tt.suite)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}
