package reporter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

const epsilon = 0.001

// task builds a minimal TaskResult.
func task(id, outcome string, trace *types.RunTrace) eval.TaskResult {
	return eval.TaskResult{
		TaskID:  id,
		Outcome: outcome,
		Trace:   trace,
	}
}

func trace(turns int) *types.RunTrace {
	return &types.RunTrace{Turns: turns}
}

func suite(runID string, tasks ...eval.TaskResult) eval.SuiteResult {
	return eval.SuiteResult{
		RunID: runID,
		Tasks: tasks,
	}
}

func TestCompare_CountsRegressionsAndImprovements(t *testing.T) {
	tests := []struct {
		name             string
		baseline         eval.SuiteResult
		current          eval.SuiteResult
		wantRegressions  int
		wantImprovements int
		wantHasRegress   bool
	}{
		{
			name:             "identical results have no changes",
			baseline:         suite("base", task("a", "pass", nil), task("b", "fail", nil)),
			current:          suite("curr", task("a", "pass", nil), task("b", "fail", nil)),
			wantRegressions:  0,
			wantImprovements: 0,
			wantHasRegress:   false,
		},
		{
			name:             "pass to fail is a regression",
			baseline:         suite("base", task("a", "pass", nil)),
			current:          suite("curr", task("a", "fail", nil)),
			wantRegressions:  1,
			wantImprovements: 0,
			wantHasRegress:   true,
		},
		{
			name:             "pass to error is a regression",
			baseline:         suite("base", task("a", "pass", nil)),
			current:          suite("curr", task("a", "error", nil)),
			wantRegressions:  1,
			wantImprovements: 0,
			wantHasRegress:   true,
		},
		{
			name:             "fail to pass is an improvement",
			baseline:         suite("base", task("a", "fail", nil)),
			current:          suite("curr", task("a", "pass", nil)),
			wantRegressions:  0,
			wantImprovements: 1,
			wantHasRegress:   false,
		},
		{
			name:             "error to pass is an improvement",
			baseline:         suite("base", task("a", "error", nil)),
			current:          suite("curr", task("a", "pass", nil)),
			wantRegressions:  0,
			wantImprovements: 1,
			wantHasRegress:   false,
		},
		{
			name: "mixed regressions and improvements",
			baseline: suite("base",
				task("a", "pass", nil),
				task("b", "fail", nil),
				task("c", "pass", nil),
			),
			current: suite("curr",
				task("a", "fail", nil),
				task("b", "pass", nil),
				task("c", "pass", nil),
			),
			wantRegressions:  1,
			wantImprovements: 1,
			wantHasRegress:   true,
		},
		{
			name:             "baseline has tasks missing from current",
			baseline:         suite("base", task("a", "pass", nil), task("b", "pass", nil)),
			current:          suite("curr", task("a", "pass", nil)),
			wantRegressions:  0,
			wantImprovements: 0,
			wantHasRegress:   false,
		},
		{
			name:             "current has tasks missing from baseline",
			baseline:         suite("base", task("a", "pass", nil)),
			current:          suite("curr", task("a", "pass", nil), task("b", "fail", nil)),
			wantRegressions:  0,
			wantImprovements: 0,
			wantHasRegress:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			report := Compare(tt.baseline, tt.current, DefaultOptions())

			must.Len(report.Regressions, tt.wantRegressions, "regressions")
			must.Len(report.Improvements, tt.wantImprovements, "improvements")
			must.Equal(tt.wantHasRegress, report.Summary.HasRegressions, "HasRegressions")
		})
	}
}

func TestCompare_ReportsTurnDeltas(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	baseline := suite("base",
		task("a", "pass", trace(5)),
		task("b", "fail", trace(3)),
	)
	current := suite("curr",
		task("a", "fail", trace(8)),
		task("b", "pass", trace(2)),
	)

	report := Compare(baseline, current, DefaultOptions())

	must.Len(report.Regressions, 1)
	reg := report.Regressions[0]
	check.Equal("a", reg.TaskID, "regression TaskID")
	check.Equal(3, reg.TurnsDelta, "regression TurnsDelta")

	must.Len(report.Improvements, 1)
	imp := report.Improvements[0]
	check.Equal("b", imp.TaskID, "improvement TaskID")
	check.Equal(-1, imp.TurnsDelta, "improvement TurnsDelta")
}

func TestCompare_NilBaselineTraceZeroTurnsDelta(t *testing.T) {
	must := require.New(t)
	baseline := suite("base", task("a", "pass", nil))
	current := suite("curr", task("a", "fail", trace(5)))

	report := Compare(baseline, current, DefaultOptions())

	must.Len(report.Regressions, 1)
	must.Equal(0, report.Regressions[0].TurnsDelta, "nil baseline trace should give a zero turns delta")
}

func TestCompare_SummaryPassRates(t *testing.T) {
	check := assert.New(t)
	baseline := suite("base",
		task("a", "pass", trace(5)),
		task("b", "fail", trace(3)),
	)
	current := suite("curr",
		task("a", "pass", trace(4)),
		task("b", "pass", trace(2)),
	)

	report := Compare(baseline, current, DefaultOptions())
	s := report.Summary

	// Baseline: 1/2 pass = 0.5, Current: 2/2 pass = 1.0
	check.InDelta(0.5, s.BaselinePassRate, epsilon, "BaselinePassRate")
	check.InDelta(1.0, s.CurrentPassRate, epsilon, "CurrentPassRate")
	check.InDelta(0.5, s.PassRateDelta, epsilon, "PassRateDelta")
	check.False(s.HasRegressions, "HasRegressions should be false when there are no regressions")
}

func TestFormatText_NoRegressions(t *testing.T) {
	check := assert.New(t)
	report := eval.ComparisonReport{
		CurrentID:  "run-2",
		BaselineID: "run-1",
		Improvements: []eval.TaskImprovement{
			{TaskID: "task-bar", BaselineOutcome: "fail", CurrentOutcome: "pass"},
		},
		Summary: eval.ComparisonSummary{
			BaselinePassRate: 0.5,
			CurrentPassRate:  1.0,
			PassRateDelta:    0.5,
			HasRegressions:   false,
		},
	}

	got := FormatText(report)

	check.Contains(got, "Eval Comparison: run-2 vs run-1")
	check.Contains(got, "No regressions found.", "should show 'No regressions found.' when there are none")
	check.Contains(got, "Improvements (1):")
	check.Contains(got, "task-bar: fail → pass")
}

func TestFormatText_WithRegressions(t *testing.T) {
	check := assert.New(t)
	report := eval.ComparisonReport{
		CurrentID:  "run-2",
		BaselineID: "run-1",
		Regressions: []eval.TaskRegression{
			{TaskID: "task-foo", BaselineOutcome: "pass", CurrentOutcome: "fail"},
		},
		Summary: eval.ComparisonSummary{
			BaselinePassRate: 1.0,
			CurrentPassRate:  0.0,
			PassRateDelta:    -1.0,
			HasRegressions:   true,
		},
	}

	got := FormatText(report)

	check.Contains(got, "Regressions (1):")
	check.Contains(got, "task-foo: pass → fail")
	check.NotContains(got, "No regressions found.", "should not show 'No regressions found.' when there are regressions")
}
