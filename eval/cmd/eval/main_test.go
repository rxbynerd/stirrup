package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

func TestLoadSuite_Valid(t *testing.T) {
	dir := t.TempDir()
	suite := types.EvalSuite{
		ID:          "test-suite",
		Description: "a test suite",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "hello", Mode: "execution"},
		},
	}
	path := filepath.Join(dir, "suite.json")
	writeJSONFile(t, path, suite)

	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "test-suite" {
		t.Errorf("ID = %q, want %q", got.ID, "test-suite")
	}
	if len(got.Tasks) != 1 {
		t.Errorf("got %d tasks, want 1", len(got.Tasks))
	}
}

func TestLoadSuite_Missing(t *testing.T) {
	_, err := loadSuite("/nonexistent/suite.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadSuite_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json"), 0o644)

	_, err := loadSuite(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadResult_Valid(t *testing.T) {
	dir := t.TempDir()
	result := eval.SuiteResult{
		SuiteID:  "s1",
		RunID:    "r1",
		PassRate: 0.5,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass"},
			{TaskID: "t2", Outcome: "fail"},
		},
	}
	path := filepath.Join(dir, "result.json")
	writeJSONFile(t, path, result)

	got, err := loadResult(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RunID != "r1" {
		t.Errorf("RunID = %q, want %q", got.RunID, "r1")
	}
	if len(got.Tasks) != 2 {
		t.Errorf("got %d tasks, want 2", len(got.Tasks))
	}
}

func TestLoadResult_Missing(t *testing.T) {
	_, err := loadResult("/nonexistent/result.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	original := eval.SuiteResult{
		SuiteID:  "s1",
		RunID:    "r1",
		PassRate: 0.75,
	}

	if err := writeJSON(path, original); err != nil {
		t.Fatalf("writeJSON error: %v", err)
	}

	got, err := loadResult(path)
	if err != nil {
		t.Fatalf("loadResult error: %v", err)
	}

	if got.SuiteID != original.SuiteID || got.RunID != original.RunID {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- parseDate tests ---

func TestParseDate_RFC3339(t *testing.T) {
	got, err := parseDate("2025-03-15T10:30:00Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2025, 3, 15, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseDate(RFC3339) = %v, want %v", got, want)
	}
}

func TestParseDate_DateOnly(t *testing.T) {
	got, err := parseDate("2025-03-15")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseDate(date-only) = %v, want %v", got, want)
	}
}

func TestParseDate_Invalid(t *testing.T) {
	_, err := parseDate("not-a-date")
	if err == nil {
		t.Fatal("expected error for invalid date")
	}
}

func TestParseDate_EmptyString(t *testing.T) {
	_, err := parseDate("")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

// --- parseDuration tests ---

func TestParseDuration_GoFormat(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"30m", 30 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
	}
	for _, tc := range cases {
		got, err := parseDuration(tc.input)
		if err != nil {
			t.Errorf("parseDuration(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestParseDuration_Days(t *testing.T) {
	got, err := parseDuration("7d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 7 * 24 * time.Hour
	if got != want {
		t.Errorf("parseDuration(7d) = %v, want %v", got, want)
	}
}

func TestParseDuration_FractionalDays(t *testing.T) {
	got, err := parseDuration("0.5d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 12 * time.Hour
	if got != want {
		t.Errorf("parseDuration(0.5d) = %v, want %v", got, want)
	}
}

func TestParseDuration_Invalid(t *testing.T) {
	_, err := parseDuration("notaduration")
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestParseDuration_BadDays(t *testing.T) {
	_, err := parseDuration("abcd")
	if err == nil {
		t.Fatal("expected error for non-numeric days suffix")
	}
}

// --- mineFailureTasks tests ---

func TestMineFailureTasks_FiltersNonSuccess(t *testing.T) {
	recordings := []types.RunRecording{
		{
			RunID:  "r1",
			Config: types.RunConfig{Prompt: "fix bug A", Mode: "execution"},
			FinalOutcome: types.RunTrace{
				ID:      "r1",
				Outcome: "success",
			},
		},
		{
			RunID:  "r2",
			Config: types.RunConfig{Prompt: "fix bug B", Mode: "execution"},
			FinalOutcome: types.RunTrace{
				ID:      "r2",
				Outcome: "error",
			},
		},
		{
			RunID:  "r3",
			Config: types.RunConfig{Prompt: "fix bug C", Mode: "planning"},
			FinalOutcome: types.RunTrace{
				ID:      "r3",
				Outcome: "max_turns",
			},
		},
		{
			RunID:  "r4",
			Config: types.RunConfig{Prompt: "fix bug D", Mode: "execution"},
			FinalOutcome: types.RunTrace{
				ID:      "r4",
				Outcome: "success",
			},
		},
	}

	tasks := mineFailureTasks(recordings, 0)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}

	if tasks[0].Prompt != "fix bug B" {
		t.Errorf("task[0].Prompt = %q, want %q", tasks[0].Prompt, "fix bug B")
	}
	if tasks[0].Mode != "execution" {
		t.Errorf("task[0].Mode = %q, want %q", tasks[0].Mode, "execution")
	}
	if tasks[0].Judge.Type != "test-command" {
		t.Errorf("task[0].Judge.Type = %q, want %q", tasks[0].Judge.Type, "test-command")
	}
	if tasks[0].Judge.Command != "go test ./..." {
		t.Errorf("task[0].Judge.Command = %q, want %q", tasks[0].Judge.Command, "go test ./...")
	}

	if tasks[1].Prompt != "fix bug C" {
		t.Errorf("task[1].Prompt = %q, want %q", tasks[1].Prompt, "fix bug C")
	}
	if tasks[1].Mode != "planning" {
		t.Errorf("task[1].Mode = %q, want %q", tasks[1].Mode, "planning")
	}
}

func TestMineFailureTasks_RespectsLimit(t *testing.T) {
	recordings := []types.RunRecording{
		{RunID: "r1", Config: types.RunConfig{Prompt: "a"}, FinalOutcome: types.RunTrace{Outcome: "error"}},
		{RunID: "r2", Config: types.RunConfig{Prompt: "b"}, FinalOutcome: types.RunTrace{Outcome: "max_turns"}},
		{RunID: "r3", Config: types.RunConfig{Prompt: "c"}, FinalOutcome: types.RunTrace{Outcome: "error"}},
	}

	tasks := mineFailureTasks(recordings, 2)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
}

func TestMineFailureTasks_NoFailures(t *testing.T) {
	recordings := []types.RunRecording{
		{RunID: "r1", Config: types.RunConfig{Prompt: "a"}, FinalOutcome: types.RunTrace{Outcome: "success"}},
	}

	tasks := mineFailureTasks(recordings, 0)
	if len(tasks) != 0 {
		t.Fatalf("got %d tasks, want 0", len(tasks))
	}
}

// --- buildLabVsProductionReport tests ---

func TestBuildLabVsProductionReport_Basic(t *testing.T) {
	prodMetrics := types.TraceMetrics{
		Count:    100,
		PassRate: 0.85,
		MeanCost: 0.45,
		MeanTurns: 4.2,
	}

	result := eval.SuiteResult{
		SuiteID:  "test-suite",
		RunID:    "run-1",
		PassRate: 0.90,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", Trace: &types.RunTrace{Cost: 0.30, Turns: 3}},
			{TaskID: "t2", Outcome: "pass", Trace: &types.RunTrace{Cost: 0.50, Turns: 4}},
			{TaskID: "t3", Outcome: "fail", Trace: &types.RunTrace{Cost: 0.40, Turns: 5}},
		},
	}

	report := buildLabVsProductionReport("exp-1", prodMetrics, result)

	if report.ExperimentID != "exp-1" {
		t.Errorf("ExperimentID = %q, want %q", report.ExperimentID, "exp-1")
	}

	// Production baseline
	if report.Production.SampleSize != 100 {
		t.Errorf("Production.SampleSize = %d, want 100", report.Production.SampleSize)
	}
	if math.Abs(report.Production.PassRate-0.85) > 0.001 {
		t.Errorf("Production.PassRate = %f, want 0.85", report.Production.PassRate)
	}
	if math.Abs(report.Production.MeanCost-0.45) > 0.001 {
		t.Errorf("Production.MeanCost = %f, want 0.45", report.Production.MeanCost)
	}
	if math.Abs(report.Production.MeanTurns-4.2) > 0.001 {
		t.Errorf("Production.MeanTurns = %f, want 4.2", report.Production.MeanTurns)
	}

	// Variant
	if len(report.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(report.Variants))
	}
	v := report.Variants[0]
	if v.Name != "test-suite" {
		t.Errorf("Variant.Name = %q, want %q", v.Name, "test-suite")
	}
	if math.Abs(v.Results.PassRate-0.90) > 0.001 {
		t.Errorf("Variant.PassRate = %f, want 0.90", v.Results.PassRate)
	}
	// Mean cost = (0.30 + 0.50 + 0.40) / 3 = 0.40
	if math.Abs(v.Results.MeanCost-0.40) > 0.001 {
		t.Errorf("Variant.MeanCost = %f, want 0.40", v.Results.MeanCost)
	}
	// Mean turns = (3 + 4 + 5) / 3 = 4.0 => MedianTurns = 4
	if v.Results.MedianTurns != 4 {
		t.Errorf("Variant.MedianTurns = %d, want 4", v.Results.MedianTurns)
	}
}

func TestBuildLabVsProductionReport_NoTraces(t *testing.T) {
	prodMetrics := types.TraceMetrics{
		Count:    50,
		PassRate: 0.70,
		MeanCost: 0.30,
	}

	result := eval.SuiteResult{
		SuiteID:  "no-traces",
		PassRate: 0.50,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass"},
			{TaskID: "t2", Outcome: "fail"},
		},
	}

	report := buildLabVsProductionReport("exp-2", prodMetrics, result)

	if len(report.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(report.Variants))
	}
	v := report.Variants[0]
	// With no traces, mean cost and turns should be zero.
	if v.Results.MeanCost != 0 {
		t.Errorf("Variant.MeanCost = %f, want 0", v.Results.MeanCost)
	}
	if v.Results.MedianTurns != 0 {
		t.Errorf("Variant.MedianTurns = %d, want 0", v.Results.MedianTurns)
	}
}

func TestBuildLabVsProductionReport_MixedTraces(t *testing.T) {
	prodMetrics := types.TraceMetrics{Count: 10, PassRate: 0.80}

	result := eval.SuiteResult{
		SuiteID:  "mixed",
		PassRate: 0.75,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", Trace: &types.RunTrace{Cost: 0.20, Turns: 2}},
			{TaskID: "t2", Outcome: "error"}, // no trace
			{TaskID: "t3", Outcome: "pass", Trace: &types.RunTrace{Cost: 0.40, Turns: 6}},
		},
	}

	report := buildLabVsProductionReport("exp-3", prodMetrics, result)
	v := report.Variants[0]

	// Only traced tasks count: mean cost = (0.20 + 0.40) / 2 = 0.30
	if math.Abs(v.Results.MeanCost-0.30) > 0.001 {
		t.Errorf("Variant.MeanCost = %f, want 0.30", v.Results.MeanCost)
	}
	// Mean turns = (2 + 6) / 2 = 4
	if v.Results.MedianTurns != 4 {
		t.Errorf("Variant.MedianTurns = %d, want 4", v.Results.MedianTurns)
	}
}

func TestPrintComparisonSummary_DoesNotPanic(t *testing.T) {
	report := types.LabVsProductionReport{
		ExperimentID: "smoke-test",
		Production: types.BaselineMetrics{
			PassRate:   0.85,
			MeanCost:   0.45,
			MeanTurns:  4.2,
			SampleSize: 100,
		},
		Variants: []types.VariantReport{
			{
				Name: "v1",
				Results: types.VariantResults{
					PassRate:    0.90,
					MeanCost:    0.38,
					MedianTurns: 3,
				},
			},
		},
	}

	// Redirect stderr to discard so test output stays clean.
	origStderr := os.Stderr
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = devNull
	defer func() {
		os.Stderr = origStderr
		devNull.Close()
	}()

	// Should not panic.
	printComparisonSummary(report)
}

func TestPrintComparisonSummary_EmptyVariants(t *testing.T) {
	report := types.LabVsProductionReport{
		ExperimentID: "empty",
		Production: types.BaselineMetrics{
			PassRate:   0.50,
			SampleSize: 10,
		},
	}

	origStderr := os.Stderr
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = devNull
	defer func() {
		os.Stderr = origStderr
		devNull.Close()
	}()

	// Should not panic with zero variants.
	printComparisonSummary(report)
}

// --- buildDriftReport tests ---

func TestBuildDriftReport_ComputesDeltas(t *testing.T) {
	current := types.TraceMetrics{
		Count:       10,
		PassRate:    0.80,
		MeanCost:    0.50,
		MeanTurns:   5.0,
		MeanTokens:  1000,
		P50Duration: 200,
		P95Duration: 500,
	}
	baseline := types.TraceMetrics{
		Count:       10,
		PassRate:    0.90,
		MeanCost:    0.40,
		MeanTurns:   4.0,
		MeanTokens:  900,
		P50Duration: 180,
		P95Duration: 450,
	}

	report := buildDriftReport(current, baseline)

	if math.Abs(report.Deltas.PassRateDelta-(-0.10)) > 0.001 {
		t.Errorf("PassRateDelta = %f, want -0.10", report.Deltas.PassRateDelta)
	}
	if math.Abs(report.Deltas.MeanCostDelta-0.10) > 0.001 {
		t.Errorf("MeanCostDelta = %f, want 0.10", report.Deltas.MeanCostDelta)
	}
	if math.Abs(report.Deltas.MeanTurnsDelta-1.0) > 0.001 {
		t.Errorf("MeanTurnsDelta = %f, want 1.0", report.Deltas.MeanTurnsDelta)
	}
}
