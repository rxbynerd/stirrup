package reporter

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NotNil(t, p, "paired statistics missing")
	assert.Equal(t, 10, p.Tasks, "paired task count")
	assert.Equal(t, 9, p.DF, "paired degrees of freedom")
	within(t, "d-bar", p.MeanDelta, -0.100, 0.0005)
	within(t, "paired SE", p.StdErr, 0.0711, 0.00005)
	within(t, "t(9) CI low", p.CILow, -0.261, 0.0005)
	within(t, "t(9) CI high", p.CIHigh, 0.061, 0.0005)
	within(t, "sign-flip p", p.PValue, 0.375, 1e-12)
	assert.True(t, p.PValueExact, "sign-flip p should be exact for 10 tasks")
	within(t, "upper bound", p.UpperBound, -0.100+1.83311*0.0711458, 0.0001)
	require.NotNil(t, p.MDE, "MDE missing")
	within(t, "MDE", *p.MDE, 0.2238, 0.0005)

	wantHatA := []float64{0.60, 0.47, 0.40}
	wantHatB := []float64{0.50, 0.30, 0.20}
	for k := range 3 {
		within(t, "baseline pass^k", s.Baseline.PassHatK[k], wantHatA[k], 0.005)
		within(t, "current pass^k", s.Current.PassHatK[k], wantHatB[k], 0.005)
	}
	within(t, "baseline pass@3", s.Baseline.PassAtK[2], 0.80, 0.005)
	within(t, "current pass@3", s.Current.PassAtK[2], 0.80, 0.005)

	// The mean drop equals the default warn margin, so the gate passes;
	// no task crosses the flip threshold even though four tasks lost a pass.
	assert.Equal(t, eval.GatePass, s.Gate, "reasons %v", s.GateReasons)
	assert.Empty(t, report.Regressions)
	assert.Nil(t, s.NoiseFloor, "noise floor reported for a baseline that is not all-pass")
}

func TestCompare_WorkedExample_WarnsAtTighterMargin(t *testing.T) {
	opts := Options{WarnMargin: 0.05, FlipThreshold: DefaultFlipThreshold}
	s := Compare(trialSuite("A", 3, workedA), trialSuite("B", 3, workedB), opts).Summary
	assert.Equal(t, eval.GateWarn, s.Gate, "reasons %v", s.GateReasons)
	require.Len(t, s.GateReasons, 1)
	assert.Contains(t, s.GateReasons[0], "upper bound +0.030 is not below 0")
}

