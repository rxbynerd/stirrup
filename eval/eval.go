// Package eval implements the evaluation framework for the stirrup harness.
// It provides deterministic replay, judging, comparison reporting, and a CLI
// for running eval suites against recorded or live harness runs.
package eval

import (
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TaskResult captures the outcome of evaluating a single EvalTask.
type TaskResult struct {
	TaskID       string          `json:"taskId"`
	Outcome      string          `json:"outcome"` // "pass" | "fail" | "error"
	Trace        *types.RunTrace `json:"trace,omitempty"`
	JudgeVerdict JudgeVerdict    `json:"judgeVerdict"`
	Error        string          `json:"error,omitempty"`
	DurationMs   int64           `json:"durationMs"`
}

// JudgeVerdict is the result of applying an EvalJudge to a run.
type JudgeVerdict struct {
	Passed  bool          `json:"passed"`
	Reason  string        `json:"reason"`
	Details []JudgeDetail `json:"details,omitempty"`
}

// JudgeDetail records the verdict of a single sub-judge in a composite.
type JudgeDetail struct {
	Type   string `json:"type"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason"`
}

// SuiteResult captures the outcome of evaluating an entire EvalSuite.
type SuiteResult struct {
	SuiteID     string       `json:"suiteId"`
	RunID       string       `json:"runId"`
	StartedAt   time.Time    `json:"startedAt"`
	CompletedAt time.Time    `json:"completedAt"`
	Tasks       []TaskResult `json:"tasks"`
	PassRate    float64      `json:"passRate"`
}

// ComparisonReport diffs two SuiteResults and flags regressions.
type ComparisonReport struct {
	CurrentID    string            `json:"currentId"`
	BaselineID   string            `json:"baselineId"`
	Regressions  []TaskRegression  `json:"regressions,omitempty"`
	Improvements []TaskImprovement `json:"improvements,omitempty"`
	Summary      ComparisonSummary `json:"summary"`
}

// TaskRegression records a task that got worse between baseline and current.
type TaskRegression struct {
	TaskID          string `json:"taskId"`
	BaselineOutcome string `json:"baselineOutcome"`
	CurrentOutcome  string `json:"currentOutcome"`
	TurnsDelta      int    `json:"turnsDelta,omitempty"`
}

// TaskImprovement records a task that got better between baseline and current.
type TaskImprovement struct {
	TaskID          string `json:"taskId"`
	BaselineOutcome string `json:"baselineOutcome"`
	CurrentOutcome  string `json:"currentOutcome"`
	TurnsDelta      int    `json:"turnsDelta,omitempty"`
}

// ComparisonSummary provides aggregate metrics for the comparison.
type ComparisonSummary struct {
	BaselinePassRate float64 `json:"baselinePassRate"`
	CurrentPassRate  float64 `json:"currentPassRate"`
	PassRateDelta    float64 `json:"passRateDelta"`
	HasRegressions   bool    `json:"hasRegressions"`
}
