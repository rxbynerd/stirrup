package runner

import (
	"context"
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
// LLM-backed judges. When the judge cannot rule, the returned result is the
// "error" outcome (keeping an LLM judge's error verdict) alongside the error.
func ReplayRecording(ctx context.Context, recording types.RunRecording, task types.EvalTask, workspaceDir string, opts judge.Options) (eval.TaskResult, error) {
	start := time.Now()

	trace := &recording.FinalOutcome
	verdict, err := judge.Evaluate(ctx, task.Judge, judge.JudgeContext{
		WorkspaceDir: workspaceDir,
		Trace:        trace,
		Options:      opts,
	})
	if err != nil {
		return judgeErrorResult(task.ID, start, verdict, err), err
	}

	outcome := "fail"
	if verdict.Passed {
		outcome = "pass"
	}
	return eval.TaskResult{
		TaskID:       task.ID,
		Outcome:      outcome,
		Trace:        trace,
		JudgeVerdict: verdict,
		DurationMs:   time.Since(start).Milliseconds(),
	}, nil
}