// singleRunSuite builds a K=1 result without Trials, PassFraction, or a
// suite-level trials field, as a single-run result file looks.
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
		name       string
		baseline   eval.SuiteResult
		current    eval.SuiteResult
		opts       Options
		wantGate   string
		wantFlips  []string
		wantReason string
	}{
		{
			name:     "unchanged k=3 run against a k=1 all-pass baseline passes",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 3, 3, 3}),
			wantGate: eval.GatePass,
		},
		{
			name:     "one lost trial stays inside the default warn margin",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 2, 3, 3}),
			wantGate: eval.GatePass,
		},
		{
			name:     "one lost trial warns under a tighter explicit margin",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 2, 3, 3}),
			opts:     Options{WarnMargin: 0.05, FlipThreshold: DefaultFlipThreshold},
			wantGate: eval.GateWarn,
		},
		{
			name:     "two lost trials warn without blocking",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 2, 2, 3}),
			wantGate: eval.GateWarn,
		},
		{
			name:       "a listed regression warns although the mean drop is inside the margin",
			baseline:   singleRunSuite("base", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass"),
			current:    trialSuite("curr", 3, []int{1, 3, 3, 3, 3, 3, 3, 3, 3, 3}),
			wantGate:   eval.GateWarn,
			wantReason: "ta (pass fraction 1.00 → 0.33)",
		},
		{
			name:       "two paired tasks with a listed regression warn instead of staying inconclusive",
			baseline:   singleRunSuite("base", "pass", "pass"),
			current:    trialSuite("curr", 3, []int{1, 2}),
			wantGate:   eval.GateWarn,
			wantReason: "regressed: ta",
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
			name:       "an all-error current run blocks",
			baseline:   allPass5,
			current:    singleRunSuite("curr", "error", "error", "error", "error", "error"),
			wantGate:   eval.GateBlock,
			wantFlips:  []string{"ta", "tb", "tc", "td", "te"},
			wantReason: "failed or errored every trial",
		},
		{
			name:     "three paired tasks are enough for the upper bound to block",
			baseline: singleRunSuite("base", "pass", "pass", "pass"),
			current:  trialSuite("curr", 3, []int{2, 2, 2}),
			wantGate: eval.GateBlock,
		},
		{
			name:     "three paired tasks are enough for a margin warning",
			baseline: singleRunSuite("base", "pass", "pass", "pass"),
			current:  trialSuite("curr", 3, []int{3, 3, 2}),
			wantGate: eval.GateWarn,
		},
		{
			name:     "three unchanged paired tasks pass",
			baseline: singleRunSuite("base", "pass", "pass", "pass"),
			current:  trialSuite("curr", 3, []int{3, 3, 3}),
			wantGate: eval.GatePass,
		},
		{
			name:     "two paired tasks without a regression are inconclusive",
			baseline: singleRunSuite("base", "pass", "pass"),
			current:  trialSuite("curr", 3, []int{2, 2}),
			wantGate: eval.GateInconclusive,
		},
		{
			name:     "an empty baseline pairs nothing and is inconclusive",
			baseline: eval.SuiteResult{RunID: "base"},
			current:  trialSuite("curr", 3, []int{3, 3, 3}),
			wantGate: eval.GateInconclusive,
		},
		{
			name:       "renaming every task warns instead of staying inconclusive",
			baseline:   eval.SuiteResult{RunID: "base", Tasks: []eval.TaskResult{{TaskID: "other", Outcome: "pass"}}},
			current:    trialSuite("curr", 3, []int{3, 3, 3}),
			wantGate:   eval.GateWarn,
			wantReason: "1 baseline task(s) missing from the current run: other",
		},
		{
			name:       "a baseline task missing from an otherwise passing run warns",
			baseline:   allPass5,
			current:    trialSuite("curr", 3, []int{3, 3, 3, 3}),
			wantGate:   eval.GateWarn,
			wantReason: "missing from the current run: te",
		},
		{
			name:      "a missing task does not lower a deterministic flip block",
			baseline:  allPass5,
			current:   trialSuite("curr", 3, []int{0, 3, 3, 3}),
			wantGate:  eval.GateBlock,
			wantFlips: []string{"ta"},
		},
		{
			name:     "a task new in the current run leaves the gate alone",
			baseline: allPass5,
			current:  trialSuite("curr", 3, []int{3, 3, 3, 3, 3, 3}),
			wantGate: eval.GatePass,
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
			require.Equal(t, tc.wantGate, s.Gate, "reasons %v, paired %+v", s.GateReasons, s.Paired)
			require.Equal(t, strings.Join(tc.wantFlips, ","), strings.Join(s.DeterministicFlips, ","), "deterministic flips")
			if s.Gate != eval.GatePass {
				require.NotEmpty(t, s.GateReasons, "gate %q carries no reason", s.Gate)
			}
			if tc.wantReason != "" {
				require.Contains(t, strings.Join(s.GateReasons, "\n"), tc.wantReason, "gate reasons %v", s.GateReasons)
			}
		})
	}
}

