package core

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func newMeteredTestProvider(t *testing.T, inner *scriptedTurnProvider) (*meteredProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	m := newMeteredProvider(endpointProvider{inner}, "anthropic")
	m.metrics = metrics
	return m, reader
}

func drain(ch <-chan types.StreamEvent) []types.StreamEvent {
	var events []types.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func TestMeteredProvider_RecordsDurationAndTokens(t *testing.T) {
	script := turnScript{events: []types.StreamEvent{
		{Type: "text_delta", Text: "ok"},
		{Type: "message_complete", StopReason: "end_turn", InputTokens: 120, OutputTokens: 8, ResponseModel: "claude-haiku-4-5-20251001"},
	}}
	m, reader := newMeteredTestProvider(t, &scriptedTurnProvider{scripts: []turnScript{script}})

	ch, err := m.Stream(context.Background(), types.StreamParams{Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	if got := drain(ch); len(got) != 2 {
		t.Fatalf("forwarded %d events, want 2", len(got))
	}

	durations, _ := findHistogram[float64](t, reader, "gen_ai.client.operation.duration")
	if len(durations) != 1 || durations[0].Count != 1 {
		t.Fatalf("duration points = %+v, want exactly one", durations)
	}
	for key, want := range map[string]string{
		"gen_ai.provider.name":  "anthropic",
		"gen_ai.request.model":  "claude-haiku-4-5",
		"gen_ai.response.model": "claude-haiku-4-5-20251001",
		"server.address":        "api.anthropic.com",
	} {
		if v, _ := attrValue(durations[0].Attributes, key); v.AsString() != want {
			t.Errorf("duration %s = %q, want %q", key, v.AsString(), want)
		}
	}
	if _, has := attrValue(durations[0].Attributes, "error.type"); has {
		t.Error("successful call carries error.type")
	}

	sums := map[string]int64{}
	tokens, _ := findHistogram[int64](t, reader, "gen_ai.client.token.usage")
	for _, dp := range tokens {
		typ, _ := attrValue(dp.Attributes, "gen_ai.token.type")
		sums[typ.AsString()] += dp.Sum
	}
	if sums["input"] != 120 || sums["output"] != 8 {
		t.Errorf("token sums = %v, want input=120 output=8", sums)
	}
}

func TestMeteredProvider_RecordsFailedRequest(t *testing.T) {
	m, reader := newMeteredTestProvider(t, &scriptedTurnProvider{scripts: []turnScript{{err: errors.New("refused")}}})

	if _, err := m.Stream(context.Background(), types.StreamParams{Model: "claude-haiku-4-5"}); err == nil {
		t.Fatal("Stream() error = nil, want the provider error")
	}

	durations, _ := findHistogram[float64](t, reader, "gen_ai.client.operation.duration")
	if len(durations) != 1 {
		t.Fatalf("duration points = %d, want 1", len(durations))
	}
	if v, _ := attrValue(durations[0].Attributes, "error.type"); v.AsString() != "provider_request_failed" {
		t.Errorf("error.type = %q, want provider_request_failed", v.AsString())
	}
}

func TestMeteredProvider_RecordsStreamError(t *testing.T) {
	script := turnScript{events: []types.StreamEvent{
		{Type: "error", Error: errors.New("broken stream")},
	}}
	m, reader := newMeteredTestProvider(t, &scriptedTurnProvider{scripts: []turnScript{script}})

	ch, err := m.Stream(context.Background(), types.StreamParams{Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	drain(ch)

	durations, _ := findHistogram[float64](t, reader, "gen_ai.client.operation.duration")
	if len(durations) != 1 {
		t.Fatalf("duration points = %d, want 1", len(durations))
	}
	if v, _ := attrValue(durations[0].Attributes, "error.type"); v.AsString() != "provider_stream_failed" {
		t.Errorf("error.type = %q, want provider_stream_failed", v.AsString())
	}
	if _, ok := findHistogram[int64](t, reader, "gen_ai.client.token.usage"); ok {
		t.Error("token usage recorded for a stream that reported none")
	}
}

func TestMeteredProvider_PassesThroughWithoutMetrics(t *testing.T) {
	script := turnScript{events: []types.StreamEvent{{Type: "message_complete", StopReason: "end_turn"}}}
	m := newMeteredProvider(&scriptedTurnProvider{scripts: []turnScript{script}}, "anthropic")

	ch, err := m.Stream(context.Background(), types.StreamParams{Model: "m"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	if got := drain(ch); len(got) != 1 {
		t.Errorf("forwarded %d events, want 1", len(got))
	}
}
