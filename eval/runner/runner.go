// Package runner orchestrates execution of eval suites, running tasks through
// the harness binary and applying judges to determine pass/fail outcomes.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/fixtureserver"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

// defaultTaskTimeoutSeconds is the per-task timeout used when the merged
// RunConfig does not pin one, matching the legacy `--timeout 300` default.
const defaultTaskTimeoutSeconds = 300

// RunConfig configures how the runner executes tasks.
type RunConfig struct {
	// HarnessPath is the path to the harness binary for live runs.
	// If empty, defaults to "stirrup" on PATH.
	HarnessPath string

	// OutputDir, when non-empty, enables per-task artifact retention: the
	// runner writes trace.jsonl, harness.stdout.txt, and harness.stderr.txt
	// under <OutputDir>/<suiteID>/<taskID>/ (trial-<n>/ below that when
	// the resolved trial count is above one, including for a task that
	// confirm-on-fail ran once). The temporary workspace is not copied.
	OutputDir string

	// Concurrency caps the number of task trials executed in parallel.
	// Values <= 0 fall back to 1 (sequential). Values larger than the
	// number of task trials are capped so we never spawn idle workers.
	Concurrency int

	// Trials is the number of independent runs per task, each with a fresh
	// workspace and harness subprocess. Values <= 0 defer to the suite's
	// trials attribute, then to 1.
	Trials int

	// TrialsPolicy is eval.TrialsPolicyFixed (the default when empty) or
	// eval.TrialsPolicyConfirmOnFail, under which only a task whose first
	// trial did not pass runs the remaining trials.
	TrialsPolicy string

	// RequireHealthy makes the comparison gate count a judge pass only when
	// the run was healthy, in addition to a suite's require_healthy
	// attribute.
	RequireHealthy bool

	// DryRun if true, validates the suite without executing tasks.
	DryRun bool

	// Model, when non-empty, is forwarded to every harness invocation
	// as --model, taking precedence over both the harness default and
	// any model pinned by the suite's run_config block.
	Model string

	// PromptModel, when non-empty, is forwarded to every harness
	// invocation as --prompt-model. It pins the model identity the
	// system prompt templates render against without changing the
	// wire model.
	PromptModel string

	// Provider, BaseURL, and APIKeyRef, when non-empty, are forwarded to
	// every harness invocation. They carry the same Changed()-gated
	// override semantics as Model: the provider a suite runs against is a
	// property of the invocation, not of the suite, so CI can retarget
	// without editing suite files. APIKeyRef is a `secret://` reference,
	// never a literal key — the harness rejects a non-reference value.
	Provider  string
	BaseURL   string
	APIKeyRef string

	// EndpointProfile and CompatProfile, when non-empty, are forwarded as
	// --provider-endpoint-profile and --provider-compat-profile. A BaseURL
	// pointing at a relay or Azure deployment resolves the derived
	// "generic" endpoint profile unless EndpointProfile declares the real
	// one. The harness validates both values.
	EndpointProfile string
	CompatProfile   string

	// AnthropicWIF, when populated, forwards Anthropic Workload Identity
	// Federation flags to every harness invocation. See
	// docs/anthropic-wif.md.
	AnthropicWIF AnthropicWIFConfig

	// JudgeOptions carries invocation-scoped settings for LLM-backed judges
	// (the --judge-* flags). When its CacheMode is not live,
	// SuiteResult.JudgeCache reports the counts in its CacheStats, which
	// RunSuite creates when nil.
	JudgeOptions judge.Options
}

// AnthropicWIFConfig carries the CLI flags `stirrup harness` accepts to
// authenticate via Workload Identity Federation (docs/anthropic-wif.md).
// The struct is passthrough-only — the runner forwards values verbatim
// to the subprocess and performs no exchange of its own.
type AnthropicWIFConfig struct {
	// FederationRuleID is the `fdrl_...` identifier of the Anthropic
	// federation rule bound to this GitHub repository.
	FederationRuleID string

	// OrganizationID is the Anthropic organisation UUID the federation
	// rule targets.
	OrganizationID string

	// ServiceAccountID is the `svac_...` identifier of the Anthropic
	// service account the WIF exchange produces a token for.
	ServiceAccountID string

	// FromGitHubActions, when true, sources the OIDC token from the
	// GitHub Actions runner environment.
	FromGitHubActions bool
}

// configured reports whether any WIF field is populated. The runner
// uses this to decide whether to emit any --anthropic-* flags at all,
// preserving the legacy invocation shape for suites that do not opt in.
func (c AnthropicWIFConfig) configured() bool {
	return c.FederationRuleID != "" ||
		c.OrganizationID != "" ||
		c.ServiceAccountID != "" ||
		c.FromGitHubActions
}

