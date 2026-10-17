package core

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// correlationFixture holds values distinctive enough that any appearance
// in a model request or metric label is a leak, not a coincidence.
func correlationFixture() *types.CorrelationConfig {
	return &types.CorrelationConfig{
		SessionID: "corr-session-5f1e",
		TaskID:    "corr-task-8a2b",
		AttemptID: "corr-attempt-3c4d",
		UserID:    "corr-user-9e7f",
		URL:       "https://cp.example.com/runs/corr-url-6b0a",
		Labels:    map[string]string{"agent_profile": "corr-label-2d9c"},
	}
}

func correlationValues(c *types.CorrelationConfig) []string {
	values := []string{c.SessionID, c.TaskID, c.AttemptID, c.UserID, c.URL}
	for _, v := range c.Labels {
		values = append(values, v)
	}
	return values
}

// recordingProvider records every request and scripts one tool call then
// a final answer, so tool-result and follow-up turns are exercised too.
type recordingProvider struct {
	mu     sync.Mutex
	params []types.StreamParams
}

func (p *recordingProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	turn := len(p.params)
	p.params = append(p.params, params)
	p.mu.Unlock()

	events := []types.StreamEvent{
		{Type: "text_delta", Text: "Done."},
		{Type: "message_complete", StopReason: "end_turn"},
	}
	if turn == 0 {
		events = []types.StreamEvent{
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		}
	}
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestLoop_CorrelationNeverReachesModelRequest(t *testing.T) {
	modes := make([]string, 0, len(prompt.ModePrompts()))
	for mode := range prompt.ModePrompts() {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	if len(modes) == 0 {
		t.Fatal("no mode prompts registered")
	}

	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			emitter := tracepkg.NewOTelTraceEmitterForTest(tp)

			prov := &recordingProvider{}
			loop := buildTestLoop(nil)
			loop.Provider = prov
			loop.Trace = emitter
			loop.Tracer = emitter.Tracer()

			cfg := buildTestConfig()
			cfg.Mode = mode
			cfg.SessionName = "corr-session-name-1a2b"
			cfg.Correlation = correlationFixture()

			if _, err := loop.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(prov.params) < 2 {
				t.Fatalf("expected a tool turn and a final turn, got %d requests", len(prov.params))
			}

			for i, params := range prov.params {
				raw, err := json.Marshal(params)
				if err != nil {
					t.Fatalf("marshal request %d: %v", i, err)
				}
				for _, v := range append(correlationValues(cfg.Correlation), cfg.SessionName) {
					if strings.Contains(string(raw), v) {
						t.Errorf("request %d carries correlation value %q", i, v)
					}
				}
			}

			stamped := false
			for _, s := range exporter.GetSpans() {
				for _, kv := range s.Attributes {
					if kv.Key == "user.id" && kv.Value.AsString() == cfg.Correlation.UserID {
						stamped = true
					}
				}
			}
			if !stamped {
				t.Error("no span carries user.id; the run did not see the correlation block")
			}
		})
	}
}

func TestLoop_CorrelationNeverBecomesMetricLabel(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	emitter := tracepkg.NewOTelTraceEmitterForTest(tp)

	loop := buildTestLoop(nil)
	loop.Provider = &recordingProvider{}
	loop.Trace = emitter
	loop.Tracer = emitter.Tracer()
	loop.Metrics = metrics

	cfg := buildTestConfig()
	cfg.SessionName = "corr-session-name-1a2b"
	cfg.Correlation = correlationFixture()
	if _, err := loop.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	values := map[string]bool{cfg.SessionName: true}
	for _, v := range correlationValues(cfg.Correlation) {
		values[v] = true
	}
	observed := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, set := range metricAttributeSets(m.Data) {
				observed++
				for _, kv := range set.ToSlice() {
					key := string(kv.Key)
					if isCorrelationKey(key) || values[kv.Value.String()] {
						t.Errorf("metric %s carries correlation attribute %s=%s", m.Name, key, kv.Value.String())
					}
				}
			}
		}
	}
	if observed == 0 {
		t.Fatal("the run recorded no metric data points")
	}
}

func isCorrelationKey(key string) bool {
	switch key {
	case "session.id", "gen_ai.conversation.id", "user.id",
		"stirrup.task.id", "stirrup.attempt.id", "stirrup.control_plane.url":
		return true
	}
	return strings.HasPrefix(key, "stirrup.label.")
}

func metricAttributeSets(data metricdata.Aggregation) []attribute.Set {
	var sets []attribute.Set
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	}
	return sets
}
