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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestResolveLLMConfig_DefaultsAndExplicitBlocks(t *testing.T) {
	temp := 0.1

	t.Run("built-in default", func(t *testing.T) {
		must := require.New(t)
		cfg, err := ResolveLLMConfig(nil, nil)
		must.NoError(err)
		must.Equal("anthropic", cfg.Provider)
		must.Equal("claude-haiku-5-5", cfg.Model)
		must.Equal("secret://ANTHROPIC_API_KEY", cfg.APIKeyRef)
	})

	t.Run("anthropic defaults fill only missing fields", func(t *testing.T) {
		must := require.New(t)
		cfg, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Model: "claude-other"})
		must.NoError(err)
		must.Equal("claude-other", cfg.Model)
		must.Equal("secret://ANTHROPIC_API_KEY", cfg.APIKeyRef)
	})

	t.Run("explicit block ignores defaults", func(t *testing.T) {
		must := require.New(t)
		explicit := &types.JudgeLLMConfig{Model: "explicit", Temperature: &temp}
		defaults := &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "default", BaseURL: "https://gw.example/v1"}
		cfg, err := ResolveLLMConfig(explicit, defaults)
		must.NoError(err)
		must.Equal("anthropic", cfg.Provider)
		must.Equal("explicit", cfg.Model)
		must.Empty(cfg.BaseURL)
		must.NotNil(cfg.Temperature)
	})

	t.Run("explicit block must name a model", func(t *testing.T) {
		must := require.New(t)
		_, err := ResolveLLMConfig(&types.JudgeLLMConfig{Provider: "anthropic"}, nil)
		must.Error(err, "an explicit block does not inherit the built-in model")
	})

	t.Run("anthropic key is never defaulted for another provider", func(t *testing.T) {
		must := require.New(t)
		cfg, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://gw.example/v1"})
		must.NoError(err)
		must.Empty(cfg.APIKeyRef, "no Anthropic key may reach another endpoint")
	})

	t.Run("anthropic model is never defaulted for another provider", func(t *testing.T) {
		must := require.New(t)
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: "openai-compatible", BaseURL: "https://gw.example/v1"})
		must.ErrorContains(err, "model is required")
	})

	t.Run("openai-compatible requires a base URL", func(t *testing.T) {
		must := require.New(t)
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m"})
		must.ErrorContains(err, "base_url")
	})

	t.Run("invalid defaults are rejected", func(t *testing.T) {
		must := require.New(t)
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{APIKeyRef: "sk-raw-key"})
		must.ErrorContains(err, "secret://")
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
			must := require.New(t)
			cfg, err := ResolveLLMConfig(&tc.cfg, nil)
			if tc.wantErr != "" {
				must.ErrorContains(err, tc.wantErr)
				return
			}
			must.NoError(err)
			must.Equal(tc.wantRef, cfg.APIKeyRef)
		})
	}

	t.Run("invocation defaults follow the same rule", func(t *testing.T) {
		must := require.New(t)
		_, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{BaseURL: "https://gw.example/v1"})
		must.ErrorContains(err, "api_key_ref is required")
	})
}

func TestCheckEndpoint_LookupPolicy(t *testing.T) {
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
			check := assert.New(t)
			must := require.New(t)
			lookup := tc.lookup
			if lookup == nil {
				lookup = func(context.Context, string, string) ([]netip.Addr, error) {
					check.Fail("lookup must not run for an address literal or the default endpoint")
					return nil, errors.New("unexpected lookup")
				}
			}
			err := checkEndpoint(context.Background(), tc.cfg, lookup)
			if tc.wantErr != "" {
				must.ErrorContains(err, tc.wantErr)
				return
			}
			must.NoError(err)
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
			must := require.New(t)
			c, err := NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m", BaseURL: tc.baseURL, TimeoutSeconds: 5}, tc.key)
			must.NoError(err)
			_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			if tc.wantErr != "" {
				must.ErrorContains(err, tc.wantErr)
				return
			}
			must.NoError(err, "the call should reach the server")
		})
	}
}

func TestCheckDialAddress_IPPolicy(t *testing.T) {
	cases := []struct {
		name        string
		address     string
		keyOverHTTP bool
		wantErr     bool
	}{
		{name: "metadata IPv4 address", address: "169.254.169.254:80", wantErr: true},
		{name: "link-local IPv6 address", address: "[fe80::1%lo0]:443", wantErr: true},
		{name: "metadata IPv6 address", address: "[fd00:ec2::254]:80", wantErr: true},
		{name: "unspecified IPv4 address", address: "0.0.0.0:443", wantErr: true},
		{name: "address without port", address: "no-port", wantErr: true},
		{name: "host name", address: "host.example:443", wantErr: true},
		{name: "loopback IPv4 address", address: "127.0.0.1:443"},
		{name: "loopback IPv6 address", address: "[::1]:80"},
		{name: "private IPv4 address", address: "10.0.0.2:8000"},
		{name: "public IPv4 address", address: "93.184.216.34:443"},
		{name: "cleartext key to public address", address: "93.184.216.34:80", keyOverHTTP: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := checkDialAddress(tc.address, tc.keyOverHTTP)
			if tc.wantErr {
				must.Error(err)
				return
			}
			must.NoError(err)
		})
	}
}

