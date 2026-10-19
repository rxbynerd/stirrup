package types

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJudgeLLMConfig_Validate(t *testing.T) {
	temp := func(v float64) *float64 { return &v }
	valid := JudgeLLMConfig{Model: "claude-haiku-5-5"}

	cases := []struct {
		name    string
		mutate  func(c *JudgeLLMConfig)
		wantErr string
	}{
		{name: "minimal anthropic", mutate: func(c *JudgeLLMConfig) {}},
		{name: "explicit anthropic with gateway", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderAnthropic
			c.BaseURL = "https://gateway.example.com"
			c.APIKeyRef = "secret://GATEWAY_KEY"
		}},
		{name: "openai-compatible complete", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderOpenAICompatible
			c.BaseURL = "https://openrouter.ai/api/v1"
			c.APIKeyRef = "secret://file:///run/secrets/key"
			c.Temperature = temp(0)
			c.TimeoutSeconds = JudgeMaxTimeoutSeconds
			c.StructuredOutput = JudgeStructuredPromptOnly
		}},
		{name: "unknown provider", mutate: func(c *JudgeLLMConfig) { c.Provider = "bedrock" }, wantErr: "provider"},
		{name: "missing model", mutate: func(c *JudgeLLMConfig) { c.Model = "  " }, wantErr: "model is required"},
		{name: "openai-compatible needs base url", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderOpenAICompatible
		}, wantErr: "base_url is required"},
		{name: "base url scheme", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "ftp://example.com" }, wantErr: "http or https"},
		{name: "base url host", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "https:///v1" }, wantErr: "include a host"},
		{name: "base url credentials", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "https://user:pw@example.com" }, wantErr: "must not embed credentials"},
		{name: "metadata host", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "http://Metadata.Google.Internal./v1" }, wantErr: "metadata"},
		{name: "metadata address", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "http://169.254.169.254/v1" }, wantErr: "link-local"},
		{name: "ipv6 metadata address", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "http://[fd00:ec2::254]/v1" }, wantErr: "metadata"},
		{name: "ipv4-mapped metadata address", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "https://[::ffff:169.254.169.254]/v1" }, wantErr: "link-local"},
		{name: "unspecified address", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "http://0.0.0.0:8080" }, wantErr: "unspecified"},
		{name: "unspecified ipv6 address", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "http://[::]:8080" }, wantErr: "unspecified"},
		{name: "multicast address", mutate: func(c *JudgeLLMConfig) { c.BaseURL = "http://224.0.0.1" }, wantErr: "multicast"},
		{name: "http with key to public address", mutate: func(c *JudgeLLMConfig) {
			c.BaseURL = "http://8.8.8.8/v1"
			c.APIKeyRef = "secret://K"
		}, wantErr: "use https"},
		{name: "http without key to public address", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderOpenAICompatible
			c.BaseURL = "http://8.8.8.8/v1"
		}},
		{name: "https with key to public address", mutate: func(c *JudgeLLMConfig) {
			c.BaseURL = "https://8.8.8.8/v1"
			c.APIKeyRef = "secret://K"
		}},
		{name: "http with key to loopback", mutate: func(c *JudgeLLMConfig) {
			c.BaseURL = "http://127.0.0.1:1234/v1"
			c.APIKeyRef = "secret://K"
		}},
		{name: "http with key to private address", mutate: func(c *JudgeLLMConfig) {
			c.BaseURL = "http://10.1.2.3:8000/v1"
			c.APIKeyRef = "secret://K"
		}},
		{name: "http with key to a hostname is checked at resolve time", mutate: func(c *JudgeLLMConfig) {
			c.BaseURL = "http://llm.example.com/v1"
			c.APIKeyRef = "secret://K"
		}},
		{name: "raw api key", mutate: func(c *JudgeLLMConfig) { c.APIKeyRef = "sk-live-abc" }, wantErr: "secret:// reference"},
		{name: "empty secret ref", mutate: func(c *JudgeLLMConfig) { c.APIKeyRef = "secret://" }, wantErr: "names no secret"},
		{name: "key pasted as a secret name", mutate: func(c *JudgeLLMConfig) { c.APIKeyRef = "secret://sk-ant-api03-REAL" }, wantErr: "environment variable name"},
		{name: "ssm secret ref", mutate: func(c *JudgeLLMConfig) { c.APIKeyRef = "secret://ssm:///prod/key" }, wantErr: "not supported"},
		{name: "empty file secret ref", mutate: func(c *JudgeLLMConfig) { c.APIKeyRef = "secret://file://" }, wantErr: "names no file"},
		{name: "timeout over cap", mutate: func(c *JudgeLLMConfig) { c.TimeoutSeconds = JudgeMaxTimeoutSeconds + 1 }, wantErr: "timeout_seconds"},
		{name: "negative timeout", mutate: func(c *JudgeLLMConfig) { c.TimeoutSeconds = -1 }, wantErr: "timeout_seconds"},
		{name: "negative input cap", mutate: func(c *JudgeLLMConfig) { c.MaxInputBytes = -1 }, wantErr: "max_input_bytes"},
		{name: "negative max tokens", mutate: func(c *JudgeLLMConfig) { c.MaxTokens = -1 }, wantErr: "max_tokens"},
		{name: "temperature too high", mutate: func(c *JudgeLLMConfig) { c.Temperature = temp(2.5) }, wantErr: "temperature"},
		{name: "temperature negative", mutate: func(c *JudgeLLMConfig) { c.Temperature = temp(-0.1) }, wantErr: "temperature"},
		{name: "unknown structured output", mutate: func(c *JudgeLLMConfig) { c.StructuredOutput = "tool" }, wantErr: "structured_output"},
		{name: "decision minimal", mutate: func(c *JudgeLLMConfig) { c.Provider = JudgeProviderDecision }},
		{name: "decision self-hosted", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderDecision
			c.BaseURL = "http://127.0.0.1:8000"
			c.AllowTruncated = true
		}},
		{name: "decision rejects temperature", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderDecision
			c.Temperature = temp(0)
		}, wantErr: "temperature is not supported"},
		{name: "decision rejects max tokens", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderDecision
			c.MaxTokens = 100
		}, wantErr: "max_tokens is not supported"},
		{name: "decision rejects structured output", mutate: func(c *JudgeLLMConfig) {
			c.Provider = JudgeProviderDecision
			c.StructuredOutput = JudgeStructuredJSONSchema
		}, wantErr: "structured_output is not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestCheckJudgeEndpointAddr_ClassifiesAddresses(t *testing.T) {
	cases := []struct {
		name        string
		addr        string
		keyOverHTTP bool
		wantErr     string
	}{
		{name: "ipv4 loopback", addr: "127.0.0.1"},
		{name: "ipv6 loopback with key over http", addr: "::1", keyOverHTTP: true},
		{name: "10/8 private with key over http", addr: "10.0.0.5", keyOverHTTP: true},
		{name: "192.168/16 private with key over http", addr: "192.168.1.20", keyOverHTTP: true},
		{name: "172.16/12 private with key over http", addr: "172.16.0.1", keyOverHTTP: true},
		{name: "ipv6 unique local with key over http", addr: "fd12::1", keyOverHTTP: true},
		{name: "public ipv4", addr: "93.184.216.34"},
		{name: "public ipv4 with key over http", addr: "93.184.216.34", keyOverHTTP: true, wantErr: "use https"},
		{name: "public ipv6 with key over http", addr: "2606:4700::1", keyOverHTTP: true, wantErr: "use https"},
		{name: "metadata address", addr: "169.254.169.254", wantErr: "link-local"},
		{name: "ipv4-mapped metadata address", addr: "::ffff:169.254.169.254", wantErr: "link-local"},
		{name: "ipv4 link-local address", addr: "169.254.0.1", wantErr: "link-local"},
		{name: "ipv6 link-local address with zone", addr: "fe80::1%en0", wantErr: "link-local"},
		{name: "ipv6 metadata address", addr: "fd00:ec2::254", wantErr: "metadata"},
		{name: "ipv4 unspecified address", addr: "0.0.0.0", wantErr: "unspecified"},
		{name: "ipv6 unspecified address", addr: "::", wantErr: "unspecified"},
		{name: "ipv6 multicast address", addr: "ff02::1", wantErr: "multicast"},
		{name: "ipv4 multicast address", addr: "239.1.1.1", wantErr: "multicast"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckJudgeEndpointAddr(netip.MustParseAddr(tc.addr), tc.keyOverHTTP)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
	require.Error(t, CheckJudgeEndpointAddr(netip.Addr{}, false), "the zero address was accepted")
}

func TestJudgeLLMConfig_Validate_DoesNotEchoKeyRef(t *testing.T) {
	rejected := []struct{ name, ref string }{
		{name: "raw key", ref: "sk-ant-api03-REALKEYMATERIAL"},
		{name: "secret reference naming a raw key", ref: "secret://sk-ant-api03-REALKEYMATERIAL"},
		{name: "secret reference with spaces", ref: "secret://REAL KEY MATERIAL"},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			err := JudgeLLMConfig{Model: "m", APIKeyRef: tc.ref}.Validate()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "REAL", "error echoes the reference")
		})
	}

	accepted := []struct{ name, ref string }{
		{name: "environment variable name", ref: "secret://ANTHROPIC_API_KEY"},
		{name: "environment variable name with leading underscore", ref: "secret://_K2"},
		{name: "file secret", ref: "secret://file:///run/secrets/key"},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			require.NoError(t, JudgeLLMConfig{Model: "m", APIKeyRef: tc.ref}.Validate())
		})
	}
}

