package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

// judgeCacheDirName is record mode's default judge cache directory, under
// `run --output` and beside a lakehouse's recordings.
const judgeCacheDirName = "judge-cache"

// judgeFlags are the --judge-* flags shared by `run` and `replay`. The model
// flags set the model for diff-review judges that have no `llm` block in the
// suite; an explicit block always wins. The cache flags select how those
// judges use the verdict cache.
type judgeFlags struct {
	provider  *string
	model     *string
	baseURL   *string
	apiKeyRef *string
	cache     *string
	cacheDir  *string
}

func addJudgeFlags(fs *flag.FlagSet) *judgeFlags {
	return &judgeFlags{
		provider:  fs.String("judge-provider", "", "Provider for diff-review judges without an llm block: anthropic or openai-compatible. Empty keeps the built-in Anthropic default."),
		model:     fs.String("judge-model", "", "Model for diff-review judges without an llm block. Empty keeps the built-in default for the provider."),
		baseURL:   fs.String("judge-base-url", "", "API base URL for diff-review judges without an llm block. Required for openai-compatible. An anthropic judge with a base URL other than the Anthropic API also needs --judge-api-key-ref."),
		apiKeyRef: fs.String("judge-api-key-ref", "", "Secret reference for the judge API key, e.g. secret://OPENROUTER_API_KEY. A reference resolved at runtime, never a literal key. Defaults to secret://ANTHROPIC_API_KEY only for the anthropic provider at the Anthropic API."),
		cache:     fs.String("judge-cache", string(judge.CacheLive), "How diff-review judges use the verdict cache: live (no cache), record (call the model and store every verdict), read-through (serve stored verdicts; call the model and store on a miss), or replay-strict (serve stored verdicts; a miss is an error and the model is never called)."),
		cacheDir:  fs.String("judge-cache-dir", "", "Judge cache directory. Required with read-through and replay-strict; record defaults to <output>/judge-cache for run and <lakehouse>/judge-cache for replay. The directory must belong to the current user, must not be writable by group or others, and must lie outside every workspace the agent under test can write. Unused with --judge-cache live."),
	}
}

// options validates the flags and returns the judge options they describe,
// without the cache itself, which openCache adds. Validation happens before
// any task runs so a misconfigured judge fails the invocation rather than
// every diff-review task.
func (f *judgeFlags) options() (judge.Options, error) {
	mode, err := judge.ParseCacheMode(*f.cache)
	if err != nil {
		return judge.Options{}, fmt.Errorf("--judge-cache: %w", err)
	}
	if mode.Reads() && *f.cacheDir == "" {
		return judge.Options{}, fmt.Errorf("--judge-cache %s needs --judge-cache-dir: a mode that serves stored verdicts reads only a directory named explicitly", mode)
	}
	opts := judge.Options{CacheMode: mode}
	if *f.provider == "" && *f.model == "" && *f.baseURL == "" && *f.apiKeyRef == "" {
		return opts, nil
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
	opts.LLMDefaults = defaults
	return opts, nil
}

// openCache gives opts the cache its mode needs and a counter for its
// outcomes. The directory is --judge-cache-dir or, in record mode only,
// recordDir; forbidden are directories the agent under test can write,
// which must not hold the cache. It returns the directory in use, or "" in
// live mode, which needs no cache.
func (f *judgeFlags) openCache(opts *judge.Options, recordDir string, forbidden []string) (string, error) {
	if opts.CacheMode.IsLive() {
		if *f.cacheDir != "" {
			fmt.Fprintf(os.Stderr, "ignoring --judge-cache-dir %q: --judge-cache is live\n", *f.cacheDir)
		}
		return "", nil
	}
	dir := *f.cacheDir
	if dir == "" && opts.CacheMode == judge.CacheRecord {
		dir = recordDir
	}
	if dir == "" {
		return "", fmt.Errorf("--judge-cache %s needs --judge-cache-dir", opts.CacheMode)
	}
	cache, err := judge.NewFileCache(dir, judge.FileCacheOptions{Mode: opts.CacheMode, ForbiddenRoots: forbidden})
	if err != nil {
		return "", fmt.Errorf("--judge-cache-dir: %w", err)
	}
	opts.Cache = cache
	opts.CacheStats = &judge.CacheStats{}
	fmt.Fprintf(os.Stderr, "Using judge cache directory %s (mode %s)\n", dir, opts.CacheMode)
	return dir, nil
}

// formatJudgeCache renders the judge cache line of a run or replay summary.
func formatJudgeCache(s eval.JudgeCacheSummary) string {
	line := fmt.Sprintf("Judge cache (%s): %d hits, %d misses, %d stored, %d bypassed",
		s.Mode, s.Hits, s.Misses, s.Stored, s.Bypassed)
	if s.WriteErrors > 0 {
		line += fmt.Sprintf(", %d write errors", s.WriteErrors)
	}
	return line
}

// warnJudgeCacheWrite reports the first verdict the cache failed to store.
func warnJudgeCacheWrite(stats *judge.CacheStats) {
	if stats == nil {
		return
	}
	if err := stats.WriteError(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: judge cache write failed: %v\n", err)
	}
}
