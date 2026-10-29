package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// commandTimeout bounds one git invocation inside the sandbox.
const commandTimeout = 60 * time.Second

// maxErrorText bounds how much sandbox-produced output an error carries.
const maxErrorText = 512

// exitCommandNotFound is the status sh reports when a command is not on
// PATH.
const exitCommandNotFound = 127

// DeterministicGitStrategy is a GitStrategy that creates a branch per task,
// stages and commits all changes on checkpoint and again on finalise, and
// reports the final branch, SHA and base SHA. Every command runs inside the
// executor's sandbox through Exec with HardeningFlags and the harness
// identity; nothing runs on the harness host.
type DeterministicGitStrategy struct {
	exec      executor.Executor
	root      string
	branch    string
	baseSHA   string
	setupDone bool
}

// NewDeterministicGitStrategy returns a strategy that runs git through
// exec.
func NewDeterministicGitStrategy(exec executor.Executor) *DeterministicGitStrategy {
	return &DeterministicGitStrategy{exec: exec}
}

// Probe reports whether git is runnable inside the executor. It satisfies
// the structural Preflighter contract used by --dry-run.
func (d *DeterministicGitStrategy) Probe(ctx context.Context) error {
	if err := d.checkExecutor(); err != nil {
		return err
	}
	res, err := d.run(ctx, "--version")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exitError(res, "--version")
	}
	if !strings.HasPrefix(strings.TrimSpace(res.Stdout), "git version") {
		return fmt.Errorf("git --version returned unexpected output: %q", truncate(res.Stdout))
	}
	return nil
}

// Setup creates the branch stirrup/{taskID} in the executor's workspace.
// The workspace argument is unused: the executor's own working directory
// is the workspace root, resolved inside the sandbox so the comparison with
// git's notion of the repository root happens on the same filesystem. A
// workspace that is not a repository is initialised with a base commit; a
// workspace that sits inside an enclosing repository is rejected so no
// branch or commit lands outside it.
func (d *DeterministicGitStrategy) Setup(ctx context.Context, _ string, taskID string) error {
	d.root = ""
	d.branch = ""
	d.baseSHA = ""
	d.setupDone = false
	if err := d.checkExecutor(); err != nil {
		return err
	}
	branch := "stirrup/" + taskID
	if !ValidBranchName(branch) {
		return fmt.Errorf("task id %q does not form a valid branch name", taskID)
	}
	root, err := WorkspaceRoot(ctx, d.exec)
	if err != nil {
		return err
	}
	// The root is recorded before the repository checks because every git
	// command names it as a safe directory.
	d.root = root
	if err := d.prepareBranch(ctx, branch); err != nil {
		d.root = ""
		return err
	}
	d.branch = branch
	d.setupDone = true
	// Fails only on an unborn branch, which has no base commit.
	if res, err := d.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil && res.ExitCode == 0 {
		if sha := strings.TrimSpace(res.Stdout); ValidSHA(sha) {
			d.baseSHA = sha
		}
	}
	return nil
}

func (d *DeterministicGitStrategy) prepareBranch(ctx context.Context, branch string) error {
	res, err := d.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	switch {
	case res.ExitCode == 0:
		toplevel := strings.TrimSuffix(res.Stdout, "\n")
		if toplevel != d.root {
			return fmt.Errorf("workspace %s is inside repository %s; refusing to operate on an enclosing repository", d.root, toplevel)
		}
	case isNotRepository(res):
		if err := d.refuseUnresolvableGitEntry(ctx); err != nil {
			return err
		}
		if err := d.initialise(ctx); err != nil {
			return err
		}
	default:
		return exitError(res, "rev-parse --show-toplevel")
	}

	if err := d.mustSucceed(ctx, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("create branch %q: %w", branch, err)
	}
	return nil
}

// Checkpoint stages all changes and commits with the given message. If there
// are no changes to commit, it silently succeeds.
func (d *DeterministicGitStrategy) Checkpoint(ctx context.Context, message string) error {
	if !d.setupDone {
		return errors.New("git setup has not run")
	}
	if err := d.mustSucceed(ctx, "add", "-A"); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}

	// Exit 0 means nothing is staged.
	res, err := d.run(ctx, "diff", "--cached", "--quiet")
	if err != nil {
		return fmt.Errorf("inspect index: %w", err)
	}
	if res.ExitCode == 0 {
		return nil
	}

	if err := d.mustSucceed(ctx, "commit", "--no-verify", "-m", message); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Finalise commits outstanding work as "Final checkpoint (outcome:
// <outcome>)", then reports the branch, HEAD SHA, base SHA and whether the
// working tree is still dirty. Each field is read even when an earlier step
// failed, and every failure is joined into the returned error. The SHA and
// branch come from the sandbox, so each is validated against its expected
// format before it is reported.
func (d *DeterministicGitStrategy) Finalise(ctx context.Context, outcome string) (*types.GitHandoff, error) {
	handoff := &types.GitHandoff{BaseSHA: d.baseSHA}
	if !d.setupDone {
		return handoff, errors.New("git setup has not run")
	}

	var errs []error
	if err := d.Checkpoint(ctx, fmt.Sprintf("Final checkpoint (outcome: %s)", outcome)); err != nil {
		errs = append(errs, fmt.Errorf("final checkpoint: %w", err))
	}

	if sha, err := d.output(ctx, "rev-parse", "HEAD"); err != nil {
		errs = append(errs, fmt.Errorf("rev-parse HEAD: %w", err))
	} else if !ValidSHA(sha) {
		errs = append(errs, fmt.Errorf("rev-parse HEAD returned %q, not a commit SHA", truncate(sha)))
	} else {
		handoff.SHA = sha
	}

	if branch, err := d.output(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		errs = append(errs, fmt.Errorf("get branch name: %w", err))
	} else if branch == "HEAD" {
		errs = append(errs, errors.New("HEAD is detached; there is no branch to report"))
	} else if !ValidBranchName(branch) {
		errs = append(errs, fmt.Errorf("rev-parse --abbrev-ref HEAD returned %q, not a branch name", truncate(branch)))
	} else {
		handoff.Branch = branch
	}

	if status, err := d.output(ctx, "status", "--porcelain"); err != nil {
		errs = append(errs, fmt.Errorf("inspect working tree: %w", err))
	} else {
		handoff.Dirty = status != ""
	}

	return handoff, errors.Join(errs...)
}

