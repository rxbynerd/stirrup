package spec

import (
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestLoadJudgeLLMConfig(t *testing.T) {
	want := types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://gw.example/v1", APIKeyRef: "secret://K", MaxTokens: 512, AllowTruncated: true}
	cases := map[string]string{
		"attrs.hcl": `
provider        = "openai-compatible"
model           = "m"
base_url        = "https://gw.example/v1"
api_key_ref     = "secret://K"
max_tokens      = 512
allow_truncated = true
`,
		"block.hcl": `
llm {
  provider        = "openai-compatible"
  model           = "m"
  base_url        = "https://gw.example/v1"
  api_key_ref     = "secret://K"
  max_tokens      = 512
  allow_truncated = true
}
`,
		"config.json": `{"provider":"openai-compatible","model":"m","baseUrl":"https://gw.example/v1","apiKeyRef":"secret://K","maxTokens":512,"allowTruncated":true}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := LoadJudgeLLMConfig(writeTemp(t, name, src))
			if err != nil {
				t.Fatalf("LoadJudgeLLMConfig: %v", err)
			}
			if got.Provider != want.Provider || got.Model != want.Model || got.BaseURL != want.BaseURL ||
				got.APIKeyRef != want.APIKeyRef || got.MaxTokens != want.MaxTokens || got.AllowTruncated != want.AllowTruncated {
				t.Errorf("config = %+v, want %+v", got, want)
			}
		})
	}
}

func TestLoadJudgeLLMConfig_Rejects(t *testing.T) {
	cases := map[string]struct{ name, src, want string }{
		"unknown hcl attribute": {"a.hcl", `modle = "m"`, "modle"},
		"attributes and block":  {"b.hcl", "model = \"m\"\nllm {\n  model = \"m\"\n}\n", "not both"},
		"other block":           {"c.hcl", "judge {\n  model = \"m\"\n}\n", "single unlabelled llm block"},
		"labelled block":        {"d.hcl", "llm \"x\" {\n  model = \"m\"\n}\n", "single unlabelled llm block"},
		"unknown json field":    {"e.json", `{"modle":"m"}`, `unknown field "modle"`},
		"trailing json":         {"f.json", `{"model":"m"} {}`, "data after"},
		"bad hcl":               {"g.hcl", `model = `, "hcl:"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadJudgeLLMConfig(writeTemp(t, tc.name, tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadJudgeLLMConfig error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
