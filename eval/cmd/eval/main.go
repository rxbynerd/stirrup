// Command eval is the CLI entrypoint for the stirrup eval framework.
// It supports running eval suites, comparing results, querying production
// metrics from a TraceLakehouse, mining failures into eval tasks, and
// detecting metric drift over time windows.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/eval/reporter"
	"github.com/rxbynerd/stirrup/eval/runner"
	"github.com/rxbynerd/stirrup/eval/spec"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

const usage = `Usage: eval <command> [options]

Commands:
  run                    Run an eval suite
  compare                Compare two eval results
  compare-to-production  Compare eval results to production metrics
  baseline               Pull production metrics as experiment baselines
  mine-failures          Turn production failures into eval tasks
  drift                  Detect metric changes over time windows
  convert                Convert a result.json into another format (e.g. JUnit XML)
  completion             Emit a shell completion script (bash|zsh|fish|powershell)

Run "eval <command> -help" for details.
`

func main() {
	log.SetFlags(0)
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run dispatches a stirrup-eval invocation. It is split out from main so
// tests can exercise short-circuit subcommands (e.g. --version) without
// shelling out to a built binary or fighting global state.
//
// args is the slice of arguments AFTER the program name (i.e. os.Args[1:]),
// stdout is where short-circuit output is written, and the return value is
// the process exit code.
func run(args []string, stdout io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	switch args[0] {
	case "--version", "-v", "version":
		_, _ = fmt.Fprintf(stdout, "stirrup-eval version %s\n", version.Full())
		return 0
	case "run":
		cmdRun(args[1:])
	case "compare":
		cmdCompare(args[1:])
	case "baseline":
		cmdBaseline(args[1:])
	case "mine-failures":
		cmdMineFailures(args[1:])
	case "drift":
		cmdDrift(args[1:])
	case "compare-to-production":
		cmdCompareToProduction(args[1:])
	case "convert":
		cmdConvert(args[1:])
	case "completion":
		return cmdCompletion(args[1:], stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", args[0], usage)
		return 1
	}
	return 0
}

// cmdCompletion writes a shell completion script for stirrup-eval to
// stdout. The supported shells mirror those exposed by `stirrup
// completion`; the underlying scripts are hand-rolled rather than
// cobra-generated because the eval module does not depend on cobra
// (see completion.go for the rationale).
//
// Returns the process exit code: 0 on success, 1 on a missing or
// unsupported shell. A nil-writer error from the emit helper surfaces
// via stderr and a non-zero exit.
func cmdCompletion(args []string, stdout io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "completion requires a shell: bash | zsh | fish | powershell")
		return 1
	}
	if err := emitEvalCompletion(args[0], stdout); err != nil {
		fmt.Fprintf(os.Stderr, "completion: %v\n", err)
		return 1
	}
	return 0
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	suitePath := fs.String("suite", "", "Path to eval suite HCL file (required)")
	harnessPath := fs.String("harness", "", "Path to stirrup binary (default: stirrup)")
	outputDir := fs.String("output", "", "Output directory for results (default: current directory)")
	concurrency := fs.Int("concurrency", 1, "Maximum number of tasks to run in parallel (values <= 0 are treated as 1)")
	dryRun := fs.Bool("dry-run", false, "Validate suite without executing tasks")
	junitPath := fs.String("junit", "", "Write JUnit XML to this path after result.json (default: disabled)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *suitePath == "" {
		log.Fatal("-suite is required")
	}

	suite, err := loadSuite(*suitePath)
	if err != nil {
		log.Fatalf("loading suite: %v", err)
	}

	if *outputDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatalf("getting working directory: %v", err)
		}
		*outputDir = wd
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	result, err := runner.RunSuite(ctx, suite, runner.RunConfig{
		HarnessPath: *harnessPath,
		OutputDir:   *outputDir,
		Concurrency: *concurrency,
		DryRun:      *dryRun,
	})
	if err != nil {
		log.Fatalf("running suite: %v", err)
	}

	// Write the canonical per-suite result alongside the per-task artifact
	// tree the runner already created. The top-level result.json is kept
	// for backward compatibility with CI workflows and downstream tooling
	// that currently read <outputDir>/result.json — duplicating it is
	// cheap and keeps blast radius minimal.
	suiteResultPath := filepath.Join(*outputDir, result.SuiteID, "result.json")
	if err := writeJSON(suiteResultPath, result); err != nil {
		log.Fatalf("writing per-suite result: %v", err)
	}
	resultPath := filepath.Join(*outputDir, "result.json")
	if err := writeJSON(resultPath, result); err != nil {
		log.Fatalf("writing result: %v", err)
	}

	if *junitPath != "" {
		// JUnit XML is a secondary derived artifact; result.json has
		// already been written. Demote a write failure to a warning so
		// the CI loop's primary artifact survives even when the JUnit
		// emit fails (e.g. read-only filesystem, exhausted inode quota).
		// cmdConvert keeps log.Fatalf — it has no prior artifact to
		// protect.
		if err := writeJUnit(*junitPath, result); err != nil {
			fmt.Fprintf(os.Stderr, "warning: writing JUnit XML: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "JUnit XML written to %s\n", *junitPath)
		}
	}

	printSummary(result)
	fmt.Fprintf(os.Stderr, "\nResults written to %s (per-suite copy at %s)\n", resultPath, suiteResultPath)
}

