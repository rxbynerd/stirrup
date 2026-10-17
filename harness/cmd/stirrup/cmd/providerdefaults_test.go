package cmd

import (
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func resolveProviderDefaults(t *testing.T, base string, args ...string) (*types.RunConfig, error) {
	t.Helper()
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	sources := RunConfigSources{Cmd: cmd, Resolve: ResolveBase}
	if base != "" {
		sources.Stdin = strings.NewReader(base)
	}
	return BuildRunConfig(sources)
}

func TestProviderDefaults_FlagOnly(t *testing.T) {
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
			cfg, err := resolveProviderDefaults(t, "", tc.args...)
			if err != nil {
				t.Fatalf("BuildRunConfig: %v", err)
			}
			if cfg.Provider.APIKeyRef != tc.wantKeyRef {
				t.Errorf("Provider.APIKeyRef = %q, want %q", cfg.Provider.APIKeyRef, tc.wantKeyRef)
			}
			if cfg.ModelRouter.Model != tc.wantModel {
				t.Errorf("ModelRouter.Model = %q, want %q", cfg.ModelRouter.Model, tc.wantModel)
			}
		})
	}
}

func TestProviderDefaults_MissingModelNamesProviderAndFlag(t *testing.T) {
	for _, provider := range []string{"openai-responses", "openai-compatible", "bedrock", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			_, err := resolveProviderDefaults(t, "", "--provider", provider)
			if err == nil {
				t.Fatal("expected an error for a missing --model")
			}
			for _, want := range []string{"--model", "--provider=" + provider} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should contain %q", err, want)
				}
			}
			if code := classifyExitCode(err); code != exitValidation {
				t.Errorf("exit code = %d, want %d", code, exitValidation)
			}
		})
	}
}

func TestProviderDefaults_ConfigFileValuesWin(t *testing.T) {
	// The file base is anthropic with an explicit key and model; neither
	// default may replace them, with or without a provider flag.
	cfg, err := resolveProviderDefaults(t, minimalRunConfigJSON(t))
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Provider.APIKeyRef != "secret://FILE_KEY" || cfg.ModelRouter.Model != "claude-sonnet-4-6" {
		t.Errorf("file values changed: keyRef=%q model=%q", cfg.Provider.APIKeyRef, cfg.ModelRouter.Model)
	}

	cfg, err = resolveProviderDefaults(t, minimalRunConfigJSON(t), "--model", "claude-opus-4-7", "--api-key-ref", "secret://FLAG_KEY")
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Provider.APIKeyRef != "secret://FLAG_KEY" || cfg.ModelRouter.Model != "claude-opus-4-7" {
		t.Errorf("flags did not win: keyRef=%q model=%q", cfg.Provider.APIKeyRef, cfg.ModelRouter.Model)
	}
}

func TestProviderDefaults_FileProviderDrivesDefaults(t *testing.T) {
	base := `{"provider":{"type":"openai-responses"},"modelRouter":{"type":"static","provider":"openai-responses"}}`

	cfg, err := resolveProviderDefaults(t, base, "--model", "gpt-5.4-nano")
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Provider.APIKeyRef != "secret://OPENAI_API_KEY" {
		t.Errorf("Provider.APIKeyRef = %q, want secret://OPENAI_API_KEY", cfg.Provider.APIKeyRef)
	}

	if _, err := resolveProviderDefaults(t, base); err == nil || !strings.Contains(err.Error(), "--provider=openai-responses") {
		t.Errorf("expected a missing-model error naming the file's provider, got %v", err)
	}
}

func TestProviderDefaults_EmptyKeyFlagOptsOutOnFileBase(t *testing.T) {
	base := `{"provider":{"type":"openai-responses"},"modelRouter":{"type":"static","model":"gpt-5.4-nano"}}`
	cfg, err := resolveProviderDefaults(t, base, "--api-key-ref=")
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Provider.APIKeyRef != "" {
		t.Errorf("Provider.APIKeyRef = %q, want empty", cfg.Provider.APIKeyRef)
	}
}

func TestProviderDefaults_KeylessDocumentGatewayGetsNoKeyDefault(t *testing.T) {
	for _, providerType := range []string{"openai-compatible", "openai-responses"} {
		t.Run(providerType, func(t *testing.T) {
			base := `{"provider":{"type":"` + providerType + `","baseUrl":"http://localhost:1234/v1"},"modelRouter":{"type":"static","model":"qwen"}}`
			cfg, err := resolveProviderDefaults(t, base)
			if err != nil {
				t.Fatalf("BuildRunConfig: %v", err)
			}
			if cfg.Provider.APIKeyRef != "" {
				t.Errorf("Provider.APIKeyRef = %q, want empty for a keyless gateway document", cfg.Provider.APIKeyRef)
			}
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
			cfg, err := resolveProviderDefaults(t, tc.base, tc.args...)
			if err != nil {
				t.Fatalf("BuildRunConfig: %v", err)
			}
			if cfg.Provider.APIKeyRef != tc.want {
				t.Errorf("Provider.APIKeyRef = %q, want %q", cfg.Provider.APIKeyRef, tc.want)
			}
		})
	}
}

func TestProviderDefaults_FlagBuiltGatewayKeepsKeyDefault(t *testing.T) {
	cfg, err := resolveProviderDefaults(t, "",
		"--provider", "openai-compatible", "--model", "qwen", "--base-url", "http://localhost:1234/v1")
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Provider.APIKeyRef != "secret://OPENAI_API_KEY" {
		t.Errorf("Provider.APIKeyRef = %q, want secret://OPENAI_API_KEY for a flag-built config", cfg.Provider.APIKeyRef)
	}
}

func TestProviderDefaults_CredentialBlockSuppressesKeyDefault(t *testing.T) {
	cfg, err := resolveProviderDefaults(t, "",
		"--provider", "openai-responses", "--model", "m",
		"--openai-identity-provider-id", "idp_1", "--openai-service-account-id", "sa_1")
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Provider.APIKeyRef != "" {
		t.Errorf("Provider.APIKeyRef = %q, want empty under WIF", cfg.Provider.APIKeyRef)
	}
}

func TestProviderDefaults_NonStaticRouterLeftToValidation(t *testing.T) {
	base := `{"provider":{"type":"openai-compatible"},"modelRouter":{"type":"dynamic","cheapModel":"a","expensiveModel":"b"}}`
	cfg, err := resolveProviderDefaults(t, base)
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.ModelRouter.Model != "" {
		t.Errorf("ModelRouter.Model = %q, want it untouched", cfg.ModelRouter.Model)
	}
}

func TestProviderDefaults_GeminiRejectsClaudeModel(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--provider", "gemini", "--model", "claude-sonnet-4-6",
		"--gcp-project", "p", "--prompt", "x",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err == nil || !strings.Contains(err.Error(), "Anthropic model name") {
		t.Fatalf("expected the claude- rejection, got %v", err)
	}
}
