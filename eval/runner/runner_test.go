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
	// Stronger assertion: with a 300ms sleep on t1 and 10–200ms on the
	// others, t1 must finish *last* under concurrency=4. A non-last t1
	// would mean a faster task got dispatched after t1 yet still finished
	// later — which is impossible without contention we don't introduce.
	// This catches regressions where the scheduler drops to effective
	// concurrency=1 but the first dispatch still happened to be a fast
	// task (which would satisfy the not-first check).
	last := completionOrder[len(completionOrder)-1]
	if last != "t1" {
		t.Errorf("expected t1 (300ms sleep) to finish last under concurrency=4; got %q; full order: %v",
			last, completionOrder)
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
		// `..`-prefixed IDs that are not exactly ".." must still be rejected,
		// otherwise an attacker could land artifacts in directories the runner
		// never intended (e.g. "..foo" on a hostile filesystem). These cover
		// the HasPrefix(id, "..") branch of validatePathSegment specifically.
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
			// Belt-and-braces: the error must come back BEFORE any directory
			// creation under OutputDir. We verify the error payload does not
			// leak a path that escaped the sandbox.
			if strings.Contains(err.Error(), fmt.Sprintf("%c..%c", os.PathSeparator, os.PathSeparator)) {
				t.Errorf("error contains escaped path traversal: %q", err.Error())
			}
		})
	}
}

