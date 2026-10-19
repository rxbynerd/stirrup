package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// trialHarnessScript records each invocation's workspace, reports whether
// a marker left by an earlier trial is visible, tracks how many
// invocations overlap, then writes the marker the file-exists judge
// checks and a trace.
func trialHarnessScript(logDir string) string {
	return fmt.Sprintf(`#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo "$PWD" >> %[1]q/workspaces.log
[ -f marker ] && echo "$PWD" >> %[1]q/dirty.log
mkdir -p %[1]q/running
touch %[1]q/running/$$
sleep 0.3
ls %[1]q/running | wc -l >> %[1]q/overlap.log
rm -f %[1]q/running/$$
echo marker > marker
echo "trial stdout"
echo "trial stderr" >&2
[ -n "$TRACE" ] && echo '{"id":"t","turns":2,"outcome":"success"}' > "$TRACE"
`, logDir)
}

// readLines returns the whitespace-separated fields of path, or nil when the
// file does not exist.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(string(data))
}

func markerJudge() types.EvalJudge {
	return types.EvalJudge{Type: "file-exists", Paths: []string{"marker"}}
}

func TestRunSuite_TrialsUseFreshWorkspacesAndPerTrialArtifacts(t *testing.T) {
	logDir := t.TempDir()
	harness := writeFakeHarness(t, trialHarnessScript(logDir))

	suite := types.EvalSuite{ID: "trials-suite", Tasks: []types.EvalTask{
		{ID: "alpha", Prompt: "p", Judge: markerJudge()},
		{ID: "beta", Prompt: "p", Judge: markerJudge()},
	}}
	out := t.TempDir()
	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
		Concurrency: 3,
		Trials:      3,
	})
	require.NoError(t, err)

	require.Equal(t, 3, result.Trials)
	require.InDelta(t, 1.0, result.PassRate, 1e-9)
	for _, tr := range result.Tasks {
		require.Len(t, tr.Trials, 3, "task %s", tr.TaskID)
		require.Equal(t, "pass", tr.Outcome, "task %s", tr.TaskID)
		require.InDelta(t, 1.0, tr.PassFraction, 1e-9, "task %s", tr.TaskID)
		for i, trial := range tr.Trials {
			require.Equal(t, i+1, trial.Trial, "task %s trial index", tr.TaskID)
			require.Equal(t, "pass", trial.Outcome, "task %s trial %d", tr.TaskID, i+1)
			require.Equal(t, 2, trial.Turns, "task %s trial %d", tr.TaskID, i+1)
		}
	}

	workspaces := readLines(t, filepath.Join(logDir, "workspaces.log"))
	require.Len(t, workspaces, 6, "2 tasks x 3 trials, got %v", workspaces)
	slices.Sort(workspaces)
	require.Len(t, slices.Compact(workspaces), 6, "trials shared a workspace")
	dirty := readLines(t, filepath.Join(logDir, "dirty.log"))
	require.Empty(t, dirty, "a trial saw an earlier trial's marker in %v", dirty)

	overlap := 0
	for _, s := range readLines(t, filepath.Join(logDir, "overlap.log")) {
		n, _ := strconv.Atoi(s)
		overlap = max(overlap, n)
	}
	require.GreaterOrEqual(t, overlap, 2, "max overlapping invocations with concurrency 3")

	for _, taskID := range []string{"alpha", "beta"} {
		taskDir := filepath.Join(out, "trials-suite", taskID)
		entries, err := os.ReadDir(taskDir)
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		require.Equal(t, []string{"trial-1", "trial-2", "trial-3"}, names, "%s holds only the trial directories", taskDir)

		for i := 1; i <= 3; i++ {
			trialDir := filepath.Join(taskDir, fmt.Sprintf("trial-%d", i))
			for _, name := range []string{"trace.jsonl", "harness.stdout.txt", "harness.stderr.txt"} {
				_, err := os.Stat(filepath.Join(trialDir, name))
				require.NoError(t, err)
			}
			stdout, _ := os.ReadFile(filepath.Join(trialDir, "harness.stdout.txt"))
			require.Contains(t, string(stdout), "trial stdout", "%s/harness.stdout.txt", trialDir)
		}
	}
}

