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
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

// defaultTaskTimeoutSeconds is the per-task timeout the runner falls back
// to when the merged RunConfig does not pin one and on the legacy
// invocation path. It matches the historic `--timeout 300` value the
// runner has always supplied so suites that opt into the RunConfig
// surface without setting `timeout = ...` keep behaving the same.
const defaultTaskTimeoutSeconds = 300

// RunConfig configures how the runner executes tasks.
type RunConfig struct {
	// HarnessPath is the path to the harness binary for live runs.
	// If empty, defaults to "stirrup" on PATH.
	HarnessPath string

	// OutputDir, when non-empty, enables per-task artifact retention. The
	// runner writes trace.jsonl, harness.stdout.txt, and harness.stderr.txt
	// for every task under <OutputDir>/<suiteID>/<taskID>/. The temporary
	// workspace is intentionally NOT copied: it can be large for repo-cloned
	// tasks and is out of scope for issue #31.
	OutputDir string

	// Concurrency caps the number of tasks executed in parallel. Values <= 0
	// fall back to 1 (sequential). Values larger than the task count are
	// capped at len(tasks) so we never spawn idle workers.
	Concurrency int

	// DryRun if true, validates the suite without executing tasks.
	DryRun bool

	// Model, when non-empty, is forwarded to every harness invocation
	// as --model. The harness applies explicit flags on top of any
	// --config document (Changed()-gated override), so this takes
	// precedence over both the harness default model and a model pinned
	// by the suite's run_config block. CI uses this to run the same
	// suite against different models (cheap gate on push, stronger
	// models at release) without editing suite files.
	Model string

	// PromptModel, when non-empty, is forwarded to every harness
	// invocation as --prompt-model (#492). It pins the model identity
	// the system prompt templates render against without changing the
	// wire model, so a sweep can compare e.g. the "claude-fable-5
	// prompt" on a newer model against that model's native prompt.
	PromptModel string

	// AnthropicWIF, when populated, instructs the runner to forward
	// Anthropic Workload Identity Federation flags to every harness
	// invocation. The four identifiers are non-secret per Anthropic's
	// WIF documentation (they identify the federation rule, the
	// organisation, and the service account, but cannot themselves
	// authenticate to the API) — see .github/workflows/smoke-anthropic.yml
	// for the established CI pattern. The harness performs the OIDC
	// token exchange at runtime using ACTIONS_ID_TOKEN_REQUEST_URL /
	// ACTIONS_ID_TOKEN_REQUEST_TOKEN from the GitHub Actions runner
	// environment; no static API key is ever materialised in the
	// runner's process, the RunConfig, or this struct.
	AnthropicWIF AnthropicWIFConfig
}

