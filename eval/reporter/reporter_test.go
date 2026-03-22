package reporter

import (
	"math"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

const epsilon = 0.001

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < epsilon
}

// helper to build a minimal TaskResult.
func task(id, outcome string, trace *types.RunTrace) eval.TaskResult {
	return eval.TaskResult{
		TaskID:  id,
		Outcome: outcome,
		Trace:   trace,
	}
}

func trace(cost float64, turns int) *types.RunTrace {
	return &types.RunTrace{Cost: cost, Turns: turns}
}

func suite(runID string, tasks ...eval.TaskResult) eval.SuiteResult {
	return eval.SuiteResult{
		RunID: runID,
		Tasks: tasks,
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name             string
		baseline         eval.SuiteResult
		current          eval.SuiteResult
		wantRegressions  int
		wantImprovements int
		wantHasRegress   bool
	}{
		{
			name:             "identical results — no changes",
			baseline:         suite("base", task("a", "pass", nil), task("b", "fail", nil)),
			current:          suite("curr", task("a", "pass", nil), task("b", "fail", nil)),
			wantRegressions:  0,
			wantImprovements: 0,
			wantHasRegress:   false,
		},
		{
			name:             "one regression pass to fail",
			baseline:         suite("base", task("a", "pass", nil)),
			current:          suite("curr", task("a", "fail", nil)),
			wantRegressions:  1,
			wantImprovements: 0,
			wantHasRegress:   true,
		},
		{
			name:             "one regression pass to error",
			baseline:         suite("base", task("a", "pass", nil)),
			current:          suite("curr", task("a", "error", nil)),
			wantRegressions:  1,
			wantImprovements: 0,
			wantHasRegress:   true,
		},
		{
			name:             "one improvement fail to pass",
			baseline:         suite("base", task("a", "fail", nil)),
			current:          suite("curr", task("a", "pass", nil)),
			wantRegressions:  0,
			wantImprovements: 1,
			wantHasRegress:   false,
		},
		{
			name:             "one improvement error to pass",
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
			name:             "baseline has tasks not in current",
			baseline:         suite("base", task("a", "pass", nil), task("b", "pass", nil)),
			current:          suite("curr", task("a", "pass", nil)),
			wantRegressions:  0,
			wantImprovements: 0,
			wantHasRegress:   false,
		},
		{
			name:             "current has tasks not in baseline",
			baseline:         suite("base", task("a", "pass", nil)),
			current:          suite("curr", task("a", "pass", nil), task("b", "fail", nil)),
			wantRegressions:  0,
			wantImprovements: 0,
			wantHasRegress:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := Compare(tt.baseline, tt.current)

			if got := len(report.Regressions); got != tt.wantRegressions {
				t.Errorf("regressions: got %d, want %d", got, tt.wantRegressions)
			}
			if got := len(report.Improvements); got != tt.wantImprovements {
				t.Errorf("improvements: got %d, want %d", got, tt.wantImprovements)
			}
			if report.Summary.HasRegressions != tt.wantHasRegress {
				t.Errorf("HasRegressions: got %v, want %v", report.Summary.HasRegressions, tt.wantHasRegress)
			}
		})
	}
}

func TestCompare_CostAndTurnDeltas(t *testing.T) {
	baseline := suite("base",
		task("a", "pass", trace(1.00, 5)),
		task("b", "fail", trace(0.50, 3)),
	)
	current := suite("curr",
		task("a", "fail", trace(1.20, 8)),
		task("b", "pass", trace(0.30, 2)),
	)

	report := Compare(baseline, current)

	if len(report.Regressions) != 1 {
		t.Fatalf("expected 1 regression, got %d", len(report.Regressions))
	}
	reg := report.Regressions[0]
	if reg.TaskID != "a" {
		t.Errorf("regression TaskID: got %q, want %q", reg.TaskID, "a")
	}
	wantCostDelta := 0.20
	if diff := reg.CostDelta - wantCostDelta; diff > 0.001 || diff < -0.001 {
		t.Errorf("regression CostDelta: got %.4f, want %.4f", reg.CostDelta, wantCostDelta)
	}
	if reg.TurnsDelta != 3 {
		t.Errorf("regression TurnsDelta: got %d, want %d", reg.TurnsDelta, 3)
	}

	if len(report.Improvements) != 1 {
		t.Fatalf("expected 1 improvement, got %d", len(report.Improvements))
	}
	imp := report.Improvements[0]
	if imp.TaskID != "b" {
		t.Errorf("improvement TaskID: got %q, want %q", imp.TaskID, "b")
	}
	wantImpCostDelta := -0.20
	if diff := imp.CostDelta - wantImpCostDelta; diff > 0.001 || diff < -0.001 {
		t.Errorf("improvement CostDelta: got %.4f, want %.4f", imp.CostDelta, wantImpCostDelta)
	}
	if imp.TurnsDelta != -1 {
		t.Errorf("improvement TurnsDelta: got %d, want %d", imp.TurnsDelta, -1)
	}
}