func TestRunSuite_TrialsAggregateMixedOutcomes(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	// The second invocation skips the marker, so its trial fails.
	harness := writeFakeHarness(t, fmt.Sprintf(`#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo x >> %[1]q
N=$(wc -l < %[1]q | tr -d ' ')
[ "$N" != "2" ] && echo marker > marker
[ -n "$TRACE" ] && echo "{\"id\":\"t\",\"turns\":$N,\"outcome\":\"success\"}" > "$TRACE"
exit 0
`, counter))

	suite := types.EvalSuite{ID: "mixed", Tasks: []types.EvalTask{{ID: "flaky", Prompt: "p", Judge: markerJudge()}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, Trials: 3})
	require.NoError(t, err)

	tr := result.Tasks[0]
	got := []string{tr.Trials[0].Outcome, tr.Trials[1].Outcome, tr.Trials[2].Outcome}
	require.Equal(t, []string{"pass", "fail", "pass"}, got)
	require.Equal(t, "pass", tr.Outcome, "majority outcome")
	require.InDelta(t, 2.0/3, tr.PassFraction, 1e-9)
	require.InDelta(t, 2.0/3, result.PassRate, 1e-9)

	// The task-level verdict and trace come from the first passing trial.
	require.True(t, tr.JudgeVerdict.Passed)
	require.NotNil(t, tr.Trace)
	require.Equal(t, 1, tr.Trace.Turns)

	require.False(t, tr.Trials[1].JudgeVerdict.Passed)
	require.Equal(t, 2, tr.Trials[1].Turns)
}

func TestRunSuite_TrialCountPrecedence(t *testing.T) {
	logDir := t.TempDir()
	harness := writeFakeHarness(t, trialHarnessScript(logDir))
	suite := types.EvalSuite{ID: "precedence", Trials: 2, Tasks: []types.EvalTask{{ID: "only", Prompt: "p", Judge: markerJudge()}}}

	cases := []struct {
		name      string
		cfgTrials int
		want      int
	}{
		{"suite attribute applies when the invocation is unset", 0, 2},
		{"invocation overrides the suite attribute", 1, 1},
		{"invocation can raise the count", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(filepath.Join(logDir, "workspaces.log"))
			out := t.TempDir()
			result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, OutputDir: out, Trials: tc.cfgTrials})
			require.NoError(t, err)
			require.Equal(t, tc.want, result.Trials)
			require.Len(t, readLines(t, filepath.Join(logDir, "workspaces.log")), tc.want, "harness runs")

			tr := result.Tasks[0]
			if tc.want == 1 {
				require.Empty(t, tr.Trials, "single trial keeps the flat result shape")
				_, err := os.Stat(filepath.Join(out, "precedence", "only", "trace.jsonl"))
				require.NoError(t, err, "single trial keeps the flat artifact layout")
			} else {
				require.Len(t, tr.Trials, tc.want)
			}
		})
	}
}