// AnthropicWIFConfig carries the four CLI flags `stirrup harness`
// accepts to authenticate via Workload Identity Federation. The values
// are GitHub Actions OIDC-exchange identifiers, NOT secrets: storing
// them on RunConfig (rather than fetching them through SecretStore at
// runtime, which is the pattern reserved for API keys) does not violate
// the project's "no secrets in RunConfig" invariant. The struct is
// passthrough-only — the runner forwards values verbatim to the
// subprocess and performs no exchange of its own.
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

	// FromGitHubActions, when true, instructs the harness to source the
	// OIDC token from the GitHub Actions runner environment
	// (ACTIONS_ID_TOKEN_REQUEST_URL / ACTIONS_ID_TOKEN_REQUEST_TOKEN).
	// This is the boolean toggle that flips the harness from "static
	// key" to "WIF exchange" — the three identifiers above are inert
	// without it.
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

	if cfg.HarnessPath == "" {
		cfg.HarnessPath = "stirrup"
	}

	// Resolve a relative, separator-bearing harness path to absolute. Each
	// task runs the harness with cmd.Dir set to its per-task temp workspace
	// (see runTask), and the OS resolves a relative exec path against the
	// child's working directory — so a path like "./stirrup" would be looked
	// up inside the temp workspace, not the caller's CWD, and fail with
	// "fork/exec ./stirrup: no such file or directory". Anchoring to an
	// absolute path here keeps the lookup independent of cmd.Dir. Bare names
	// (no separator) are left alone so PATH resolution via exec.LookPath
	// still works; already-absolute paths are unchanged.
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

	if cfg.DryRun {
		tasks := make([]eval.TaskResult, len(suite.Tasks))
		for i, t := range suite.Tasks {
			tasks[i] = dryRunTask(t, baseline)
		}
		passCount := 0
		for _, tr := range tasks {
			if tr.Outcome == "pass" {
				passCount++
			}
		}
		passRate := float64(0)
		if len(tasks) > 0 {
			passRate = float64(passCount) / float64(len(tasks))
		}
		return eval.SuiteResult{
			SuiteID:     suite.ID,
			RunID:       runID,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			Tasks:       tasks,
			PassRate:    passRate,
		}, nil
	}

	results := runTasksConcurrently(ctx, suite.Tasks, cfg, suiteArtifactDir, baseline)

	passCount := 0
	for _, tr := range results {
		if tr.Outcome == "pass" {
			passCount++
		}
	}

	passRate := float64(0)
	if len(results) > 0 {
		passRate = float64(passCount) / float64(len(results))
	}

	return eval.SuiteResult{
		SuiteID:     suite.ID,
		RunID:       runID,
		StartedAt:   startedAt,
		CompletedAt: time.Now(),
		Tasks:       results,
		PassRate:    passRate,
	}, nil
}

// runTasksConcurrently dispatches tasks across a bounded worker pool while
// preserving the input order in the returned slice. Concurrency is capped at
// len(tasks) so we never spawn idle workers; values <= 0 collapse to 1
// (the historical sequential behaviour). Per-task errors do not abort
// siblings — every task contributes a TaskResult.
func runTasksConcurrently(ctx context.Context, tasks []types.EvalTask, cfg RunConfig, suiteArtifactDir string, baseline *types.RunConfig) []eval.TaskResult {
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(tasks) {
		concurrency = len(tasks)
	}

	results := make([]eval.TaskResult, len(tasks))

	type job struct {
		idx  int
		task types.EvalTask
	}
	jobs := make(chan job)

	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			for j := range jobs {
				results[j.idx] = runTask(ctx, j.task, cfg, suiteArtifactDir, baseline)
			}
		})
	}

	// Feed jobs; honour ctx cancellation so we don't deadlock if all workers
	// have exited (e.g. on context cancellation, runTask still produces a
	// result via the harness exec error path, but the dispatcher needs to
	// stop pushing too).
	for i, t := range tasks {
		select {
		case <-ctx.Done():
			// Drain remaining tasks as cancellation errors so the result
			// slice stays in sync with the input slice.
			for ; i < len(tasks); i++ {
				results[i] = eval.TaskResult{
					TaskID:  tasks[i].ID,
					Outcome: "error",
					Error:   ctx.Err().Error(),
					JudgeVerdict: eval.JudgeVerdict{
						Passed: false,
						Reason: ctx.Err().Error(),
					},
				}
			}
			close(jobs)
			wg.Wait()
			return results
		case jobs <- job{idx: i, task: t}:
		}
	}
	close(jobs)
	wg.Wait()

	return results
}

// validateSuite checks that a suite has the minimum required fields and that
// every task ID is a path-safe single segment (so per-task artifact directories
// cannot escape OutputDir via traversal sequences).
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
	}
	return nil
}

