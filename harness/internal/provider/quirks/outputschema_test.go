package quirks

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOutputSchema_CapabilityRules pins which models advertise JSON Schema
// structured outputs: the Claude 5.x and Opus 4.7/4.8 families, and gpt-5
// and gpt-6 on both OpenAI wires (gateway-prefixed on Chat Completions
// too). Everything else, including Gemini (unprobed on Vertex AI),
// open-weight and unknown models, resolves to no support so the judges
// keep their prompt-only contract.
func TestOutputSchema_CapabilityRules(t *testing.T) {
	granted := []Target{
		{ProviderType: "anthropic", Model: "claude-opus-4-7"},
		{ProviderType: "anthropic", Model: "claude-opus-4-8"},
		{ProviderType: "anthropic", Model: "claude-opus-5-5"},
		{ProviderType: "anthropic", Model: "claude-sonnet-5"},
		{ProviderType: "anthropic", Model: "claude-sonnet-5-5"},
		{ProviderType: "anthropic", Model: "claude-haiku-5-5"},
		{ProviderType: "anthropic", Model: "claude-fable-5-1"},
		{ProviderType: "anthropic", Model: "claude-mythos-5-1"},
		{ProviderType: "openai-compatible", Model: "gpt-5-nano"},
		{ProviderType: "openai-compatible", Model: "gpt-5.5"},
		{ProviderType: "openai-compatible", Model: "gpt-6-luna"},
		{ProviderType: "openai-compatible", Model: "openai/gpt-5.5"},
		{ProviderType: "openai-compatible", Model: "openai/gpt-6-astra"},
		{ProviderType: "openai-responses", Model: "gpt-5.4-mini"},
		{ProviderType: "openai-responses", Model: "gpt-6-astra"},
		{ProviderType: "openai-responses", Endpoint: "generic", Model: "gpt-5.5"},
	}
	for _, target := range granted {
		t.Run("granted/"+target.ProviderType+"/"+target.Model, func(t *testing.T) {
			must := require.New(t)
			must.True(DefaultRegistry().Resolve(target).OutputSchema.Supported)
		})
	}

	ungranted := []Target{
		{ProviderType: "anthropic", Model: "claude-haiku-4-5"},
		{ProviderType: "anthropic", Model: "claude-sonnet-4-6"},
		{ProviderType: "anthropic", Model: "claude-opus-4-6"},
		{ProviderType: "openai-compatible", Model: "gpt-4o"},
		{ProviderType: "openai-compatible", Model: "o3-mini"},
		{ProviderType: "openai-compatible", Model: "deepseek-v4-flash"},
		{ProviderType: "openai-compatible", Model: "qwen3.6-27b"},
		{ProviderType: "openai-compatible", Model: "deepseek/deepseek-v4"},
		{ProviderType: "openai-responses", Model: "gpt-4o"},
		{ProviderType: "openai-responses", Model: "o3"},
		{ProviderType: "gemini", Model: "gemini-2.5-pro"},
		{ProviderType: "gemini", Model: "gemini-3.8-flash"},
		{ProviderType: "gemini", Model: "gemini-3.1-pro-preview"},
		{ProviderType: "bedrock", Model: "us.anthropic.claude-haiku-5-5"},
		{ProviderType: "replay", Model: "claude-haiku-5-5"},
		{ProviderType: "mystery-provider", Model: "some-model"},
	}
	for _, target := range ungranted {
		t.Run("ungranted/"+target.ProviderType+"/"+target.Model, func(t *testing.T) {
			must := require.New(t)
			must.Zero(DefaultRegistry().Resolve(target).OutputSchema)
		})
	}
}

// TestOutputSchema_ComposesWithSiblingRules pins that the structured-output
// rules add to, rather than replace, the per-family quirks that fire on
// the same model.
func TestOutputSchema_ComposesWithSiblingRules(t *testing.T) {
	t.Run("claude-haiku-5-5 keeps omit-sampling, effort and tool search", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve(Target{ProviderType: "anthropic", Model: "claude-haiku-5-5"})
		must.True(q.OutputSchema.Supported)
		must.True(q.BehaviourFlags.Anthropic.OmitSamplingParams)
		must.NotEmpty(q.BehaviourFlags.Anthropic.EffortLevels)
		must.True(q.ToolSearch.Supported)
	})

	t.Run("claude-sonnet-5-5 keeps auto-only tool choice", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve(Target{ProviderType: "anthropic", Model: "claude-sonnet-5-5"})
		must.True(q.OutputSchema.Supported)
		must.Equal(ToolChoiceCapability{Supported: true, Auto: true}, q.ToolChoice)
	})

	t.Run("gpt-6 on chat keeps tools-require-responses", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve(Target{ProviderType: "openai-compatible", Model: "gpt-6-luna"})
		must.True(q.OutputSchema.Supported)
		must.True(q.BehaviourFlags.OpenAI.ToolsRequireResponses)
		must.True(q.BehaviourFlags.OpenAI.StrictMode)
	})
}
