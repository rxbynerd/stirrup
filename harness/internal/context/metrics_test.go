package context

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func newRecorder(t *testing.T, inner ContextStrategy, strat string) (ContextStrategy, *sdkmetric.ManualReader) {
	t.Helper()
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	must.NoError(err)
	return NewMetricRecorder(inner, m, strat), reader
}

// oversizedHistory returns alternating user and assistant messages large
// enough that a tight budget forces the sliding-window strategy to drop
// messages from the front.
func oversizedHistory() []types.Message {
	msgs := make([]types.Message, 5)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = types.Message{
			Role:    role,
			Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", 400)}},
		}
	}
	return msgs
}

// TestMetricRecorder_RecordsNoop verifies that a Prepare call which
// did not trigger compaction (messages already fit) records kind="noop".
func TestMetricRecorder_RecordsNoop(t *testing.T) {
	must := require.New(t)
	rec, reader := newRecorder(t, NewSlidingWindowStrategy(), "sliding-window")

	// Tiny message slice with a generous budget, so no compaction is expected.
	msgs := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
	}
	_, err := rec.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          1000,
		CurrentTokens:      4,
		ReserveForResponse: 100,
	})
	must.NoError(err)

	dps := collectStrategyRuns(t, reader)
	must.Len(dps, 1)
	must.Equal("sliding-window", dps[0].attrs["strategy"])
	must.Equal("noop", dps[0].attrs["kind"])
}

// TestMetricRecorder_RecordsCompaction verifies that a Prepare call
// which triggered compaction records kind="compaction".
func TestMetricRecorder_RecordsCompaction(t *testing.T) {
	must := require.New(t)
	rec, reader := newRecorder(t, NewSlidingWindowStrategy(), "sliding-window")

	// The TokenBudget reports CurrentTokens well above the available room,
	// so the truncation branch fires.
	_, err := rec.Prepare(context.Background(), oversizedHistory(), TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      500,
		ReserveForResponse: 50,
	})
	must.NoError(err)

	dps := collectStrategyRuns(t, reader)
	must.Len(dps, 1)
	must.Equal("compaction", dps[0].attrs["kind"])
}

// TestMetricRecorder_LastCompactionPassthrough verifies the wrapper
// does not hide the inner strategy's LastCompaction event from the
// loop. Without this, the loop's existing context_compactions counter
// would never increment because the loop reads LastCompaction off the
// strategy directly.
func TestMetricRecorder_LastCompactionPassthrough(t *testing.T) {
	must := require.New(t)
	inner := NewSlidingWindowStrategy()
	rec, _ := newRecorder(t, inner, "sliding-window")

	_, err := rec.Prepare(context.Background(), oversizedHistory(), TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      500,
		ReserveForResponse: 50,
	})
	must.NoError(err)

	must.NotNil(rec.LastCompaction(), "wrapper LastCompaction returned nil after compaction; pass-through broken")
	must.NotNil(inner.LastCompaction(), "inner LastCompaction returned nil after compaction; setup invariant violated")
}

// TestMetricRecorder_NilMetricsReturnsInner verifies the wrapper has
// zero overhead when metrics are disabled.
func TestMetricRecorder_NilMetricsReturnsInner(t *testing.T) {
	must := require.New(t)
	inner := NewSlidingWindowStrategy()
	got := NewMetricRecorder(inner, nil, "sliding-window")
	must.Same(inner, got, "nil metrics must return the inner strategy unchanged")
}

// collectStrategyRuns returns the data points of the strategy_runs counter.
func collectStrategyRuns(t *testing.T, reader *sdkmetric.ManualReader) []counterDataPoint {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))
	return collectInt64Counters(t, rm, "stirrup.context.strategy_runs")
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
			must.True(ok, "metric %q is not a Sum[int64]", name)
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