// validatePathSegment rejects identifiers that are not a single non-traversing
// path component. The runner uses these IDs verbatim as directory names under
// OutputDir, so any non-segment value risks artifact paths escaping the
// configured tree.
//
// The check is intentionally minimal: ContainsAny("/\\") rejects all path
// separators (covers POSIX `/` and Windows `\\`, so a separate
// ContainsRune(os.PathSeparator) check would be dead code), and the explicit
// `.` / `..` / `..`-prefix checks reject the traversal forms that survive a
// no-separator input. A redundant filepath.Clean / filepath.IsAbs round-trip
// would only add reachable branches on Windows quirks irrelevant to this
// Linux/macOS tool, so they were dropped to keep the validator's surface area
// honest about what it actually enforces.
func validatePathSegment(label, id string) error {
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("%s %q must not contain path separators", label, id)
	}
	if strings.ContainsRune(id, '\x00') {
		// A NUL inside a filename is rejected by every supported
		// filesystem, but the validator is the single place that
		// documents what the runner will accept as a directory name;
		// reject it here so the error surfaces against the operator's
		// suite ID rather than later as an obscure filesystem syscall
		// error. Cheap to check, hard to silently let through.
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

	tmpDir, err := os.MkdirTemp("", "eval-task-"+task.ID+"-")
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("creating temp directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

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

	// The trace file lives OUTSIDE the workspace. Writing it into
	// workspaceDir would expose harness internals to the agent — a
	// list_directory or read_file of the in-progress trace.jsonl — which
	// breaks task hermeticity and can manufacture spurious judge passes
	// (a task asked to "read README.md and summarise it" in an empty
	// workspace summarised the leaked trace instead). A sibling temp dir
	// keeps the trace retrievable for artifact retention without polluting
	// the agent's view of its workspace.
	traceDir, err := os.MkdirTemp("", "eval-trace-"+task.ID+"-")
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("creating trace directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(traceDir) }()
	traceFile := filepath.Join(traceDir, "trace.jsonl")

	// Merge baseline + per-task overrides into a fresh RunConfig and
	// write it next to the workspace. A nil baseline (suite declared no
	// run_config_file / run_config block) preserves the legacy
	// five-flag invocation: no --config arg, no redacted artifact.
	merged, mergeErr := buildMergedConfig(baseline, task.RunConfigOverrides)
	if mergeErr != nil {
		return errorResult(task.ID, start, mergeErr)
	}

	configPath := ""
	if merged != nil {
		// Route the runner-managed trace path through the merged
		// config so --trace never has to appear on the command line
		// when --config is in use. The harness's applyOverrides
		// coerces TraceEmitter.Type to "jsonl" whenever --trace is
		// passed and --trace-emitter is not, which would silently
		// reset a suite's intentional otel emitter to jsonl. Setting
		// FilePath in the merged config lets the harness pick up the
		// per-task trace path without that coercion side-effect.
		merged.TraceEmitter.FilePath = traceFile

		// Validate the merged config before spawning the harness so a
		// structural error (e.g. a read-only mode paired with an
		// allow-all permission policy, or a write-tool entry under
		// planning mode) becomes a per-task "error" outcome instead
		// of a wasted subprocess launch followed by a harness boot
		// failure. dryRunTask already runs this check; keeping the
		// live path in sync prevents the two from drifting.
		if vErr := types.ValidateRunConfig(merged); vErr != nil {
			return errorResult(task.ID, start, vErr)
		}

		configPath = filepath.Join(tmpDir, "runconfig.json")
		if err := writeMergedConfig(configPath, merged); err != nil {
			return errorResult(task.ID, start, err)
		}
	}

	// --workspace is always needed: the runner manages the per-task
	// tmpdir and the harness has no way to discover it otherwise.
	// --trace is conditional: when a merged config is in use, the
	// runner already injected the trace path into TraceEmitter.FilePath
	// above, and passing --trace as well would trigger the harness's
	// applyOverrides path that coerces TraceEmitter.Type to "jsonl"
	// (silently clobbering a suite's intentional otel emitter).
	// --prompt and --mode are passed only when the task supplies a
	// non-zero value, so the merged config's prompt/mode is honoured
	// when the task itself does not override. --timeout is never
	// passed alongside --config — the merged config carries it.
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
		// Legacy invocation has no in-config carrier for these two —
		// fall back to the historic defaults so behaviour for suites
		// that have not opted into the RunConfig surface is unchanged.
		if task.Mode == "" {
			args = append(args, "--mode", "execution")
		}
		args = append(args, "--timeout", fmt.Sprintf("%d", defaultTaskTimeoutSeconds))
	}

	// Forward Anthropic WIF identifiers verbatim to every harness
	// invocation. The four flags are non-secret per Anthropic's WIF
	// documentation; the actual OIDC exchange happens inside the
	// harness using the GitHub Actions runner environment variables.
	// Emitting them unconditionally when configured (rather than gated
	// on configPath, like --trace/--mode/--timeout) is intentional:
	// credential posture is orthogonal to the suite's --config opt-in,
	// and a suite that bundles a RunConfig but no provider credential
	// still needs WIF identifiers passed on the command line.
	args = appendAnthropicWIFArgs(args, cfg.AnthropicWIF)

	// Forward the model override unconditionally (not gated on
	// configPath): the harness's flag resolution applies an explicitly
	// passed --model on top of the --config document, which is exactly
	// the operator-override semantic this field promises.
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	// Forward the prompt-model override with the same unconditional,
	// Changed()-backed semantics as --model above.
	if cfg.PromptModel != "" {
		args = append(args, "--prompt-model", cfg.PromptModel)
	}

	cmd := exec.CommandContext(ctx, cfg.HarnessPath, args...)
	cmd.Dir = workspaceDir

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	cmdErr := cmd.Run()

	// Persist artifacts before consuming results so we capture state even
	// when the harness exits non-zero. retainArtifacts is best-effort:
	// retention failures must not mask the harness/judge result.
	if suiteArtifactDir != "" {
		retainArtifacts(suiteArtifactDir, task.ID, traceFile, stdoutBuf.Bytes(), stderrBuf.Bytes())
		if merged != nil {
			retainRedactedConfig(suiteArtifactDir, task.ID, merged)
		}
	}

	if cmdErr != nil {
		// The harness may still have produced a trace even on failure.
		// Try to parse it before giving up entirely.
		trace, traceErr := parseTraceFile(traceFile)
		if traceErr != nil {
			return errorResult(task.ID, start, fmt.Errorf("harness failed: %w\nstdout: %s\nstderr: %s", cmdErr, stdoutBuf.String(), stderrBuf.String()))
		}
		// Harness failed but left a trace — use it for the result.
		verdict, judgeErr := judge.Evaluate(ctx, task.Judge, judge.JudgeContext{
			WorkspaceDir: workspaceDir,
			Trace:        trace,
		})
		if judgeErr != nil {
			return errorResult(task.ID, start, fmt.Errorf("judge failed after harness error: %w", judgeErr))
		}
		return buildResult(task.ID, start, trace, verdict)
	}

	trace, err := parseTraceFile(traceFile)
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("parsing trace: %w", err))
	}

	verdict, err := judge.Evaluate(ctx, task.Judge, judge.JudgeContext{
		WorkspaceDir: workspaceDir,
		Trace:        trace,
	})
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("judge failed: %w", err))
	}

	return buildResult(task.ID, start, trace, verdict)
}

