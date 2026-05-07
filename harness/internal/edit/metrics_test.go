package edit

import (
	"context"
	"encoding/json"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security/codescanner"
	"github.com/rxbynerd/stirrup/types"
)

// TestMultiStrategy_RecordsAttemptsAndDuration verifies that:
//
//   - A single applicable candidate that succeeds records exactly one
//     attempt with success=true and fell_back_from="".
//   - The duration histogram records at least one observation tagged
//     with the strategy that ran.
func TestMultiStrategy_RecordsAttemptsAndDuration(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "test.txt", "hello world")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	strat := NewMultiStrategy(defaultFuzzyThreshold)
	strat.Metrics = m

	input := json.RawMessage(`{"path":"test.txt","old_string":"world","new_string":"universe"}`)
	result, err := strat.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	dps := collectInt64DataPoints(t, rm, "stirrup.edit.attempts")
	if len(dps) != 1 {
		t.Fatalf("expected 1 attempt data point, got %d", len(dps))
	}
	if dps[0].attrs["strategy"] != "search-replace" {
		t.Errorf("strategy = %q, want %q", dps[0].attrs["strategy"], "search-replace")
	}
	if dps[0].attrs["fell_back_from"] != "" {
		t.Errorf("fell_back_from = %q, want empty (primary attempt)", dps[0].attrs["fell_back_from"])
	}
	if dps[0].attrs["success"] != "true" {
		t.Errorf("success = %q, want true", dps[0].attrs["success"])
	}

	if !histogramRecorded(t, rm, "stirrup.edit.duration_ms") {
		t.Error("stirrup.edit.duration_ms recorded no observations")
	}
}

// TestMultiStrategy_RecordsFallback verifies that when the primary
// candidate fails (Applied=false) and a secondary applicable candidate
// is supplied, two attempts are recorded with the second one's
// fell_back_from naming the first.
//
// We force a fallback by providing both `old_string` (search-replace,
// which fails because the file does not contain "missing") and
// `content` (whole-file, which succeeds).
func TestMultiStrategy_RecordsFallback(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "test.txt", "original content")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	strat := NewMultiStrategy(defaultFuzzyThreshold)
	strat.Metrics = m

	input := json.RawMessage(`{
		"path": "test.txt",
		"old_string": "missing",
		"new_string": "replaced",
		"content": "totally new content"
	}`)
	result, err := strat.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true via fallback; error: %s", result.Error)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	dps := collectInt64DataPoints(t, rm, "stirrup.edit.attempts")
	if len(dps) != 2 {
		t.Fatalf("expected 2 attempt data points (primary + fallback), got %d", len(dps))
	}

	var primary, fallback int64DataPoint
	for _, dp := range dps {
		if dp.attrs["fell_back_from"] == "" {
			primary = dp
		} else {
			fallback = dp
		}
	}
	if primary.attrs["strategy"] != "search-replace" {
		t.Errorf("primary strategy = %q, want search-replace", primary.attrs["strategy"])
	}
	if primary.attrs["success"] != "false" {
		t.Errorf("primary success = %q, want false", primary.attrs["success"])
	}
	if fallback.attrs["strategy"] != "whole-file" {
		t.Errorf("fallback strategy = %q, want whole-file", fallback.attrs["strategy"])
	}
	if fallback.attrs["fell_back_from"] != "search-replace" {
		t.Errorf("fell_back_from = %q, want search-replace", fallback.attrs["fell_back_from"])
	}
	if fallback.attrs["success"] != "true" {
		t.Errorf("fallback success = %q, want true", fallback.attrs["success"])
	}
}

// TestScannedStrategy_RecordsScansAndFindings verifies stirrup
// .codescanner.scans (one per Apply that runs the scanner) and
// stirrup.codescanner.findings (one per finding, with severity and
// blocked attributes). A planted secret in fresh content triggers a
// block finding.
func TestScannedStrategy_RecordsScansAndFindings(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "config.js", "const x = 1;\n")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	scanner := codescanner.NewPatternScanner()
	wrapped := NewScannedStrategy(NewWholeFileStrategy(), scanner, &types.CodeScannerConfig{Type: "patterns"}, nil)
	scanned, ok := wrapped.(*ScannedStrategy)
	if !ok {
		t.Fatal("expected ScannedStrategy from NewScannedStrategy")
	}
	scanned.Metrics = m

	// A GitHub PAT pattern is a known block finding.
	input := json.RawMessage(`{
		"path": "config.js",
		"content": "const token = \"ghp_abcdefghijklmnopqrstuvwxyz0123456789\";\n"
	}`)
	result, err := scanned.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Errorf("expected Applied=false on block finding")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	scans := collectInt64DataPoints(t, rm, "stirrup.codescanner.scans")
	if len(scans) != 1 {
		t.Fatalf("expected 1 scan data point, got %d", len(scans))
	}
	if scans[0].attrs["scanner"] != "patterns" {
		t.Errorf("scanner = %q, want patterns", scans[0].attrs["scanner"])
	}
	if scans[0].value != 1 {
		t.Errorf("scan count = %d, want 1", scans[0].value)
	}

	findings := collectInt64DataPoints(t, rm, "stirrup.codescanner.findings")
	if len(findings) == 0 {
		t.Fatal("expected at least one finding data point")
	}
	// We expect at least one block finding, attributed to the patterns
	// scanner with blocked=true and severity=block.
	var sawBlock bool
	for _, dp := range findings {
		if dp.attrs["scanner"] != "patterns" {
			t.Errorf("finding scanner = %q, want patterns", dp.attrs["scanner"])
		}
		if dp.attrs["severity"] == codescanner.SeverityBlock && dp.attrs["blocked"] == "true" {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Errorf("expected a block-severity, blocked=true finding; got: %+v", findings)
	}
}

// --- helpers ---

type int64DataPoint struct {
	value int64
	attrs map[string]string
}

func collectInt64DataPoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []int64DataPoint {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is not a Sum[int64]", name)
			}
			out := make([]int64DataPoint, 0, len(sum.DataPoints))
			for _, dp := range sum.DataPoints {
				attrs := make(map[string]string)
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.Emit()
				}
				out = append(out, int64DataPoint{value: dp.Value, attrs: attrs})
			}
			return out
		}
	}
	return nil
}

func histogramRecorded(t *testing.T, rm metricdata.ResourceMetrics, name string) bool {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			h, ok := mt.Data.(metricdata.Histogram[float64])
			if !ok {
				return false
			}
			for _, dp := range h.DataPoints {
				if dp.Count > 0 {
					return true
				}
			}
		}
	}
	return false
}
