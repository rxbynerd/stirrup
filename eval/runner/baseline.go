package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rxbynerd/stirrup/eval/judge"
)

// createJudgeBaseline commits the seeded workspace to a judge-owned bare
// repository in a new 0700 temp dir outside the workspace, so diff-review
// judges diff the agent's changes against it without git ever reading the
// workspace's own .git. The returned cleanup removes the temp dir.
func createJudgeBaseline(ctx context.Context, taskID, workspaceDir string) (*judge.Baseline, func(), error) {
	dir, err := os.MkdirTemp("", "evaljudge-"+taskID+"-")
	if err != nil {
		return nil, nil, fmt.Errorf("creating judge git dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	base, err := judge.CreateBaseline(ctx, workspaceDir, filepath.Join(dir, "judge.git"))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating diff-review baseline: %w", err)
	}
	return &base, cleanup, nil
}

// retainJudgeBaseline writes the baseline sidecar and bundle beside the
// task's retained trace so `replay --judge-baseline` can diff against the
// same commit. Retention errors are reported on stderr but never mask the
// TaskResult.
func retainJudgeBaseline(ctx context.Context, suiteArtifactDir, taskID string, base judge.Baseline) {
	taskDir := filepath.Join(suiteArtifactDir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: mkdir: %v\n", taskID, err)
		return
	}
	if err := judge.WriteBaselineSidecar(ctx, base, taskDir); err != nil {
		fmt.Fprintf(os.Stderr, "eval: artifact retention failed for task %q: judge baseline: %v\n", taskID, err)
	}
}
