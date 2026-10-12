package provider

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// assertFirstChunkSeries checks that the adapter recorded one
// gen_ai.client.operation.time_to_first_chunk point labelled with the same
// provider, model and endpoint the agentic loop uses for the other GenAI
// client metrics, so the series line up.
func assertFirstChunkSeries(t *testing.T, reader *sdkmetric.ManualReader, adapter ProviderAdapter, wantProvider, wantModel string) {
	t.Helper()
	if got := providerHistogramTotalCount(t, reader, "gen_ai.client.operation.time_to_first_chunk"); got != 1 {
		t.Fatalf("time_to_first_chunk count = %d, want 1", got)
	}
	h, _ := providerHistogramFinder(t, reader, "gen_ai.client.operation.time_to_first_chunk")
	attrs := h.DataPoints[0].Attributes
	wantHost, wantPort := ServerEndpointOf(adapter)
	for key, want := range map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  wantProvider,
		"gen_ai.request.model":  wantModel,
		"server.address":        wantHost,
	} {
		if v, ok := attrs.Value(attribute.Key(key)); !ok || v.AsString() != want {
			t.Errorf("time_to_first_chunk %s = %q ok=%v, want %q", key, v.AsString(), ok, want)
		}
	}
	if v, ok := attrs.Value("server.port"); !ok || v.AsInt64() != int64(wantPort) {
		t.Errorf("time_to_first_chunk server.port = %d ok=%v, want %d", v.AsInt64(), ok, wantPort)
	}
	if n := attrs.Len(); n != 5 {
		t.Errorf("time_to_first_chunk attribute count = %d, want 5", n)
	}
}

func TestServerEndpointOf(t *testing.T) {
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
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("ServerEndpointOf() = (%q, %d), want (%q, %d)", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}