func TestJudgeLLMConfig_Validate_DoesNotEchoBaseURL(t *testing.T) {
	cases := []struct{ name, raw string }{
		{name: "credentials in url", raw: "https://user:hunter2@example.com"},
		{name: "malformed url", raw: "ht!tp://a b:hunter2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := JudgeLLMConfig{Model: "m", BaseURL: tc.raw}.Validate()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "hunter2", "error leaks the base URL credential")
		})
	}
}

func TestJudgeLLMConfig_EffectiveDefaults(t *testing.T) {
	var zero JudgeLLMConfig

	t.Run("zero value provider is anthropic", func(t *testing.T) {
		require.Equal(t, JudgeProviderAnthropic, zero.EffectiveProvider())
	})
	t.Run("zero value timeout is 30 seconds", func(t *testing.T) {
		require.Equal(t, 30, zero.EffectiveTimeoutSeconds())
	})
	t.Run("zero value input cap is 64 KiB", func(t *testing.T) {
		require.Equal(t, 65536, zero.EffectiveMaxInputBytes())
	})
	t.Run("zero value max tokens is 1024", func(t *testing.T) {
		require.Equal(t, 1024, zero.EffectiveMaxTokens())
	})
	t.Run("zero value structured output is json schema", func(t *testing.T) {
		require.Equal(t, JudgeStructuredJSONSchema, zero.EffectiveStructuredOutput()) //nolint:testifylint // enum constant, not an encoded payload
	})

	t.Run("explicit values are preserved", func(t *testing.T) {
		c := JudgeLLMConfig{Provider: JudgeProviderOpenAICompatible, TimeoutSeconds: 5, MaxInputBytes: 10, MaxTokens: 7, StructuredOutput: JudgeStructuredPromptOnly}
		require.Equal(t, JudgeProviderOpenAICompatible, c.EffectiveProvider())
		require.Equal(t, 5, c.EffectiveTimeoutSeconds())
		require.Equal(t, 10, c.EffectiveMaxInputBytes())
		require.Equal(t, 7, c.EffectiveMaxTokens())
		require.Equal(t, JudgeStructuredPromptOnly, c.EffectiveStructuredOutput())
	})
}

func TestEvalJudge_LLMWireShape(t *testing.T) {
	temp := 0.0
	j := EvalJudge{Type: "diff-review", Criteria: "c", LLM: &JudgeLLMConfig{
		Provider: JudgeProviderOpenAICompatible, Model: "m", BaseURL: "https://x/v1", APIKeyRef: "secret://K",
		Temperature: &temp, AllowTruncated: true,
	}}
	data, err := json.Marshal(j)
	require.NoError(t, err)

	wantFields := []struct{ name, want string }{
		{name: "llm key", want: `"llm":`},
		{name: "base url key", want: `"baseUrl":`},
		{name: "api key ref key", want: `"apiKeyRef":`},
		{name: "explicit zero temperature", want: `"temperature":0`},
		{name: "allow truncated flag", want: `"allowTruncated":true`},
	}
	for _, tc := range wantFields {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, string(data), tc.want)
		})
	}

	data, err = json.Marshal(EvalJudge{Type: "file-exists"})
	require.NoError(t, err)
	require.NotContains(t, string(data), `"llm"`, "non-LLM judge serialises an llm key")
}