// writeJUnit serialises a SuiteResult to path as JUnit XML using the
// reporter package. The file is created with explicit 0o644 permissions
// (matching writeJSON; bypassing umask). Close errors are surfaced to
// the caller — on NFS, tmpfs-over-full-disk, and Docker overlay volumes
// the underlying write failure only materialises at close(2), not
// write(2), so a deferred-and-discarded close would silently truncate
// the file and ship it to CI with exit code 0.
func writeJUnit(path string, result eval.SuiteResult) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // operator-supplied path; same trust model as --output / writeJSON
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := reporter.WriteJUnit(f, result); err != nil {
		_ = f.Close() // encode error is the meaningful one
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}

// cmdConvert converts an existing result.json into another format. Today
// only --to-junit is supported; the subcommand is shaped so other targets
// (TAP, GitHub annotations, etc.) can be slotted in without restructuring.
func cmdConvert(args []string) {
	fs := flag.NewFlagSet("convert", flag.ExitOnError)
	fromPath := fs.String("from", "", "Path to a SuiteResult JSON produced by `eval run` (required)")
	toJUnit := fs.String("to-junit", "", "Write JUnit XML to this path (required)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *fromPath == "" {
		log.Fatal("-from is required")
	}
	if *toJUnit == "" {
		log.Fatal("-to-junit is required")
	}

	result, err := loadResult(*fromPath)
	if err != nil {
		log.Fatalf("loading result: %v", err)
	}

	if err := writeJUnit(*toJUnit, result); err != nil {
		log.Fatalf("writing JUnit XML: %v", err)
	}

	fmt.Fprintf(os.Stderr, "JUnit XML written to %s\n", *toJUnit)
}

func cmdCompare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	currentPath := fs.String("current", "", "Path to current result JSON (required)")
	baselinePath := fs.String("baseline", "", "Path to baseline result JSON (required)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *currentPath == "" {
		log.Fatal("-current is required")
	}
	if *baselinePath == "" {
		log.Fatal("-baseline is required")
	}

	current, err := loadResult(*currentPath)
	if err != nil {
		log.Fatalf("loading current result: %v", err)
	}
	baseline, err := loadResult(*baselinePath)
	if err != nil {
		log.Fatalf("loading baseline result: %v", err)
	}

	report := reporter.Compare(baseline, current)
	fmt.Print(reporter.FormatText(report))

	if report.Summary.HasRegressions {
		os.Exit(1)
	}
}

// loadSuite reads a suite HCL file at path and returns the parsed
// types.EvalSuite. HCL is the only accepted authoring format; the
// legacy JSON loader was removed once HCL became canonical, so any
// extension other than .hcl is rejected with a clear error rather
// than silently accepted and parsed as JSON.
func loadSuite(path string) (types.EvalSuite, error) {
	if ext := strings.ToLower(filepath.Ext(path)); ext != ".hcl" {
		return types.EvalSuite{}, fmt.Errorf("unsupported suite file extension %q (expected .hcl)", ext)
	}
	return spec.LoadSuiteHCL(path)
}

