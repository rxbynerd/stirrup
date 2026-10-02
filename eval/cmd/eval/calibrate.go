package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rxbynerd/stirrup/eval/calibrate"
	"github.com/rxbynerd/stirrup/eval/golden"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/eval/spec"
	"github.com/rxbynerd/stirrup/types"
)

// maxCalibrateRepeats bounds --repeats so a typo cannot multiply spend.
const maxCalibrateRepeats = 100

// cmdJudgeCalibrate measures a diff-review judge against a golden set. It
// is a measurement, not a gate: it exits 0 whatever the judge's agreement,
// 2 on a usage or configuration error found before any judgment, and 1
// when the run itself fails or every judgment ended in error, which leaves
// no metric to report.
func cmdJudgeCalibrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("judge-calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	goldenPath := fs.String("golden", "", "Golden set JSON file of labelled diff-review cases (required)")
	jf := addJudgeFlags(fs)
	fs.Lookup("judge-provider").Usage = "Judge provider: anthropic, openai-compatible or decision. Empty means anthropic, or the --judge-config provider."
	fs.Lookup("judge-cache-dir").Usage = "Judge cache directory. Required with read-through and replay-strict; record defaults to <directory of --output>/judge-cache. The directory must belong to the current user and must not be writable by group or others. Unused with --judge-cache live."
	judgeConfig := fs.String("judge-config", "", "File holding the judge's llm settings: HCL (llm block attributes, or one llm block) or JSON (.json). The --judge-* model flags override its connection fields.")
	repeats := fs.Int("repeats", 1, fmt.Sprintf("Times to judge each case (1-%d). Each repeat is cached as a separate sample.", maxCalibrateRepeats))
	output := fs.String("output", "", "Write the JSON report to this file")
	priceInput := fs.Float64("price-input", 0, "USD per million input tokens, for a cost estimate")
	priceOutput := fs.Float64("price-output", 0, "USD per million output tokens, for a cost estimate")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "judge-calibrate: "+format+"\n", a...)
		return 2
	}
	if fs.NArg() > 0 {
		return usageErr("unexpected arguments: %v", fs.Args())
	}
	if *goldenPath == "" {
		return usageErr("--golden is required")
	}
	if *repeats < 1 || *repeats > maxCalibrateRepeats {
		return usageErr("--repeats must be between 1 and %d", maxCalibrateRepeats)
	}
	var prices *calibrate.Prices
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "price-input" || f.Name == "price-output" {
			prices = &calibrate.Prices{InputPerMTok: *priceInput, OutputPerMTok: *priceOutput}
		}
	})
	for _, p := range []float64{*priceInput, *priceOutput} {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			return usageErr("--price-input and --price-output must be finite and not negative")
		}
	}

	set, err := golden.Load(*goldenPath)
	if err != nil {
		return usageErr("loading golden set: %v", err)
	}
	cfg, err := calibrationJudge(jf, *judgeConfig)
	if err != nil {
		return usageErr("%v", err)
	}
	mode, err := judge.ParseCacheMode(*jf.cache)
	if err != nil {
		return usageErr("--judge-cache: %v", err)
	}
	if mode.Reads() && *jf.cacheDir == "" {
		return usageErr("--judge-cache %s needs --judge-cache-dir: a mode that serves stored verdicts reads only a directory named explicitly", mode)
	}
	if mode == judge.CacheRecord && *jf.cacheDir == "" && *output == "" {
		return usageErr("--judge-cache record needs --judge-cache-dir or --output")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := judge.PreflightLLMConfig(ctx, cfg, mode); err != nil {
		return usageErr("%v", err)
	}
	workDir, err := os.MkdirTemp("", "stirrup-judge-calibrate-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "judge-calibrate: creating work directory: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	opts := judge.Options{CacheMode: mode}
	recordDir := ""
	if *output != "" {
		recordDir = filepath.Join(filepath.Dir(*output), judgeCacheDirName)
	}
	if _, err := jf.openCache(stderr, &opts, recordDir, []string{workDir}); err != nil {
		return usageErr("%v", err)
	}

	judgments, err := calibrate.Run(ctx, set, calibrate.Config{Judge: cfg, Repeats: *repeats, Options: opts, WorkDir: workDir, Progress: stderr})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "judge-calibrate: %v (after %d judgments)\n", err, len(judgments))
		return 1
	}
	if opts.CacheStats != nil {
		_, _ = fmt.Fprintln(stderr, formatJudgeCache(opts.CacheStats.Summary(mode)))
	}
	warnJudgeCache(stderr, opts.CacheStats)
	report := calibrate.NewReport(*goldenPath, set.Name, len(set.Cases), cfg, *repeats, judgments, prices)
	if err := calibrate.WriteText(stdout, report); err != nil {
		_, _ = fmt.Fprintf(stderr, "judge-calibrate: writing report: %v\n", err)
		return 1
	}
	if *output != "" {
		if err := writeCalibrationReport(*output, report); err != nil {
			_, _ = fmt.Fprintf(stderr, "judge-calibrate: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "Wrote %s\n", *output)
	}
	if report.Metrics.Decided == 0 {
		_, _ = fmt.Fprintf(stderr, "judge-calibrate: all %d judgments ended in error; no agreement was measured\n", len(judgments))
		return 1
	}
	return 0
}

// calibrationJudge resolves the judge under calibration: the --judge-config
// file, if any, with the --judge-* model flags laid over it.
func calibrationJudge(jf *judgeFlags, configPath string) (types.JudgeLLMConfig, error) {
	var cfg types.JudgeLLMConfig
	if configPath != "" {
		loaded, err := spec.LoadJudgeLLMConfig(configPath)
		if err != nil {
			return types.JudgeLLMConfig{}, fmt.Errorf("--judge-config: %w", err)
		}
		cfg = loaded
	}
	for _, f := range []struct {
		flag  string
		field *string
	}{
		{*jf.provider, &cfg.Provider},
		{*jf.model, &cfg.Model},
		{*jf.baseURL, &cfg.BaseURL},
		{*jf.apiKeyRef, &cfg.APIKeyRef},
	} {
		if f.flag != "" {
			*f.field = f.flag
		}
	}
	resolved, err := judge.ResolveLLMConfig(nil, &cfg)
	if err != nil {
		return types.JudgeLLMConfig{}, fmt.Errorf("judge configuration: %w", err)
	}
	return resolved, nil
}

func writeCalibrationReport(path string, r calibrate.Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}
