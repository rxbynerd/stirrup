package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/types"
)

// endpointProvider reports a fixed server endpoint on top of a scripted
// provider, as the HTTP adapters do.
type endpointProvider struct {
	*scriptedTurnProvider
}

func (endpointProvider) ServerEndpoint() (string, int) { return "api.anthropic.com", 443 }

// findHistogram returns the named histogram's points, and false when the
// instrument recorded nothing.
func findHistogram[N int64 | float64](t *testing.T, reader sdkmetric.Reader, name string) ([]metricdata.HistogramDataPoint[N], bool) {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm), "collect metrics")
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[N])
			must.True(ok, "%s data type = %T", name, m.Data)
			return hist.DataPoints, true
		}
	}
	return nil, false
}

func collectHistogram[N int64 | float64](t *testing.T, h *retryHarness, name string) []metricdata.HistogramDataPoint[N] {
	t.Helper()
	must := require.New(t)
	points, ok := findHistogram[N](t, h.reader, name)
	must.True(ok, "metric %q not collected", name)
	return points
}

func attrValue(set attribute.Set, key string) (attribute.Value, bool) {
	return set.Value(attribute.Key(key))
}

// assertStringAttr checks a string attribute on a data point. A missing
// attribute reads as the empty string.
func assertStringAttr(t *testing.T, set attribute.Set, key, want string) {
	t.Helper()
	check := assert.New(t)
	got, _ := attrValue(set, key)
	check.Equal(want, got.AsString(), "attribute %s", key)
}

func TestLoop_GenAIClientMetrics_RecordPerAttemptAndPerTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const responseModel = "claude-sonnet-4-6-20260101"
	toolTurn := turnScript{events: []types.StreamEvent{
		{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use", InputTokens: 50, OutputTokens: 5, ResponseModel: responseModel},
	}}
	failedWithUsage := turnScript{events: []types.StreamEvent{
		{Type: "message_complete", InputTokens: 200, OutputTokens: 20},
		{Type: "error", Error: types.MarkRetryable(errors.New("overloaded"))},
	}}
	answerTurn := turnScript{events: []types.StreamEvent{
		{Type: "text_delta", Text: "All done."},
		{Type: "message_complete", StopReason: "end_turn", InputTokens: 100, OutputTokens: 10, ResponseModel: responseModel},
	}}
	prov := endpointProvider{&scriptedTurnProvider{scripts: []turnScript{toolTurn, failedWithUsage, answerTurn}}}
	h := newRetryHarness(t, prov)

	_, err := h.loop.Run(context.Background(), retryConfig(3))
	must.NoError(err, "Run()")

	// Three provider attempts: two successful streams and one failed
	// mid-stream attempt that the turn retry discarded.
	var ok, failed uint64
	for _, dp := range collectHistogram[float64](t, h, "gen_ai.client.operation.duration") {
		errType, failedPoint := attrValue(dp.Attributes, "error.type")
		if failedPoint {
			check.Equal("provider_stream_failed", errType.AsString(), "attribute error.type")
			failed += dp.Count
		} else {
			ok += dp.Count
		}

		wantResponse := responseModel
		if failedPoint {
			wantResponse = ""
		}
		assertStringAttr(t, dp.Attributes, "gen_ai.response.model", wantResponse)
		assertStringAttr(t, dp.Attributes, "gen_ai.provider.name", "anthropic")
		assertStringAttr(t, dp.Attributes, "gen_ai.request.model", "claude-sonnet-4-6")
		assertStringAttr(t, dp.Attributes, "server.address", "api.anthropic.com")
		port, _ := attrValue(dp.Attributes, "server.port")
		check.Equal(int64(443), port.AsInt64(), "attribute server.port")
		for _, banned := range []string{"run.id", "run.parent_id", "run.subagent", "tool.name", "gen_ai.conversation.id"} {
			_, has := attrValue(dp.Attributes, banned)
			check.False(has, "operation.duration carries high-cardinality attribute %q", banned)
		}
	}
	check.Equal(uint64(2), ok, "successful operation.duration points")
	check.Equal(uint64(1), failed, "failed operation.duration points")

	// Each attempt records the usage it reported: the retried attempt's
	// usage stays a point of its own rather than folding into the
	// successful one.
	count := map[string]uint64{}
	sum := map[string]int64{}
	for _, dp := range collectHistogram[int64](t, h, "gen_ai.client.token.usage") {
		typ, _ := attrValue(dp.Attributes, "gen_ai.token.type")
		count[typ.AsString()] += dp.Count
		sum[typ.AsString()] += dp.Sum
		wantResponse := responseModel
		if dp.Sum == 200 || dp.Sum == 20 { // the failed attempt's usage
			wantResponse = ""
		}
		model, _ := attrValue(dp.Attributes, "gen_ai.response.model")
		check.Equal(wantResponse, model.AsString(), "attribute gen_ai.response.model of token usage point with sum %d", dp.Sum)
	}
	check.Equal(uint64(3), count["input"], "input token usage points")
	check.Equal(uint64(3), count["output"], "output token usage points")
	check.Equal(int64(350), sum["input"], "input token usage sum")
	check.Equal(int64(35), sum["output"], "output token usage sum")
}

