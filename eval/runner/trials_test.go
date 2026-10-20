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
	must := require.New(t)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	must.NoError(err)
	return strings.Fields(string(data))
}

func markerJudge() types.EvalJudge {
	return types.EvalJudge{Type: "file-exists", Paths: []string{"marker"}}
}

func TestRunSuite_TrialsUseFreshWorkspacesAndPerTrialArtifacts(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	must.Equal(3, result.Trials)
	must.InDelta(1.0, result.PassRate, 1e-9)
	for _, tr := range result.Tasks {
		must.Len(tr.Trials, 3, "task %s", tr.TaskID)
		must.Equal("pass", tr.Outcome, "task %s", tr.TaskID)
		must.InDelta(1.0, tr.PassFraction, 1e-9, "task %s", tr.TaskID)
		for i, trial := range tr.Trials {
			must.Equal(i+1, trial.Trial, "task %s trial index", tr.TaskID)
			must.Equal("pass", trial.Outcome, "task %s trial %d", tr.TaskID, i+1)
			must.Equal(2, trial.Turns, "task %s trial %d", tr.TaskID, i+1)
		}
	}

	workspaces := readLines(t, filepath.Join(logDir, "workspaces.log"))
	must.Len(workspaces, 6, "2 tasks x 3 trials, got %v", workspaces)
	slices.Sort(workspaces)
	must.Len(slices.Compact(workspaces), 6, "trials shared a workspace")
	dirty := readLines(t, filepath.Join(logDir, "dirty.log"))
	must.Empty(dirty, "a trial saw an earlier trial's marker in %v", dirty)

	overlap := 0
	for _, s := range readLines(t, filepath.Join(logDir, "overlap.log")) {
		n, _ := strconv.Atoi(s)
		overlap = max(overlap, n)
	}
	must.GreaterOrEqual(overlap, 2, "max overlapping invocations with concurrency 3")

	for _, taskID := range []string{"alpha", "beta"} {
		taskDir := filepath.Join(out, "trials-suite", taskID)
		entries, err := os.ReadDir(taskDir)
		must.NoError(err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		must.Equal([]string{"trial-1", "trial-2", "trial-3"}, names, "%s holds only the trial directories", taskDir)

		for i := 1; i <= 3; i++ {
			trialDir := filepath.Join(taskDir, fmt.Sprintf("trial-%d", i))
			for _, name := range []string{"trace.jsonl", "harness.stdout.txt", "harness.stderr.txt"} {
				_, err := os.Stat(filepath.Join(trialDir, name))
				must.NoError(err)
			}
			stdout, _ := os.ReadFile(filepath.Join(trialDir, "harness.stdout.txt"))
			must.Contains(string(stdout), "trial stdout", "%s/harness.stdout.txt", trialDir)
		}
	}
}

func TestRunSuite_TrialsAggregateMixedOutcomes(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	tr := result.Tasks[0]
	got := []string{tr.Trials[0].Outcome, tr.Trials[1].Outcome, tr.Trials[2].Outcome}
	must.Equal([]string{"pass", "fail", "pass"}, got)
	must.Equal("pass", tr.Outcome, "majority outcome")
	must.InDelta(2.0/3, tr.PassFraction, 1e-9)
	must.InDelta(2.0/3, result.PassRate, 1e-9)

	// The task-level verdict and trace come from the first passing trial.
	must.True(tr.JudgeVerdict.Passed)
	must.NotNil(tr.Trace)
	must.Equal(1, tr.Trace.Turns)

	must.False(tr.Trials[1].JudgeVerdict.Passed)
	must.Equal(2, tr.Trials[1].Turns)
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
			must := require.New(t)
			_ = os.Remove(filepath.Join(logDir, "workspaces.log"))
			out := t.TempDir()
			result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, OutputDir: out, Trials: tc.cfgTrials})
			must.NoError(err)
			must.Equal(tc.want, result.Trials)
			must.Len(readLines(t, filepath.Join(logDir, "workspaces.log")), tc.want, "harness runs")

			tr := result.Tasks[0]
			if tc.want == 1 {
				must.Empty(tr.Trials, "single trial keeps the flat result shape")
				_, err := os.Stat(filepath.Join(out, "precedence", "only", "trace.jsonl"))
				must.NoError(err, "single trial keeps the flat artifact layout")
			} else {
				must.Len(tr.Trials, tc.want)
			}
		})
	}
}