func TestCompare_FlipThreshold(t *testing.T) {
	baseline := singleRunSuite("base", "pass", "pass", "pass")
	current := trialSuite("curr", 3, []int{2, 1, 3})

	report := Compare(baseline, current, DefaultOptions())
	require.Len(t, report.Regressions, 1, "default threshold regressions %+v, want only tb (1/3)", report.Regressions)
	require.Equal(t, "tb", report.Regressions[0].TaskID)
	r := report.Regressions[0]
	within(t, "regression baseline fraction", r.BaselinePassFraction, 1, 1e-12)
	within(t, "regression current fraction", r.CurrentPassFraction, 1.0/3, 1e-12)

	loose := Compare(baseline, current, Options{WarnMargin: DefaultWarnMargin, FlipThreshold: 0.7})
	assert.Len(t, loose.Regressions, 2, "threshold 0.7 regressions %+v, want ta and tb", loose.Regressions)

	improved := Compare(trialSuite("base", 3, []int{1, 3, 0}), singleRunSuite("curr", "pass", "pass", "fail"), DefaultOptions())
	require.Len(t, improved.Improvements, 1, "improvements %+v, want only ta (1/3 to 1/1)", improved.Improvements)
	require.Equal(t, "ta", improved.Improvements[0].TaskID)
}

func ids[T any](items []T, id func(T) string) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = id(it)
	}
	return strings.Join(out, ",")
}

// TestCompare_FlipThresholdBoundary pins that a pass fraction exactly at
// the threshold counts as a regression (or an improvement on the mirror),
// at every trial count and with the tolerance on threshold arithmetic.
func TestCompare_FlipThresholdBoundary(t *testing.T) {
	cases := []struct {
		name         string
		baseline     eval.SuiteResult
		current      eval.SuiteResult
		threshold    float64
		wantRegress  string
		wantImproved string
	}{
		{"k=2 half passed", trialSuite("base", 2, []int{2, 2, 2}), trialSuite("curr", 2, []int{1, 2, 2}), 0.5, "ta", ""},
		{"k=4 half passed", trialSuite("base", 4, []int{4, 4, 4}), trialSuite("curr", 4, []int{2, 4, 4}), 0.5, "ta", ""},
		{"k=4 just above the threshold", trialSuite("base", 4, []int{4, 4, 4}), trialSuite("curr", 4, []int{3, 4, 4}), 0.5, "", ""},
		{"k=2 improvement from half", trialSuite("base", 2, []int{1, 2, 2}), trialSuite("curr", 2, []int{2, 2, 2}), 0.5, "", "ta"},
		{"k=4 improvement from half", trialSuite("base", 4, []int{2, 4, 4}), trialSuite("curr", 4, []int{4, 4, 4}), 0.5, "", "ta"},
		{"k=4 improvement from just above", trialSuite("base", 4, []int{3, 4, 4}), trialSuite("curr", 4, []int{4, 4, 4}), 0.5, "", ""},
		{"one third against a rounded threshold", trialSuite("base", 3, []int{3, 3, 3}), trialSuite("curr", 3, []int{1, 3, 3}), 0.3333333333, "ta", ""},
		{"one third improvement against a rounded threshold", trialSuite("base", 3, []int{1, 3, 3}), trialSuite("curr", 3, []int{3, 3, 3}), 0.3333333333, "", "ta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := Compare(tc.baseline, tc.current, Options{WarnMargin: DefaultWarnMargin, FlipThreshold: tc.threshold})
			gotRegress := ids(report.Regressions, func(r eval.TaskRegression) string { return r.TaskID })
			require.Equal(t, tc.wantRegress, gotRegress)
			gotImproved := ids(report.Improvements, func(r eval.TaskImprovement) string { return r.TaskID })
			require.Equal(t, tc.wantImproved, gotImproved)
		})
	}
}