// RunSuite executes all tasks in a suite and returns the aggregate result.
// Returned SuiteResult.Tasks preserves suite task order regardless of the
// order in which the workers actually finished.
func RunSuite(ctx context.Context, suite types.EvalSuite, cfg RunConfig) (eval.SuiteResult, error) {
	if err := validateSuite(suite); err != nil {
		return eval.SuiteResult{}, err
	}
	if cfg.Trials > eval.MaxTrials {
		return eval.SuiteResult{}, fmt.Errorf("trials must be at most %d, got %d", eval.MaxTrials, cfg.Trials)
	}
	if err := eval.ValidateTrialsPolicy(cfg.TrialsPolicy); err != nil {
		return eval.SuiteResult{}, err
	}
	if cfg.TrialsPolicy == "" {
		cfg.TrialsPolicy = eval.TrialsPolicyFixed
	}
	if err := judge.PreflightSuite(ctx, suite.Tasks, cfg.JudgeOptions); err != nil {
		return eval.SuiteResult{}, err
	}

	if cfg.HarnessPath == "" {
		cfg.HarnessPath = "stirrup"
	}

	// Resolve a relative, separator-bearing harness path to absolute:
	// each task runs with cmd.Dir set to a per-task temp workspace, so a
	// relative path like "./stirrup" would otherwise be looked up there
	// instead of the caller's CWD. Bare names (PATH lookup via
	// exec.LookPath) and already-absolute paths are left alone.
	if strings.ContainsRune(cfg.HarnessPath, os.PathSeparator) && !filepath.IsAbs(cfg.HarnessPath) {
		abs, err := filepath.Abs(cfg.HarnessPath)
		if err != nil {
			return eval.SuiteResult{}, fmt.Errorf("resolving harness path %q: %w", cfg.HarnessPath, err)
		}
		cfg.HarnessPath = abs
	}

	suiteArtifactDir := ""
	if cfg.OutputDir != "" {
		if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
			return eval.SuiteResult{}, fmt.Errorf("creating output directory: %w", err)
		}
		// suite.ID has already been validated as a safe single-segment path,
		// so this join cannot escape OutputDir.
		suiteArtifactDir = filepath.Join(cfg.OutputDir, suite.ID)
		if err := os.MkdirAll(suiteArtifactDir, 0o755); err != nil {
			return eval.SuiteResult{}, fmt.Errorf("creating suite artifact directory: %w", err)
		}
	}

	runID := fmt.Sprintf("eval-%d", time.Now().UnixMilli())
	startedAt := time.Now()

	// Load the suite-level RunConfig baseline once; per-task overlays are
	// applied against a fresh clone inside runTask / dry-run validation.
	// A nil baseline preserves the legacy five-flag invocation path.
	baseline, baselineErr := resolveBaseline(suite)
	if baselineErr != nil {
		return eval.SuiteResult{}, baselineErr
	}

	if err := checkLiveFlagsAgainstReplay(suite, baseline, cfg); err != nil {
		return eval.SuiteResult{}, err
	}

	trials := resolveTrials(cfg, suite)
	// With one trial both policies schedule the same runs; recording fixed
	// keeps such a result comparable with a fixed single-run baseline.
	if trials == 1 {
		cfg.TrialsPolicy = eval.TrialsPolicyFixed
	}

	if cfg.DryRun {
		// A dry run validates each task once regardless of trials.
		tasks := make([]eval.TaskResult, len(suite.Tasks))
		for i, t := range suite.Tasks {
			tasks[i] = aggregateTrials(t.ID, []eval.TaskResult{dryRunTask(t, baseline)})
		}
		return eval.SuiteResult{
			SuiteID:      suite.ID,
			RunID:        runID,
			StartedAt:    startedAt,
			CompletedAt:  time.Now(),
			Tasks:        tasks,
			PassRate:     suitePassRate(tasks),
			Trials:       trials,
			TrialsPolicy: cfg.TrialsPolicy,
		}, nil
	}

	if err := cfg.JudgeOptions.CheckCacheAvailable(suite.Tasks); err != nil {
		return eval.SuiteResult{}, err
	}
	cacheMode := cfg.JudgeOptions.CacheMode
	if !cacheMode.IsLive() && cfg.JudgeOptions.CacheStats == nil {
		cfg.JudgeOptions.CacheStats = &judge.CacheStats{}
	}

	requireHealthy := cfg.RequireHealthy || suite.RequireHealthy
	results := runTasksConcurrently(ctx, suite.Tasks, cfg, suiteArtifactDir, baseline, trials, requireHealthy)

	result := eval.SuiteResult{
		SuiteID:      suite.ID,
		RunID:        runID,
		StartedAt:    startedAt,
		CompletedAt:  time.Now(),
		Tasks:        results,
		PassRate:     suitePassRate(results),
		Trials:       trials,
		TrialsPolicy: cfg.TrialsPolicy,

		HealthyPassRate: eval.HealthyPassRate(results),
		RequireHealthy:  requireHealthy,
	}
	if !cacheMode.IsLive() {
		summary := cfg.JudgeOptions.CacheStats.Summary(cacheMode)
		result.JudgeCache = &summary
	}
	return result, nil
}

