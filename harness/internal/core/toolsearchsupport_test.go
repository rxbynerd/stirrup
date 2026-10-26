package core

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

const hostedSearchModel = "gpt-5.5"

func TestBuildToolSearchSupport_CustomResponsesBaseURLUsesHarnessSearch(t *testing.T) {
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

func TestBuildToolSearchSupport_DefaultResponsesBaseURLKeepsNative(t *testing.T) {
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
			must.True(support("", hostedSearchModel).Supported)
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

func TestBuildToolSearchSupport_LogsEachCustomEndpointOnce(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "openai-responses"},
		Providers: map[string]types.ProviderConfig{
			"gateway": {Type: "openai-responses", BaseURL: "https://gateway.example/v1"},
		},
	}
	var buf bytes.Buffer
	support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.NewTextHandler(&buf, nil)))
	support("gateway", hostedSearchModel)
	support("gateway", hostedSearchModel)

	must.Equal(1, strings.Count(buf.String(), "provider=gateway"))
	must.Equal(1, strings.Count(buf.String(), "\n"))
}

func TestBuildToolSearchSupport_HarnessStrategyLogsNothing(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example/v1"},
		Tools:    types.ToolsConfig{LazyLoading: types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness}},
	}
	var buf bytes.Buffer
	buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.NewTextHandler(&buf, nil)))

	must.Empty(buf.String())
}
