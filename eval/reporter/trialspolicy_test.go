package reporter

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// confirmTask builds a task result from its trial outcomes the way the
// runner does under confirm-on-fail: one outcome keeps the single-run
// shape, several are kept as trials and decided by majority.
func confirmTask(id string, outcomes ...string) eval.TaskResult {
	if len(outcomes) == 1 {
		r := eval.TaskResult{TaskID: id, Outcome: outcomes[0]}
		r.PassFraction = r.Counts().PassFraction()
		return r
	}
	r := eval.TaskResult{TaskID: id}
	for i, o := range outcomes {
		r.Trials = append(r.Trials, eval.TrialResult{Trial: i + 1, Outcome: o})
	}
	c := r.Counts()
	r.Outcome = c.Outcome()
	r.PassFraction = c.PassFraction()
	return r
}

func confirmSuite(runID string, tasks ...eval.TaskResult) eval.SuiteResult {
	return eval.SuiteResult{RunID: runID, Trials: 3, TrialsPolicy: eval.TrialsPolicyConfirmOnFail, Tasks: tasks}
}

func TestCompare_ConfirmOnFailOnBothSidesPairsOnEveryTrial(t *testing.T) {
	must := require.New(t)
	baseline := confirmSuite("base",
		confirmTask("a", "pass"), confirmTask("b", "pass"), confirmTask("c", "pass"), confirmTask("d", "pass"))
	current := confirmSuite("cur",
		confirmTask("a", "fail", "pass", "pass"), confirmTask("b", "pass"), confirmTask("c", "pass"), confirmTask("d", "pass"))

	report := Compare(baseline, current, DefaultOptions())
	s := report.Summary

	must.False(s.FirstTrialOnly)
	must.NotNil(s.Paired)
	within(t, "mean delta over every trial run", s.Paired.MeanDelta, -1.0/12, 1e-12)
	within(t, "paired SE over every trial run", s.Paired.StdErr, 1.0/12, 1e-12)
	must.Equal(eval.GatePass, s.Gate, "a first-trial failure the reruns overturned stays within the warn margin; reasons %v", s.GateReasons)
	within(t, "task delta", report.Tasks[0].Delta, -1.0/3, 1e-12)
	within(t, "current pass rate over every trial run", s.CurrentPassRate, (2.0/3+3)/4, 1e-12)

	must.Len(s.Current.PassHatK, 1, "pass^k stops at the smallest per-task trial count")
	within(t, "current pass^1", s.Current.PassHatK[0], (2.0/3+3)/4, 1e-12)
	must.Equal(eval.TrialsPolicyConfirmOnFail, s.Current.TrialsPolicy)

	must.NotNil(s.NoiseFloor)
	must.Equal(10, s.NoiseFloor.PooledTrials, "the noise floor pools every trial run")
	within(t, "pooled pass rate", s.NoiseFloor.PerTrialPassRate, 9.0/10, 1e-12)

	must.Empty(s.DeterministicFlips, "a confirmed pass is not a flip")
	must.Empty(report.Regressions, "2/3 is above the flip threshold")
	must.NotContains(FormatText(report), "first trial only")
}

func TestCompare_ConfirmOnFailFlipNeedsEveryTrialToFail(t *testing.T) {
	must := require.New(t)
	baseline := confirmSuite("base",
		confirmTask("a", "pass"), confirmTask("b", "pass"), confirmTask("c", "pass"), confirmTask("d", "pass"))
	current := confirmSuite("cur",
		confirmTask("a", "fail", "fail", "fail"), confirmTask("b", "fail", "fail", "pass"), confirmTask("c", "pass"), confirmTask("d", "pass"))

	report := Compare(baseline, current, DefaultOptions())

	must.Equal([]string{"a"}, report.Summary.DeterministicFlips)
	must.Equal(eval.GateBlock, report.Summary.Gate)
	must.Len(report.Regressions, 2, "both tasks are at or below the flip threshold")
}

