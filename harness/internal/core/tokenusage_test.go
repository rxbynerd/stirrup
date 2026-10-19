package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func TestStreamEventsToResult_MergesReportedUsage(t *testing.T) {
	full := types.StreamEvent{
		Type:             "message_complete",
		StopReason:       "end_turn",
		InputTokens:      1200,
		OutputTokens:     42,
		CacheReadTokens:  1000,
		CacheWriteTokens: 150,
		ReasoningTokens:  30,
	}
	cases := []struct {
		name   string
		events []types.StreamEvent
		want   types.TokenUsage
	}{
		{
			name:   "zero later event leaves counts",
			events: []types.StreamEvent{full, {Type: "message_complete"}},
			want:   types.TokenUsage{Input: 1200, Output: 42, CacheRead: 1000, CacheWrite: 150, Reasoning: 30},
		},
		{
			name: "later non-zero count overwrites",
			events: []types.StreamEvent{
				{Type: "message_complete", OutputTokens: 10},
				{Type: "message_complete", OutputTokens: 12},
			},
			want: types.TokenUsage{Output: 12},
		},
		{
			name: "later input snapshot replaces the earlier breakdown",
			events: []types.StreamEvent{
				{Type: "message_complete", InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50},
				{Type: "message_complete", InputTokens: 120, OutputTokens: 12},
			},
			want: types.TokenUsage{Input: 120, Output: 12},
		},
		{
			name:   "duplicate identical event does not double",
			events: []types.StreamEvent{full, full},
			want:   types.TokenUsage{Input: 1200, Output: 42, CacheRead: 1000, CacheWrite: 150, Reasoning: 30},
		},
		{
			name: "usage split across three events",
			events: []types.StreamEvent{
				{Type: "message_complete", StopReason: "tool_use"},
				{Type: "message_complete", InputTokens: 5000, CacheReadTokens: 3000, CacheWriteTokens: 1500},
				{Type: "message_complete", OutputTokens: 40, ReasoningTokens: 12},
			},
			want: types.TokenUsage{Input: 5000, Output: 40, CacheRead: 3000, CacheWrite: 1500, Reasoning: 12},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan types.StreamEvent, len(tc.events))
			for _, ev := range tc.events {
				ch <- ev
			}
			close(ch)
			result, err := streamEventsToResult(context.Background(), ch, transport.NewNullTransport(), true, ignoreEmitFailure, nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, result.Usage)
		})
	}
}

func TestTokenTracker_CheckBudgetSaturates(t *testing.T) {
	tt := &TokenTracker{}
	tt.RecordTurn(types.TokenUsage{Input: math.MaxInt - 5})
	tt.RecordTurn(types.TokenUsage{Input: 100, Output: 100})
	budget := 1000
	check := tt.CheckBudget(&budget)
	require.False(t, check.WithinBudget, "saturated total %+v must exceed the budget", tt.Tokens())
}

type usageObservation struct {
	turns   []types.TurnTrace
	run     *types.RunTrace
	logs    string
	spans   tracetest.SpanStubs
	metrics map[string]int64
}

// usageTraceEmitter records every TurnTrace and forwards the run to a
// JSONL emitter, so a test sees per-turn values and the run aggregate.
type usageTraceEmitter struct {
	recordingTraceEmitter
	agg trace.TraceEmitter
}

func (e *usageTraceEmitter) Start(runID string, config *types.RunConfig) {
	e.agg.Start(runID, config)
}

func (e *usageTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.recordingTraceEmitter.RecordTurn(turn)
	e.agg.RecordTurn(turn)
}

func (e *usageTraceEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	return e.agg.Finish(ctx, outcome)
}

