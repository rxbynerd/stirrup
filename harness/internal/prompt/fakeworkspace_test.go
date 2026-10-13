package prompt

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// fakeWorkspace is a recording WorkspaceReader standing in for a sandbox:
// it answers listings and the fragments' shell commands from its fields and
// never touches the harness host.
type fakeWorkspace struct {
	caps    executor.ExecutorCapabilities
	entries []string
	listErr error
	// root answers `pwd -P`.
	root string
	// toplevel answers `git rev-parse --show-toplevel`; empty answers
	// git's not-a-repository failure.
	toplevel        string
	status          string
	statusTruncated bool
	statusExitCode  int

	listCalls []string
	execCalls []string
}

func newFakeWorkspace(root string) *fakeWorkspace {
	return &fakeWorkspace{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true, MaxTimeout: time.Minute},
		root: root,
	}
}

func (f *fakeWorkspace) ListDirectory(_ context.Context, path string) ([]string, error) {
	f.listCalls = append(f.listCalls, path)
	return f.entries, f.listErr
}

func (f *fakeWorkspace) Capabilities() executor.ExecutorCapabilities { return f.caps }

func (f *fakeWorkspace) Exec(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
	f.execCalls = append(f.execCalls, command)
	switch {
	case command == "pwd -P":
		return &executor.ExecResult{Stdout: f.root + "\n"}, nil
	case !strings.HasPrefix(command, "git "):
		return nil, errors.New("fakeWorkspace: unexpected command " + command)
	case strings.Contains(command, "'rev-parse' '--show-toplevel'"):
		if f.toplevel == "" {
			return &executor.ExecResult{ExitCode: 128, Stderr: "fatal: not a git repository (or any of the parent directories): .git\n"}, nil
		}
		return &executor.ExecResult{Stdout: f.toplevel + "\n"}, nil
	case strings.Contains(command, "'status'"):
		return &executor.ExecResult{ExitCode: f.statusExitCode, Stdout: f.status, OutputTruncated: f.statusTruncated}, nil
	default:
		return nil, errors.New("fakeWorkspace: unexpected git command " + command)
	}
}

// statusCommands returns the recorded git status invocations.
func (f *fakeWorkspace) statusCommands() []string {
	var out []string
	for _, c := range f.execCalls {
		if strings.Contains(c, "'status'") {
			out = append(out, c)
		}
	}
	return out
}
