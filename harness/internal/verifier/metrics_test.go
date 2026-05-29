package verifier

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
)

// TestMetricRecorder_RecordsPassingRun verifies that wrapping a passing
// verifier records runs (passed=true) and duration with the type label
// supplied at construction time.
func TestMetricRecorder_RecordsPassingRun(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	wrapped := NewMetricRecorder(passingStub(map[string]any{"ok": true}), m, "test-runner")
	if _, err := wrapped.Verify(context.Background(), VerifyContext{}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	dps := collectInt64Counters(t, rm, "stirrup.verifier.runs")
	if len(dps) != 1 {
		t.Fatalf("expected 1 run data point, got %d", len(dps))
	}
	if dps[0].attrs["type"] != "test-runner" {
		t.Errorf("type = %q, want test-runner", dps[0].attrs["type"])
	}
	if dps[0].attrs["passed"] != "true" {
		t.Errorf("passed = %q, want true", dps[0].attrs["passed"])
	}

	if !histogramHasObservation(t, rm, "stirrup.verifier.duration_ms") {
		t.Error("stirrup.verifier.duration_ms recorded no observations")
	}
}

// TestMetricRecorder_RecordsFailingRun verifies passed=false on a
// failing verifier (Passed=false, no error).
func TestMetricRecorder_RecordsFailingRun(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	wrapped := NewMetricRecorder(failingStub("nope", nil), m, "llm-judge")
	if _, err := wrapped.Verify(context.Background(), VerifyContext{}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	dps := collectInt64Counters(t, rm, "stirrup.verifier.runs")
	if len(dps) != 1 {
		t.Fatalf("expected 1 run data point, got %d", len(dps))
	}
	if dps[0].attrs["type"] != "llm-judge" {
		t.Errorf("type = %q, want llm-judge", dps[0].attrs["type"])
	}
	if dps[0].attrs["passed"] != "false" {
		t.Errorf("passed = %q, want false", dps[0].attrs["passed"])
	}
}

// TestMetricRecorder_TreatsErrorAsFailed asserts that a verifier that
// returns an error counts as passed=false on the metric, even though
// the result itself may be nil.
func TestMetricRecorder_TreatsErrorAsFailed(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	wrapped := NewMetricRecorder(errorStub(errors.New("boom")), m, "test-runner")
	_, gotErr := wrapped.Verify(context.Background(), VerifyContext{})
	if gotErr == nil {
		t.Fatal("expected error to propagate")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	dps := collectInt64Counters(t, rm, "stirrup.verifier.runs")
	if len(dps) != 1 {
		t.Fatalf("expected 1 run data point, got %d", len(dps))
	}
	if dps[0].attrs["passed"] != "false" {
		t.Errorf("passed = %q, want false on error", dps[0].attrs["passed"])
	}
}

// TestMetricRecorder_NilMetricsReturnsInner verifies the zero-overhead
// path when metrics are disabled — the wrapper returns inner unchanged
// so no extra allocation or method dispatch happens per Verify.
func TestMetricRecorder_NilMetricsReturnsInner(t *testing.T) {
	inner := passingStub(nil)
	got := NewMetricRecorder(inner, nil, "none")
	if got != inner {
		t.Errorf("nil metrics must return inner unchanged")
	}
}

// --- helpers ---

type counterDataPoint struct {
	value int64
	attrs map[string]string
}

func collectInt64Counters(t *testing.T, rm metricdata.ResourceMetrics, name string) []counterDataPoint {
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