// TestRunSuite_ContextCancellation exercises the dispatcher's ctx.Done()
// drain branch: when the context is cancelled mid-suite, the runner must
// (a) return promptly without deadlocking, (b) return a result slice with
// every slot populated (no zero-value TaskResults), and (c) record at
// least the un-dispatched tasks as outcome="error" carrying the
// ctx.Err() message. In-flight workers continue writing into their own
// disjoint slots, so all slots end up populated by exactly one writer
// (drain or worker) — confirming the ownership invariant the
// implementation comment relies on.
//
// Without this test the goroutine-leak prevention path has zero coverage
// and a regression that, for example, miscounted the drain start index
// (leaving zero-value slots) or dropped wg.Wait() (leaking workers
// writing into a returned slice) would not be caught.
func TestRunSuite_ContextCancellation(t *testing.T) {
	// A harness that sleeps 500ms so the dispatcher is guaranteed to be
	// blocked on `jobs <-` for the un-dispatched tail when cancellation
	// fires.
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
	// Fire cancellation 50ms after start. With concurrency=2 and a 500ms
	// per-task sleep, only the first 2 tasks have a chance to dispatch
	// before cancel; the remaining 6 must take the drain branch.
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

// configRecordingHarness writes a fake harness that records the
// `--config` path it was invoked with into argsLog and copies the
// config file's bytes into a parallel file at configLog so tests can
// inspect what the runner handed the binary. The harness still writes
// a minimal trace file so the judge has something to chew on.
func configRecordingHarness(t *testing.T, argsLog, configLog string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
CONFIG=""
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; echo "config=$2" >> %q; shift 2 ;;
    --trace)  TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$CONFIG" ] && [ -f "$CONFIG" ]; then
  cat "$CONFIG" > %q
fi
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`, argsLog, configLog)
	return writeFakeHarness(t, script)
}

func intPtr(v int) *int { return &v }

// TestRunSuite_MergedConfigPassedToHarness verifies that a suite with an
// inline RunConfig baseline plus a task override produces a per-task
// JSON config file at a path the runner hands the harness via --config,
// and that the file deserialises into the expected merged shape (task
// override layered on top of the suite baseline).
func TestRunSuite_MergedConfigPassedToHarness(t *testing.T) {
	logDir := t.TempDir()
	argsLog := filepath.Join(logDir, "args.log")
	configLog := filepath.Join(logDir, "config.json")
	harness := configRecordingHarness(t, argsLog, configLog)

	suite := types.EvalSuite{
		ID: "merged-config-suite",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				Mode: "execution",
				Provider: &types.ProviderConfig{
					Type:      "openai-responses",
					APIKeyRef: "secret://OPENAI_KEY",
				},
				MaxTurns: intPtr(8),
			},
		},
		Tasks: []types.EvalTask{
			{
				ID:     "t1",
				Prompt: "do thing",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
				RunConfigOverrides: &types.RunConfigOverrides{
					MaxTurns: intPtr(4),
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	argsContent, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading args log: %v", err)
	}
	if !strings.Contains(string(argsContent), "config=") {
		t.Fatalf("args log did not record --config invocation: %q", string(argsContent))
	}

	configContent, err := os.ReadFile(configLog)
	if err != nil {
		t.Fatalf("reading config log: %v", err)
	}
	var got types.RunConfig
	if err := json.Unmarshal(configContent, &got); err != nil {
		t.Fatalf("config JSON not deserialisable: %v\n%s", err, string(configContent))
	}
	if got.Mode != "execution" {
		t.Errorf("Mode = %q, want %q", got.Mode, "execution")
	}
	if got.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want %q", got.Provider.Type, "openai-responses")
	}
	if got.Provider.APIKeyRef != "secret://OPENAI_KEY" {
		t.Errorf("Provider.APIKeyRef = %q, want %q", got.Provider.APIKeyRef, "secret://OPENAI_KEY")
	}
	// Task override must win over the suite baseline (4 over 8).
	if got.MaxTurns != 4 {
		t.Errorf("MaxTurns = %d, want 4 (task override should win)", got.MaxTurns)
	}
}

// TestRunSuite_SuiteLevelModePinned pins the precedence contract for
// mode: a suite that bakes mode into a file-based RunConfig baseline
// must not have it silently shadowed by a default --mode flag injected
// by the runner. The harness's applyOverrides treats any flag set on
// the command line as authoritative, so passing --mode unconditionally
// would defeat the suite-level baseline. The fix is to only forward
// --mode when the task pins its own Mode.
func TestRunSuite_SuiteLevelModePinned(t *testing.T) {
	logDir := t.TempDir()
	argsLog := filepath.Join(logDir, "args.log")
	configLog := filepath.Join(logDir, "config.json")
	harness := configRecordingHarness(t, argsLog, configLog)

	// Use a file-based baseline so the merged config carries Mode =
	// "planning" before validation. Inline run_config { mode = ... } is
	// no longer surfaced in the HCL grammar (B-1 fix).
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "base.json")
	timeout := 120
	baseline := types.RunConfig{
		Mode: "planning",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://K",
		},
		MaxTurns: 4,
		Timeout:  &timeout,
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatalf("marshalling baseline: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatalf("writing baseline file: %v", err)
	}

	suite := types.EvalSuite{
		ID:        "mode-pinned-suite",
		RunConfig: &types.RunConfigSource{File: cfgPath},
		Tasks: []types.EvalTask{
			// Task has NO Mode set; the suite-level baseline must win.
			{
				ID:     "t1",
				Prompt: "p",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
			},
		},
	}

	_, err = RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The fake harness in configRecordingHarness only records --config.
	// To verify --mode is absent we re-run the assertion with a wider
	// recorder. Instead, read the config the runner wrote: it should
	// carry Mode = "planning". And separately, assert by reading the
	// args log that we *do* see --config (so the path was set) without
	// any --mode pair.
	argsContent, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading args log: %v", err)
	}
	if !strings.Contains(string(argsContent), "config=") {
		t.Fatalf("args log did not record --config invocation: %q", string(argsContent))
	}

	configContent, err := os.ReadFile(configLog)
	if err != nil {
		t.Fatalf("reading config log: %v", err)
	}
	var got types.RunConfig
	if err := json.Unmarshal(configContent, &got); err != nil {
		t.Fatalf("config JSON not deserialisable: %v\n%s", err, string(configContent))
	}
	if got.Mode != "planning" {
		t.Errorf("Mode = %q, want %q (suite baseline must reach harness)", got.Mode, "planning")
	}

	// Verify --mode is absent from the args. Use a wider-recording
	// harness this time that captures every arg.
	wideArgs := filepath.Join(logDir, "wide-args.log")
	wideScript := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do echo "$a" >> %q; done
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`, wideArgs)
	wideHarness := writeFakeHarness(t, wideScript)
	_, err = RunSuite(context.Background(), suite, RunConfig{HarnessPath: wideHarness})
	if err != nil {
		t.Fatalf("unexpected error (wide harness): %v", err)
	}
	wideContent, err := os.ReadFile(wideArgs)
	if err != nil {
		t.Fatalf("reading wide args log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(wideContent)), "\n") {
		if strings.TrimSpace(line) == "--mode" {
			t.Errorf("--mode flag must not appear when task has no explicit Mode; full args:\n%s", string(wideContent))
		}
	}
}