func TestLoop_GenAITokenUsage_OmitsUnreportedCounts(t *testing.T) {
	must := require.New(t)
	noUsage := turnScript{events: []types.StreamEvent{
		{Type: "text_delta", Text: "All done."},
		{Type: "message_complete", StopReason: "end_turn"},
	}}
	h := newRetryHarness(t, &scriptedTurnProvider{scripts: []turnScript{noUsage}})
	_, err := h.loop.Run(context.Background(), buildTestConfig())
	must.NoError(err, "Run()")

	points, found := findHistogram[int64](t, h.reader, "gen_ai.client.token.usage")
	must.False(found, "token usage recorded %d points for a response that reported no usage", len(points))
}

func TestLoop_GenAIOperationDuration_NonRetryableFailure(t *testing.T) {
	must := require.New(t)
	h := newRetryHarness(t, &scriptedTurnProvider{scripts: []turnScript{{err: errors.New("bad request")}}})
	_, _ = h.loop.Run(context.Background(), buildTestConfig())

	points := collectHistogram[float64](t, h, "gen_ai.client.operation.duration")
	must.Len(points, 1, "operation.duration points %+v", points)
	must.Equal(uint64(1), points[0].Count, "operation.duration count")

	errType, _ := attrValue(points[0].Attributes, "error.type")
	must.Equal("provider_request_failed", errType.AsString(), "attribute error.type")

	_, found := findHistogram[int64](t, h.reader, "gen_ai.client.token.usage")
	must.False(found, "token usage recorded for a request that failed before streaming")
}

func TestLoop_TokenCounters_CarryProviderAndModel(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	h := newRetryHarness(t, &scriptedTurnProvider{scripts: []turnScript{answerScript()}})
	_, err := h.loop.Run(context.Background(), buildTestConfig())
	must.NoError(err, "Run()")

	var rm metricdata.ResourceMetrics
	must.NoError(h.reader.Collect(context.Background(), &rm), "collect metrics")
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.tokens.input" && m.Name != "stirrup.harness.tokens.output" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				provider, _ := attrValue(dp.Attributes, "gen_ai.provider.name")
				model, _ := attrValue(dp.Attributes, "gen_ai.request.model")
				check.Equal("anthropic", provider.AsString(), "%s attribute gen_ai.provider.name", m.Name)
				check.Equal("claude-sonnet-4-6", model.AsString(), "%s attribute gen_ai.request.model", m.Name)
				seen[m.Name] = true
			}
		}
	}
	check.Len(seen, 2, "token counters observed %v, want input and output", seen)
}
