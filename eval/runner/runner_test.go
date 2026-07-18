package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
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
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")

	// Reads --trace from args and writes a JSONL trace there; the first
	// argument (the "harness" subcommand) is skipped.
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

	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	t.Logf("task outcome: %s, verdict: %s", result.Tasks[0].Outcome, result.Tasks[0].JudgeVerdict.Reason)
}

// TestRunSuite_RelativeHarnessPath pins that RunSuite anchors a relative,
// separator-bearing harness path (e.g. "./stirrup") to absolute before
// invoking it, since it execs against each task's temp workspace rather
// than the caller's CWD.
func TestRunSuite_RelativeHarnessPath(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
shift
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo hello > output.txt
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.0,"outcome":"success"}' > "$TRACE"
fi
`
	if err := os.WriteFile(filepath.Join(dir, "fake-harness"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// CWD = dir so "./fake-harness" resolves the way "./stirrup" does
	// relative to GITHUB_WORKSPACE in CI.
	t.Chdir(dir)

	suite := types.EvalSuite{
		ID: "relative-harness-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "task-relative-harness",
				Prompt: "create output.txt",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"output.txt"},
				},
			},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: "./fake-harness",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	got := result.Tasks[0]
	if got.Outcome == "error" {
		t.Fatalf("task errored (relative harness path not resolved to absolute?): %s", got.Error)
	}
	if got.Outcome != "pass" {
		t.Errorf("outcome = %q, want %q (reason: %s)", got.Outcome, "pass", got.JudgeVerdict.Reason)
	}
}

// TestRunSuite_SeedsFilesAndHidesTrace pins two coupled behaviours: (1)
// task.Files are written into the workspace before the harness runs, and
// (2) the per-task trace file is NOT in the workspace, so the agent cannot
// list/read harness internals.
func TestRunSuite_SeedsFilesAndHidesTrace(t *testing.T) {
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")
	script := `#!/bin/sh
shift
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
ls -a .
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.0,"outcome":"success"}' > "$TRACE"
fi
`
	if err := os.WriteFile(harnessPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	outputDir := t.TempDir()
	suite := types.EvalSuite{
		ID: "seed-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "seed-task",
				Prompt: "summarise README.md",
				Files:  map[string]string{"README.md": "# Stirrup\nA coding-agent harness.\n"},
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"README.md"},
				},
			},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		OutputDir:   outputDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result.Tasks[0].Outcome; got != "pass" {
		t.Fatalf("outcome = %q, want pass (seed file should exist for the judge)", got)
	}

	stdout, err := os.ReadFile(filepath.Join(outputDir, "seed-suite", "seed-task", "harness.stdout.txt"))
	if err != nil {
		t.Fatalf("reading retained stdout: %v", err)
	}
	listing := string(stdout)
	if !strings.Contains(listing, "README.md") {
		t.Errorf("workspace listing %q does not contain seeded README.md", listing)
	}
	if strings.Contains(listing, "trace.jsonl") {
		t.Errorf("trace.jsonl leaked into the workspace; listing = %q", listing)
	}
}

func TestReplayRecording_Passing(t *testing.T) {
	workspace := t.TempDir()
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
		{
			name: "traversal in suite ID",
			suite: types.EvalSuite{
				ID:    "../evil",
				Tasks: []types.EvalTask{{ID: "t1"}},
			},
			wantErr: `suite ID "../evil" must not contain path separators`,
		},
		{
			name: "absolute suite ID",
			suite: types.EvalSuite{
				ID:    "/etc/passwd",
				Tasks: []types.EvalTask{{ID: "t1"}},
			},
			wantErr: `suite ID "/etc/passwd" must not contain path separators`,
		},
		{
			name: "duplicate task IDs",
			suite: types.EvalSuite{
				ID:    "s1",
				Tasks: []types.EvalTask{{ID: "t1"}, {ID: "t1"}},
			},
			wantErr: `duplicate task ID "t1"`,
		},
		{
			name: "traversal in task ID",
			suite: types.EvalSuite{
				ID:    "s1",
				Tasks: []types.EvalTask{{ID: "../escape"}},
			},
			wantErr: `task ID "../escape" must not contain path separators`,
		},
		{
			name: "dot-segment task ID",
			suite: types.EvalSuite{
				ID:    "s1",
				Tasks: []types.EvalTask{{ID: "."}},
			},
			wantErr: `task ID "." is a reserved path segment`,
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

// writeFakeHarness writes body as a POSIX-shell harness double and returns
// its path. Skips on non-Unix runners since /bin/sh is not portable to
// Windows.
func writeFakeHarness(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake harness uses /bin/sh; skipped on Windows")
	}
	harnessDir := t.TempDir()
	path := filepath.Join(harnessDir, "fake-harness")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunSuite_ConcurrencyOrdersDeterministically pins that the worker pool
// preserves suite task order in the returned SuiteResult.Tasks slice
// regardless of which workers actually finished first, using a
// completion-order log to confirm concurrency genuinely happened.
func TestRunSuite_ConcurrencyOrdersDeterministically(t *testing.T) {
	logDir := t.TempDir()
	completionLog := filepath.Join(logDir, "completion.log")

	// POSIX guarantees small (<= PIPE_BUF) appends are atomic, so the
	// harness can append the task ID directly without a tempfile-rename.
	script := fmt.Sprintf(`#!/bin/sh