// runTasksConcurrently runs the suite's trials across a bounded worker
// pool and returns one aggregated result per task in input order. Under
// eval.TrialsPolicyFixed every (task, trial) pair runs. Under
// eval.TrialsPolicyConfirmOnFail every task's first trial runs, then the
// remaining trials of each task that needsConfirmation; once ctx is
// cancelled no confirmation round starts, so each task keeps its first
// trial alone. Per-trial errors do not abort siblings.
func runTasksConcurrently(ctx context.Context, tasks []types.EvalTask, cfg RunConfig, suiteArtifactDir string, baseline *types.RunConfig, trials int, requireHealthy bool) []eval.TaskResult {
	runs := make([][]eval.TaskResult, len(tasks))
	if cfg.TrialsPolicy == eval.TrialsPolicyConfirmOnFail {
		for i := range runs {
			runs[i] = make([]eval.TaskResult, 1)
		}
		runTrialJobs(ctx, tasks, cfg, suiteArtifactDir, baseline, trials, trialJobs(len(tasks), 1), runs)

		if ctx.Err() == nil {
			first := make([]eval.TaskResult, len(tasks))
			for i := range runs {
				first[i] = runs[i][0]
				if needsConfirmation(first[i], requireHealthy) {
					runs[i] = append(runs[i], make([]eval.TaskResult, trials-1)...)
				}
			}
			runTrialJobs(ctx, tasks, cfg, suiteArtifactDir, baseline, trials, confirmationJobs(first, trials, requireHealthy), runs)
		}
	} else {
		for i := range runs {
			runs[i] = make([]eval.TaskResult, trials)
		}
		runTrialJobs(ctx, tasks, cfg, suiteArtifactDir, baseline, trials, trialJobs(len(tasks), trials), runs)
	}

	results := make([]eval.TaskResult, len(tasks))
	for i, t := range tasks {
		results[i] = aggregateTrials(t.ID, runs[i])
	}
	return results
}

// trialJob addresses one trial of one task by zero-based indexes.
type trialJob struct {
	task  int
	trial int
}

// trialJobs lists trials 1 to trials of every task, trial-major.
func trialJobs(tasks, trials int) []trialJob {
	jobs := make([]trialJob, 0, tasks*trials)
	for trial := range trials {
		for task := range tasks {
			jobs = append(jobs, trialJob{task: task, trial: trial})
		}
	}
	return jobs
}

// confirmationJobs lists, trial-major, trials 2 to trials of every task
// whose first trial needsConfirmation.
func confirmationJobs(first []eval.TaskResult, trials int, requireHealthy bool) []trialJob {
	var jobs []trialJob
	for trial := 1; trial < trials; trial++ {
		for task, r := range first {
			if needsConfirmation(r, requireHealthy) {
				jobs = append(jobs, trialJob{task: task, trial: trial})
			}
		}
	}
	return jobs
}

// needsConfirmation reports whether a first trial did not pass by the
// comparison gate's rule, so a judge pass on an unhealthy run is confirmed
// when the gate requires healthy runs.
func needsConfirmation(first eval.TaskResult, requireHealthy bool) bool {
	return first.GateCounts(requireHealthy).Pass == 0
}

// runTrialJobs runs jobs in order across at most cfg.Concurrency workers
// (at least one), storing each result at runs[task][trial]. Once ctx is
// cancelled every undispatched job is recorded as a cancellation error, so
// each task still aggregates every trial it was scheduled.
func runTrialJobs(ctx context.Context, tasks []types.EvalTask, cfg RunConfig, suiteArtifactDir string, baseline *types.RunConfig, trials int, jobs []trialJob, runs [][]eval.TaskResult) {
	if len(jobs) == 0 {
		return
	}
	concurrency := min(max(cfg.Concurrency, 1), len(jobs))

	queue := make(chan trialJob)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			for j := range queue {
				runs[j.task][j.trial] = runTrial(ctx, tasks[j.task], cfg, suiteArtifactDir, baseline, j.trial+1, trials)
			}
		})
	}

	// Honour ctx cancellation so the dispatcher doesn't deadlock if all
	// workers have exited.
dispatch:
	for n, j := range jobs {
		select {
		case <-ctx.Done():
			for _, rest := range jobs[n:] {
				runs[rest.task][rest.trial] = errorResult(tasks[rest.task].ID, time.Now(), ctx.Err())
			}
			break dispatch
		case queue <- j:
		}
	}
	close(queue)
	wg.Wait()
}

