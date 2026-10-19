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
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	return exec
}

func TestWholeFileStrategy_ToolDefinition(t *testing.T) {
	s := NewWholeFileStrategy()
	def := s.ToolDefinition()

	t.Run("names the tool write_file", func(t *testing.T) {
		require.Equal(t, "write_file", def.Name)
	})
	t.Run("publishes a non-empty input schema", func(t *testing.T) {
		require.NotEmpty(t, def.InputSchema)
	})
}

func TestWholeFileStrategy_Apply_WritesFile(t *testing.T) {
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"path": "hello.txt", "content": "hello world"}`)

	result, err := s.Apply(context.Background(), input, exec)
	require.NoError(t, err)
	require.True(t, result.Applied, "error: %s", result.Error)
	require.Equal(t, "hello.txt", result.Path)

	content, err := exec.ReadFile(context.Background(), "hello.txt")
	require.NoError(t, err)
	require.Equal(t, "hello world", content)
}

func TestWholeFileStrategy_Apply_MissingPath(t *testing.T) {
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"content": "hello"}`)

	result, err := s.Apply(context.Background(), input, exec)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.NotEmpty(t, result.Error)
}

func TestWholeFileStrategy_Apply_InvalidJSON(t *testing.T) {
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{invalid`)

	_, err := s.Apply(context.Background(), input, exec)
	require.Error(t, err)
}

func TestWholeFileStrategy_Apply_CreatesSubdirectories(t *testing.T) {
	exec := newWorkspaceExecutor(t)
	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"path": "deep/nested/dir/file.txt", "content": "nested content"}`)

	result, err := s.Apply(context.Background(), input, exec)
	require.NoError(t, err)
	require.True(t, result.Applied, "error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "deep/nested/dir/file.txt")
	require.NoError(t, err)
	require.Equal(t, "nested content", content)
}
