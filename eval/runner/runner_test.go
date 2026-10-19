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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunSuite_RejectsSuiteWithoutTasks(t *testing.T) {
	_, err := RunSuite(context.Background(), types.EvalSuite{ID: "s1"}, RunConfig{})
	require.EqualError(t, err, "suite must contain at least one task")
}

func TestRunSuite_RejectsMissingSuiteID(t *testing.T) {
	suite := types.EvalSuite{
		Tasks: []types.EvalTask{{ID: "t1", Prompt: "hi"}},
	}
	_, err := RunSuite(context.Background(), suite, RunConfig{})
	require.EqualError(t, err, "suite ID is required")
}

// validJudge satisfies suite validation for tests that never run the judge.
var validJudge = types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}

func TestRunSuite_DryRunPassesEveryTask(t *testing.T) {
	suite := types.EvalSuite{
		ID: "test-suite",
		Tasks: []types.EvalTask{
			{ID: "task-1", Prompt: "do something", Judge: validJudge},
			{ID: "task-2", Prompt: "do something else", Judge: validJudge},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	require.NoError(t, err)

	require.Equal(t, "test-suite", result.SuiteID)
	require.Len(t, result.Tasks, 2)
	for _, tr := range result.Tasks {
		t.Run(tr.TaskID, func(t *testing.T) {
			require.Equal(t, "pass", tr.Outcome)
			require.True(t, tr.JudgeVerdict.Passed, "verdict %+v", tr.JudgeVerdict)
			require.Equal(t, types.JudgeStatusPass, tr.JudgeVerdict.Status)
		})
	}
	require.InDelta(t, 1.0, result.PassRate, 1e-9)
}

func TestRunSuite_RunsTaskThroughFakeHarness(t *testing.T) {
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
	require.NoError(t, os.WriteFile(harnessPath, []byte(script), 0o755))

	workspaceContent := t.TempDir()
	targetFile := filepath.Join(workspaceContent, "output.txt")
	require.NoError(t, os.WriteFile(targetFile, []byte("hello world"), 0o644))

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
	require.NoError(t, err)

	require.Len(t, result.Tasks, 1)
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fake-harness"), []byte(script), 0o755))

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
	require.NoError(t, err)
	require.Len(t, result.Tasks, 1)
	got := result.Tasks[0]
	require.NotEqual(t, "error", got.Outcome,
		"task errored (relative harness path not resolved to absolute?): %s", got.Error)
	require.Equal(t, "pass", got.Outcome, "reason: %s", got.JudgeVerdict.Reason)
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
	require.NoError(t, os.WriteFile(harnessPath, []byte(script), 0o755))

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
	require.NoError(t, err)
	require.Equal(t, "pass", result.Tasks[0].Outcome, "seed file should exist for the judge")

	stdout, err := os.ReadFile(filepath.Join(outputDir, "seed-suite", "seed-task", "harness.stdout.txt"))
	require.NoError(t, err, "reading retained stdout")
	listing := string(stdout)
	require.Contains(t, listing, "README.md")
	require.NotContains(t, listing, "trace.jsonl", "trace must stay out of the workspace")
}

func TestReplayRecording_PassesWhenJudgePasses(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("ok"), 0o644))

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

	result, err := ReplayRecording(context.Background(), recording, task, workspace, judge.Options{}, "")
	require.NoError(t, err)

	require.Equal(t, "pass", result.Outcome)
	require.True(t, result.JudgeVerdict.Passed)
	require.NotNil(t, result.Trace)
}

func TestReplayRecording_FailsWhenJudgeFails(t *testing.T) {
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

	result, err := ReplayRecording(context.Background(), recording, task, workspace, judge.Options{}, "")
	require.NoError(t, err)

	require.Equal(t, "fail", result.Outcome)
	require.False(t, result.JudgeVerdict.Passed)
}

func TestParseTraceFile_ReturnsLastRecord(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid trace", func(t *testing.T) {
		trace := types.RunTrace{ID: "t1", Turns: 5}
		data, err := json.Marshal(trace)
		require.NoError(t, err)
		path := filepath.Join(dir, "valid.jsonl")
		require.NoError(t, os.WriteFile(path, append([]byte("ignored first line\n"), append(data, '\n')...), 0o644))

		got, err := parseTraceFile(path)
		require.NoError(t, err)
		require.Equal(t, "t1", got.ID)
		require.Equal(t, 5, got.Turns)
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(dir, "empty.jsonl")
		require.NoError(t, os.WriteFile(path, []byte(""), 0o644))

		_, err := parseTraceFile(path)
		require.Error(t, err)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := parseTraceFile(filepath.Join(dir, "nonexistent.jsonl"))
		require.Error(t, err)
	})
}

