package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// mockExecutor implements executor.Executor for unit testing tool handlers
// without touching the real filesystem or shell.
type mockExecutor struct {
	readFileFunc      func(ctx context.Context, path string) (string, error)
	writeFileFunc     func(ctx context.Context, path string, content string) error
	listDirectoryFunc func(ctx context.Context, path string) ([]string, error)
	execFunc          func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
	resolvePathFunc   func(relativePath string) (string, error)
}

func (m *mockExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	if m.readFileFunc != nil {
		return m.readFileFunc(ctx, path)
	}
	return "", fmt.Errorf("ReadFile not mocked")
}

func (m *mockExecutor) WriteFile(ctx context.Context, path string, content string) error {
	if m.writeFileFunc != nil {
		return m.writeFileFunc(ctx, path, content)
	}
	return fmt.Errorf("WriteFile not mocked")
}

func (m *mockExecutor) ListDirectory(ctx context.Context, path string) ([]string, error) {
	if m.listDirectoryFunc != nil {
		return m.listDirectoryFunc(ctx, path)
	}
	return nil, fmt.Errorf("ListDirectory not mocked")
}

func (m *mockExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	if m.execFunc != nil {
		return m.execFunc(ctx, command, timeout)
	}
	return nil, fmt.Errorf("Exec not mocked")
}

func (m *mockExecutor) ResolvePath(relativePath string) (string, error) {
	if m.resolvePathFunc != nil {
		return m.resolvePathFunc(relativePath)
	}
	return "/workspace/" + relativePath, nil
}

func (m *mockExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   true,
		CanExec:    true,
		CanNetwork: true,
		MaxTimeout: 5 * time.Minute,
	}
}

// --- shellQuote tests ---

func TestShellQuote_EmbeddedSingleQuote(t *testing.T) {
	// shellQuote is unexported, so we test it indirectly via the search tool.
	// When a pattern contains a single quote, the grep command must still be
	// well-formed. We verify shellQuote produces the correct escaping by
	// checking the command string passed to the executor.
	var capturedCmd string
	mock := &mockExecutor{
		execFunc: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			capturedCmd = command
			return &executor.ExecResult{ExitCode: 1, Stdout: "", Stderr: ""}, nil
		},
	}

	t.Run("direct", func(t *testing.T) {
		got := shellQuote("a'b")
		// Expected: 'a'\''b' — close quote, escaped literal quote, reopen quote.
		want := "'a'\\''b'"
		if got != want {
			t.Errorf("shellQuote(\"a'b\") = %q, want %q", got, want)
		}
	})

	t.Run("indirect_via_search", func(t *testing.T) {
		searchTool := SearchFilesTool(mock)
		input, _ := json.Marshal(map[string]string{
			"pattern": "it's",
			"type":    "grep",
		})
		_, _ = searchTool.Handler(context.Background(), input)

		// The captured command should contain the correctly escaped pattern.
		if !strings.Contains(capturedCmd, "'it'\\''s'") {
			t.Errorf("expected escaped single quote in command, got: %s", capturedCmd)
		}
	})
}

// --- SearchFilesTool tests ---

func TestSearchFilesTool_InvalidType(t *testing.T) {
	mock := &mockExecutor{}
	searchTool := SearchFilesTool(mock)

	input, _ := json.Marshal(map[string]string{
		"pattern": "foo",
		"type":    "exec",
	})

	_, err := searchTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for invalid search type 'exec'")
	}
	if !strings.Contains(err.Error(), "type must be") {
		t.Errorf("error should mention invalid type, got: %v", err)
	}
}

func TestSearchFilesTool_GrepMode(t *testing.T) {
	mock := &mockExecutor{
		resolvePathFunc: func(relativePath string) (string, error) {
			if relativePath != "." {
				t.Errorf("expected default path '.', got %q", relativePath)
			}
			return "/workspace", nil
		},
		execFunc: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			if !strings.HasPrefix(command, "grep") {
				t.Errorf("expected grep command, got: %s", command)
			}
			if !strings.Contains(command, "'/workspace'") {
				t.Errorf("expected resolved workspace path in command, got: %s", command)
			}
			return &executor.ExecResult{
				ExitCode: 0,
				Stdout:   "main.go:10:func main() {\n",
			}, nil
		},
	}

	searchTool := SearchFilesTool(mock)
	input, _ := json.Marshal(map[string]string{
		"pattern": "func main",
		"type":    "grep",
	})

	result, err := searchTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "main.go:10") {
		t.Errorf("expected grep output in result, got: %s", result)
	}
}

func TestSearchFilesTool_PathTraversalRejected(t *testing.T) {
	mock := &mockExecutor{
		resolvePathFunc: func(relativePath string) (string, error) {
			if relativePath != "../../etc" {
				t.Errorf("unexpected path: %q", relativePath)
			}
			return "", fmt.Errorf("path escapes workspace")
		},
	}

	searchTool := SearchFilesTool(mock)
	input, _ := json.Marshal(map[string]string{
		"pattern": "passwd",
		"path":    "../../etc",
		"type":    "grep",
	})

	_, err := searchTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected traversal error")
	}
	if !strings.Contains(err.Error(), "resolve search path") {
		t.Fatalf("expected resolved-path error, got %v", err)
	}
}

// --- WebFetchTool tests ---