func TestCompare_MixedTrialCounts(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3, 3, 2}), DefaultOptions())
	s := report.Summary

	assert.Equal(t, 1, s.Baseline.Trials, "baseline trials")
	assert.Equal(t, 3, s.Current.Trials, "current trials")
	require.Len(t, s.Baseline.PassHatK, 1)
	require.Len(t, s.Current.PassHatK, 3)
	within(t, "current pass^3", s.Current.PassHatK[2], 0.8, 1e-12)
	within(t, "current pass rate", s.CurrentPassRate, 14.0/15, 1e-12)
	within(t, "pass rate delta", s.PassRateDelta, 14.0/15-1, 1e-12)

	require.Len(t, report.Tasks, 5)
	assert.Equal(t, 1, report.Tasks[4].BaselineTrials, "baseline trials of the last paired task")
	assert.Equal(t, 3, report.Tasks[4].CurrentTrials, "current trials of the last paired task")

	nf := s.NoiseFloor
	require.NotNil(t, nf, "noise floor missing for an all-pass baseline")
	assert.Equal(t, 20, nf.PooledTrials, "pooled trials: 5 baseline and 15 current")
	within(t, "per-trial pass rate", nf.PerTrialPassRate, 19.0/20, 1e-12)
	require.NotNil(t, nf.SingleRunFalseAlarm, "false-alarm rates missing for a pool with a failure: %+v", nf)
	require.NotNil(t, nf.FlipRuleFalseAlarm)
	within(t, "single-run false alarm", *nf.SingleRunFalseAlarm, FlipFalseAlarmRate(0.95, 5, 1), 1e-12)
	within(t, "3-of-3 false alarm", *nf.FlipRuleFalseAlarm, FlipFalseAlarmRate(0.95, 5, 3), 1e-12)
}

func TestCompare_SinglePairedTask(t *testing.T) {
	s := Compare(singleRunSuite("base", "pass"), trialSuite("curr", 3, []int{2}), DefaultOptions()).Summary
	assert.Nil(t, s.Paired, "paired statistics with n=1")
	assert.Equal(t, eval.GateInconclusive, s.Gate)
	assert.Nil(t, s.Baseline.StdErr, "task-level SE with one task")
}

// TestCompare_CommittedBaselineWithoutTrials pins that the committed
// single-run baselines load and compare against a multi-trial run.
func TestCompare_CommittedBaselineWithoutTrials(t *testing.T) {
	for _, name := range []string{"dogfood-seed.json", "provider-quirks-openai.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "baselines", name))
			require.NoError(t, err)
			var baseline eval.SuiteResult
			require.NoError(t, json.Unmarshal(data, &baseline), "unmarshal")
			require.Zero(t, baseline.Trials, "committed baseline should be a single-run file")
			require.NotEmpty(t, baseline.Tasks)

			current := eval.SuiteResult{RunID: "curr", Trials: 3}
			for _, bt := range baseline.Tasks {
				require.Empty(t, bt.Trials, "task %s has explicit trials", bt.TaskID)
				require.Equal(t, 1, bt.Counts().Total(), "task %s: want one implicit trial, got %+v", bt.TaskID, bt.Counts())
				ct := trialTask(bt.TaskID, 3, 3)
				current.Tasks = append(current.Tasks, ct)
			}

			s := Compare(baseline, current, DefaultOptions()).Summary
			require.Equal(t, eval.GatePass, s.Gate, "reasons %v", s.GateReasons)
			within(t, "baseline pass rate", s.BaselinePassRate, baseline.PassRate, 1e-12)
			require.Equal(t, 1, s.Baseline.Trials, "baseline trials")
			require.Equal(t, 3, s.Current.Trials, "current trials")
		})
	}
}

func TestOptions_Validate_AcceptsDefaultsAndRejectsOutOfRange(t *testing.T) {
	require.NoError(t, DefaultOptions().Validate())

	cases := []struct {
		name string
		opts Options
	}{
		{"negative warn margin", Options{WarnMargin: -0.1, FlipThreshold: 0.5}},
		{"warn margin above one", Options{WarnMargin: 1.5, FlipThreshold: 0.5}},
		{"flip threshold of one", Options{WarnMargin: 0.05, FlipThreshold: 1}},
		{"negative flip threshold", Options{WarnMargin: 0.05, FlipThreshold: -0.1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.opts.Validate())
		})
	}
}