func TestCompare_FixedPolicyUsesEveryTrial(t *testing.T) {
	must := require.New(t)
	report := Compare(trialSuite("A", 3, []int{3, 3, 3}), trialSuite("B", 3, []int{2, 3, 3}), DefaultOptions())
	s := report.Summary

	must.False(s.FirstTrialOnly)
	must.Equal(eval.TrialsPolicyFixed, s.Baseline.TrialsPolicy, "an unset policy reads as fixed")
	within(t, "mean delta over every trial", s.Paired.MeanDelta, -1.0/9, 1e-12)
	must.Len(s.Current.PassHatK, 3)
	must.NotContains(FormatText(report), "first trial only")
}

func TestCompare_MixedPoliciesPairOnTheFirstTrialOfBothSides(t *testing.T) {
	must := require.New(t)
	fixed := trialSuite("A", 3, []int{3, 3, 3})
	fixed.Tasks[0] = trialTask("ta", 2, 3)
	fixed.Tasks[0].Trials[0].Outcome, fixed.Tasks[0].Trials[2].Outcome = "fail", "pass"
	confirm := confirmSuite("B", confirmTask("ta", "pass"), confirmTask("tb", "pass"), confirmTask("tc", "pass"))

	report := Compare(fixed, confirm, DefaultOptions())

	must.True(report.Summary.FirstTrialOnly)
	within(t, "first-trial mean delta", report.Summary.Paired.MeanDelta, 1.0/3, 1e-12)
	text := FormatText(report)
	must.Contains(text, "Warning: trials policies differ (baseline fixed, current confirm-on-fail)")
	must.Contains(text, "K=3 (confirm-on-fail)")
}

func TestCompare_MixedPoliciesPairOnTheFirstTrialWithAConfirmOnFailBaseline(t *testing.T) {
	must := require.New(t)
	confirm := confirmSuite("A", confirmTask("ta", "pass"), confirmTask("tb", "pass"), confirmTask("tc", "pass"))
	fixed := eval.SuiteResult{RunID: "B", Trials: 3, TrialsPolicy: eval.TrialsPolicyFixed, Tasks: []eval.TaskResult{
		confirmTask("ta", "fail", "pass", "pass"), confirmTask("tb", "pass", "pass", "pass"), confirmTask("tc", "pass", "pass", "pass"),
	}}

	report := Compare(confirm, fixed, DefaultOptions())

	must.True(report.Summary.FirstTrialOnly)
	within(t, "first-trial mean delta", report.Summary.Paired.MeanDelta, -1.0/3, 1e-12)
	must.Len(report.Summary.Current.PassHatK, 1, "the fixed side is also paired on its first trial")
	must.Contains(FormatText(report), "Warning: trials policies differ (baseline confirm-on-fail, current fixed)")
}

