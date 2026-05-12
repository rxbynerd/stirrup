// Package runner orchestrates execution of eval suites, running tasks through
// the harness binary and applying judges to determine pass/fail outcomes.
package runner

import (
	"bufio"
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
)

// maxRunConfigFileBytes mirrors the harness's loadRunConfigFile cap
// (harness/cmd/stirrup/cmd/harness.go ~ maxConfigFileBytes). A RunConfig
// is at most a few KB; anything in the MB range is almost certainly a
// mistake (a symlink to /dev/zero, a binary pasted into the path, etc.).
// Kept in sync with the harness rather than factored out because
// harness/cmd/... is internal to the stirrup binary and the helper has
// no other consumers — duplicating ~25 lines keeps the cross-module
// surface area smaller than exporting it would.
const maxRunConfigFileBytes int64 = 1 << 20 // 1 MiB

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

	if cfg.DryRun {
		return dryRunSuite(suite, runID, startedAt), nil
	}

	results := runTasksConcurrently(ctx, suite, cfg, suiteArtifactDir)

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
func runTasksConcurrently(ctx context.Context, suite types.EvalSuite, cfg RunConfig, suiteArtifactDir string) []eval.TaskResult {
	tasks := suite.Tasks
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
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j.idx] = runTask(ctx, suite, j.task, cfg, suiteArtifactDir)
			}
		}()
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
// removed.
func runTask(ctx context.Context, suite types.EvalSuite, task types.EvalTask, cfg RunConfig, suiteArtifactDir string) eval.TaskResult {
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

	traceFile := filepath.Join(tmpDir, "trace.jsonl")

	args := []string{
		"harness",
		"--prompt", task.Prompt,
		"--workspace", workspaceDir,
		"--trace", traceFile,
		"--timeout", "300",
	}
	// Only forward --mode when the task explicitly pins one at the task
	// level. Suite-level baseline mode (or any merged-config mode) comes
	// through --config; the harness's applyOverrides treats every flag
	// present on the command line as authoritative, so passing a default
	// here would silently shadow a suite-level run_config { mode = "..." }.
	if task.Mode != "" {
		args = append(args, "--mode", task.Mode)
	}

	// Materialise the merged RunConfig (suite baseline + task overrides)
	// into the task's tmpDir and hand it to the harness via --config. The
	// harness's documented precedence rule keeps the runner's explicit
	// flags (--prompt, --mode, --workspace, --trace, --timeout)
	// authoritative even when --config is set. Cleanup rides on the
	// existing tmpDir os.RemoveAll defer.
	mergedCfg, mergeErr := mergeRunConfig(suite, task)
	if mergeErr != nil {
		return errorResult(task.ID, start, fmt.Errorf("merging run-config: %w", mergeErr))
	}
	if mergedCfg != nil {
		cfgPath := filepath.Join(tmpDir, "run_config.json")
		data, err := json.Marshal(mergedCfg)
		if err != nil {
			return errorResult(task.ID, start, fmt.Errorf("marshalling merged run-config: %w", err))
		}
		if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
			return errorResult(task.ID, start, fmt.Errorf("writing merged run-config: %w", err))
		}
		args = append(args, "--config", cfgPath)
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
		retainArtifacts(suiteArtifactDir, task.ID, traceFile, stdoutBuf.Bytes(), stderrBuf.Bytes(), mergedCfg)
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
	})
	if err != nil {
		return errorResult(task.ID, start, fmt.Errorf("judge failed: %w", err))
	}

	return buildResult(task.ID, start, trace, verdict)
}

// retainArtifacts copies the per-task harness output and trace into the
// suite's artifact directory. taskID has already been validated as a safe
// single-segment path. Retention errors are intentionally swallowed — they
// must not mask the underlying TaskResult — but failures are still reported
// via stderr so an operator can see when the artifact tree is incomplete.
//
// When mergedCfg is non-nil the redacted form of the merged RunConfig is
// also written as run_config.redacted.json so an operator can audit the
// exact (post-`secret://` redaction) configuration the harness was
// handed. Redact() is invariant in the codebase — chunk B re-uses it
// rather than open-coding the secret-stripping rules.
func retainArtifacts(suiteArtifactDir, taskID, traceFile string, stdout, stderr []byte, mergedCfg *types.RunConfig) {
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
	if mergedCfg != nil {
		// Belt-and-braces: parse-time and validate-time both reject raw
		// credentials in api_key_ref, but if a misconfiguration ever
		// slipped through, Redact() would quietly rewrite it to the
		// sentinel and the redacted artifact would hide the bad config
		// from audit. Emit a warning to stderr before redacting so an
		// operator inspecting the artifact tree sees the misconfiguration.
		warnIfRawAPIKeyRef(taskID, mergedCfg)
		redacted := mergedCfg.Redact()
		data, err := json.MarshalIndent(redacted, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: run-config marshal: %v\n", taskID, err)
			return
		}
		if err := os.WriteFile(filepath.Join(taskDir, "run_config.redacted.json"), data, 0o640); err != nil {
			fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: run-config: %v\n", taskID, err)
		}
	}
}

