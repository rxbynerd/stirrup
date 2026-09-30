package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func TestStreamEventsToResult_MergesReportedUsage(t *testing.T) {
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{
		Type:             "message_complete",
		StopReason:       "end_turn",
		InputTokens:      1200,
		CacheReadTokens:  1000,
		CacheWriteTokens: 150,
		ReasoningTokens:  30,
	}
	// A trailing usage-only event must not clear counts reported earlier.
	ch <- types.StreamEvent{Type: "message_complete", OutputTokens: 42}
	close(ch)

	result, err := streamEventsToResult(context.Background(), ch, transport.NewNullTransport(), slog.Default())
	if err != nil {
		t.Fatalf("streamEventsToResult() error: %v", err)
	}
	want := types.TokenUsage{Input: 1200, Output: 42, CacheRead: 1000, CacheWrite: 150, Reasoning: 30}
	if result.Usage != want {
		t.Errorf("Usage = %+v, want %+v", result.Usage, want)
	}
}

type usageObservation struct {
	turns   []types.TurnTrace
	run     *types.RunTrace
	logs    string
	spans   tracetest.SpanStubs
	metrics map[string]int64
}

func runWithUsageObservers(t *testing.T, events []types.StreamEvent) usageObservation {
	t.Helper()
	loop := buildTestLoop(&mockProvider{events: events})
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	var logBuf bytes.Buffer
	loop.Logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	spans := tracetest.NewInMemoryExporter()
	loop.Tracer = sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans)).Tracer("test")

	reader := sdkmetric.NewManualReader()
	metrics, err := observability.NewMetricsForTesting(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	loop.Metrics = metrics

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	sums := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					sums[m.Name] += dp.Value
				}
			}
		}
	}

	turns, _ := rec.snapshot()
	return usageObservation{turns: turns, run: runTrace, logs: logBuf.String(), spans: spans.GetSpans(), metrics: sums}
}

func turnCompletedLog(t *testing.T, logs string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"msg":"turn completed"`) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		return rec
	}
	t.Fatalf("no turn completed log line in:\n%s", logs)
	return nil
}

func providerSpanAttrs(t *testing.T, spans tracetest.SpanStubs) map[attribute.Key]attribute.Value {
	t.Helper()
	for _, s := range spans {
		if s.Name != "provider.stream" {
			continue
		}
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes {
			attrs[kv.Key] = kv.Value
		}
		return attrs
	}
	t.Fatal("no provider.stream span recorded")
	return nil
}

func TestLoop_ProviderReportedUsageReplacesEstimate(t *testing.T) {
	obs := runWithUsageObservers(t, []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{
			Type:             "message_complete",
			StopReason:       "end_turn",
			InputTokens:      5000,
			OutputTokens:     40,
			CacheReadTokens:  3000,
			CacheWriteTokens: 1500,
			ReasoningTokens:  12,
		},
	})

	want := types.TokenUsage{Input: 5000, Output: 40, CacheRead: 3000, CacheWrite: 1500, Reasoning: 12}
	if len(obs.turns) != 1 {
		t.Fatalf("recorded %d turns, want 1", len(obs.turns))
	}
	if obs.turns[0].Tokens != want {
		t.Errorf("TurnTrace.Tokens = %+v, want %+v", obs.turns[0].Tokens, want)
	}
	if !obs.turns[0].InputReported {
		t.Error("TurnTrace.InputReported = false, want true for a provider-reported input")
	}

	logRec := turnCompletedLog(t, obs.logs)
	for key, wantVal := range map[string]any{
		"tokens.input":          float64(5000),
		"tokens.input.reported": true,
		"tokens.output":         float64(40),
		"tokens.cache_read":     float64(3000),
		"tokens.cache_write":    float64(1500),
		"tokens.reasoning":      float64(12),
	} {
		if logRec[key] != wantVal {
			t.Errorf("turn completed log %s = %v, want %v", key, logRec[key], wantVal)
		}
	}

	attrs := providerSpanAttrs(t, obs.spans)
	if got := attrs["tokens.input.reported"].AsBool(); !got {
		t.Error("provider.stream tokens.input.reported = false, want true")
	}
	for key, wantVal := range map[attribute.Key]int64{
		"tokens.input":       5000,
		"tokens.cache_read":  3000,
		"tokens.cache_write": 1500,
		"tokens.reasoning":   12,
	} {
		if got := attrs[key].AsInt64(); got != wantVal {
			t.Errorf("provider.stream %s = %d, want %d", key, got, wantVal)
		}
	}

	for name, wantVal := range map[string]int64{
		"stirrup.harness.tokens.input":       5000,
		"stirrup.harness.tokens.output":      40,
		"stirrup.harness.tokens.cache_read":  3000,
		"stirrup.harness.tokens.cache_write": 1500,
	} {
		if got := obs.metrics[name]; got != wantVal {
			t.Errorf("metric %s = %d, want %d", name, got, wantVal)
		}
	}
}

// TestLoop_UnreportedUsageFallsBackToEstimate pins the estimate path for
// adapters that report no input figure (e.g. the eval ReplayProvider).
func TestLoop_UnreportedUsageFallsBackToEstimate(t *testing.T) {
	obs := runWithUsageObservers(t, []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn", OutputTokens: 7},
	})

	if len(obs.turns) != 1 {
		t.Fatalf("recorded %d turns, want 1", len(obs.turns))
	}
	turn := obs.turns[0]
	if turn.InputReported {
		t.Error("TurnTrace.InputReported = true, want false when the provider reports no input")
	}
	if turn.Tokens.Input <= 0 {
		t.Errorf("TurnTrace.Tokens.Input = %d, want a positive estimate", turn.Tokens.Input)
	}
	if turn.Tokens.CacheRead != 0 || turn.Tokens.CacheWrite != 0 || turn.Tokens.Reasoning != 0 {
		t.Errorf("unreported breakdown must stay zero, got %+v", turn.Tokens)
	}
	if got := obs.metrics["stirrup.harness.tokens.input"]; got != int64(turn.Tokens.Input) {
		t.Errorf("metric tokens.input = %d, want the estimate %d", got, turn.Tokens.Input)
	}
	if logRec := turnCompletedLog(t, obs.logs); logRec["tokens.input.reported"] != false {
		t.Errorf("turn completed log tokens.input.reported = %v, want false", logRec["tokens.input.reported"])
	}
	if got := providerSpanAttrs(t, obs.spans)["tokens.input.reported"].AsBool(); got {
		t.Error("provider.stream tokens.input.reported = true, want false")
	}
}
