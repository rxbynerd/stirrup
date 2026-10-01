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

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}

	if result.Trials != 3 {
		t.Errorf("SuiteResult.Trials = %d, want 3", result.Trials)
	}
	if result.PassRate != 1 {
		t.Errorf("PassRate = %v, want 1", result.PassRate)
	}
	for _, tr := range result.Tasks {
		if len(tr.Trials) != 3 {
			t.Fatalf("task %s has %d trials, want 3", tr.TaskID, len(tr.Trials))
		}
		if tr.Outcome != "pass" || tr.PassFraction != 1 {
			t.Errorf("task %s: outcome=%q fraction=%v, want pass 1", tr.TaskID, tr.Outcome, tr.PassFraction)
		}
		for i, trial := range tr.Trials {
			if trial.Trial != i+1 || trial.Outcome != "pass" || trial.Turns != 2 {
				t.Errorf("task %s trial %d = %+v", tr.TaskID, i+1, trial)
			}
		}
	}

	workspaces := readLines(t, filepath.Join(logDir, "workspaces.log"))
	if len(workspaces) != 6 {
		t.Fatalf("harness ran %d times, want 6 (2 tasks x 3 trials): %v", len(workspaces), workspaces)
	}
	slices.Sort(workspaces)
	if len(slices.Compact(workspaces)) != 6 {
		t.Errorf("trials shared a workspace: %v", workspaces)
	}
	if dirty := readLines(t, filepath.Join(logDir, "dirty.log")); len(dirty) != 0 {
		t.Errorf("a trial saw an earlier trial's marker in %v", dirty)
	}
	overlap := 0
	for _, s := range readLines(t, filepath.Join(logDir, "overlap.log")) {
		n, _ := strconv.Atoi(s)
		overlap = max(overlap, n)
	}
	if overlap < 2 {
		t.Errorf("max overlapping invocations = %d, want >= 2 with concurrency 3", overlap)
	}

	for _, taskID := range []string{"alpha", "beta"} {
		taskDir := filepath.Join(out, "trials-suite", taskID)
		entries, err := os.ReadDir(taskDir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if want := []string{"trial-1", "trial-2", "trial-3"}; !slices.Equal(names, want) {
			t.Errorf("%s contains %v, want only %v", taskDir, names, want)
		}
		for i := 1; i <= 3; i++ {
			trialDir := filepath.Join(taskDir, fmt.Sprintf("trial-%d", i))
			for _, name := range []string{"trace.jsonl", "harness.stdout.txt", "harness.stderr.txt"} {
				if _, err := os.Stat(filepath.Join(trialDir, name)); err != nil {
					t.Errorf("missing artifact: %v", err)
				}
			}
			stdout, _ := os.ReadFile(filepath.Join(trialDir, "harness.stdout.txt"))
			if !strings.Contains(string(stdout), "trial stdout") {
				t.Errorf("%s/harness.stdout.txt = %q", trialDir, stdout)
			}
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
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	tr := result.Tasks[0]
	got := []string{tr.Trials[0].Outcome, tr.Trials[1].Outcome, tr.Trials[2].Outcome}
	if !slices.Equal(got, []string{"pass", "fail", "pass"}) {
		t.Fatalf("trial outcomes = %v, want [pass fail pass]", got)
	}
	if tr.Outcome != "pass" {
		t.Errorf("majority outcome = %q, want pass", tr.Outcome)
	}
	if tr.PassFraction != 2.0/3 || result.PassRate != 2.0/3 {
		t.Errorf("pass fraction = %v, suite pass rate = %v, want 2/3", tr.PassFraction, result.PassRate)
	}
	if !tr.JudgeVerdict.Passed || tr.Trace == nil || tr.Trace.Turns != 1 {
		t.Errorf("task-level verdict/trace should come from the first passing trial: %+v %+v", tr.JudgeVerdict, tr.Trace)
	}
	if tr.Trials[1].JudgeVerdict.Passed || tr.Trials[1].Turns != 2 {
		t.Errorf("failing trial = %+v", tr.Trials[1])
	}
}

func TestRunSuite_TrialsPrecedence(t *testing.T) {
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
			if err != nil {
				t.Fatalf("RunSuite: %v", err)
			}
			if result.Trials != tc.want {
				t.Errorf("SuiteResult.Trials = %d, want %d", result.Trials, tc.want)
			}
			if runs := len(readLines(t, filepath.Join(logDir, "workspaces.log"))); runs != tc.want {
				t.Errorf("harness ran %d times, want %d", runs, tc.want)
			}
			tr := result.Tasks[0]
			if tc.want == 1 {
				if len(tr.Trials) != 0 {
					t.Errorf("single trial should keep the flat result shape, got %d trials", len(tr.Trials))
				}
				if _, err := os.Stat(filepath.Join(out, "precedence", "only", "trace.jsonl")); err != nil {
					t.Errorf("single trial should keep the flat artifact layout: %v", err)
				}
			} else if len(tr.Trials) != tc.want {
				t.Errorf("got %d trials, want %d", len(tr.Trials), tc.want)
			}
		})
	}
}

