package executor

import (
	"context"
	"strings"
	"testing"
)

var _ Executor = (*NoneExecutor)(nil)

func TestNoneExecutor_ReadFile(t *testing.T) {
	n := NewNoneExecutor()
	_, err := n.ReadFile(context.Background(), "main.go")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention not supported, got %q", err.Error())
	}
}

func TestNoneExecutor_WriteFile(t *testing.T) {
	n := NewNoneExecutor()
	err := n.WriteFile(context.Background(), "main.go", "content")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention not supported, got %q", err.Error())
	}
}

func TestNoneExecutor_ListDirectory(t *testing.T) {
	n := NewNoneExecutor()
	_, err := n.ListDirectory(context.Background(), ".")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention not supported, got %q", err.Error())
	}
}

func TestNoneExecutor_Exec(t *testing.T) {
	n := NewNoneExecutor()
	_, err := n.Exec(context.Background(), "echo hi", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention not supported, got %q", err.Error())
	}
}

func TestNoneExecutor_ResolvePath(t *testing.T) {
	n := NewNoneExecutor()
	got, err := n.ResolvePath("foo/bar.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "foo/bar.go" {
		t.Errorf("expected path unchanged, got %q", got)
	}
}

func TestNoneExecutor_Capabilities(t *testing.T) {
	n := NewNoneExecutor()
	caps := n.Capabilities()
	if caps.CanRead || caps.CanWrite || caps.CanExec || caps.CanNetwork {
		t.Errorf("expected all capabilities false, got %+v", caps)
	}
}
