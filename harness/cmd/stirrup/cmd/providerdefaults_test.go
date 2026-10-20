package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func resolveProviderDefaults(t *testing.T, base string, args ...string) (*types.RunConfig, error) {
	t.Helper()
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags(args))
	sources := RunConfigSources{Cmd: cmd, Resolve: ResolveBase}
	if base != "" {
		sources.Stdin = strings.NewReader(base)
	}
	return BuildRunConfig(sources)
}

func TestProviderDefaults_FlagOnlyKeyRefAndModel(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantKeyRef string
		wantModel  string
	}{
		{"anthropic keeps the historical pair", []string{"--provider", "anthropic"}, "secret://ANTHROPIC_API_KEY", "claude-sonnet-4-6"},
		{"no provider flag is anthropic", nil, "secret://ANTHROPIC_API_KEY", "claude-sonnet-4-6"},
		{"openai-responses defaults to the OpenAI key", []string{"--provider", "openai-responses", "--model", "gpt-5.4-nano"}, "secret://OPENAI_API_KEY", "gpt-5.4-nano"},
		{"openai-compatible defaults to the OpenAI key", []string{"--provider", "openai-compatible", "--model", "qwen"}, "secret://OPENAI_API_KEY", "qwen"},
		{"explicit key reference wins", []string{"--provider", "openai-responses", "--model", "m", "--api-key-ref", "secret://OPENAI_KEY"}, "secret://OPENAI_KEY", "m"},
		{"empty key reference opts out of the default", []string{"--provider", "openai-compatible", "--model", "m", "--api-key-ref="}, "", "m"},
		{"bedrock carries no key reference", []string{"--provider", "bedrock", "--model", "anthropic.claude-sonnet-4-6-v1:0"}, "", "anthropic.claude-sonnet-4-6-v1:0"},
		{"gemini carries no key reference", []string{"--provider", "gemini", "--model", "gemini-3.1-pro-preview", "--gcp-project", "p"}, "", "gemini-3.1-pro-preview"},
		{"explicit anthropic model wins", []string{"--model", "claude-opus-4-7"}, "secret://ANTHROPIC_API_KEY", "claude-opus-4-7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg, err := resolveProviderDefaults(t, "", tc.args...)
			must.NoError(err)
			must.Equal(tc.wantKeyRef, cfg.Provider.APIKeyRef)
			must.Equal(tc.wantModel, cfg.ModelRouter.Model)
		})
	}
}

func TestProviderDefaults_MissingModelNamesProviderAndFlag(t *testing.T) {
	for _, provider := range []string{"openai-responses", "openai-compatible", "bedrock", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			must := require.New(t)
			_, err := resolveProviderDefaults(t, "", "--provider", provider)
			must.Error(err)
			must.ErrorContains(err, "--model")
			must.ErrorContains(err, "--provider="+provider)
			must.Equal(exitValidation, classifyExitCode(err))
		})
	}
}

func TestProviderDefaults_ConfigFileValuesWin(t *testing.T) {
	// The file base is anthropic with an explicit key and model; neither
	// default may replace them, with or without a provider flag.
	t.Run("file values are kept", func(t *testing.T) {
		must := require.New(t)
		cfg, err := resolveProviderDefaults(t, minimalRunConfigJSON(t))
		must.NoError(err)
		must.Equal("secret://FILE_KEY", cfg.Provider.APIKeyRef)
		must.Equal("claude-sonnet-4-6", cfg.ModelRouter.Model)
	})

	t.Run("flags override file values", func(t *testing.T) {
		must := require.New(t)
		cfg, err := resolveProviderDefaults(t, minimalRunConfigJSON(t),
			"--model", "claude-opus-4-7", "--api-key-ref", "secret://FLAG_KEY")
		must.NoError(err)
		must.Equal("secret://FLAG_KEY", cfg.Provider.APIKeyRef)
		must.Equal("claude-opus-4-7", cfg.ModelRouter.Model)
	})
}

func TestProviderDefaults_FileProviderDrivesDefaults(t *testing.T) {
	must := require.New(t)
	base := `{"provider":{"type":"openai-responses"},"modelRouter":{"type":"static","provider":"openai-responses"}}`

	cfg, err := resolveProviderDefaults(t, base, "--model", "gpt-5.4-nano")
	must.NoError(err)
	must.Equal("secret://OPENAI_API_KEY", cfg.Provider.APIKeyRef)

	_, err = resolveProviderDefaults(t, base)
	must.ErrorContains(err, "--provider=openai-responses")
}