// appendAnthropicWIFArgs adds the four `stirrup harness` flags that
// configure Anthropic Workload Identity Federation when the operator
// has set any of them. Each identifier flag is emitted only when
// non-empty so a partial configuration (e.g. WIF rule ID without a
// service account) still reaches the harness, where ValidateRunConfig
// can produce a single coherent error rather than the runner silently
// dropping fields. The boolean --anthropic-from-github-actions has no
// value argument; it is appended when the toggle is set.
//
// The flag spellings here MUST match `harness/cmd/stirrup/cmd/runconfigflags.go`
// exactly — the runner forwards them verbatim to the subprocess.
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
// suite's artifact directory. taskID has already been validated as a safe
// single-segment path. Retention errors are intentionally swallowed — they
// must not mask the underlying TaskResult — but failures are still reported
// via stderr so an operator can see when the artifact tree is incomplete.
func retainArtifacts(suiteArtifactDir, taskID, traceFile string, stdout, stderr []byte) {
	taskDir := filepath.Join(suiteArtifactDir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: mkdir: %v\n", taskID, err)
		return
	}
	// Copy the trace file (best-effort: if the harness never wrote one, skip
	// silently — that case is already reflected in the TaskResult).
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
// before the harness runs. Paths were validated as workspace-relative and
// non-escaping at suite load time (spec.convertSeedFiles); the containment
// re-check here is defence-in-depth for callers that build EvalTask
// directly (tests, future programmatic suites) and bypass the HCL loader.
// Parent directories are created as needed. Files are written 0o644 — they
// are fixture content, not secrets.
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

// cloneRepo clones a git repository at the given ref into the target directory.
func cloneRepo(ctx context.Context, repo, ref, targetDir string) error {
	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", repo, targetDir)
	if output, err := cloneCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone: %w\n%s", err, output)
	}

	if ref != "" {
		checkoutCmd := exec.CommandContext(ctx, "git", "checkout", ref)
		checkoutCmd.Dir = targetDir
		if output, err := checkoutCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout %s: %w\n%s", ref, err, output)
		}
	}

	return nil
}

