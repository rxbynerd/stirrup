// Package runner orchestrates execution of eval suites, running tasks through
// the harness binary and applying judges to determine pass/fail outcomes.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// OutputDir is created before running so callers can write suite artifacts there.
	OutputDir string

	// Concurrency is the desired number of parallel tasks.
	// TODO: not yet implemented; tasks currently run sequentially.
	Concurrency int

	// DryRun if true, validates the suite without executing tasks.
	DryRun bool
}

// RunSuite executes all tasks in a suite and returns the aggregate result.
func RunSuite(ctx context.Context, suite types.EvalSuite, cfg RunConfig) (eval.SuiteResult, error) {
	if err := validateSuite(suite); err != nil {
		return eval.SuiteResult{}, err
	}

	if cfg.HarnessPath == "" {
		cfg.HarnessPath = "stirrup"
	}

	if cfg.OutputDir != "" {
		if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
			return eval.SuiteResult{}, fmt.Errorf("creating output directory: %w", err)
		}
	}

	runID := fmt.Sprintf("eval-%d", time.Now().UnixMilli())
	startedAt := time.Now()

	if cfg.DryRun {
		tasks := make([]eval.TaskResult, len(suite.Tasks))
		for i, t := range suite.Tasks {
			tasks[i] = eval.TaskResult{
				TaskID:  t.ID,
				Outcome: "pass",
				JudgeVerdict: eval.JudgeVerdict{
					Passed: true,
					Reason: "dry run — skipped",
				},
			}
		}
		return eval.SuiteResult{
			SuiteID:     suite.ID,
			RunID:       runID,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			Tasks:       tasks,
			PassRate:    1.0,
		}, nil
	}

	tasks := make([]eval.TaskResult, 0, len(suite.Tasks))
	for _, t := range suite.Tasks {
		result := runTask(ctx, t, cfg)
		tasks = append(tasks, result)
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

// validateSuite checks that a suite has the minimum required fields.
func validateSuite(suite types.EvalSuite) error {
	if suite.ID == "" {
		return fmt.Errorf("suite ID is required")
	}
	if len(suite.Tasks) == 0 {
		return fmt.Errorf("suite must contain at least one task")
	}
	return nil
}

// runTask executes a single eval task and returns the result.
func runTask(ctx context.Context, task types.EvalTask, cfg RunConfig) eval.TaskResult {
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
		"--mode", taskMode(task),
		"--workspace", workspaceDir,
		"--trace", traceFile,
		"--timeout", "300",
	}

	cmd := exec.CommandContext(ctx, cfg.HarnessPath, args...)
	cmd.Dir = workspaceDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		// The harness may still have produced a trace even on failure.
		// Try to parse it before giving up entirely.
		trace, traceErr := parseTraceFile(traceFile)
		if traceErr != nil {
			return errorResult(task.ID, start, fmt.Errorf("harness failed: %w\noutput: %s", err, output))
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
