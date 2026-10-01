// Package eval implements the evaluation framework for the stirrup harness.
// It provides deterministic replay, judging, comparison reporting, and a CLI
// for running eval suites against recorded or live harness runs.
package eval

import (
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TaskResult captures the outcome of evaluating a single EvalTask. A task
// run more than once carries every run in Trials; the task-level Outcome,
// Trace, JudgeVerdict, and Error then summarise them (see docs/eval.md).
type TaskResult struct {
	TaskID       string          `json:"taskId"`
	Outcome      string          `json:"outcome"` // "pass" | "fail" | "error"
	Trace        *types.RunTrace `json:"trace,omitempty"`
	JudgeVerdict JudgeVerdict    `json:"judgeVerdict"`
	Error        string          `json:"error,omitempty"`
	DurationMs   int64           `json:"durationMs"`
	Trials       []TrialResult   `json:"trials,omitempty"`
	PassFraction float64         `json:"passFraction"`
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

// TrialResult records one independent run of a task.
type TrialResult struct {
	Trial        int          `json:"trial"` // 1-based
	Outcome      string       `json:"outcome"`
	JudgeVerdict JudgeVerdict `json:"judgeVerdict"`
	Error        string       `json:"error,omitempty"`
	DurationMs   int64        `json:"durationMs"`
	Turns        int          `json:"turns"`
}

// TrialCounts tallies a task's trial outcomes. Any outcome other than
// "pass" or "fail" counts as an error.
type TrialCounts struct {
	Pass  int `json:"pass"`
	Fail  int `json:"fail"`
	Error int `json:"error"`
}

// Counts tallies the task's trials. A result without Trials (a single run,
// or a result file written before trials existed) is one trial whose
// outcome is Outcome.
func (t TaskResult) Counts() TrialCounts {
	var c TrialCounts
	if len(t.Trials) == 0 {
		c.add(t.Outcome)
		return c
	}
	for _, tr := range t.Trials {
		c.add(tr.Outcome)
	}
	return c
}

func (c *TrialCounts) add(outcome string) {
	switch outcome {
	case "pass":
		c.Pass++
	case "fail":
		c.Fail++
	default:
		c.Error++
	}
}

// Total is the number of trials tallied.
func (c TrialCounts) Total() int {
	return c.Pass + c.Fail + c.Error
}

// PassFraction is passes divided by trials; errors count as non-passes.
func (c TrialCounts) PassFraction() float64 {
	if c.Total() == 0 {
		return 0
	}
	return float64(c.Pass) / float64(c.Total())
}

// Outcome applies the majority rule: "pass" when more than half the
// trials passed, "fail" when more than half failed, otherwise "error".
// Errors never count toward "fail", so infrastructure or judge errors
// cannot confirm a failure, and a split with no strict majority (including
// any even-K tie) is reported as "error" rather than guessed.
func (c TrialCounts) Outcome() string {
	n := c.Total()
	switch {
	case 2*c.Pass > n:
		return "pass"
	case 2*c.Fail > n:
		return "fail"
	default:
		return "error"
	}
}

// SuiteResult captures the outcome of evaluating an entire EvalSuite.
// PassRate is the mean of the per-task PassFraction values, which equals
// the fraction of passing tasks when every task ran once. Trials is the
// number of runs per task; zero (a result written before trials existed)
// means one.
type SuiteResult struct {
	SuiteID     string       `json:"suiteId"`
	RunID       string       `json:"runId"`
	StartedAt   time.Time    `json:"startedAt"`
	CompletedAt time.Time    `json:"completedAt"`
	Tasks       []TaskResult `json:"tasks"`
	PassRate    float64      `json:"passRate"`
	Trials      int          `json:"trials,omitempty"`
}

// ComparisonReport diffs two SuiteResults and flags regressions.
// BaselineOnly and CurrentOnly list, sorted, the task IDs present on one
// side only; they are always emitted, as empty arrays when there are none.
type ComparisonReport struct {
	CurrentID    string            `json:"currentId"`
	BaselineID   string            `json:"baselineId"`
	Regressions  []TaskRegression  `json:"regressions,omitempty"`
	Improvements []TaskImprovement `json:"improvements,omitempty"`
	Tasks        []TaskComparison  `json:"tasks,omitempty"`
	BaselineOnly []string          `json:"baselineOnly"`
	CurrentOnly  []string          `json:"currentOnly"`
	Summary      ComparisonSummary `json:"summary"`
}

// TaskRegression records a task that got worse between baseline and current.
type TaskRegression struct {
	TaskID               string  `json:"taskId"`
	BaselineOutcome      string  `json:"baselineOutcome"`
	CurrentOutcome       string  `json:"currentOutcome"`
	BaselinePassFraction float64 `json:"baselinePassFraction"`
	CurrentPassFraction  float64 `json:"currentPassFraction"`
	TurnsDelta           int     `json:"turnsDelta,omitempty"`
}

// TaskImprovement records a task that got better between baseline and current.
type TaskImprovement struct {
	TaskID               string  `json:"taskId"`
	BaselineOutcome      string  `json:"baselineOutcome"`
	CurrentOutcome       string  `json:"currentOutcome"`
	BaselinePassFraction float64 `json:"baselinePassFraction"`
	CurrentPassFraction  float64 `json:"currentPassFraction"`
	TurnsDelta           int     `json:"turnsDelta,omitempty"`
}

// TaskComparison pairs one task's pass fractions across the two results.
type TaskComparison struct {
	TaskID               string  `json:"taskId"`
	BaselinePassFraction float64 `json:"baselinePassFraction"`
	CurrentPassFraction  float64 `json:"currentPassFraction"`
	BaselineTrials       int     `json:"baselineTrials"`
	CurrentTrials        int     `json:"currentTrials"`
	Delta                float64 `json:"delta"`
}

// Gate decisions reported in ComparisonSummary.Gate.
const (
	GatePass         = "pass"
	GateWarn         = "warn"
	GateBlock        = "block"
	GateInconclusive = "inconclusive"
)

// ComparisonSummary provides aggregate metrics and the gate decision for
// the comparison. The statistics are defined in docs/eval.md#statistics.
type ComparisonSummary struct {
	BaselinePassRate float64 `json:"baselinePassRate"`
	CurrentPassRate  float64 `json:"currentPassRate"`
	PassRateDelta    float64 `json:"passRateDelta"`
	HasRegressions   bool    `json:"hasRegressions"`

	Gate               string   `json:"gate"`
	GateReasons        []string `json:"gateReasons,omitempty"`
	WarnMargin         float64  `json:"warnMargin"`
	FlipThreshold      float64  `json:"flipThreshold"`
	DeterministicFlips []string `json:"deterministicFlips,omitempty"`

	Baseline   RateSummary    `json:"baseline"`
	Current    RateSummary    `json:"current"`
	Paired     *PairedSummary `json:"paired,omitempty"`
	NoiseFloor *NoiseFloor    `json:"noiseFloor,omitempty"`
}

// RateSummary describes one side of a comparison over all of its tasks.
// StdErr is nil with fewer than two tasks. PassHatK[k-1] and PassAtK[k-1]
// hold the pass^k and pass@k estimates for k up to the smallest per-task
// trial count.
type RateSummary struct {
	Tasks      int       `json:"tasks"`
	Trials     int       `json:"trials"`
	PassRate   float64   `json:"passRate"`
	StdErr     *float64  `json:"stdErr,omitempty"`
	WilsonLow  float64   `json:"wilsonLow"`
	WilsonHigh float64   `json:"wilsonHigh"`
	PassHatK   []float64 `json:"passHatK,omitempty"`
	PassAtK    []float64 `json:"passAtK,omitempty"`
}

// PairedSummary holds the paired-difference statistics over tasks present
// in both results. It is omitted when fewer than two tasks pair. MDE is
// nil when the standard error is zero.
type PairedSummary struct {
	Tasks       int      `json:"tasks"`
	MeanDelta   float64  `json:"meanDelta"`
	StdErr      float64  `json:"stdErr"`
	DF          int      `json:"df"`
	CILow       float64  `json:"ciLow"`
	CIHigh      float64  `json:"ciHigh"`
	UpperBound  float64  `json:"upperBound"`
	PValue      float64  `json:"pValue"`
	PValueExact bool     `json:"pValueExact"`
	MDE         *float64 `json:"mde,omitempty"`
}

// NoiseFloor estimates how often an unchanged agent would trip a flip
// gate, assuming every paired task passes each trial independently with
// the pooled per-trial pass rate observed across both results. It is
// reported only when every paired task passed all of its baseline trials.
// The false-alarm rates are nil when every pooled trial passed, as the
// pooled rate then carries no variation to estimate from.
type NoiseFloor struct {
	PerTrialPassRate    float64  `json:"perTrialPassRate"`
	PooledTrials        int      `json:"pooledTrials"`
	SingleRunFalseAlarm *float64 `json:"singleRunFalseAlarm,omitempty"`
	FlipRuleFalseAlarm  *float64 `json:"flipRuleFalseAlarm,omitempty"`
}