func TestRunSuite_SingleTrialResultShape(t *testing.T) {
	must := require.New(t)
	harness := writeFakeHarness(t, trialHarnessScript(t.TempDir()))
	suite := types.EvalSuite{ID: "single", Tasks: []types.EvalTask{{ID: "only", Prompt: "p", Judge: markerJudge()}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	must.NoError(err)

	must.Equal(1, result.Trials)
	tr := result.Tasks[0]
	must.Empty(tr.Trials)
	must.Equal("pass", tr.Outcome)
	must.InDelta(1.0, tr.PassFraction, 1e-9)
	must.NotNil(tr.Trace)
}

func TestRunSuite_DryRunReportsPlannedTrials(t *testing.T) {
	must := require.New(t)
	suite := types.EvalSuite{ID: "dry", Trials: 2, Tasks: []types.EvalTask{{ID: "a", Prompt: "p", Judge: markerJudge()}, {ID: "b", Prompt: "p", Judge: markerJudge()}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true, Trials: 3})
	must.NoError(err)

	must.Equal(3, result.Trials)
	must.InDelta(1.0, result.PassRate, 1e-9)
	for _, tr := range result.Tasks {
		must.Empty(tr.Trials, "dry-run task %s", tr.TaskID)
		must.InDelta(1.0, tr.PassFraction, 1e-9, "dry-run task %s", tr.TaskID)
	}
}

func TestRunSuite_RejectsTrialsAboveTheCap(t *testing.T) {
	must := require.New(t)
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
			must := require.New(t)
			_, err := RunSuite(context.Background(), tc.suite, tc.cfg)
			must.ErrorContains(err, "trials must be at most 20, got 21")
		})
	}

	atCap := types.EvalSuite{ID: "cap", Trials: 20, Tasks: []types.EvalTask{task}}
	_, err := RunSuite(context.Background(), atCap, RunConfig{DryRun: true})
	must.NoError(err, "trials at the cap")
}

func TestRunSuite_RejectsNegativeSuiteTrials(t *testing.T) {
	must := require.New(t)
	suite := types.EvalSuite{ID: "neg", Trials: -1, Tasks: []types.EvalTask{{ID: "a", Prompt: "p", Judge: markerJudge()}}}
	_, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	must.ErrorContains(err, "trials")
}

// TestRunSuite_TrialsCancellation pins that cancelling mid-suite still
// yields every task with a full set of trials, undispatched ones recorded
// as context errors.
func TestRunSuite_TrialsCancellation(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
		check.NoError(err)
		done <- r
	}()
	var result eval.SuiteResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		must.FailNow("RunSuite did not return after cancellation")
	}

	cancelled := 0
	for i, tr := range result.Tasks {
		must.Equal(suite.Tasks[i].ID, tr.TaskID, "Tasks[%d]", i)
		must.Len(tr.Trials, 2, "task %s", tr.TaskID)
		for _, trial := range tr.Trials {
			must.NotEmpty(trial.Outcome, "task %s has an empty trial slot", tr.TaskID)
			if strings.Contains(trial.Error, "context canceled") {
				cancelled++
			}
		}
	}
	must.Positive(cancelled, "no trial recorded the cancellation")
}

func TestAggregateTrials_MajorityAndErrors(t *testing.T) {
	trace := func(turns int) *types.RunTrace { return &types.RunTrace{Turns: turns} }

	t.Run("single trial is returned unchanged", func(t *testing.T) {
		must := require.New(t)
		in := eval.TaskResult{TaskID: "x", Outcome: "fail", DurationMs: 7, Trace: trace(3)}
		got := aggregateTrials("x", []eval.TaskResult{in})
		must.Empty(got.Trials)
		must.Equal("fail", got.Outcome)
		must.Equal(int64(7), got.DurationMs)
		must.Zero(got.PassFraction)
		must.Same(in.Trace, got.Trace)
	})

	t.Run("majority failure takes the first failing trial", func(t *testing.T) {
		must := require.New(t)
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "pass", DurationMs: 1, Trace: trace(1), JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "ok"}},
			{Outcome: "fail", DurationMs: 2, Trace: trace(2), JudgeVerdict: eval.JudgeVerdict{Reason: "first failure"}},
			{Outcome: "fail", DurationMs: 3, Trace: trace(3), JudgeVerdict: eval.JudgeVerdict{Reason: "second failure"}},
		})
		must.Equal("fail", got.Outcome)
		must.Equal("first failure", got.JudgeVerdict.Reason)
		must.Equal(2, got.Trace.Turns)
		must.Equal(int64(6), got.DurationMs, "sum of the trial durations")
		must.InDelta(1.0/3, got.PassFraction, 1e-9)
		must.Equal(3, got.Trials[2].Turns)
		must.Equal(3, got.Trials[2].Trial)
	})

	t.Run("no majority reports the split", func(t *testing.T) {
		must := require.New(t)
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
			{Outcome: "fail"},
			{Outcome: "error", Error: "harness exited 7"},
		})
		want := "no majority across 3 trials: 1 passed, 1 failed, 1 errored"
		must.Equal("error", got.Outcome)
		must.Equal(want, got.Error)
		must.Equal(want, got.JudgeVerdict.Reason)
		must.False(got.JudgeVerdict.Passed)
		must.Nil(got.Trace, "no-majority result carries no representative trace")
	})

	t.Run("error majority keeps the first error", func(t *testing.T) {
		must := require.New(t)
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "error", Error: "first"},
			{Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
			{Outcome: "error", Error: "second"},
		})
		must.Equal("error", got.Outcome)
		must.Equal("first", got.Error)
	})
}
