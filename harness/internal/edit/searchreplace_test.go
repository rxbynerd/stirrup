package edit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestSearchReplaceStrategy_ToolDefinition(t *testing.T) {
	s := NewSearchReplaceStrategy()
	def := s.ToolDefinition()

	if def.Name != "search_replace" {
		t.Errorf("name: got %q, want %q", def.Name, "search_replace")
	}
	if len(def.InputSchema) == 0 {
		t.Error("input schema should not be empty")
	}
}

func TestSearchReplaceStrategy_BasicReplacement(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	// Create an initial file.
	if err := exec.WriteFile(context.Background(), "main.go", "package main\n\nfunc hello() {\n\treturn\n}\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "main.go",
		"old_string": "func hello() {\n\treturn\n}",
		"new_string": "func hello() {\n\tfmt.Println(\"hello\")\n}"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}
	if result.Path != "main.go" {
		t.Errorf("path: got %q, want %q", result.Path, "main.go")
	}

	// Verify the file was updated.
	content, err := exec.ReadFile(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(content, "fmt.Println") {
		t.Errorf("expected replacement text in file; got: %s", content)
	}
	if strings.Contains(content, "return") {
		t.Errorf("expected old text to be removed; got: %s", content)
	}

	// Verify diff output.
	if result.Diff == "" {
		t.Error("expected non-empty diff")
	}
	if !strings.Contains(result.Diff, "---") {
		t.Error("diff should contain unified diff header")
	}
	if !strings.Contains(result.Diff, "-\treturn") {
		t.Errorf("diff should show removed line; got:\n%s", result.Diff)
	}
	if !strings.Contains(result.Diff, "+\tfmt.Println") {
		t.Errorf("diff should show added line; got:\n%s", result.Diff)
	}
}

func TestSearchReplaceStrategy_NoMatch(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	if err := exec.WriteFile(context.Background(), "file.txt", "hello world\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "file.txt",
		"old_string": "nonexistent text",
		"new_string": "replacement"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for no match")
	}
	if !strings.Contains(result.Error, "not found") {
		t.Errorf("expected 'not found' error; got: %s", result.Error)
	}
}

func TestSearchReplaceStrategy_MultipleMatches(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	if err := exec.WriteFile(context.Background(), "file.txt", "foo\nbar\nfoo\nbaz\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "file.txt",
		"old_string": "foo",
		"new_string": "qux"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for multiple matches")
	}
	if !strings.Contains(result.Error, "2 locations") {
		t.Errorf("expected error mentioning 2 locations; got: %s", result.Error)
	}

	// Verify the file was not modified.
	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(content, "qux") {
		t.Error("file should not have been modified")
	}
}

func TestSearchReplaceStrategy_FileCreation(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "new_file.txt",
		"old_string": "",
		"new_string": "brand new content\n"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	// Verify the file was created.
	content, err := exec.ReadFile(context.Background(), "new_file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "brand new content\n" {
		t.Errorf("content: got %q, want %q", content, "brand new content\n")
	}

	// Verify diff shows all lines as additions.
	if !strings.Contains(result.Diff, "+brand new content") {
		t.Errorf("diff should show added line; got:\n%s", result.Diff)
	}
}

func TestSearchReplaceStrategy_FileCreation_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	if err := exec.WriteFile(context.Background(), "existing.txt", "already here\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "existing.txt",
		"old_string": "",
		"new_string": "overwrite"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when old_string is empty and file exists")
	}
	if !strings.Contains(result.Error, "already exists") {
		t.Errorf("expected 'already exists' error; got: %s", result.Error)
	}
}

func TestSearchReplaceStrategy_MissingPath(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{"old_string": "x", "new_string": "y"}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for missing path")
	}
	if result.Error == "" {
		t.Error("expected an error message")
	}
}

func TestSearchReplaceStrategy_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{invalid`)

	_, err = s.Apply(context.Background(), input, exec)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSearchReplaceStrategy_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "missing.txt",
		"old_string": "something",
		"new_string": "else"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for nonexistent file")
	}
	if !strings.Contains(result.Error, "read file") {
		t.Errorf("expected read file error; got: %s", result.Error)
	}
}

func TestSearchReplaceStrategy_DiffOutput(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	if err := exec.WriteFile(context.Background(), "diff.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewSearchReplaceStrategy()
	input := json.RawMessage(`{
		"path": "diff.txt",
		"old_string": "line 3",
		"new_string": "line THREE"
	}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	// Diff should have unified diff headers.
	if !strings.HasPrefix(result.Diff, "--- a/diff.txt\n+++ b/diff.txt\n") {
		t.Errorf("diff should start with file headers; got:\n%s", result.Diff)
	}
	// Diff should show the specific change.
	if !strings.Contains(result.Diff, "-line 3") {
		t.Errorf("diff should show removed line; got:\n%s", result.Diff)
	}
	if !strings.Contains(result.Diff, "+line THREE") {
		t.Errorf("diff should show added line; got:\n%s", result.Diff)
	}
}