PROMPT=""
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --prompt) PROMPT="$2"; shift 2 ;;
    --trace)  TRACE="$2";  shift 2 ;;
    *) shift ;;
  esac
done
# PROMPT format: "<sleep-ms>:<task-id>".
SLEEP_MS=$(echo "$PROMPT" | cut -d: -f1)
TASK_ID=$(echo "$PROMPT" | cut -d: -f2)
SLEEP_S=$(awk -v ms="$SLEEP_MS" 'BEGIN { printf "%%.3f", ms/1000 }')
sleep "$SLEEP_S"
echo "$TASK_ID" >> %q
[ -n "$TRACE" ] && echo "{\"id\":\"trace-$TASK_ID\",\"turns\":1,\"outcome\":\"success\"}" > "$TRACE"
`, completionLog)
	harness := writeFakeHarness(t, script)

	// The longest sleep is on the first task, so a sequential run would put
	// t1 first in the completion log, but a concurrent run will not.
	tasks := []types.EvalTask{
		{ID: "t1", Prompt: "300:t1", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		{ID: "t2", Prompt: "20:t2", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		{ID: "t3", Prompt: "200:t3", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		{ID: "t4", Prompt: "10:t4", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		{ID: "t5", Prompt: "100:t5", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
	}
	suite := types.EvalSuite{ID: "concurrency-suite", Tasks: tasks}

	out := t.TempDir()
	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
		Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Tasks) != len(tasks) {
		t.Fatalf("got %d task results, want %d", len(result.Tasks), len(tasks))
	}
	for i, want := range tasks {
		if result.Tasks[i].TaskID != want.ID {
			t.Errorf("Tasks[%d].TaskID = %q, want %q (suite order not preserved)", i, result.Tasks[i].TaskID, want.ID)
		}
	}

	logBytes, err := os.ReadFile(completionLog)
	if err != nil {
		t.Fatalf("reading completion log: %v", err)
	}
	completionOrder := strings.Fields(string(logBytes))
	if len(completionOrder) != len(tasks) {
		t.Fatalf("completion log has %d entries, want %d: %q", len(completionOrder), len(tasks), completionOrder)
	}
	if completionOrder[0] == "t1" {
		t.Errorf("first task to finish was t1 (longest sleep): suggests sequential execution. log=%v", completionOrder)
	}
	// t1 (300ms sleep) must finish last under concurrency=4; a non-last t1
	// would mean a faster task got dispatched after it yet finished later,
	// which is impossible without contention this test doesn't introduce.
	last := completionOrder[len(completionOrder)-1]
	if last != "t1" {
		t.Errorf("expected t1 (300ms sleep) to finish last under concurrency=4; got %q; full order: %v",
			last, completionOrder)
	}
}

// TestRunSuite_ConcurrencyZeroDefaultsToOne pins that Concurrency<=0
// collapses to a sequential run rather than deadlocking.
func TestRunSuite_ConcurrencyZeroDefaultsToOne(t *testing.T) {
	script := `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "seq-suite",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
			{ID: "t2", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	done := make(chan struct{})
	go func() {
		_, err := RunSuite(context.Background(), suite, RunConfig{
			HarnessPath: harness,
			Concurrency: 0,
		})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunSuite with Concurrency=0 deadlocked")
	}
}

