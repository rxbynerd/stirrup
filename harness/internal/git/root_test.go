package git

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestWorkspaceRoot(t *testing.T) {
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
			exec := newScriptedExecutor(func(command string) (*executor.ExecResult, error) {
				if command != "pwd -P" {
					t.Fatalf("unexpected command %q", command)
				}
				return tt.result, tt.err
			})
			got, err := WorkspaceRoot(context.Background(), exec)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("WorkspaceRoot() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("WorkspaceRoot() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("WorkspaceRoot() = %q, want %q", got, tt.want)
			}
		})
	}
}
