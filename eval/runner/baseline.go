package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rxbynerd/stirrup/eval/judge"
)

// initBaselineRepo makes the seeded workspace a git repository with a single
// "baseline" commit, so a diff-review judge can diff the agent's changes
// against it. It is applied only to tasks that need it: a task with a repo
// already has history, and other judges do not read git state.
func initBaselineRepo(ctx context.Context, dir string) error {
	steps := [][]string{
		{"init", "-q"},
		{"config", "user.name", "stirrup-eval"},
		{"config", "user.email", "stirrup-eval@localhost.invalid"},
		{"add", "--all"},
		{"commit", "-q", "--allow-empty", "--no-verify", "-m", "baseline"},
	}
	for _, step := range steps {
		args := append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, step...)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = judge.GitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("creating git baseline: git %s: %w\n%s", step[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
