package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

// ReplayRecording re-evaluates a recorded run against a judge.
// It does not re-execute the harness — it only applies the judge
// to the workspace state described in the recording.
//
// The caller is responsible for setting up workspaceDir with the
// post-run file state before calling this function. opts configures
// LLM-backed judges. baselineFile, when non-empty, is a judge-baseline.json
// sidecar retained by `run`; diff-review judges then diff against that
// baseline, and otherwise against the HEAD of workspaceDir's own
// repository. A judge tree that judge.ValidateTree rejects is not evaluated.
// When the judge cannot rule, the returned result has the "error"
// outcome. An LLM judge also returns the error, and its error verdict is kept;
// a composite reports the failure through its verdict's status and returns a
// nil error, so callers must check the outcome as well as the error.
func ReplayRecording(ctx context.Context, recording types.RunRecording, task types.EvalTask, workspaceDir string, opts judge.Options, baselineFile string) (eval.TaskResult, error) {
	start := time.Now()
	if err := judge.ValidateTree(task.Judge); err != nil {
		err = fmt.Errorf("task %q: %w", task.ID, err)
		return errorResult(task.ID, start, err), err
	}

	jctx := judge.JudgeContext{
		WorkspaceDir: workspaceDir,
		Trace:        &recording.FinalOutcome,
		Options:      opts,
	}
	if baselineFile != "" && judge.ContainsType(task.Judge, "diff-review") {
		dir, err := os.MkdirTemp("", "evaljudge-"+task.ID+"-")
		if err != nil {
			err = fmt.Errorf("creating judge git dir: %w", err)
			return errorResult(task.ID, start, err), err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		base, err := judge.LoadBaselineSidecar(ctx, baselineFile, filepath.Join(dir, "judge.git"))
		if err != nil {
			err = fmt.Errorf("loading judge baseline: %w", err)
			return errorResult(task.ID, start, err), err
		}
		jctx.Baseline = &base
	}

	verdict, err := judge.Evaluate(ctx, task.Judge, jctx)
	if err != nil {
		return judgeErrorResult(task.ID, start, jctx.Trace, verdict, err), err
	}

	return buildResult(task.ID, start, jctx.Trace, verdict), nil
}
