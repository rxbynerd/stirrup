package reporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/eval"
)

// trialTask builds a task result with passes passing trials followed by
// failing ones, aggregated the way the runner aggregates trials.
func trialTask(id string, passes, k int) eval.TaskResult {
	trials := make([]eval.TrialResult, k)
	for i := range trials {
		outcome := "fail"
		if i < passes {
			outcome = "pass"
		}
		trials[i] = eval.TrialResult{Trial: i + 1, Outcome: outcome}
	}
	r := eval.TaskResult{TaskID: id, Trials: trials}
	c := r.Counts()
	r.Outcome = c.Outcome()
	r.PassFraction = c.PassFraction()
	return r
}

func trialSuite(runID string, k int, passes []int) eval.SuiteResult {
	tasks := make([]eval.TaskResult, len(passes))
	for i, p := range passes {
		tasks[i] = trialTask(taskName(i), p, k)
	}
	return eval.SuiteResult{RunID: runID, Trials: k, Tasks: tasks}
}

func taskName(i int) string {
	return "t" + string(rune('a'+i))
}

// Worked example with hand-computed statistics: ten tasks, three trials each.
var (
	workedA = []int{3, 3, 3, 3, 2, 2, 1, 1, 0, 0}
	workedB = []int{3, 2, 3, 2, 2, 1, 0, 1, 0, 1}
)

func TestCompare_WorkedExample(t *testing.T) {
	report := Compare(trialSuite("A", 3, workedA), trialSuite("B", 3, workedB), DefaultOptions())
	s := report.Summary

	within(t, "baseline pass rate", s.BaselinePassRate, 0.60, 1e-9)
	within(t, "current pass rate", s.CurrentPassRate, 0.50, 1e-9)
	within(t, "baseline task-level SE", *s.Baseline.StdErr, 0.130, 0.0005)
	within(t, "current task-level SE", *s.Current.StdErr, 0.114, 0.0005)

	p := s.Paired
	if p == nil {
		t.Fatal("paired statistics missing")
	}
	if p.Tasks != 10 || p.DF != 9 {
		t.Errorf("paired n=%d df=%d, want 10 and 9", p.Tasks, p.DF)
	}
	within(t, "d-bar", p.MeanDelta, -0.100, 0.0005)
	within(t, "paired SE", p.StdErr, 0.0711, 0.00005)
	within(t, "t(9) CI low", p.CILow, -0.261, 0.0005)
	within(t, "t(9) CI high", p.CIHigh, 0.061, 0.0005)
	within(t, "sign-flip p", p.PValue, 0.375, 1e-12)
	if !p.PValueExact {
		t.Error("sign-flip p should be exact for 10 tasks")
	}
	within(t, "upper bound", p.UpperBound, -0.100+1.83311*0.0711458, 0.0001)
	if p.MDE == nil {
		t.Fatal("MDE missing")
	}
	within(t, "MDE", *p.MDE, 0.2238, 0.0005)

	wantHatA := []float64{0.60, 0.47, 0.40}
	wantHatB := []float64{0.50, 0.30, 0.20}
	for k := range 3 {
		within(t, "baseline pass^k", s.Baseline.PassHatK[k], wantHatA[k], 0.005)
		within(t, "current pass^k", s.Current.PassHatK[k], wantHatB[k], 0.005)
	}
	within(t, "baseline pass@3", s.Baseline.PassAtK[2], 0.80, 0.005)
	within(t, "current pass@3", s.Current.PassAtK[2], 0.80, 0.005)

	// A 10-point drop on ten tasks is not distinguishable from noise: the
	// gate warns but does not block, and no task crosses the flip
	// threshold even though four tasks lost a pass.
	if s.Gate != eval.GateWarn {
		t.Errorf("gate = %q, want %q (reasons %v)", s.Gate, eval.GateWarn, s.GateReasons)
	}
	if len(report.Regressions) != 0 {
		t.Errorf("regressions = %+v, want none", report.Regressions)
	}
	if s.NoiseFloor != nil {
		t.Errorf("noise floor reported for a baseline that is not all-pass: %+v", s.NoiseFloor)
	}
}

// singleRunSuite builds a K=1 result the way pre-trials result files look:
// no Trials, no PassFraction, no suite-level trials field.
func singleRunSuite(runID string, outcomes ...string) eval.SuiteResult {
	tasks := make([]eval.TaskResult, len(outcomes))
	for i, o := range outcomes {
		tasks[i] = eval.TaskResult{TaskID: taskName(i), Outcome: o}
	}
	return eval.SuiteResult{RunID: runID, Tasks: tasks}
}