func TestValidateSuite_ChecksIDsTasksAndJudges(t *testing.T) {
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
			suite: types.EvalSuite{ID: "s1", Tasks: []types.EvalTask{{ID: "t1", Judge: validJudge}}},
		},
		{
			name:    "task without a judge",
			suite:   types.EvalSuite{ID: "s1", Tasks: []types.EvalTask{{ID: "t1"}}},
			wantErr: `task "t1": unknown judge type: ""`,
		},
		{
			name: "empty nested composite",
			suite: types.EvalSuite{ID: "s1", Tasks: []types.EvalTask{{ID: "t1", Judge: types.EvalJudge{
				Type:   "composite",
				Judges: []types.EvalJudge{{Type: "composite"}},
			}}}},
			wantErr: `task "t1": sub-judge 1: composite judge requires at least one sub-judge`,
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
				Tasks: []types.EvalTask{{ID: "t1", Judge: validJudge}, {ID: "t1", Judge: validJudge}},
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
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
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
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
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
	require.NoError(t, err)

	require.Len(t, result.Tasks, len(tasks))
	for i, want := range tasks {
		assert.Equal(t, want.ID, result.Tasks[i].TaskID, "Tasks[%d]: suite order not preserved", i)
	}

	logBytes, err := os.ReadFile(completionLog)
	require.NoError(t, err, "reading completion log")
	completionOrder := strings.Fields(string(logBytes))
	require.Len(t, completionOrder, len(tasks), "completion log: %q", completionOrder)
	assert.NotEqual(t, "t1", completionOrder[0],
		"t1 has the longest sleep, so finishing first suggests sequential execution; log=%v", completionOrder)
	// t1 (300ms sleep) must finish last under concurrency=4; a non-last t1
	// would mean a faster task got dispatched after it yet finished later,
	// which is impossible without contention this test doesn't introduce.
	assert.Equal(t, "t1", completionOrder[len(completionOrder)-1],
		"t1 (300ms sleep) must finish last under concurrency=4; full order: %v", completionOrder)
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
		defer close(done)
		_, err := RunSuite(context.Background(), suite, RunConfig{
			HarnessPath: harness,
			Concurrency: 0,
		})
		assert.NoError(t, err)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "RunSuite with Concurrency=0 deadlocked")
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
	require.NoError(t, err)

	require.Len(t, result.Tasks, 3, "sibling failures must not abort the suite")
	assert.Equal(t, "ok-1", result.Tasks[0].TaskID)
	assert.Equal(t, "broken", result.Tasks[1].TaskID)
	assert.Equal(t, "ok-2", result.Tasks[2].TaskID)
	assert.Equal(t, "error", result.Tasks[1].Outcome, "broken task outcome")
	// Siblings must each get a verdict, even if it's a fail (the placeholder
	// file does not exist in the temp workspace).
	for _, idx := range []int{0, 2} {
		assert.NotEmpty(t, result.Tasks[idx].Outcome, "sibling task %s outcome", result.Tasks[idx].TaskID)
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
	require.NoError(t, err)

	for _, taskID := range []string{"alpha", "beta"} {
		t.Run(taskID, func(t *testing.T) {
			base := filepath.Join(out, "artifact-suite", taskID)
			for _, name := range []string{"trace.jsonl", "harness.stdout.txt", "harness.stderr.txt"} {
				p := filepath.Join(base, name)
				info, err := os.Stat(p)
				require.NoError(t, err, "missing artifact %s", p)
				require.False(t, info.IsDir(), "artifact %s is a directory, want regular file", p)
			}

			traceData, err := os.ReadFile(filepath.Join(base, "trace.jsonl"))
			if err == nil {
				require.Contains(t, string(traceData), `"id":"trace-1"`)
			}
			stdout, _ := os.ReadFile(filepath.Join(base, "harness.stdout.txt"))
			require.Contains(t, string(stdout), "stdout chatter")
			stderr, _ := os.ReadFile(filepath.Join(base, "harness.stderr.txt"))
			require.Contains(t, string(stderr), "stderr chatter")
		})
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
			require.Error(t, err)
			require.ErrorContains(t, err, tc.wantSub)
			// The error payload must not leak a path that escaped the sandbox.
			require.NotContains(t, err.Error(), fmt.Sprintf("%c..%c", os.PathSeparator, os.PathSeparator))
		})
	}
}

