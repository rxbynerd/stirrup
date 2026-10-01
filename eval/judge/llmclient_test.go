package judge

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func TestResolveLLMConfig_EndpointPolicy(t *testing.T) {
	cases := []struct {
		name    string
		cfg     types.JudgeLLMConfig
		wantRef string
		wantErr string
	}{
		{name: "default host gets the default key", cfg: types.JudgeLLMConfig{Model: "m"}, wantRef: DefaultLLMKeyRef},
		{name: "explicit default host gets the default key", cfg: types.JudgeLLMConfig{Model: "m", BaseURL: "https://API.anthropic.com./"}, wantRef: DefaultLLMKeyRef},
		{name: "anthropic gateway without a ref", cfg: types.JudgeLLMConfig{Model: "m", BaseURL: "https://gw.example/v1"}, wantErr: "api_key_ref is required"},
		{name: "lookalike host without a ref", cfg: types.JudgeLLMConfig{Model: "m", BaseURL: "https://api.anthropic.com.evil.example"}, wantErr: "api_key_ref is required"},
		{name: "anthropic gateway with a ref", cfg: types.JudgeLLMConfig{Model: "m", BaseURL: "https://gw.example/v1", APIKeyRef: "secret://GW"}, wantRef: "secret://GW"},
		{name: "http with key to a public address", cfg: types.JudgeLLMConfig{Model: "m", BaseURL: "http://8.8.8.8/v1", APIKeyRef: "secret://GW"}, wantErr: "use https"},
		{name: "http with key to loopback", cfg: types.JudgeLLMConfig{Model: "m", BaseURL: "http://127.0.0.1:8001", APIKeyRef: "secret://GW"}, wantRef: "secret://GW"},
		{name: "keyless openai-compatible over http", cfg: types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "http://8.8.8.8/v1"}},
		{name: "metadata endpoint", cfg: types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "http://169.254.169.254/v1"}, wantErr: "link-local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ResolveLLMConfig(&tc.cfg, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.APIKeyRef != tc.wantRef {
				t.Errorf("APIKeyRef = %q, want %q", cfg.APIKeyRef, tc.wantRef)
			}
		})
	}

	t.Run("invocation defaults follow the same rule", func(t *testing.T) {
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{BaseURL: "https://gw.example/v1"})
		if err == nil || !strings.Contains(err.Error(), "api_key_ref is required") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCheckEndpoint(t *testing.T) {
	resolvesTo := func(addrs ...string) func(context.Context, string, string) ([]netip.Addr, error) {
		return func(context.Context, string, string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, a := range addrs {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		}
	}
	cases := []struct {
		name    string
		cfg     types.JudgeLLMConfig
		lookup  func(context.Context, string, string) ([]netip.Addr, error)
		wantErr string
	}{
		{name: "https to a public host", cfg: types.JudgeLLMConfig{BaseURL: "https://gw.example/v1", APIKeyRef: "secret://K"}, lookup: resolvesTo("93.184.216.34")},
		{name: "http with key to a LAN host", cfg: types.JudgeLLMConfig{BaseURL: "http://vllm.lan:8000/v1", APIKeyRef: "secret://K"}, lookup: resolvesTo("192.168.1.20", "fd12::20")},
		{name: "http with key to a public host", cfg: types.JudgeLLMConfig{BaseURL: "http://gw.example/v1", APIKeyRef: "secret://K"}, lookup: resolvesTo("93.184.216.34"), wantErr: "use https"},
		{name: "http with key to a host with one public address", cfg: types.JudgeLLMConfig{BaseURL: "http://gw.example/v1", APIKeyRef: "secret://K"}, lookup: resolvesTo("10.0.0.1", "93.184.216.34"), wantErr: "use https"},
		{name: "http without key to a public host", cfg: types.JudgeLLMConfig{BaseURL: "http://gw.example/v1"}, lookup: resolvesTo("93.184.216.34")},
		{name: "host resolving to metadata", cfg: types.JudgeLLMConfig{BaseURL: "https://gw.example/v1"}, lookup: resolvesTo("169.254.169.254"), wantErr: "link-local"},
		{name: "host resolving to unspecified", cfg: types.JudgeLLMConfig{BaseURL: "https://gw.example/v1"}, lookup: resolvesTo("::"), wantErr: "unspecified"},
		{name: "host resolving to nothing", cfg: types.JudgeLLMConfig{BaseURL: "https://gw.example/v1"}, lookup: resolvesTo(), wantErr: "no address"},
		{
			name: "lookup failure", cfg: types.JudgeLLMConfig{BaseURL: "https://gw.example/v1"},
			lookup:  func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("no such host") },
			wantErr: "no such host",
		},
		{name: "literal address skips lookup", cfg: types.JudgeLLMConfig{BaseURL: "http://169.254.169.254/v1"}, wantErr: "link-local"},
		{name: "default endpoint skips lookup", cfg: types.JudgeLLMConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := tc.lookup
			if lookup == nil {
				lookup = func(context.Context, string, string) ([]netip.Addr, error) {
					t.Error("lookup called")
					return nil, errors.New("unexpected lookup")
				}
			}
			err := checkEndpoint(context.Background(), tc.cfg, lookup)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want accepted", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewClient_RefusesForbiddenAddressesAtDialTime(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	t.Cleanup(ok.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ok.URL, "http://"))

	cases := []struct {
		name, baseURL, key, wantErr string
	}{
		{name: "loopback", baseURL: ok.URL, key: "sk-test-0123456789"},
		{name: "localhost resolves to loopback", baseURL: "http://localhost:" + port, key: "sk-test-0123456789"},
		{name: "metadata address", baseURL: "http://169.254.169.254", key: "sk-test-0123456789", wantErr: "link-local"},
		{name: "unspecified ipv4", baseURL: "http://0.0.0.0:" + port, wantErr: "unspecified"},
		{name: "unspecified ipv6", baseURL: "http://[::]:" + port, wantErr: "unspecified"},
		{name: "cleartext key to a public address", baseURL: "http://192.0.2.1:9", key: "sk-test-0123456789", wantErr: "use https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m", BaseURL: tc.baseURL, TimeoutSeconds: 5}, tc.key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want the call to reach the server", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckDialAddress(t *testing.T) {
	for _, address := range []string{"169.254.169.254:80", "[fe80::1%lo0]:443", "[fd00:ec2::254]:80", "0.0.0.0:443", "no-port", "host.example:443"} {
		if err := checkDialAddress(address, false); err == nil {
			t.Errorf("checkDialAddress(%q) accepted", address)
		}
	}
	for _, address := range []string{"127.0.0.1:443", "[::1]:80", "10.0.0.2:8000", "93.184.216.34:443"} {
		if err := checkDialAddress(address, false); err != nil {
			t.Errorf("checkDialAddress(%q) = %v", address, err)
		}
	}
	if err := checkDialAddress("93.184.216.34:80", true); err == nil {
		t.Error("cleartext key to a public address accepted")
	}
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