// warnIfRawAPIKeyRef emits a stderr warning when the merged config
// carries an apiKeyRef that does not use the secret:// scheme. The
// invariant is enforced at parse and validate time; this is a
// last-line safeguard so that if a regression ever lets a raw value
// through, an operator inspecting the audit artifact sees the
// misconfiguration instead of a silently-redacted line.
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

// parseTraceFile reads a JSONL trace file and returns the RunTrace from the
// last line.
func parseTraceFile(path string) (*types.RunTrace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening trace file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var lastLine string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			lastLine = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading trace file: %w", err)
	}
	if lastLine == "" {
		return nil, fmt.Errorf("trace file is empty")
	}

	var trace types.RunTrace
	if err := json.Unmarshal([]byte(lastLine), &trace); err != nil {
		return nil, fmt.Errorf("parsing trace JSON: %w", err)
	}

	return &trace, nil
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

// dryRunSuite materialises the dry-run SuiteResult: for suites with no
// run-config surface every task is recorded as a vacuous pass (today's
// behaviour); for suites that supply a baseline or per-task overrides
// the merged RunConfig for each task is run through ValidateRunConfig
// and any validation error is surfaced per-task. The dry-run path
// never invokes the harness binary.
func dryRunSuite(suite types.EvalSuite, runID string, startedAt time.Time) eval.SuiteResult {
	hasConfigSurface := suite.RunConfig != nil
	if !hasConfigSurface {
		for _, t := range suite.Tasks {
			if t.RunConfigOverrides != nil {
				hasConfigSurface = true
				break
			}
		}
	}

	tasks := make([]eval.TaskResult, len(suite.Tasks))
	passCount := 0
	for i, t := range suite.Tasks {
		if !hasConfigSurface {
			tasks[i] = eval.TaskResult{
				TaskID:  t.ID,
				Outcome: "pass",
				JudgeVerdict: eval.JudgeVerdict{
					Passed: true,
					Reason: "dry run — skipped",
				},
			}
			passCount++
			continue
		}

		merged, mergeErr := mergeRunConfig(suite, t)
		if mergeErr != nil {
			tasks[i] = eval.TaskResult{
				TaskID:  t.ID,
				Outcome: "error",
				Error:   mergeErr.Error(),
				JudgeVerdict: eval.JudgeVerdict{
					Passed: false,
					Reason: fmt.Sprintf("dry run — merging run-config: %v", mergeErr),
				},
			}
			continue
		}
		if merged == nil {
			// hasConfigSurface implies at least one task had overrides; tasks
			// without their own overrides + no suite baseline still take this
			// branch. Treat them as vacuous-pass: there's nothing to validate.
			tasks[i] = eval.TaskResult{
				TaskID:  t.ID,
				Outcome: "pass",
				JudgeVerdict: eval.JudgeVerdict{
					Passed: true,
					Reason: "dry run — skipped (no merged config)",
				},
			}
			passCount++
			continue
		}
		// ValidateRunConfig requires Timeout to be set. The runner always
		// passes --timeout 300 to the harness, and `timeout` is not
		// surfaced in the HCL grammar (it is runner-owned), so the merged
		// config built from an inline run_config block never carries one.
		// Inject the runner's default before validating so dry-run does
		// not falsely fail every suite that uses an inline block. A
		// non-zero timeout already present in a JSON baseline survives.
		if merged.Timeout == nil {
			defaultTimeout := 300
			merged.Timeout = &defaultTimeout
		}
		if err := types.ValidateRunConfig(merged); err != nil {
			tasks[i] = eval.TaskResult{
				TaskID:  t.ID,
				Outcome: "fail",
				JudgeVerdict: eval.JudgeVerdict{
					Passed: false,
					Reason: fmt.Sprintf("dry run — RunConfig validation failed: %v", err),
				},
			}
			continue
		}
		tasks[i] = eval.TaskResult{
			TaskID:  t.ID,
			Outcome: "pass",
			JudgeVerdict: eval.JudgeVerdict{
				Passed: true,
				Reason: "dry run — RunConfig validated",
			},
		}
		passCount++
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
	}
}

