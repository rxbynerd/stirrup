package spec

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestLoadJudgeLLMConfig_ReadsEveryFormat(t *testing.T) {
	want := types.JudgeLLMConfig{
		Provider:       "openai-compatible",
		Model:          "m",
		BaseURL:        "https://gw.example/v1",
		APIKeyRef:      "secret://K",
		MaxTokens:      512,
		AllowTruncated: true,
	}
	cases := []struct {
		name, file, src string
	}{
		{"top-level attributes", "attrs.hcl", `
provider        = "openai-compatible"
model           = "m"
base_url        = "https://gw.example/v1"
api_key_ref     = "secret://K"
max_tokens      = 512
allow_truncated = true
`},
		{"llm block", "block.hcl", `
llm {
  provider        = "openai-compatible"
  model           = "m"
  base_url        = "https://gw.example/v1"
  api_key_ref     = "secret://K"
  max_tokens      = 512
  allow_truncated = true
}
`},
		{"json object", "config.json", `{"provider":"openai-compatible","model":"m","baseUrl":"https://gw.example/v1","apiKeyRef":"secret://K","maxTokens":512,"allowTruncated":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadJudgeLLMConfig(writeTemp(t, tc.file, tc.src))
			require.NoError(t, err)
			require.Equal(t, want.Provider, got.Provider)
			require.Equal(t, want.Model, got.Model)
			require.Equal(t, want.BaseURL, got.BaseURL)
			require.Equal(t, want.APIKeyRef, got.APIKeyRef)
			require.Equal(t, want.MaxTokens, got.MaxTokens)
			require.Equal(t, want.AllowTruncated, got.AllowTruncated)
		})
	}
}

func TestLoadJudgeLLMConfig_RejectsInvalidInput(t *testing.T) {
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
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadJudgeLLMConfig_ErrorsDoNotQuoteTheSource(t *testing.T) {
	const key = "sk-ant-api03-SECRETVALUE123"
	cases := map[string]string{
		"unquoted key":         "model = \"m\"\napi_key_ref = " + key + "\n",
		"unterminated string":  "model = \"m\"\napi_key_ref = \"" + key + "\n",
		"key in a block":       "llm {\n  api_key_ref = " + key + "\n}\n",
		"key as an expression": "api_key_ref = " + key + "(1)\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTemp(t, "judge.hcl", src)
			_, err := LoadJudgeLLMConfig(path)
			require.ErrorContains(t, err, path)
			require.NotContains(t, err.Error(), "SECRETVALUE", "error quotes the key")
		})
	}
}
