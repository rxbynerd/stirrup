package git

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestWorkspaceRoot_ParsesPwdOutput(t *testing.T) {
	tests := []struct {
		name    string
		result  *executor.ExecResult
		err     error
		want    string
		wantErr string
	}{
		{name: "absolute path", result: &executor.ExecResult{Stdout: "/workspace\n"}, want: "/workspace"},
		{name: "path with a trailing space is kept intact", result: &executor.ExecResult{Stdout: "/work space \n"}, want: "/work space "},
		{name: "relative path", result: &executor.ExecResult{Stdout: "workspace\n"}, wantErr: "not an absolute path"},
		{name: "empty output", result: &executor.ExecResult{Stdout: "\n"}, wantErr: "not an absolute path"},
		{name: "embedded newline", result: &executor.ExecResult{Stdout: "/a\n/b\n"}, wantErr: "not an absolute path"},
		{name: "non-zero exit", result: &executor.ExecResult{ExitCode: 1, Stderr: "pwd: denied"}, wantErr: "exit 1: pwd: denied"},
		{name: "nil result", wantErr: "no result"},
		{name: "exec error", err: errors.New("exec unavailable"), wantErr: "exec unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			var gotCommand string
			exec := newScriptedExecutor(func(command string) (*executor.ExecResult, error) {
				gotCommand = command
				return tt.result, tt.err
			})
			got, err := WorkspaceRoot(context.Background(), exec)
			must.Equal("pwd -P", gotCommand)
			if tt.wantErr != "" {
				must.ErrorContains(err, tt.wantErr)
				return
			}
			must.NoError(err)
			must.Equal(tt.want, got)
		})
	}
}
