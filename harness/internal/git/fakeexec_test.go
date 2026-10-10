package git

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// scriptedExecutor is a recording executor.Executor whose Exec answers from
// a response function instead of running anything. Only Exec and
// Capabilities are exercised by the strategy; the file-I/O methods error.
type scriptedExecutor struct {
	caps    executor.ExecutorCapabilities
	respond func(command string) (*executor.ExecResult, error)
	calls   []string
}

func newScriptedExecutor(respond func(command string) (*executor.ExecResult, error)) *scriptedExecutor {
	return &scriptedExecutor{
		caps:    executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true, MaxTimeout: time.Minute},
		respond: respond,
	}
}

func (s *scriptedExecutor) ReadFile(context.Context, string) (string, error) {
	return "", errors.New("scriptedExecutor: ReadFile not implemented")
}

func (s *scriptedExecutor) WriteFile(context.Context, string, string) error {
	return errors.New("scriptedExecutor: WriteFile not implemented")
}

func (s *scriptedExecutor) ListDirectory(context.Context, string) ([]string, error) {
	return nil, errors.New("scriptedExecutor: ListDirectory not implemented")
}

func (s *scriptedExecutor) ResolvePath(relativePath string) (string, error) {
	return relativePath, nil
}

func (s *scriptedExecutor) Capabilities() executor.ExecutorCapabilities {
	return s.caps
}

func (s *scriptedExecutor) Exec(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
	s.calls = append(s.calls, command)
	return s.respond(command)
}

// fakeRepo scripts the answers of a sandbox whose working directory is root
// and whose git reports toplevel as the repository root. Every other git
// command succeeds silently.
type fakeRepo struct {
	root     string
	toplevel string
	sha      string
	branch   string
	// staged controls `diff --cached --quiet`: true answers exit 1.
	staged bool
	// notRepo answers show-toplevel with git's not-a-repository failure.
	notRepo bool
	// notRepoStderr replaces the default not-a-repository message.
	notRepoStderr string
	// gitEntry answers the workspace .git existence test with success.
	gitEntry bool
	// gitMissing answers every git command the way sh reports an absent
	// binary.
	gitMissing bool
}

func (f *fakeRepo) respond(command string) (*executor.ExecResult, error) {
	switch {
	case command == "pwd -P":
		return &executor.ExecResult{Stdout: f.root + "\n"}, nil
	case command == "test -e .git || test -L .git":
		if f.gitEntry {
			return &executor.ExecResult{}, nil
		}
		return &executor.ExecResult{ExitCode: 1}, nil
	case !strings.HasPrefix(command, "git "):
		return nil, errors.New("unexpected non-git command: " + command)
	case f.gitMissing:
		return &executor.ExecResult{ExitCode: 127, Stderr: "sh: git: not found\n"}, nil
	case hasArgs(command, "rev-parse", "--show-toplevel"):
		if f.notRepo {
			stderr := f.notRepoStderr
			if stderr == "" {
				stderr = "fatal: not a git repository (or any of the parent directories): .git\n"
			}
			return &executor.ExecResult{ExitCode: 128, Stderr: stderr}, nil
		}
		return &executor.ExecResult{Stdout: f.toplevel + "\n"}, nil
	case hasArgs(command, "init"):
		f.notRepo = false
		f.toplevel = f.root
		return &executor.ExecResult{}, nil
	case hasArgs(command, "diff", "--cached", "--quiet"):
		if f.staged {
			return &executor.ExecResult{ExitCode: 1}, nil
		}
		return &executor.ExecResult{}, nil
	case hasArgs(command, "rev-parse", "--verify", "--quiet", "HEAD"), hasArgs(command, "rev-parse", "HEAD"):
		return &executor.ExecResult{Stdout: f.sha + "\n"}, nil
	case hasArgs(command, "rev-parse", "--abbrev-ref", "HEAD"):
		return &executor.ExecResult{Stdout: f.branch + "\n"}, nil
	case hasArgs(command, "--version"):
		return &executor.ExecResult{Stdout: "git version 2.54.0\n"}, nil
	default:
		return &executor.ExecResult{}, nil
	}
}

// hasArgs reports whether command carries the given arguments, in order,
// as consecutive single-quoted words.
func hasArgs(command string, args ...string) bool {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Contains(command, strings.Join(quoted, " "))
}