// parseTraceFile reads a JSONL trace file and returns the RunTrace from
// the last well-formed line. The streaming parser, malformed-line
// skipping, and 4 MiB per-record cap all live in types/trace so the new
// `stirrup trace …` subcommands and the eval runner share one
// implementation.
func parseTraceFile(path string) (*types.RunTrace, error) {
	r, err := tracereader.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.Last()
}

// errorResult builds a TaskResult with outcome "error".
func errorResult(taskID string, start time.Time, err error) eval.TaskResult {
	return eval.TaskResult{
		TaskID:  taskID,
		Outcome: "error",
		Error:   err.Error(),
		JudgeVerdict: eval.JudgeVerdict{
			Passed: false,
			Reason: err.Error(),
		},
		DurationMs: time.Since(start).Milliseconds(),
	}
}

// buildMergedConfig produces a per-task RunConfig from the suite baseline
// and per-task overlay. A nil baseline returns (nil, nil): the suite has
// not opted into the RunConfig surface, so the legacy invocation path
// stays in effect. The baseline argument is cloned before the overlay is
// applied so callers can reuse it across tasks.
//
// types.ValidateRunConfig requires Timeout to be set and positive, but
// `timeout` is not surfaced in the HCL grammar (it is runner-owned at
// the eval layer). Without an injection here, every suite whose merged
// config originates from an inline run_config block — the common case
// — would false-fail both dry-run validation and the live-run pre-
// flight check with a misleading "timeout is required" error. Inject
// the runner's default Timeout when the merged config does not already
// pin one; a value already present in a JSON baseline (loaded via
// run_config_file) or set by an overlay survives unchanged.
func buildMergedConfig(baseline *types.RunConfig, overlay *types.RunConfigOverrides) (*types.RunConfig, error) {
	if baseline == nil {
		return nil, nil
	}
	// JSON round-trip clones every field, including pointer-typed
	// sub-structs. Cheap enough for RunConfig-sized blobs and avoids
	// hand-maintaining a deep copier that would silently miss new fields.
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

// writeMergedConfig marshals the merged RunConfig to path. The file is
// created inside the per-task tmpdir and is removed with the rest of the
// workspace via the caller's defer os.RemoveAll. Permissions are 0o600 —
// although secret values are not present (only secret:// references), the
// file still carries the operator's chosen run posture and should not be
// world-readable on shared CI runners.
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
// alongside the trace and harness output streams. Redact() rewrites every
// secret:// reference to "secret://[REDACTED]" before the file lands on
// disk so a retained artifact never carries a resolved secret out of the
// process. Retention errors are reported on stderr to match retainArtifacts
// but never mask the TaskResult.
//
// A stderr warning fires for any apiKeyRef that does not use the
// secret:// scheme. The invariant is already enforced at HCL parse time
// (eval/spec.validateInlineAPIKeyRefs) and again by
// types.ValidateRunConfig before the harness is spawned, but the warning
// here is the third defensive layer: if a regression ever lets a raw
// value through both prior gates, Redact() will quietly rewrite it to
// the sentinel — masking the misconfiguration from anyone inspecting the
// artifact tree. The warning makes the bypass visible.
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
	// 0o600 matches the on-disk live config: although Redact() strips
	// secrets, the file still carries operational posture (provider
	// type, model, network allowlists, permission policy). On shared
	// CI runners the artifact tree may be inspectable by other jobs;
	// 0o600 narrows that exposure.
	if err := os.WriteFile(filepath.Join(taskDir, "run_config.redacted.json"), data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: redacted config write: %v\n", taskID, err)
	}
}

