package core

import (
	"context"
	"errors"
	"testing"

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
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[N])
			if !ok {
				t.Fatalf("%s data = %T", name, m.Data)
			}
			return hist.DataPoints, true
		}
	}
	return nil, false
}

func collectHistogram[N int64 | float64](t *testing.T, h *retryHarness, name string) []metricdata.HistogramDataPoint[N] {
	t.Helper()
	points, ok := findHistogram[N](t, h.reader, name)
	if !ok {
		t.Fatalf("metric %q not collected", name)
	}
	return points
}

func attrValue(set attribute.Set, key string) (attribute.Value, bool) {
	return set.Value(attribute.Key(key))
}

func TestLoop_GenAIClientMetrics_RecordPerAttemptAndPerTurn(t *testing.T) {
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

	if _, err := h.loop.Run(context.Background(), retryConfig(3)); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Three provider attempts: two successful streams and one failed
	// mid-stream attempt that the turn retry discarded.
	var ok, failed uint64
	for _, dp := range collectHistogram[float64](t, h, "gen_ai.client.operation.duration") {
		if errType, has := attrValue(dp.Attributes, "error.type"); has {
			if errType.AsString() != "provider_stream_failed" {
				t.Errorf("error.type = %q, want provider_stream_failed", errType.AsString())
			}
			failed += dp.Count
		} else {
			ok += dp.Count
		}
		_, failedPoint := attrValue(dp.Attributes, "error.type")
		wantResponse := responseModel
		if failedPoint {
			wantResponse = ""
		}
		if v, _ := attrValue(dp.Attributes, "gen_ai.response.model"); v.AsString() != wantResponse {
			t.Errorf("gen_ai.response.model = %q, want %q", v.AsString(), wantResponse)
		}
		if v, _ := attrValue(dp.Attributes, "gen_ai.provider.name"); v.AsString() != "anthropic" {
			t.Errorf("gen_ai.provider.name = %q, want anthropic", v.AsString())
		}
		if v, _ := attrValue(dp.Attributes, "gen_ai.request.model"); v.AsString() != "claude-sonnet-4-6" {
			t.Errorf("gen_ai.request.model = %q, want claude-sonnet-4-6", v.AsString())
		}
		if v, _ := attrValue(dp.Attributes, "server.address"); v.AsString() != "api.anthropic.com" {
			t.Errorf("server.address = %q, want api.anthropic.com", v.AsString())
		}
		if v, _ := attrValue(dp.Attributes, "server.port"); v.AsInt64() != 443 {
			t.Errorf("server.port = %d, want 443", v.AsInt64())
		}
		for _, banned := range []string{"run.id", "run.parent_id", "run.subagent", "tool.name", "gen_ai.conversation.id"} {
			if _, has := attrValue(dp.Attributes, banned); has {
				t.Errorf("operation.duration carries high-cardinality attribute %q", banned)
			}
		}
	}
	if ok != 2 || failed != 1 {
		t.Errorf("duration points: %d successful, %d failed; want 2 and 1", ok, failed)
	}

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
		if v, _ := attrValue(dp.Attributes, "gen_ai.response.model"); v.AsString() != wantResponse {
			t.Errorf("token usage point (sum %d) gen_ai.response.model = %q, want %q", dp.Sum, v.AsString(), wantResponse)
		}
	}
	if count["input"] != 3 || count["output"] != 3 {
		t.Errorf("token usage points = %v, want 3 input and 3 output", count)
	}
	if sum["input"] != 350 || sum["output"] != 35 {
		t.Errorf("token usage sums = %v, want input=350 output=35", sum)
	}
}

func TestLoop_GenAITokenUsage_OmitsUnreportedCounts(t *testing.T) {
	noUsage := turnScript{events: []types.StreamEvent{
		{Type: "text_delta", Text: "All done."},
		{Type: "message_complete", StopReason: "end_turn"},
	}}
	h := newRetryHarness(t, &scriptedTurnProvider{scripts: []turnScript{noUsage}})
	if _, err := h.loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if points, ok := findHistogram[int64](t, h.reader, "gen_ai.client.token.usage"); ok {
		t.Errorf("token usage recorded %d points for a response that reported no usage, want none", len(points))
	}
}

func TestLoop_GenAIOperationDuration_NonRetryableFailure(t *testing.T) {
	h := newRetryHarness(t, &scriptedTurnProvider{scripts: []turnScript{{err: errors.New("bad request")}}})
	_, _ = h.loop.Run(context.Background(), buildTestConfig())

	points := collectHistogram[float64](t, h, "gen_ai.client.operation.duration")
	if len(points) != 1 || points[0].Count != 1 {
		t.Fatalf("duration points = %+v, want exactly one", points)
	}
	if v, _ := attrValue(points[0].Attributes, "error.type"); v.AsString() != "provider_request_failed" {
		t.Errorf("error.type = %q, want provider_request_failed", v.AsString())
	}
	if _, ok := findHistogram[int64](t, h.reader, "gen_ai.client.token.usage"); ok {
		t.Error("token usage recorded for a request that failed before streaming")
	}
}
