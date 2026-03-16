package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// DeterministicGitStrategy is a GitStrategy that creates a branch per task,
// stages and commits all changes on checkpoint, and reports the final
// branch/SHA on finalise. All git commands run against the configured workspace.
type DeterministicGitStrategy struct {
	workspace string
	branch    string
}

// NewDeterministicGitStrategy returns a new DeterministicGitStrategy.
func NewDeterministicGitStrategy() *DeterministicGitStrategy {
	return &DeterministicGitStrategy{}
}

// Setup creates a new branch named stirrup/{taskID} from the current HEAD
// in the given workspace directory.
func (d *DeterministicGitStrategy) Setup(ctx context.Context, workspace string, taskID string) error {
	d.workspace = workspace
	d.branch = "stirrup/" + taskID

	if err := d.git(ctx, "checkout", "-b", d.branch); err != nil {
		return fmt.Errorf("create branch %q: %w", d.branch, err)
	}
	return nil
}

// Checkpoint stages all changes and commits with the given message. If there
// are no changes to commit, it silently succeeds.
func (d *DeterministicGitStrategy) Checkpoint(ctx context.Context, message string) error {
	if err := d.git(ctx, "add", "-A"); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}

	// Check whether there is anything to commit. "diff --cached --quiet"
	// exits 0 when the index matches HEAD (nothing staged).
	if err := d.git(ctx, "diff", "--cached", "--quiet"); err == nil {
		// Nothing to commit — this is not an error.
		return nil
	}

	if err := d.git(ctx, "commit", "-m", message); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Finalise returns the current branch name and HEAD SHA.
func (d *DeterministicGitStrategy) Finalise(ctx context.Context) (*GitResult, error) {
	sha, err := d.gitOutput(ctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}

	branch, err := d.gitOutput(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("get branch name: %w", err)
	}

	return &GitResult{
		Branch: branch,
		SHA:    sha,
	}, nil
}

// git runs a git command in the workspace directory.
func (d *DeterministicGitStrategy) git(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = d.workspace
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// gitOutput runs a git command and returns its trimmed stdout.
func (d *DeterministicGitStrategy) gitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = d.workspace
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
