package main

import (
	"flag"
	"fmt"

	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

// judgeFlags are the --judge-* flags shared by `run` and `replay`. They set
// the model for diff-review judges that have no `llm` block in the suite;
// an explicit block always wins.
type judgeFlags struct {
	provider  *string
	model     *string
	baseURL   *string
	apiKeyRef *string
}

func addJudgeFlags(fs *flag.FlagSet) *judgeFlags {
	return &judgeFlags{
		provider:  fs.String("judge-provider", "", "Provider for diff-review judges without an llm block: anthropic or openai-compatible. Empty keeps the built-in Anthropic default."),
		model:     fs.String("judge-model", "", "Model for diff-review judges without an llm block. Empty keeps the built-in default for the provider."),
		baseURL:   fs.String("judge-base-url", "", "API base URL for diff-review judges without an llm block. Required for openai-compatible; optional for anthropic (gateway)."),
		apiKeyRef: fs.String("judge-api-key-ref", "", "Secret reference for the judge API key, e.g. secret://OPENROUTER_API_KEY. A reference resolved at runtime, never a literal key. Defaults to secret://ANTHROPIC_API_KEY for the anthropic provider."),
	}
}

// options validates the flags and returns the judge options they describe.
// Validation happens before any task runs so a misconfigured judge fails the
// invocation rather than every diff-review task.
func (f *judgeFlags) options() (judge.Options, error) {
	if *f.provider == "" && *f.model == "" && *f.baseURL == "" && *f.apiKeyRef == "" {
		return judge.Options{}, nil
	}
	defaults := &types.JudgeLLMConfig{
		Provider:  *f.provider,
		Model:     *f.model,
		BaseURL:   *f.baseURL,
		APIKeyRef: *f.apiKeyRef,
	}
	if _, err := judge.ResolveLLMConfig(nil, defaults); err != nil {
		return judge.Options{}, fmt.Errorf("--judge-* flags: %w", err)
	}
	return judge.Options{LLMDefaults: defaults}, nil
}