func runWithUsageObservers(t *testing.T, prov provider.ProviderAdapter, config *types.RunConfig) usageObservation {
	t.Helper()
	loop := buildTestLoop(nil)
	loop.Provider = prov
	rec := &usageTraceEmitter{agg: trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false)}
	loop.Trace = rec

	var logBuf bytes.Buffer
	loop.Logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	spans := tracetest.NewInMemoryExporter()
	loop.Tracer = sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans)).Tracer("test")

	reader := sdkmetric.NewManualReader()
	metrics, err := observability.NewMetricsForTesting(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	require.NoError(t, err)
	loop.Metrics = metrics

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
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

func turnCompletedLogs(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"msg":"turn completed"`) {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "decode log line %q", line)
		out = append(out, rec)
	}
	require.NotEmpty(t, out, "no turn completed log line in:\n%s", logs)
	return out
}

func providerSpanAttrs(t *testing.T, spans tracetest.SpanStubs) []map[attribute.Key]attribute.Value {
	t.Helper()
	var out []map[attribute.Key]attribute.Value
	for _, s := range spans {
		if s.Name != "provider.stream" {
			continue
		}
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes {
			attrs[kv.Key] = kv.Value
		}
		out = append(out, attrs)
	}
	require.NotEmpty(t, out, "no provider.stream span recorded")
	return out
}

func TestLoop_ProviderReportedUsageReplacesEstimate(t *testing.T) {
	obs := runWithUsageObservers(t, &mockProvider{events: []types.StreamEvent{
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
	}}, buildTestConfig())

	want := types.TokenUsage{Input: 5000, Output: 40, CacheRead: 3000, CacheWrite: 1500, Reasoning: 12}
	require.Len(t, obs.turns, 1)
	turn := obs.turns[0]

	t.Run("turn trace", func(t *testing.T) {
		require.Equal(t, want, turn.Tokens)
		require.True(t, turn.InputReported, "TurnTrace.InputReported")
	})

	t.Run("run trace", func(t *testing.T) {
		require.Equal(t, want, obs.run.TokenUsage)
	})

	logRec := turnCompletedLogs(t, obs.logs)[0]
	logWant := map[string]any{
		"tokens.input":          float64(5000),
		"tokens.input_reported": true,
		"tokens.output":         float64(40),
		"tokens.cache_read":     float64(3000),
		"tokens.cache_write":    float64(1500),
		"tokens.reasoning":      float64(12),
	}
	for key, wantVal := range logWant {
		t.Run("log "+key, func(t *testing.T) {
			require.Equal(t, wantVal, logRec[key])
		})
	}

	attrs := providerSpanAttrs(t, obs.spans)[0]
	t.Run("span input_reported", func(t *testing.T) {
		require.True(t, attrs["tokens.input_reported"].AsBool())
	})
	spanWant := map[attribute.Key]int64{
		"tokens.input":       5000,
		"tokens.cache_read":  3000,
		"tokens.cache_write": 1500,
		"tokens.reasoning":   12,
	}
	for key, wantVal := range spanWant {
		t.Run("span "+string(key), func(t *testing.T) {
			require.Equal(t, wantVal, attrs[key].AsInt64())
		})
	}

	metricWant := map[string]int64{
		"stirrup.harness.tokens.input":       5000,
		"stirrup.harness.tokens.output":      40,
		"stirrup.harness.tokens.cache_read":  3000,
		"stirrup.harness.tokens.cache_write": 1500,
	}
	for name, wantVal := range metricWant {
		t.Run("metric "+name, func(t *testing.T) {
			require.Equal(t, wantVal, obs.metrics[name])
		})
	}
}

// TestLoop_UnreportedUsageFallsBackToEstimate pins the estimate path for
// adapters that report no input figure.
func TestLoop_UnreportedUsageFallsBackToEstimate(t *testing.T) {
	obs := runWithUsageObservers(t, &mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn", OutputTokens: 7},
	}}, buildTestConfig())

	require.Len(t, obs.turns, 1)
	turn := obs.turns[0]

	t.Run("turn trace", func(t *testing.T) {
		require.False(t, turn.InputReported, "TurnTrace.InputReported")
		require.Positive(t, turn.Tokens.Input, "estimated input tokens")
		require.Zero(t, turn.Tokens.CacheRead, "unreported breakdown: cache read")
		require.Zero(t, turn.Tokens.CacheWrite, "unreported breakdown: cache write")
		require.Zero(t, turn.Tokens.Reasoning, "unreported breakdown: reasoning")
	})

	t.Run("metrics", func(t *testing.T) {
		require.Equal(t, int64(turn.Tokens.Input), obs.metrics["stirrup.harness.tokens.input"], "estimated input")
		for _, name := range []string{"stirrup.harness.tokens.cache_read", "stirrup.harness.tokens.cache_write"} {
			require.NotContains(t, obs.metrics, name, "no data point on the estimate path")
		}
	})

	t.Run("turn completed log", func(t *testing.T) {
		logRec := turnCompletedLogs(t, obs.logs)[0]
		require.Equal(t, false, logRec["tokens.input_reported"])
	})

	t.Run("provider span", func(t *testing.T) {
		attrs := providerSpanAttrs(t, obs.spans)[0]
		require.False(t, attrs["tokens.input_reported"].AsBool(), "tokens.input_reported")
		for _, key := range []attribute.Key{"tokens.cache_read", "tokens.cache_write", "tokens.reasoning"} {
			require.NotContains(t, attrs, key, "breakdown absent on the estimate path")
		}
	})
}

func TestLoop_MixedReportedAndEstimatedTurns(t *testing.T) {
	obs := runWithUsageObservers(t, &multiCallProvider{calls: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use", InputTokens: 5000, OutputTokens: 40, CacheReadTokens: 3000},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn", OutputTokens: 7},
		},
	}}, buildTestConfig())

	require.Len(t, obs.turns, 2)
	t.Run("input reporting per turn", func(t *testing.T) {
		require.True(t, obs.turns[0].InputReported, "turn 0 reports input")
		require.False(t, obs.turns[1].InputReported, "turn 1 estimates input")
		require.Equal(t, 5000, obs.turns[0].Tokens.Input, "turn 0 input is the reported value")
	})
	estimate := obs.turns[1].Tokens.Input
	require.Positive(t, estimate, "turn 1 input estimate")

	wantRun := types.TokenUsage{Input: 5000 + estimate, Output: 47, CacheRead: 3000}
	require.Equal(t, wantRun, obs.run.TokenUsage)

	spans := providerSpanAttrs(t, obs.spans)
	require.Len(t, spans, 2)
	t.Run("turn 1 provider span", func(t *testing.T) {
		require.Equal(t, int64(estimate), spans[1]["tokens.input"].AsInt64())
		require.False(t, spans[1]["tokens.input_reported"].AsBool(), "turn 1 tokens.input_reported")
	})

	logs := turnCompletedLogs(t, obs.logs)
	require.Len(t, logs, 2)
	require.InDelta(t, float64(estimate), logs[1]["tokens.input"], 0, "turn 1 log tokens.input")

	t.Run("metrics", func(t *testing.T) {
		require.Equal(t, int64(5000+estimate), obs.metrics["stirrup.harness.tokens.input"], "reported turn plus estimate")
		require.Equal(t, int64(3000), obs.metrics["stirrup.harness.tokens.cache_read"], "reported turn only")
	})
}

// The provider's input already includes its cache reads, so the budget
// counts 1000 + 50 tokens, not 1000 + 800 + 50.
func TestLoop_BudgetCountsCachedInputOnce(t *testing.T) {
	config := buildTestConfig()
	budget := 1000
	config.MaxTokenBudget = &budget

	obs := runWithUsageObservers(t, &multiCallProvider{calls: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use", InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 800},
		},
	}}, config)

	require.Equal(t, "budget_exceeded", obs.run.Outcome)
	want := types.TokenUsage{Input: 1000, Output: 50, CacheRead: 800}
	require.Equal(t, want, obs.run.TokenUsage)
}

func TestTokenTracker_CachedInputIsNotCountedTwice(t *testing.T) {
	tt := &TokenTracker{}
	tt.RecordTurn(types.TokenUsage{Input: 1000, Output: 50, CacheRead: 800})
	require.Equal(t, 1000, tt.Tokens().Input)

	within := 1050
	check := tt.CheckBudget(&within)
	require.True(t, check.WithinBudget, "1000 input + 50 output against a budget of 1050")

	below := 1049
	check = tt.CheckBudget(&below)
	require.False(t, check.WithinBudget, "1000 input + 50 output against a budget of 1049")
}

// An openai-compatible server that ignores stream_options.include_usage
// sends no usage; the turn's input falls back to the estimate.
func TestLoop_ChatServerWithoutUsageFallsBackToEstimate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+`{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	bearer := func(context.Context) (string, error) { return "test-key", nil }
	adapter := provider.NewOpenAICompatibleAdapter(bearer, srv.URL, provider.OpenAIAuthConfig{}, provider.RetryPolicy{})

	obs := runWithUsageObservers(t, adapter, buildTestConfig())
	require.Len(t, obs.turns, 1)
	turn := obs.turns[0]
	require.Equal(t, "end_turn", turn.StopReason, "stop reason from the fake server")
	require.False(t, turn.InputReported, "TurnTrace.InputReported")
	require.Positive(t, turn.Tokens.Input, "estimated input tokens")
}
