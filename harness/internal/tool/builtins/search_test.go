package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// fsExecutor is a minimal Executor that resolves paths to a real temp dir
// so the Go-native grep/find walkers (which read os files directly) can be
// exercised without involving a sandbox. Exec is not used by the native
// path; tests that want to exercise the rg path stub it explicitly.
type fsExecutor struct {
	root    string
	canExec bool
	execFn  func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
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

// TestRipgrepDetector_OnceCache covers the once-cache semantics of the
// detector itself rather than going via the package-level seam. The
// existing withRipgrepProbe helper swaps the whole detector, which means
// the production once-cache path (sync.Once + atomic.Bool) is never
// exercised by the rest of the suite. A regression that breaks the
// caching contract — e.g. dropping sync.Once for an unsynchronised
// re-probe per call — would not be caught.
func TestRipgrepDetector_OnceCache(t *testing.T) {
	var probeCount atomic.Int64
	d := &ripgrepDetector{
		probe: func() bool {
			probeCount.Add(1)
			return true
		},
	}

	first := d.detect()
	second := d.detect()
	third := d.detect()

	if got := probeCount.Load(); got != 1 {
		t.Errorf("probe must run exactly once, got %d invocations", got)
	}
	if !first || !second || !third {
		t.Errorf("all three detect() calls must return the cached value (true); got %v %v %v", first, second, third)
	}
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

// TestGrepNative_SkipsSymlinks pins the CWE-59 fix: a symlink inside the
// workspace pointing to an external file must NOT have its content surfaced
// to the model by grep_files. exec.ResolvePath only validates the search
// root, so the WalkDir callback must explicitly skip symlink entries before
// calling os.ReadFile (which would otherwise follow the symlink).
func TestGrepNative_SkipsSymlinks(t *testing.T) {
	withRipgrepProbe(t, false)
	workspace := t.TempDir()
	outside := t.TempDir()

	// Regular file inside the workspace: this one MUST be matched.
	if err := os.WriteFile(filepath.Join(workspace, "regular.txt"), []byte("match-target\n"), 0o644); err != nil {
		t.Fatalf("WriteFile regular: %v", err)
	}
	// External file with the same match content — it lives outside the
	// workspace and MUST NOT appear in results.
	externalPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(externalPath, []byte("match-target\n"), 0o644); err != nil {
		t.Fatalf("WriteFile external: %v", err)
	}
	// Symlink inside the workspace pointing at the external file. Without
	// the symlink-skip guard, grepNative would follow this link via
	// os.ReadFile and surface its content.
	symlinkPath := filepath.Join(workspace, "escape.txt")
	if err := os.Symlink(externalPath, symlinkPath); err != nil {
		t.Skipf("symlink not supported on this platform: %v", err)
	}

	grep := GrepFilesTool(&fsExecutor{root: workspace})
	input, _ := json.Marshal(map[string]any{"pattern": "match-target"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "regular.txt") {
		t.Errorf("regular file should match, got %q", out)
	}
	if strings.Contains(out, "escape.txt") {
		t.Errorf("symlinked path must not appear in results (CWE-59), got %q", out)
	}
	if strings.Contains(out, "secret.txt") {
		t.Errorf("external symlink target content must not be surfaced, got %q", out)
	}
}

// TestGrepViaRipgrep_TransportErrorFallsBackToNative pins the resilience
// contract: an executor transport failure (Docker socket flake, container
// restart, microVM hiccup) must NOT escalate to a hard tool-call error.
// rg exit code >= 2 already falls back to native; transport failure
// preventing rg from launching is less severe and should behave the same.
func TestGrepViaRipgrep_TransportErrorFallsBackToNative(t *testing.T) {
	withRipgrepProbe(t, true)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fe := &fsExecutor{
		root:    dir,
		canExec: true,
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return nil, errors.New("docker socket: connection refused")
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("transport error must not surface; expected native fallback, got: %v", err)
	}
	if !strings.Contains(out, "a.go") {
		t.Errorf("expected native walker to surface a.go, got %q", out)
	}
}

// TestGrepViaRipgrep_ContextCancelPropagates pins the other half: when the
// executor returns context.Canceled (the caller asked to stop), we must
// NOT silently re-walk natively. The cancellation must propagate.
func TestGrepViaRipgrep_ContextCancelPropagates(t *testing.T) {
	withRipgrepProbe(t, true)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fe := &fsExecutor{
		root:    dir,
		canExec: true,
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return nil, context.Canceled
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	_, err := grep.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected context.Canceled to propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
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

func TestFindFilesTool_PathTraversalRejected(t *testing.T) {
	mock := &mockExecutor{
		resolvePathFunc: func(string) (string, error) {
			return "", errors.New("path escapes workspace")
		},
	}
	find := FindFilesTool(mock)
	input, _ := json.Marshal(map[string]any{"name": "*.go", "path": "../../etc"})
	_, err := find.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected resolve-path error")
	}
	if !strings.Contains(err.Error(), "resolve search path") {
		t.Errorf("expected resolve search path error, got %v", err)
	}
}

func TestFindFilesTool_MaxResultsValidation(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})

	// Below the minimum.
	input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": 0})
	_, err := find.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "max_results must be >=") {
		t.Errorf("expected lower-bound error, got %v", err)
	}

	// Above the maximum.
	input, _ = json.Marshal(map[string]any{"name": "*.go", "max_results": 5000})
	_, err = find.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "max_results must be <=") {
		t.Errorf("expected upper-bound error, got %v", err)
	}
}

