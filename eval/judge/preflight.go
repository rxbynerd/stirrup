package judge

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/rxbynerd/stirrup/types"
)

// ValidateLLMBlock checks j's own llm block, not those of nested judges:
// only a diff-review judge may carry one, and it must pass
// types.JudgeLLMConfig.Validate. Suite loaders and the runner share it so
// every suite source is held to the same rules.
func ValidateLLMBlock(j types.EvalJudge) error {
	if j.LLM == nil {
		return nil
	}
	if j.Type != "diff-review" {
		return fmt.Errorf("judge.type %q does not support an llm block (use type \"diff-review\")", j.Type)
	}
	if err := j.LLM.Validate(); err != nil {
		return fmt.Errorf("llm block: %w", err)
	}
	return nil
}

// PreflightSuite checks, before any task runs, what every diff-review
// judge in tasks will need: a valid resolved configuration, a resolvable
// api_key_ref, an allowed endpoint, and git on PATH. A failure here would
// otherwise surface as an error on every judged task after its agent had
// run. Each distinct reference is resolved once and its value discarded;
// errors name the task and the reference, never the value. Under
// CacheReplayStrict no model is called, so references and endpoints are not
// checked. The decision provider is refused as the invocation default, since
// it would then decide every judge without an llm block.
func PreflightSuite(ctx context.Context, tasks []types.EvalTask, opts Options) error {
	if _, err := ParseCacheMode(string(opts.CacheMode)); err != nil {
		return err
	}
	if opts.LLMDefaults != nil && opts.LLMDefaults.EffectiveProvider() == types.JudgeProviderDecision {
		return fmt.Errorf("judge provider %q cannot be the default for a suite's diff-review judges: it is usable only on shadow judges and by judge-calibrate", types.JudgeProviderDecision)
	}
	offline := opts.CacheMode == CacheReplayStrict
	check := newConfigChecker()
	needGit := false
	for _, task := range tasks {
		for _, j := range diffReviewJudges(task.Judge) {
			needGit = true
			cfg, err := ResolveLLMConfig(j.LLM, opts.LLMDefaults)
			if err != nil {
				return fmt.Errorf("task %q: diff-review judge: %w", task.ID, err)
			}
			if offline {
				continue
			}
			if err := check(ctx, cfg); err != nil {
				return fmt.Errorf("task %q: diff-review judge: %w", task.ID, err)
			}
		}
	}
	if needGit {
		return checkGit()
	}
	return nil
}

// PreflightLLMConfig is PreflightSuite for a single resolved diff-review
// judge configuration, for callers that judge outside a suite.
func PreflightLLMConfig(ctx context.Context, cfg types.JudgeLLMConfig, mode CacheMode) error {
	if _, err := ParseCacheMode(string(mode)); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid judge llm configuration: %w", err)
	}
	if mode != CacheReplayStrict {
		if err := newConfigChecker()(ctx, cfg); err != nil {
			return err
		}
	}
	return checkGit()
}

// newConfigChecker returns a check that cfg's api_key_ref resolves and its
// endpoint is allowed, remembering each reference's and endpoint's result.
func newConfigChecker() func(context.Context, types.JudgeLLMConfig) error {
	refs := map[string]error{}
	endpoints := map[string]error{}
	return func(ctx context.Context, cfg types.JudgeLLMConfig) error {
		if cfg.APIKeyRef != "" {
			refErr, seen := refs[cfg.APIKeyRef]
			if !seen {
				_, refErr = resolveSecretRef(cfg.APIKeyRef)
				refs[cfg.APIKeyRef] = refErr
			}
			if refErr != nil {
				return fmt.Errorf("resolving api_key_ref %s: %w", cfg.APIKeyRef, refErr)
			}
		}
		endpoint := cfg.BaseURL + "\x00" + cfg.APIKeyRef
		endpointErr, seen := endpoints[endpoint]
		if !seen {
			endpointErr = CheckEndpoint(ctx, cfg)
			endpoints[endpoint] = endpointErr
		}
		return endpointErr
	}
}

func checkGit() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("diff-review judges need git on PATH: %w", err)
	}
	return nil
}

// diffReviewJudges returns j and its nested judges that are diff-review
// judges.
func diffReviewJudges(j types.EvalJudge) []types.EvalJudge {
	var out []types.EvalJudge
	if j.Type == "diff-review" {
		out = append(out, j)
	}
	for _, sub := range j.Judges {
		out = append(out, diffReviewJudges(sub)...)
	}
	return out
}