func TestCompare_GateDecisions(t *testing.T) {
	allPass5 := singleRunSuite("base", "pass", "pass", "pass", "pass", "pass")

	cases := []struct {
		name      string
		baseline  eval.SuiteResult
		current   eval.SuiteResult
		opts      Options
		wantGate  string
		wantFlips []string
	}{
		{
			name:     "unchanged K=3 run against a K=1 all-pass baseline passes",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 3, 3, 3}),
			wantGate: eval.GatePass,
		},
		{
			name:     "one flaky trial warns without blocking",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 2, 3, 3}),
			wantGate: eval.GateWarn,
		},
		{
			name:     "a small drop inside the warn margin passes",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 2, 3, 3}),
			opts:     Options{WarnMargin: 0.1, FlipThreshold: DefaultFlipThreshold},
			wantGate: eval.GatePass,
		},
		{
			name:     "a confirmed mean drop blocks without any deterministic flip",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{1, 1, 1, 2, 2}),
			wantGate: eval.GateBlock,
		},
		{
			name:      "a deterministic flip blocks even when the interval does not",
			baseline:  allPass5,
			current:   trialSuite("curr", 3, []int{3, 3, 0, 3, 3}),
			wantGate:  eval.GateBlock,
			wantFlips: []string{"tc"},
		},
		{
			name:      "a deterministic flip blocks below the minimum task count",
			baseline:  singleRunSuite("base", "pass"),
			current:   trialSuite("curr", 3, []int{0}),
			wantGate:  eval.GateBlock,
			wantFlips: []string{"ta"},
		},
		{
			name:      "single-run results keep the any-flip behaviour",
			baseline:  allPass5,
			current:   singleRunSuite("curr", "pass", "fail", "pass", "pass", "pass"),
			wantGate:  eval.GateBlock,
			wantFlips: []string{"tb"},
		},
		{
			name:      "an all-error current run blocks",
			baseline:  allPass5,
			current:   singleRunSuite("curr", "error", "error", "error", "error", "error"),
			wantGate:  eval.GateBlock,
			wantFlips: []string{"ta", "tb", "tc", "td", "te"},
		},
		{
			name:     "two paired tasks are inconclusive",
			baseline: singleRunSuite("base", "pass", "pass"),
			current:  trialSuite("curr", 3, []int{1, 2}),
			wantGate: eval.GateInconclusive,
		},
		{
			name:     "no paired tasks is inconclusive",
			baseline: eval.SuiteResult{RunID: "base", Tasks: []eval.TaskResult{{TaskID: "other", Outcome: "pass"}}},
			current:  trialSuite("curr", 3, []int{3, 3, 3}),
			wantGate: eval.GateInconclusive,
		},
		{
			name:     "an improvement passes",
			baseline: singleRunSuite("base", "fail", "fail", "pass", "pass"),
			current:  trialSuite("curr", 3, []int{3, 3, 3, 3}),
			wantGate: eval.GatePass,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts == (Options{}) {
				opts = DefaultOptions()
			}
			s := Compare(tc.baseline, tc.current, opts).Summary
			if s.Gate != tc.wantGate {
				t.Errorf("gate = %q, want %q (reasons %v, paired %+v)", s.Gate, tc.wantGate, s.GateReasons, s.Paired)
			}
			if strings.Join(s.DeterministicFlips, ",") != strings.Join(tc.wantFlips, ",") {
				t.Errorf("deterministic flips = %v, want %v", s.DeterministicFlips, tc.wantFlips)
			}
			if s.Gate != eval.GatePass && len(s.GateReasons) == 0 {
				t.Errorf("gate %q carries no reason", s.Gate)
			}
		})
	}
}