func TestRunSuite_SingleTrialResultShape(t *testing.T) {
	harness := writeFakeHarness(t, trialHarnessScript(t.TempDir()))
	suite := types.EvalSuite{ID: "single", Tasks: []types.EvalTask{{ID: "only", Prompt: "p", Judge: markerJudge()}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	require.NoError(t, err)

	require.Equal(t, 1, result.Trials)
	tr := result.Tasks[0]
	require.Empty(t, tr.Trials)
	require.Equal(t, "pass", tr.Outcome)
	require.InDelta(t, 1.0, tr.PassFraction, 1e-9)
	require.NotNil(t, tr.Trace)
}

func TestRunSuite_DryRunReportsPlannedTrials(t *testing.T) {
	suite := types.EvalSuite{ID: "dry", Trials: 2, Tasks: []types.EvalTask{{ID: "a", Prompt: "p", Judge: markerJudge()}, {ID: "b", Prompt: "p", Judge: markerJudge()}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true, Trials: 3})
	require.NoError(t, err)

	require.Equal(t, 3, result.Trials)
	require.InDelta(t, 1.0, result.PassRate, 1e-9)
	for _, tr := range result.Tasks {
		require.Empty(t, tr.Trials, "dry-run task %s", tr.TaskID)
		require.InDelta(t, 1.0, tr.PassFraction, 1e-9, "dry-run task %s", tr.TaskID)
	}
}

func TestRunSuite_RejectsTrialsAboveTheCap(t *testing.T) {
	task := types.EvalTask{ID: "a", Prompt: "p", Judge: markerJudge()}
	cases := []struct {
		name  string
		suite types.EvalSuite
		cfg   RunConfig
	}{
		{"suite attribute", types.EvalSuite{ID: "over", Trials: 21, Tasks: []types.EvalTask{task}}, RunConfig{DryRun: true}},
		{"invocation value", types.EvalSuite{ID: "over", Tasks: []types.EvalTask{task}}, RunConfig{DryRun: true, Trials: 21}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RunSuite(context.Background(), tc.suite, tc.cfg)
			require.ErrorContains(t, err, "trials must be at most 20, got 21")
		})
	}

	atCap := types.EvalSuite{ID: "cap", Trials: 20, Tasks: []types.EvalTask{task}}
	_, err := RunSuite(context.Background(), atCap, RunConfig{DryRun: true})
	require.NoError(t, err, "trials at the cap")
}

func TestRunSuite_RejectsNegativeSuiteTrials(t *testing.T) {
	suite := types.EvalSuite{ID: "neg", Trials: -1, Tasks: []types.EvalTask{{ID: "a", Prompt: "p", Judge: markerJudge()}}}
	_, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	require.ErrorContains(t, err, "trials")
}

// TestRunSuite_TrialsCancellation pins that cancelling mid-suite still
// yields every task with a full set of trials, undispatched ones recorded
// as context errors.
func TestRunSuite_TrialsCancellation(t *testing.T) {
	harness := writeFakeHarness(t, `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
sleep 0.5
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`)
	suite := types.EvalSuite{ID: "cancel", Tasks: []types.EvalTask{
		{ID: "a", Prompt: "p", Judge: markerJudge()},
		{ID: "b", Prompt: "p", Judge: markerJudge()},
		{ID: "c", Prompt: "p", Judge: markerJudge()},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	done := make(chan eval.SuiteResult, 1)
	go func() {
		r, err := RunSuite(ctx, suite, RunConfig{HarnessPath: harness, Concurrency: 1, Trials: 2})
		assert.NoError(t, err)
		done <- r
	}()
	var result eval.SuiteResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "RunSuite did not return after cancellation")
	}

	cancelled := 0
	for i, tr := range result.Tasks {
		require.Equal(t, suite.Tasks[i].ID, tr.TaskID, "Tasks[%d]", i)
		require.Len(t, tr.Trials, 2, "task %s", tr.TaskID)
		for _, trial := range tr.Trials {
			require.NotEmpty(t, trial.Outcome, "task %s has an empty trial slot", tr.TaskID)
			if strings.Contains(trial.Error, "context canceled") {
				cancelled++
			}
		}
	}
	require.Positive(t, cancelled, "no trial recorded the cancellation")
}

func TestAggregateTrials_MajorityAndErrors(t *testing.T) {
	trace := func(turns int) *types.RunTrace { return &types.RunTrace{Turns: turns} }

	t.Run("single trial is returned unchanged", func(t *testing.T) {
		in := eval.TaskResult{TaskID: "x", Outcome: "fail", DurationMs: 7, Trace: trace(3)}
		got := aggregateTrials("x", []eval.TaskResult{in})
		require.Empty(t, got.Trials)
		require.Equal(t, "fail", got.Outcome)
		require.Equal(t, int64(7), got.DurationMs)
		require.Zero(t, got.PassFraction)
		require.Same(t, in.Trace, got.Trace)
	})

	t.Run("majority failure takes the first failing trial", func(t *testing.T) {
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "pass", DurationMs: 1, Trace: trace(1), JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "ok"}},
			{Outcome: "fail", DurationMs: 2, Trace: trace(2), JudgeVerdict: eval.JudgeVerdict{Reason: "first failure"}},
			{Outcome: "fail", DurationMs: 3, Trace: trace(3), JudgeVerdict: eval.JudgeVerdict{Reason: "second failure"}},
		})
		require.Equal(t, "fail", got.Outcome)
		require.Equal(t, "first failure", got.JudgeVerdict.Reason)
		require.Equal(t, 2, got.Trace.Turns)
		require.Equal(t, int64(6), got.DurationMs, "sum of the trial durations")
		require.InDelta(t, 1.0/3, got.PassFraction, 1e-9)
		require.Equal(t, 3, got.Trials[2].Turns)
		require.Equal(t, 3, got.Trials[2].Trial)
	})

	t.Run("no majority reports the split", func(t *testing.T) {
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
			{Outcome: "fail"},
			{Outcome: "error", Error: "harness exited 7"},
		})
		want := "no majority across 3 trials: 1 passed, 1 failed, 1 errored"
		require.Equal(t, "error", got.Outcome)
		require.Equal(t, want, got.Error)
		require.Equal(t, want, got.JudgeVerdict.Reason)
		require.False(t, got.JudgeVerdict.Passed)
		require.Nil(t, got.Trace, "no-majority result carries no representative trace")
	})

	t.Run("error majority keeps the first error", func(t *testing.T) {
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "error", Error: "first"},
			{Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
			{Outcome: "error", Error: "second"},
		})
		require.Equal(t, "error", got.Outcome)
		require.Equal(t, "first", got.Error)
	})
}
