package quirks

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestToolSearchCapability_ResolvesPerModel(t *testing.T) {
	cases := []struct {
		provider, model string
		supported       bool
		variant         string
	}{
		{"anthropic", "claude-opus-4-5-20251101", true, "bm25"},
		{"anthropic", "claude-opus-4-6", true, "bm25"},
		{"anthropic", "claude-opus-4-8", true, "bm25"},
		{"anthropic", "claude-sonnet-4-5-20250929", true, "bm25"},
		{"anthropic", "claude-sonnet-4-6", true, "bm25"},
		{"anthropic", "claude-haiku-4-5-20251001", true, "bm25"},
		{"anthropic", "claude-sonnet-5-5", true, "bm25"},
		{"anthropic", "claude-opus-5", true, "bm25"},
		{"anthropic", "claude-fable-5-1", true, "bm25"},
		{"anthropic", "claude-haiku-5-5", true, "bm25"},
		{"anthropic", "claude-mythos-5-1", true, "bm25"},
		{"anthropic", "claude-opus-4-1-20250805", false, ""},
		{"anthropic", "claude-sonnet-4-20250514", false, ""},
		{"anthropic", "claude-3-5-sonnet-20241022", false, ""},
		{"anthropic", "claude-3-5-haiku-20241022", false, ""},
		{"openai-responses", "gpt-5.4", true, "hosted"},
		{"openai-responses", "gpt-5.5-mini", true, "hosted"},
		{"openai-responses", "gpt-6-astra", true, "hosted"},
		{"openai-responses", "gpt-5", false, ""},
		{"openai-responses", "gpt-5.2", false, ""},
		{"openai-responses", "o3", false, ""},
		{"openai-compatible", "gpt-6", false, ""},
		{"gemini", "gemini-3.8-pro", false, ""},
		{"bedrock", "anthropic.claude-sonnet-5-5", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			must := require.New(t)
			endpoint := ""
			if tc.provider == "openai-responses" || tc.provider == "openai-compatible" {
				endpoint = types.EndpointProfileOpenAI
			}
			got := DefaultRegistry().Resolve(Target{ProviderType: tc.provider, Endpoint: endpoint, Model: tc.model}).ToolSearch
			must.Equal(tc.supported, got.Supported)
			must.Equal(tc.variant, got.Variant)
		})
	}
}

func TestToolSearchCapability_HostedSearchOnlyOnOpenAIEndpointProfile(t *testing.T) {
	for _, tc := range []struct {
		endpoint  string
		supported bool
	}{
		{types.EndpointProfileOpenAI, true},
		{types.EndpointProfileGeneric, false},
		{types.EndpointProfileAzureOpenAI, false},
		{types.EndpointProfileBedrockMantle, false},
		{"", false},
	} {
		t.Run("endpoint "+tc.endpoint, func(t *testing.T) {
			must := require.New(t)
			for _, model := range []string{"gpt-5.4", "gpt-6-astra"} {
				got := DefaultRegistry().Resolve(Target{ProviderType: "openai-responses", Endpoint: tc.endpoint, Model: model}).ToolSearch
				must.Equal(tc.supported, got.Supported, model)
			}
		})
	}
}

func TestBuiltinRules_EndpointIsARecognisedProfile(t *testing.T) {
	for _, rule := range BuiltinRules() {
		if rule.Endpoint == "" {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			must.Contains(types.ValidEndpointProfileValues(), rule.Endpoint)
			must.Contains([]string{"openai-compatible", "openai-responses"}, rule.ProviderType)
		})
	}
}
