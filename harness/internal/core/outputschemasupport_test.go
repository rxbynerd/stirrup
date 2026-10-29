package core

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// TestBuildOutputSchemaSupport_ResolvesDefaultProvider pins that the
// judges' recorded structured-output mode follows the default provider's
// capability for the judge model, not the run's main model.
func TestBuildOutputSchemaSupport_ResolvesDefaultProvider(t *testing.T) {
	cases := []struct {
		name     string
		provider types.ProviderConfig
		model    string
		want     bool
	}{
		{"anthropic haiku 5.5 is granted", types.ProviderConfig{Type: "anthropic"}, "claude-haiku-5-5", true},
		{"anthropic haiku 4.5 is not", types.ProviderConfig{Type: "anthropic"}, "claude-haiku-4-5", false},
		{"openai-compatible gpt-5 is granted", types.ProviderConfig{Type: "openai-compatible"}, "gpt-5-mini", true},
		{"openai-compatible open-weight model is not", types.ProviderConfig{Type: "openai-compatible"}, "qwen3.6-27b", false},
		{"bedrock is not", types.ProviderConfig{Type: "bedrock"}, "us.anthropic.claude-haiku-5-5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			config := &types.RunConfig{Provider: tc.provider, ModelRouter: types.ModelRouterConfig{Model: "claude-opus-5-5"}}
			support := buildOutputSchemaSupport(config, quirks.DefaultRegistry())
			must.Equal(tc.want, support(tc.model))
		})
	}
}