func TestGrepFilesTool_MaxResultsValidation(t *testing.T) {
	withRipgrepProbe(t, false)
	grep := GrepFilesTool(&fsExecutor{root: t.TempDir()})

	input, _ := json.Marshal(map[string]any{"pattern": "x", "max_results": 0})
	_, err := grep.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "max_results must be >=") {
		t.Errorf("expected lower-bound error, got %v", err)
	}

	input, _ = json.Marshal(map[string]any{"pattern": "x", "max_results": 5000})
	_, err = grep.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "max_results must be <=") {
		t.Errorf("expected upper-bound error, got %v", err)
	}
}

func TestGrepFilesTool_EmptyPattern(t *testing.T) {
	grep := GrepFilesTool(&fsExecutor{root: t.TempDir()})
	input, _ := json.Marshal(map[string]any{"pattern": ""})
	_, err := grep.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "pattern is required") {
		t.Errorf("expected pattern-required error, got %v", err)
	}
}

// TestFindFilesTool_SchemaBasenameSemantics pins the name-field description
// to make basename-only matching explicit. The pre-fix description included
// `**/handler_*.ts` as an example, but filepath.Match does not understand
// `**` and the matcher is basename-only, so models taking the example at
// face value got silent zero-match results.
func TestFindFilesTool_SchemaBasenameSemantics(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	schema := string(find.InputSchema)

	if !strings.Contains(schema, "basename only") {
		t.Errorf("name schema description must state basename-only semantics, got: %s", schema)
	}
	if !strings.Contains(schema, "Does not support '**'") {
		t.Errorf("name schema description must call out missing '**' support, got: %s", schema)
	}
	if strings.Contains(schema, "**/handler_*.ts") {
		t.Errorf("name schema must not advertise the misleading '**/handler_*.ts' example, got: %s", schema)
	}
	if !strings.Contains(schema, "include field") {
		t.Errorf("name schema description must direct path matching to the include field, got: %s", schema)
	}
}

func TestFindFilesTool_EmptyName(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	input, _ := json.Marshal(map[string]any{"name": ""})
	_, err := find.Handler(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected name-required error, got %v", err)
	}
}

func TestGrepFilesTool_RipgrepNoMatches(t *testing.T) {
	withRipgrepProbe(t, true)
	fe := &fsExecutor{
		root:    t.TempDir(),
		canExec: true,
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			// rg exit code 1 is "no matches found", not a hard error.
			return &executor.ExecResult{ExitCode: 1}, nil
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "absent"})
	out, err := grep.Handler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No matches found") {
		t.Errorf("expected no-match notice, got %q", out)
	}
}

// TestDoubleStarMatch_EscapesMetacharacters pins the regex-injection fix:
// the previous implementation escaped only `.`, so any other regex
// metacharacter in the glob produced either a malformed regex (the
// regexp.Compile error was silently swallowed, so the filter failed open
// — CWE-185) or a regex with unintended semantics (capturing groups,
// quantifiers). It also iterated by byte, splitting multi-byte UTF-8 across
// the default branch. This table covers both shapes.
func TestDoubleStarMatch_EscapesMetacharacters(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// Square brackets in the glob must be treated as literal, not as a
		// regex character class. Pre-fix this either compiled into a class
		// or threw an "[" parse error swallowed by the helper.
		{"literal brackets match", "src/[abc].go", "src/[abc].go", true},
		{"literal brackets no match", "src/[abc].go", "src/a.go", false},

		// Parentheses must not introduce a capturing group; the literal
		// path is "src/(util).go" not "src/util.go".
		{"literal parens match", "src/(util).go", "src/(util).go", true},
		{"literal parens no match", "src/(util).go", "src/util.go", false},

		// `+` is a regex quantifier; treated as a literal in glob.
		{"plus literal match", "src/a+b.go", "src/a+b.go", true},
		{"plus literal no match", "src/a+b.go", "src/aab.go", false},

		// Curly braces, pipes, dollar, caret — all regex metacharacters
		// that must round-trip as literals.
		{"braces literal", "src/{x}.go", "src/{x}.go", true},
		{"dollar literal", "src/$var.go", "src/$var.go", true},
		{"caret literal", "src/^a.go", "src/^a.go", true},

		// Non-ASCII path: pre-fix the byte-indexed loop garbled the
		// multi-byte rune into invalid regex fragments.
		{"utf8 match", "café/**/x.go", "café/sub/x.go", true},
		{"utf8 prefix mismatch", "café/**/x.go", "cafe/sub/x.go", false},

		// `**` and `*` still behave as wildcards (regression guard).
		{"double star match", "src/**/x.go", "src/a/b/x.go", true},
		{"single star match", "src/*.go", "src/foo.go", true},
		{"single star no cross segment", "src/*.go", "src/sub/foo.go", false},

		// `[!.]*` — leading `!` and `.` must be literals (filepath.Match
		// already handles this via globHit's basename/rel fall-throughs;
		// doubleStarMatch's job is to not crash). The regex must compile.
		{"bang dot star literal", "[!.]*", "[!.]xyz", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := doubleStarMatch(c.pattern, c.path)
			if got != c.want {
				t.Errorf("doubleStarMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
			}
		})
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
