package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
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
	require.NoError(t, err)
	require.Len(t, drain(ch), 2)

	durations, _ := findHistogram[float64](t, reader, "gen_ai.client.operation.duration")
	require.Len(t, durations, 1)
	require.EqualValues(t, 1, durations[0].Count)

	for _, tc := range []struct {
		name, key, want string
	}{
		{name: "provider name", key: "gen_ai.provider.name", want: "anthropic"},
		{name: "request model", key: "gen_ai.request.model", want: "claude-haiku-4-5"},
		{name: "response model", key: "gen_ai.response.model", want: "claude-haiku-4-5-20251001"},
		{name: "server address", key: "server.address", want: "api.anthropic.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := attrValue(durations[0].Attributes, tc.key)
			require.Equal(t, tc.want, v.AsString())
		})
	}

	_, hasErrorType := attrValue(durations[0].Attributes, "error.type")
	require.False(t, hasErrorType, "successful call carries error.type")

	sums := map[string]int64{}
	tokens, _ := findHistogram[int64](t, reader, "gen_ai.client.token.usage")
	for _, dp := range tokens {
		typ, _ := attrValue(dp.Attributes, "gen_ai.token.type")
		sums[typ.AsString()] += dp.Sum
	}
	require.Equal(t, int64(120), sums["input"], "token sums: %v", sums)
	require.Equal(t, int64(8), sums["output"], "token sums: %v", sums)
}

func TestMeteredProvider_RecordsFailedRequest(t *testing.T) {
	m, reader := newMeteredTestProvider(t, &scriptedTurnProvider{scripts: []turnScript{{err: errors.New("refused")}}})

	_, err := m.Stream(context.Background(), types.StreamParams{Model: "claude-haiku-4-5"})
	require.Error(t, err, "Stream() should return the provider error")

	durations, _ := findHistogram[float64](t, reader, "gen_ai.client.operation.duration")
	require.Len(t, durations, 1)
	errorType, _ := attrValue(durations[0].Attributes, "error.type")
	require.Equal(t, "provider_request_failed", errorType.AsString())
}

func TestMeteredProvider_RecordsStreamError(t *testing.T) {
	script := turnScript{events: []types.StreamEvent{
		{Type: "error", Error: errors.New("broken stream")},
	}}
	m, reader := newMeteredTestProvider(t, &scriptedTurnProvider{scripts: []turnScript{script}})

	ch, err := m.Stream(context.Background(), types.StreamParams{Model: "claude-haiku-4-5"})
	require.NoError(t, err)
	drain(ch)

	durations, _ := findHistogram[float64](t, reader, "gen_ai.client.operation.duration")
	require.Len(t, durations, 1)
	errorType, _ := attrValue(durations[0].Attributes, "error.type")
	require.Equal(t, "provider_stream_failed", errorType.AsString())

	_, hasTokenUsage := findHistogram[int64](t, reader, "gen_ai.client.token.usage")
	require.False(t, hasTokenUsage, "token usage recorded for a stream that reported none")
}

func TestMeteredProvider_PassesThroughWithoutMetrics(t *testing.T) {
	script := turnScript{events: []types.StreamEvent{{Type: "message_complete", StopReason: "end_turn"}}}
	m := newMeteredProvider(&scriptedTurnProvider{scripts: []turnScript{script}}, "anthropic")

	ch, err := m.Stream(context.Background(), types.StreamParams{Model: "m"})
	require.NoError(t, err)
	require.Len(t, drain(ch), 1)
}