func TestRunSuite_SingleTrialResultShape(t *testing.T) {
	harness := writeFakeHarness(t, trialHarnessScript(t.TempDir()))
	suite := types.EvalSuite{ID: "single", Tasks: []types.EvalTask{{ID: "only", Prompt: "p", Judge: markerJudge()}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if result.Trials != 1 {
		t.Errorf("SuiteResult.Trials = %d, want 1", result.Trials)
	}
	tr := result.Tasks[0]
	if len(tr.Trials) != 0 || tr.Outcome != "pass" || tr.PassFraction != 1 || tr.Trace == nil {
		t.Errorf("single-run result = %+v", tr)
	}
}

func TestRunSuite_DryRunReportsPlannedTrials(t *testing.T) {
	suite := types.EvalSuite{ID: "dry", Trials: 2, Tasks: []types.EvalTask{{ID: "a", Prompt: "p"}, {ID: "b", Prompt: "p"}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true, Trials: 3})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if result.Trials != 3 || result.PassRate != 1 {
		t.Errorf("dry run trials=%d passRate=%v, want 3 and 1", result.Trials, result.PassRate)
	}
	for _, tr := range result.Tasks {
		if len(tr.Trials) != 0 || tr.PassFraction != 1 {
			t.Errorf("dry-run task %s = %+v, want one validation pass", tr.TaskID, tr)
		}
	}
}

func TestRunSuite_RejectsNegativeSuiteTrials(t *testing.T) {
	suite := types.EvalSuite{ID: "neg", Trials: -1, Tasks: []types.EvalTask{{ID: "a", Prompt: "p"}}}
	if _, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true}); err == nil || !strings.Contains(err.Error(), "trials") {
		t.Errorf("error = %v, want a trials validation error", err)
	}
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
		if err != nil {
			t.Errorf("RunSuite: %v", err)
		}
		done <- r
	}()
	var result eval.SuiteResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunSuite did not return after cancellation")
	}

	cancelled := 0
	for i, tr := range result.Tasks {
		if tr.TaskID != suite.Tasks[i].ID {
			t.Errorf("Tasks[%d].TaskID = %q, want %q", i, tr.TaskID, suite.Tasks[i].ID)
		}
		if len(tr.Trials) != 2 {
			t.Fatalf("task %s has %d trials, want 2", tr.TaskID, len(tr.Trials))
		}
		for _, trial := range tr.Trials {
			if trial.Outcome == "" {
				t.Errorf("task %s has an empty trial slot", tr.TaskID)
			}
			if strings.Contains(trial.Error, "context canceled") {
				cancelled++
			}
		}
	}
	if cancelled == 0 {
		t.Error("no trial recorded the cancellation")
	}
}

func TestAggregateTrials(t *testing.T) {
	trace := func(turns int) *types.RunTrace { return &types.RunTrace{Turns: turns} }

	t.Run("single trial is returned unchanged", func(t *testing.T) {
		in := eval.TaskResult{TaskID: "x", Outcome: "fail", DurationMs: 7, Trace: trace(3)}
		got := aggregateTrials("x", []eval.TaskResult{in})
		if len(got.Trials) != 0 || got.Outcome != "fail" || got.DurationMs != 7 || got.PassFraction != 0 || got.Trace != in.Trace {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("majority failure takes the first failing trial", func(t *testing.T) {
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "pass", DurationMs: 1, Trace: trace(1), JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "ok"}},
			{Outcome: "fail", DurationMs: 2, Trace: trace(2), JudgeVerdict: eval.JudgeVerdict{Reason: "first failure"}},
			{Outcome: "fail", DurationMs: 3, Trace: trace(3), JudgeVerdict: eval.JudgeVerdict{Reason: "second failure"}},
		})
		if got.Outcome != "fail" || got.JudgeVerdict.Reason != "first failure" || got.Trace.Turns != 2 {
			t.Errorf("got outcome=%q verdict=%+v trace=%+v", got.Outcome, got.JudgeVerdict, got.Trace)
		}
		if got.DurationMs != 6 {
			t.Errorf("DurationMs = %d, want the sum 6", got.DurationMs)
		}
		if got.PassFraction != 1.0/3 {
			t.Errorf("PassFraction = %v, want 1/3", got.PassFraction)
		}
		if got.Trials[2].Turns != 3 || got.Trials[2].Trial != 3 {
			t.Errorf("trial 3 = %+v", got.Trials[2])
		}
	})

	t.Run("no majority reports the split", func(t *testing.T) {
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
			{Outcome: "fail"},
			{Outcome: "error", Error: "harness exited 7"},
		})
		want := "no majority across 3 trials: 1 passed, 1 failed, 1 errored"
		if got.Outcome != "error" || got.Error != want || got.JudgeVerdict.Reason != want || got.JudgeVerdict.Passed {
			t.Errorf("got outcome=%q error=%q verdict=%+v", got.Outcome, got.Error, got.JudgeVerdict)
		}
		if got.Trace != nil {
			t.Errorf("no-majority result should carry no representative trace")
		}
	})

	t.Run("error majority keeps the first error", func(t *testing.T) {
		got := aggregateTrials("x", []eval.TaskResult{
			{Outcome: "error", Error: "first"},
			{Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
			{Outcome: "error", Error: "second"},
		})
		if got.Outcome != "error" || got.Error != "first" {
			t.Errorf("got outcome=%q error=%q", got.Outcome, got.Error)
		}
	})
}
