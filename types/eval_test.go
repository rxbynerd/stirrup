package types

import (
	"encoding/json"
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
		{name: "raw api key", mutate: func(c *JudgeLLMConfig) { c.APIKeyRef = "sk-live-abc" }, wantErr: "secret:// reference"},
		{name: "empty secret ref", mutate: func(c *JudgeLLMConfig) { c.APIKeyRef = "secret://" }, wantErr: "names no secret"},
		{name: "timeout over cap", mutate: func(c *JudgeLLMConfig) { c.TimeoutSeconds = JudgeMaxTimeoutSeconds + 1 }, wantErr: "timeout_seconds"},
		{name: "negative timeout", mutate: func(c *JudgeLLMConfig) { c.TimeoutSeconds = -1 }, wantErr: "timeout_seconds"},
		{name: "negative input cap", mutate: func(c *JudgeLLMConfig) { c.MaxInputBytes = -1 }, wantErr: "max_input_bytes"},
		{name: "negative max tokens", mutate: func(c *JudgeLLMConfig) { c.MaxTokens = -1 }, wantErr: "max_tokens"},
		{name: "temperature too high", mutate: func(c *JudgeLLMConfig) { c.Temperature = temp(2.5) }, wantErr: "temperature"},
		{name: "temperature negative", mutate: func(c *JudgeLLMConfig) { c.Temperature = temp(-0.1) }, wantErr: "temperature"},
		{name: "unknown structured output", mutate: func(c *JudgeLLMConfig) { c.StructuredOutput = "tool" }, wantErr: "structured_output"},
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
