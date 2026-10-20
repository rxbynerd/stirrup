package edit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "test.txt", "hello world")

	reader, m := newMetricsFixture(t)

	strat := NewMultiStrategy(defaultFuzzyThreshold)
	strat.Metrics = m

	input := json.RawMessage(`{"path":"test.txt","operation":"replace","old_string":"world","new_string":"universe"}`)
	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	rm := collectMetrics(t, reader)

	dps := collectInt64DataPoints(t, rm, "stirrup.edit.attempts")
	must.Len(dps, 1, "stirrup.edit.attempts data points")
	check.Equal("search-replace", dps[0].attrs["strategy"], "strategy")
	check.Empty(dps[0].attrs["fell_back_from"], "fell_back_from on the primary attempt")
	check.Equal("true", dps[0].attrs["success"], "success")

	check.True(histogramRecorded(t, rm, "stirrup.edit.duration"), "stirrup.edit.duration observations")
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
	check := assert.New(t)
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "test.txt", "original content")

	reader, m := newMetricsFixture(t)

	strat := NewMultiStrategy(defaultFuzzyThreshold)
	strat.Metrics = m

	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "replace",
		"old_string": "missing",
		"new_string": "replaced",
		"content": "totally new content"
	}`)
	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true via fallback; error: %s", result.Error)

	rm := collectMetrics(t, reader)

	dps := collectInt64DataPoints(t, rm, "stirrup.edit.attempts")
	must.Len(dps, 2, "expected primary and fallback attempt data points")

	var primary, fallback int64DataPoint
	for _, dp := range dps {
		if dp.attrs["fell_back_from"] == "" {
			primary = dp
		} else {
			fallback = dp
		}
	}
	check.Equal("search-replace", primary.attrs["strategy"], "primary strategy")
	check.Equal("false", primary.attrs["success"], "primary success")
	check.Equal("whole-file", fallback.attrs["strategy"], "fallback strategy")
	check.Equal("search-replace", fallback.attrs["fell_back_from"], "fallback fell_back_from")
	check.Equal("true", fallback.attrs["success"], "fallback success")
}

// TestScannedStrategy_RecordsScansAndFindings verifies
// stirrup.codescanner.scans (one per Apply that runs the scanner) and
// stirrup.codescanner.findings (one per finding, with severity and
// blocked attributes). A planted secret in fresh content triggers a
// block finding.
func TestScannedStrategy_RecordsScansAndFindings(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "config.js", "const x = 1;\n")

	reader, m := newMetricsFixture(t)
	scanned := newPatternScannedStrategy(t, &types.CodeScannerConfig{Type: "patterns"})
	scanned.Metrics = m

	// A GitHub PAT pattern is a known block finding.
	input := json.RawMessage(`{
		"path": "config.js",
		"content": "const token = \"ghp_abcdefghijklmnopqrstuvwxyz0123456789\";\n"
	}`)
	result, err := scanned.Apply(context.Background(), input, exec)
	must.NoError(err)
	check.False(result.Applied, "expected Applied=false on block finding")

	rm := collectMetrics(t, reader)

	scans := collectInt64DataPoints(t, rm, "stirrup.codescanner.scans")
	must.Len(scans, 1, "stirrup.codescanner.scans data points")
	check.Equal("patterns", scans[0].attrs["scanner"], "scanner")
	check.Equal(int64(1), scans[0].value, "scan count")

	findings := collectInt64DataPoints(t, rm, "stirrup.codescanner.findings")
	must.NotEmpty(findings, "stirrup.codescanner.findings data points")
	var sawBlock bool
	for _, dp := range findings {
		check.Equal("patterns", dp.attrs["scanner"], "finding scanner")
		if dp.attrs["severity"] == codescanner.SeverityBlock && dp.attrs["blocked"] == "true" {
			sawBlock = true
		}
	}
	check.True(sawBlock, "expected a block-severity, blocked=true finding; got: %+v", findings)
}

// TestScannedStrategy_RecordsWarnFinding asserts that warn-severity
// findings are recorded with severity=warn and blocked=false.
func TestScannedStrategy_RecordsWarnFinding(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "config.py", "x = 1\n")

	reader, m := newMetricsFixture(t)
	scanned := newPatternScannedStrategy(t, &types.CodeScannerConfig{Type: "patterns"})
	scanned.Metrics = m

	// `eval(` matches the sink/dynamic_eval warn-severity pattern. The
	// edit succeeds (warns do not block by default) and the wrapper
	// records one warn finding with blocked=false.
	input := json.RawMessage(`{
		"path": "config.py",
		"content": "result = eval(input())\n"
	}`)
	result, err := scanned.Apply(context.Background(), input, exec)
	must.NoError(err)
	check.True(result.Applied, "expected Applied=true on warn-only finding")

	rm := collectMetrics(t, reader)

	findings := collectInt64DataPoints(t, rm, "stirrup.codescanner.findings")
	must.NotEmpty(findings, "stirrup.codescanner.findings data points")
	var sawWarn bool
	for _, dp := range findings {
		check.Equal("patterns", dp.attrs["scanner"], "finding scanner")
		if dp.attrs["severity"] == codescanner.SeverityWarn && dp.attrs["blocked"] == "false" {
			sawWarn = true
		}
	}
	check.True(sawWarn, "expected a warn-severity, blocked=false finding; got: %+v", findings)
}

// TestScannedStrategy_RecordsWarnFindingPromotedByBlockOnWarn asserts
// that when BlockOnWarn=true, warn-severity findings are promoted into
// the blocked bucket: blocked=true on the metric attribute.
func TestScannedStrategy_RecordsWarnFindingPromotedByBlockOnWarn(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "config.py", "x = 1\n")

	reader, m := newMetricsFixture(t)
	scanned := newPatternScannedStrategy(t, &types.CodeScannerConfig{
		Type:        "patterns",
		BlockOnWarn: true,
	})
	scanned.Metrics = m

	input := json.RawMessage(`{
		"path": "config.py",
		"content": "result = eval(input())\n"
	}`)
	result, err := scanned.Apply(context.Background(), input, exec)
	must.NoError(err)
	check.False(result.Applied, "expected Applied=false when BlockOnWarn promotes the warn")

	rm := collectMetrics(t, reader)

	findings := collectInt64DataPoints(t, rm, "stirrup.codescanner.findings")
	must.NotEmpty(findings, "stirrup.codescanner.findings data points")
	var sawPromoted bool
	for _, dp := range findings {
		if dp.attrs["severity"] == codescanner.SeverityWarn && dp.attrs["blocked"] == "true" {
			sawPromoted = true
		}
	}
	check.True(sawPromoted, "expected a warn-severity, blocked=true finding (BlockOnWarn promotion); got: %+v", findings)
}

// TestScannedStrategy_RecordsNoFindingsOnCleanContent asserts that a
// scan with no findings records the scan counter and no findings
// observations.
func TestScannedStrategy_RecordsNoFindingsOnCleanContent(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "config.txt", "anything\n")

	reader, m := newMetricsFixture(t)
	scanned := newPatternScannedStrategy(t, &types.CodeScannerConfig{Type: "patterns"})
	scanned.Metrics = m

	// Plain text with no secret patterns, no sinks.
	input := json.RawMessage(`{
		"path": "config.txt",
		"content": "hello world\nthis is fine\n"
	}`)
	result, err := scanned.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true on clean content; error: %s", result.Error)

	rm := collectMetrics(t, reader)

	scans := collectInt64DataPoints(t, rm, "stirrup.codescanner.scans")
	must.Len(scans, 1, "stirrup.codescanner.scans data points")

	findings := collectInt64DataPoints(t, rm, "stirrup.codescanner.findings")
	check.Empty(findings, "findings data points on clean content")
}

// --- helpers ---

// newMetricsFixture returns a manual reader and edit metrics that record into it.
func newMetricsFixture(t *testing.T) (*sdkmetric.ManualReader, *observability.Metrics) {
	t.Helper()
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	must.NoError(err)
	return reader, m
}

func newPatternScannedStrategy(t *testing.T, cfg *types.CodeScannerConfig) *ScannedStrategy {
	t.Helper()
	must := require.New(t)
	wrapped := NewScannedStrategy(NewWholeFileStrategy(), codescanner.NewPatternScanner(), cfg, nil)
	scanned, ok := wrapped.(*ScannedStrategy)
	must.True(ok, "expected ScannedStrategy from NewScannedStrategy")
	return scanned
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))
	return rm
}

type int64DataPoint struct {
	value int64
	attrs map[string]string
}

func collectInt64DataPoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []int64DataPoint {
	t.Helper()
	must := require.New(t)
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			must.True(ok, "metric %q is not a Sum[int64]", name)
			out := make([]int64DataPoint, 0, len(sum.DataPoints))
			for _, dp := range sum.DataPoints {
				attrs := make(map[string]string)
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.String()
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