func TestWebFetchTool_SchemeRejection(t *testing.T) {
	fetchTool := WebFetchTool()

	cases := []struct {
		name string
		url  string
	}{
		{"file_scheme", "file:///etc/passwd"},
		{"ftp_scheme", "ftp://example.com/file"},
		{"javascript_scheme", "javascript:alert(1)"},
		{"data_scheme", "data:text/html,<h1>Hi</h1>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"url": tc.url})
			_, err := fetchTool.Handler(context.Background(), input)
			if err == nil {
				t.Fatalf("expected error for scheme %q, got nil", tc.url)
			}
			if !strings.Contains(err.Error(), "http://") && !strings.Contains(err.Error(), "https://") {
				t.Errorf("error should mention allowed schemes, got: %v", err)
			}
		})
	}
}

func TestWebFetchTool_PrivateHostRejection(t *testing.T) {
	fetchTool := WebFetchTool()
	input, _ := json.Marshal(map[string]string{"url": "http://127.0.0.1/secret"})

	_, err := fetchTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for private host")
	}
	if !strings.Contains(err.Error(), "private host") {
		t.Fatalf("expected private host rejection, got %v", err)
	}
}

func TestWebFetchTool_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	fetchTool := newWebFetchTool(webFetchOptions{
		client:            srv.Client(),
		allowPrivateHosts: true,
	})
	input, _ := json.Marshal(map[string]string{"url": srv.URL})

	_, err := fetchTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status code 403, got: %v", err)
	}
}

func TestWebFetchTool_Truncation(t *testing.T) {
	// Return a body larger than maxFetchSize (100 KB).
	bigBody := strings.Repeat("A", 120*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, bigBody)
	}))
	defer srv.Close()

	fetchTool := newWebFetchTool(webFetchOptions{
		client:            srv.Client(),
		allowPrivateHosts: true,
	})
	input, _ := json.Marshal(map[string]string{"url": srv.URL})

	result, err := fetchTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, truncatedNotice) {
		t.Error("expected truncation notice in result")
	}
	// The result should be at most maxFetchSize + len(truncatedNotice).
	maxExpected := maxFetchSize + len(truncatedNotice)
	if len(result) > maxExpected {
		t.Errorf("result length %d exceeds expected max %d", len(result), maxExpected)
	}
}

// --- ReadFileTool tests ---

func TestReadFileTool_EmptyPath(t *testing.T) {
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]string{"path": ""})
	_, err := readTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("error should mention path is required, got: %v", err)
	}
}

// --- WriteFileTool tests ---

func TestWriteFileTool_Success(t *testing.T) {
	var writtenPath, writtenContent string
	mock := &mockExecutor{
		writeFileFunc: func(ctx context.Context, path string, content string) error {
			writtenPath = path
			writtenContent = content
			return nil
		},
	}

	writeTool := WriteFileTool(mock)
	input, _ := json.Marshal(map[string]any{
		"path":    "src/main.go",
		"content": "package main",
	})

	result, err := writeTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if writtenPath != "src/main.go" {
		t.Errorf("written path = %q, want %q", writtenPath, "src/main.go")
	}
	if writtenContent != "package main" {
		t.Errorf("written content = %q, want %q", writtenContent, "package main")
	}
	if !strings.Contains(result, "12 bytes") {
		t.Errorf("expected byte count in result, got: %s", result)
	}
	if !strings.Contains(result, "src/main.go") {
		t.Errorf("expected path in result, got: %s", result)
	}
}

// --- RunCommandTool tests ---

func TestRunCommandTool_NonZeroExitCode(t *testing.T) {
	mock := &mockExecutor{
		execFunc: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				ExitCode: 1,
				Stdout:   "some output",
				Stderr:   "error occurred",
			}, nil
		},
	}

	runTool := RunCommandTool(mock)
	input, _ := json.Marshal(map[string]string{"command": "false"})

	result, err := runTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "[exit code: 1]") {
		t.Errorf("expected exit code in result, got: %s", result)
	}
	if !strings.Contains(result, "some output") {
		t.Errorf("expected stdout in result, got: %s", result)
	}
	if !strings.Contains(result, "STDERR:") {
		t.Errorf("expected stderr label in result, got: %s", result)
	}
}

func TestRunCommandTool_EmptyCommand(t *testing.T) {
	mock := &mockExecutor{}
	runTool := RunCommandTool(mock)

	input, _ := json.Marshal(map[string]string{"command": ""})
	_, err := runTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for empty command")
	}
	if !strings.Contains(err.Error(), "command is required") {
		t.Errorf("error should mention command is required, got: %v", err)
	}
}

// --- RegisterBuiltins tests ---

func TestRegisterBuiltins(t *testing.T) {
	mock := &mockExecutor{}
	registry := tool.NewRegistry()
	RegisterBuiltins(registry, mock)

	expectedTools := []string{
		"read_file",
		"write_file",
		"list_directory",
		"search_files",
		"run_command",
		"web_fetch",
	}

	defs := registry.List()
	if len(defs) != len(expectedTools) {
		t.Fatalf("expected %d tools registered, got %d", len(expectedTools), len(defs))
	}

	registered := make(map[string]bool)
	for _, def := range defs {
		registered[def.Name] = true
	}

	for _, name := range expectedTools {
		if !registered[name] {
			t.Errorf("expected tool %q to be registered", name)
		}
	}
}
