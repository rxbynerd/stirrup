package security

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

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

func TestSecurityLogger_EmitWithoutCounter(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	sl.PathTraversalBlocked("../etc/passwd", "/workspace")

	// Emit should write a JSON line; no panic when no counter is wired.
	if !strings.Contains(buf.String(), `"event":"path_traversal_blocked"`) {
		t.Errorf("expected path_traversal_blocked event in output, got %q", buf.String())
	}
}

func TestSecurityLogger_EmitIncrementsCounter(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	c := &fakeCounter{}
	sl.SetEventCounter(c)

	sl.PathTraversalBlocked("../etc/passwd", "/workspace")
	sl.PrototypePollutionBlocked("read_file", []string{"__proto__"})
	sl.SecretRedactedInOutput("anthropic_api_key", "transport.stdio.event.text")

	if len(c.calls) != 3 {
		t.Fatalf("counter.Add called %d times, want 3", len(c.calls))
	}
	for i, call := range c.calls {
		if call.incr != 1 {
			t.Errorf("calls[%d].incr = %d, want 1", i, call.incr)
		}
	}
}

func TestSecurityLogger_EmitIncrementsRealOTelCounter(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	counter, err := provider.Meter("test").Int64Counter("stirrup.harness.security_events")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	sl.SetEventCounter(counter)

	sl.PathTraversalBlocked("a", "b")
	sl.ToolInputRejected("read_file", []string{"err"})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var total int64
	var seenEvents []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.security_events" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data type: %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
				if v, ok := dp.Attributes.Value("event"); ok {
					seenEvents = append(seenEvents, v.AsString())
				}
			}
		}
	}
	if total != 2 {
		t.Errorf("counter total = %d, want 2", total)
	}
	if !contains(seenEvents, "path_traversal_blocked") {
		t.Errorf("missing path_traversal_blocked, saw %v", seenEvents)
	}
	if !contains(seenEvents, "tool_input_rejected") {
		t.Errorf("missing tool_input_rejected, saw %v", seenEvents)
	}
}

// TestSecurityLogger_SetEventCounterIsRaceFree exercises the race detector
// against concurrent SetEventCounter and Emit calls. Without the lock around
// the SetEventCounter write, `go test -race` flags this as a data race
// because Emit reads sl.counter under sl.mu while SetEventCounter writes it
// without holding the lock.
func TestSecurityLogger_SetEventCounterIsRaceFree(t *testing.T) {
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

// TestSecurityLogger_GuardEventHelpers asserts each guard-flavoured
// SecurityLogger helper emits the expected event name and structured
// data fields. Without coverage here, a refactor that silently
// renamed an event or dropped a field (e.g. `criterion` on
// guard_denied) would erode the operator-visible contract while the
// build still passes.
func TestSecurityLogger_GuardEventHelpers(t *testing.T) {
	cases := []struct {
		name      string
		fire      func(sl *SecurityLogger)
		event     string
		wantField map[string]any
	}{
		{
			name:  "guard_allowed",
			fire:  func(sl *SecurityLogger) { sl.GuardAllowed("pre_turn", "granite-guardian") },
			event: "guard_allowed",
			wantField: map[string]any{
				"phase":   "pre_turn",
				"guardId": "granite-guardian",
			},
		},
		{
			name:  "guard_denied carries criterion and reason",
			fire:  func(sl *SecurityLogger) { sl.GuardDenied("pre_tool", "granite-guardian", "harm", "rule violation") },
			event: "guard_denied",
			wantField: map[string]any{
				"phase":     "pre_tool",
				"guardId":   "granite-guardian",
				"criterion": "harm",
				"reason":    "rule violation",
			},
		},
		{
			name:  "guard_skipped",
			fire:  func(sl *SecurityLogger) { sl.GuardSkipped("pre_turn", "granite-guardian") },
			event: "guard_skipped",
			wantField: map[string]any{
				"phase":   "pre_turn",
				"guardId": "granite-guardian",
			},
		},
		{
			name:  "guard_spotlighted carries reason",
			fire:  func(sl *SecurityLogger) { sl.GuardSpotlighted("pre_turn", "granite-guardian", "low confidence") },
			event: "guard_spotlighted",
			wantField: map[string]any{
				"phase":   "pre_turn",
				"guardId": "granite-guardian",
				"reason":  "low confidence",
			},
		},
		{
			name:  "guard_error carries error string",
			fire:  func(sl *SecurityLogger) { sl.GuardError("post_turn", "granite-guardian", "connection refused") },
			event: "guard_error",
			wantField: map[string]any{
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
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
				t.Fatalf("unmarshal %q: %v\noutput=%q", tc.name, err, buf.String())
			}
			if got.Event != tc.event {
				t.Errorf("Event = %q, want %q", got.Event, tc.event)
			}
			for k, want := range tc.wantField {
				if got.Data[k] != want {
					t.Errorf("Data[%q] = %v, want %v", k, got.Data[k], want)
				}
			}
		})
	}
}

// Confirm that Emit still produces the expected JSON output regardless of
// whether the counter is wired.
func TestSecurityLogger_EmitJSONShapeWithCounter(t *testing.T) {
	var buf bytes.Buffer
	sl := NewSecurityLogger(&buf, "run-1")
	sl.SetEventCounter(&fakeCounter{})
	sl.SecretRedactedInOutput("anthropic_api_key", "transport.grpc.event.text")

	var got SecurityEvent
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Event != "secret_redacted_in_output" {
		t.Errorf("Event = %q, want secret_redacted_in_output", got.Event)
	}
	if got.Data["pattern"] != "anthropic_api_key" {
		t.Errorf("Data.pattern = %v, want anthropic_api_key", got.Data["pattern"])
	}
	if got.Data["location"] != "transport.grpc.event.text" {
		t.Errorf("Data.location = %v, want transport.grpc.event.text", got.Data["location"])
	}
}
