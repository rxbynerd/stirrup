package spec

import (
	"reflect"
	"strings"
	"testing"

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

func TestLoadSuiteHCL_DiffReviewLLMBlock(t *testing.T) {
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
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}

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
	got := suite.Tasks[0].Judge.LLM
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LLM = %+v, want %+v", got, want)
	}
}

func TestLoadSuiteHCL_DiffReviewLLMBlockOptional(t *testing.T) {
	suite, err := LoadSuiteHCL(writeTemp(t, "nollm.hcl", diffReviewSuite("")))
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if suite.Tasks[0].Judge.LLM != nil {
		t.Errorf("LLM = %+v, want nil when no block is declared", suite.Tasks[0].Judge.LLM)
	}
}

func TestLoadSuiteHCL_DiffReviewLLMTemperatureUnsetStaysNil(t *testing.T) {
	src := diffReviewSuite(`
      llm {
        model = "claude-haiku-4-5-20251001"
      }
`)
	suite, err := LoadSuiteHCL(writeTemp(t, "temp.hcl", src))
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if suite.Tasks[0].Judge.LLM.Temperature != nil {
		t.Errorf("Temperature = %v, want nil so it is omitted on the wire", *suite.Tasks[0].Judge.LLM.Temperature)
	}
}

func TestLoadSuiteHCL_DiffReviewLLMInsideComposite(t *testing.T) {
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
          model = "claude-haiku-4-5-20251001"
        }
      }
    }
  }
}
`
	suite, err := LoadSuiteHCL(writeTemp(t, "composite.hcl", src))
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if suite.Tasks[0].Judge.Judges[1].LLM == nil {
		t.Error("nested diff-review judge lost its llm block")
	}
}

func TestLoadSuiteHCL_DiffReviewLLMBlockErrors(t *testing.T) {
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
			_, err := LoadSuiteHCL(writeTemp(t, "bad.hcl", diffReviewSuite("      "+tc.llm)))
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadSuiteHCL_BadBaseURLErrorDoesNotEchoCredentials(t *testing.T) {
	src := diffReviewSuite(llmBlock(`model = "m"`, `base_url = "https://user:hunter2@example.com"`))
	_, err := LoadSuiteHCL(writeTemp(t, "leak.hcl", src))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error echoes the embedded credential: %v", err)
	}
}

func TestLoadSuiteHCL_LLMBlockRejectedOnOtherJudgeTypes(t *testing.T) {
	for _, body := range []string{
		`type = "file-exists"
      paths = ["a.txt"]`,
		`type = "test-command"
      command = "true"`,
		`type = "composite"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }`,
	} {
		src := `
suite "bad" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      ` + body + `
      llm {
        model = "claude-haiku-4-5-20251001"
      }
    }
  }
}
`
		_, err := LoadSuiteHCL(writeTemp(t, "wrongtype.hcl", src))
		if err == nil {
			t.Errorf("llm block accepted on judge %q", strings.SplitN(body, "\n", 2)[0])
			continue
		}
		if !strings.Contains(err.Error(), "does not support an llm block") || !strings.Contains(err.Error(), "diff-review") {
			t.Errorf("error %q should name the llm block and the diff-review type", err)
		}
	}
}
