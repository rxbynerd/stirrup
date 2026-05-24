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
	// shellQuote is unexported; verify directly. When a pattern contains a
	// single quote, the constructed shell command must still be well-formed.
	got := shellQuote("a'b")
	// Expected: 'a'\''b' — close quote, escaped literal quote, reopen quote.
	want := "'a'\\''b'"
	if got != want {
		t.Errorf("shellQuote(\"a'b\") = %q, want %q", got, want)
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
		_, _ = fmt.Fprint(w, bigBody)
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

func TestReadFileTool_LineNumberedOutput(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "alpha\nbeta\ngamma\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "file.txt"})
	result, err := readTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "1\talpha\n2\tbeta\n3\tgamma"
	if result != want {
		t.Errorf("output mismatch\n got: %q\nwant: %q", result, want)
	}
}

func TestReadFileTool_LineRange(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "one\ntwo\nthree\nfour\nfive\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":       "file.txt",
		"start_line": 2,
		"limit":      2,
	})
	result, err := readTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "2\ttwo\n3\tthree"
	if result != want {
		t.Errorf("output mismatch\n got: %q\nwant: %q", result, want)
	}
}

func TestReadFileTool_StartLinePastEOF(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "only one line\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":       "file.txt",
		"start_line": 500,
	})
	result, err := readTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("expected non-error for start_line past EOF, got %v", err)
	}
	if !strings.Contains(result, "past end of file") {
		t.Errorf("expected past-EOF notice, got: %q", result)
	}
}

func TestReadFileTool_LimitOverMax(t *testing.T) {
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":  "file.txt",
		"limit": 100000,
	})
	_, err := readTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for limit over maximum")
	}
	if !strings.Contains(err.Error(), "limit must be <=") {
		t.Errorf("error should mention limit upper bound, got: %v", err)
	}
}

func TestReadFileTool_PaddingAlignsColumns(t *testing.T) {
	// A 12-line range crosses single→double digit boundaries; line numbers
	// must be right-aligned to a width of 2 so columns line up.
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = "x"
	}
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return strings.Join(lines, "\n") + "\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "file.txt"})
	result, err := readTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Line 1 should be " 1\tx" (padded), line 12 should be "12\tx".
	if !strings.Contains(result, " 1\tx") {
		t.Errorf("expected padded line 1 prefix \" 1\\t\", got: %q", result)
	}
	if !strings.Contains(result, "12\tx") {
		t.Errorf("expected unpadded line 12 prefix \"12\\t\", got: %q", result)
	}
}

// --- ListDirectoryTool tests ---

func TestListDirectoryTool_FlatDefault(t *testing.T) {
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			return []string{"file.go", "sub/"}, nil
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "."})
	result, err := listTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "file.go\nsub/" {
		t.Errorf("unexpected output: %q", result)
	}
}

func TestListDirectoryTool_Recursive(t *testing.T) {
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			switch path {
			case ".":
				return []string{"a.go", "sub/"}, nil
			case "./sub":
				return []string{"b.go", "nested/"}, nil
			case "./sub/nested":
				return []string{"c.go"}, nil
			}
			return nil, fmt.Errorf("unexpected path %q", path)
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 5,
	})
	result, err := listTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"a.go", "sub/", "sub/b.go", "sub/nested/", "sub/nested/c.go"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q in result, got: %q", want, result)
		}
	}
}

func TestListDirectoryTool_RecursiveDepthLimit(t *testing.T) {
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			switch path {
			case ".":
				return []string{"sub/"}, nil
			case "./sub":
				return []string{"deeper/"}, nil
			case "./sub/deeper":
				return []string{"file.go"}, nil
			}
			return nil, nil
		},
	}
	listTool := ListDirectoryTool(mock)

	// max_depth=1 should list "sub/" and "sub/deeper/" but NOT "sub/deeper/file.go".
	input, _ := json.Marshal(map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 1,
	})
	result, err := listTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "sub/") {
		t.Errorf("expected sub/ in result, got: %q", result)
	}
	if !strings.Contains(result, "sub/deeper/") {
		t.Errorf("expected sub/deeper/ at depth=1, got: %q", result)
	}
	if strings.Contains(result, "file.go") {
		t.Errorf("did not expect deeper file at max_depth=1, got: %q", result)
	}
}

func TestListDirectoryTool_MaxEntriesTruncation(t *testing.T) {
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			return []string{"a", "b", "c", "d", "e"}, nil
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":        ".",
		"max_entries": 3,
	})
	result, err := listTool.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "[truncated") {
		t.Errorf("expected truncation sentinel, got: %q", result)
	}
}

func TestListDirectoryTool_EmptyPath(t *testing.T) {
	mock := &mockExecutor{}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{"path": ""})
	_, err := listTool.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Errorf("expected path-required error, got %v", err)
	}
}

func TestListDirectoryTool_MaxEntriesValidation(t *testing.T) {
	mock := &mockExecutor{}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{"path": ".", "max_entries": 0})
	_, err := listTool.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "max_entries must be >=") {
		t.Errorf("expected lower-bound error, got %v", err)
	}

	input, _ = json.Marshal(map[string]any{"path": ".", "max_entries": 100000})
	_, err = listTool.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "max_entries must be <=") {
		t.Errorf("expected upper-bound error, got %v", err)
	}
}