func (d *DeterministicGitStrategy) checkExecutor() error {
	if d.exec == nil {
		return errors.New("deterministic git strategy requires an executor")
	}
	if !d.exec.Capabilities().CanExec {
		return errors.New("deterministic git strategy requires an executor that can run commands")
	}
	return nil
}

// refuseUnresolvableGitEntry fails when git reports no repository although
// the workspace holds a .git entry, typically a worktree or submodule .git
// file naming a host gitdir the sandbox cannot see. Initialising over it
// would replace or hide the operator's repository.
func (d *DeterministicGitStrategy) refuseUnresolvableGitEntry(ctx context.Context) error {
	res, err := d.exec.Exec(ctx, "test -e .git || test -L .git", commandTimeout)
	if err != nil {
		return fmt.Errorf("inspect workspace .git entry: %w", err)
	}
	if res == nil {
		return errors.New("inspect workspace .git entry: executor returned no result")
	}
	if res.ExitCode == 0 {
		return fmt.Errorf("workspace %s has a .git entry that git cannot open inside the sandbox; the workspace must be a self-contained repository, not a worktree or submodule whose gitdir lives elsewhere", d.root)
	}
	return nil
}

// initialise turns a non-repository workspace into a repository whose base
// commit holds the workspace's initial state, so the first checkpoint and
// the final SHA have a base to be compared against.
func (d *DeterministicGitStrategy) initialise(ctx context.Context) error {
	if err := d.mustSucceed(ctx, "init", "--quiet"); err != nil {
		return fmt.Errorf("initialise repository: %w", err)
	}
	res, err := d.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exitError(res, "rev-parse --show-toplevel")
	}
	if toplevel := strings.TrimSuffix(res.Stdout, "\n"); toplevel != d.root {
		return fmt.Errorf("initialised repository root %s does not match workspace %s", toplevel, d.root)
	}
	if err := d.mustSucceed(ctx, "add", "-A"); err != nil {
		return fmt.Errorf("stage initial workspace state: %w", err)
	}
	if err := d.mustSucceed(ctx, "commit", "--no-verify", "--allow-empty", "-m", "Initial workspace state"); err != nil {
		return fmt.Errorf("commit initial workspace state: %w", err)
	}
	return nil
}

// run executes one git command inside the sandbox. Once the root is known
// every command also names it as a safe directory, because the sandbox
// user frequently does not own the mounted workspace.
func (d *DeterministicGitStrategy) run(ctx context.Context, args ...string) (*executor.ExecResult, error) {
	full := args
	if d.root != "" {
		full = append([]string{"-c", "safe.directory=" + d.root}, args...)
	}
	res, err := d.exec.Exec(ctx, CommandLine(full...), commandTimeout)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	if res == nil {
		return nil, fmt.Errorf("git %s: executor returned no result", args[0])
	}
	return res, nil
}

func (d *DeterministicGitStrategy) mustSucceed(ctx context.Context, args ...string) error {
	res, err := d.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exitError(res, strings.Join(args, " "))
	}
	return nil
}

func (d *DeterministicGitStrategy) output(ctx context.Context, args ...string) (string, error) {
	res, err := d.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", exitError(res, strings.Join(args, " "))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// isNotRepository recognises git's refusal to find a repository. Git 2.56
// reports a dangling .git file as an invalid gitfile instead of "not a git
// repository", and both must take the refuse-or-initialise path.
func isNotRepository(res *executor.ExecResult) bool {
	if res.ExitCode == 0 {
		return false
	}
	stderr := strings.ToLower(res.Stderr)
	return strings.Contains(stderr, "not a git repository") ||
		strings.Contains(stderr, "gitfile does not point to a valid repository")
}

func exitError(res *executor.ExecResult, command string) error {
	if res.ExitCode == exitCommandNotFound {
		return fmt.Errorf("git is not available in the sandbox: %s", execFailure(res))
	}
	return fmt.Errorf("git %s: %s", command, execFailure(res))
}

// execFailure renders a non-zero result, preferring stderr.
func execFailure(res *executor.ExecResult) string {
	if res == nil {
		return "no result"
	}
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return fmt.Sprintf("exit %d: %s", res.ExitCode, truncate(s))
	}
	if s := strings.TrimSpace(res.Stdout); s != "" {
		return fmt.Sprintf("exit %d: %s", res.ExitCode, truncate(s))
	}
	return fmt.Sprintf("exit %d", res.ExitCode)
}

func truncate(s string) string {
	if len(s) <= maxErrorText {
		return s
	}
	return s[:maxErrorText] + "..."
}