// validateSuite checks that a suite has the minimum required fields, that every
// task ID is a path-safe single segment (so per-task artifact directories
// cannot escape OutputDir via traversal sequences), and that every task's judge
// tree is valid, so a misconfigured judge fails before any agent run.
// checkLiveFlagsAgainstReplay refuses a live-run flag that would not take
// effect: the harness drops a task's recording only when --provider is
// switched, so --model, --base-url, --api-key-ref or a profile flag alone
// would leave a replay-backed suite replaying while its result named the
// requested model.
func checkLiveFlagsAgainstReplay(suite types.EvalSuite, baseline *types.RunConfig, cfg RunConfig) error {
	if cfg.Provider != "" || (cfg.Model == "" && cfg.BaseURL == "" && cfg.APIKeyRef == "" && cfg.EndpointProfile == "" && cfg.CompatProfile == "") {
		return nil
	}
	if !suiteReplaysByDefault(suite, baseline) {
		return nil
	}
	return fmt.Errorf("suite %q is replay-backed: --model, --base-url, --api-key-ref, --endpoint-profile and --compat-profile apply only to a live run; pass --provider to switch it live", suite.ID)
}

func suiteReplaysByDefault(suite types.EvalSuite, baseline *types.RunConfig) bool {
	if baseline != nil && baseline.Provider.Type == "replay" {
		return true
	}
	for _, task := range suite.Tasks {
		if task.RunConfigOverrides != nil && task.RunConfigOverrides.Provider != nil && task.RunConfigOverrides.Provider.Type == "replay" {
			return true
		}
	}
	return false
}

func validateSuite(suite types.EvalSuite) error {
	if suite.ID == "" {
		return fmt.Errorf("suite ID is required")
	}
	if err := validatePathSegment("suite ID", suite.ID); err != nil {
		return err
	}
	if len(suite.Tasks) == 0 {
		return fmt.Errorf("suite must contain at least one task")
	}
	if suite.Trials < 0 {
		return fmt.Errorf("suite trials must not be negative, got %d", suite.Trials)
	}
	if suite.Trials > eval.MaxTrials {
		return fmt.Errorf("suite trials must be at most %d, got %d", eval.MaxTrials, suite.Trials)
	}
	seen := make(map[string]struct{}, len(suite.Tasks))
	for _, t := range suite.Tasks {
		if t.ID == "" {
			return fmt.Errorf("task ID is required")
		}
		if err := validatePathSegment("task ID", t.ID); err != nil {
			return err
		}
		if _, dup := seen[t.ID]; dup {
			return fmt.Errorf("duplicate task ID %q", t.ID)
		}
		seen[t.ID] = struct{}{}
		if err := judge.ValidateTree(t.Judge); err != nil {
			return fmt.Errorf("task %q: %w", t.ID, err)
		}
	}
	return nil
}

// validatePathSegment rejects identifiers that are not a single
// non-traversing path component. The runner uses these IDs verbatim as
// directory names under OutputDir, so any non-segment value risks
// artifact paths escaping the configured tree.
func validatePathSegment(label, id string) error {
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("%s %q must not contain path separators", label, id)
	}
	if strings.ContainsRune(id, '\x00') {
		// Rejected by every filesystem, but caught here for a clear error.
		return fmt.Errorf("%s %q must not contain a null byte", label, id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("%s %q is a reserved path segment", label, id)
	}
	if strings.HasPrefix(id, "..") {
		return fmt.Errorf("%s %q must not start with traversal sequence", label, id)
	}
	return nil
}

