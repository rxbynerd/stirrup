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

// defaultTaskTimeoutSeconds is the per-task timeout used when the merged
// RunConfig does not pin one, matching the legacy `--timeout 300` default.
const defaultTaskTimeoutSeconds = 300

// RunConfig configures how the runner executes tasks.
type RunConfig struct {
	// HarnessPath is the path to the harness binary for live runs.
	// If empty, defaults to "stirrup" on PATH.
	HarnessPath string

	// OutputDir, when non-empty, enables per-task artifact retention. The
	// runner writes trace.jsonl, harness.stdout.txt, and harness.stderr.txt
	// for every task under <OutputDir>/<suiteID>/<taskID>/. The temporary
	// workspace itself is not copied.
	OutputDir string

	// Concurrency caps the number of tasks executed in parallel. Values <= 0
	// fall back to 1 (sequential). Values larger than the task count are
	// capped at len(tasks) so we never spawn idle workers.
	Concurrency int

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

	// AnthropicWIF, when populated, forwards Anthropic Workload Identity
	// Federation flags to every harness invocation. See
	// docs/anthropic-wif.md.
	AnthropicWIF AnthropicWIFConfig
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

	// Feed jobs; honour ctx cancellation so the dispatcher doesn't
	// deadlock if all workers have exited.
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

	// The trace file lives outside the workspace: writing it inside would
	// let the agent read its own in-progress trace.jsonl via
	// list_directory/read_file, breaking task hermeticity (observed
	// manufacturing spurious judge passes).
	traceDir, err := os.MkdirTemp("", "eval-trace-"+task.ID+"-")
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

		configPath = filepath.Join(tmpDir, "runconfig.json")
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
// the last well-formed line, via the shared reader in types/trace.
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
