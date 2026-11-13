package security

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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
	must := require.New(t)
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	sl.PathTraversalBlocked("../etc/passwd", "/workspace")

	// Emit writes a JSON line and does not panic when no counter is wired.
	must.Contains(buf.String(), `"event":"path_traversal_blocked"`)
}

func TestSecurityLogger_Emit_IncrementsCounterPerEvent(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	c := &fakeCounter{}
	sl.SetEventCounter(c)

	sl.PathTraversalBlocked("../etc/passwd", "/workspace")
	sl.PrototypePollutionBlocked("read_file", []string{"__proto__"})
	sl.SecretRedactedInOutput("anthropic_api_key", "transport.stdio.event.text")

	must.Len(c.calls, 3)
	for i, call := range c.calls {
		must.Equal(int64(1), call.incr, "calls[%d].incr", i)
	}
}

func TestSecurityLogger_Emit_IncrementsOTelCounter(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	counter, err := provider.Meter("test").Int64Counter("stirrup.harness.security_events")
	must.NoError(err)
	sl.SetEventCounter(counter)

	sl.PathTraversalBlocked("a", "b")
	sl.ToolInputRejected("read_file", []string{"err"})

	total, events := collectSecurityEventCounts(t, reader)
	must.Equal(int64(2), total)
	must.Contains(events, "path_traversal_blocked")
	must.Contains(events, "tool_input_rejected")
}

// collectSecurityEventCounts sums the security_events counter across its data
// points and returns the "event" attribute of each data point.
func collectSecurityEventCounts(t *testing.T, reader *sdkmetric.ManualReader) (int64, []string) {
	t.Helper()
	must := require.New(t)

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))

	var total int64
	var events []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.security_events" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			must.True(ok, "unexpected data type %T", m.Data)
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
			must := require.New(t)
			var buf bytes.Buffer
			sl := NewSecurityLogger(&buf, "run-1")
			tc.fire(sl)
			var got SecurityEvent
			must.NoError(json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got), "output=%q", buf.String())
			must.Equal(tc.event, got.Event)
			for k, want := range tc.wantData {
				must.Equal(want, got.Data[k], "Data[%q]", k)
			}
		})
	}
}

// TestSecurityLogger_Emit_JSONShapeWithCounter confirms that Emit still
// produces the expected JSON output when a counter is wired.
func TestSecurityLogger_Emit_JSONShapeWithCounter(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")
	sl.SetEventCounter(&fakeCounter{})
	sl.SecretRedactedInOutput("anthropic_api_key", "transport.grpc.event.text")

	var got SecurityEvent
	must.NoError(json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got))
	must.Equal("secret_redacted_in_output", got.Event)
	must.Equal("anthropic_api_key", got.Data["pattern"])
	must.Equal("transport.grpc.event.text", got.Data["location"])
}

func TestSecurityLogger_SandboxRuntimeDiagnostic_EventLevelAndKeys(t *testing.T) {
	observed := map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo,dummy0"}
	cases := []struct {
		name      string
		passed    bool
		wantEvent string
		wantLevel string
		wantKeys  []string
	}{
		{
			name:      "passed",
			passed:    true,
			wantEvent: "sandbox_runtime_diagnostic_passed",
			wantLevel: "info",
			wantKeys:  []string{"observed", "runtime"},
		},
		{
			name:      "failed",
			passed:    false,
			wantEvent: "sandbox_runtime_diagnostic_failed",
			wantLevel: "error",
			wantKeys:  []string{"check", "observed", "reason", "runtime"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer
			sl := NewSecurityLogger(&buf, "run-1")
			sl.SandboxRuntimeDiagnostic("nitro-isolated", tc.passed, "dmi_product_name", "product name does not end in .enclave", observed)

			var got SecurityEvent
			must.NoError(json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got), "output=%q", buf.String())
			must.Equal(tc.wantEvent, got.Event)
			must.Equal(tc.wantLevel, got.Level)
			keys := make([]string, 0, len(got.Data))
			for k := range got.Data {
				keys = append(keys, k)
			}
			must.ElementsMatch(tc.wantKeys, keys)
			must.Equal("nitro-isolated", got.Data["runtime"])
			must.Equal(map[string]any{"dmiProductName": "m5d.enclave", "interfaces": "lo,dummy0"}, got.Data["observed"])
			if !tc.passed {
				must.Equal("dmi_product_name", got.Data["check"])
				must.Equal("product name does not end in .enclave", got.Data["reason"])
			}
		})
	}
}

// Observed values come from the sandbox image, so a secret-shaped value
// must reach the event stream redacted, in the observed map and the reason.
func TestSecurityLogger_SandboxRuntimeDiagnostic_RedactsObservedSecrets(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")
	sl.SandboxRuntimeDiagnostic("nitro-isolated", false, "dmi_product_name",
		`product name "sk-ant-abc123_DEF-456" does not end in .enclave`,
		map[string]string{"dmiProductName": "sk-ant-abc123_DEF-456"})

	must.NotContains(buf.String(), "sk-ant-abc123_DEF-456")
	var got SecurityEvent
	must.NoError(json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got))
	observed, ok := got.Data["observed"].(map[string]any)
	must.True(ok, "observed is a JSON object: %#v", got.Data["observed"])
	must.Contains(observed["dmiProductName"], redactedPlaceholder)
	must.Contains(got.Data["reason"], redactedPlaceholder)
}

func TestSecurityLogger_SandboxRuntimeDiagnostic_BoundsObservedValues(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")
	long := strings.Repeat("é", sandboxDiagnosticValueMaxBytes)
	sl.SandboxRuntimeDiagnostic("nitro-isolated", false, "interfaces", long, map[string]string{"interfaces": long})

	var got SecurityEvent
	must.NoError(json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got))
	observed, ok := got.Data["observed"].(map[string]any)
	must.True(ok, "observed is a JSON object: %#v", got.Data["observed"])
	for _, v := range []any{observed["interfaces"], got.Data["reason"]} {
		s, isString := v.(string)
		must.True(isString, "bounded value is a string: %#v", v)
		must.LessOrEqual(len(s), sandboxDiagnosticValueMaxBytes)
		must.True(utf8.ValidString(s), "bounded value splits a rune: %q", s)
		must.True(strings.HasSuffix(s, "…"), "bounded value is marked as cut: %q", s)
	}
}
