package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// fsExecutor is a minimal Executor that resolves paths to a real temp dir
// so the Go-native grep/find walkers (which read os files directly) can be
// exercised without involving a sandbox. Exec is not used by the native
// path; tests that want to exercise the rg path stub it explicitly.
type fsExecutor struct {
	root   string
	canExec bool
	execFn func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
}

func (f *fsExecutor) ReadFile(context.Context, string) (string, error)        { return "", nil }
func (f *fsExecutor) WriteFile(context.Context, string, string) error         { return nil }
func (f *fsExecutor) ListDirectory(context.Context, string) ([]string, error) { return nil, nil }
func (f *fsExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	if f.execFn != nil {
		return f.execFn(ctx, command, timeout)
	}
	return &executor.ExecResult{ExitCode: 0}, nil
}
func (f *fsExecutor) ResolvePath(relativePath string) (string, error) {
	if relativePath == "." || relativePath == "" {
		return f.root, nil
	}
	return filepath.Join(f.root, relativePath), nil
}
func (f *fsExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: f.canExec, MaxTimeout: time.Minute}
}

// withRipgrepProbe pins the cached ripgrep detection to a known value so a
// single test does not depend on the host having (or not having) rg
// installed. Restores the global detector on cleanup.
func withRipgrepProbe(t *testing.T, present bool) {
	t.Helper()
	prev := defaultRipgrepDetector
	t.Cleanup(func() { defaultRipgrepDetector = prev })
	defaultRipgrepDetector = &ripgrepDetector{probe: func() bool { return present }}
}

// --- GrepFilesTool tests ---

func TestGrepFilesTool_NativeMatchesContent(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "Hello"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "a.go:2:func Hello() {}") {
		t.Errorf("expected path:line:match in output, got %q", out)
	}
}

func TestGrepFilesTool_InvalidRegex(t *testing.T) {
	withRipgrepProbe(t, false)
	grep := GrepFilesTool(&fsExecutor{root: t.TempDir()})
	// `[` is an unbalanced character class — a common mistake for models
	// that confuse shell globs with regexes.
	input, _ := json.Marshal(map[string]any{"pattern": "["})
	_, err := grep.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("error should mention invalid regex, got: %v", err)
	}
}

func TestGrepFilesTool_MaxResultsBound(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	// 5 matching files, ask for at most 2.
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, "file"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(path, []byte("needle\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "needle", "max_results": 2})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 results, got %d: %q", len(lines), out)
	}
}

func TestGrepFilesTool_IncludeExclude(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "skip.go"), []byte("needle\n"), 0o644)

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{
		"pattern": "needle",
		"include": []string{"*.go"},
		"exclude": []string{"skip.go"},
	})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "a.go:") {
		t.Errorf("expected a.go in result, got %q", out)
	}
	if strings.Contains(out, "a.txt") {
		t.Errorf("a.txt should be excluded by include filter, got %q", out)
	}
	if strings.Contains(out, "skip.go") {
		t.Errorf("skip.go should be excluded by exclude filter, got %q", out)
	}
}

func TestGrepFilesTool_NoMatches(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("hello\n"), 0o644)

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "absent_pattern"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No matches found") {
		t.Errorf("expected no-match notice, got %q", out)
	}
}

func TestGrepFilesTool_BinaryFilesSkipped(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	// Binary file contains the literal bytes of "match" but also a NUL —
	// the heuristic should skip it so we don't dump garbage at the model.
	_ = os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("match\x00more"), 0o644)
	// Plain text file with the same word; this one should match.
	_ = os.WriteFile(filepath.Join(dir, "text.txt"), []byte("match\n"), 0o644)

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "match"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "blob.bin") {
		t.Errorf("binary file should be skipped, got %q", out)
	}
	if !strings.Contains(out, "text.txt") {
		t.Errorf("text file should match, got %q", out)
	}
}

