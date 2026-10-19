package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// assertFirstChunkSeries checks that the adapter recorded one
// gen_ai.client.operation.time_to_first_chunk point labelled with the same
// provider, model and endpoint the agentic loop uses for the other GenAI
// client metrics, so the series line up.
func assertFirstChunkSeries(t *testing.T, reader *sdkmetric.ManualReader, adapter ProviderAdapter, wantProvider, wantModel string) {
	t.Helper()
	require.Equal(t, uint64(1), providerHistogramTotalCount(t, reader, "gen_ai.client.operation.time_to_first_chunk"))

	h, _ := providerHistogramFinder(t, reader, "gen_ai.client.operation.time_to_first_chunk")
	attrs := h.DataPoints[0].Attributes
	wantHost, wantPort := ServerEndpointOf(adapter)
	for key, want := range map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  wantProvider,
		"gen_ai.request.model":  wantModel,
		"server.address":        wantHost,
	} {
		t.Run(key, func(t *testing.T) {
			v, ok := attrs.Value(attribute.Key(key))
			require.True(t, ok, "attribute is missing")
			require.Equal(t, want, v.AsString())
		})
	}

	port, ok := attrs.Value("server.port")
	require.True(t, ok, "server.port attribute is missing")
	require.EqualValues(t, wantPort, port.AsInt64())
	require.Equal(t, 5, attrs.Len(), "attribute count")
}

func TestServerEndpointOf_ResolvesHostAndPort(t *testing.T) {
	anthropic := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
	compat := NewOpenAICompatibleAdapter(staticBearer("k"), "http://llm.internal:8080/v1/", OpenAIAuthConfig{}, RetryPolicy{})
	responses := NewOpenAIResponsesAdapter(staticBearer("k"), "", OpenAIAuthConfig{})
	global := NewGeminiAdapter(staticBearer("k"), "proj", "global", nil)
	regional := NewGeminiAdapter(staticBearer("k"), "proj", "europe-west4", nil)

	cases := []struct {
		name     string
		adapter  ProviderAdapter
		wantHost string
		wantPort int
	}{
		{"anthropic", anthropic, "api.anthropic.com", 443},
		{"openai-compatible custom base URL", compat, "llm.internal", 8080},
		{"openai-responses default base URL", responses, "api.openai.com", 443},
		{"gemini global", global, "aiplatform.googleapis.com", 443},
		{"gemini regional", regional, "europe-west4-aiplatform.googleapis.com", 443},
		{"normalizing wrapper delegates", NewNormalizingAdapter(anthropic, "anthropic"), "api.anthropic.com", 443},
		{"batch wrapper delegates", &BatchAdapter{inner: compat}, "llm.internal", 8080},
		{"bedrock regional runtime host", &BedrockAdapter{region: "us-east-1"}, "bedrock-runtime.us-east-1.amazonaws.com", 443},
		{"bedrock china partition", &BedrockAdapter{region: "cn-north-1"}, "bedrock-runtime.cn-north-1.amazonaws.com.cn", 443},
		{"bedrock without a region", &BedrockAdapter{}, "", 0},
		{"wrapped bedrock delegates", NewNormalizingAdapter(&BedrockAdapter{region: "eu-west-2"}, "bedrock"), "bedrock-runtime.eu-west-2.amazonaws.com", 443},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := ServerEndpointOf(tc.adapter)
			require.Equal(t, tc.wantHost, host)
			require.Equal(t, tc.wantPort, port)
		})
	}
}
