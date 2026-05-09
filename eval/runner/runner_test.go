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

// writeFakeHarness writes a POSIX-shell harness double that records its
// invocation order, optionally sleeps for a per-task amount, optionally
// exits non-zero, and writes a minimal trace file. It returns the harness
// path and an "order log" path the caller can inspect to see which task
// the harness was last called with. Skips on non-Unix runners since
// /bin/sh is not portable to Windows.
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

// TestRunSuite_ConcurrencyOrdersDeterministically verifies the worker pool
// preserves suite task order in the returned SuiteResult.Tasks slice
// regardless of which workers actually finished first. The fake harness
// records a completion-order log keyed by task ID; the assertion is on
// (a) the result-slice order matching the input order, and (b) the
// completion-order log being a non-input order — i.e. concurrency
// genuinely happened. We avoid wall-clock thresholds because they're
// notoriously flaky under load (issue #31 review).
func TestRunSuite_ConcurrencyOrdersDeterministically(t *testing.T) {
	logDir := t.TempDir()
	completionLog := filepath.Join(logDir, "completion.log")

	// Per-task sleep is decoded from the prompt; the harness appends the
	// task ID to completionLog atomically (using a tempfile rename trick is
	// unnecessary because we only need the *finishing order* of distinct
	// IDs, and POSIX guarantees small (<= PIPE_BUF) appends are atomic).
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

	// Sleeps are chosen so that finishing order strictly differs from input
	// order under any realistic concurrency level: the longest sleep is on
	// the first task so a sequential run would put t1 first in the
	// completion log, but a concurrent run will not.
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

	// Concurrency must have actually happened — the completion order must
	// differ from the input order. With concurrency=4 and t1 sleeping
	// longest, t1 cannot be first in the completion log on any machine
	// that's not pathologically slow.
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
}

// TestRunSuite_ConcurrencyZeroDefaultsToOne pins the documented behaviour
// that Concurrency<=0 collapses to a sequential run. The test passes when
// the run completes successfully; a regression that, for example, blocked
// on a zero-buffered channel with no workers would deadlock here.
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
		// expected
	case <-time.After(10 * time.Second):
		t.Fatal("RunSuite with Concurrency=0 deadlocked")
	}
}

// TestRunSuite_FailureDoesNotAbortSiblings verifies the per-task error
// containment invariant: if one task's harness returns non-zero (or its
// trace is malformed), the other tasks still produce TaskResults and the
// suite result still surfaces all of them.
func TestRunSuite_FailureDoesNotAbortSiblings(t *testing.T) {
	// The fake harness fails iff prompt == "boom", otherwise writes a valid trace.
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

		// Trace content should be the harness's last JSON line.
		traceData, err := os.ReadFile(filepath.Join(base, "trace.jsonl"))
		if err == nil && !strings.Contains(string(traceData), `"id":"trace-1"`) {
			t.Errorf("trace.jsonl for %s did not contain expected payload: %q", taskID, string(traceData))
		}
		// Stdout/stderr files should contain the chatter we emitted.
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

// TestRunSuite_RejectsTraversalIDs is the load-bearing security test: any
// suite/task ID that would resolve outside <OutputDir>/<suiteID>/<taskID>/
// must be rejected at validation time so the runner never attempts the
// MkdirAll. We pick the rejection strategy (over silent sanitisation)
// because silently rewriting an attacker-controlled ID into a different
// path would shadow legitimate IDs and produce nondeterministic artifact
// trees.
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
			// Belt-and-braces: the error must come back BEFORE any directory
			// creation under OutputDir. We verify the error payload does not
			// leak a path that escaped the sandbox.
			if strings.Contains(err.Error(), fmt.Sprintf("%c..%c", os.PathSeparator, os.PathSeparator)) {
				t.Errorf("error contains escaped path traversal: %q", err.Error())
			}
		})
	}
}
