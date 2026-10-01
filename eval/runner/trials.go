package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// resolveTrials picks the runs per task: the invocation's value when set,
// else the suite's trials attribute, else one.
func resolveTrials(cfg RunConfig, suite types.EvalSuite) int {
	if cfg.Trials > 0 {
		return cfg.Trials
	}
	if suite.Trials > 0 {
		return suite.Trials
	}
	return 1
}

// runTrial executes one trial of a task. Each call gets its own workspace
// and harness subprocess from runTask. With more than one trial, artifacts
// land in <suiteArtifactDir>/<taskID>/trial-<n>/: runTask retains into
// <dir>/<taskID>/, so it is pointed at a staging directory whose task
// subdirectory is then renamed into place.
func runTrial(ctx context.Context, task types.EvalTask, cfg RunConfig, suiteArtifactDir string, baseline *types.RunConfig, trial, trials int) eval.TaskResult {
	if trials == 1 || suiteArtifactDir == "" {
		return runTask(ctx, task, cfg, suiteArtifactDir, baseline)
	}

	taskDir := filepath.Join(suiteArtifactDir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q trial %d: mkdir: %v\n", task.ID, trial, err)
		return runTask(ctx, task, cfg, "", baseline)
	}
	staging, err := os.MkdirTemp(taskDir, fmt.Sprintf(".trial-%d-", trial))
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q trial %d: staging: %v\n", task.ID, trial, err)
		return runTask(ctx, task, cfg, "", baseline)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	result := runTask(ctx, task, cfg, staging, baseline)

	dest := filepath.Join(taskDir, fmt.Sprintf("trial-%d", trial))
	if err := os.RemoveAll(dest); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q trial %d: clearing %s: %v\n", task.ID, trial, dest, err)
		return result
	}
	if err := os.Rename(filepath.Join(staging, task.ID), dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q trial %d: %v\n", task.ID, trial, err)
	}
	return result
}

// aggregateTrials folds a task's trial results into one TaskResult. A
// single trial is returned as-is with its PassFraction set. Otherwise the
// Outcome follows the majority rule, DurationMs is the sum across trials,
// and Trace, JudgeVerdict, and Error come from the first trial of the
// majority outcome; without a strict majority the verdict and error
// describe the split instead.
func aggregateTrials(taskID string, runs []eval.TaskResult) eval.TaskResult {
	if len(runs) == 1 {
		r := runs[0]
		r.PassFraction = r.Counts().PassFraction()
		return r
	}

	res := eval.TaskResult{TaskID: taskID, Trials: make([]eval.TrialResult, len(runs))}
	for i, r := range runs {
		turns := 0
		if r.Trace != nil {
			turns = r.Trace.Turns
		}
		res.Trials[i] = eval.TrialResult{
			Trial:        i + 1,
			Outcome:      r.Outcome,
			JudgeVerdict: r.JudgeVerdict,
			Error:        r.Error,
			DurationMs:   r.DurationMs,
			Turns:        turns,
		}
		res.DurationMs += r.DurationMs
	}

	counts := res.Counts()
	res.Outcome = counts.Outcome()
	res.PassFraction = counts.PassFraction()

	if hasStrictMajority(counts, res.Outcome) {
		for _, r := range runs {
			if r.Outcome == res.Outcome {
				res.Trace = r.Trace
				res.JudgeVerdict = r.JudgeVerdict
				res.Error = r.Error
				return res
			}
		}
	}
	split := fmt.Sprintf("no majority across %d trials: %d passed, %d failed, %d errored",
		counts.Total(), counts.Pass, counts.Fail, counts.Error)
	res.Error = split
	res.JudgeVerdict = eval.JudgeVerdict{Passed: false, Reason: split}
	return res
}

func hasStrictMajority(c eval.TrialCounts, outcome string) bool {
	n := map[string]int{"pass": c.Pass, "fail": c.Fail, "error": c.Error}[outcome]
	return 2*n > c.Total()
}

// suitePassRate is the mean per-task pass fraction.
func suitePassRate(results []eval.TaskResult) float64 {
	if len(results) == 0 {
		return 0
	}
	var sum float64
	for _, r := range results {
		sum += r.PassFraction
	}
	return sum / float64(len(results))
}