// TestRunSuite_ContextCancel_DrainsUndispatchedTasks pins the dispatcher's
// ctx.Done() drain branch: when the context is cancelled mid-suite, the runner
// returns promptly, every result slot is populated (no zero-value TaskResults),
// and un-dispatched tasks are recorded as outcome="error" carrying the
// ctx.Err() message.
func TestRunSuite_ContextCancel_DrainsUndispatchedTasks(t *testing.T) {
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
		require.FailNow(t, "RunSuite did not return after context cancellation (deadlock?)")
	}

	require.NoError(t, outcome.err, "RunSuite returned suite-level error after cancel")
	require.Len(t, outcome.result, len(tasks), "drain must populate every slot")

	for i, tr := range outcome.result {
		t.Run(tasks[i].ID, func(t *testing.T) {
			// Zero-value TaskResult would have empty TaskID and empty Outcome.
			// Either branch (worker or drain) must produce a non-zero result.
			require.NotEmpty(t, tr.TaskID, "drain left a zero-value slot")
			require.NotEmpty(t, tr.Outcome, "slot was never written")
			require.Equal(t, tasks[i].ID, tr.TaskID, "input order not preserved")
			if tr.Outcome == "error" {
				require.Equal(t, types.JudgeStatusError, tr.JudgeVerdict.Status, "error outcome verdict status")
			}
		})
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
	require.Positive(t, errCount,
		"expected at least one task to record context-canceled error; got outcomes: %v",
		collectOutcomes(outcome.result))
}

func collectOutcomes(results []eval.TaskResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = fmt.Sprintf("%s=%s", r.TaskID, r.Outcome)
	}
	return out
}