func loadResult(path string) (eval.SuiteResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return eval.SuiteResult{}, err
	}
	var result eval.SuiteResult
	if err := json.Unmarshal(data, &result); err != nil {
		return eval.SuiteResult{}, fmt.Errorf("parsing result JSON: %w", err)
	}
	return result, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func printSummary(result eval.SuiteResult) {
	passed := 0
	failed := 0
	errored := 0
	for _, t := range result.Tasks {
		switch t.Outcome {
		case "pass":
			passed++
		case "fail":
			failed++
		case "error":
			errored++
		}
	}

	fmt.Printf("Suite: %s (run: %s)\n", result.SuiteID, result.RunID)
	fmt.Printf("Tasks: %d total, %d passed, %d failed, %d errors\n",
		len(result.Tasks), passed, failed, errored)
	fmt.Printf("Pass rate: %.1f%%\n", result.PassRate*100)
}

// cmdBaseline pulls production metrics from a lakehouse as experiment baselines.
func cmdBaseline(args []string) {
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	afterStr := fs.String("after", "", "Filter traces after this date (RFC3339 or YYYY-MM-DD)")
	beforeStr := fs.String("before", "", "Filter traces before this date (RFC3339 or YYYY-MM-DD)")
	mode := fs.String("mode", "", "Filter by run mode")
	model := fs.String("model", "", "Filter by model name")
	provider := fs.String("provider", "", "Filter by provider name (e.g. anthropic, openai-responses, gemini)")
	output := fs.String("output", "", "Write TraceMetrics JSON to this file")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer func() { _ = store.Close() }()

	filter := types.TraceFilter{
		Mode:     *mode,
		Model:    *model,
		Provider: *provider,
	}
	if *afterStr != "" {
		t, err := parseDate(*afterStr)
		if err != nil {
			log.Fatalf("parsing -after: %v", err)
		}
		filter.After = &t
	}
	if *beforeStr != "" {
		t, err := parseDate(*beforeStr)
		if err != nil {
			log.Fatalf("parsing -before: %v", err)
		}
		filter.Before = &t
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	metrics, err := store.Metrics(ctx, filter)
	if err != nil {
		log.Fatalf("computing metrics: %v", err)
	}

	if *output != "" {
		if err := writeJSON(*output, metrics); err != nil {
			log.Fatalf("writing metrics: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Metrics written to %s\n", *output)
	}

	printMetricsSummary(metrics)
}

// cmdMineFailures queries production failures and constructs eval tasks from them.
func cmdMineFailures(args []string) {
	fs := flag.NewFlagSet("mine-failures", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	afterStr := fs.String("after", "", "Filter traces after this date (RFC3339 or YYYY-MM-DD)")
	limit := fs.Int("limit", 0, "Maximum number of failures to mine")
	output := fs.String("output", "", "Write EvalSuite HCL to this file (.hcl recommended)")
	includeBatch := fs.Bool("include-batch", false, "By default, batch runs (provider.batch.enabled=true) are excluded from mined failures because their wall-clock duration inflates apparent stall metrics. Pass --include-batch to include them.")
	includeInconclusive := fs.Bool("include-inconclusive", false, "By default, only EvalOutcome==failed traces are mined. Inconclusive runs (max_turns / budget_exceeded / timeout / max_tokens / stalled / cancelled / verification_error) are typically investigated manually rather than codified as regressions; pass --include-inconclusive to include them anyway.")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer func() { _ = store.Close() }()

	filter := types.TraceFilter{}
	if *afterStr != "" {
		t, err := parseDate(*afterStr)
		if err != nil {
			log.Fatalf("parsing -after: %v", err)
		}
		filter.After = &t
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	recordings, err := store.QueryRecordings(ctx, filter)
	if err != nil {
		log.Fatalf("querying recordings: %v", err)
	}

	tasks := mineFailureTasksFiltered(recordings, *limit, *includeBatch, *includeInconclusive)

	suite := types.EvalSuite{
		ID:          fmt.Sprintf("mined-failures-%d", time.Now().Unix()),
		Description: fmt.Sprintf("Failures mined from production (%d of %d recordings)", len(tasks), len(recordings)),
		Tasks:       tasks,
	}

	if *output != "" {
		if err := writeSuiteHCL(*output, suite); err != nil {
			log.Fatalf("writing suite: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Suite written to %s\n", *output)
	}

	fmt.Printf("%d failures mined from %d total recordings\n", len(tasks), len(recordings))
}

// writeSuiteHCL serialises a types.EvalSuite as canonical HCL and writes
// it to path. Used by mine-failures to emit a starter suite that the
// run subcommand can load directly. hclwrite is responsible for
// escaping `"`, `\`, `${...}` interpolation markers, and other
// HCL-significant sequences in user-supplied prompts.
func writeSuiteHCL(path string, s types.EvalSuite) error {
	f := hclwrite.NewEmptyFile()
	suiteBody := f.Body().AppendNewBlock("suite", []string{s.ID}).Body()
	if s.Description != "" {
		suiteBody.SetAttributeValue("description", cty.StringVal(s.Description))
	}
	for _, t := range s.Tasks {
		taskBody := suiteBody.AppendNewBlock("task", []string{t.ID}).Body()
		if t.Description != "" {
			taskBody.SetAttributeValue("description", cty.StringVal(t.Description))
		}
		if t.Repo != "" {
			taskBody.SetAttributeValue("repo", cty.StringVal(t.Repo))
		}
		if t.Ref != "" {
			taskBody.SetAttributeValue("ref", cty.StringVal(t.Ref))
		}
		if t.Mode != "" {
			taskBody.SetAttributeValue("mode", cty.StringVal(t.Mode))
		}
		if t.Prompt != "" {
			taskBody.SetAttributeValue("prompt", cty.StringVal(t.Prompt))
		}
		appendJudgeBlock(taskBody, t.Judge)
	}
	return os.WriteFile(path, f.Bytes(), 0o644)
}

// appendJudgeBlock appends an EvalJudge as a `judge { ... }` block on
// parent. Recursive for composite judges so nested `judge` blocks
// preserve source order.
func appendJudgeBlock(parent *hclwrite.Body, j types.EvalJudge) {
	body := parent.AppendNewBlock("judge", nil).Body()
	body.SetAttributeValue("type", cty.StringVal(j.Type))
	if j.Command != "" {
		body.SetAttributeValue("command", cty.StringVal(j.Command))
	}
	if len(j.Paths) > 0 {
		vals := make([]cty.Value, len(j.Paths))
		for i, p := range j.Paths {
			vals[i] = cty.StringVal(p)
		}
		body.SetAttributeValue("paths", cty.ListVal(vals))
	}
	if j.Path != "" {
		body.SetAttributeValue("path", cty.StringVal(j.Path))
	}
	if j.Pattern != "" {
		body.SetAttributeValue("pattern", cty.StringVal(j.Pattern))
	}
	if j.Criteria != "" {
		body.SetAttributeValue("criteria", cty.StringVal(j.Criteria))
	}
	if j.Type == "composite" {
		if j.Require != "" {
			body.SetAttributeValue("require", cty.StringVal(j.Require))
		}
		for _, sub := range j.Judges {
			appendJudgeBlock(body, sub)
		}
	}
}

// cmdDrift detects metric changes between two adjacent time windows.
func cmdDrift(args []string) {
	fs := flag.NewFlagSet("drift", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	windowStr := fs.String("window", "", "Current window duration, e.g. 24h or 7d (required)")
	compareWindowStr := fs.String("compare-window", "", "Baseline window duration (defaults to -window)")
	mode := fs.String("mode", "", "Filter by run mode")
	model := fs.String("model", "", "Filter by model name")
	provider := fs.String("provider", "", "Filter by provider name (e.g. anthropic, openai-responses, gemini)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}
	if *windowStr == "" {
		log.Fatal("-window is required")
	}

	window, err := parseDuration(*windowStr)
	if err != nil {
		log.Fatalf("parsing -window: %v", err)
	}

	compareWindow := window
	if *compareWindowStr != "" {
		compareWindow, err = parseDuration(*compareWindowStr)
		if err != nil {
			log.Fatalf("parsing -compare-window: %v", err)
		}
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now()
	currentStart := now.Add(-window)
	baselineStart := currentStart.Add(-compareWindow)

	currentFilter := types.TraceFilter{
		After:    &currentStart,
		Before:   &now,
		Mode:     *mode,
		Model:    *model,
		Provider: *provider,
	}
	baselineEnd := currentStart
	baselineFilter := types.TraceFilter{
		After:    &baselineStart,
		Before:   &baselineEnd,
		Mode:     *mode,
		Model:    *model,
		Provider: *provider,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	currentMetrics, err := store.Metrics(ctx, currentFilter)
	if err != nil {
		log.Fatalf("computing current metrics: %v", err)
	}
	baselineMetrics, err := store.Metrics(ctx, baselineFilter)
	if err != nil {
		log.Fatalf("computing baseline metrics: %v", err)
	}

	report := buildDriftReport(currentMetrics, baselineMetrics)
	hasDrift := printDriftReport(report)
	if hasDrift {
		os.Exit(1)
	}
}

// mineFailureTasksFiltered filters recordings for non-passing outcomes
// and converts them into EvalTasks with a default test-command judge.
//
// As of #273 the filter is `EvalOutcomeFor(rec.FinalOutcome) ==
// EvalFailed` by default, replacing the previous `Outcome !=
// "success"` predicate. Failed and inconclusive are distinct
// categories: failed means the harness produced a wrong-direction
// result; inconclusive means the harness ran out of room or was
// interrupted. The latter is typically investigated manually rather
// than codified as a regression, so it is excluded by default.
// Pass includeInconclusive=true to mine both.
//
// When includeBatch is false (the documented default), recordings
// whose RunConfig opted into batch provider submission are skipped:
// their wall-clock duration is dominated by provider-side queue time,
// not the harness's stall pattern, so including them inflates apparent
// stall metrics and obscures the prompt patterns mine-failures is here
// to surface (#138).
func mineFailureTasksFiltered(recordings []types.RunRecording, limit int, includeBatch bool, includeInconclusive bool) []types.EvalTask {
	var tasks []types.EvalTask
	for _, rec := range recordings {
		switch types.EvalOutcomeFor(rec.FinalOutcome) {
		case types.EvalPassed:
			continue
		case types.EvalInconclusive:
			if !includeInconclusive {
				continue
			}
		case types.EvalFailed:
			// keep
		}
		if !includeBatch && isBatchRecording(rec) {
			continue
		}
		task := types.EvalTask{
			ID:          fmt.Sprintf("mined-%s", rec.RunID),
			Description: fmt.Sprintf("Mined from failed run %s (outcome: %s)", rec.RunID, rec.FinalOutcome.Outcome),
			Prompt:      rec.Config.Prompt,
			Mode:        rec.Config.Mode,
			Judge: types.EvalJudge{
				Type:    "test-command",
				Command: "go test ./...",
			},
		}
		tasks = append(tasks, task)
		if limit > 0 && len(tasks) >= limit {
			break
		}
	}
	return tasks
}

// isBatchRecording is a thin spelling of ProviderConfig.IsBatchEnabled
// kept here only so existing tests can call it directly. Both this and
// lakehouse.isBatchRun now route through the canonical
// ProviderConfig.IsBatchEnabled predicate (#138) so a future change
// to the batch posture rule lands in one place.
func isBatchRecording(rec types.RunRecording) bool {
	return rec.Config.Provider.IsBatchEnabled()
}

// buildDriftReport computes deltas between current and baseline metrics.
// The streaming and batch duration percentiles are differenced
// separately so a drift signal compares like-for-like (#138) and a
// run mix shift (more batch traffic) does not register as a
// streaming-latency regression.
func buildDriftReport(current, baseline types.TraceMetrics) types.DriftReport {
	return types.DriftReport{
		Current:  current,
		Baseline: baseline,
		Deltas: types.DriftDeltas{
			PassRateDelta:         current.PassRate - baseline.PassRate,
			MeanTurnsDelta:        current.MeanTurns - baseline.MeanTurns,
			MeanTokensDelta:       current.MeanTokens - baseline.MeanTokens,
			P50DurationDelta:      current.P50Duration - baseline.P50Duration,
			P95DurationDelta:      current.P95Duration - baseline.P95Duration,
			BatchP50DurationDelta: current.BatchP50Duration - baseline.BatchP50Duration,
			BatchP95DurationDelta: current.BatchP95Duration - baseline.BatchP95Duration,
		},
	}
}

// printMetricsSummary prints a human-readable summary of TraceMetrics to stdout.
func printMetricsSummary(m types.TraceMetrics) {
	fmt.Printf("Traces:       %d\n", m.Count)
	fmt.Printf("Pass rate:    %.1f%%\n", m.PassRate*100)
	fmt.Printf("Mean turns:   %.1f\n", m.MeanTurns)
	fmt.Printf("P50 duration: %.0fms\n", m.P50Duration)
	fmt.Printf("P95 duration: %.0fms\n", m.P95Duration)
}

// printDriftReport prints the drift report and returns true if significant drift
// was detected. Thresholds: pass rate drop > 5pp, turns increase > 20%.
func printDriftReport(report types.DriftReport) bool {
	fmt.Printf("%-16s %12s %12s %12s\n", "Metric", "Current", "Baseline", "Delta")
	fmt.Printf("%-16s %12s %12s %12s\n", "------", "-------", "--------", "-----")

	fmt.Printf("%-16s %11.1f%% %11.1f%% %+11.1fpp\n",
		"Pass rate", report.Current.PassRate*100, report.Baseline.PassRate*100, report.Deltas.PassRateDelta*100)
	fmt.Printf("%-16s %12.1f %12.1f %+12.1f\n",
		"Mean turns", report.Current.MeanTurns, report.Baseline.MeanTurns, report.Deltas.MeanTurnsDelta)
	fmt.Printf("%-16s %11.0fms %11.0fms %+11.0fms\n",
		"P50 duration", report.Current.P50Duration, report.Baseline.P50Duration, report.Deltas.P50DurationDelta)
	fmt.Printf("%-16s %11.0fms %11.0fms %+11.0fms\n",
		"P95 duration", report.Current.P95Duration, report.Baseline.P95Duration, report.Deltas.P95DurationDelta)

	var flags []string

	// Pass rate drop > 5 percentage points
	if report.Deltas.PassRateDelta < -0.05 {
		flags = append(flags, fmt.Sprintf("pass rate dropped %.1fpp", report.Deltas.PassRateDelta*100))
	}

	// Turns increase > 20%
	if report.Baseline.MeanTurns > 0 && report.Deltas.MeanTurnsDelta/report.Baseline.MeanTurns > 0.20 {
		flags = append(flags, fmt.Sprintf("mean turns increased %.0f%%",
			(report.Deltas.MeanTurnsDelta/report.Baseline.MeanTurns)*100))
	}

	if len(flags) > 0 {
		fmt.Println()
		fmt.Println("Significant drift detected:")
		for _, f := range flags {
			fmt.Printf("  - %s\n", f)
		}
		return true
	}

	fmt.Println()
	fmt.Println("No significant drift detected.")
	return false
}

// cmdCompareToProduction compares eval suite results against production metrics
// from a lakehouse, producing a LabVsProductionReport.
func cmdCompareToProduction(args []string) {
	fs := flag.NewFlagSet("compare-to-production", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	resultsPath := fs.String("results", "", "Path to eval SuiteResult JSON (required)")
	experimentID := fs.String("experiment-id", "", "Experiment identifier")
	afterStr := fs.String("after", "", "Filter production traces after this date (RFC3339 or YYYY-MM-DD)")
	beforeStr := fs.String("before", "", "Filter production traces before this date (RFC3339 or YYYY-MM-DD)")
	mode := fs.String("mode", "", "Filter by run mode")
	model := fs.String("model", "", "Filter by model name")
	provider := fs.String("provider", "", "Filter by provider name (e.g. anthropic, openai-responses, gemini)")
	outputPath := fs.String("output", "", "Output path for report JSON")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}
	if *resultsPath == "" {
		log.Fatal("-results is required")
	}

	result, err := loadResult(*resultsPath)
	if err != nil {
		log.Fatalf("loading results: %v", err)
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer func() { _ = store.Close() }()

	filter := types.TraceFilter{
		Mode:     *mode,
		Model:    *model,
		Provider: *provider,
	}
	if *afterStr != "" {
		t, err := parseDate(*afterStr)
		if err != nil {
			log.Fatalf("parsing -after: %v", err)
		}
		filter.After = &t
	}
	if *beforeStr != "" {
		t, err := parseDate(*beforeStr)
		if err != nil {
			log.Fatalf("parsing -before: %v", err)
		}
		filter.Before = &t
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	prodMetrics, err := store.Metrics(ctx, filter)
	if err != nil {
		log.Fatalf("computing production metrics: %v", err)
	}

	expID := *experimentID
	if expID == "" {
		expID = result.SuiteID
	}

	report := buildLabVsProductionReport(expID, prodMetrics, result)

	if *outputPath != "" {
		if err := writeJSON(*outputPath, report); err != nil {
			log.Fatalf("writing report: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Report written to %s\n", *outputPath)
	} else {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			log.Fatalf("marshalling report: %v", err)
		}
		fmt.Println(string(data))
	}

	fmt.Fprintln(os.Stderr)
	printComparisonSummary(os.Stderr, report)
}

// buildLabVsProductionReport constructs a LabVsProductionReport from production
// TraceMetrics and an eval SuiteResult.
func buildLabVsProductionReport(experimentID string, prodMetrics types.TraceMetrics, result eval.SuiteResult) types.LabVsProductionReport {
	production := types.BaselineMetrics{
		PassRate:   prodMetrics.PassRate,
		MeanTurns:  prodMetrics.MeanTurns,
		SampleSize: prodMetrics.Count,
	}

	// Compute lab variant metrics from the SuiteResult.
	var totalTurns int
	tracedTasks := 0
	for _, task := range result.Tasks {
		if task.Trace != nil {
			totalTurns += task.Trace.Turns
			tracedTasks++
		}
	}

	var meanTurns float64
	if tracedTasks > 0 {
		meanTurns = float64(totalTurns) / float64(tracedTasks)
	}

	variant := types.VariantReport{
		Name: result.SuiteID,
		Results: types.VariantResults{
			PassRate: result.PassRate,
		},
	}

	// MedianTurns is an int field; use the truncated mean as an approximation
	// when we don't have enough data points for a proper median.
	variant.Results.MedianTurns = int(meanTurns)

	return types.LabVsProductionReport{
		ExperimentID: experimentID,
		Production:   production,
		Variants:     []types.VariantReport{variant},
	}
}

// printComparisonSummary prints a human-readable table comparing
// production metrics to each lab variant. The destination writer is
// injected so tests can supply io.Discard rather than mutating
// os.Stderr globally; callers in cmdCompareToProduction pass os.Stderr.
func printComparisonSummary(w io.Writer, report types.LabVsProductionReport) {
	// Writes target a terminal (os.Stderr in production, io.Discard in
	// tests); a partial-write error here is unrecoverable and not worth
	// propagating to the caller.
	p := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format, args...)
	}
	p("Experiment: %s\n", report.ExperimentID)
	p("Production sample size: %d\n\n", report.Production.SampleSize)

	for _, v := range report.Variants {
		p("Variant: %s\n", v.Name)
		p("%-16s %12s %12s %12s\n", "Metric", "Production", "Lab", "Delta")
		p("%-16s %12s %12s %12s\n", "------", "----------", "---", "-----")

		prodPassPct := report.Production.PassRate * 100
		labPassPct := v.Results.PassRate * 100
		p("%-16s %11.1f%% %11.1f%% %+11.1fpp\n",
			"Pass rate", prodPassPct, labPassPct, labPassPct-prodPassPct)

		p("%-16s %12.1f %12d %+12.1f\n",
			"Mean turns",
			report.Production.MeanTurns,
			v.Results.MedianTurns,
			float64(v.Results.MedianTurns)-report.Production.MeanTurns)
	}
}

// parseDate parses a date string in either RFC3339 or "2006-01-02" format.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as RFC3339 or YYYY-MM-DD", s)
}

// parseDuration parses a duration string supporting Go's standard format plus
// a "d" suffix for days (e.g. "7d" = 168h).
func parseDuration(s string) (time.Duration, error) {
	// Try Go's built-in parser first.
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	// Handle "Nd" suffix for days.
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("cannot parse %q as duration", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}

	return 0, fmt.Errorf("cannot parse %q as duration (use Go format like 24h or Nd for days)", s)
}
