package verifier

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
)

// TestMetricRecorder_RecordsPassingRun verifies that wrapping a passing
// verifier records runs (passed=true) and duration with the type label
// supplied at construction time.
func TestMetricRecorder_RecordsPassingRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	reader, m := newRecorderTestMetrics(t)

	wrapped := NewMetricRecorder(passingStub(map[string]any{"ok": true}), m, "test-runner")
	_, err := wrapped.Verify(context.Background(), VerifyContext{})
	must.NoError(err)

	rm := collectRunMetrics(t, reader)

	dps := collectInt64Counters(t, rm, "stirrup.verifier.runs")
	must.Len(dps, 1)
	check.Equal("test-runner", dps[0].attrs["type"])
	check.Equal("true", dps[0].attrs["passed"])

	check.True(histogramHasObservation(t, rm, "stirrup.verifier.duration"),
		"stirrup.verifier.duration recorded no observations")
}

// TestMetricRecorder_RecordsFailingRun verifies passed=false on a
// failing verifier (Passed=false, no error).
func TestMetricRecorder_RecordsFailingRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	reader, m := newRecorderTestMetrics(t)

	wrapped := NewMetricRecorder(failingStub("nope", nil), m, "llm-judge")
	_, err := wrapped.Verify(context.Background(), VerifyContext{})
	must.NoError(err)

	rm := collectRunMetrics(t, reader)

	dps := collectInt64Counters(t, rm, "stirrup.verifier.runs")
	must.Len(dps, 1)
	check.Equal("llm-judge", dps[0].attrs["type"])
	check.Equal("false", dps[0].attrs["passed"])
}

// TestMetricRecorder_TreatsErrorAsFailed asserts that a verifier that
// returns an error counts as passed=false on the metric, even though
// the result itself may be nil.
func TestMetricRecorder_TreatsErrorAsFailed(t *testing.T) {
	must := require.New(t)
	reader, m := newRecorderTestMetrics(t)

	wrapped := NewMetricRecorder(errorStub(errors.New("boom")), m, "test-runner")
	_, err := wrapped.Verify(context.Background(), VerifyContext{})
	must.Error(err, "verifier error must propagate")

	rm := collectRunMetrics(t, reader)

	dps := collectInt64Counters(t, rm, "stirrup.verifier.runs")
	must.Len(dps, 1)
	must.Equal("false", dps[0].attrs["passed"])
}

// TestMetricRecorder_NilMetricsReturnsInner verifies the zero-overhead
// path when metrics are disabled — the wrapper returns inner unchanged
// so no extra allocation or method dispatch happens per Verify.
func TestMetricRecorder_NilMetricsReturnsInner(t *testing.T) {
	must := require.New(t)
	inner := passingStub(nil)
	got := NewMetricRecorder(inner, nil, "none")
	must.Same(inner, got)
}

// --- helpers ---

// newRecorderTestMetrics returns a manual reader and metrics that export to it.
func newRecorderTestMetrics(t *testing.T) (*sdkmetric.ManualReader, *observability.Metrics) {
	t.Helper()
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	must.NoError(err)
	return reader, m
}

// collectRunMetrics collects everything recorded so far by reader.
func collectRunMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))
	return rm
}

type counterDataPoint struct {
	value int64
	attrs map[string]string
}

func collectInt64Counters(t *testing.T, rm metricdata.ResourceMetrics, name string) []counterDataPoint {
	t.Helper()
	must := require.New(t)
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			must.Truef(ok, "metric %q is not a Sum[int64]", name)
			out := make([]counterDataPoint, 0, len(sum.DataPoints))
			for _, dp := range sum.DataPoints {
				attrs := make(map[string]string)
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.String()
				}
				out = append(out, counterDataPoint{value: dp.Value, attrs: attrs})
			}
			return out
		}
	}
	return nil
}

func histogramHasObservation(t *testing.T, rm metricdata.ResourceMetrics, name string) bool {
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