// runTask executes a single eval task and returns the result. If
// suiteArtifactDir is non-empty, the task's trace and harness output streams
// are copied into <suiteArtifactDir>/<taskID>/ before the temp workspace is
// removed. When baseline is non-nil, the runner merges the task's
// RunConfigOverrides on top, writes the result to a per-task temp file,
// invokes the harness with --config, and retains a redacted copy under
// <suiteArtifactDir>/<taskID>/run_config.redacted.json.
func runTask(ctx context.Context, task types.EvalTask, cfg RunConfig, suiteArtifactDir string, baseline *types.RunConfig) eval.TaskResult {
	start := time.Now()

	// "evaltask-", not "eval-task-": the hyphenated form makes
	// "eval-task-<id>" contain the literal substring "sk-<id>", which the
	// sensitive-data detector read as an OpenAI key — latching Rule-of-Two
	// mid-run off a tool result that echoed the path. The detector regex is
	// now boundary-anchored, but this naming keeps the runner from
	// manufacturing key-shaped paths for any future pattern pack. Same for
	// the trace dir below.
	tmpDir, err := os.MkdirTemp("", "evaltask-"+task.ID+"-")
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("creating temp directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	if err := cfg.JudgeOptions.CheckCacheOutside(tmpDir); err != nil {
		return errorResult(task.ID, start, err)
	}

	workspaceDir := tmpDir
	if task.Repo != "" {
		if err := cloneRepo(ctx, task.Repo, task.Ref, tmpDir); err != nil {
			return errorResult(task.ID, start, fmt.Errorf("cloning repo: %w", err))
		}
	}

	// Seed declared workspace files after any clone (so a seed can override
	// a cloned file) and before the harness runs.
	if err := seedWorkspaceFiles(workspaceDir, task.Files); err != nil {
		return errorResult(task.ID, start, err)
	}

	// The baseline is taken after clone and seeding, so a diff-review
	// judge sees only the agent's changes.
	diffReviewed := judge.ContainsType(task.Judge, "diff-review")
	var judgeBaseline *judge.Baseline
	if diffReviewed {
		base, cleanup, err := createJudgeBaseline(ctx, task.ID, workspaceDir)
		if err != nil {
			return errorResult(task.ID, start, err)
		}
		defer cleanup()
		judgeBaseline = base
	}

	// The trace file lives outside the workspace: writing it inside would
	// let the agent read its own in-progress trace.jsonl via
	// list_directory/read_file, breaking task hermeticity (observed
	// manufacturing spurious judge passes).
	traceDir, err := os.MkdirTemp("", "evaltrace-"+task.ID+"-")
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("creating trace directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(traceDir) }()
	traceFile := filepath.Join(traceDir, "trace.jsonl")

	// A nil baseline (suite declared no run_config_file / run_config
	// block) preserves the legacy five-flag invocation: no --config
	// arg, no redacted artifact.
	merged, mergeErr := buildMergedConfig(baseline, task.RunConfigOverrides)
	if mergeErr != nil {
		return errorResult(task.ID, start, mergeErr)
	}

	if task.FixtureServer != nil {
		if merged == nil {
			return errorResult(task.ID, start, fmt.Errorf("task declares a fixture_server and needs a suite run_config or run_config_file baseline"))
		}
		server, err := fixtureserver.Start(*task.FixtureServer)
		if err != nil {
			return errorResult(task.ID, start, err)
		}
		defer func() { _ = server.Close() }()
		if err := applyFixtureServer(merged, *task.FixtureServer, server.URL); err != nil {
			return errorResult(task.ID, start, err)
		}
		if task.FixtureServer.Mode == types.FixtureModeSSE {
			cfg = withoutProviderFlags(cfg)
		}
	}

	configPath := ""
	if merged != nil {
		// Route the trace path through the merged config rather than
		// --trace: the harness's applyOverrides coerces
		// TraceEmitter.Type to "jsonl" whenever --trace is passed
		// without --trace-emitter, which would silently reset a
		// suite's intentional otel emitter to jsonl.
		merged.TraceEmitter.FilePath = traceFile

		// Validate before spawning the harness so a structural error
		// becomes a per-task "error" outcome instead of a wasted
		// subprocess launch. dryRunTask runs the same check.
		if vErr := types.ValidateRunConfig(merged); vErr != nil {
			return errorResult(task.ID, start, vErr)
		}

		// A diff-review judge diffs the whole workspace, so the config
		// is kept out of it to stay out of the diff.
		configDir := tmpDir
		if diffReviewed {
			configDir = traceDir
		}
		configPath = filepath.Join(configDir, "runconfig.json")
		if err := writeMergedConfig(configPath, merged); err != nil {
			return errorResult(task.ID, start, err)
		}
	}

	// --trace is omitted when a merged config is in use: the trace path
	// already rides in TraceEmitter.FilePath (see above), and passing
	// --trace too would trigger the jsonl-coercion side effect.
	args := []string{"harness"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, "--workspace", workspaceDir)
	if configPath == "" {
		args = append(args, "--trace", traceFile)
	}
	if task.Prompt != "" {
		args = append(args, "--prompt", task.Prompt)
	}
	if task.Mode != "" {
		args = append(args, "--mode", task.Mode)
	}
	if configPath == "" {
		// Legacy invocation has no in-config carrier for these two.
		if task.Mode == "" {
			args = append(args, "--mode", "execution")
		}
		args = append(args, "--timeout", fmt.Sprintf("%d", defaultTaskTimeoutSeconds))
	}

	// WIF/model/prompt-model are forwarded unconditionally (not gated
	// on configPath): credential and model posture are orthogonal to
	// the suite's --config opt-in, and the harness applies an
	// explicitly passed flag on top of any --config document.
	args = appendAnthropicWIFArgs(args, cfg.AnthropicWIF)

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	if cfg.PromptModel != "" {
		args = append(args, "--prompt-model", cfg.PromptModel)
	}

	// Emitted independently so an invocation can retarget just the base URL
	// — a gateway in front of the same provider type — without restating
	// the rest.
	if cfg.Provider != "" {
		args = append(args, "--provider", cfg.Provider)
	}
	if cfg.BaseURL != "" {
		args = append(args, "--base-url", cfg.BaseURL)
	}
	if cfg.APIKeyRef != "" {
		args = append(args, "--api-key-ref", cfg.APIKeyRef)
	}
	if cfg.EndpointProfile != "" {
		args = append(args, "--provider-endpoint-profile", cfg.EndpointProfile)
	}
	if cfg.CompatProfile != "" {
		args = append(args, "--provider-compat-profile", cfg.CompatProfile)
	}

	cmd := exec.CommandContext(ctx, cfg.HarnessPath, args...)
	cmd.Dir = workspaceDir

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	cmdErr := cmd.Run()

	// Persist artifacts before consuming results so state is captured
	// even when the harness exits non-zero. retainArtifacts is
	// best-effort: retention failures must not mask the result.
	if suiteArtifactDir != "" {
		retainArtifacts(suiteArtifactDir, task.ID, traceFile, stdoutBuf.Bytes(), stderrBuf.Bytes())
		if merged != nil {
			retainRedactedConfig(suiteArtifactDir, task.ID, merged)
		}
		if judgeBaseline != nil {
			retainJudgeBaseline(ctx, suiteArtifactDir, task.ID, *judgeBaseline)
		}
	}

	if cmdErr != nil {
		// The harness may still have produced a trace even on failure.
		trace, traceErr := parseTraceFile(traceFile)
		if traceErr != nil {
			return errorResult(task.ID, start, fmt.Errorf("harness failed: %w\nstdout: %s\nstderr: %s", cmdErr, stdoutBuf.String(), stderrBuf.String()))
		}
		// Harness failed but left a trace — use it for the result.
		verdict, judgeErr := judge.Evaluate(ctx, task.Judge, judge.JudgeContext{
			WorkspaceDir: workspaceDir,
			Trace:        trace,
			Baseline:     judgeBaseline,
			Options:      cfg.JudgeOptions,
		})
		if judgeErr != nil {
			return judgeErrorResult(task.ID, start, trace, verdict, fmt.Errorf("judge failed after harness error: %w", judgeErr))
		}
		return buildResult(task.ID, start, trace, verdict, true)
	}

	trace, err := parseTraceFile(traceFile)
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("parsing trace: %w", err))
	}

	verdict, err := judge.Evaluate(ctx, task.Judge, judge.JudgeContext{
		WorkspaceDir: workspaceDir,
		Trace:        trace,
		Baseline:     judgeBaseline,
		Options:      cfg.JudgeOptions,
	})
	if err != nil {
		return judgeErrorResult(task.ID, start, trace, verdict, fmt.Errorf("judge failed: %w", err))
	}

	return buildResult(task.ID, start, trace, verdict, false)
}