func TestProviderDefaults_EmptyKeyFlagOptsOutOnFileBase(t *testing.T) {
	must := require.New(t)
	base := `{"provider":{"type":"openai-responses"},"modelRouter":{"type":"static","model":"gpt-5.4-nano"}}`
	cfg, err := resolveProviderDefaults(t, base, "--api-key-ref=")
	must.NoError(err)
	must.Empty(cfg.Provider.APIKeyRef)
}

func TestProviderDefaults_KeylessDocumentGatewayGetsNoKeyDefault(t *testing.T) {
	for _, providerType := range []string{"openai-compatible", "openai-responses"} {
		t.Run(providerType, func(t *testing.T) {
			must := require.New(t)
			base := `{"provider":{"type":"` + providerType + `","baseUrl":"http://localhost:1234/v1"},"modelRouter":{"type":"static","model":"qwen"}}`
			cfg, err := resolveProviderDefaults(t, base)
			must.NoError(err)
			must.Empty(cfg.Provider.APIKeyRef, "a keyless gateway document must not get a key default")
		})
	}
}

func TestProviderDefaults_DocumentKeyDefaultKeptWithoutGateway(t *testing.T) {
	tests := []struct {
		name string
		base string
		args []string
		want string
	}{
		{
			"openai document without baseUrl",
			`{"provider":{"type":"openai-compatible"},"modelRouter":{"type":"static","model":"qwen"}}`,
			nil,
			"secret://OPENAI_API_KEY",
		},
		{
			"anthropic document with baseUrl",
			`{"provider":{"type":"anthropic","baseUrl":"https://gateway.example.com"},"modelRouter":{"type":"static","model":"claude-sonnet-4-6"}}`,
			nil,
			"secret://ANTHROPIC_API_KEY",
		},
		{
			"openai document with baseUrl keeps its own key",
			`{"provider":{"type":"openai-compatible","baseUrl":"http://localhost:1234/v1","apiKeyRef":"secret://GATEWAY_KEY"},"modelRouter":{"type":"static","model":"qwen"}}`,
			nil,
			"secret://GATEWAY_KEY",
		},
		{
			"explicit key flag over a gateway document",
			`{"provider":{"type":"openai-compatible","baseUrl":"http://localhost:1234/v1"},"modelRouter":{"type":"static","model":"qwen"}}`,
			[]string{"--api-key-ref", "secret://FLAG_KEY"},
			"secret://FLAG_KEY",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg, err := resolveProviderDefaults(t, tc.base, tc.args...)
			must.NoError(err)
			must.Equal(tc.want, cfg.Provider.APIKeyRef)
		})
	}
}

func TestProviderDefaults_FlagBuiltGatewayKeepsKeyDefault(t *testing.T) {
	must := require.New(t)
	cfg, err := resolveProviderDefaults(t, "",
		"--provider", "openai-compatible", "--model", "qwen", "--base-url", "http://localhost:1234/v1")
	must.NoError(err)
	must.Equal("secret://OPENAI_API_KEY", cfg.Provider.APIKeyRef)
}

func TestProviderDefaults_CredentialBlockSuppressesKeyDefault(t *testing.T) {
	must := require.New(t)
	cfg, err := resolveProviderDefaults(t, "",
		"--provider", "openai-responses", "--model", "m",
		"--openai-identity-provider-id", "idp_1", "--openai-service-account-id", "sa_1")
	must.NoError(err)
	must.Empty(cfg.Provider.APIKeyRef, "no key default applies under workload identity federation")
}

func TestProviderDefaults_NonStaticRouterLeftToValidation(t *testing.T) {
	must := require.New(t)
	base := `{"provider":{"type":"openai-compatible"},"modelRouter":{"type":"dynamic","cheapModel":"a","expensiveModel":"b"}}`
	cfg, err := resolveProviderDefaults(t, base)
	must.NoError(err)
	must.Empty(cfg.ModelRouter.Model)
}

func TestProviderDefaults_GeminiRejectsClaudeModel(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--provider", "gemini", "--model", "claude-sonnet-4-6",
		"--gcp-project", "p", "--prompt", "x",
	}))
	_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	must.ErrorContains(err, "Anthropic model name")
}

// TestProviderDefaults_ReplayCarriesNoKeyAndLabelsTheModel pins that the
// replay provider gets no key reference and a placeholder model, so a
// recorded run needs only --provider and --replay-file.
func TestProviderDefaults_ReplayCarriesNoKeyAndLabelsTheModel(t *testing.T) {
	must := require.New(t)
	cfg, err := resolveProviderDefaults(t, "", "--provider", "replay", "--replay-file", "run.jsonl")
	must.NoError(err)
	must.Empty(cfg.Provider.APIKeyRef)
	must.Equal("replay", cfg.ModelRouter.Model)
	must.Equal("run.jsonl", cfg.Provider.ReplayFile)
}