func TestCompare_FlipThreshold(t *testing.T) {
	baseline := singleRunSuite("base", "pass", "pass", "pass")
	current := trialSuite("curr", 3, []int{2, 1, 3})

	report := Compare(baseline, current, DefaultOptions())
	if len(report.Regressions) != 1 || report.Regressions[0].TaskID != "tb" {
		t.Fatalf("default threshold regressions = %+v, want only tb (1/3)", report.Regressions)
	}
	r := report.Regressions[0]
	within(t, "regression baseline fraction", r.BaselinePassFraction, 1, 1e-12)
	within(t, "regression current fraction", r.CurrentPassFraction, 1.0/3, 1e-12)

	loose := Compare(baseline, current, Options{WarnMargin: DefaultWarnMargin, FlipThreshold: 0.7})
	if len(loose.Regressions) != 2 {
		t.Errorf("threshold 0.7 regressions = %+v, want ta and tb", loose.Regressions)
	}

	improved := Compare(trialSuite("base", 3, []int{1, 3, 0}), singleRunSuite("curr", "pass", "pass", "fail"), DefaultOptions())
	if len(improved.Improvements) != 1 || improved.Improvements[0].TaskID != "ta" {
		t.Errorf("improvements = %+v, want only ta (1/3 to 1/1)", improved.Improvements)
	}
}

func TestCompare_MixedTrialCounts(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3, 3, 2}), DefaultOptions())
	s := report.Summary

	if s.Baseline.Trials != 1 || s.Current.Trials != 3 {
		t.Errorf("trials = %d vs %d, want 1 vs 3", s.Baseline.Trials, s.Current.Trials)
	}
	if len(s.Baseline.PassHatK) != 1 || len(s.Current.PassHatK) != 3 {
		t.Errorf("pass^k rows = %v vs %v, want lengths 1 and 3", s.Baseline.PassHatK, s.Current.PassHatK)
	}
	within(t, "current pass^3", s.Current.PassHatK[2], 0.8, 1e-12)
	within(t, "current pass rate", s.CurrentPassRate, 14.0/15, 1e-12)
	within(t, "pass rate delta", s.PassRateDelta, 14.0/15-1, 1e-12)

	if len(report.Tasks) != 5 || report.Tasks[4].BaselineTrials != 1 || report.Tasks[4].CurrentTrials != 3 {
		t.Errorf("per-task pairs = %+v", report.Tasks)
	}

	nf := s.NoiseFloor
	if nf == nil {
		t.Fatal("noise floor missing for an all-pass baseline")
	}
	if nf.PooledTrials != 20 {
		t.Errorf("pooled trials = %d, want 20 (5 baseline + 15 current)", nf.PooledTrials)
	}
	within(t, "per-trial pass rate", nf.PerTrialPassRate, 19.0/20, 1e-12)
	within(t, "single-run false alarm", nf.SingleRunFalseAlarm, FlipFalseAlarmRate(0.95, 5, 1), 1e-12)
	within(t, "3-of-3 false alarm", nf.FlipRuleFalseAlarm, FlipFalseAlarmRate(0.95, 5, 3), 1e-12)
}

func TestCompare_SinglePairedTask(t *testing.T) {
	s := Compare(singleRunSuite("base", "pass"), trialSuite("curr", 3, []int{2}), DefaultOptions()).Summary
	if s.Paired != nil {
		t.Errorf("paired statistics with n=1: %+v, want nil", s.Paired)
	}
	if s.Gate != eval.GateInconclusive {
		t.Errorf("gate = %q, want inconclusive", s.Gate)
	}
	if s.Baseline.StdErr != nil {
		t.Errorf("task-level SE with one task: %v, want nil", *s.Baseline.StdErr)
	}
}

// TestCompare_CommittedBaselineWithoutTrials pins that the committed
// single-run baselines load and compare against a multi-trial run.
func TestCompare_CommittedBaselineWithoutTrials(t *testing.T) {
	for _, name := range []string{"dogfood-seed.json", "provider-quirks-openai.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "baselines", name))
			if err != nil {
				t.Fatal(err)
			}
			var baseline eval.SuiteResult
			if err := json.Unmarshal(data, &baseline); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if baseline.Trials != 0 || len(baseline.Tasks) == 0 {
				t.Fatalf("baseline trials=%d tasks=%d, want a legacy single-run file", baseline.Trials, len(baseline.Tasks))
			}

			current := eval.SuiteResult{RunID: "curr", Trials: 3}
			for _, bt := range baseline.Tasks {
				if len(bt.Trials) != 0 || bt.Counts().Total() != 1 {
					t.Fatalf("task %s: want one implicit trial, got %+v", bt.TaskID, bt.Counts())
				}
				ct := trialTask(bt.TaskID, 3, 3)
				current.Tasks = append(current.Tasks, ct)
			}

			s := Compare(baseline, current, DefaultOptions()).Summary
			if s.Gate != eval.GatePass {
				t.Errorf("gate = %q, want pass (reasons %v)", s.Gate, s.GateReasons)
			}
			within(t, "baseline pass rate", s.BaselinePassRate, baseline.PassRate, 1e-12)
			if s.Baseline.Trials != 1 || s.Current.Trials != 3 {
				t.Errorf("trials = %d vs %d, want 1 vs 3", s.Baseline.Trials, s.Current.Trials)
			}
		})
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := DefaultOptions().Validate(); err != nil {
		t.Errorf("defaults invalid: %v", err)
	}
	for _, bad := range []Options{
		{WarnMargin: -0.1, FlipThreshold: 0.5},
		{WarnMargin: 1.5, FlipThreshold: 0.5},
		{WarnMargin: 0.05, FlipThreshold: 1},
		{WarnMargin: 0.05, FlipThreshold: -0.1},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v: want a validation error", bad)
		}
	}
}