// TestRunSuite_TaskLevelModeForwarded asserts the inverse of
// TestRunSuite_SuiteLevelModePinned: a task with an explicit Mode
// still has --mode forwarded so per-task overrides keep working.
func TestRunSuite_TaskLevelModeForwarded(t *testing.T) {
	logDir := t.TempDir()
	argsLog := filepath.Join(logDir, "args.log")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do echo "$a" >> %q; done
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`, argsLog)
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "task-mode-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "t1",
				Mode:   "review",
				Prompt: "p",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
			},
		},
	}
	_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading args log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	sawMode := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "--mode" {
			sawMode = true
			if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "review" {
				t.Errorf("--mode value = %q, want %q; full args:\n%s",
					func() string {
						if i+1 < len(lines) {
							return lines[i+1]
						}
						return "<missing>"
					}(),
					"review", string(content))
			}
		}
	}
	if !sawMode {
		t.Errorf("--mode flag must appear when task pins Mode; full args:\n%s", string(content))
	}
}

// TestRunSuite_FileBaselineLoaded verifies the suite.RunConfig.File
// path is read by the runner, parsed as JSON, and used as the merged
// config baseline. Without task overrides the merged config must equal
// the file's contents verbatim.
func TestRunSuite_FileBaselineLoaded(t *testing.T) {
	logDir := t.TempDir()
	argsLog := filepath.Join(logDir, "args.log")
	configLog := filepath.Join(logDir, "config.json")
	harness := configRecordingHarness(t, argsLog, configLog)

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "base.json")
	timeout := 120
	baseline := types.RunConfig{
		Mode: "execution",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://ANTHROPIC_KEY",
		},
		MaxTurns: 6,
		Timeout:  &timeout,
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatalf("marshalling baseline: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatalf("writing baseline file: %v", err)
	}

	suite := types.EvalSuite{
		ID:        "file-baseline-suite",
		RunConfig: &types.RunConfigSource{File: cfgPath},
		Tasks: []types.EvalTask{
			{
				ID:     "t1",
				Prompt: "p",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
			},
		},
	}

	_, err = RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configContent, err := os.ReadFile(configLog)
	if err != nil {
		t.Fatalf("reading config log: %v", err)
	}
	var got types.RunConfig
	if err := json.Unmarshal(configContent, &got); err != nil {
		t.Fatalf("config JSON not deserialisable: %v", err)
	}
	if got.Mode != "execution" || got.Provider.Type != "anthropic" || got.MaxTurns != 6 {
		t.Errorf("file-baseline merge did not survive round-trip: %#v", got)
	}
}

// TestRunSuite_NoConfigSurfaceLegacyInvocation pins the backwards-compat
// contract: a suite with no RunConfig and no per-task overrides must NOT
// add --config to the args slice, and no run_config.redacted.json
// artifact may appear under OutputDir.
func TestRunSuite_NoConfigSurfaceLegacyInvocation(t *testing.T) {
	logDir := t.TempDir()
	argsLog := filepath.Join(logDir, "args.log")
	// Capture ALL args, not just --config, so we can assert --config is absent.
	script := fmt.Sprintf(`#!/bin/sh
echo "args:" >> %q
for a in "$@"; do echo "  $a" >> %q; done
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`, argsLog, argsLog)
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "legacy-suite",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	out := t.TempDir()
	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logBytes, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading args log: %v", err)
	}
	if strings.Contains(string(logBytes), "--config") {
		t.Errorf("args log contains --config for a no-config-surface suite: %q", string(logBytes))
	}

	// No artifact may exist on disk.
	artifact := filepath.Join(out, "legacy-suite", "t1", "run_config.redacted.json")
	if _, err := os.Stat(artifact); err == nil {
		t.Errorf("run_config.redacted.json should not exist for a no-config-surface suite (found at %s)", artifact)
	}
}

// TestRunSuite_RetainedArtifactRedacted pins the redaction guarantee:
// the persisted run_config.redacted.json must NOT contain the literal
// secret reference value and MUST contain the redaction sentinel.
func TestRunSuite_RetainedArtifactRedacted(t *testing.T) {
	logDir := t.TempDir()
	argsLog := filepath.Join(logDir, "args.log")
	configLog := filepath.Join(logDir, "config.json")
	harness := configRecordingHarness(t, argsLog, configLog)

	const realKeyRef = "secret://REAL_KEY_PLAINTEXT"
	suite := types.EvalSuite{
		ID: "redacted-suite",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				Mode: "execution",
				Provider: &types.ProviderConfig{
					Type:      "openai-responses",
					APIKeyRef: realKeyRef,
				},
			},
		},
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	out := t.TempDir()
	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	artifact := filepath.Join(out, "redacted-suite", "t1", "run_config.redacted.json")
	content, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("reading redacted artifact: %v", err)
	}
	if strings.Contains(string(content), "REAL_KEY_PLAINTEXT") {
		t.Errorf("redacted artifact leaked secret reference value:\n%s", string(content))
	}
	if !strings.Contains(string(content), "secret://[REDACTED]") {
		t.Errorf("redacted artifact missing redaction sentinel:\n%s", string(content))
	}

	// Belt-and-braces: the artifact is redacted, but the file the
	// harness actually received via --config must still carry the
	// original reference. A regression that fed the redacted form to
	// the harness would break startup at the SecretStore (the
	// SECRET://[REDACTED] env var would fail to resolve). The
	// configRecordingHarness copies the --config file's bytes into
	// configLog while the runner's tmpDir is still alive.
	handed, err := os.ReadFile(configLog)
	if err != nil {
		t.Fatalf("reading config handed to harness: %v", err)
	}
	var handedCfg types.RunConfig
	if err := json.Unmarshal(handed, &handedCfg); err != nil {
		t.Fatalf("config handed to harness is not valid JSON: %v\n%s", err, string(handed))
	}
	if handedCfg.Provider.APIKeyRef != realKeyRef {
		t.Errorf("harness --config Provider.APIKeyRef = %q, want %q (un-redacted reference)",
			handedCfg.Provider.APIKeyRef, realKeyRef)
	}

	// Also pin the audit artifact's permission bits (B-4): owner rw,
	// group r, world none.
	info, err := os.Stat(artifact)
	if err != nil {
		t.Fatalf("stat redacted artifact: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
		t.Errorf("redacted artifact mode = %#o, want %#o", got, want)
	}
}

// TestRunSuite_DryRunValidatesMergedConfig asserts that --dry-run runs
// ValidateRunConfig on the merged config and surfaces validator errors
// per task. We use a read-only mode paired with an allow-all permission
// policy, which ValidateRunConfig rejects.
func TestRunSuite_DryRunValidatesMergedConfig(t *testing.T) {
	suite := types.EvalSuite{
		ID: "dry-run-invalid-suite",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				Mode: "planning",
				Provider: &types.ProviderConfig{
					Type:      "anthropic",
					APIKeyRef: "secret://K",
				},
			},
		},
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	tr := result.Tasks[0]
	// Pin the exact outcome: "fail" is the validation-failure branch
	// (versus "error" for a merge failure). A regression that
	// conflated the two would silently break operator-facing
	// reporting; `!= "pass"` is too permissive to catch that.
	if tr.Outcome != "fail" {
		t.Errorf("Outcome = %q, want %q", tr.Outcome, "fail")
	}
	if !strings.Contains(tr.JudgeVerdict.Reason, "RunConfig validation failed") {
		t.Errorf("reason = %q, want it to mention RunConfig validation failed", tr.JudgeVerdict.Reason)
	}
}

// TestRunSuite_DryRunValidatesAndPasses pins the dry-run success
// outcome: a suite whose merged config validates cleanly produces a
// "pass" outcome with the dry-run reason. This is the most common
// production case for --dry-run; the existing dry-run tests only
// covered the no-config-surface and validation-failure paths.
func TestRunSuite_DryRunValidatesAndPasses(t *testing.T) {
	suite := types.EvalSuite{
		ID: "dry-run-valid-suite",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				Mode: "execution",
				Provider: &types.ProviderConfig{
					Type:      "openai-responses",
					APIKeyRef: "secret://OPENAI_KEY",
				},
				MaxTurns: intPtr(6),
			},
		},
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	tr := result.Tasks[0]
	if tr.Outcome != "pass" {
		t.Errorf("Outcome = %q, want %q", tr.Outcome, "pass")
	}
	if !strings.Contains(tr.JudgeVerdict.Reason, "RunConfig validated") {
		t.Errorf("Reason = %q, want it to mention RunConfig validated", tr.JudgeVerdict.Reason)
	}
	if result.PassRate != 1.0 {
		t.Errorf("PassRate = %f, want 1.0", result.PassRate)
	}
}

// TestRunSuite_DryRunMergeErrorSurfacedPerTask pins the dry-run
// "merge failed" branch: when mergeRunConfig itself returns an error
// (e.g. RunConfigSource.File points at a nonexistent file), the task
// outcome must be "error" with the merge failure surfaced as the
// judge reason. The branch was reachable but had no test.
func TestRunSuite_DryRunMergeErrorSurfacedPerTask(t *testing.T) {
	suite := types.EvalSuite{
		ID:        "dry-run-merge-err-suite",
		RunConfig: &types.RunConfigSource{File: filepath.Join(t.TempDir(), "does-not-exist.json")},
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	tr := result.Tasks[0]
	if tr.Outcome != "error" {
		t.Errorf("Outcome = %q, want %q", tr.Outcome, "error")
	}
	if !strings.Contains(tr.JudgeVerdict.Reason, "merging run-config") {
		t.Errorf("Reason = %q, want it to mention merging run-config", tr.JudgeVerdict.Reason)
	}
}

// TestRunSuite_DryRunMixedTaskConfigSurface pins the "merged == nil"
// vacuous-pass branch: when the suite has no suite-level RunConfig,
// task-1 has RunConfigOverrides, and task-2 has none, task-2's
// merge returns (nil, nil) and the dry-run path emits a vacuous
// pass with the "skipped (no merged config)" reason. The branch was
// reachable but untested.
func TestRunSuite_DryRunMixedTaskConfigSurface(t *testing.T) {
	suite := types.EvalSuite{
		ID: "dry-run-mixed-suite",
		// No suite-level RunConfig — hasConfigSurface is driven by task-1.
		Tasks: []types.EvalTask{
			{
				ID:     "t1",
				Prompt: "p",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
				RunConfigOverrides: &types.RunConfigOverrides{
					Mode:     "execution",
					Provider: &types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://K"},
					MaxTurns: intPtr(4),
				},
			},
			{
				ID:     "t2",
				Prompt: "p",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
				// No RunConfigOverrides; merged will be (nil, nil).
			},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(result.Tasks))
	}

	// task-1: merged config validates, outcome pass with "RunConfig validated".
	t1 := result.Tasks[0]
	if t1.TaskID != "t1" {
		t.Fatalf("Tasks[0].TaskID = %q, want t1", t1.TaskID)
	}
	if t1.Outcome != "pass" {
		t.Errorf("t1.Outcome = %q, want pass", t1.Outcome)
	}
	if !strings.Contains(t1.JudgeVerdict.Reason, "RunConfig validated") {
		t.Errorf("t1.Reason = %q, want it to mention RunConfig validated", t1.JudgeVerdict.Reason)
	}

	// task-2: no overrides, merged nil, vacuous pass with "skipped".
	t2 := result.Tasks[1]
	if t2.TaskID != "t2" {
		t.Fatalf("Tasks[1].TaskID = %q, want t2", t2.TaskID)
	}
	if t2.Outcome != "pass" {
		t.Errorf("t2.Outcome = %q, want pass", t2.Outcome)
	}
	if !strings.Contains(t2.JudgeVerdict.Reason, "skipped") {
		t.Errorf("t2.Reason = %q, want it to mention skipped", t2.JudgeVerdict.Reason)
	}
}

// TestRunSuite_DryRunInlineConfigWithNoTimeout pins the contract that
// an inline run_config block — which cannot carry a timeout (the field
// is intentionally not surfaced in the HCL grammar) — validates
// successfully under --dry-run. The runner injects its own default
// timeout before validation so suites that don't express a timeout
// (i.e. every inline-block suite) don't falsely fail dry-run.
func TestRunSuite_DryRunInlineConfigWithNoTimeout(t *testing.T) {
	suite := types.EvalSuite{
		ID: "dry-run-no-timeout-suite",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				// Mode is set via the carrier here for the programmatic
				// test; the HCL grammar no longer surfaces it (B-1).
				Mode: "execution",
				Provider: &types.ProviderConfig{
					Type:      "anthropic",
					APIKeyRef: "secret://K",
				},
				MaxTurns: intPtr(4),
			},
		},
		Tasks: []types.EvalTask{
			{
				ID:     "t1",
				Mode:   "execution",
				Prompt: "p",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
			},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	tr := result.Tasks[0]
	if tr.Outcome != "pass" {
		t.Errorf("Outcome = %q, want %q (dry-run must inject default timeout)", tr.Outcome, "pass")
	}
	if !strings.Contains(tr.JudgeVerdict.Reason, "RunConfig validated") {
		t.Errorf("Reason = %q, want it to mention RunConfig validated", tr.JudgeVerdict.Reason)
	}
}

// TestRunSuite_DryRunNoConfigStillSkipsAsPass pins today's behaviour for
// suites that have no run-config surface — dry-run must remain a vacuous
// pass for every task so existing CI invocations keep their semantics.
func TestRunSuite_DryRunNoConfigStillSkipsAsPass(t *testing.T) {
	suite := types.EvalSuite{
		ID: "dry-run-legacy",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p"},
			{ID: "t2", Prompt: "p"},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tr := range result.Tasks {
		if tr.Outcome != "pass" {
			t.Errorf("task %s: outcome = %q, want pass (no-config-surface dry-run regression)", tr.TaskID, tr.Outcome)
		}
		if !strings.Contains(tr.JudgeVerdict.Reason, "skipped") {
			t.Errorf("task %s: reason = %q, want it to mention skipped", tr.TaskID, tr.JudgeVerdict.Reason)
		}
	}
	if result.PassRate != 1.0 {
		t.Errorf("PassRate = %f, want 1.0", result.PassRate)
	}
}

// TestMergeRunConfig_NoSurfaceReturnsNil pins the (nil, nil) contract:
// the runner uses that signal to skip --config entirely and preserve
// backwards compat with pre-#177 suites.
func TestMergeRunConfig_NoSurfaceReturnsNil(t *testing.T) {
	suite := types.EvalSuite{ID: "s", Tasks: []types.EvalTask{{ID: "t1"}}}
	got, err := mergeRunConfig(suite, suite.Tasks[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %#v, want nil", got)
	}
}

// TestMergeRunConfig_InlineBaseline asserts that the suite-level
// inline overrides materialise into a non-nil merged config with all
// sparse fields populated (provider, model router, context strategy,
// edit strategy, verifier, mode, max-turns).
func TestMergeRunConfig_InlineBaseline(t *testing.T) {
	suite := types.EvalSuite{
		ID: "s",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				Mode:            "execution",
				Provider:        &types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://K"},
				ModelRouter:     &types.ModelRouterConfig{Type: "static", Model: "gpt-5.4-nano"},
				ContextStrategy: &types.ContextStrategyConfig{Type: "full-history"},
				EditStrategy:    &types.EditStrategyConfig{Type: "string-replace"},
				Verifier:        &types.VerifierConfig{Type: "noop"},
				MaxTurns:        intPtr(12),
			},
		},
		Tasks: []types.EvalTask{{ID: "t1"}},
	}
	got, err := mergeRunConfig(suite, suite.Tasks[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want a populated merged config")
	}
	if got.Mode != "execution" {
		t.Errorf("Mode = %q", got.Mode)
	}
	if got.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q", got.Provider.Type)
	}
	if got.ModelRouter.Model != "gpt-5.4-nano" {
		t.Errorf("ModelRouter.Model = %q", got.ModelRouter.Model)
	}
	if got.ContextStrategy.Type != "full-history" {
		t.Errorf("ContextStrategy.Type = %q", got.ContextStrategy.Type)
	}
	if got.EditStrategy.Type != "string-replace" {
		t.Errorf("EditStrategy.Type = %q", got.EditStrategy.Type)
	}
	if got.Verifier.Type != "noop" {
		t.Errorf("Verifier.Type = %q", got.Verifier.Type)
	}
	if got.MaxTurns != 12 {
		t.Errorf("MaxTurns = %d, want 12", got.MaxTurns)
	}
}

// TestMergeRunConfig_TaskOverridesPreserveBaseline asserts that a task
// that sets only one field leaves the suite's other baseline fields
// intact — sparse semantics, not whole-record replacement.
func TestMergeRunConfig_TaskOverridesPreserveBaseline(t *testing.T) {
	suite := types.EvalSuite{
		ID: "s",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				Mode:     "execution",
				Provider: &types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://K"},
				MaxTurns: intPtr(10),
			},
		},
		Tasks: []types.EvalTask{
			{
				ID: "t1",
				RunConfigOverrides: &types.RunConfigOverrides{
					MaxTurns: intPtr(3),
				},
			},
		},
	}
	got, err := mergeRunConfig(suite, suite.Tasks[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mode != "execution" {
		t.Errorf("Mode = %q, want baseline preserved", got.Mode)
	}
	if got.Provider.Type != "openai-responses" {
		t.Errorf("Provider = %#v, want baseline preserved", got.Provider)
	}
	if got.MaxTurns != 3 {
		t.Errorf("MaxTurns = %d, want task override 3", got.MaxTurns)
	}
}

// TestMergeRunConfig_NilOverridesPreserveBaseline pins the sparse
// semantics for nil-pointer fields: a task override with all-nil
// pointer fields must not zero out the suite baseline.
func TestMergeRunConfig_NilOverridesPreserveBaseline(t *testing.T) {
	suite := types.EvalSuite{
		ID: "s",
		RunConfig: &types.RunConfigSource{
			Inline: &types.RunConfigOverrides{
				Mode:     "execution",
				Provider: &types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://K"},
				MaxTurns: intPtr(7),
			},
		},
		Tasks: []types.EvalTask{
			{ID: "t1", RunConfigOverrides: &types.RunConfigOverrides{}}, // empty; all nil
		},
	}
	got, err := mergeRunConfig(suite, suite.Tasks[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mode != "execution" || got.Provider.Type != "anthropic" || got.MaxTurns != 7 {
		t.Errorf("baseline not preserved: %#v", got)
	}
}

// TestMergeRunConfig_FileBaselineRoundTrip asserts that a JSON file
// referenced by suite.RunConfig.File is read and parsed verbatim.
func TestMergeRunConfig_FileBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "base.json")
	timeout := 60
	baseline := types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "gemini", APIKeyRef: "secret://G"},
		MaxTurns: 5,
		Timeout:  &timeout,
	}
	data, _ := json.Marshal(baseline)
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	suite := types.EvalSuite{
		ID:        "s",
		RunConfig: &types.RunConfigSource{File: cfgPath},
		Tasks:     []types.EvalTask{{ID: "t1"}},
	}
	got, err := mergeRunConfig(suite, suite.Tasks[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mode != "execution" || got.Provider.Type != "gemini" || got.MaxTurns != 5 {
		t.Errorf("file baseline not round-tripped: %#v", got)
	}
}

// TestMergeRunConfig_FileBaselineMissingFile surfaces a clear error
// when the file referenced by suite.RunConfig.File cannot be opened.
func TestMergeRunConfig_FileBaselineMissingFile(t *testing.T) {
	suite := types.EvalSuite{
		ID:        "s",
		RunConfig: &types.RunConfigSource{File: filepath.Join(t.TempDir(), "missing.json")},
		Tasks:     []types.EvalTask{{ID: "t1"}},
	}
	_, err := mergeRunConfig(suite, suite.Tasks[0])
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "loading suite run-config file") {
		t.Errorf("error = %q, want it to mention loading", err.Error())
	}
}

// TestMergeRunConfig_FileBaselineRejectsUnknownFields ensures we keep
// the harness's strict-decoding contract: a typo in the config file
// surfaces as an error rather than being silently dropped.
func TestMergeRunConfig_FileBaselineRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bogus.json")
	// `runId` is a valid field, but `runIdTypo` is not.
	if err := os.WriteFile(cfgPath, []byte(`{"mode":"execution","runIdTypo":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	suite := types.EvalSuite{
		ID:        "s",
		RunConfig: &types.RunConfigSource{File: cfgPath},
		Tasks:     []types.EvalTask{{ID: "t1"}},
	}
	_, err := mergeRunConfig(suite, suite.Tasks[0])
	if err == nil {
		t.Fatal("expected error for unknown field in config JSON")
	}
}

// TestMergeRunConfig_TaskOverridesOnlyNoBaseline asserts that a task
// with overrides but no suite-level baseline still produces a non-nil
// merged config built from a zero-value RunConfig.
func TestMergeRunConfig_TaskOverridesOnlyNoBaseline(t *testing.T) {
	suite := types.EvalSuite{
		ID: "s",
		Tasks: []types.EvalTask{
			{
				ID: "t1",
				RunConfigOverrides: &types.RunConfigOverrides{
					Mode:     "execution",
					Provider: &types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://K"},
					MaxTurns: intPtr(2),
				},
			},
		},
	}
	got, err := mergeRunConfig(suite, suite.Tasks[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want a populated merged config from task-only overrides")
	}
	if got.Mode != "execution" || got.Provider.Type != "anthropic" || got.MaxTurns != 2 {
		t.Errorf("task-only overrides not materialised: %#v", got)
	}
}
