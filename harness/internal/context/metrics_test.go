package context

import (
	"context"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func newRecorder(t *testing.T, inner ContextStrategy, strat string) (ContextStrategy, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	return NewMetricRecorder(inner, m, strat), reader
}

// TestMetricRecorder_RecordsNoop verifies that a Prepare call which
// did not trigger compaction (messages already fit) records kind="noop".
func TestMetricRecorder_RecordsNoop(t *testing.T) {
	rec, reader := newRecorder(t, NewSlidingWindowStrategy(), "sliding-window")

	// Tiny message slice with a generous budget — no compaction
	// expected.
	msgs := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
	}
	if _, err := rec.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          1000,
		CurrentTokens:      4,
		ReserveForResponse: 100,
	}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := collectInt64Counters(t, rm, "stirrup.context.strategy_runs")
	if len(dps) != 1 {
		t.Fatalf("expected 1 strategy run, got %d", len(dps))
	}
	if dps[0].attrs["strategy"] != "sliding-window" {
		t.Errorf("strategy = %q, want sliding-window", dps[0].attrs["strategy"])
	}
	if dps[0].attrs["kind"] != "noop" {
		t.Errorf("kind = %q, want noop", dps[0].attrs["kind"])
	}
}

// TestMetricRecorder_RecordsCompaction verifies that a Prepare call
// which triggered compaction records kind="compaction".
func TestMetricRecorder_RecordsCompaction(t *testing.T) {
	rec, reader := newRecorder(t, NewSlidingWindowStrategy(), "sliding-window")

	// Five messages with a tight budget so the strategy must drop
	// some from the front. The TokenBudget reports CurrentTokens
	// well above the available room so the truncation branch fires.
	msgs := make([]types.Message, 5)
	for i := range msgs {
		msgs[i] = types.Message{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", 400)}},
		}
	}
	if _, err := rec.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      500,
		ReserveForResponse: 50,
	}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := collectInt64Counters(t, rm, "stirrup.context.strategy_runs")
	if len(dps) != 1 {
		t.Fatalf("expected 1 strategy run, got %d", len(dps))
	}
	if dps[0].attrs["kind"] != "compaction" {
		t.Errorf("kind = %q, want compaction", dps[0].attrs["kind"])
	}
}

// TestMetricRecorder_LastCompactionPassthrough verifies the wrapper
// does not hide the inner strategy's LastCompaction event from the
// loop. Without this, the loop's existing context_compactions counter
// would never increment because the loop reads LastCompaction off the
// strategy directly.
func TestMetricRecorder_LastCompactionPassthrough(t *testing.T) {
	inner := NewSlidingWindowStrategy()
	rec, _ := newRecorder(t, inner, "sliding-window")

	msgs := make([]types.Message, 5)
	for i := range msgs {
		msgs[i] = types.Message{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", 400)}},
		}
	}
	if _, err := rec.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      500,
		ReserveForResponse: 50,
	}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if rec.LastCompaction() == nil {
		t.Error("wrapper LastCompaction returned nil after compaction; pass-through broken")
	}
	if inner.LastCompaction() == nil {
		t.Error("inner LastCompaction returned nil after compaction; setup invariant violated")
	}
}

// TestMetricRecorder_NilMetricsReturnsInner verifies the wrapper has
// zero overhead when metrics are disabled.
func TestMetricRecorder_NilMetricsReturnsInner(t *testing.T) {
	inner := NewSlidingWindowStrategy()
	got := NewMetricRecorder(inner, nil, "sliding-window")
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