func TestFormatText_ReportsWorkedExampleStatistics(t *testing.T) {
	report := Compare(trialSuite("run-a", 3, workedA), trialSuite("run-b", 3, workedB), DefaultOptions())
	got := FormatText(report)
	for _, want := range []string{
		"Eval Comparison: run-b vs run-a",
		"Gate: PASS",
		"Pass Rate: 60.0% → 50.0% (-10.0%)",
		"n=10 tasks, K=3",
		"pass^k (k=1..3): 0.600, 0.467, 0.400; pass@k: 0.600, 0.733, 0.800",
		"Paired: n=10, mean delta -0.100, SE 0.0711, two-sided 95% t(9) CI [-0.261, +0.061], one-sided 95% upper bound +0.030",
		"sign-flip p 0.375 (exact), MDE 0.22",
		"No regressions found.",
	} {
		assert.Contains(t, got, want)
	}
}

func TestFormatText_ReportsNoiseFloorAndUndefinedPaired(t *testing.T) {
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
		assert.Contains(t, got, want)
	}
}

// TestComparisonReport_JSON_UsesDocumentedFieldNames pins the documented JSON field names.
func TestComparisonReport_JSON_UsesDocumentedFieldNames(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 2, 3}), DefaultOptions())
	data, err := json.Marshal(report)
	require.NoError(t, err, "marshal")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))

	summary := doc["summary"].(map[string]any)
	for _, key := range []string{
		"baselinePassRate", "currentPassRate", "passRateDelta", "hasRegressions",
		"gate", "warnMargin", "flipThreshold", "baseline", "current", "paired", "noiseFloor",
	} {
		assert.Contains(t, summary, key)
	}
	paired := summary["paired"].(map[string]any)
	for _, key := range []string{"tasks", "meanDelta", "stdErr", "df", "ciLow", "ciHigh", "upperBound", "pValue", "pValueExact", "mde"} {
		assert.Contains(t, paired, key)
	}

	flat := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3}), DefaultOptions())
	flatData, err := json.Marshal(flat)
	require.NoError(t, err, "marshal")
	var flatDoc map[string]any
	require.NoError(t, json.Unmarshal(flatData, &flatDoc))
	flatPaired := flatDoc["summary"].(map[string]any)["paired"].(map[string]any)
	assert.NotContains(t, flatPaired, "mde", "mde present for a zero standard error: %s", flatData)

	current := summary["current"].(map[string]any)
	for _, key := range []string{"tasks", "trials", "passRate", "stdErr", "wilsonLow", "wilsonHigh", "passHatK", "passAtK"} {
		assert.Contains(t, current, key)
	}
	assert.Contains(t, doc, "tasks", "report missing per-task pairs")
}

func TestFormatText_MDEUndefinedAtZeroStdErr(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3}), DefaultOptions())
	got := FormatText(report)
	assert.Contains(t, got, "MDE n/a")
	assert.NotContains(t, got, "MDE 0.00", "report prints a zero MDE")
}

func TestFloorGate_OnlyRaisesToMoreSevereGate(t *testing.T) {
	gates := []string{eval.GatePass, eval.GateInconclusive, eval.GateWarn, eval.GateBlock}
	for _, floor := range gates {
		for _, base := range gates {
			t.Run(base+" floored at "+floor, func(t *testing.T) {
				floorApplies := gateSeverity[floor] >= gateSeverity[base]
				gate, got := floorGate(base, []string{"base reason"}, floor, "floor reason")

				want := base
				if floorApplies {
					want = floor
				}
				require.Equal(t, want, gate)
				require.Equal(t, floorApplies, slices.Contains(got, "floor reason"), "reasons %v", got)
				require.Contains(t, got, "base reason")
			})
		}
	}
}

