package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// --- run() dispatch tests ---

// TestRun_Version exercises the --version short-circuit through run() so we
// don't need to shell out to a built binary or fight global state. Each
// accepted spelling (--version / -v / version) must exit 0 and print to
// stdout.
func TestRun_Version(t *testing.T) {
	for _, arg := range []string{"--version", "-v", "version"} {
		t.Run(arg, func(t *testing.T) {
			var stdout bytes.Buffer
			code := run([]string{arg}, &stdout)
			if code != 0 {
				t.Fatalf("run(%q) exit code = %d, want 0", arg, code)
			}
			out := stdout.String()
			if !strings.HasPrefix(out, "stirrup-eval version ") {
				t.Fatalf("stdout = %q, want prefix %q", out, "stirrup-eval version ")
			}
			// Default link-time version when no -ldflags injected.
			if !strings.Contains(out, "dev") {
				t.Fatalf("stdout = %q, want it to contain default version %q", out, "dev")
			}
		})
	}
}

// TestRun_NoArgs documents the empty-args contract: usage goes to stderr,
// stdout stays untouched, exit code is 1.
func TestRun_NoArgs(t *testing.T) {
	var stdout bytes.Buffer
	code := run(nil, &stdout)
	if code != 1 {
		t.Fatalf("run(nil) exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout should be empty for usage error, got %q", stdout.String())
	}
}

