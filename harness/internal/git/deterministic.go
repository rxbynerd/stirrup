package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

// DeterministicGitStrategy is a GitStrategy that creates a branch per task,
// stages and commits all changes on checkpoint and again on finalise, and
// reports the final branch, SHA and base SHA. All git commands run on the
// host against the configured workspace with HardeningFlags and the harness
// identity.
type DeterministicGitStrategy struct {
	workspace string
	branch    string
	baseSHA   string
	setupDone bool
}

// NewDeterministicGitStrategy returns a new DeterministicGitStrategy.
func NewDeterministicGitStrategy() *DeterministicGitStrategy {
	return &DeterministicGitStrategy{}
}

// Setup creates a new branch named stirrup/{taskID} from the current HEAD
// in the given workspace directory. An empty workspace means the process's
// current directory.
func (d *DeterministicGitStrategy) Setup(ctx context.Context, workspace string, taskID string) error {
	d.workspace = workspace
	d.branch = "stirrup/" + taskID
	d.baseSHA = ""
	d.setupDone = false

	if err := d.git(ctx, "checkout", "-b", d.branch); err != nil {
		return fmt.Errorf("create branch %q: %w", d.branch, err)
	}
	d.setupDone = true
	// Fails only on an unborn branch, which has no base commit.
	if sha, err := d.gitOutput(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		d.baseSHA = sha
	}
	return nil
}

// Checkpoint stages all changes and commits with the given message. If there
// are no changes to commit, it silently succeeds.
func (d *DeterministicGitStrategy) Checkpoint(ctx context.Context, message string) error {
	if err := d.git(ctx, "add", "-A"); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}

	// "diff --cached --quiet" exits 0 when nothing is staged.
	if err := d.git(ctx, "diff", "--cached", "--quiet"); err == nil {
		return nil
	}

	if err := d.git(ctx, "commit", "--no-verify", "-m", message); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Finalise commits outstanding work as "Final checkpoint (outcome:
// <outcome>)", then reports the branch, HEAD SHA, base SHA and whether the
// working tree is still dirty. Each field is read even when an earlier step
// failed, and every failure is joined into the returned error.
func (d *DeterministicGitStrategy) Finalise(ctx context.Context, outcome string) (*types.GitHandoff, error) {
	handoff := &types.GitHandoff{BaseSHA: d.baseSHA}
	if !d.setupDone {
		return handoff, errors.New("git setup has not run")
	}

	var errs []error
	if err := d.Checkpoint(ctx, fmt.Sprintf("Final checkpoint (outcome: %s)", outcome)); err != nil {
		errs = append(errs, fmt.Errorf("final checkpoint: %w", err))
	}

	if sha, err := d.gitOutput(ctx, "rev-parse", "HEAD"); err != nil {
		errs = append(errs, fmt.Errorf("rev-parse HEAD: %w", err))
	} else {
		handoff.SHA = sha
	}

	if branch, err := d.gitOutput(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		errs = append(errs, fmt.Errorf("get branch name: %w", err))
	} else {
		handoff.Branch = branch
	}

	if status, err := d.gitOutput(ctx, "status", "--porcelain"); err != nil {
		errs = append(errs, fmt.Errorf("inspect working tree: %w", err))
	} else {
		handoff.Dirty = status != ""
	}

	return handoff, errors.Join(errs...)
}

func (d *DeterministicGitStrategy) git(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", harnessArgs(args)...)
	cmd.Dir = d.workspace
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// gitOutput runs a git command and returns its trimmed stdout.
func (d *DeterministicGitStrategy) gitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", harnessArgs(args)...)
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

func harnessArgs(args []string) []string {
	out := append([]string{}, HardeningFlags...)
	out = append(out, IdentityFlags()...)
	return append(out, args...)
}
