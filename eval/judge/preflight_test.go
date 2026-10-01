package judge

import (
	"context"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestValidateLLMBlock(t *testing.T) {
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
		err := ValidateLLMBlock(tc.judge)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestPreflightSuite(t *testing.T) {
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
			err := PreflightSuite(context.Background(), tc.tasks, tc.opts)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "sk-preflight") {
				t.Errorf("err carries a key value: %v", err)
			}
		})
	}
}

func TestPreflightSuite_RequiresGitOnlyForDiffReview(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PREFLIGHT_KEY", "k")
	if err := PreflightSuite(context.Background(), []types.EvalTask{{ID: "a", Judge: types.EvalJudge{Type: "file-exists"}}}, Options{}); err != nil {
		t.Errorf("suite without diff-review judges: %v", err)
	}
	review := types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{Model: "m", APIKeyRef: "secret://PREFLIGHT_KEY"}}
	err := PreflightSuite(context.Background(), []types.EvalTask{{ID: "a", Judge: review}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "git on PATH") {
		t.Errorf("err = %v, want a missing-git error", err)
	}
}