func TestFormatText_Statistics(t *testing.T) {
	report := Compare(trialSuite("run-a", 3, workedA), trialSuite("run-b", 3, workedB), DefaultOptions())
	got := FormatText(report)
	for _, want := range []string{
		"Eval Comparison: run-b vs run-a",
		"Gate: WARN",
		"Pass Rate: 60.0% → 50.0% (-10.0%)",
		"n=10 tasks, K=3",
		"pass^k (k=1..3): 0.600, 0.467, 0.400; pass@k: 0.600, 0.733, 0.800",
		"Paired: n=10, mean delta -0.100, SE 0.0711, two-sided 95% t(9) CI [-0.261, +0.061], one-sided 95% upper bound +0.030",
		"sign-flip p 0.375 (exact), MDE 0.22",
		"No regressions found.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
}

func TestFormatText_NoiseFloorAndUndefinedPaired(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass"), trialSuite("curr", 3, []int{0}), DefaultOptions())
	got := FormatText(report)
	for _, want := range []string{
		"Gate: BLOCK",
		"deterministic flip: ta",
		"Paired: n=1 task(s); paired statistics need at least 2",
		"Noise floor (all-pass baseline, n=1)",
		"the 3-of-3 flip rule",
		"ta: pass → fail (pass fraction 1.00 → 0.00)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
}

// TestComparisonReport_JSONShape pins the documented JSON field names.
func TestComparisonReport_JSONShape(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 2, 3}), DefaultOptions())
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	summary := doc["summary"].(map[string]any)
	for _, key := range []string{
		"baselinePassRate", "currentPassRate", "passRateDelta", "hasRegressions",
		"gate", "warnMargin", "flipThreshold", "baseline", "current", "paired", "noiseFloor",
	} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary missing %q in %s", key, data)
		}
	}
	paired := summary["paired"].(map[string]any)
	for _, key := range []string{"tasks", "meanDelta", "stdErr", "df", "ciLow", "ciHigh", "upperBound", "pValue", "pValueExact", "mde"} {
		if _, ok := paired[key]; !ok {
			t.Errorf("paired missing %q", key)
		}
	}

	flat := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3}), DefaultOptions())
	flatData, err := json.Marshal(flat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var flatDoc map[string]any
	if err := json.Unmarshal(flatData, &flatDoc); err != nil {
		t.Fatal(err)
	}
	flatPaired := flatDoc["summary"].(map[string]any)["paired"].(map[string]any)
	if _, ok := flatPaired["mde"]; ok {
		t.Errorf("mde present for a zero standard error: %s", flatData)
	}
	current := summary["current"].(map[string]any)
	for _, key := range []string{"tasks", "trials", "passRate", "stdErr", "wilsonLow", "wilsonHigh", "passHatK", "passAtK"} {
		if _, ok := current[key]; !ok {
			t.Errorf("current missing %q", key)
		}
	}
	if _, ok := doc["tasks"]; !ok {
		t.Error("report missing per-task pairs")
	}
}

func TestFormatText_MDEUndefinedAtZeroStdErr(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3}), DefaultOptions())
	got := FormatText(report)
	if !strings.Contains(got, "MDE n/a") {
		t.Errorf("report missing %q:\n%s", "MDE n/a", got)
	}
	if strings.Contains(got, "MDE 0.00") {
		t.Errorf("report prints a zero MDE:\n%s", got)
	}
}