func TestGrepFilesTool_RipgrepPathSelected(t *testing.T) {
	withRipgrepProbe(t, true)
	var capturedCmd string
	fe := &fsExecutor{
		root:    t.TempDir(),
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			capturedCmd = command
			return &executor.ExecResult{ExitCode: 0, Stdout: "x.go:1:hit\n"}, nil
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "hit"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(capturedCmd, "rg ") {
		t.Errorf("expected rg command, got %q", capturedCmd)
	}
	if !strings.Contains(out, "x.go:1:hit") {
		t.Errorf("expected rg stdout in result, got %q", out)
	}
}

func TestGrepFilesTool_RipgrepFallsBackOnHardError(t *testing.T) {
	withRipgrepProbe(t, true)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("foo\n"), 0o644)
	fe := &fsExecutor{
		root:    dir,
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 2, Stderr: "rg: borken"}, nil
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "foo"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Native fallback should have found the match in a.go.
	if !strings.Contains(out, "a.go") {
		t.Errorf("expected native fallback to surface result, got %q", out)
	}
}

func TestGrepFilesTool_PathTraversalRejected(t *testing.T) {
	mock := &mockExecutor{
		resolvePathFunc: func(relativePath string) (string, error) {
			return "", errors.New("path escapes workspace")
		},
	}
	grep := GrepFilesTool(mock)
	input, _ := json.Marshal(map[string]any{"pattern": "secret", "path": "../../etc"})
	_, err := grep.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected resolve-path error")
	}
	if !strings.Contains(err.Error(), "resolve search path") {
		t.Errorf("expected resolve search path error, got %v", err)
	}
}

// --- FindFilesTool tests ---

func TestFindFilesTool_GlobMatch(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)

	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	out, err := find.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "a.go") {
		t.Errorf("expected a.go in result, got %q", out)
	}
	if strings.Contains(out, "b.txt") {
		t.Errorf("b.txt should not match *.go, got %q", out)
	}
}

func TestFindFilesTool_RecursesIntoSubdirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "sub", "deep", "handler_user.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "noise.txt"), []byte("x"), 0o644)

	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "handler_*.go"})
	out, err := find.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, filepath.Join("sub", "deep", "handler_user.go")) {
		t.Errorf("expected nested file in result, got %q", out)
	}
}

func TestFindFilesTool_MaxResultsBound(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		_ = os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))+".go"), []byte("x"), 0o644)
	}
	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": 2})
	out, err := find.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 results, got %d: %q", len(lines), out)
	}
}

func TestFindFilesTool_NoMatches(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	input, _ := json.Marshal(map[string]any{"name": "*.nope"})
	out, err := find.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No matches found") {
		t.Errorf("expected no-match notice, got %q", out)
	}
}

func TestFindFilesTool_InvalidGlob(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	// An unbalanced bracket is rejected by filepath.Match.
	input, _ := json.Marshal(map[string]any{"name": "[broken"})
	_, err := find.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for invalid glob")
	}
	if !strings.Contains(err.Error(), "invalid glob") {
		t.Errorf("error should mention invalid glob, got: %v", err)
	}
}

func TestFindFilesTool_DoubleStarGlob(t *testing.T) {
	// `**` is not supported by filepath.Match (which is shell-style) — the
	// model is more likely to pass a basename like "handler_*.go" and rely
	// on the walker recursing. But include filters DO honour `**`. Verify
	// that path.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "deep"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "pkg", "deep", "x.go"), []byte("y"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "top.go"), []byte("y"), 0o644)

	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{
		"name":    "*.go",
		"include": []string{"pkg/**"},
	})
	out, err := find.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "top.go") {
		t.Errorf("top.go should not pass pkg/** include, got %q", out)
	}
	if !strings.Contains(out, filepath.Join("pkg", "deep", "x.go")) {
		t.Errorf("nested .go should pass pkg/** include, got %q", out)
	}
}