// judgeErrorResult is errorResult for a judge that could not rule. An
// LLM judge's own error verdict, which carries the call's provenance, is kept
// in place of the generic one.
func judgeErrorResult(taskID string, start time.Time, trace *types.RunTrace, verdict eval.JudgeVerdict, err error) eval.TaskResult {
	result := errorResult(taskID, start, err)
	result.Trace = trace
	result.Verification = types.LastVerification(trace)
	if verdict.Status == types.JudgeStatusError {
		result.JudgeVerdict = verdict
	}
	return result
}

// appendAnthropicWIFArgs adds the `stirrup harness` WIF flags for any
// operator-set identifier, leaving unset fields to the harness's own
// ValidateRunConfig error. Flag spellings must match
// harness/cmd/stirrup/cmd/runconfigflags.go exactly.
func appendAnthropicWIFArgs(args []string, wif AnthropicWIFConfig) []string {
	if !wif.configured() {
		return args
	}
	if wif.FederationRuleID != "" {
		args = append(args, "--anthropic-federation-rule-id", wif.FederationRuleID)
	}
	if wif.OrganizationID != "" {
		args = append(args, "--anthropic-organization-id", wif.OrganizationID)
	}
	if wif.ServiceAccountID != "" {
		args = append(args, "--anthropic-service-account-id", wif.ServiceAccountID)
	}
	if wif.FromGitHubActions {
		args = append(args, "--anthropic-from-github-actions")
	}
	return args
}

// retainArtifacts copies the per-task harness output and trace into the
// suite's artifact directory. Retention errors are swallowed (they must
// not mask the TaskResult) but reported via stderr.
func retainArtifacts(suiteArtifactDir, taskID, traceFile string, stdout, stderr []byte) {
	taskDir := filepath.Join(suiteArtifactDir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: mkdir: %v\n", taskID, err)
		return
	}
	// Best-effort: if the harness never wrote a trace, skip silently.
	if data, err := os.ReadFile(traceFile); err == nil {
		if err := os.WriteFile(filepath.Join(taskDir, "trace.jsonl"), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: trace: %v\n", taskID, err)
		}
	}
	if err := os.WriteFile(filepath.Join(taskDir, "harness.stdout.txt"), stdout, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: stdout: %v\n", taskID, err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "harness.stderr.txt"), stderr, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: stderr: %v\n", taskID, err)
	}
}

