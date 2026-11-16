package observability

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGenAIProviderName_TranslatesStirrupTypes pins the stirrup→OTel GenAI provider enum
// translation table. Vendor APM dashboards filter on the spec enum
// values; if any of these mappings drift, dashboards stop matching
// stirrup spans and metrics for that provider with no other surface signal.
func TestGenAIProviderName_TranslatesStirrupTypes(t *testing.T) {
	cases := []struct {
		name        string
		stirrupType string
		want        string
	}{
		{"anthropic passes through", "anthropic", "anthropic"},
		{"bedrock maps to aws.bedrock", "bedrock", "aws.bedrock"},
		// openai-compatible falls through unchanged: it is generic Chat Completions, not OpenAI specifically.
		{"openai-compatible passes through", "openai-compatible", "openai-compatible"},
		{"openai-responses maps to openai", "openai-responses", "openai"},
		{"gemini maps to gcp.vertex_ai", "gemini", "gcp.vertex_ai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, GenAIProviderName(tc.stirrupType, ""))
		})
	}
}

func TestGenAIProviderName_OpenAICompatibleHost(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string
	}{
		{"deepseek", "api.deepseek.com", "deepseek"},
		{"groq", "api.groq.com", "groq"},
		{"mistral", "api.mistral.ai", "mistral"},
		{"xai", "api.x.ai", "x_ai"},
		{"azure openai resource", "my-resource.openai.azure.com", "azure.ai.openai"},
		{"azure cognitive services resource", "my-resource.cognitiveservices.azure.com", "azure.ai.openai"},
		{"host match ignores case and a trailing dot", "API.DeepSeek.com.", "deepseek"},
		{"openrouter has no registered name", "openrouter.ai", "openai-compatible"},
		{"self-hosted server", "localhost", "openai-compatible"},
		{"unknown host", "llm.internal.example", "openai-compatible"},
		{"look-alike host is not a subdomain", "evilapi.deepseek.com.example", "openai-compatible"},
		{"suffix without a dot boundary", "notapi.groq.com", "openai-compatible"},
		{"no host", "", "openai-compatible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, GenAIProviderName("openai-compatible", tc.host))
		})
	}
}

func TestGenAIProviderName_HostOnlyAffectsOpenAICompatible(t *testing.T) {
	for _, stirrupType := range []string{"anthropic", "openai-responses", "gemini", "bedrock"} {
		t.Run(stirrupType, func(t *testing.T) {
			must := require.New(t)
			must.Equal(GenAIProviderName(stirrupType, ""), GenAIProviderName(stirrupType, "api.deepseek.com"))
		})
	}
}
