package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestAPIExecutor(serverURL string) *APIExecutor {
	e := NewAPIExecutor("test-token", "octocat", "hello-world", "main")
	e.baseURL = serverURL
	return e
}

func TestAPIExecutor_ReadFile_Success(t *testing.T) {
	content := "package main\n\nfunc main() {}\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/contents/main.go") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("expected ref=main, got %q", r.URL.Query().Get("ref"))
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/vnd.github.v3.raw" {
			t.Errorf("unexpected accept header: %s", r.Header.Get("Accept"))
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(content))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	got, err := e.ReadFile(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != content {
		t.Errorf("expected %q, got %q", content, got)
	}
}

func TestAPIExecutor_ReadFile_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message": "Not Found"}`))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	_, err := e.ReadFile(context.Background(), "nonexistent.go")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error should mention HTTP 404, got %q", err.Error())
	}
}

func TestAPIExecutor_ListDirectory_Success(t *testing.T) {
	entries := []githubContentEntry{
		{Name: "README.md"},
		{Name: "main.go"},
		{Name: "go.mod"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github.v3+json" {
			t.Errorf("unexpected accept header: %s", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	names, err := e.ListDirectory(context.Background(), ".")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(names))
	}
	expected := []string{"README.md", "main.go", "go.mod"}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("entry %d: expected %q, got %q", i, expected[i], name)
		}
	}
}

func TestAPIExecutor_ListDirectory_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	names, err := e.ListDirectory(context.Background(), "empty-dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty list, got %d entries", len(names))
	}
}

func TestAPIExecutor_WriteFile_Unsupported(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	err := e.WriteFile(context.Background(), "file.txt", "content")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "write operations not supported") {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

func TestAPIExecutor_Exec_Unsupported(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	_, err := e.Exec(context.Background(), "ls", time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "command execution not supported") {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

func TestAPIExecutor_ResolvePath(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	got, err := e.ResolvePath("some/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "some/path" {
		t.Errorf("expected %q, got %q", "some/path", got)
	}
}

func TestAPIExecutor_Capabilities(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	caps := e.Capabilities()

	if !caps.CanRead {
		t.Error("expected CanRead to be true")
	}
	if caps.CanWrite {
		t.Error("expected CanWrite to be false")
	}
	if caps.CanExec {
		t.Error("expected CanExec to be false")
	}
	if !caps.CanNetwork {
		t.Error("expected CanNetwork to be true")
	}
}

func TestAPIExecutor_ReadFile_NoRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "" {
			t.Errorf("expected no ref query param, got %q", r.URL.Query().Get("ref"))
		}
		w.Write([]byte("content"))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	e.ref = "" // clear ref
	_, err := e.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Verify that APIExecutor satisfies the Executor interface.
var _ Executor = (*APIExecutor)(nil)
