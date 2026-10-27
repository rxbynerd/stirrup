package core

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

const hostedSearchModel = "gpt-5.5"

func TestBuildToolSearchSupport_DefaultEndpointKeepsHostedSearch(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
	}{
		{"empty", ""},
		{"exact url", "https://api.openai.com/v1"},
		{"trailing slash", "https://api.openai.com/v1/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			config := &types.RunConfig{Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: tt.baseURL}}
			support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
			must.Equal(quirks.ToolSearchCapability{Supported: true, Variant: "hosted"}, support("", hostedSearchModel))
		})
	}
}

func TestBuildToolSearchSupport_CustomBaseURLDerivesGenericAndUsesHarnessSearch(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
	}{
		{"azure deployment", "https://example.openai.azure.com/openai/v1"},
		{"litellm gateway", "http://litellm.internal:4000/v1"},
		{"openai host with another path", "https://api.openai.com/v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			config := &types.RunConfig{Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: tt.baseURL}}
			support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
			must.Equal(quirks.ToolSearchCapability{}, support("", hostedSearchModel))
		})
	}
}

func TestBuildToolSearchSupport_DeclaredOpenAIProfileOnCustomBaseURLKeepsHostedSearch(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{Provider: types.ProviderConfig{
		Type:            "openai-responses",
		BaseURL:         "https://relay.example.com/v1",
		EndpointProfile: types.EndpointProfileOpenAI,
	}}
	support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
	must.Equal(quirks.ToolSearchCapability{Supported: true, Variant: "hosted"}, support("", hostedSearchModel))
}

func TestBuildToolSearchSupport_UnprobedEndpointProfilesUseHarnessSearch(t *testing.T) {
	for _, tc := range []struct {
		profile string
		baseURL string
	}{
		{types.EndpointProfileAzureOpenAI, "https://example.openai.azure.com/openai/v1"},
		{types.EndpointProfileBedrockMantle, "https://bedrock-mantle.us-east-1.api.aws/v1"},
		{types.EndpointProfileGeneric, ""},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			must := require.New(t)
			config := &types.RunConfig{Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: tc.baseURL, EndpointProfile: tc.profile}}
			support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
			must.Equal(quirks.ToolSearchCapability{}, support("", hostedSearchModel))
		})
	}
}

func TestBuildToolSearchSupport_RouterKeyUsesThatProvidersBaseURL(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "openai-responses"},
		Providers: map[string]types.ProviderConfig{
			"gateway": {Type: "openai-responses", BaseURL: "https://gateway.example/v1"},
			"direct":  {Type: "openai-responses", BaseURL: "https://api.openai.com/v1"},
		},
	}
	support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))

	must.True(support("", hostedSearchModel).Supported, "the default provider keeps native search")
	must.True(support("openai-responses", hostedSearchModel).Supported, "a key naming the default provider's type keeps native search")
	must.True(support("direct", hostedSearchModel).Supported)
	must.False(support("gateway", hostedSearchModel).Supported)
}

func TestBuildToolSearchSupport_CustomBaseURLDoesNotAffectOtherProviderTypes(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example/v1"},
	}
	support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
	must.True(support("anthropic", "claude-sonnet-4-6").Supported)
}

func TestBuildToolSearchSupport_LogsEachDerivedGenericEndpointOnce(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "openai-responses"},
		Providers: map[string]types.ProviderConfig{
			"gateway": {Type: "openai-responses", BaseURL: "https://gateway.example/v1"},
		},
		ModelRouter: types.ModelRouterConfig{
			Type:              "dynamic",
			Provider:          "gateway",
			Model:             hostedSearchModel,
			CheapProvider:     "gateway",
			CheapModel:        "gpt-5.4-mini",
			ExpensiveProvider: "openai-responses",
			ExpensiveModel:    hostedSearchModel,
		},
	}
	var buf bytes.Buffer
	support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.NewTextHandler(&buf, nil)))
	support("gateway", hostedSearchModel)
	support("gateway", hostedSearchModel)

	must.Equal(1, strings.Count(buf.String(), "provider=gateway"))
	must.Equal(1, strings.Count(buf.String(), "\n"))
	must.Contains(buf.String(), "declare providers[gateway].endpointProfile openai only if this endpoint forwards OpenAI's hosted tool_search and defer_loading unchanged")
}

func TestBuildToolSearchSupport_DerivedGenericNoticeNamesTheDefaultProvidersField(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{
		Provider:    types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example/v1"},
		ModelRouter: types.ModelRouterConfig{Type: "static", Provider: "openai-responses", Model: hostedSearchModel},
	}
	var buf bytes.Buffer
	buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.NewTextHandler(&buf, nil)))

	must.Contains(buf.String(), "declare provider.endpointProfile openai only if")
}

