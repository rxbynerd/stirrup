package edit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func newSearchReplaceExecutor(t *testing.T) *executor.LocalExecutor {
	t.Helper()
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	return exec
}

func writeSearchReplaceFixture(t *testing.T, exec *executor.LocalExecutor, path, content string) {
	t.Helper()
	require.NoError(t, exec.WriteFile(context.Background(), path, content))
}

func TestSearchReplaceStrategy_ToolDefinition(t *testing.T) {
	s := NewSearchReplaceStrategy()
	def := s.ToolDefinition()

	assert.Equal(t, "search_replace", def.Name)
	assert.NotEmpty(t, def.InputSchema)
}

func TestSearchReplaceStrategy_ReplacesUniqueMatch(t *testing.T) {
	ctx := context.Background()
	exec := newSearchReplaceExecutor(t)
	writeSearchReplaceFixture(t, exec, "main.go", "package main\n\nfunc hello() {\n\treturn\n}\n")

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "main.go",
		"old_string": "func hello() {\n\treturn\n}",
		"new_string": "func hello() {\n\tfmt.Println(\"hello\")\n}"
	}`)

	result, err := s.Apply(ctx, input, exec)
	require.NoError(t, err)
	require.True(t, result.Applied, "error: %s", result.Error)
	assert.Equal(t, "main.go", result.Path)

	content, err := exec.ReadFile(ctx, "main.go")
	require.NoError(t, err)
	assert.Contains(t, content, "fmt.Println")
	assert.NotContains(t, content, "return")

	assert.NotEmpty(t, result.Diff)
	assert.Contains(t, result.Diff, "---")
	assert.Contains(t, result.Diff, "-\treturn")
	assert.Contains(t, result.Diff, "+\tfmt.Println")
}

func TestSearchReplaceStrategy_NoMatchReportsNotFound(t *testing.T) {
	ctx := context.Background()
	exec := newSearchReplaceExecutor(t)
	writeSearchReplaceFixture(t, exec, "file.txt", "hello world\n")

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "file.txt",
		"old_string": "nonexistent text",
		"new_string": "replacement"
	}`)

	result, err := s.Apply(ctx, input, exec)
	require.NoError(t, err)
	assert.False(t, result.Applied)
	assert.Contains(t, result.Error, "not found")
}

func TestSearchReplaceStrategy_MultipleMatchesRejected(t *testing.T) {
	ctx := context.Background()
	exec := newSearchReplaceExecutor(t)
	writeSearchReplaceFixture(t, exec, "file.txt", "foo\nbar\nfoo\nbaz\n")

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "file.txt",
		"old_string": "foo",
		"new_string": "qux"
	}`)

	result, err := s.Apply(ctx, input, exec)
	require.NoError(t, err)
	assert.False(t, result.Applied)
	assert.Contains(t, result.Error, "2 locations")

	content, err := exec.ReadFile(ctx, "file.txt")
	require.NoError(t, err)
	assert.NotContains(t, content, "qux", "file should not have been modified")
}

func TestSearchReplaceStrategy_EmptyOldStringCreatesFile(t *testing.T) {
	ctx := context.Background()
	exec := newSearchReplaceExecutor(t)

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "new_file.txt",
		"old_string": "",
		"new_string": "brand new content\n"
	}`)

	result, err := s.Apply(ctx, input, exec)
	require.NoError(t, err)
	require.True(t, result.Applied, "error: %s", result.Error)

	content, err := exec.ReadFile(ctx, "new_file.txt")
	require.NoError(t, err)
	assert.Equal(t, "brand new content\n", content)

	assert.Contains(t, result.Diff, "+brand new content")
}

func TestSearchReplaceStrategy_EmptyOldStringRejectsExistingFile(t *testing.T) {
	ctx := context.Background()
	exec := newSearchReplaceExecutor(t)
	writeSearchReplaceFixture(t, exec, "existing.txt", "already here\n")

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "existing.txt",
		"old_string": "",
		"new_string": "overwrite"
	}`)

	result, err := s.Apply(ctx, input, exec)
	require.NoError(t, err)
	assert.False(t, result.Applied)
	assert.Contains(t, result.Error, "already exists")
}

func TestSearchReplaceStrategy_MissingPath(t *testing.T) {
	exec := newSearchReplaceExecutor(t)

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{"old_string": "x", "new_string": "y"}`)

	result, err := s.Apply(context.Background(), input, exec)
	require.NoError(t, err)
	assert.False(t, result.Applied)
	assert.NotEmpty(t, result.Error, "expected an error message for missing path")
}

func TestSearchReplaceStrategy_InvalidJSONReturnsError(t *testing.T) {
	exec := newSearchReplaceExecutor(t)

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{invalid`)

	_, err := s.Apply(context.Background(), input, exec)
	require.Error(t, err)
}

func TestSearchReplaceStrategy_MissingFileReportsReadError(t *testing.T) {
	exec := newSearchReplaceExecutor(t)

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "missing.txt",
		"old_string": "something",
		"new_string": "else"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	require.NoError(t, err)
	assert.False(t, result.Applied)
	assert.Contains(t, result.Error, "read file")
}

func TestSearchReplaceStrategy_DiffShowsChangedLine(t *testing.T) {
	ctx := context.Background()
	exec := newSearchReplaceExecutor(t)
	writeSearchReplaceFixture(t, exec, "diff.txt", "line 1\nline 2\nline 3\nline 4\nline 5\n")

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "diff.txt",
		"old_string": "line 3",
		"new_string": "line THREE"
	}`)

	result, err := s.Apply(ctx, input, exec)
	require.NoError(t, err)
	require.True(t, result.Applied, "error: %s", result.Error)

	assert.True(t, strings.HasPrefix(result.Diff, "--- a/diff.txt\n+++ b/diff.txt\n"),
		"diff should start with file headers; got:\n%s", result.Diff)
	assert.Contains(t, result.Diff, "-line 3")
	assert.Contains(t, result.Diff, "+line THREE")
}
