// Package reporter implements comparison reporting for eval suite results.
// It diffs a current SuiteResult against a baseline and flags regressions
// and improvements.
package reporter

import (
	"github.com/rxbynerd/stirrup/eval"
)

// Compare diffs a current SuiteResult against a baseline and produces a
// ComparisonReport. Tasks present in one suite but not the other are silently
// skipped — only tasks that appear in both are compared.
func Compare(baseline, current eval.SuiteResult) eval.ComparisonReport {
	baselineByID := indexByTaskID(baseline.Tasks)

	var regressions []eval.TaskRegression
	var improvements []eval.TaskImprovement

	for i := range current.Tasks {
		ct := &current.Tasks[i]
		bt, exists := baselineByID[ct.TaskID]
		if !exists {
			continue
		}

		if isPass(bt.Outcome) && isFail(ct.Outcome) {
			regressions = append(regressions, eval.TaskRegression{
				TaskID:          ct.TaskID,
				BaselineOutcome: bt.Outcome,
				CurrentOutcome:  ct.Outcome,
				CostDelta:       costDelta(bt, ct),
				TurnsDelta:      turnsDelta(bt, ct),
			})
		} else if isFail(bt.Outcome) && isPass(ct.Outcome) {
			improvements = append(improvements, eval.TaskImprovement{
				TaskID:          ct.TaskID,
				BaselineOutcome: bt.Outcome,
				CurrentOutcome:  ct.Outcome,
				CostDelta:       costDelta(bt, ct),
				TurnsDelta:      turnsDelta(bt, ct),
			})
		}
	}

	return eval.ComparisonReport{
		CurrentID:    current.RunID,
		BaselineID:   baseline.RunID,
		Regressions:  regressions,
		Improvements: improvements,
		Summary:      computeSummary(baseline, current, regressions),
	}
}

// indexByTaskID builds a lookup map from TaskID to TaskResult.
func indexByTaskID(tasks []eval.TaskResult) map[string]*eval.TaskResult {
	m := make(map[string]*eval.TaskResult, len(tasks))
	for i := range tasks {
		m[tasks[i].TaskID] = &tasks[i]
	}
	return m
}

// isPass returns true if the outcome represents a passing state.
func isPass(outcome string) bool {
	return outcome == "pass"
}

// isFail returns true if the outcome represents a non-passing state.
func isFail(outcome string) bool {
	return outcome == "fail" || outcome == "error"
}

// costDelta computes (current cost - baseline cost) from RunTrace, returning
// 0 if either trace is nil.
func costDelta(baseline, current *eval.TaskResult) float64 {
	if baseline.Trace == nil || current.Trace == nil {
		return 0
	}
	return current.Trace.Cost - baseline.Trace.Cost
}

// turnsDelta computes (current turns - baseline turns) from RunTrace,
// returning 0 if either trace is nil.
func turnsDelta(baseline, current *eval.TaskResult) int {
	if baseline.Trace == nil || current.Trace == nil {
		return 0
	}
	return current.Trace.Turns - baseline.Trace.Turns
}

// passRate computes the fraction of tasks with outcome "pass".
func passRate(tasks []eval.TaskResult) float64 {
	if len(tasks) == 0 {
		return 0
	}
	passed := 0
	for _, t := range tasks {
		if isPass(t.Outcome) {
			passed++
		}
	}
	return float64(passed) / float64(len(tasks))
}

// totalCost sums the cost from all task RunTraces.
func totalCost(tasks []eval.TaskResult) float64 {
	var sum float64
	for _, t := range tasks {
		if t.Trace != nil {
			sum += t.Trace.Cost
		}
	}
	return sum
}

// computeSummary builds aggregate comparison metrics.
func computeSummary(baseline, current eval.SuiteResult, regressions []eval.TaskRegression) eval.ComparisonSummary {
	bpr := passRate(baseline.Tasks)
	cpr := passRate(current.Tasks)
	bc := totalCost(baseline.Tasks)
	cc := totalCost(current.Tasks)

	return eval.ComparisonSummary{
		BaselinePassRate: bpr,
		CurrentPassRate:  cpr,
		PassRateDelta:    cpr - bpr,
		BaselineCost:     bc,
		CurrentCost:      cc,
		CostDelta:        cc - bc,
		HasRegressions:   len(regressions) > 0,
	}
}