func TestBuildToolSearchSupport_DerivedGenericLogsNothingForModelsWithoutHostedSearch(t *testing.T) {
	for _, tc := range []struct {
		providerType string
		model        string
	}{
		{"openai-responses", "gpt-4o"},
		{"openai-compatible", hostedSearchModel},
	} {
		t.Run(tc.providerType+"/"+tc.model, func(t *testing.T) {
			must := require.New(t)
			config := &types.RunConfig{
				Provider:    types.ProviderConfig{Type: tc.providerType, BaseURL: "https://gateway.example/v1"},
				ModelRouter: types.ModelRouterConfig{Type: "static", Provider: tc.providerType, Model: tc.model},
			}
			var buf bytes.Buffer
			buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.NewTextHandler(&buf, nil)))

			must.Empty(buf.String())
		})
	}
}

func TestBuildToolSearchSupport_DeclaredEndpointProfileLogsNothing(t *testing.T) {
	for _, profile := range []string{types.EndpointProfileGeneric, types.EndpointProfileAzureOpenAI, types.EndpointProfileOpenAI} {
		t.Run(profile, func(t *testing.T) {
			must := require.New(t)
			config := &types.RunConfig{
				Provider:    types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example/v1", EndpointProfile: profile},
				ModelRouter: types.ModelRouterConfig{Type: "static", Provider: "openai-responses", Model: hostedSearchModel},
			}
			var buf bytes.Buffer
			buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.NewTextHandler(&buf, nil)))

			must.Empty(buf.String())
		})
	}
}

func TestBuildToolSearchSupport_HarnessStrategyLogsNothing(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{
		Provider:    types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example/v1"},
		ModelRouter: types.ModelRouterConfig{Type: "static", Provider: "openai-responses", Model: hostedSearchModel},
		Tools:       types.ToolsConfig{LazyLoading: types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness}},
	}
	var buf bytes.Buffer
	buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.NewTextHandler(&buf, nil)))

	must.Empty(buf.String())
}

func mixedEndpointConfig() *types.RunConfig {
	return &types.RunConfig{
		Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example.com/v1"},
		Providers: map[string]types.ProviderConfig{
			"direct": {Type: "openai-responses", BaseURL: "https://relay.example.com/v1", EndpointProfile: types.EndpointProfileOpenAI},
			"azure":  {Type: "openai-responses", BaseURL: "https://example.openai.azure.com/openai/v1", EndpointProfile: types.EndpointProfileAzureOpenAI},
		},
	}
}

func TestBuildToolSearchSupport_MixedEndpointsResolvePerProvider(t *testing.T) {
	must := require.New(t)
	support := buildToolSearchSupport(mixedEndpointConfig(), quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))

	must.False(support("", hostedSearchModel).Supported, "the default provider derives generic")
	must.True(support("direct", hostedSearchModel).Supported, "direct declares openai")
	must.False(support("azure", hostedSearchModel).Supported, "azure-openai is unprobed")
}

func TestDescribeToolSearch_NamesEachProvidersEndpointProfile(t *testing.T) {
	loop := buildTestLoop(nil)
	registry := loop.Tools.(*tool.Registry)
	registerLazyMCPTool(registry, lazyMCPTool)
	registry.Register(builtins.ToolSearchTool(types.LazyToolLoadingConfig{}))
	loader := tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{MCPServers: []string{"github"}})
	config := mixedEndpointConfig()
	support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))

	for _, tc := range []struct {
		key  string
		want string
	}{
		{"", "the harness tool_search (endpoint profile generic)"},
		{"direct", "the native hosted tool search"},
		{"azure", "the harness tool_search (endpoint profile azure-openai)"},
	} {
		t.Run("key "+tc.key, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, describeToolSearch(config, quirks.DefaultRegistry(), loader, support, router.ModelSelection{Provider: tc.key, Model: hostedSearchModel}))
		})
	}
}

func TestDescribeToolSearch_NamesTheAllowListWhenNoProfileWouldHelp(t *testing.T) {
	loop := buildTestLoop(nil)
	registry := loop.Tools.(*tool.Registry)
	registerLazyMCPTool(registry, lazyMCPTool)
	registry.Register(builtins.ToolSearchTool(types.LazyToolLoadingConfig{}))
	loader := tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{MCPServers: []string{"github"}})
	const allowList = "the harness tool_search (model not in the native tool search allow-list)"

	for _, tc := range []struct {
		name         string
		providerType string
		model        string
		want         string
	}{
		{"responses model with hosted search", "openai-responses", "gpt-5.4", "the harness tool_search (endpoint profile generic)"},
		{"responses model without hosted search", "openai-responses", "gpt-4o", allowList},
		{"chat completions", "openai-compatible", "gpt-5.4", allowList},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			config := &types.RunConfig{Provider: types.ProviderConfig{Type: tc.providerType, BaseURL: "https://gateway.example.com/v1"}}
			support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
			must.Equal(tc.want, describeToolSearch(config, quirks.DefaultRegistry(), loader, support, router.ModelSelection{Provider: tc.providerType, Model: tc.model}))
		})
	}
}