// seedWorkspaceFiles writes each declared seed file into the workspace
// before the harness runs. Paths are validated as workspace-relative and
// non-escaping at suite load time (spec.convertSeedFiles); the
// containment re-check here is defence-in-depth for callers that build
// EvalTask directly and bypass the HCL loader.
func seedWorkspaceFiles(workspaceDir string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}
	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		return fmt.Errorf("resolving workspace dir: %w", err)
	}
	for rel, content := range files {
		if filepath.IsAbs(rel) {
			return fmt.Errorf("seed file path %q must be relative to the workspace", rel)
		}
		dest := filepath.Join(absWorkspace, rel)
		if dest != absWorkspace && !strings.HasPrefix(dest, absWorkspace+string(filepath.Separator)) {
			return fmt.Errorf("seed file path %q escapes the workspace", rel)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("creating directory for seed file %q: %w", rel, err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing seed file %q: %w", rel, err)
		}
	}
	return nil
}

// cloneRepo materialises a git repository in targetDir. With an empty ref it
// takes a depth-1 clone of the default branch. With a ref (a branch, a tag, or
// a full commit SHA) it fetches exactly that commit by name, because a
// depth-1 clone holds only the default branch tip and cannot check out any
// other commit. The server must permit fetching the object by SHA, which
// GitHub does.
func cloneRepo(ctx context.Context, repo, ref, targetDir string) error {
	if strings.HasPrefix(repo, "-") || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("repo %q and ref %q must not start with %q", repo, ref, "-")
	}
	if ref == "" {
		cloneCmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", repo, targetDir)
		if output, err := cloneCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone: %w\n%s", err, output)
		}
		return nil
	}

	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", repo},
		{"fetch", "--quiet", "--depth", "1", "origin", ref},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = targetDir
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w\n%s", args[0], err, output)
		}
	}
	return nil
}

// parseTraceFile reads a JSONL trace file and returns the RunTrace from
// the last well-formed line, via the shared reader in types/trace.
func parseTraceFile(path string) (*types.RunTrace, error) {
	r, err := tracereader.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.Last()
}

// errorResult builds a TaskResult with outcome "error". The run counts as
// failed whatever the cause: no judge verdict exists for it.
func errorResult(taskID string, start time.Time, err error) eval.TaskResult {
	return eval.TaskResult{
		TaskID:       taskID,
		Outcome:      "error",
		Error:        err.Error(),
		RunHealth:    types.RunFailed,
		Verification: types.VerificationNone,
		Delivery:     eval.DeliveryNotMeasured,
		JudgeVerdict: eval.JudgeVerdict{
			Passed: false,
			Status: types.JudgeStatusError,
			Reason: err.Error(),
		},
		DurationMs: time.Since(start).Milliseconds(),
	}
}

// buildMergedConfig produces a per-task RunConfig from the suite baseline
// and per-task overlay. A nil baseline returns (nil, nil), preserving the
// legacy invocation path. The baseline is cloned before the overlay is
// applied so callers can reuse it across tasks.
//
// `timeout` is not surfaced in the HCL grammar (it is runner-owned), but
// types.ValidateRunConfig requires it set and positive. The runner's
// default Timeout is injected here when the merged config does not
// already pin one, so suites using an inline run_config block don't
// false-fail validation; a value already present survives unchanged.
func buildMergedConfig(baseline *types.RunConfig, overlay *types.RunConfigOverrides) (*types.RunConfig, error) {
	if baseline == nil {
		return nil, nil
	}
	// JSON round-trip clones every field, including pointer-typed
	// sub-structs, without hand-maintaining a deep copier.
	data, err := json.Marshal(baseline)
	if err != nil {
		return nil, fmt.Errorf("cloning suite baseline RunConfig: %w", err)
	}
	clone := &types.RunConfig{}
	if err := json.Unmarshal(data, clone); err != nil {
		return nil, fmt.Errorf("cloning suite baseline RunConfig: %w", err)
	}
	merged := mergeOverrides(clone, overlay)
	if merged != nil && merged.Timeout == nil {
		t := defaultTaskTimeoutSeconds
		merged.Timeout = &t
	}
	return merged, nil
}

// writeMergedConfig marshals the merged RunConfig to path, created inside
// the per-task tmpdir and removed with the rest of the workspace.
// Permissions are 0o600: the file carries the operator's chosen run
// posture and should not be world-readable on shared CI runners.
func writeMergedConfig(path string, cfg *types.RunConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling merged RunConfig: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing merged RunConfig: %w", err)
	}
	return nil
}

