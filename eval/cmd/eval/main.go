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
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/eval/reporter"
	"github.com/rxbynerd/stirrup/eval/runner"
	"github.com/rxbynerd/stirrup/types"
)

const usage = `Usage: eval <command> [options]

Commands:
  run             Run an eval suite
  compare         Compare two eval results
  baseline        Pull production metrics as experiment baselines
  mine-failures   Turn production failures into eval tasks
  drift           Detect metric changes over time windows

Run "eval <command> -help" for details.
`

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "compare":
		cmdCompare(os.Args[2:])
	case "baseline":
		cmdBaseline(os.Args[2:])
	case "mine-failures":
		cmdMineFailures(os.Args[2:])
	case "drift":
		cmdDrift(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	suitePath := fs.String("suite", "", "Path to eval suite JSON file (required)")
	harnessPath := fs.String("harness", "", "Path to harness binary (default: stirrup-harness)")
	outputDir := fs.String("output", "", "Output directory for results (default: current directory)")
	concurrency := fs.Int("concurrency", 1, "Number of tasks to run in parallel")
	dryRun := fs.Bool("dry-run", false, "Validate suite without executing tasks")
	fs.Parse(args)

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

	resultPath := filepath.Join(*outputDir, "result.json")
	if err := writeJSON(resultPath, result); err != nil {
		log.Fatalf("writing result: %v", err)
	}

	printSummary(result)
	fmt.Fprintf(os.Stderr, "\nResults written to %s\n", resultPath)
}

func cmdCompare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	currentPath := fs.String("current", "", "Path to current result JSON (required)")
	baselinePath := fs.String("baseline", "", "Path to baseline result JSON (required)")
	fs.Parse(args)

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

func loadSuite(path string) (types.EvalSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return types.EvalSuite{}, err
	}
	var suite types.EvalSuite
	if err := json.Unmarshal(data, &suite); err != nil {
		return types.EvalSuite{}, fmt.Errorf("parsing suite JSON: %w", err)
	}
	return suite, nil
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
	fmt.Printf("Total cost: $%.4f\n", result.TotalCost)
}

// cmdBaseline pulls production metrics from a lakehouse as experiment baselines.
func cmdBaseline(args []string) {
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	afterStr := fs.String("after", "", "Filter traces after this date (RFC3339 or YYYY-MM-DD)")
	beforeStr := fs.String("before", "", "Filter traces before this date (RFC3339 or YYYY-MM-DD)")
	mode := fs.String("mode", "", "Filter by run mode")
	model := fs.String("model", "", "Filter by model name")
	output := fs.String("output", "", "Write TraceMetrics JSON to this file")
	fs.Parse(args)

	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer store.Close()

	filter := types.TraceFilter{
		Mode:  *mode,
		Model: *model,
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
	output := fs.String("output", "", "Write EvalSuite JSON to this file")
	fs.Parse(args)

	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer store.Close()

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

	tasks := mineFailureTasks(recordings, *limit)

	suite := types.EvalSuite{
		ID:          fmt.Sprintf("mined-failures-%d", time.Now().Unix()),
		Description: fmt.Sprintf("Failures mined from production (%d of %d recordings)", len(tasks), len(recordings)),
		Tasks:       tasks,
	}

	if *output != "" {
		if err := writeJSON(*output, suite); err != nil {
			log.Fatalf("writing suite: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Suite written to %s\n", *output)
	}

	fmt.Printf("%d failures mined from %d total recordings\n", len(tasks), len(recordings))
}

// cmdDrift detects metric changes between two adjacent time windows.
func cmdDrift(args []string) {
	fs := flag.NewFlagSet("drift", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	windowStr := fs.String("window", "", "Current window duration, e.g. 24h or 7d (required)")
	compareWindowStr := fs.String("compare-window", "", "Baseline window duration (defaults to -window)")
	mode := fs.String("mode", "", "Filter by run mode")
	model := fs.String("model", "", "Filter by model name")
	fs.Parse(args)

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
	defer store.Close()

	now := time.Now()
	currentStart := now.Add(-window)
	baselineStart := currentStart.Add(-compareWindow)

	currentFilter := types.TraceFilter{
		After: &currentStart,
		Before: &now,
		Mode:  *mode,
		Model: *model,
	}
	baselineEnd := currentStart
	baselineFilter := types.TraceFilter{
		After:  &baselineStart,
		Before: &baselineEnd,
		Mode:   *mode,
		Model:  *model,
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

// mineFailureTasks filters recordings for non-success outcomes and converts
// them into EvalTasks with a default test-command judge.
func mineFailureTasks(recordings []types.RunRecording, limit int) []types.EvalTask {
	var tasks []types.EvalTask
	for _, rec := range recordings {
		if rec.FinalOutcome.Outcome == "success" {
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

// buildDriftReport computes deltas between current and baseline metrics.
func buildDriftReport(current, baseline types.TraceMetrics) types.DriftReport {
	return types.DriftReport{
		Current:  current,
		Baseline: baseline,
		Deltas: types.DriftDeltas{
			PassRateDelta:    current.PassRate - baseline.PassRate,
			MeanCostDelta:    current.MeanCost - baseline.MeanCost,
			MeanTurnsDelta:   current.MeanTurns - baseline.MeanTurns,
			MeanTokensDelta:  current.MeanTokens - baseline.MeanTokens,
			P50DurationDelta: current.P50Duration - baseline.P50Duration,
			P95DurationDelta: current.P95Duration - baseline.P95Duration,
		},
	}
}

// printMetricsSummary prints a human-readable summary of TraceMetrics to stdout.
func printMetricsSummary(m types.TraceMetrics) {
	fmt.Printf("Traces:       %d\n", m.Count)
	fmt.Printf("Pass rate:    %.1f%%\n", m.PassRate*100)
	fmt.Printf("Mean cost:    $%.4f\n", m.MeanCost)
	fmt.Printf("Mean turns:   %.1f\n", m.MeanTurns)
	fmt.Printf("P50 duration: %.0fms\n", m.P50Duration)
	fmt.Printf("P95 duration: %.0fms\n", m.P95Duration)
}

// printDriftReport prints the drift report and returns true if significant drift
// was detected. Thresholds: pass rate drop > 5pp, cost increase > 20%,
// turns increase > 20%.
func printDriftReport(report types.DriftReport) bool {
	fmt.Printf("%-16s %12s %12s %12s\n", "Metric", "Current", "Baseline", "Delta")
	fmt.Printf("%-16s %12s %12s %12s\n", "------", "-------", "--------", "-----")

	fmt.Printf("%-16s %11.1f%% %11.1f%% %+11.1fpp\n",
		"Pass rate", report.Current.PassRate*100, report.Baseline.PassRate*100, report.Deltas.PassRateDelta*100)
	fmt.Printf("%-16s %11s %11s %+12s\n",
		"Mean cost", formatDollars(report.Current.MeanCost), formatDollars(report.Baseline.MeanCost), formatDollars(report.Deltas.MeanCostDelta))
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

	// Cost increase > 20%
	if report.Baseline.MeanCost > 0 && report.Deltas.MeanCostDelta/report.Baseline.MeanCost > 0.20 {
		flags = append(flags, fmt.Sprintf("mean cost increased %.0f%%",
			(report.Deltas.MeanCostDelta/report.Baseline.MeanCost)*100))
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

func formatDollars(v float64) string {
	return fmt.Sprintf("$%.4f", v)
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