func TestCompare_ConfirmOnFailPairingFollowsRequireHealthy(t *testing.T) {
	healthyPass := func(id string) eval.TaskResult {
		return eval.TaskResult{TaskID: id, Outcome: "pass", PassFraction: 1, RunHealth: types.RunHealthy}
	}
	healthyFixed := func(id string) eval.TaskResult {
		r := healthyPass(id)
		r.Trials = []eval.TrialResult{
			{Trial: 1, Outcome: "pass", RunHealth: types.RunHealthy},
			{Trial: 2, Outcome: "pass", RunHealth: types.RunHealthy},
			{Trial: 3, Outcome: "pass", RunHealth: types.RunHealthy},
		}
		return r
	}
	confirmBaseline := confirmSuite("base", healthyPass("a"), healthyPass("b"), healthyPass("c"), healthyPass("d"))
	fixedBaseline := eval.SuiteResult{RunID: "base", Trials: 3, TrialsPolicy: eval.TrialsPolicyFixed, Tasks: []eval.TaskResult{
		healthyFixed("a"), healthyFixed("b"), healthyFixed("c"), healthyFixed("d"),
	}}
	confirmed := eval.TaskResult{TaskID: "a", Outcome: "pass", PassFraction: 1, RunHealth: types.RunDegraded, Trials: []eval.TrialResult{
		{Trial: 1, Outcome: "pass", RunHealth: types.RunDegraded},
		{Trial: 2, Outcome: "pass", RunHealth: types.RunHealthy},
		{Trial: 3, Outcome: "pass", RunHealth: types.RunHealthy},
	}}

	cases := []struct {
		name           string
		baseline       eval.SuiteResult
		requireHealthy bool
		wantMeanDelta  float64
	}{
		{"same policy, judge outcome alone", confirmBaseline, false, 0},
		{"same policy, the unhealthy trial is one of three under require_healthy", confirmBaseline, true, -1.0 / 12},
		{"mismatched policies, judge outcome alone", fixedBaseline, false, 0},
		{"mismatched policies, the unhealthy first trial fails under require_healthy", fixedBaseline, true, -0.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			current := confirmSuite("cur", confirmed, healthyPass("b"), healthyPass("c"), healthyPass("d"))
			current.RequireHealthy = tc.requireHealthy

			s := Compare(tc.baseline, current, DefaultOptions()).Summary

			must.NotNil(s.Paired)
			within(t, "mean delta", s.Paired.MeanDelta, tc.wantMeanDelta, 1e-12)
			must.Empty(s.DeterministicFlips, "two healthy confirmation passes")
		})
	}
}

func TestFormatText_NotesTheFirstTrialTreatmentOnlyForMismatchedPolicies(t *testing.T) {
	confirmBaseline := confirmSuite("base", confirmTask("a", "pass"), confirmTask("b", "pass"), confirmTask("c", "pass"))
	fixedBaseline := trialSuite("base", 3, []int{3, 3, 3})
	for i, id := range []string{"a", "b", "c"} {
		fixedBaseline.Tasks[i].TaskID = id
	}
	current := confirmSuite("cur", confirmTask("a", "pass"), confirmTask("b", "pass"), confirmTask("c", "fail", "pass", "pass"))
	note := "Note: the trials policies differ; the paired statistics, the noise floor, and pass^k / pass@k use each task's first trial only."

	t.Run("same policy", func(t *testing.T) {
		must := require.New(t)
		text := FormatText(Compare(confirmBaseline, current, DefaultOptions()))
		must.NotContains(text, note)
		must.NotContains(text, "Warning: trials policies differ")
		must.Contains(text, "K=3 (confirm-on-fail)")
	})

	t.Run("mismatched policies", func(t *testing.T) {
		must := require.New(t)
		text := FormatText(Compare(fixedBaseline, current, DefaultOptions()))
		must.Contains(text, note)
		must.Contains(text, "pass^k (k=1..1)")
		must.Contains(text, "with n=3 paired tasks and K=1,", "one lost first trial is one run of one task")
	})
}

func TestTrialsPolicyMismatch_ErrorsOnlyWhenPoliciesDiffer(t *testing.T) {
	cases := []struct {
		name              string
		baseline, current string
		wantErr           string
	}{
		{"same policy", eval.TrialsPolicyConfirmOnFail, eval.TrialsPolicyConfirmOnFail, ""},
		{"unset matches fixed", "", eval.TrialsPolicyFixed, ""},
		{"fixed against confirm-on-fail", eval.TrialsPolicyFixed, eval.TrialsPolicyConfirmOnFail, "baseline ran fixed, current ran confirm-on-fail"},
		{"confirm-on-fail against unset", eval.TrialsPolicyConfirmOnFail, "", "baseline ran confirm-on-fail, current ran fixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := TrialsPolicyMismatch(eval.SuiteResult{TrialsPolicy: tc.baseline}, eval.SuiteResult{TrialsPolicy: tc.current})
			if tc.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantErr)
		})
	}
}