// retainRedactedConfig writes the redacted form of the merged RunConfig
// alongside the trace and harness output streams; see docs/eval.md for
// the retention and redaction guarantees. Retention errors are reported
// on stderr but never mask the TaskResult.
func retainRedactedConfig(suiteArtifactDir, taskID string, cfg *types.RunConfig) {
	warnIfRawAPIKeyRef(taskID, cfg)
	redacted := cfg.Redact()
	data, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: redacted config marshal: %v\n", taskID, err)
		return
	}
	taskDir := filepath.Join(suiteArtifactDir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: mkdir: %v\n", taskID, err)
		return
	}
	// 0o600 narrows exposure on shared CI runners: the file still
	// carries operational posture even after redaction.
	if err := os.WriteFile(filepath.Join(taskDir, "run_config.redacted.json"), data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: redacted config write: %v\n", taskID, err)
	}
}

// warnIfRawAPIKeyRef emits a stderr warning for any apiKeyRef field in
// the merged config that does not use the secret:// scheme (see
// docs/eval.md for why this defensive layer exists). The field list
// here must stay in lockstep with types.RunConfig.Redact() and
// eval/spec.validateInlineAPIKeyRefs.
func warnIfRawAPIKeyRef(taskID string, cfg *types.RunConfig) {
	if cfg == nil {
		return
	}
	check := func(path, ref string) {
		if ref == "" || strings.HasPrefix(ref, "secret://") {
			return
		}
		fmt.Fprintf(os.Stderr,
			"eval: task %q: %s is a raw value (not a secret:// reference); it should have been rejected at parse/validate time — redacting in artifact but the harness will refuse this configuration\n",
			taskID, path)
	}
	check("provider.apiKeyRef", cfg.Provider.APIKeyRef)
	for name, p := range cfg.Providers {
		check(fmt.Sprintf("providers[%s].apiKeyRef", name), p.APIKeyRef)
	}
	if cfg.Executor.VcsBackend != nil {
		check("executor.vcsBackend.apiKeyRef", cfg.Executor.VcsBackend.APIKeyRef)
	}
	for i, server := range cfg.Tools.MCPServers {
		check(fmt.Sprintf("tools.mcpServers[%d].apiKeyRef", i), server.APIKeyRef)
	}
}

// dryRunTask produces a TaskResult for a single task during a --dry-run
// invocation. With a nil baseline the task passes with "dry run —
// skipped". With a baseline, the runner builds the merged RunConfig and
// calls ValidateRunConfig; a validation failure flips the outcome to
// "error" without affecting other tasks in the same pass.
func dryRunTask(task types.EvalTask, baseline *types.RunConfig) eval.TaskResult {
	merged, err := buildMergedConfig(baseline, task.RunConfigOverrides)
	if err != nil {
		return errorResult(task.ID, time.Now(), err)
	}
	if task.FixtureServer != nil {
		if merged == nil {
			return errorResult(task.ID, time.Now(), fmt.Errorf("task declares a fixture_server and needs a suite run_config or run_config_file baseline"))
		}
		if err := applyFixtureServer(merged, *task.FixtureServer, dryRunFixtureURL); err != nil {
			return errorResult(task.ID, time.Now(), err)
		}
		if task.FixtureServer.Mode == types.FixtureModeSSE {
			if _, err := fixtureserver.LoadTurns(task.FixtureServer.StreamFile); err != nil {
				return errorResult(task.ID, time.Now(), err)
			}
		}
	}
	if merged != nil {
		if vErr := types.ValidateRunConfig(merged); vErr != nil {
			return errorResult(task.ID, time.Now(), vErr)
		}
	}
	return eval.TaskResult{
		TaskID:  task.ID,
		Outcome: "pass",
		JudgeVerdict: eval.JudgeVerdict{
			Passed: true,
			Status: types.JudgeStatusPass,
			Reason: "dry run — skipped",
		},
	}
}

// buildResult constructs a TaskResult from a trace and verdict. A verdict
// whose status is "error" is the error outcome whatever its Passed field
// says, so a judge that could not rule never counts as a pass or a fail.
// The outcome is the verdict alone; harnessFailed, the trace, and the
// verifier results only fill the run health fields beside it.
func buildResult(taskID string, start time.Time, trace *types.RunTrace, verdict eval.JudgeVerdict, harnessFailed bool) eval.TaskResult {
	result := eval.TaskResult{
		TaskID:       taskID,
		Outcome:      "fail",
		Trace:        trace,
		JudgeVerdict: verdict,
		DurationMs:   time.Since(start).Milliseconds(),
		RunHealth:    types.RunHealthFor(trace, harnessFailed),
		Verification: types.LastVerification(trace),
		Delivery:     eval.DeliveryNotMeasured,
	}
	switch {
	case verdict.Status == types.JudgeStatusError:
		result.Outcome = "error"
		result.JudgeVerdict.Passed = false
		result.Error = verdict.Reason
		if result.Error == "" {
			result.Error = "judge returned an error verdict"
		}
	case verdict.Passed:
		result.Outcome = "pass"
	}
	return result
}