// TestLoadSuite_Missing exercises the underlying os error surface
// when the suite path does not exist.
func TestLoadSuite_Missing(t *testing.T) {
	_, err := loadSuite("/nonexistent/suite.hcl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestLoadSuite_InvalidHCL pins error propagation: if loadSuite ever
// stopped returning the diagnostic from spec.LoadSuiteHCL, this test
// would catch it.
func TestLoadSuite_InvalidHCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.hcl")
	if err := os.WriteFile(path, []byte("suite \"x\" { {{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadSuite(path)
	if err == nil {
		t.Fatal("expected error for invalid HCL")
	}
}

// TestLoadSuite_HCL exercises the happy path: the loader must accept
// a .hcl file and produce the canonical EvalSuite shape.
func TestLoadSuite_HCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.hcl")
	src := `
suite "hcl-suite" {
  description = "an HCL suite"

  task "t1" {
    description = "first task"
    mode        = "execution"
    prompt      = "hello"

    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "hcl-suite" {
		t.Errorf("ID = %q, want %q", got.ID, "hcl-suite")
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got.Tasks))
	}
	if got.Tasks[0].ID != "t1" {
		t.Errorf("Tasks[0].ID = %q, want %q", got.Tasks[0].ID, "t1")
	}
	if got.Tasks[0].Judge.Type != "test-command" {
		t.Errorf("Tasks[0].Judge.Type = %q, want %q", got.Tasks[0].Judge.Type, "test-command")
	}
}

// TestLoadSuite_UnsupportedExtension documents the dispatcher contract
// for unknown file extensions: only .hcl is accepted, and the error
// message must say so. Notably .json is rejected the same way as
// .yaml — the legacy JSON loader is gone.
func TestLoadSuite_UnsupportedExtension(t *testing.T) {
	cases := []struct {
		name string
		ext  string
	}{
		{name: "yaml", ext: ".yaml"},
		{name: "json (legacy format removed)", ext: ".json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "suite"+tc.ext)
			if err := os.WriteFile(path, []byte("anything"), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := loadSuite(path)
			if err == nil {
				t.Fatal("expected error for unsupported extension")
			}
			if !strings.Contains(err.Error(), ".hcl") {
				t.Fatalf("error = %q, want it to mention .hcl", err.Error())
			}
		})
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

// --- writeSuiteHCL tests ---

// TestWriteSuiteHCL_RoundTrip ensures the HCL emitted by mine-failures
// is parseable by the canonical loader. Without this, mined suites
// would silently regress to a non-loadable format the moment the
// emitter and loader drift.
func TestWriteSuiteHCL_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mined.hcl")

	original := types.EvalSuite{
		ID:          "mined-suite",
		Description: "starter from mine-failures",
		Tasks: []types.EvalTask{
			{
				ID:          "single",
				Description: "single judge task",
				Repo:        "https://example.invalid/repo",
				Ref:         "main",
				Mode:        "execution",
				Prompt:      "fix the bug",
				Judge: types.EvalJudge{
					Type:    "test-command",
					Command: "go test ./...",
				},
			},
			{
				ID:     "composite",
				Mode:   "execution",
				Prompt: "produce brief.md",
				Judge: types.EvalJudge{
					Type:    "composite",
					Require: "all",
					Judges: []types.EvalJudge{
						{Type: "file-exists", Paths: []string{"brief.md"}},
						{Type: "file-contains", Path: "brief.md", Pattern: "(?i)token"},
					},
				},
			},
		},
	}

	if err := writeSuiteHCL(path, original); err != nil {
		t.Fatalf("writeSuiteHCL: %v", err)
	}

	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("loadSuite after writeSuiteHCL: %v", err)
	}

	if got.ID != original.ID || got.Description != original.Description {
		t.Errorf("suite metadata mismatch: got %+v want %+v", got, original)
	}
	if len(got.Tasks) != len(original.Tasks) {
		t.Fatalf("got %d tasks, want %d", len(got.Tasks), len(original.Tasks))
	}
	for i, want := range original.Tasks {
		if got.Tasks[i].ID != want.ID ||
			got.Tasks[i].Mode != want.Mode ||
			got.Tasks[i].Prompt != want.Prompt {
			t.Errorf("task[%d] mismatch: got %+v want %+v", i, got.Tasks[i], want)
		}
		if got.Tasks[i].Judge.Type != want.Judge.Type {
			t.Errorf("task[%d].Judge.Type = %q, want %q", i, got.Tasks[i].Judge.Type, want.Judge.Type)
		}
	}
	if got.Tasks[1].Judge.Require != "all" {
		t.Errorf("composite Require = %q, want %q", got.Tasks[1].Judge.Require, "all")
	}
	if len(got.Tasks[1].Judge.Judges) != 2 {
		t.Errorf("composite has %d sub-judges, want 2", len(got.Tasks[1].Judge.Judges))
	}
}

// TestWriteSuiteHCL_EscapesInterpolation ensures hclwrite is escaping
// HCL-significant sequences (in particular `${...}` interpolation
// markers) so that user prompts mined out of production traces are
// preserved verbatim through the round trip rather than re-interpreted
// by the loader.
func TestWriteSuiteHCL_EscapesInterpolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mined.hcl")

	dangerous := `prompt with ${var} interpolation and "quotes" plus a backslash \ end`
	original := types.EvalSuite{
		ID: "mined",
		Tasks: []types.EvalTask{
			{
				ID:     "tricky",
				Prompt: dangerous,
				Judge: types.EvalJudge{
					Type:    "test-command",
					Command: `go test ${PKG}`,
				},
			},
		},
	}

	if err := writeSuiteHCL(path, original); err != nil {
		t.Fatalf("writeSuiteHCL: %v", err)
	}
	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("loadSuite: %v", err)
	}
	if got.Tasks[0].Prompt != dangerous {
		t.Errorf("Prompt = %q, want %q", got.Tasks[0].Prompt, dangerous)
	}
	if got.Tasks[0].Judge.Command != `go test ${PKG}` {
		t.Errorf("Command = %q, want %q", got.Tasks[0].Judge.Command, `go test ${PKG}`)
	}
}

// --- buildLabVsProductionReport tests ---

func TestBuildLabVsProductionReport_Basic(t *testing.T) {
	prodMetrics := types.TraceMetrics{
		Count:     100,
		PassRate:  0.85,
		MeanTurns: 4.2,
	}

	result := eval.SuiteResult{
		SuiteID:  "test-suite",
		RunID:    "run-1",
		PassRate: 0.90,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", Trace: &types.RunTrace{Turns: 3}},
			{TaskID: "t2", Outcome: "pass", Trace: &types.RunTrace{Turns: 4}},
			{TaskID: "t3", Outcome: "fail", Trace: &types.RunTrace{Turns: 5}},
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
	// Mean turns = (3 + 4 + 5) / 3 = 4.0 => MedianTurns = 4
	if v.Results.MedianTurns != 4 {
		t.Errorf("Variant.MedianTurns = %d, want 4", v.Results.MedianTurns)
	}
}

func TestBuildLabVsProductionReport_NoTraces(t *testing.T) {
	prodMetrics := types.TraceMetrics{
		Count:    50,
		PassRate: 0.70,
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
	// With no traces, turns should be zero.
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
			{TaskID: "t1", Outcome: "pass", Trace: &types.RunTrace{Turns: 2}},
			{TaskID: "t2", Outcome: "error"}, // no trace
			{TaskID: "t3", Outcome: "pass", Trace: &types.RunTrace{Turns: 6}},
		},
	}

	report := buildLabVsProductionReport("exp-3", prodMetrics, result)
	v := report.Variants[0]

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
			MeanTurns:  4.2,
			SampleSize: 100,
		},
		Variants: []types.VariantReport{
			{
				Name: "v1",
				Results: types.VariantResults{
					PassRate:    0.90,
					MedianTurns: 3,
				},
			},
		},
	}

	// Should not panic. Using io.Discard avoids the global os.Stderr
	// mutation pattern, which is unsafe under -race / -parallel.
	printComparisonSummary(io.Discard, report)
}

func TestPrintComparisonSummary_EmptyVariants(t *testing.T) {
	report := types.LabVsProductionReport{
		ExperimentID: "empty",
		Production: types.BaselineMetrics{
			PassRate:   0.50,
			SampleSize: 10,
		},
	}

	// Should not panic with zero variants.
	printComparisonSummary(io.Discard, report)
}

// --- buildDriftReport tests ---

func TestBuildDriftReport_ComputesDeltas(t *testing.T) {
	current := types.TraceMetrics{
		Count:       10,
		PassRate:    0.80,
		MeanTurns:   5.0,
		MeanTokens:  1000,
		P50Duration: 200,
		P95Duration: 500,
	}
	baseline := types.TraceMetrics{
		Count:       10,
		PassRate:    0.90,
		MeanTurns:   4.0,
		MeanTokens:  900,
		P50Duration: 180,
		P95Duration: 450,
	}

	report := buildDriftReport(current, baseline)

	if math.Abs(report.Deltas.PassRateDelta-(-0.10)) > 0.001 {
		t.Errorf("PassRateDelta = %f, want -0.10", report.Deltas.PassRateDelta)
	}
	if math.Abs(report.Deltas.MeanTurnsDelta-1.0) > 0.001 {
		t.Errorf("MeanTurnsDelta = %f, want 1.0", report.Deltas.MeanTurnsDelta)
	}
}
