package observability

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newInferenceTestMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := newMetricsFromMeter(provider.Meter("test"), provider)
	if err != nil {
		t.Fatalf("newMetricsFromMeter() error: %v", err)
	}
	return m, reader
}

func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not collected", name)
	return metricdata.Metrics{}
}

func inferenceAttrMap(set attribute.Set) map[string]attribute.Value {
	out := make(map[string]attribute.Value)
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func testInference() InferenceAttrs {
	return InferenceAttrs{
		ProviderType:  "anthropic",
		RequestModel:  "claude-haiku-4-5",
		ResponseModel: "claude-haiku-4-5-20251001",
		ServerAddress: "api.anthropic.com",
		ServerPort:    443,
	}
}

func TestGenAIClientMetrics_InstrumentDefinitions(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	ctx := context.Background()
	a := testInference()
	m.RecordInferenceTokens(ctx, a, 10, 5)
	m.RecordInferenceDuration(ctx, a, time.Second, "")
	m.RecordFirstChunk(ctx, metric.WithAttributes(), a, time.Second)

	tokenBuckets := []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	durationBuckets := []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}

	tokens := collectMetric(t, reader, "gen_ai.client.token.usage")
	if tokens.Unit != "{token}" {
		t.Errorf("token usage unit = %q, want {token}", tokens.Unit)
	}
	th, ok := tokens.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("token usage data = %T, want Histogram[int64]", tokens.Data)
	}
	for _, dp := range th.DataPoints {
		if !slices.Equal(dp.Bounds, tokenBuckets) {
			t.Errorf("token usage bounds = %v, want %v", dp.Bounds, tokenBuckets)
		}
	}

	for _, name := range []string{"gen_ai.client.operation.duration", "gen_ai.client.operation.time_to_first_chunk"} {
		got := collectMetric(t, reader, name)
		if got.Unit != "s" {
			t.Errorf("%s unit = %q, want s", name, got.Unit)
		}
		h, ok := got.Data.(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("%s data = %T, want Histogram[float64]", name, got.Data)
		}
		for _, dp := range h.DataPoints {
			if !slices.Equal(dp.Bounds, durationBuckets) {
				t.Errorf("%s bounds = %v, want %v", name, dp.Bounds, durationBuckets)
			}
		}
	}
}

func TestRecordInferenceTokens_RecordsInputAndOutputPoints(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	m.RecordInferenceTokens(context.Background(), testInference(), 1200, 80)

	got := collectMetric(t, reader, "gen_ai.client.token.usage").Data.(metricdata.Histogram[int64])
	sums := make(map[string]int64)
	for _, dp := range got.DataPoints {
		attrs := inferenceAttrMap(dp.Attributes)
		if dp.Count != 1 {
			t.Errorf("token type %v count = %d, want 1", attrs["gen_ai.token.type"], dp.Count)
		}
		sums[attrs["gen_ai.token.type"].AsString()] = dp.Sum
		want := map[string]attribute.Value{
			"gen_ai.operation.name": attribute.StringValue("chat"),
			"gen_ai.provider.name":  attribute.StringValue("anthropic"),
			"gen_ai.request.model":  attribute.StringValue("claude-haiku-4-5"),
			"gen_ai.response.model": attribute.StringValue("claude-haiku-4-5-20251001"),
			"server.address":        attribute.StringValue("api.anthropic.com"),
			"server.port":           attribute.IntValue(443),
			"gen_ai.token.type":     attrs["gen_ai.token.type"],
		}
		if len(attrs) != len(want) {
			t.Errorf("attribute set = %v, want exactly %v", attrs, want)
		}
		for k, v := range want {
			if attrs[k] != v {
				t.Errorf("attribute %s = %v, want %v", k, attrs[k], v)
			}
		}
	}
	if len(sums) != 2 || sums["input"] != 1200 || sums["output"] != 80 {
		t.Errorf("token sums by type = %v, want input=1200 output=80", sums)
	}
}

func TestInferenceAttrs_OmitsUnknownFields(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	m.RecordInferenceDuration(context.Background(), InferenceAttrs{ProviderType: "bedrock", RequestModel: "m"}, time.Second, "")

	got := collectMetric(t, reader, "gen_ai.client.operation.duration").Data.(metricdata.Histogram[float64])
	attrs := inferenceAttrMap(got.DataPoints[0].Attributes)
	want := map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "aws.bedrock",
		"gen_ai.request.model":  "m",
	}
	if len(attrs) != len(want) {
		t.Errorf("attribute set = %v, want exactly %v", attrs, want)
	}
	for k, v := range want {
		if attrs[k].AsString() != v {
			t.Errorf("attribute %s = %v, want %v", k, attrs[k], v)
		}
	}
}

