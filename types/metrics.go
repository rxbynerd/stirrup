package types

import "time"

// ExperimentReport holds the aggregated results of an experiment.
type ExperimentReport struct {
	ExperimentID string          `json:"experimentId"`
	Suite        string          `json:"suite"`
	Variants     []VariantReport `json:"variants"`
}

// VariantReport holds the results for a single experiment variant.
type VariantReport struct {
	Name    string             `json:"name"`
	Config  RunConfigOverrides `json:"config"`
	Results VariantResults     `json:"results"`
}

// VariantResults holds the aggregated metrics for a variant.
type VariantResults struct {
	PassRate                float64      `json:"passRate"`
	MedianTurns             int          `json:"medianTurns"`
	MeanTokens              TokenUsage   `json:"meanTokens"`
	MeanToolCalls           float64      `json:"meanToolCalls"`
	ToolFailureRate         float64      `json:"toolFailureRate"`
	MeanWallClockMs         int64        `json:"meanWallClockMs"`
	MeanDiffLines           float64      `json:"meanDiffLines"`
	MeanVerificationRetries float64      `json:"meanVerificationRetries"`
	Consistency             float64      `json:"consistency"`
	PerTask                 []TaskResult `json:"perTask"`
}

// TaskResult holds the result of a single task within a variant.
type TaskResult struct {
	TaskID  string     `json:"taskId"`
	Runs    []RunTrace `json:"runs"`
	Passed  int        `json:"passed"`
	Total   int        `json:"total"`
}

// ProductionTrace extends RunTrace with production-specific metadata.
type ProductionTrace struct {
	RunTrace
	HarnessVersion string `json:"harnessVersion"`
	TaskSource     string `json:"taskSource"`
	TargetRepo     string `json:"targetRepo"`
	UserID         string `json:"userId,omitempty"`
}

// BaselineMetrics holds aggregate production metrics for comparison.
type BaselineMetrics struct {
	PassRate    float64    `json:"passRate"`
	MeanTurns  float64    `json:"meanTurns"`
	MeanTokens TokenUsage `json:"meanTokens"`
	SampleSize int        `json:"sampleSize"`
}

// DriftReport describes performance changes between two time windows.
type DriftReport struct {
	Current  TraceMetrics `json:"current"`
	Baseline TraceMetrics `json:"baseline"`
	Deltas   DriftDeltas  `json:"deltas"`
}

// DriftDeltas holds the absolute differences between current and baseline metrics.
type DriftDeltas struct {
	PassRateDelta    float64 `json:"passRateDelta"`
	MeanTurnsDelta   float64 `json:"meanTurnsDelta"`
	MeanTokensDelta  float64 `json:"meanTokensDelta"`
	P50DurationDelta float64 `json:"p50DurationDelta"`
	P95DurationDelta float64 `json:"p95DurationDelta"`
}

// TraceMetrics holds aggregate metrics computed over a set of traces.
type TraceMetrics struct {
	Count       int     `json:"count"`
	PassRate    float64 `json:"passRate"`
	MeanTurns   float64 `json:"meanTurns"`
	MeanTokens  float64 `json:"meanTokens"`
	P50Duration float64 `json:"p50DurationMs"`
	P95Duration float64 `json:"p95DurationMs"`
}

// DateRange defines a time window for trace queries.
type DateRange struct {
	Start string `json:"start"` // ISO 8601
	End   string `json:"end"`   // ISO 8601
}

// TraceFilter defines criteria for querying traces from a lakehouse.
type TraceFilter struct {
	// After limits to traces started after this time.
	After *time.Time `json:"after,omitempty"`

	// Before limits to traces started before this time.
	Before *time.Time `json:"before,omitempty"`

	// Outcome filters by trace outcome (e.g. "success", "error", "max_turns").
	Outcome string `json:"outcome,omitempty"`

	// Mode filters by run mode (e.g. "execution", "planning").
	Mode string `json:"mode,omitempty"`

	// Provider filters by provider name.
	Provider string `json:"provider,omitempty"`

	// Model filters by model name.
	Model string `json:"model,omitempty"`

	// Limit caps the number of results. Zero means no limit.
	Limit int `json:"limit,omitempty"`
}

// LabVsProductionReport compares experiment results to production metrics.
type LabVsProductionReport struct {
	ExperimentID string          `json:"experimentId"`
	Production   BaselineMetrics `json:"production"`
	Variants     []VariantReport `json:"variants"`
}
