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
	Passed bool `json:"passed"`

	// Status is types.JudgeStatusPass, JudgeStatusFail or JudgeStatusError
	// and is always consistent with Passed: only "pass" sets Passed. An
	// "error" verdict means the judge could not rule, which is distinct
	// from the criteria not being met. Empty only on results written
	// before the field existed.
	Status string `json:"status,omitempty"`

	Reason  string        `json:"reason"`
	Details []JudgeDetail `json:"details,omitempty"`

	// Record is the provenance of the LLM call behind the verdict. Nil for
	// deterministic judges.
	Record *types.JudgeRecord `json:"record,omitempty"`
}

// JudgeStatusSkipped is the JudgeDetail.Status of a sub-judge a composite did
// not evaluate because an earlier sub-judge had already decided the outcome.
const JudgeStatusSkipped = "skipped"

// JudgeStatusShadow is the JudgeDetail.Status of an evaluated shadow
// sub-judge, whose verdict is recorded in ShadowVerdict and never counts
// toward the composite's outcome.
const JudgeStatusShadow = "shadow"

// JudgeDetail records the verdict of a single sub-judge in a composite.
type JudgeDetail struct {
	Type   string `json:"type"`
	Passed bool   `json:"passed"`

	// Status is types.JudgeStatusPass, JudgeStatusFail or JudgeStatusError
	// for an evaluated sub-judge, JudgeStatusShadow for an evaluated shadow
	// sub-judge, or JudgeStatusSkipped for one that was not evaluated. Only
	// "pass" sets Passed.
	Status string `json:"status,omitempty"`

	// ShadowVerdict is the pass, fail or error status a shadow sub-judge
	// reached. Empty for every other sub-judge.
	ShadowVerdict string `json:"shadowVerdict,omitempty"`

	Reason string `json:"reason"`

	// Record is the provenance of the LLM call behind the sub-judge's
	// verdict, including an error verdict. Nil for deterministic,
	// composite and skipped sub-judges.
	Record *types.JudgeRecord `json:"record,omitempty"`

	// Details holds the entries of a nested composite sub-judge, so the
	// records of judges at any depth stay reachable. Empty for every other
	// sub-judge.
	Details []JudgeDetail `json:"details,omitempty"`
}

// SuiteResult captures the outcome of evaluating an entire EvalSuite.
type SuiteResult struct {
	SuiteID     string       `json:"suiteId"`
	RunID       string       `json:"runId"`
	StartedAt   time.Time    `json:"startedAt"`
	CompletedAt time.Time    `json:"completedAt"`
	Tasks       []TaskResult `json:"tasks"`
	PassRate    float64      `json:"passRate"`

	// JudgeCache counts how diff-review verdicts used the judge cache. Nil
	// when the invocation ran live.
	JudgeCache *JudgeCacheSummary `json:"judgeCache,omitempty"`
}

// JudgeCacheSummary tallies the judge cache outcomes of one invocation, one
// count per diff-review verdict that carries a record. Misses include the
// verdicts that were then stored.
type JudgeCacheSummary struct {
	Mode     string `json:"mode"`
	Hits     int    `json:"hits"`
	Misses   int    `json:"misses"`
	Stored   int    `json:"stored"`
	Bypassed int    `json:"bypassed"`

	// Replaced counts unusable entries that read-through found and judged
	// again in their place. Each is also a miss.
	Replaced int `json:"replaced,omitempty"`

	// WriteErrors counts verdicts the cache failed to store.
	WriteErrors int `json:"writeErrors,omitempty"`
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
