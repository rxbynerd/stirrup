package types

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
	MeanCost                float64      `json:"meanCost"`
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
	MeanCost   float64    `json:"meanCost"`
	MeanTurns  float64    `json:"meanTurns"`
	MeanTokens TokenUsage `json:"meanTokens"`
	SampleSize int        `json:"sampleSize"`
}

// DriftReport describes performance changes between two time windows.
type DriftReport struct {
	Current    BaselineMetrics `json:"current"`
	Previous   BaselineMetrics `json:"previous"`
	PassDelta  float64         `json:"passDelta"`
	CostDelta  float64         `json:"costDelta"`
	TurnsDelta float64         `json:"turnsDelta"`
}

// DateRange defines a time window for trace queries.
type DateRange struct {
	Start string `json:"start"` // ISO 8601
	End   string `json:"end"`   // ISO 8601
}

// TraceFilter defines criteria for querying traces.
type TraceFilter struct {
	Mode     string `json:"mode,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// LabVsProductionReport compares experiment results to production metrics.
type LabVsProductionReport struct {
	ExperimentID string          `json:"experimentId"`
	Production   BaselineMetrics `json:"production"`
	Variants     []VariantReport `json:"variants"`
}
