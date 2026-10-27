package core

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

const (
	scopedContextWindow   = 123_456
	unscopedContextWindow = 64_000
)

// endpointScopedRegistry sets every loop-side capability for
// openai-responses, and a larger set of values only on the openai profile.
func endpointScopedRegistry() *quirks.Registry {
	return quirks.NewRegistry([]quirks.Rule{
		{
			ProviderType: "openai-responses",
			ModelMatch:   "gpt-5*",
			Description:  "test: endpoint-agnostic",
			Apply: func(q *quirks.ProviderQuirks) {
				q.ContextWindow = unscopedContextWindow
			},
		},
		{
			ProviderType: "openai-responses",
			ModelMatch:   "gpt-5*",
			Endpoint:     types.EndpointProfileOpenAI,
			Description:  "test: openai endpoint",
			Apply: func(q *quirks.ProviderQuirks) {
				q.ContextWindow = scopedContextWindow
				q.ToolChoice = quirks.ToolChoiceCapability{Supported: true, Required: true}
				q.StructuredToolResults = quirks.StructuredToolResultCapability{Supported: true, ContentBlockArray: true}
				q.ToolSearch = quirks.ToolSearchCapability{Supported: true, Variant: "hosted"}
			},
		},
	})
}

func TestLoopSideLookups_ResolveWithTheProvidersEndpointProfile(t *testing.T) {
	const model = "gpt-5.5"
	reg := endpointScopedRegistry()
	for _, tc := range []struct {
		name   string
		config *types.RunConfig
		key    string
		scoped bool
	}{
		{
			name:   "default provider declared openai",
			config: &types.RunConfig{Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: "https://relay.example.com/v1", EndpointProfile: types.EndpointProfileOpenAI}},
			key:    "openai-responses",
			scoped: true,
		},
		{
			name:   "default provider derived generic",
			config: &types.RunConfig{Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example.com/v1"}},
			key:    "openai-responses",
			scoped: false,
		},
		{
			name: "named provider declared openai",
			config: &types.RunConfig{
				Provider:  types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example.com/v1"},
				Providers: map[string]types.ProviderConfig{"direct": {Type: "openai-responses", BaseURL: "https://relay.example.com/v1", EndpointProfile: types.EndpointProfileOpenAI}},
			},
			key:    "direct",
			scoped: true,
		},
		{
			name: "named provider derived generic",
			config: &types.RunConfig{
				Provider:  types.ProviderConfig{Type: "openai-responses"},
				Providers: map[string]types.ProviderConfig{"gateway": {Type: "openai-responses", BaseURL: "https://gateway.example.com/v1"}},
			},
			key:    "gateway",
			scoped: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)

			loop := &AgenticLoop{ContextWindows: reg}
			window := loop.resolveContextWindow(tc.config, tc.key, model)
			policy, ok := buildEscalationPolicy(1, tc.config, reg).(*defaultEscalationPolicy)
			must.True(ok)
			toolChoice := policy.toolChoice(tc.key, model)
			structured := buildStructuredResultSupport(tc.config, reg)(tc.key, model)
			toolSearch := buildToolSearchSupport(tc.config, reg, slog.New(slog.DiscardHandler))(tc.key, model)

			if tc.scoped {
				must.Equal(scopedContextWindow, window.tokens)
			} else {
				must.Equal(unscopedContextWindow, window.tokens)
			}
			must.Equal(tc.scoped, toolChoice.Required)
			must.Equal(tc.scoped, structured)
			must.Equal(tc.scoped, toolSearch.Supported)
		})
	}
}

func TestTargetFor_UnknownKeyHasNoEndpoint(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{Provider: types.ProviderConfig{Type: "openai-responses", EndpointProfile: types.EndpointProfileOpenAI}}
	must.Equal(quirks.Target{ProviderType: "mystery", Model: "gpt-5.5"}, targetFor(config, "mystery", "gpt-5.5"))
	must.Equal(quirks.Target{ProviderType: "openai-responses", Endpoint: types.EndpointProfileOpenAI, Model: "gpt-5.5"}, targetFor(config, "openai-responses", "gpt-5.5"))
}