func TestJoinEndpoint_JoinsBaseAndPath(t *testing.T) {
	cases := []struct{ name, base, path, want string }{
		{name: "bare host", base: "https://api.anthropic.com", path: "/v1/messages", want: "https://api.anthropic.com/v1/messages"},
		{name: "trailing slash on base", base: "https://api.anthropic.com/", path: "/v1/messages", want: "https://api.anthropic.com/v1/messages"},
		{name: "base with path prefix", base: "https://openrouter.ai/api/v1", path: "/chat/completions", want: "https://openrouter.ai/api/v1/chat/completions"},
		{name: "local endpoint with trailing slash", base: "http://localhost:8001/v1/", path: "/chat/completions", want: "http://localhost:8001/v1/chat/completions"},
		{name: "query string stays after the path", base: "https://gw.example/v1?api-version=2", path: "/chat/completions", want: "https://gw.example/v1/chat/completions?api-version=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := joinEndpoint(tc.base, tc.path)
			must.NoError(err)
			must.Equal(tc.want, got)
		})
	}
}

func TestNewClient_DispatchesOnProvider(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		must := require.New(t)
		client, err := NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m"}, "k")
		must.NoError(err)
		must.IsType((*anthropicClient)(nil), client)
	})

	t.Run("openai-compatible", func(t *testing.T) {
		must := require.New(t)
		client, err := NewClient(types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://gw.example/v1"}, "")
		must.NoError(err)
		must.IsType((*openaiClient)(nil), client)
	})

	t.Run("unsupported provider", func(t *testing.T) {
		must := require.New(t)
		_, err := NewClient(types.JudgeLLMConfig{Provider: "bedrock", Model: "m"}, "k")
		must.Error(err)
	})
}

func TestNewClient_AppliesConfiguredTimeout(t *testing.T) {
	t.Run("configured timeout", func(t *testing.T) {
		must := require.New(t)
		c, err := NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m", TimeoutSeconds: 7}, "k")
		must.NoError(err)
		must.Equal(7*time.Second, c.(*anthropicClient).http.Timeout)
	})

	t.Run("default timeout", func(t *testing.T) {
		must := require.New(t)
		c, err := NewClient(types.JudgeLLMConfig{Provider: "anthropic", Model: "m"}, "k")
		must.NoError(err)
		must.Equal(30*time.Second, c.(*anthropicClient).http.Timeout)
	})
}

func TestResolveSecretRef_ReadsEnvAndFileRefs(t *testing.T) {
	must := require.New(t)
	t.Setenv("JUDGE_TEST_KEY", "  env-secret\n")
	keyFile := filepath.Join(t.TempDir(), "key")
	must.NoError(os.WriteFile(keyFile, []byte("file-secret\n"), 0o600))

	cases := []struct{ name, ref, want string }{
		{name: "environment variable", ref: "secret://JUDGE_TEST_KEY", want: "env-secret"},
		{name: "file", ref: "secret://file://" + keyFile, want: "file-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := resolveSecretRef(tc.ref)
			must.NoError(err)
			must.Equal(tc.want, got)
		})
	}
}

func TestResolveSecretRef_RejectsMalformedAndRawRefs(t *testing.T) {
	must := require.New(t)
	t.Setenv("JUDGE_EMPTY_KEY", "")
	emptyFile := filepath.Join(t.TempDir(), "empty")
	must.NoError(os.WriteFile(emptyFile, []byte(" \n"), 0o600))

	cases := []struct {
		name    string
		ref     string
		wantMsg string
	}{
		{name: "empty environment variable", ref: "secret://JUDGE_EMPTY_KEY", wantMsg: "JUDGE_EMPTY_KEY"},
		{name: "unset environment variable", ref: "secret://JUDGE_UNSET_KEY_XYZ", wantMsg: "JUDGE_UNSET_KEY_XYZ"},
		{name: "empty name", ref: "secret://", wantMsg: "names no secret"},
		{name: "empty file path", ref: "secret://file://", wantMsg: "names no file"},
		{name: "empty file", ref: "secret://file://" + emptyFile, wantMsg: "is empty"},
		{name: "missing file", ref: "secret://file:///no/such/file", wantMsg: "reading secret file"},
		{name: "unsupported scheme", ref: "secret://ssm:///prod/judge-key", wantMsg: "not supported"},
		{name: "key material as environment variable name", ref: "secret://sk-ant-api03-REALKEYMATERIAL", wantMsg: "environment variable name"},
		{name: "environment variable name starting with a digit", ref: "secret://9STARTS_WITH_DIGIT", wantMsg: "environment variable name"},
		{name: "environment variable name with a space", ref: "secret://HAS SPACE", wantMsg: "environment variable name"},
		{name: "raw literal key", ref: "sk-literal-key", wantMsg: "raw credentials are not permitted"},
		{name: "vault URI", ref: "vault://prod/judge", wantMsg: "raw credentials are not permitted"},
		{name: "https URL", ref: "https://example.com/secret-key-material1", wantMsg: "raw credentials are not permitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := resolveSecretRef(tc.ref)
			must.ErrorContains(err, tc.wantMsg)

			name := strings.TrimPrefix(tc.ref, "secret://")
			if name != "" && !strings.HasPrefix(name, "JUDGE_") && !strings.HasPrefix(name, "file://") {
				must.NotContains(err.Error(), name, "the error must not echo the reference")
			}
		})
	}
}
