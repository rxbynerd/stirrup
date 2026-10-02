package types

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestJudgeLLMConfigValidate(t *testing.T) {
	temp := func(v float64) *float64 { return &v }
	valid := JudgeLLMConfig{Model: "claude-haiku-4-5-20251001"}

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
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckJudgeEndpointAddr(t *testing.T) {
	cases := []struct {
		addr        string
		keyOverHTTP bool
		wantErr     string
	}{
		{addr: "127.0.0.1"},
		{addr: "::1", keyOverHTTP: true},
		{addr: "10.0.0.5", keyOverHTTP: true},
		{addr: "192.168.1.20", keyOverHTTP: true},
		{addr: "172.16.0.1", keyOverHTTP: true},
		{addr: "fd12::1", keyOverHTTP: true},
		{addr: "93.184.216.34"},
		{addr: "93.184.216.34", keyOverHTTP: true, wantErr: "use https"},
		{addr: "2606:4700::1", keyOverHTTP: true, wantErr: "use https"},
		{addr: "169.254.169.254", wantErr: "link-local"},
		{addr: "::ffff:169.254.169.254", wantErr: "link-local"},
		{addr: "169.254.0.1", wantErr: "link-local"},
		{addr: "fe80::1%en0", wantErr: "link-local"},
		{addr: "fd00:ec2::254", wantErr: "metadata"},
		{addr: "0.0.0.0", wantErr: "unspecified"},
		{addr: "::", wantErr: "unspecified"},
		{addr: "ff02::1", wantErr: "multicast"},
		{addr: "239.1.1.1", wantErr: "multicast"},
	}
	for _, tc := range cases {
		err := CheckJudgeEndpointAddr(netip.MustParseAddr(tc.addr), tc.keyOverHTTP)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s (keyOverHTTP=%v): %v, want accepted", tc.addr, tc.keyOverHTTP, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s (keyOverHTTP=%v): %v, want error containing %q", tc.addr, tc.keyOverHTTP, err, tc.wantErr)
		}
	}
	if err := CheckJudgeEndpointAddr(netip.Addr{}, false); err == nil {
		t.Error("the zero address was accepted")
	}
}

func TestJudgeLLMConfigValidateDoesNotEchoKeyRef(t *testing.T) {
	for _, ref := range []string{"sk-ant-api03-REALKEYMATERIAL", "secret://sk-ant-api03-REALKEYMATERIAL", "secret://REAL KEY MATERIAL"} {
		err := JudgeLLMConfig{Model: "m", APIKeyRef: ref}.Validate()
		if err == nil {
			t.Fatalf("api_key_ref %q accepted", ref)
		}
		if strings.Contains(err.Error(), "REAL") {
			t.Errorf("error echoes the reference: %v", err)
		}
	}
	for _, ref := range []string{"secret://ANTHROPIC_API_KEY", "secret://_K2", "secret://file:///run/secrets/key"} {
		if err := (JudgeLLMConfig{Model: "m", APIKeyRef: ref}).Validate(); err != nil {
			t.Errorf("api_key_ref %q rejected: %v", ref, err)
		}
	}
}

func TestJudgeLLMConfigValidateDoesNotEchoBaseURL(t *testing.T) {
	for _, raw := range []string{"https://user:hunter2@example.com", "ht!tp://a b:hunter2"} {
		err := JudgeLLMConfig{Model: "m", BaseURL: raw}.Validate()
		if err == nil {
			t.Fatalf("base url %q accepted", raw)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the base URL credential: %v", err)
		}
	}
}

func TestJudgeLLMConfigEffectiveDefaults(t *testing.T) {
	var c JudgeLLMConfig
	if got := c.EffectiveProvider(); got != JudgeProviderAnthropic {
		t.Errorf("EffectiveProvider() = %q", got)
	}
	if got := c.EffectiveTimeoutSeconds(); got != 30 {
		t.Errorf("EffectiveTimeoutSeconds() = %d, want 30", got)
	}
	if got := c.EffectiveMaxInputBytes(); got != 65536 {
		t.Errorf("EffectiveMaxInputBytes() = %d, want 65536", got)
	}
	if got := c.EffectiveMaxTokens(); got != 1024 {
		t.Errorf("EffectiveMaxTokens() = %d, want 1024", got)
	}
	if got := c.EffectiveStructuredOutput(); got != JudgeStructuredJSONSchema {
		t.Errorf("EffectiveStructuredOutput() = %q", got)
	}

	c = JudgeLLMConfig{Provider: JudgeProviderOpenAICompatible, TimeoutSeconds: 5, MaxInputBytes: 10, MaxTokens: 7, StructuredOutput: JudgeStructuredPromptOnly}
	if c.EffectiveProvider() != JudgeProviderOpenAICompatible || c.EffectiveTimeoutSeconds() != 5 ||
		c.EffectiveMaxInputBytes() != 10 || c.EffectiveMaxTokens() != 7 || c.EffectiveStructuredOutput() != JudgeStructuredPromptOnly {
		t.Errorf("explicit values not preserved: %+v", c)
	}
}

func TestEvalJudgeLLMWireShape(t *testing.T) {
	temp := 0.0
	j := EvalJudge{Type: "diff-review", Criteria: "c", LLM: &JudgeLLMConfig{
		Provider: JudgeProviderOpenAICompatible, Model: "m", BaseURL: "https://x/v1", APIKeyRef: "secret://K",
		Temperature: &temp, AllowTruncated: true,
	}}
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"llm":`, `"baseUrl":`, `"apiKeyRef":`, `"temperature":0`, `"allowTruncated":true`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("wire form missing %s: %s", want, data)
		}
	}

	data, err = json.Marshal(EvalJudge{Type: "file-exists"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"llm"`) {
		t.Errorf("non-LLM judge serialises an llm key: %s", data)
	}
}