func TestListDirectoryTool_RecursiveRootError(t *testing.T) {
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			return nil, fmt.Errorf("root inaccessible")
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{"path": ".", "recursive": true})
	_, err := listTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error when root is inaccessible")
	}
}

func TestReadFileTool_StartLineBelowOne(t *testing.T) {
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":       "file.txt",
		"start_line": 0,
	})
	_, err := readTool.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "start_line must be >=") {
		t.Errorf("expected start_line lower-bound error, got %v", err)
	}
}

func TestReadFileTool_LimitBelowOne(t *testing.T) {
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":  "file.txt",
		"limit": 0,
	})
	_, err := readTool.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "limit must be >=") {
		t.Errorf("expected limit lower-bound error, got %v", err)
	}
}

func TestReadFileTool_ExecError(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "", fmt.Errorf("file not found")
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "missing.txt"})
	_, err := readTool.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Errorf("expected file-not-found error, got %v", err)
	}
}

func TestListDirectoryTool_MaxDepthOverLimit(t *testing.T) {
	mock := &mockExecutor{}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 11,
	})
	_, err := listTool.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for max_depth over limit")
	}
	if !strings.Contains(err.Error(), "max_depth must be <=") {
		t.Errorf("error should mention max_depth bound, got: %v", err)
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
		"grep_files",
		"find_files",
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

	// The legacy search_files name must remain absent — the registry no
	// longer ships it, and the dispatcher emits a directional hint for
	// callers that still use it. Asserting absence here keeps a future
	// accidental re-introduction from silently passing the positive
	// checks above.
	if registered["search_files"] {
		t.Error("search_files must not be registered (split into grep_files and find_files)")
	}
}

// maxToolDescriptionLen caps each enriched description so a worst-case
// system prompt with every tool registered stays bounded. 2000 chars per
// tool is generous — the longest description today is well under 1000.
const maxToolDescriptionLen = 2000

// extractJSONExample pulls the JSON object that follows the rightmost
// "Example" marker in a tool description and returns the raw bytes.
// Matching from the rightmost marker keeps tools with multiple worked
// examples (e.g. edit_file's "Example (replace): ...") working without
// the test having to know each tool's example label. The boundary is the
// matching '}' for the first '{' after the marker, walking the brace
// balance so embedded objects do not terminate the scan early.
func extractJSONExample(desc string) (string, bool) {
	marker := strings.LastIndex(desc, "Example")
	if marker < 0 {
		return "", false
	}
	rest := desc[marker:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := open; i < len(rest); i++ {
		c := rest[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open : i+1], true
			}
		}
	}
	return "", false
}

// TestBuiltinDescriptions_EnrichedShape asserts that every registered
// built-in tool description satisfies the #227 contract: when-to-use
// guidance, a syntactically valid JSON example, and a length below the
// system-prompt budget cap. Failures here mean a future contributor
// pared a description back below the agreed shape — the test exists to
// surface that regression before the change ships.
func TestBuiltinDescriptions_EnrichedShape(t *testing.T) {
	mock := &mockExecutor{}
	registry := tool.NewRegistry()
	RegisterBuiltins(registry, mock)

	for _, def := range registry.List() {
		t.Run(def.Name, func(t *testing.T) {
			if len(def.Description) > maxToolDescriptionLen {
				t.Errorf("description length %d exceeds cap %d", len(def.Description), maxToolDescriptionLen)
			}
			// "Use this" is the canonical when-to-use opener used across
			// the enriched descriptions. Asserting on it (rather than a
			// hand-curated per-tool phrase) keeps the contract uniform
			// and forces future tools to adopt the same convention.
			if !strings.Contains(def.Description, "Use this") {
				t.Errorf("description missing when-to-use guidance (expected substring %q)", "Use this")
			}
			example, ok := extractJSONExample(def.Description)
			if !ok {
				t.Fatalf("description missing JSON example after \"Example\" marker; got: %s", def.Description)
			}
			var probe map[string]any
			if err := json.Unmarshal([]byte(example), &probe); err != nil {
				t.Errorf("example is not valid JSON: %v\nexample: %s", err, example)
			}
		})
	}
}

// TestSpawnAgentTool_EnrichedShape applies the same description contract
// to spawn_agent, which is wired separately from RegisterBuiltins (the
// factory injects a real spawner closure). A trivial spawner suffices
// because only the static tool definition is under test.
func TestSpawnAgentTool_EnrichedShape(t *testing.T) {
	noopSpawner := func(ctx context.Context, prompt, mode string, maxTurns int) (json.RawMessage, error) {
		return json.RawMessage("{}"), nil
	}
	def := SpawnAgentTool(noopSpawner)
	if len(def.Description) > maxToolDescriptionLen {
		t.Errorf("description length %d exceeds cap %d", len(def.Description), maxToolDescriptionLen)
	}
	if !strings.Contains(def.Description, "Use this") {
		t.Errorf("description missing when-to-use guidance")
	}
	example, ok := extractJSONExample(def.Description)
	if !ok {
		t.Fatalf("description missing JSON example after \"Example\" marker")
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(example), &probe); err != nil {
		t.Errorf("example is not valid JSON: %v\nexample: %s", err, example)
	}
}
