package security

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeCounter records calls for assertion. It satisfies the EventCounter
// interface defined in securityevent.go.
type fakeCounter struct {
	calls []fakeCounterCall
}

type fakeCounterCall struct {
	incr int64
	// We only need to confirm Add was called with the right increment +
	// "event" attribute; we deliberately do not introspect the option type
	// because that is OTel-internal.
}

func (f *fakeCounter) Add(_ context.Context, incr int64, _ ...metric.AddOption) {
	f.calls = append(f.calls, fakeCounterCall{incr: incr})
}

func TestSecurityLogger_Emit_WritesEventWithoutCounter(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	sl.PathTraversalBlocked("../etc/passwd", "/workspace")

	// Emit writes a JSON line and does not panic when no counter is wired.
	require.Contains(t, buf.String(), `"event":"path_traversal_blocked"`)
}

func TestSecurityLogger_Emit_IncrementsCounterPerEvent(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	c := &fakeCounter{}
	sl.SetEventCounter(c)

	sl.PathTraversalBlocked("../etc/passwd", "/workspace")
	sl.PrototypePollutionBlocked("read_file", []string{"__proto__"})
	sl.SecretRedactedInOutput("anthropic_api_key", "transport.stdio.event.text")

	require.Len(t, c.calls, 3)
	for i, call := range c.calls {
		require.Equal(t, int64(1), call.incr, "calls[%d].incr", i)
	}
}

func TestSecurityLogger_Emit_IncrementsOTelCounter(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	counter, err := provider.Meter("test").Int64Counter("stirrup.harness.security_events")
	require.NoError(t, err)
	sl.SetEventCounter(counter)

	sl.PathTraversalBlocked("a", "b")
	sl.ToolInputRejected("read_file", []string{"err"})

	total, events := collectSecurityEventCounts(t, reader)
	require.Equal(t, int64(2), total)
	require.Contains(t, events, "path_traversal_blocked")
	require.Contains(t, events, "tool_input_rejected")
}

// collectSecurityEventCounts sums the security_events counter across its data
// points and returns the "event" attribute of each data point.
func collectSecurityEventCounts(t *testing.T, reader *sdkmetric.ManualReader) (int64, []string) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	var total int64
	var events []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.security_events" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "unexpected data type %T", m.Data)
			for _, dp := range sum.DataPoints {
				total += dp.Value
				if v, ok := dp.Attributes.Value("event"); ok {
					events = append(events, v.AsString())
				}
			}
		}
	}
	return total, events
}

// TestSecurityLogger_SetEventCounter_IsRaceFree runs concurrent SetEventCounter
// and Emit calls. Run it under -race to catch unsynchronised access to the
// event counter.
func TestSecurityLogger_SetEventCounter_IsRaceFree(t *testing.T) {
	sl := NewSecurityLogger(io.Discard, "run-race")

	const iterations = 1000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		c := &fakeCounter{}
		for i := 0; i < iterations; i++ {
			// Alternate between a real fake and nil to make sure both write
			// paths (set and unset) are exercised.
			if i%2 == 0 {
				sl.SetEventCounter(c)
			} else {
				sl.SetEventCounter(nil)
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			sl.PathTraversalBlocked("a", "b")
		}
	}()

	wg.Wait()
}

// TestSecurityLogger_Guard_EmitsEventAndFields checks that each guard helper
// emits its event name and the structured data fields operators rely on, such
// as criterion on guard_denied.
func TestSecurityLogger_Guard_EmitsEventAndFields(t *testing.T) {
	cases := []struct {
		name     string
		fire     func(sl *SecurityLogger)
		event    string
		wantData map[string]any
	}{
		{
			name:  "guard allowed",
			fire:  func(sl *SecurityLogger) { sl.GuardAllowed("pre_turn", "granite-guardian") },
			event: "guard_allowed",
			wantData: map[string]any{
				"phase":   "pre_turn",
				"guardId": "granite-guardian",
			},
		},
		{
			name:  "guard denied includes criterion and reason",
			fire:  func(sl *SecurityLogger) { sl.GuardDenied("pre_tool", "granite-guardian", "harm", "rule violation") },
			event: "guard_denied",
			wantData: map[string]any{
				"phase":     "pre_tool",
				"guardId":   "granite-guardian",
				"criterion": "harm",
				"reason":    "rule violation",
			},
		},
		{
			name:  "guard skipped",
			fire:  func(sl *SecurityLogger) { sl.GuardSkipped("pre_turn", "granite-guardian") },
			event: "guard_skipped",
			wantData: map[string]any{
				"phase":   "pre_turn",
				"guardId": "granite-guardian",
			},
		},
		{
			name:  "guard spotlighted includes reason",
			fire:  func(sl *SecurityLogger) { sl.GuardSpotlighted("pre_turn", "granite-guardian", "low confidence") },
			event: "guard_spotlighted",
			wantData: map[string]any{
				"phase":   "pre_turn",
				"guardId": "granite-guardian",
				"reason":  "low confidence",
			},
		},
		{
			name:  "guard error includes error string",
			fire:  func(sl *SecurityLogger) { sl.GuardError("post_turn", "granite-guardian", "connection refused") },
			event: "guard_error",
			wantData: map[string]any{
				"phase":   "post_turn",
				"guardId": "granite-guardian",
				"error":   "connection refused",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sl := NewSecurityLogger(&buf, "run-1")
			tc.fire(sl)
			var got SecurityEvent
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got), "output=%q", buf.String())
			require.Equal(t, tc.event, got.Event)
			for k, want := range tc.wantData {
				require.Equal(t, want, got.Data[k], "Data[%q]", k)
			}
		})
	}
}

// TestSecurityLogger_Emit_JSONShapeWithCounter confirms that Emit still
// produces the expected JSON output when a counter is wired.
func TestSecurityLogger_Emit_JSONShapeWithCounter(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")
	sl.SetEventCounter(&fakeCounter{})
	sl.SecretRedactedInOutput("anthropic_api_key", "transport.grpc.event.text")

	var got SecurityEvent
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got))
	require.Equal(t, "secret_redacted_in_output", got.Event)
	require.Equal(t, "anthropic_api_key", got.Data["pattern"])
	require.Equal(t, "transport.grpc.event.text", got.Data["location"])
}
