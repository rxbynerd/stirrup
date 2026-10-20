package spec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// llmBlock renders an llm block with one attribute per line; HCL rejects
// single-line blocks with more than one attribute.
func llmBlock(attrs ...string) string {
	return "llm {\n" + strings.Join(attrs, "\n") + "\n}"
}

func diffReviewSuite(judgeBody string) string {
	return `
suite "dr" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type     = "diff-review"
      criteria = "the change adds a test"
` + judgeBody + `
    }
  }
}
`
}

func TestLoadSuiteHCL_DiffReviewLLMBlock_MapsAllFields(t *testing.T) {
	must := require.New(t)
	src := diffReviewSuite(`
      llm {
        provider          = "openai-compatible"
        model             = "openai/gpt-6-luna"
        base_url          = "https://openrouter.ai/api/v1"
        api_key_ref       = "secret://OPENROUTER_API_KEY"
        timeout_seconds   = 60
        max_input_bytes   = 131072
        temperature       = 0
        max_tokens        = 4096
        structured_output = "prompt_only"
        allow_truncated   = true
      }
`)
	suite, err := LoadSuiteHCL(writeTemp(t, "llm.hcl", src))
	must.NoError(err)

	zero := 0.0
	want := &types.JudgeLLMConfig{
		Provider:         "openai-compatible",
		Model:            "openai/gpt-6-luna",
		BaseURL:          "https://openrouter.ai/api/v1",
		APIKeyRef:        "secret://OPENROUTER_API_KEY",
		TimeoutSeconds:   60,
		MaxInputBytes:    131072,
		Temperature:      &zero,
		MaxTokens:        4096,
		StructuredOutput: "prompt_only",
		AllowTruncated:   true,
	}
	must.Equal(want, suite.Tasks[0].Judge.LLM)
}

func TestLoadSuiteHCL_DiffReviewLLMBlock_OmittedIsNil(t *testing.T) {
	must := require.New(t)
	suite, err := LoadSuiteHCL(writeTemp(t, "nollm.hcl", diffReviewSuite("")))
	must.NoError(err)
	must.Nil(suite.Tasks[0].Judge.LLM)
}

func TestLoadSuiteHCL_DiffReviewLLMBlock_UnsetTemperatureIsNil(t *testing.T) {
	must := require.New(t)
	src := diffReviewSuite(`
      llm {
        model = "claude-haiku-5-5"
      }
`)
	suite, err := LoadSuiteHCL(writeTemp(t, "temp.hcl", src))
	must.NoError(err)

	llm := suite.Tasks[0].Judge.LLM
	must.NotNil(llm)
	must.Nil(llm.Temperature, "temperature must stay nil so it is omitted on the wire")
}

func TestLoadSuiteHCL_DiffReviewLLMBlock_InsideComposite(t *testing.T) {
	must := require.New(t)
	src := `
suite "dr" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type = "composite"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }
      judge {
        type     = "diff-review"
        criteria = "c"
        llm {
          model = "claude-haiku-5-5"
        }
      }
    }
  }
}
`
	suite, err := LoadSuiteHCL(writeTemp(t, "composite.hcl", src))
	must.NoError(err)
	must.NotNil(suite.Tasks[0].Judge.Judges[1].LLM, "nested diff-review judge lost its llm block")
}

func TestLoadSuiteHCL_DiffReviewLLMBlock_RejectsInvalidFields(t *testing.T) {
	cases := []struct {
		name  string
		llm   string
		wants []string
	}{
		{
			name:  "raw credential",
			llm:   llmBlock(`model = "m"`, `api_key_ref = "sk-live-abc123"`),
			wants: []string{`task "t1"`, "llm block", "secret:// reference"},
		},
		{
			name:  "missing model",
			llm:   llmBlock(`provider = "anthropic"`),
			wants: []string{"llm block", "model is required"},
		},
		{
			name:  "unknown provider",
			llm:   llmBlock(`provider = "bedrock"`, `model = "m"`),
			wants: []string{"llm block", "provider", "bedrock"},
		},
		{
			name:  "openai-compatible without base url",
			llm:   llmBlock(`provider = "openai-compatible"`, `model = "m"`),
			wants: []string{"llm block", "base_url is required"},
		},
		{
			name:  "base url without scheme",
			llm:   llmBlock(`model = "m"`, `base_url = "api.example.com/v1"`),
			wants: []string{"llm block", "base_url must use http or https"},
		},
		{
			name:  "base url with ftp scheme",
			llm:   llmBlock(`model = "m"`, `base_url = "ftp://example.com"`),
			wants: []string{"llm block", "http or https"},
		},
		{
			name:  "base url without host",
			llm:   llmBlock(`model = "m"`, `base_url = "https:///v1"`),
			wants: []string{"llm block", "host"},
		},
		{
			name:  "base url with embedded credentials",
			llm:   llmBlock(`model = "m"`, `base_url = "https://user:pw@example.com"`),
			wants: []string{"llm block", "credentials"},
		},
		{
			name:  "timeout above cap",
			llm:   llmBlock(`model = "m"`, `timeout_seconds = 301`),
			wants: []string{"llm block", "timeout_seconds"},
		},
		{
			name:  "unknown structured output",
			llm:   llmBlock(`model = "m"`, `structured_output = "tool_use"`),
			wants: []string{"llm block", "structured_output"},
		},
		{
			name:  "temperature out of range",
			llm:   llmBlock(`model = "m"`, `temperature = 3`),
			wants: []string{"llm block", "temperature"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := LoadSuiteHCL(writeTemp(t, "bad.hcl", diffReviewSuite("      "+tc.llm)))
			must.Error(err)
			for _, want := range tc.wants {
				must.ErrorContains(err, want)
			}
		})
	}
}

func TestLoadSuiteHCL_LLMBlock_ErrorOmitsAPIKeyRefValue(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{name: "raw key", ref: "sk-live-abc123"},
		{name: "secret reference", ref: "secret://sk-live-abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			src := diffReviewSuite("      " + llmBlock(`model = "m"`, `api_key_ref = "`+tc.ref+`"`))
			_, err := LoadSuiteHCL(writeTemp(t, "leak.hcl", src))
			must.Error(err)
			must.NotContains(err.Error(), "abc123", "error echoes the api_key_ref value")
		})
	}
}

func TestLoadSuiteHCL_LLMBlock_ErrorOmitsBaseURLCredentials(t *testing.T) {
	must := require.New(t)
	src := diffReviewSuite(llmBlock(`model = "m"`, `base_url = "https://user:hunter2@example.com"`))
	_, err := LoadSuiteHCL(writeTemp(t, "leak.hcl", src))
	must.Error(err)
	must.NotContains(err.Error(), "hunter2", "error echoes the embedded credential")
}

func TestLoadSuiteHCL_LLMBlockRejectedOnOtherJudgeTypes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "file-exists judge",
			body: `type = "file-exists"
      paths = ["a.txt"]`,
		},
		{
			name: "test-command judge",
			body: `type = "test-command"
      command = "true"`,
		},
		{
			name: "composite judge",
			body: `type = "composite"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			src := `
suite "bad" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      ` + tc.body + `
      llm {
        model = "claude-haiku-5-5"
      }
    }
  }
}
`
			_, err := LoadSuiteHCL(writeTemp(t, "wrongtype.hcl", src))
			must.Error(err, "llm block should be rejected on this judge type")
			must.ErrorContains(err, "does not support an llm block")
			must.ErrorContains(err, "diff-review")
		})
	}
}