func TestCompare_FlipRuleBlockSurvivesRegressionFloor(t *testing.T) {
	s := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{0, 1, 3}), DefaultOptions()).Summary
	assert.Equal(t, eval.GateBlock, s.Gate, "reasons %v", s.GateReasons)
	for _, r := range s.GateReasons {
		assert.NotContains(t, r, "regressed:", "block reasons include the regression floor: %v", s.GateReasons)
	}
}

// TestCompare_WarnMarginBoundaryIgnoresTaskOrder pins that a mean delta
// exactly at the warn margin never warns, whatever order the float
// summation sees the tasks in.
func TestCompare_WarnMarginBoundaryIgnoresTaskOrder(t *testing.T) {
	// Six tasks lose a trial, three gain one, eleven are unchanged: the
	// mean delta is exactly -0.05 and the upper bound stays above zero.
	baselinePasses := slices.Concat(repeat(3, 6), repeat(2, 3), repeat(3, 11))
	currentPasses := slices.Concat(repeat(2, 6), repeat(3, 3), repeat(3, 11))
	opts := Options{WarnMargin: 0.05, FlipThreshold: DefaultFlipThreshold}

	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		order := rng.Perm(len(baselinePasses))
		b := make([]int, len(order))
		c := make([]int, len(order))
		for i, j := range order {
			b[i], c[i] = baselinePasses[j], currentPasses[j]
		}
		s := Compare(trialSuite("base", 3, b), trialSuite("curr", 3, c), opts).Summary
		require.Equal(t, eval.GatePass, s.Gate, "order %v: reasons %v, mean delta %.17g", order, s.GateReasons, s.Paired.MeanDelta)
	}
}

func repeat(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestFormatText_ResolutionNote(t *testing.T) {
	baseline := singleRunSuite("base", "pass", "pass", "pass", "pass", "pass")
	current := trialSuite("curr", 3, []int{3, 3, 3, 3, 2})

	tight := FormatText(Compare(baseline, current, Options{WarnMargin: 0.05, FlipThreshold: DefaultFlipThreshold}))
	assert.Contains(t, tight, "one lost trial lowers the mean pass fraction by 0.067, more than the warn margin 0.050")

	roomy := FormatText(Compare(baseline, current, DefaultOptions()))
	assert.NotContains(t, roomy, "one lost trial", "default margin report carries a resolution note")

	exact := FormatText(Compare(baseline, current, Options{WarnMargin: 1.0 / 15, FlipThreshold: DefaultFlipThreshold}))
	assert.NotContains(t, exact, "one lost trial", "margin equal to one lost trial carries a resolution note")
}

func TestCompare_ListsUnpairedTasks(t *testing.T) {
	baseline := singleRunSuite("base", "pass", "pass", "pass", "pass", "pass")
	current := trialSuite("curr", 3, []int{3, 3, 3, 3})
	current.Tasks[1].TaskID = "zz-new"
	current.Tasks[3].TaskID = "aa-new"

	report := Compare(baseline, current, DefaultOptions())
	assert.Equal(t, []string{"tb", "td", "te"}, report.BaselineOnly)
	assert.Equal(t, []string{"aa-new", "zz-new"}, report.CurrentOnly)
	assert.Len(t, report.Tasks, 2)

	text := FormatText(report)
	for _, want := range []string{
		"Missing from current run (3): tb, td, te",
		"New in current run (2): aa-new, zz-new",
	} {
		assert.Contains(t, text, want)
	}

	data, err := json.Marshal(report)
	require.NoError(t, err, "marshal")
	var round eval.ComparisonReport
	require.NoError(t, json.Unmarshal(data, &round), "unmarshal")
	assert.Equal(t, []string{"tb", "td", "te"}, round.BaselineOnly, "round trip lost the baseline-only list")
	assert.Equal(t, []string{"aa-new", "zz-new"}, round.CurrentOnly, "round trip lost the current-only list")
}

func TestCompare_UnpairedListsAreEmptyArraysWhenEveryTaskPairs(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3}), DefaultOptions())
	data, err := json.Marshal(report)
	require.NoError(t, err, "marshal")
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &doc))
	for _, key := range []string{"baselineOnly", "currentOnly"} {
		t.Run(key, func(t *testing.T) {
			assert.Equal(t, "[]", string(doc[key]))
		})
	}

	text := FormatText(report)
	assert.NotContains(t, text, "Missing from current run", "report lists unpaired tasks when every task pairs")
	assert.NotContains(t, text, "New in current run", "report lists unpaired tasks when every task pairs")
}

