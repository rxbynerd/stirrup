package permission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	return NewMetricRecorder(inner, m, policy), reader
}

// TestMetricRecorder_RecordsAllow verifies decision="allow" on a
// PermissionResult{Allowed:true}.
func TestMetricRecorder_RecordsAllow(t *testing.T) {
	rec, reader := newRecorder(t,
		&stubPolicy{result: &PermissionResult{Allowed: true}},
		"allow-all",
	)
	if _, err := rec.Check(context.Background(), types.ToolDefinition{Name: "read_file"}, nil); err != nil {
		t.Fatalf("Check: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := collectInt64Counters(t, rm, "stirrup.permission.decisions")
	if len(dps) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(dps))
	}
	if dps[0].attrs["decision"] != "allow" {
		t.Errorf("decision = %q, want allow", dps[0].attrs["decision"])
	}
	if dps[0].attrs["policy"] != "allow-all" {
		t.Errorf("policy = %q, want allow-all", dps[0].attrs["policy"])
	}
	if dps[0].attrs["tool"] != "read_file" {
		t.Errorf("tool = %q, want read_file", dps[0].attrs["tool"])
	}
}

// TestMetricRecorder_RecordsDeny verifies decision="deny" on a
// PermissionResult{Allowed:false}.
func TestMetricRecorder_RecordsDeny(t *testing.T) {
	rec, reader := newRecorder(t,
		&stubPolicy{result: &PermissionResult{Allowed: false, Reason: "no"}},
		"deny-side-effects",
	)
	if _, err := rec.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, nil); err != nil {
		t.Fatalf("Check: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := collectInt64Counters(t, rm, "stirrup.permission.decisions")
	if len(dps) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(dps))
	}
	if dps[0].attrs["decision"] != "deny" {
		t.Errorf("decision = %q, want deny", dps[0].attrs["decision"])
	}
	if dps[0].attrs["policy"] != "deny-side-effects" {
		t.Errorf("policy = %q, want deny-side-effects", dps[0].attrs["policy"])
	}
}

// TestMetricRecorder_RecordsError verifies decision="error" when the
// inner policy returns a non-nil error.
func TestMetricRecorder_RecordsError(t *testing.T) {
	rec, reader := newRecorder(t,
		&stubPolicy{err: errors.New("transport failed")},
		"ask-upstream",
	)
	_, gotErr := rec.Check(context.Background(), types.ToolDefinition{Name: "spawn_agent"}, nil)
	if gotErr == nil {
		t.Fatal("expected error to propagate")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := collectInt64Counters(t, rm, "stirrup.permission.decisions")
	if len(dps) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(dps))
	}
	if dps[0].attrs["decision"] != "error" {
		t.Errorf("decision = %q, want error", dps[0].attrs["decision"])
	}
	if dps[0].attrs["policy"] != "ask-upstream" {
		t.Errorf("policy = %q, want ask-upstream", dps[0].attrs["policy"])
	}
}

// TestMetricRecorder_NilMetricsReturnsInner verifies the wrapper has
// zero overhead when metrics are disabled.
func TestMetricRecorder_NilMetricsReturnsInner(t *testing.T) {
	inner := &stubPolicy{result: &PermissionResult{Allowed: true}}
	got := NewMetricRecorder(inner, nil, "allow-all")
	if got != inner {
		t.Errorf("nil metrics must return inner unchanged")
	}
}

// TestUnwrap_PassesThroughWrapper verifies the standalone Unwrap helper
// returns the inner policy when wrapped, and the policy itself when not.
func TestUnwrap_PassesThroughWrapper(t *testing.T) {
	inner := &stubPolicy{result: &PermissionResult{Allowed: true}}
	if Unwrap(inner) != inner {
		t.Error("Unwrap on unwrapped policy must return self")
	}

	rec, _ := newRecorder(t, inner, "allow-all")
	if Unwrap(rec) != inner {
		t.Error("Unwrap on wrapped policy must return inner")
	}
}

// TestMetricRecorder_AddApprovalToolViaUnwrap verifies that callers
// can reach the inner AskUpstreamPolicy through Unwrap to register
// late-arriving approval tools. Without this access path,
// wrapping ask-upstream would silently auto-allow tools registered
// after policy construction (e.g. spawn_agent). The wrapper does not
// expose its own AddApprovalTool — the canonical entry point is
// Unwrap, which is uniform across wrapped and unwrapped policies.
func TestMetricRecorder_AddApprovalToolViaUnwrap(t *testing.T) {
	ask := NewAskUpstreamPolicy(nopTransport{}, map[string]bool{}, 0)
	rec, _ := newRecorder(t, ask, "ask-upstream")

	inner, ok := Unwrap(rec).(*AskUpstreamPolicy)
	if !ok {
		t.Fatalf("Unwrap should reach *AskUpstreamPolicy through the metric wrapper, got %T", Unwrap(rec))
	}
	inner.AddApprovalTool("late_tool")

	got := ask.ApprovalToolNames()
	var found bool
	for _, n := range got {
		if n == "late_tool" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("late_tool not in approval set; got %v", got)
	}
}

// nopTransport is a Transport stub for tests that need an
// AskUpstreamPolicy without exercising the wire path.
type nopTransport struct{}

func (nopTransport) Emit(_ types.HarnessEvent) error             { return nil }
func (nopTransport) OnControl(_ func(event types.ControlEvent)) {}

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
					attrs[string(kv.Key)] = kv.Value.Emit()
				}
				out = append(out, counterDataPoint{value: dp.Value, attrs: attrs})
			}
			return out
		}
	}
	return nil
}
