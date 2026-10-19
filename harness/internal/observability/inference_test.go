package observability

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	return m, reader
}

func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	require.FailNowf(t, "metric not collected", "metric %q", name)
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

	t.Run("token usage", func(t *testing.T) {
		tokens := collectMetric(t, reader, "gen_ai.client.token.usage")
		require.Equal(t, "{token}", tokens.Unit)
		th, ok := tokens.Data.(metricdata.Histogram[int64])
		require.True(t, ok, "token usage data = %T, want Histogram[int64]", tokens.Data)
		for _, dp := range th.DataPoints {
			require.True(t, slices.Equal(tokenBuckets, dp.Bounds), "bounds = %v, want %v", dp.Bounds, tokenBuckets)
		}
	})

	for _, name := range []string{"gen_ai.client.operation.duration", "gen_ai.client.operation.time_to_first_chunk"} {
		t.Run(name, func(t *testing.T) {
			got := collectMetric(t, reader, name)
			require.Equal(t, "s", got.Unit)
			h, ok := got.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "data = %T, want Histogram[float64]", got.Data)
			for _, dp := range h.DataPoints {
				require.True(t, slices.Equal(durationBuckets, dp.Bounds), "bounds = %v, want %v", dp.Bounds, durationBuckets)
			}
		})
	}
}

func TestRecordInferenceTokens_RecordsInputAndOutputPoints(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	m.RecordInferenceTokens(context.Background(), testInference(), 1200, 80)

	got := collectMetric(t, reader, "gen_ai.client.token.usage").Data.(metricdata.Histogram[int64])
	sums := make(map[string]int64)
	for _, dp := range got.DataPoints {
		attrs := inferenceAttrMap(dp.Attributes)
		tokenType := attrs["gen_ai.token.type"].AsString()
		sums[tokenType] = dp.Sum
		t.Run(tokenType, func(t *testing.T) {
			require.Equal(t, uint64(1), dp.Count)
			want := map[string]attribute.Value{
				"gen_ai.operation.name": attribute.StringValue("chat"),
				"gen_ai.provider.name":  attribute.StringValue("anthropic"),
				"gen_ai.request.model":  attribute.StringValue("claude-haiku-4-5"),
				"gen_ai.response.model": attribute.StringValue("claude-haiku-4-5-20251001"),
				"server.address":        attribute.StringValue("api.anthropic.com"),
				"server.port":           attribute.IntValue(443),
				"gen_ai.token.type":     attrs["gen_ai.token.type"],
			}
			require.Equal(t, want, attrs)
		})
	}
	require.Equal(t, map[string]int64{"input": 1200, "output": 80}, sums)
}

func TestInferenceAttrs_OmitsUnknownFields(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	m.RecordInferenceDuration(context.Background(), InferenceAttrs{ProviderType: "bedrock", RequestModel: "m"}, time.Second, "")

	got := collectMetric(t, reader, "gen_ai.client.operation.duration").Data.(metricdata.Histogram[float64])
	attrs := inferenceAttrMap(got.DataPoints[0].Attributes)
	gotStrings := make(map[string]string, len(attrs))
	for k, v := range attrs {
		gotStrings[k] = v.AsString()
	}
	want := map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "aws.bedrock",
		"gen_ai.request.model":  "m",
	}
	require.Equal(t, want, gotStrings)
}

func TestRecordInferenceTokens_SkipsUnreportedCounts(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	m.RecordInferenceTokens(context.Background(), testInference(), 0, 80)

	got := collectMetric(t, reader, "gen_ai.client.token.usage").Data.(metricdata.Histogram[int64])
	require.Len(t, got.DataPoints, 1, "only the output point is recorded")
	attrs := inferenceAttrMap(got.DataPoints[0].Attributes)
	require.Equal(t, "output", attrs["gen_ai.token.type"].AsString())
	require.Equal(t, int64(80), got.DataPoints[0].Sum)
}

func TestRecordInferenceDuration_SetsErrorTypeOnlyOnFailure(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	ctx := context.Background()
	m.RecordInferenceDuration(ctx, testInference(), 2*time.Second, "")
	m.RecordInferenceDuration(ctx, testInference(), 500*time.Millisecond, "provider_request_failed")

	got := collectMetric(t, reader, "gen_ai.client.operation.duration").Data.(metricdata.Histogram[float64])
	require.Len(t, got.DataPoints, 2, "one data point per error.type value")
	for _, dp := range got.DataPoints {
		errType, hasErr := inferenceAttrMap(dp.Attributes)["error.type"]
		switch {
		case hasErr && errType.AsString() == "provider_request_failed":
			require.InDelta(t, 0.5, dp.Sum, 1e-9, "failed request duration in seconds")
		case !hasErr:
			require.InDelta(t, 2.0, dp.Sum, 1e-9, "successful request duration in seconds")
		default:
			require.Failf(t, "unexpected error.type", "error.type = %v", errType)
		}
	}
}

func TestRecordFirstChunk_FeedsLegacyAndGenAIHistograms(t *testing.T) {
	m, reader := newInferenceTestMetrics(t)
	legacy := metric.WithAttributes(attribute.String("provider.type", "anthropic"))
	m.RecordFirstChunk(context.Background(), legacy, testInference(), 1500*time.Millisecond)

	ttfb := collectMetric(t, reader, "stirrup.harness.provider_ttfb").Data.(metricdata.Histogram[float64])
	require.InDelta(t, 1500.0, ttfb.DataPoints[0].Sum, 1e-9, "legacy histogram sum in milliseconds")

	ttfc := collectMetric(t, reader, "gen_ai.client.operation.time_to_first_chunk").Data.(metricdata.Histogram[float64])
	require.InDelta(t, 1.5, ttfc.DataPoints[0].Sum, 1e-9, "GenAI histogram sum in seconds")
	attrs := inferenceAttrMap(ttfc.DataPoints[0].Attributes)
	require.Equal(t, "anthropic", attrs["gen_ai.provider.name"].AsString())
	require.Equal(t, "claude-haiku-4-5", attrs["gen_ai.request.model"].AsString())
	require.NotContains(t, attrs, "provider.type", "GenAI histogram carries the legacy attribute")
}

func TestServerEndpointFromURL_ExtractsHostAndPort(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		wantHost string
		wantPort int
	}{
		{"https with default port", "https://api.anthropic.com/v1/messages", "api.anthropic.com", 443},
		{"http with explicit port", "http://localhost:11434/v1", "localhost", 11434},
		{"http with default port", "http://llm.internal/v1", "llm.internal", 80},
		{"https with explicit port", "https://us-central1-aiplatform.googleapis.com:8443/v1/projects", "us-central1-aiplatform.googleapis.com", 8443},
		{"empty url", "", "", 0},
		{"url without scheme", "not a url", "", 0},
		{"relative path", "/relative/path", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := ServerEndpointFromURL(tc.url)
			require.Equal(t, tc.wantHost, host)
			require.Equal(t, tc.wantPort, port)
		})
	}
}
