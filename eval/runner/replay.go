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
// post-run file state before calling this function.
func ReplayRecording(ctx context.Context, recording types.RunRecording, task types.EvalTask, workspaceDir string) (eval.TaskResult, error) {
	start := time.Now()

	verdict, err := judge.Evaluate(ctx, task.Judge, judge.JudgeContext{
		WorkspaceDir: workspaceDir,
	})
	if err != nil {
		return eval.TaskResult{}, err
	}

	outcome := "fail"
	if verdict.Passed {
		outcome = "pass"
	}

	trace := &recording.FinalOutcome
	return eval.TaskResult{
		TaskID:       task.ID,
		Outcome:      outcome,
		Trace:        trace,
		JudgeVerdict: verdict,
		DurationMs:   time.Since(start).Milliseconds(),
	}, nil
}
