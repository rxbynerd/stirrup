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
			result, err := streamEventsToResult(context.Background(), ch, transport.NewNullTransport(), slog.Default())
			if err != nil {
				t.Fatalf("streamEventsToResult() error: %v", err)
			}
			if result.Usage != tc.want {
				t.Errorf("Usage = %+v, want %+v", result.Usage, tc.want)
			}
		})
	}
}

func TestTokenTracker_CheckBudgetSaturates(t *testing.T) {
	tt := &TokenTracker{}
	tt.RecordTurn(types.TokenUsage{Input: math.MaxInt - 5})
	tt.RecordTurn(types.TokenUsage{Input: 100, Output: 100})
	budget := 1000
	if check := tt.CheckBudget(&budget); check.WithinBudget {
		t.Errorf("CheckBudget = within budget for a saturated total %+v, want exceeded", tt.Tokens())
	}
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
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	loop.Metrics = metrics

	runTrace, err := loop.Run(context.Background(), config)
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

func turnCompletedLogs(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"msg":"turn completed"`) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	if len(out) == 0 {
		t.Fatalf("no turn completed log line in:\n%s", logs)
	}
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
	if len(out) == 0 {
		t.Fatal("no provider.stream span recorded")
	}
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
	if len(obs.turns) != 1 {
		t.Fatalf("recorded %d turns, want 1", len(obs.turns))
	}
	if obs.turns[0].Tokens != want {
		t.Errorf("TurnTrace.Tokens = %+v, want %+v", obs.turns[0].Tokens, want)
	}
	if !obs.turns[0].InputReported {
		t.Error("TurnTrace.InputReported = false, want true for a provider-reported input")
	}
	if obs.run.TokenUsage != want {
		t.Errorf("RunTrace.TokenUsage = %+v, want %+v", obs.run.TokenUsage, want)
	}

	logRec := turnCompletedLogs(t, obs.logs)[0]
	for key, wantVal := range map[string]any{
		"tokens.input":          float64(5000),
		"tokens.input_reported": true,
		"tokens.output":         float64(40),
		"tokens.cache_read":     float64(3000),
		"tokens.cache_write":    float64(1500),
		"tokens.reasoning":      float64(12),
	} {
		if logRec[key] != wantVal {
			t.Errorf("turn completed log %s = %v, want %v", key, logRec[key], wantVal)
		}
	}

	attrs := providerSpanAttrs(t, obs.spans)[0]
	if got := attrs["tokens.input_reported"].AsBool(); !got {
		t.Error("provider.stream tokens.input_reported = false, want true")
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
// adapters that report no input figure.
func TestLoop_UnreportedUsageFallsBackToEstimate(t *testing.T) {
	obs := runWithUsageObservers(t, &mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn", OutputTokens: 7},
	}}, buildTestConfig())

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
	for _, name := range []string{"stirrup.harness.tokens.cache_read", "stirrup.harness.tokens.cache_write"} {
		if got, ok := obs.metrics[name]; ok {
			t.Errorf("metric %s recorded %d, want no data point on the estimate path", name, got)
		}
	}
	if logRec := turnCompletedLogs(t, obs.logs)[0]; logRec["tokens.input_reported"] != false {
		t.Errorf("turn completed log tokens.input_reported = %v, want false", logRec["tokens.input_reported"])
	}
	attrs := providerSpanAttrs(t, obs.spans)[0]
	if got := attrs["tokens.input_reported"].AsBool(); got {
		t.Error("provider.stream tokens.input_reported = true, want false")
	}
	for _, key := range []attribute.Key{"tokens.cache_read", "tokens.cache_write", "tokens.reasoning"} {
		if v, ok := attrs[key]; ok {
			t.Errorf("provider.stream %s = %s, want absent on the estimate path", key, v.String())
		}
	}
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

	if len(obs.turns) != 2 {
		t.Fatalf("recorded %d turns, want 2", len(obs.turns))
	}
	if !obs.turns[0].InputReported || obs.turns[1].InputReported {
		t.Errorf("InputReported = %v, %v; want true, false", obs.turns[0].InputReported, obs.turns[1].InputReported)
	}
	if obs.turns[0].Tokens.Input != 5000 {
		t.Errorf("turn 0 input = %d, want the reported 5000", obs.turns[0].Tokens.Input)
	}
	estimate := obs.turns[1].Tokens.Input
	if estimate <= 0 {
		t.Fatalf("turn 1 input = %d, want a positive estimate", estimate)
	}

	wantRun := types.TokenUsage{Input: 5000 + estimate, Output: 47, CacheRead: 3000}
	if obs.run.TokenUsage != wantRun {
		t.Errorf("RunTrace.TokenUsage = %+v, want %+v", obs.run.TokenUsage, wantRun)
	}

	spans := providerSpanAttrs(t, obs.spans)
	if len(spans) != 2 {
		t.Fatalf("provider.stream spans = %d, want 2", len(spans))
	}
	if got := spans[1]["tokens.input"].AsInt64(); got != int64(estimate) {
		t.Errorf("turn 1 provider.stream tokens.input = %d, want the estimate %d", got, estimate)
	}
	if spans[1]["tokens.input_reported"].AsBool() {
		t.Error("turn 1 provider.stream tokens.input_reported = true, want false")
	}
	logs := turnCompletedLogs(t, obs.logs)
	if len(logs) != 2 {
		t.Fatalf("turn completed logs = %d, want 2", len(logs))
	}
	if logs[1]["tokens.input"] != float64(estimate) {
		t.Errorf("turn 1 log tokens.input = %v, want the estimate %d", logs[1]["tokens.input"], estimate)
	}

	if got := obs.metrics["stirrup.harness.tokens.input"]; got != int64(5000+estimate) {
		t.Errorf("metric tokens.input = %d, want %d (reported turn plus estimate)", got, 5000+estimate)
	}
	if got := obs.metrics["stirrup.harness.tokens.cache_read"]; got != 3000 {
		t.Errorf("metric tokens.cache_read = %d, want 3000 from the reported turn only", got)
	}
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

	if obs.run.Outcome != "budget_exceeded" {
		t.Errorf("Outcome = %q, want budget_exceeded", obs.run.Outcome)
	}
	want := types.TokenUsage{Input: 1000, Output: 50, CacheRead: 800}
	if obs.run.TokenUsage != want {
		t.Errorf("RunTrace.TokenUsage = %+v, want %+v", obs.run.TokenUsage, want)
	}
}

func TestTokenTracker_CachedInputIsNotCountedTwice(t *testing.T) {
	tt := &TokenTracker{}
	tt.RecordTurn(types.TokenUsage{Input: 1000, Output: 50, CacheRead: 800})
	if got := tt.Tokens().Input; got != 1000 {
		t.Errorf("Tokens().Input = %d, want 1000", got)
	}
	within := 1050
	if check := tt.CheckBudget(&within); !check.WithinBudget {
		t.Error("CheckBudget(1050) exceeded for 1000 input + 50 output")
	}
	below := 1049
	if check := tt.CheckBudget(&below); check.WithinBudget {
		t.Error("CheckBudget(1049) within budget for 1000 input + 50 output")
	}
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
	if len(obs.turns) != 1 {
		t.Fatalf("recorded %d turns, want 1", len(obs.turns))
	}
	if obs.turns[0].StopReason != "end_turn" {
		t.Fatalf("StopReason = %q, want end_turn from the fake server", obs.turns[0].StopReason)
	}
	if obs.turns[0].InputReported {
		t.Error("InputReported = true, want false for a stream without usage")
	}
	if obs.turns[0].Tokens.Input <= 0 {
		t.Errorf("Tokens.Input = %d, want a positive estimate", obs.turns[0].Tokens.Input)
	}
}
