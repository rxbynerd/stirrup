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
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j.idx] = runTask(ctx, j.task, cfg, suiteArtifactDir, baseline)
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

	traceFile := filepath.Join(tmpDir, "trace.jsonl")

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
		configPath = filepath.Join(tmpDir, "runconfig.json")
		if err := writeMergedConfig(configPath, merged); err != nil {
			return errorResult(task.ID, start, err)
		}
	}

	args := []string{"harness"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args,
		"--prompt", task.Prompt,
		"--mode", taskMode(task),
		"--workspace", workspaceDir,
		"--trace", traceFile,
		"--timeout", "300",
	)

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

// taskMode returns the mode for a task, defaulting to "execution".
func taskMode(task types.EvalTask) string {
	if task.Mode != "" {
		return task.Mode
	}
	return "execution"
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
	return mergeOverrides(clone, overlay), nil
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
func retainRedactedConfig(suiteArtifactDir, taskID string, cfg *types.RunConfig) {
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
	if err := os.WriteFile(filepath.Join(taskDir, "run_config.redacted.json"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: redacted config write: %v\n", taskID, err)
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