// mergeRunConfig resolves the *types.RunConfig the runner should hand
// the harness for a given task by layering:
//
//  1. the suite-level baseline (loaded from `suite.RunConfig.File` if
//     set, otherwise materialised from `suite.RunConfig.Inline`),
//  2. any sparse `task.RunConfigOverrides` on top.
//
// It returns (nil, nil) when neither the suite nor the task supplies
// any run-config surface — that case maps to the legacy invocation
// path where the runner omits `--config` entirely. Returning a
// pointer (rather than a value) lets the caller distinguish "no
// merged config" from "merged config with all zero-value fields".
func mergeRunConfig(suite types.EvalSuite, task types.EvalTask) (*types.RunConfig, error) {
	if suite.RunConfig == nil && task.RunConfigOverrides == nil {
		return nil, nil
	}

	var cfg types.RunConfig
	if suite.RunConfig != nil {
		switch {
		case suite.RunConfig.File != "":
			loaded, err := loadRunConfigFile(suite.RunConfig.File)
			if err != nil {
				return nil, fmt.Errorf("loading suite run-config file: %w", err)
			}
			cfg = *loaded
		case suite.RunConfig.Inline != nil:
			applyOverrides(&cfg, suite.RunConfig.Inline)
		}
	}

	if task.RunConfigOverrides != nil {
		applyOverrides(&cfg, task.RunConfigOverrides)
	}

	return &cfg, nil
}

// applyOverrides applies a sparse RunConfigOverrides on top of an
// existing RunConfig. A nil pointer override means "do not touch this
// field"; a non-nil pointer (or non-empty string) replaces the
// baseline value. The semantics match the existing
// types.RunConfigOverrides precedent used by experiments.
func applyOverrides(cfg *types.RunConfig, ov *types.RunConfigOverrides) {
	// Defensive nil guard: every current call site in mergeRunConfig
	// already nil-checks ov, so this branch is unreachable today and
	// shows up as the only uncovered line in this function. Kept on
	// purpose — if a future call site forgets the check, applyOverrides
	// would nil-deref on `ov.Mode`, and the failure mode would be
	// nondeterministic rather than a clean no-op. Removing it would
	// flip a clear contract ("safe to call with nil") into a sharp edge.
	if ov == nil {
		return
	}
	if ov.Mode != "" {
		cfg.Mode = ov.Mode
	}
	if ov.Provider != nil {
		cfg.Provider = *ov.Provider
	}
	if ov.ModelRouter != nil {
		cfg.ModelRouter = *ov.ModelRouter
	}
	if ov.ContextStrategy != nil {
		cfg.ContextStrategy = *ov.ContextStrategy
	}
	if ov.EditStrategy != nil {
		cfg.EditStrategy = *ov.EditStrategy
	}
	if ov.Verifier != nil {
		cfg.Verifier = *ov.Verifier
	}
	if ov.MaxTurns != nil {
		cfg.MaxTurns = *ov.MaxTurns
	}
}

// loadRunConfigFile reads a JSON RunConfig file at path with the same
// guard rails as the harness's own loader: size capped at
// maxRunConfigFileBytes, unknown fields rejected so config typos
// surface immediately. Kept in sync by hand with
// harness/cmd/stirrup/cmd/harness.go#loadRunConfigFile (the canonical
// implementation); if that helper changes its cap or strictness, mirror
// it here.
func loadRunConfigFile(path string) (*types.RunConfig, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("reading config file %q: is a directory", path)
	}
	if info.Size() > maxRunConfigFileBytes {
		return nil, fmt.Errorf("reading config file %q: %d bytes exceeds %d byte cap",
			path, info.Size(), maxRunConfigFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("parsing config file %q: file is empty", path)
	}
	var cfg types.RunConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}
	return &cfg, nil
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