// Three of five tasks lose one trial each: the two-sided 95% interval
// straddles zero, yet the one-sided 95% upper bound sits below it.
func TestCompare_BlocksOnOneSidedUpperBound(t *testing.T) {
	baseline := singleRunSuite("base", "pass", "pass", "pass", "pass", "pass")
	report := Compare(baseline, trialSuite("curr", 3, []int{2, 2, 2, 3, 3}), DefaultOptions())
	s := report.Summary

	require.Equal(t, eval.GateBlock, s.Gate, "reasons %v", s.GateReasons)
	assert.Empty(t, report.Regressions, "the bound alone blocks")
	assert.Empty(t, s.DeterministicFlips, "the bound alone blocks")
	p := s.Paired
	within(t, "d-bar", p.MeanDelta, -0.2, 1e-9)
	within(t, "paired SE", p.StdErr, 0.0816, 0.0001)
	within(t, "upper bound", p.UpperBound, -0.0259, 0.0005)
	within(t, "CI low", p.CILow, -0.4267, 0.0005)
	within(t, "CI high", p.CIHigh, 0.0267, 0.0005)
	within(t, "sign-flip p", p.PValue, 0.25, 1e-12)

	reasons := strings.Join(s.GateReasons, "\n")
	assert.Contains(t, reasons, "one-sided 95% upper bound on the mean delta is -0.026, below 0")
	text := FormatText(report)
	assert.Contains(t, text, "Gate: BLOCK")
	assert.Contains(t, text, "two-sided 95% t(4) CI [-0.427, +0.027]")
	assert.Contains(t, text, "one-sided 95% upper bound -0.026")
	assert.NotContains(t, text, "confirmed", "block report claims a confirmed regression")
}

func TestNoiseFloor_AllPassPoolReportsInsufficientVariation(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3, 3, 3}), DefaultOptions())
	nf := report.Summary.NoiseFloor
	require.NotNil(t, nf, "noise floor missing for an all-pass baseline")
	assert.InDelta(t, 1.0, nf.PerTrialPassRate, 1e-12, "noise floor %+v", nf)
	assert.Equal(t, 20, nf.PooledTrials, "noise floor %+v", nf)
	assert.Nil(t, nf.SingleRunFalseAlarm, "false-alarm rates reported for an all-pass pool: %+v", nf)
	assert.Nil(t, nf.FlipRuleFalseAlarm, "false-alarm rates reported for an all-pass pool: %+v", nf)

	text := FormatText(report)
	assert.Contains(t, text, "insufficient variation")
	assert.NotContains(t, text, "0.00%", "report prints a 0.00 percent false-alarm rate")

	data, err := json.Marshal(report.Summary.NoiseFloor)
	require.NoError(t, err, "marshal")
	assert.NotContains(t, string(data), "FalseAlarm", "noise floor JSON carries a false-alarm rate")
}

func TestNoiseFloor_PoolWithAFailurePrintsBothRates(t *testing.T) {
	report := Compare(singleRunSuite("base", "pass", "pass", "pass", "pass", "pass"), trialSuite("curr", 3, []int{3, 3, 3, 3, 2}), DefaultOptions())
	text := FormatText(report)
	assert.Contains(t, text, "per-trial pass rate 0.950 over 20 pooled trials")
	assert.Contains(t, text, "a single-run flip gate false-alarms on 22.62% of pushes, the 3-of-3 flip rule on 0.06%")
	assert.NotContains(t, text, "insufficient variation", "report flags insufficient variation despite a failure")
}