func TestCompare_NilTraces(t *testing.T) {
	baseline := suite("base", task("a", "pass", nil))
	current := suite("curr", task("a", "fail", trace(1.00, 5)))

	report := Compare(baseline, current)

	if len(report.Regressions) != 1 {
		t.Fatalf("expected 1 regression, got %d", len(report.Regressions))
	}
	if report.Regressions[0].CostDelta != 0 {
		t.Errorf("expected zero CostDelta with nil baseline trace, got %f", report.Regressions[0].CostDelta)
	}
	if report.Regressions[0].TurnsDelta != 0 {
		t.Errorf("expected zero TurnsDelta with nil baseline trace, got %d", report.Regressions[0].TurnsDelta)
	}
}

func TestCompare_Summary(t *testing.T) {
	baseline := suite("base",
		task("a", "pass", trace(1.00, 5)),
		task("b", "fail", trace(0.50, 3)),
	)
	current := suite("curr",
		task("a", "pass", trace(0.80, 4)),
		task("b", "pass", trace(0.40, 2)),
	)

	report := Compare(baseline, current)
	s := report.Summary

	// Baseline: 1/2 pass = 0.5, Current: 2/2 pass = 1.0
	if !approxEqual(s.BaselinePassRate, 0.5) {
		t.Errorf("BaselinePassRate: got %f, want 0.5", s.BaselinePassRate)
	}
	if !approxEqual(s.CurrentPassRate, 1.0) {
		t.Errorf("CurrentPassRate: got %f, want 1.0", s.CurrentPassRate)
	}
	if !approxEqual(s.PassRateDelta, 0.5) {
		t.Errorf("PassRateDelta: got %f, want 0.5", s.PassRateDelta)
	}
	if !approxEqual(s.BaselineCost, 1.50) {
		t.Errorf("BaselineCost: got %f, want 1.50", s.BaselineCost)
	}
	if !approxEqual(s.CurrentCost, 1.20) {
		t.Errorf("CurrentCost: got %f, want 1.20", s.CurrentCost)
	}
	if !approxEqual(s.CostDelta, -0.30) {
		t.Errorf("CostDelta: got %f, want -0.30", s.CostDelta)
	}
	if s.HasRegressions {
		t.Error("HasRegressions should be false when there are no regressions")
	}
}

func TestFormatText_NoRegressions(t *testing.T) {
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
			BaselineCost:     1.50,
			CurrentCost:      1.20,
			CostDelta:        -0.30,
			HasRegressions:   false,
		},
	}

	got := FormatText(report)

	if !strings.Contains(got, "Eval Comparison: run-2 vs run-1") {
		t.Error("missing header")
	}
	if !strings.Contains(got, "No regressions found.") {
		t.Error("should show 'No regressions found.' when there are none")
	}
	if !strings.Contains(got, "Improvements (1):") {
		t.Error("missing improvements section")
	}
	if !strings.Contains(got, "task-bar: fail → pass") {
		t.Error("missing improvement detail")
	}
	if !strings.Contains(got, "-$0.30") {
		t.Error("missing negative cost delta")
	}
}

func TestFormatText_WithRegressions(t *testing.T) {
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
			BaselineCost:     1.00,
			CurrentCost:      1.50,
			CostDelta:        0.50,
			HasRegressions:   true,
		},
	}

	got := FormatText(report)

	if !strings.Contains(got, "Regressions (1):") {
		t.Error("missing regressions header")
	}
	if !strings.Contains(got, "task-foo: pass → fail") {
		t.Error("missing regression detail")
	}
	if strings.Contains(got, "No regressions found.") {
		t.Error("should not show 'No regressions found.' when there are regressions")
	}
}
