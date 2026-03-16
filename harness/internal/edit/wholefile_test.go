package edit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestWholeFileStrategy_ToolDefinition(t *testing.T) {
	s := NewWholeFileStrategy()
	def := s.ToolDefinition()

	if def.Name != "write_file" {
		t.Errorf("name: got %q, want %q", def.Name, "write_file")
	}
	if len(def.InputSchema) == 0 {
		t.Error("input schema should not be empty")
	}
}

func TestWholeFileStrategy_Apply(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"path": "hello.txt", "content": "hello world"}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true, got false; error: %s", result.Error)
	}
	if result.Path != "hello.txt" {
		t.Errorf("path: got %q, want %q", result.Path, "hello.txt")
	}

	// Verify the file was actually written.
	content, err := exec.ReadFile(context.Background(), "hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "hello world" {
		t.Errorf("content: got %q, want %q", content, "hello world")
	}
}

func TestWholeFileStrategy_Apply_MissingPath(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"content": "hello"}`)

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

func TestWholeFileStrategy_Apply_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewWholeFileStrategy()
	input := json.RawMessage(`{invalid`)

	_, err = s.Apply(context.Background(), input, exec)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestWholeFileStrategy_Apply_CreatesSubdirectories(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewWholeFileStrategy()
	input := json.RawMessage(`{"path": "deep/nested/dir/file.txt", "content": "nested content"}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "deep/nested/dir/file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "nested content" {
		t.Errorf("content: got %q, want %q", content, "nested content")
	}
}
