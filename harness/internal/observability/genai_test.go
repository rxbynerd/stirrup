package observability

import "testing"

// TestGenAIProviderName pins the stirrup→OTel GenAI provider enum
// translation table. Vendor APM dashboards filter on the spec enum
// values; if any of these mappings drift, dashboards stop matching
// stirrup spans and metrics for that provider with no other surface signal.
func TestGenAIProviderName(t *testing.T) {
	cases := []struct {
		stirrupType string
		want        string
	}{
		{"anthropic", "anthropic"},
		{"bedrock", "aws.bedrock"},
		// openai-compatible falls through unchanged: it is generic Chat Completions, not OpenAI specifically.
		{"openai-compatible", "openai-compatible"},
		{"openai-responses", "openai"},
		{"gemini", "gcp.vertex_ai"},
	}
	for _, tc := range cases {
		t.Run(tc.stirrupType, func(t *testing.T) {
			if got := GenAIProviderName(tc.stirrupType); got != tc.want {
				t.Errorf("GenAIProviderName(%q) = %q, want %q", tc.stirrupType, got, tc.want)
			}
		})
	}
}
