package permission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// stubPolicy is a configurable PermissionPolicy used to drive the
// metric recorder through allow / deny / error decision paths.
type stubPolicy struct {
	result *PermissionResult
	err    error
}

func (s *stubPolicy) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*PermissionResult, error) {
	return s.result, s.err
}

func newRecorder(t *testing.T, inner PermissionPolicy, policy string) (PermissionPolicy, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	require.NoError(t, err)
	return NewMetricRecorder(inner, m, policy), reader
}

// TestMetricRecorder_RecordsAllow verifies decision="allow" on a
// PermissionResult{Allowed:true}.
func TestMetricRecorder_RecordsAllow(t *testing.T) {
	rec, reader := newRecorder(t,
		&stubPolicy{result: &PermissionResult{Allowed: true}},
		"allow-all",
	)
	_, err := rec.Check(context.Background(), types.ToolDefinition{Name: "read_file"}, nil)
	require.NoError(t, err)

	dps := collectDecisions(t, reader)
	require.Len(t, dps, 1)
	assert.Equal(t, "allow", dps[0].attrs["decision"])
	assert.Equal(t, "allow-all", dps[0].attrs["policy"])
	assert.Equal(t, "read_file", dps[0].attrs["tool"])
}

// TestMetricRecorder_RecordsDeny verifies decision="deny" on a
// PermissionResult{Allowed:false}.
func TestMetricRecorder_RecordsDeny(t *testing.T) {
	rec, reader := newRecorder(t,
		&stubPolicy{result: &PermissionResult{Allowed: false, Reason: "no"}},
		"deny-side-effects",
	)
	_, err := rec.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, nil)
	require.NoError(t, err)

	dps := collectDecisions(t, reader)
	require.Len(t, dps, 1)
	assert.Equal(t, "deny", dps[0].attrs["decision"])
	assert.Equal(t, "deny-side-effects", dps[0].attrs["policy"])
}

// TestMetricRecorder_RecordsError verifies decision="error" when the
// inner policy returns a non-nil error.
func TestMetricRecorder_RecordsError(t *testing.T) {
	rec, reader := newRecorder(t,
		&stubPolicy{err: errors.New("transport failed")},
		"ask-upstream",
	)
	_, err := rec.Check(context.Background(), types.ToolDefinition{Name: "spawn_agent"}, nil)
	require.Error(t, err, "expected error to propagate")

	dps := collectDecisions(t, reader)
	require.Len(t, dps, 1)
	assert.Equal(t, "error", dps[0].attrs["decision"])
	assert.Equal(t, "ask-upstream", dps[0].attrs["policy"])
}

// TestMetricRecorder_NilMetrics_ReturnsInner verifies the wrapper has
// zero overhead when metrics are disabled.
func TestMetricRecorder_NilMetrics_ReturnsInner(t *testing.T) {
	inner := &stubPolicy{result: &PermissionResult{Allowed: true}}
	got := NewMetricRecorder(inner, nil, "allow-all")
	require.Same(t, inner, got, "nil metrics must return inner unchanged")
}

// TestUnwrap_PassesThroughWrapper verifies the standalone Unwrap helper
// returns the inner policy when wrapped, and the policy itself when not.
func TestUnwrap_PassesThroughWrapper(t *testing.T) {
	inner := &stubPolicy{result: &PermissionResult{Allowed: true}}

	t.Run("unwrapped policy returns itself", func(t *testing.T) {
		require.Same(t, inner, Unwrap(inner))
	})

	t.Run("wrapped policy returns inner", func(t *testing.T) {
		rec, _ := newRecorder(t, inner, "allow-all")
		require.Same(t, inner, Unwrap(rec))
	})
}

// TestMetricRecorder_Unwrap_AddsApprovalTools verifies that callers can
// reach the inner AskUpstreamPolicy through Unwrap to register tools that
// arrive after construction. The wrapper has no AddApprovalTool of its
// own, so an unreachable inner policy would auto-allow those tools.
func TestMetricRecorder_Unwrap_AddsApprovalTools(t *testing.T) {
	ask := NewAskUpstreamPolicy(nopTransport{}, map[string]bool{}, 0)
	rec, _ := newRecorder(t, ask, "ask-upstream")

	inner, ok := Unwrap(rec).(*AskUpstreamPolicy)
	require.True(t, ok, "Unwrap should reach *AskUpstreamPolicy through the metric wrapper, got %T", Unwrap(rec))
	inner.AddApprovalTool("late_tool")

	require.Contains(t, ask.ApprovalToolNames(), "late_tool")
}

// nopTransport is a Transport stub for tests that need an
// AskUpstreamPolicy without exercising the wire path.
type nopTransport struct{}

func (nopTransport) Emit(_ types.HarnessEvent) error            { return nil }
func (nopTransport) OnControl(_ func(event types.ControlEvent)) {}

// --- helpers ---

type counterDataPoint struct {
	value int64
	attrs map[string]string
}

// collectDecisions returns the data points recorded on the permission
// decision counter since the reader was created.
func collectDecisions(t *testing.T, reader *sdkmetric.ManualReader) []counterDataPoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return collectInt64Counters(t, rm, "stirrup.permission.decisions")
}

func collectInt64Counters(t *testing.T, rm metricdata.ResourceMetrics, name string) []counterDataPoint {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != name {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %q is not a Sum[int64]", name)
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