func TestRecordInferenceTokens_SkipsUnreportedCounts(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	m.RecordInferenceTokens(context.Background(), testInference(), 0, 80)

	got := collectMetric(t, reader, "gen_ai.client.token.usage").Data.(metricdata.Histogram[int64])
	if len(got.DataPoints) != 1 {
		t.Fatalf("data points = %d, want only the output point", len(got.DataPoints))
	}
	attrs := inferenceAttrMap(got.DataPoints[0].Attributes)
	if attrs["gen_ai.token.type"].AsString() != "output" || got.DataPoints[0].Sum != 80 {
		t.Errorf("point = %v sum %d, want output sum 80", attrs["gen_ai.token.type"], got.DataPoints[0].Sum)
	}
}

func TestRecordInferenceDuration_ErrorTypeOnlyOnFailure(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	ctx := context.Background()
	m.RecordInferenceDuration(ctx, testInference(), 2*time.Second, "")
	m.RecordInferenceDuration(ctx, testInference(), 500*time.Millisecond, "provider_request_failed")

	got := collectMetric(t, reader, "gen_ai.client.operation.duration").Data.(metricdata.Histogram[float64])
	if len(got.DataPoints) != 2 {
		t.Fatalf("data points = %d, want 2 (one per error.type value)", len(got.DataPoints))
	}
	for _, dp := range got.DataPoints {
		errType, hasErr := inferenceAttrMap(dp.Attributes)["error.type"]
		switch {
		case hasErr && errType.AsString() == "provider_request_failed":
			if dp.Sum != 0.5 {
				t.Errorf("failed duration sum = %v, want 0.5 seconds", dp.Sum)
			}
		case !hasErr:
			if dp.Sum != 2 {
				t.Errorf("successful duration sum = %v, want 2 seconds", dp.Sum)
			}
		default:
			t.Errorf("unexpected error.type %v", errType)
		}
	}
}

func TestRecordFirstChunk_FeedsLegacyAndGenAIHistograms(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	legacy := metric.WithAttributes(attribute.String("provider.type", "anthropic"))
	m.RecordFirstChunk(context.Background(), legacy, testInference(), 1500*time.Millisecond)

	ttfb := collectMetric(t, reader, "stirrup.harness.provider_ttfb").Data.(metricdata.Histogram[float64])
	if ttfb.DataPoints[0].Sum != 1500 {
		t.Errorf("provider_ttfb sum = %v, want 1500 milliseconds", ttfb.DataPoints[0].Sum)
	}
	ttfc := collectMetric(t, reader, "gen_ai.client.operation.time_to_first_chunk").Data.(metricdata.Histogram[float64])
	if ttfc.DataPoints[0].Sum != 1.5 {
		t.Errorf("time_to_first_chunk sum = %v, want 1.5 seconds", ttfc.DataPoints[0].Sum)
	}
	attrs := inferenceAttrMap(ttfc.DataPoints[0].Attributes)
	if attrs["gen_ai.provider.name"].AsString() != "anthropic" || attrs["gen_ai.request.model"].AsString() != "claude-haiku-4-5" {
		t.Errorf("time_to_first_chunk attributes = %v, want GenAI provider and model", attrs)
	}
	if _, ok := attrs["provider.type"]; ok {
		t.Errorf("time_to_first_chunk carries legacy attribute provider.type")
	}
}

func TestServerEndpointFromURL(t *testing.T) {
	cases := []struct {
		url      string
		wantHost string
		wantPort int
	}{
		{"https://api.anthropic.com/v1/messages", "api.anthropic.com", 443},
		{"http://localhost:11434/v1", "localhost", 11434},
		{"http://llm.internal/v1", "llm.internal", 80},
		{"https://us-central1-aiplatform.googleapis.com:8443/v1/projects", "us-central1-aiplatform.googleapis.com", 8443},
		{"", "", 0},
		{"not a url", "", 0},
		{"/relative/path", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			host, port := ServerEndpointFromURL(tc.url)
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("ServerEndpointFromURL(%q) = (%q, %d), want (%q, %d)", tc.url, host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}
