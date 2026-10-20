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
			must.Equal(tc.want, GenAIProviderName(tc.stirrupType))
		})
	}
}