// writeArgvRecordingHarness writes a fake harness that records its argv, one
// argument per line, to argvPath and writes a minimal success trace so the
// runner reaches artifact retention.
func writeArgvRecordingHarness(t *testing.T) (harnessPath, argvPath string) {
	t.Helper()
	dir := t.TempDir()
	harnessPath = filepath.Join(dir, "fake-harness")
	argvPath = filepath.Join(dir, "argv.txt")
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
`, argvPath)
	require.NoError(t, os.WriteFile(harnessPath, []byte(script), 0o755))
	return harnessPath, argvPath
}

// readArgvFile returns the argv recorded by a harness from writeArgvRecordingHarness.
func readArgvFile(t *testing.T, argvPath string) string {
	t.Helper()
	data, err := os.ReadFile(argvPath)
	require.NoError(t, err, "reading captured argv")
	return string(data)
}

// requireArgvHas asserts that each arg appears as a complete line of the captured argv.
func requireArgvHas(t *testing.T, captured string, args ...string) {
	t.Helper()
	for _, arg := range args {
		require.Contains(t, captured, arg+"\n", "captured argv missing %q", arg)
	}
}

// TestAppendAnthropicWIFArgs_EmitsOnlySetFields pins the on-the-wire flag
// spellings the runner forwards to `stirrup harness`; these must match the
// harness side (harness/cmd/stirrup/cmd/runconfigflags.go).
func TestAppendAnthropicWIFArgs_EmitsOnlySetFields(t *testing.T) {
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
			name: "boolean toggle with no identifiers forwards only its flag",
			wif: AnthropicWIFConfig{
				FromGitHubActions: true,
			},
			want: []string{"--anthropic-from-github-actions"},
		},
		{
			name: "partial config forwards what is set and leaves validation to the harness",
			wif: AnthropicWIFConfig{
				FederationRuleID: "fdrl_only",
			},
			want: []string{"--anthropic-federation-rule-id", "fdrl_only"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := appendAnthropicWIFArgs(nil, tc.wif)
			require.Equal(t, tc.want, got)
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
	harnessPath, argvPath := writeArgvRecordingHarness(t)

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
	require.NoError(t, err)

	requireArgvHas(t, readArgvFile(t, argvPath),
		"--anthropic-federation-rule-id",
		"fdrl_01TEST",
		"--anthropic-organization-id",
		"00000000-0000-0000-0000-000000000000",
		"--anthropic-service-account-id",
		"svac_01TEST",
		"--anthropic-from-github-actions",
	)
}

// TestRunSuite_ForwardsModelFlag pins that RunConfig.Model reaches the
// harness argv as --model.
func TestRunSuite_ForwardsModelFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath := writeArgvRecordingHarness(t)

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
		Model:       "claude-haiku-5-5",
	})
	require.NoError(t, err)

	requireArgvHas(t, readArgvFile(t, argvPath), "--model", "claude-haiku-5-5")
}

// TestRunSuite_NoModelFlagWhenUnset pins that an empty RunConfig.Model does
// not emit --model at all.
func TestRunSuite_NoModelFlagWhenUnset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath := writeArgvRecordingHarness(t)

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
	require.NoError(t, err)

	require.NotContains(t, readArgvFile(t, argvPath), "--model\n")
}

// TestRunSuite_ForwardsPromptModelFlag pins that RunConfig.PromptModel
// reaches the harness argv as --prompt-model.
func TestRunSuite_ForwardsPromptModelFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath := writeArgvRecordingHarness(t)

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
	require.NoError(t, err)

	requireArgvHas(t, readArgvFile(t, argvPath),
		"--model", "claude-fable-6",
		"--prompt-model", "claude-fable-5",
	)
}

// An empty RunConfig.PromptModel must not emit --prompt-model at all,
// mirroring TestRunSuite_NoModelFlagWhenUnset.
func TestRunSuite_NoPromptModelFlagWhenUnset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath := writeArgvRecordingHarness(t)

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
	require.NoError(t, err)

	require.NotContains(t, readArgvFile(t, argvPath), "--prompt-model\n")
}

// TestRunSuite_ForwardsProviderFlags pins that RunConfig.Provider,
// BaseURL, and APIKeyRef reach the harness argv. The per-push eval gate
// (ci.yml::eval-gate) runs a suite that pins no provider of its own
// against OpenRouter purely through these three flags; a silent drop
// would send every task to the harness's default Anthropic provider and
// surface as a wall of auth errors rather than a missing-flag error.
func TestRunSuite_ForwardsProviderFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath := writeArgvRecordingHarness(t)

	suite := types.EvalSuite{
		ID: "provider-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "provider-task",
				Prompt: "noop",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"unused-by-provider-test.txt"},
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		Provider:    "openai-compatible",
		BaseURL:     "https://openrouter.ai/api/v1",
		APIKeyRef:   "secret://OPENROUTER_API_KEY",
	})
	require.NoError(t, err)

	requireArgvHas(t, readArgvFile(t, argvPath),
		"--provider", "openai-compatible",
		"--base-url", "https://openrouter.ai/api/v1",
		"--api-key-ref", "secret://OPENROUTER_API_KEY",
	)
}

// Each provider flag is emitted independently, so an unset field must
// not appear at all — a suite that pins its own provider in run_config
// and only wants the base URL retargeted must not have --provider
// silently reasserted at the harness default.
func TestRunSuite_NoProviderFlagsWhenUnset(t *testing.T) {
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
	require.NoError(t, os.WriteFile(harnessPath, []byte(script), 0o755))

	suite := types.EvalSuite{
		ID: "no-provider-suite",
		Tasks: []types.EvalTask{
			{
				ID:     "no-provider-task",
				Prompt: "noop",
				Judge: types.EvalJudge{
					Type:  "file-exists",
					Paths: []string{"unused-by-provider-test.txt"},
				},
			},
		},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		BaseURL:     "https://openrouter.ai/api/v1",
	})
	require.NoError(t, err)

	data, err := os.ReadFile(argvCapture)
	require.NoError(t, err, "reading captured argv")
	captured := string(data)
	require.Contains(t, captured, "--base-url\n")

	tests := []struct {
		name string
		flag string
	}{
		{name: "omits provider flag", flag: "--provider"},
		{name: "omits api key ref flag", flag: "--api-key-ref"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NotContains(t, captured, tc.flag+"\n")
		})
	}
}