func TestRateSummary_WilsonIntervalOverTasks(t *testing.T) {
	// Pass fractions 1, 1, 2/3, 1/3, 1/3 give 10/3 successes over 5 tasks.
	s := rateSummary(trialSuite("r", 3, []int{3, 3, 2, 1, 1}))
	within(t, "pass rate", s.PassRate, 2.0/3, 1e-12)
	within(t, "Wilson low", s.WilsonLow, 0.27520, 0.0001)
	within(t, "Wilson high", s.WilsonHigh, 0.91331, 0.0001)

	threeOfFive := rateSummary(trialSuite("r", 3, []int{3, 3, 3, 0, 0}))
	within(t, "3/5 Wilson low", threeOfFive.WilsonLow, 0.23072, 0.0001)
	within(t, "3/5 Wilson high", threeOfFive.WilsonHigh, 0.88238, 0.0001)
}

func TestRateSummary_PassHatKStopsAtTheSmallestTrialCount(t *testing.T) {
	r := eval.SuiteResult{RunID: "mixed", Tasks: []eval.TaskResult{
		trialTask("a", 2, 2),
		trialTask("b", 2, 3),
		trialTask("c", 3, 3),
	}}
	s := rateSummary(r)

	require.Len(t, s.PassHatK, 2, "pass^k rows should stop at the smallest trial count")
	require.Len(t, s.PassAtK, 2, "pass@k rows should stop at the smallest trial count")
	within(t, "pass^1", s.PassHatK[0], 8.0/9, 1e-12)
	within(t, "pass^2", s.PassHatK[1], 7.0/9, 1e-12)
	within(t, "pass@2", s.PassAtK[1], 1, 1e-12)
}

const workedExampleReportDefault = `Eval Comparison: current-run vs baseline-run

Gate: PASS

Pass Rate: 60.0% → 50.0% (-10.0%)
  baseline: 60.0% (95% Wilson over tasks [31.3%, 83.2%]), SE 0.130, n=10 tasks, K=3
            pass^k (k=1..3): 0.600, 0.467, 0.400; pass@k: 0.600, 0.733, 0.800
  current:  50.0% (95% Wilson over tasks [23.7%, 76.3%]), SE 0.114, n=10 tasks, K=3
            pass^k (k=1..3): 0.500, 0.300, 0.200; pass@k: 0.500, 0.700, 0.800
Paired: n=10, mean delta -0.100, SE 0.0711, two-sided 95% t(9) CI [-0.261, +0.061], one-sided 95% upper bound +0.030, sign-flip p 0.375 (exact), MDE 0.22

No regressions found.
`

func TestFormatText_WorkedExampleGolden(t *testing.T) {
	baseline, current := trialSuite("baseline-run", 3, workedA), trialSuite("current-run", 3, workedB)

	got := FormatText(Compare(baseline, current, DefaultOptions()))
	assert.Equal(t, workedExampleReportDefault, got, "default-options report differs from the golden text")

	tight := FormatText(Compare(baseline, current, Options{WarnMargin: 0.05, FlipThreshold: DefaultFlipThreshold}))
	wantTight := strings.Replace(workedExampleReportDefault, "Gate: PASS\n",
		"Gate: WARN\n  - mean delta -0.100 is below -0.050 and the one-sided 95% upper bound +0.030 is not below 0\n", 1)
	assert.Equal(t, wantTight, tight, "0.05-margin report differs from the golden text")
}

// TestFormatText_WorkedExampleMatchesDocs keeps the report block in
// docs/eval.md identical to what the code prints for the worked example.
func TestFormatText_WorkedExampleMatchesDocs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "eval.md"))
	require.NoError(t, err)
	assert.Contains(t, string(doc), strings.TrimRight(workedExampleReportDefault, "\n"),
		"docs/eval.md does not contain the worked-example report printed by FormatText")
}
