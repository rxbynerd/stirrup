package judge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestResolveLLMConfig(t *testing.T) {
	temp := 0.1

	t.Run("built-in default", func(t *testing.T) {
		cfg, err := ResolveLLMConfig(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "anthropic" || cfg.Model != "claude-haiku-4-5-20251001" || cfg.APIKeyRef != "secret://ANTHROPIC_API_KEY" {
			t.Errorf("cfg = %+v", cfg)
		}
	})

	t.Run("anthropic defaults fill only missing fields", func(t *testing.T) {
		cfg, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Model: "claude-other"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model != "claude-other" || cfg.APIKeyRef != "secret://ANTHROPIC_API_KEY" {
			t.Errorf("cfg = %+v", cfg)
		}
	})

	t.Run("explicit block ignores defaults", func(t *testing.T) {
		explicit := &types.JudgeLLMConfig{Model: "explicit", Temperature: &temp}
		defaults := &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "default", BaseURL: "https://gw.example/v1"}
		cfg, err := ResolveLLMConfig(explicit, defaults)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "anthropic" || cfg.Model != "explicit" || cfg.BaseURL != "" || cfg.Temperature == nil {
			t.Errorf("cfg = %+v", cfg)
		}
	})

	t.Run("explicit block must name a model", func(t *testing.T) {
		if _, err := ResolveLLMConfig(&types.JudgeLLMConfig{Provider: "anthropic"}, nil); err == nil {
			t.Error("expected an error: an explicit block does not inherit the built-in model")
		}
	})

	t.Run("anthropic key is never defaulted for another provider", func(t *testing.T) {
		cfg, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://gw.example/v1"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIKeyRef != "" {
			t.Errorf("APIKeyRef = %q, want empty so no Anthropic key reaches another endpoint", cfg.APIKeyRef)
		}
	})

	t.Run("anthropic model is never defaulted for another provider", func(t *testing.T) {
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: "openai-compatible", BaseURL: "https://gw.example/v1"})
		if err == nil || !strings.Contains(err.Error(), "model is required") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("openai-compatible requires a base URL", func(t *testing.T) {
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m"})
		if err == nil || !strings.Contains(err.Error(), "base_url") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("invalid defaults are rejected", func(t *testing.T) {
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{APIKeyRef: "sk-raw-key"})
		if err == nil || !strings.Contains(err.Error(), "secret://") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestJoinEndpoint(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://api.anthropic.com", "/v1/messages", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/", "/v1/messages", "https://api.anthropic.com/v1/messages"},
		{"https://openrouter.ai/api/v1", "/chat/completions", "https://openrouter.ai/api/v1/chat/completions"},
		{"http://localhost:8001/v1/", "/chat/completions", "http://localhost:8001/v1/chat/completions"},
		{"https://gw.example/v1?api-version=2", "/chat/completions", "https://gw.example/v1/chat/completions?api-version=2"},
	}
	for _, tc := range cases {
		got, err := joinEndpoint(tc.base, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("joinEndpoint(%q, %q) = %q, %v; want %q", tc.base, tc.path, got, err, tc.want)
		}
	}
}

func TestNewClientDispatchesOnProvider(t *testing.T) {
	anth, err := NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m"}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := anth.(*anthropicClient); !ok {
		t.Errorf("anthropic provider built %T", anth)
	}

	oa, err := NewClient(types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://gw.example/v1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := oa.(*openaiClient); !ok {
		t.Errorf("openai-compatible provider built %T", oa)
	}

	if _, err := NewClient(types.JudgeLLMConfig{Provider: "bedrock", Model: "m"}, "k"); err == nil {
		t.Error("expected an error for an unsupported provider")
	}
}

func TestNewClientAppliesConfiguredTimeout(t *testing.T) {
	c, err := NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m", TimeoutSeconds: 7}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.(*anthropicClient).http.Timeout.Seconds(); got != 7 {
		t.Errorf("timeout = %vs, want 7s", got)
	}

	c, err = NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m"}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.(*anthropicClient).http.Timeout.Seconds(); got != 30 {
		t.Errorf("default timeout = %vs, want 30s", got)
	}
}

func TestResolveSecretRef(t *testing.T) {
	t.Setenv("JUDGE_TEST_KEY", "  env-secret\n")
	t.Setenv("JUDGE_EMPTY_KEY", "")
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyFile := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(emptyFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	good := map[string]string{
		"secret://JUDGE_TEST_KEY":    "env-secret",
		"secret://file://" + keyFile: "file-secret",
	}
	for ref, want := range good {
		got, err := resolveSecretRef(ref)
		if err != nil || got != want {
			t.Errorf("resolveSecretRef(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}

	bad := map[string]string{
		"secret://JUDGE_EMPTY_KEY":       "JUDGE_EMPTY_KEY",
		"secret://JUDGE_UNSET_KEY_XYZ":   "JUDGE_UNSET_KEY_XYZ",
		"secret://":                      "empty environment variable name",
		"secret://file://":               "empty file path",
		"secret://file://" + emptyFile:   "is empty",
		"secret://file:///no/such/file":  "reading secret file",
		"sk-literal-key":                 "unknown secret reference scheme",
		"vault://prod/judge":             "unknown secret reference scheme",
		"https://example.com/secret-key": "unknown secret reference scheme",
	}
	for ref, wantMsg := range bad {
		got, err := resolveSecretRef(ref)
		if err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("resolveSecretRef(%q) = %q, %v; want error containing %q", ref, got, err, wantMsg)
		}
	}
}
