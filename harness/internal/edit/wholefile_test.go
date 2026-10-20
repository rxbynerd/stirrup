package edit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func newWorkspaceExecutor(t *testing.T) *executor.LocalExecutor {
	t.Helper()
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)
	return exec
}

func TestWholeFileStrategy_ToolDefinition(t *testing.T) {
	s := NewWholeFileStrategy()
	def := s.ToolDefinition()

	t.Run("names the tool write_file", func(t *testing.T) {
		must := require.New(t)
		must.Equal("write_file", def.Name)
	})
	t.Run("publishes a non-empty input schema", func(t *testing.T) {
		must := require.New(t)
		must.NotEmpty(def.InputSchema)
	})
}

func TestWholeFileStrategy_Apply_WritesFile(t *testing.T) {
	must := require.New(t)
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"path": "hello.txt", "content": "hello world"}`)

	result, err := s.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "error: %s", result.Error)
	must.Equal("hello.txt", result.Path)

	content, err := exec.ReadFile(context.Background(), "hello.txt")
	must.NoError(err)
	must.Equal("hello world", content)
}

func TestWholeFileStrategy_Apply_MissingPath(t *testing.T) {
	must := require.New(t)
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"content": "hello"}`)

	result, err := s.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.NotEmpty(result.Error)
}

func TestWholeFileStrategy_Apply_InvalidJSON(t *testing.T) {
	must := require.New(t)
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{invalid`)

	_, err := s.Apply(context.Background(), input, exec)
	must.Error(err)
}

func TestWholeFileStrategy_Apply_CreatesSubdirectories(t *testing.T) {
	must := require.New(t)
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"path": "deep/nested/dir/file.txt", "content": "nested content"}`)

	result, err := s.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "deep/nested/dir/file.txt")
	must.NoError(err)
	must.Equal("nested content", content)
}
