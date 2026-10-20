package judge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestValidateLLMBlock_ChecksJudgeTypeAndModel(t *testing.T) {
	cases := []struct {
		name    string
		judge   types.EvalJudge
		wantErr string
	}{
		{name: "no block", judge: types.EvalJudge{Type: "file-exists"}},
		{name: "valid diff-review block", judge: types.EvalJudge{Type: "diff-review", LLM: &types.JudgeLLMConfig{Model: "m"}}},
		{name: "block on another judge", judge: types.EvalJudge{Type: "file-exists", LLM: &types.JudgeLLMConfig{Model: "m"}}, wantErr: `judge.type "file-exists" does not support an llm block`},
		{name: "invalid block", judge: types.EvalJudge{Type: "diff-review", LLM: &types.JudgeLLMConfig{}}, wantErr: "llm block: model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := ValidateLLMBlock(tc.judge)
			if tc.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestPreflightSuite_ValidatesJudgeConfigPerTask(t *testing.T) {
	requireGit(t)
	t.Setenv("PREFLIGHT_KEY", "sk-preflight-0123456789")
	t.Setenv("PREFLIGHT_UNSET_KEY", "")
	reviewWith := func(llm *types.JudgeLLMConfig) types.EvalJudge {
		return types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: llm}
	}
	gateway := func(ref string) *types.JudgeLLMConfig {
		return &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "http://127.0.0.1:9/v1", APIKeyRef: ref}
	}
	cases := []struct {
		name    string
		tasks   []types.EvalTask
		opts    Options
		wantErr []string
	}{
		{name: "no diff-review judges", tasks: []types.EvalTask{{ID: "a", Judge: types.EvalJudge{Type: "file-exists"}}}},
		{name: "resolvable key", tasks: []types.EvalTask{{ID: "a", Judge: reviewWith(gateway("secret://PREFLIGHT_KEY"))}}},
		{
			name:    "missing key names the task and the reference",
			tasks:   []types.EvalTask{{ID: "ok", Judge: types.EvalJudge{Type: "file-exists"}}, {ID: "review", Judge: reviewWith(gateway("secret://PREFLIGHT_UNSET_KEY"))}},
			wantErr: []string{`task "review"`, "secret://PREFLIGHT_UNSET_KEY", "is empty or not set"},
		},
		{
			name:    "nested judge",
			tasks:   []types.EvalTask{{ID: "nested", Judge: types.EvalJudge{Type: "composite", Judges: []types.EvalJudge{{Type: "file-exists"}, reviewWith(gateway("secret://PREFLIGHT_UNSET_KEY"))}}}},
			wantErr: []string{`task "nested"`, "PREFLIGHT_UNSET_KEY"},
		},
		{
			name:    "invocation defaults apply",
			tasks:   []types.EvalTask{{ID: "defaulted", Judge: reviewWith(nil)}},
			opts:    Options{LLMDefaults: gateway("secret://PREFLIGHT_UNSET_KEY")},
			wantErr: []string{`task "defaulted"`, "PREFLIGHT_UNSET_KEY"},
		},
		{
			name:    "refused endpoint",
			tasks:   []types.EvalTask{{ID: "meta", Judge: reviewWith(&types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://169.254.169.254/v1"})}},
			wantErr: []string{`task "meta"`, "link-local"},
		},
		{
			name:    "invalid configuration",
			tasks:   []types.EvalTask{{ID: "bad", Judge: reviewWith(&types.JudgeLLMConfig{Provider: "anthropic"})}},
			wantErr: []string{`task "bad"`, "model is required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := PreflightSuite(context.Background(), tc.tasks, tc.opts)
			if len(tc.wantErr) == 0 {
				must.NoError(err)
				return
			}
			must.Error(err)
			for _, want := range tc.wantErr {
				must.ErrorContains(err, want)
			}
			must.NotContains(err.Error(), "sk-preflight")
		})
	}
}

func TestPreflightLLMConfig_ChecksKeysEndpointsAndCacheMode(t *testing.T) {
	requireGit(t)
	t.Setenv("PREFLIGHT_KEY", "sk-preflight-0123456789")
	t.Setenv("PREFLIGHT_UNSET_KEY", "")
	cfg := func(ref string) types.JudgeLLMConfig {
		return types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "jev-latest", BaseURL: "http://127.0.0.1:9", APIKeyRef: ref}
	}

	t.Run("resolvable decision config", func(t *testing.T) {
		must := require.New(t)
		err := PreflightLLMConfig(context.Background(), cfg("secret://PREFLIGHT_KEY"), CacheLive)
		must.NoError(err)
	})
	t.Run("missing key names the reference", func(t *testing.T) {
		must := require.New(t)
		err := PreflightLLMConfig(context.Background(), cfg("secret://PREFLIGHT_UNSET_KEY"), CacheReadThrough)
		must.ErrorContains(err, "secret://PREFLIGHT_UNSET_KEY")
		must.NotContains(err.Error(), "sk-preflight")
	})
	t.Run("replay-strict needs no key", func(t *testing.T) {
		must := require.New(t)
		err := PreflightLLMConfig(context.Background(), cfg("secret://PREFLIGHT_UNSET_KEY"), CacheReplayStrict)
		must.NoError(err)
	})
	t.Run("refused endpoint", func(t *testing.T) {
		must := require.New(t)
		refused := types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://169.254.169.254/v1"}
		err := PreflightLLMConfig(context.Background(), refused, CacheLive)
		must.ErrorContains(err, "link-local")
	})
	t.Run("invalid config", func(t *testing.T) {
		must := require.New(t)
		err := PreflightLLMConfig(context.Background(), types.JudgeLLMConfig{}, CacheLive)
		must.ErrorContains(err, "model is required")
	})
	t.Run("invalid cache mode", func(t *testing.T) {
		must := require.New(t)
		err := PreflightLLMConfig(context.Background(), cfg(""), "bogus")
		must.ErrorContains(err, "bogus")
	})
}

func TestPreflightSuite_RequiresGitOnlyForDiffReview(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PREFLIGHT_KEY", "k")

	t.Run("suite without diff-review judges needs no git", func(t *testing.T) {
		must := require.New(t)
		err := PreflightSuite(context.Background(), []types.EvalTask{{ID: "a", Judge: types.EvalJudge{Type: "file-exists"}}}, Options{})
		must.NoError(err)
	})
	t.Run("diff-review judge needs git on PATH", func(t *testing.T) {
		must := require.New(t)
		review := types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{Model: "m", APIKeyRef: "secret://PREFLIGHT_KEY"}}
		err := PreflightSuite(context.Background(), []types.EvalTask{{ID: "a", Judge: review}}, Options{})
		must.ErrorContains(err, "git on PATH")
	})
}
