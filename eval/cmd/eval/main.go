// Command eval is the CLI entrypoint for the stirrup eval framework.
// It supports running eval suites and comparing results between runs.
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
	"syscall"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/reporter"
	"github.com/rxbynerd/stirrup/eval/runner"
	"github.com/rxbynerd/stirrup/types"
)

const usage = `Usage: eval <command> [options]

Commands:
  run       Run an eval suite
  compare   Compare two eval results

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
