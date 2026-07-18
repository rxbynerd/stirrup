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
  ingest                 Ingest harness JSONL traces into a lakehouse
  replay                 Re-evaluate recorded runs against suite judges
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
	case "ingest":
		cmdIngest(args[1:])
	case "replay":
		cmdReplay(args[1:])
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
// stdout, returning the process exit code (0 on success, 1 on a
// missing or unsupported shell).
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
	acceptQuarantine := fs.Bool("accept-quarantine", false, "Permit execution of suites whose QuarantineFlags is non-empty. Without this flag, mined-from-production suites that carry classified content are refused. See #115.")
	model := fs.String("model", "", "Model to run every task with (forwarded to each harness invocation as --model). Overrides the harness default and any model pinned by the suite's run_config block. Empty (the default) preserves existing behaviour.")
	promptModel := fs.String("prompt-model", "", "Prompt model to render system prompts with (forwarded to each harness invocation as --prompt-model). The wire model is unchanged; combine with --model to compare a prompt tuned for one model against another. Empty (the default) derives the prompt model from the effective model.")
	// Forwarded verbatim to every `stirrup harness` invocation so the
	// eval-gate CI job can authenticate via WIF instead of a static
	// ANTHROPIC_API_KEY; the four identifiers are non-secret.
	anthropicFederationRuleID := fs.String("anthropic-federation-rule-id", "", "Anthropic federation rule ID (`fdrl_...`). Forwarded to every harness invocation. Non-secret: identifies the federation rule but cannot itself authenticate.")
	anthropicOrganizationID := fs.String("anthropic-organization-id", "", "Anthropic organisation UUID. Forwarded to every harness invocation. Required alongside --anthropic-federation-rule-id when WIF is in use.")
	anthropicServiceAccountID := fs.String("anthropic-service-account-id", "", "Anthropic service account ID (`svac_...`). Forwarded to every harness invocation. Required alongside --anthropic-federation-rule-id when WIF is in use.")
	anthropicFromGitHubActions := fs.Bool("anthropic-from-github-actions", false, "Forward --anthropic-from-github-actions to every harness invocation. The harness then sources the OIDC token from ACTIONS_ID_TOKEN_REQUEST_URL / ACTIONS_ID_TOKEN_REQUEST_TOKEN.")
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

	if len(suite.QuarantineFlags) > 0 {
		if !*acceptQuarantine {
			log.Fatalf("refusing to execute quarantined suite %q (flags: %v). Re-run with --accept-quarantine to acknowledge the privacy implications.", suite.ID, suite.QuarantineFlags)
		}
		fmt.Fprintf(os.Stderr,
			"eval run: executing quarantined suite %q with flags %v (operator opted in via --accept-quarantine)\n",
			suite.ID, suite.QuarantineFlags)
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
		Model:       *model,
		PromptModel: *promptModel,
		AnthropicWIF: runner.AnthropicWIFConfig{
			FederationRuleID:  *anthropicFederationRuleID,
			OrganizationID:    *anthropicOrganizationID,
			ServiceAccountID:  *anthropicServiceAccountID,
			FromGitHubActions: *anthropicFromGitHubActions,
		},
	})
	if err != nil {
		log.Fatalf("running suite: %v", err)
	}

	// result.json is written both at the top level (legacy location CI
	// workflows read) and per-suite (canonical location alongside the
	// per-task artifact tree).
	suiteResultPath := filepath.Join(*outputDir, result.SuiteID, "result.json")
	if err := writeJSON(suiteResultPath, result); err != nil {
		log.Fatalf("writing per-suite result: %v", err)
	}
	resultPath := filepath.Join(*outputDir, "result.json")
	if err := writeJSON(resultPath, result); err != nil {
		log.Fatalf("writing result: %v", err)
	}

	if *junitPath != "" {
		// A JUnit write failure is demoted to a warning so the CI
		// loop's primary artifact (result.json, already written)
		// survives. cmdConvert keeps log.Fatalf — it has no prior
		// artifact to protect.
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
// types.EvalSuite. .hcl is the only accepted extension; the legacy
// JSON loader has been removed.
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

// cmdMineFailures queries production traces, hydrates recordings
// opportunistically, and constructs an EvalSuite of regression tasks
// that capture the failing-turn context an operator needs to write a
// meaningful test.
//
// The candidate set comes from QueryTraces rather than
// QueryRecordings because traces always exist (every harness run
// emits one) while recordings are opportunistic (only the streaming
// JSONL emitter writes them). Mining stays useful when no recording
// is available — the task just carries less context.
func cmdMineFailures(args []string) {
	fs := flag.NewFlagSet("mine-failures", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	afterStr := fs.String("after", "", "Filter traces after this date (RFC3339 or YYYY-MM-DD)")
	beforeStr := fs.String("before", "", "Filter traces before this date (RFC3339 or YYYY-MM-DD)")
	outcome := fs.String("outcome", "failed", "Filter by EvalOutcome (passed|failed|inconclusive). Defaults to failed; pass --include-inconclusive to broaden.")
	limit := fs.Int("limit", 0, "Maximum number of tasks to mine")
	sampleBy := fs.String("sample-by", "", "Stratify --limit across a dimension: outcome|model|mode|provider. Empty (the default) takes the top --limit by recency.")
	output := fs.String("output", "", "Write EvalSuite HCL to this file (.hcl recommended). Empty: dry-run to stdout.")
	includeBatch := fs.Bool("include-batch", false, "By default, batch runs (provider.batch.enabled=true) are excluded from mined failures because their wall-clock duration inflates apparent stall metrics. Pass --include-batch to include them.")
	includeInconclusive := fs.Bool("include-inconclusive", false, "Include EvalOutcome==inconclusive (max_turns / timeout / etc.) traces in addition to the --outcome target.")
	acceptQuarantine := fs.Bool("accept-quarantine", false, "Mined suites carry raw conversation content. If the source recordings trip a quarantine classifier, the file write is refused unless --accept-quarantine is set. See #115.")
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
	if *beforeStr != "" {
		t, err := parseDate(*beforeStr)
		if err != nil {
			log.Fatalf("parsing -before: %v", err)
		}
		filter.Before = &t
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	traces, err := store.QueryTraces(ctx, filter)
	if err != nil {
		log.Fatalf("querying traces: %v", err)
	}

	// Recordings are opportunistic; a trace missing from this map
	// gets a thin-trace task from the mining loop below.
	recordings, err := store.QueryRecordings(ctx, types.TraceFilter{})
	if err != nil {
		log.Fatalf("querying recordings: %v", err)
	}
	recByID := make(map[string]types.RunRecording, len(recordings))
	for _, rec := range recordings {
		recByID[rec.RunID] = rec
	}

	// Pre-filter before sampling so a 50-trace cap on a 5,000-trace
	// lakehouse doesn't waste the sampler's time on passing runs.
	target := types.EvalOutcome(*outcome)
	filtered := filterTracesForMining(traces, target, *includeInconclusive, *includeBatch)
	selected := sampleTraces(filtered, *sampleBy, *limit)

	var (
		hydratedRecordings []types.RunRecording
		tasks              []types.EvalTask
	)
	for _, t := range selected {
		rec, hasRec := recByID[t.ID]
		tasks = append(tasks, buildMinedTask(t, rec, hasRec))
		if hasRec {
			hydratedRecordings = append(hydratedRecordings, rec)
		}
	}

	// Only recordings actually used for hydration are classified —
	// a bare trace carries no transcript content, so it cannot trip
	// the quarantine classifier.
	flags := types.ClassifyForQuarantine(hydratedRecordings)
	if len(flags) > 0 && *output != "" && !*acceptQuarantine {
		fmt.Fprintf(os.Stderr,
			"mine-failures: refusing to write quarantined suite to %s (flags: %v). Re-run with --accept-quarantine to acknowledge the privacy implications.\n",
			*output, flags)
		os.Exit(1)
	}
	if len(flags) > 0 {
		fmt.Fprintf(os.Stderr, "mine-failures: quarantine flags present: %v\n", flags)
	}

	suite := types.EvalSuite{
		ID:              fmt.Sprintf("mined-failures-%d", time.Now().Unix()),
		Description:     fmt.Sprintf("Failures mined from production: %d task(s) from %d candidate trace(s) (%d with recordings)", len(tasks), len(filtered), len(hydratedRecordings)),
		Tasks:           tasks,
		QuarantineFlags: flags,
	}

	if *output != "" {
		if err := writeSuiteHCL(*output, suite); err != nil {
			log.Fatalf("writing suite: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Suite written to %s\n", *output)
	} else {
		// Dry-run.
		fmt.Fprintln(os.Stderr, "mine-failures: dry-run (no --output set); preview only:")
		for _, t := range tasks {
			fmt.Fprintf(os.Stderr, "  - %s: %s\n", t.ID, oneLine(t.Description))
		}
	}

	fmt.Printf("%d task(s) mined from %d candidate trace(s) (%d hydrated with recordings)\n",
		len(tasks), len(filtered), len(hydratedRecordings))
}

// filterTracesForMining applies the outcome, includeInconclusive,
// and includeBatch predicates, returning the subset to sample from.
func filterTracesForMining(traces []types.RunTrace, target types.EvalOutcome, includeInconclusive, includeBatch bool) []types.RunTrace {
	out := make([]types.RunTrace, 0, len(traces))
	for _, t := range traces {
		got := types.EvalOutcomeFor(t)
		matches := got == target ||
			(includeInconclusive && got == types.EvalInconclusive)
		if !matches {
			continue
		}
		if !includeBatch && t.Config.Provider.IsBatchEnabled() {
			continue
		}
		out = append(out, t)
	}
	return out
}

// sampleTraces stratifies a candidate slice across the dimension
// named by sampleBy. Empty sampleBy takes the top limit by recency;
// limit=0 returns all. Allocation uses largest-remainder rounding so
// a limit smaller than the stratum count still surfaces at least one
// trace per non-empty stratum where possible.
func sampleTraces(traces []types.RunTrace, sampleBy string, limit int) []types.RunTrace {
	if limit <= 0 || len(traces) <= limit {
		return traces
	}
	if sampleBy == "" {
		return traces[:limit]
	}

	type stratum struct {
		key     string
		members []types.RunTrace
	}
	keys := []string{}
	byKey := map[string]*stratum{}
	keyFn := func(t types.RunTrace) string {
		switch sampleBy {
		case "outcome":
			return string(types.EvalOutcomeFor(t))
		case "model":
			return t.Config.ModelRouter.Model
		case "mode":
			return t.Config.Mode
		case "provider":
			return t.Config.Provider.Type
		default:
			return ""
		}
	}
	for _, t := range traces {
		k := keyFn(t)
		if _, ok := byKey[k]; !ok {
			byKey[k] = &stratum{key: k}
			keys = append(keys, k)
		}
		byKey[k].members = append(byKey[k].members, t)
	}

	type alloc struct {
		key       string
		quota     int
		remainder float64
		members   []types.RunTrace
	}
	allocs := make([]alloc, 0, len(keys))
	total := len(traces)
	used := 0
	for _, k := range keys {
		s := byKey[k]
		want := float64(limit*len(s.members)) / float64(total)
		quota := int(want)
		used += quota
		allocs = append(allocs, alloc{
			key:       k,
			quota:     quota,
			remainder: want - float64(quota),
			members:   s.members,
		})
	}
	for used < limit {
		// Largest remainder wins; ties break by lowest quota, then key,
		// for a deterministic result.
		bestIdx := -1
		for i := range allocs {
			if allocs[i].quota >= len(allocs[i].members) {
				continue
			}
			if bestIdx == -1 {
				bestIdx = i
				continue
			}
			cur := allocs[bestIdx]
			cand := allocs[i]
			switch {
			case cand.remainder > cur.remainder:
				bestIdx = i
			case cand.remainder == cur.remainder && cand.quota < cur.quota:
				bestIdx = i
			case cand.remainder == cur.remainder && cand.quota == cur.quota && cand.key < cur.key:
				bestIdx = i
			}
		}
		if bestIdx == -1 {
			break // every stratum capped
		}
		allocs[bestIdx].quota++
		allocs[bestIdx].remainder = 0
		used++
	}

	out := make([]types.RunTrace, 0, limit)
	for _, a := range allocs {
		n := min(a.quota, len(a.members))
		out = append(out, a.members[:n]...)
	}
	return out
}

// buildMinedTask produces an EvalTask from a trace plus an optional
// recording. When present, the recording contributes failing-turn
// context to the description; otherwise the description flags the
// trace as thin.
func buildMinedTask(trace types.RunTrace, rec types.RunRecording, hasRecording bool) types.EvalTask {
	desc := fmt.Sprintf("Mined from run %s (outcome: %s)", trace.ID, trace.Outcome)
	if !hasRecording {
		desc += " — thin trace only, no recording available; refine prompt manually."
	} else {
		desc += "\n" + summariseFailingTurn(rec)
	}
	return types.EvalTask{
		ID:          fmt.Sprintf("mined-%s", trace.ID),
		Description: desc,
		Prompt:      trace.Config.Prompt,
		Mode:        trace.Config.Mode,
		Judge: types.EvalJudge{
			Type:    "test-command",
			Command: "go test ./...",
		},
	}
}

// summariseFailingTurn extracts a short excerpt of the last turn's
// assistant text and any failing tool call. Sub-agent tool activity
// gets its own line so multi-agent failures don't read as
// single-turn ones.
func summariseFailingTurn(rec types.RunRecording) string {
	if len(rec.Turns) == 0 {
		return "  recording present but no turns captured."
	}
	last := rec.Turns[len(rec.Turns)-1]
	parts := []string{
		fmt.Sprintf("  last turn: %d", last.Turn),
	}

	var assistantText string
	for _, blk := range last.ModelOutput {
		if blk.Type == "text" && blk.Text != "" {
			assistantText = blk.Text
			break
		}
	}
	if assistantText != "" {
		parts = append(parts, fmt.Sprintf("  assistant: %s", truncate(oneLine(assistantText), 240)))
	}

	for _, tc := range last.ToolCalls {
		if tc.Success {
			continue
		}
		parts = append(parts, fmt.Sprintf("  failing tool: %s (output: %s)", tc.Name, truncate(oneLine(tc.Output), 200)))
	}

	for _, summary := range rec.FinalOutcome.ToolCalls {
		if summary.ParentRunID != "" || (summary.RunID != "" && summary.RunID != rec.RunID) {
			parts = append(parts, fmt.Sprintf("  sub-agent tool: %s (run %s, parent %s)", summary.Name, summary.RunID, summary.ParentRunID))
		}
	}
	return strings.Join(parts, "\n")
}

// oneLine collapses internal newlines to spaces so a multi-line
// excerpt fits onto one line of the suite description without
// breaking HCL.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate caps s at n runes (NOT bytes) and appends an ellipsis
// when truncation happens. Used to keep mined task descriptions
// readable in the HCL output.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
	if len(s.QuarantineFlags) > 0 {
		vals := make([]cty.Value, len(s.QuarantineFlags))
		for i, f := range s.QuarantineFlags {
			vals[i] = cty.StringVal(string(f))
		}
		suiteBody.SetAttributeValue("quarantine_flags", cty.ListVal(vals))
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

// mineFailureTasksFiltered filters recordings for non-passing
// outcomes and converts them into EvalTasks with a default
// test-command judge. Failed and inconclusive are distinct
// categories (see docs/eval.md); only failed is mined by default,
// pass includeInconclusive=true to mine both.
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
// kept here so existing tests can call it directly.
func isBatchRecording(rec types.RunRecording) bool {
	return rec.Config.Provider.IsBatchEnabled()
}

// buildDriftReport computes deltas between current and baseline
// metrics. Streaming and batch duration percentiles are differenced
// separately so a run-mix shift toward batch traffic does not
// register as a streaming-latency regression.
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

	if report.Deltas.PassRateDelta < -0.05 {
		flags = append(flags, fmt.Sprintf("pass rate dropped %.1fpp", report.Deltas.PassRateDelta*100))
	}

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

	// MedianTurns is an int field; the truncated mean approximates it.
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
	// Go's format first.
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	if trimmed, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot parse %q as duration", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}

	return 0, fmt.Errorf("cannot parse %q as duration (use Go format like 24h or Nd for days)", s)
}