// TestRunSuite_FailureDoesNotAbortSiblings pins that if one task's harness
// returns non-zero, the other tasks still produce TaskResults and the
// suite result surfaces all of them.
func TestRunSuite_FailureDoesNotAbortSiblings(t *testing.T) {
	// Fails iff prompt == "boom", otherwise writes a valid trace.
	script := `#!/bin/sh
PROMPT=""
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --prompt) PROMPT="$2"; shift 2 ;;
    --trace)  TRACE="$2";  shift 2 ;;
    *) shift ;;
  esac
done
if [ "$PROMPT" = "boom" ]; then
  echo "harness boom" >&2
  exit 7
fi
[ -n "$TRACE" ] && echo '{"id":"ok","turns":1,"outcome":"success"}' > "$TRACE"
`
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "failure-suite",
		Tasks: []types.EvalTask{
			{ID: "ok-1", Prompt: "ok", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
			{ID: "broken", Prompt: "boom", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
			{ID: "ok-2", Prompt: "ok", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("unexpected suite error: %v", err)
	}

	if len(result.Tasks) != 3 {
		t.Fatalf("got %d task results, want 3 (sibling failures must not abort)", len(result.Tasks))
	}
	if result.Tasks[0].TaskID != "ok-1" || result.Tasks[1].TaskID != "broken" || result.Tasks[2].TaskID != "ok-2" {
		t.Errorf("Tasks ordering = [%s, %s, %s], want [ok-1, broken, ok-2]",
			result.Tasks[0].TaskID, result.Tasks[1].TaskID, result.Tasks[2].TaskID)
	}
	if result.Tasks[1].Outcome != "error" {
		t.Errorf("broken task Outcome = %q, want %q", result.Tasks[1].Outcome, "error")
	}
	// Siblings must each get a verdict, even if it's a fail (the placeholder
	// file does not exist in the temp workspace).
	for _, idx := range []int{0, 2} {
		if result.Tasks[idx].Outcome == "" {
			t.Errorf("sibling task %s has empty outcome", result.Tasks[idx].TaskID)
		}
	}
}

// TestRunSuite_RetainsArtifacts asserts that, when OutputDir is set, every
// task gets a per-task directory under <OutputDir>/<suiteID>/<taskID>/ with
// trace.jsonl, harness.stdout.txt, and harness.stderr.txt files. The
// stdout/stderr files exist even when empty (so consumers don't need to
// branch on file existence).
func TestRunSuite_RetainsArtifacts(t *testing.T) {
	script := `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo "stdout chatter"
echo "stderr chatter" >&2
[ -n "$TRACE" ] && echo '{"id":"trace-1","turns":1,"outcome":"success"}' > "$TRACE"
`
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "artifact-suite",
		Tasks: []types.EvalTask{
			{ID: "alpha", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
			{ID: "beta", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	out := t.TempDir()
	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, taskID := range []string{"alpha", "beta"} {
		base := filepath.Join(out, "artifact-suite", taskID)
		for _, name := range []string{"trace.jsonl", "harness.stdout.txt", "harness.stderr.txt"} {
			p := filepath.Join(base, name)
			info, err := os.Stat(p)
			if err != nil {
				t.Errorf("missing artifact %s: %v", p, err)
				continue
			}
			if info.IsDir() {
				t.Errorf("artifact %s is a directory, want regular file", p)
			}
		}

		traceData, err := os.ReadFile(filepath.Join(base, "trace.jsonl"))
		if err == nil && !strings.Contains(string(traceData), `"id":"trace-1"`) {
			t.Errorf("trace.jsonl for %s did not contain expected payload: %q", taskID, string(traceData))
		}
		stdout, _ := os.ReadFile(filepath.Join(base, "harness.stdout.txt"))
		if !strings.Contains(string(stdout), "stdout chatter") {
			t.Errorf("harness.stdout.txt for %s = %q, want to contain %q", taskID, string(stdout), "stdout chatter")
		}
		stderr, _ := os.ReadFile(filepath.Join(base, "harness.stderr.txt"))
		if !strings.Contains(string(stderr), "stderr chatter") {
			t.Errorf("harness.stderr.txt for %s = %q, want to contain %q", taskID, string(stderr), "stderr chatter")
		}
	}
}

// TestRunSuite_RejectsTraversalIDs pins that any suite/task ID that would
// resolve outside <OutputDir>/<suiteID>/<taskID>/ is rejected at
// validation time, rather than silently sanitised (which would shadow
// legitimate IDs and produce nondeterministic artifact trees).
func TestRunSuite_RejectsTraversalIDs(t *testing.T) {
	cases := []struct {
		name    string
		suiteID string
		taskID  string
		wantSub string
	}{
		{name: "suite parent traversal", suiteID: "../evil", taskID: "t", wantSub: "suite ID"},
		{name: "task parent traversal", suiteID: "ok", taskID: "../evil", wantSub: "task ID"},
		{name: "task absolute path", suiteID: "ok", taskID: "/etc/passwd", wantSub: "task ID"},
		{name: "task with separator", suiteID: "ok", taskID: "sub/dir", wantSub: "task ID"},
		{name: "task dot segment", suiteID: "ok", taskID: "..", wantSub: "task ID"},
		// `..`-prefixed IDs that are not exactly ".." must also be rejected
		// (validatePathSegment's HasPrefix(id, "..") branch).
		{name: "task dotdot prefix short", suiteID: "ok", taskID: "..foo", wantSub: "task ID"},
		{name: "task triple dot prefix", suiteID: "ok", taskID: "...evil", wantSub: "task ID"},
		{name: "task dotdot prefix hidden", suiteID: "ok", taskID: "..hidden", wantSub: "task ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suite := types.EvalSuite{
				ID:    tc.suiteID,
				Tasks: []types.EvalTask{{ID: tc.taskID, Prompt: "p"}},
			}
			_, err := RunSuite(context.Background(), suite, RunConfig{OutputDir: t.TempDir()})
			if err == nil {
				t.Fatal("expected validation error for traversal ID")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
			// The error payload must not leak a path that escaped the sandbox.
			if strings.Contains(err.Error(), fmt.Sprintf("%c..%c", os.PathSeparator, os.PathSeparator)) {
				t.Errorf("error contains escaped path traversal: %q", err.Error())
			}
		})
	}
}

// TestRunSuite_ContextCancellation pins the dispatcher's ctx.Done() drain
// branch: when the context is cancelled mid-suite, the runner returns
// promptly, every result slot is populated (no zero-value TaskResults),
// and un-dispatched tasks are recorded as outcome="error" carrying the
// ctx.Err() message.
func TestRunSuite_ContextCancellation(t *testing.T) {
	// Sleeps 500ms so the dispatcher is guaranteed to be blocked on
	// `jobs <-` for the un-dispatched tail when cancellation fires.
	script := `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
sleep 0.5
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`
	harness := writeFakeHarness(t, script)

	tasks := make([]types.EvalTask, 8)
	for i := range tasks {
		tasks[i] = types.EvalTask{
			ID:     fmt.Sprintf("c%d", i),
			Prompt: "p",
			Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
		}
	}
	suite := types.EvalSuite{ID: "cancel-suite", Tasks: tasks}

	ctx, cancel := context.WithCancel(context.Background())
	// With concurrency=2 and a 500ms per-task sleep, only the first 2
	// tasks dispatch before cancel; the remaining 6 take the drain branch.
	time.AfterFunc(50*time.Millisecond, cancel)

	type runOutcome struct {
		result []eval.TaskResult
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		res, err := RunSuite(ctx, suite, RunConfig{
			HarnessPath: harness,
			Concurrency: 2,
		})
		done <- runOutcome{result: res.Tasks, err: err}
	}()

	var outcome runOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunSuite did not return after context cancellation (deadlock?)")
	}

	if outcome.err != nil {
		t.Fatalf("RunSuite returned suite-level error after cancel: %v", outcome.err)
	}

	if len(outcome.result) != len(tasks) {
		t.Fatalf("got %d task results, want %d (drain must populate every slot)",
			len(outcome.result), len(tasks))
	}
	for i, tr := range outcome.result {
		// Zero-value TaskResult would have empty TaskID and empty Outcome.
		// Either branch (worker or drain) must produce a non-zero result.
		if tr.TaskID == "" {
			t.Errorf("Tasks[%d] has empty TaskID — drain left a zero-value slot", i)
		}
		if tr.Outcome == "" {
			t.Errorf("Tasks[%d] (%s) has empty Outcome — slot was never written", i, tr.TaskID)
		}
		if tr.TaskID != tasks[i].ID {
			t.Errorf("Tasks[%d].TaskID = %q, want %q (input order not preserved)",
				i, tr.TaskID, tasks[i].ID)
		}
	}

	// The drain path must have flagged at least one task as outcome="error"
	// with the cancellation cause. (Tasks dispatched before cancel may
	// finish either way depending on whether the harness exec returns
	// before or after ctx cancel propagates; only the drain branch is
	// deterministic about producing error outcomes.)
	errCount := 0
	for _, tr := range outcome.result {
		if tr.Outcome == "error" && strings.Contains(tr.Error, "context canceled") {
			errCount++
		}
	}
	if errCount == 0 {
		t.Errorf("expected at least one task to record context-canceled error; got outcomes: %v",
			collectOutcomes(outcome.result))
	}
}

func collectOutcomes(results []eval.TaskResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = fmt.Sprintf("%s=%s", r.TaskID, r.Outcome)
	}
	return out
}

// TestAppendAnthropicWIFArgs pins the on-the-wire flag spellings the runner
// forwards to `stirrup harness`; these must match the harness side
// (harness/cmd/stirrup/cmd/runconfigflags.go).
func TestAppendAnthropicWIFArgs(t *testing.T) {
	tests := []struct {
		name string
		wif  AnthropicWIFConfig
		want []string
	}{
		{
			name: "empty config emits nothing",
			wif:  AnthropicWIFConfig{},
			want: nil,
		},
		{
			name: "full config emits all four flags in canonical order",
			wif: AnthropicWIFConfig{
				FederationRuleID:  "fdrl_test",
				OrganizationID:    "org-uuid",
				ServiceAccountID:  "svac_test",
				FromGitHubActions: true,
			},
			want: []string{
				"--anthropic-federation-rule-id", "fdrl_test",
				"--anthropic-organization-id", "org-uuid",
				"--anthropic-service-account-id", "svac_test",
				"--anthropic-from-github-actions",
			},
		},
		{
			name: "boolean-only toggle — no identifiers set, only the flag forwarded",
			wif: AnthropicWIFConfig{
				FromGitHubActions: true,
			},
			want: []string{"--anthropic-from-github-actions"},
		},
		{
			name: "partial config forwards what is set (harness produces the error)",
			wif: AnthropicWIFConfig{
				FederationRuleID: "fdrl_only",
			},
			want: []string{"--anthropic-federation-rule-id", "fdrl_only"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := appendAnthropicWIFArgs(nil, tc.wif)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("arg[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRunSuite_ForwardsAnthropicWIFFlags exercises the runner end-to-end
// with a fake harness that captures its argv to disk, pinning that a
// configured AnthropicWIFConfig reaches the subprocess argv (and not merely
// the RunConfig struct).
func TestRunSuite_ForwardsAnthropicWIFFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")
	argvCapture := filepath.Join(harnessDir, "argv.txt")

	// Records argv (one arg per line) to a predetermined path and writes a
	// minimal success trace so the runner reaches artifact-retention.
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a"; done > %q
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.01,"outcome":"success"}' > "$TRACE"
fi
`, argvCapture)
	if err := os.WriteFile(harnessPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// The judge type is irrelevant: the harness captures argv before the
	// runner invokes a judge; only the captured argv is asserted on.
	suite := types.EvalSuite{
		ID: "wif-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "wif-task",
				Prompt: "noop",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"unused-by-wif-test.txt"},
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		AnthropicWIF: AnthropicWIFConfig{
			FederationRuleID:  "fdrl_01TEST",
			OrganizationID:    "00000000-0000-0000-0000-000000000000",
			ServiceAccountID:  "svac_01TEST",
			FromGitHubActions: true,
		},
	})
	if err != nil {
		t.Fatalf("RunSuite returned error: %v", err)
	}

	data, err := os.ReadFile(argvCapture)
	if err != nil {
		t.Fatalf("reading captured argv: %v", err)
	}
	captured := string(data)
	wantFragments := []string{
		"--anthropic-federation-rule-id",
		"fdrl_01TEST",
		"--anthropic-organization-id",
		"00000000-0000-0000-0000-000000000000",
		"--anthropic-service-account-id",
		"svac_01TEST",
		"--anthropic-from-github-actions",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(captured, frag+"\n") {
			t.Errorf("captured argv missing %q\nfull capture:\n%s", frag, captured)
		}
	}
}

// TestRunSuite_ForwardsModelFlag pins that RunConfig.Model reaches the
// harness argv as --model.
func TestRunSuite_ForwardsModelFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")
	argvCapture := filepath.Join(harnessDir, "argv.txt")

	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a"; done > %q
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.01,"outcome":"success"}' > "$TRACE"
fi
`, argvCapture)
	if err := os.WriteFile(harnessPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	suite := types.EvalSuite{
		ID: "model-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "model-task",
				Prompt: "noop",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"unused-by-model-test.txt"},
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		Model:       "claude-haiku-4-5-20251001",
	})
	if err != nil {
		t.Fatalf("RunSuite returned error: %v", err)
	}

	data, err := os.ReadFile(argvCapture)
	if err != nil {
		t.Fatalf("reading captured argv: %v", err)
	}
	captured := string(data)
	for _, frag := range []string{"--model", "claude-haiku-4-5-20251001"} {
		if !strings.Contains(captured, frag+"\n") {
			t.Errorf("captured argv missing %q\nfull capture:\n%s", frag, captured)
		}
	}
}

// TestRunSuite_NoModelFlagWhenUnset pins that an empty RunConfig.Model does
// not emit --model at all.
func TestRunSuite_NoModelFlagWhenUnset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")
	argvCapture := filepath.Join(harnessDir, "argv.txt")

	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a"; done > %q
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.01,"outcome":"success"}' > "$TRACE"
fi
`, argvCapture)
	if err := os.WriteFile(harnessPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	suite := types.EvalSuite{
		ID: "no-model-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "no-model-task",
				Prompt: "noop",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"unused-by-model-test.txt"},
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
	})
	if err != nil {
		t.Fatalf("RunSuite returned error: %v", err)
	}

	data, err := os.ReadFile(argvCapture)
	if err != nil {
		t.Fatalf("reading captured argv: %v", err)
	}
	if strings.Contains(string(data), "--model\n") {
		t.Errorf("captured argv unexpectedly contains --model:\n%s", string(data))
	}
}

// TestRunSuite_ForwardsPromptModelFlag pins that RunConfig.PromptModel
// reaches the harness argv as --prompt-model.
func TestRunSuite_ForwardsPromptModelFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")
	argvCapture := filepath.Join(harnessDir, "argv.txt")

	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a"; done > %q
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.01,"outcome":"success"}' > "$TRACE"
fi
`, argvCapture)
	if err := os.WriteFile(harnessPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	suite := types.EvalSuite{
		ID: "prompt-model-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "prompt-model-task",
				Prompt: "noop",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"unused-by-prompt-model-test.txt"},
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		Model:       "claude-fable-6",
		PromptModel: "claude-fable-5",
	})
	if err != nil {
		t.Fatalf("RunSuite returned error: %v", err)
	}

	data, err := os.ReadFile(argvCapture)
	if err != nil {
		t.Fatalf("reading captured argv: %v", err)
	}
	captured := string(data)
	for _, frag := range []string{"--model", "claude-fable-6", "--prompt-model", "claude-fable-5"} {
		if !strings.Contains(captured, frag+"\n") {
			t.Errorf("captured argv missing %q\nfull capture:\n%s", frag, captured)
		}
	}
}

// An empty RunConfig.PromptModel must not emit --prompt-model at all,
// mirroring TestRunSuite_NoModelFlagWhenUnset.
func TestRunSuite_NoPromptModelFlagWhenUnset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-harness")
	argvCapture := filepath.Join(harnessDir, "argv.txt")

	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a"; done > %q
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.01,"outcome":"success"}' > "$TRACE"
fi
`, argvCapture)
	if err := os.WriteFile(harnessPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	suite := types.EvalSuite{
		ID: "no-prompt-model-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "no-prompt-model-task",
				Prompt: "noop",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"unused-by-prompt-model-test.txt"},
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
	})
	if err != nil {
		t.Fatalf("RunSuite returned error: %v", err)
	}

	data, err := os.ReadFile(argvCapture)
	if err != nil {
		t.Fatalf("reading captured argv: %v", err)
	}
	if strings.Contains(string(data), "--prompt-model\n") {
		t.Errorf("captured argv unexpectedly contains --prompt-model:\n%s", string(data))
	}
}