// warnIfRawAPIKeyRef emits a stderr warning for any apiKeyRef field in
// the merged config that does not use the secret:// scheme. The
// invariant is enforced at HCL parse time and again by
// types.ValidateRunConfig before the harness is spawned, so reaching
// this code with a raw value means both prior gates were bypassed —
// likely a regression in either layer. Without this warning, Redact()
// would silently rewrite the raw value to "[REDACTED]" in the audit
// artifact and an operator inspecting the artifact tree would have no
// signal that the configuration was rejected for that reason.
//
// The field list here must stay in lockstep with the redactor in
// types.RunConfig.Redact() and with eval/spec.validateInlineAPIKeyRefs.
// A new secret-bearing field added to RunConfig without extending this
// helper would create a defense-in-depth hole.
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
// invocation. With a nil baseline the task behaves as it does today
// (pass, "dry run — skipped"). With a baseline, the runner builds the
// merged RunConfig and calls ValidateRunConfig; a validation failure
// flips the outcome to "error" and surfaces the validator's message.
// Other tasks in the same dry-run pass are unaffected (each task is
// validated independently).
func dryRunTask(task types.EvalTask, baseline *types.RunConfig) eval.TaskResult {
	merged, err := buildMergedConfig(baseline, task.RunConfigOverrides)
	if err != nil {
		return eval.TaskResult{
			TaskID:  task.ID,
			Outcome: "error",
			Error:   err.Error(),
			JudgeVerdict: eval.JudgeVerdict{
				Passed: false,
				Reason: err.Error(),
			},
		}
	}
	if merged != nil {
		if vErr := types.ValidateRunConfig(merged); vErr != nil {
			return eval.TaskResult{
				TaskID:  task.ID,
				Outcome: "error",
				Error:   vErr.Error(),
				JudgeVerdict: eval.JudgeVerdict{
					Passed: false,
					Reason: vErr.Error(),
				},
			}
		}
	}
	return eval.TaskResult{
		TaskID:  task.ID,
		Outcome: "pass",
		JudgeVerdict: eval.JudgeVerdict{
			Passed: true,
			Reason: "dry run — skipped",
		},
	}
}

// buildResult constructs a TaskResult from a trace and verdict.
func buildResult(taskID string, start time.Time, trace *types.RunTrace, verdict eval.JudgeVerdict) eval.TaskResult {
	outcome := "fail"
	if verdict.Passed {
		outcome = "pass"
	}
	return eval.TaskResult{
		TaskID:       taskID,
		Outcome:      outcome,
		Trace:        trace,
		JudgeVerdict: verdict,
		DurationMs:   time.Since(start).Milliseconds(),
	}
}
