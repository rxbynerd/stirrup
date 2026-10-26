package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
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

// HostWorkspaceRoot declares the HostPathWorkspace capability: fsExecutor
// stands in for LocalExecutor, resolving paths onto a real temp directory on
// the host, so it belongs on the host-backed dispatch branch the same way
// LocalExecutor does.
func (f *fsExecutor) HostWorkspaceRoot() string {
	return f.root
}

// TestGrepNative_PermissionDeniedSubdirSkipped covers the fs.ErrPermission
// branch in grepNative. A 0000-mode subdirectory inside the workspace must
// be skipped silently (returning fs.SkipDir from the callback) so that
// matches in readable siblings are still returned.
func TestGrepNative_PermissionDeniedSubdirSkipped(t *testing.T) {
	must := require.New(t)
	if os.Getuid() == 0 {
		t.Skip("running as root: 0000 mode does not deny access")
	}
	withRipgrepProbe(t, false)
	dir := t.TempDir()

	must.NoError(os.WriteFile(filepath.Join(dir, "readable.txt"), []byte("needle\n"), 0o644))
	locked := filepath.Join(dir, "locked")
	must.NoError(os.MkdirAll(locked, 0o755))
	must.NoError(os.WriteFile(filepath.Join(locked, "secret.txt"), []byte("needle\n"), 0o644))
	must.NoError(os.Chmod(locked, 0o000))
	// Restore mode on cleanup so t.TempDir() removal succeeds.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err, "permission-denied dir must not surface as an error")
	must.Contains(out, "readable.txt", "readable sibling must still match")
}

// TestFindNative_PermissionDeniedSubdirSkipped is the symmetric coverage
// for findNative's fs.ErrPermission branch.
func TestFindNative_PermissionDeniedSubdirSkipped(t *testing.T) {
	must := require.New(t)
	if os.Getuid() == 0 {
		t.Skip("running as root: 0000 mode does not deny access")
	}
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "readable.go"), []byte("x"), 0o644))
	locked := filepath.Join(dir, "locked")
	must.NoError(os.MkdirAll(locked, 0o755))
	must.NoError(os.WriteFile(filepath.Join(locked, "secret.go"), []byte("x"), 0o644))
	must.NoError(os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	out, err := invokeText(context.Background(), find, input)
	must.NoError(err, "permission-denied dir must not surface as an error")
	must.Contains(out, "readable.go", "readable sibling must still match")
}

// TestRipgrepDetector_CachesProbeResult covers the once-cache semantics of the
// detector itself rather than going via the package-level seam. The
// existing withRipgrepProbe helper swaps the whole detector, which means
// the production once-cache path (sync.Once + atomic.Bool) is never
// exercised by the rest of the suite. A regression that breaks the
// caching contract — e.g. dropping sync.Once for an unsynchronised
// re-probe per call — would not be caught.
func TestRipgrepDetector_CachesProbeResult(t *testing.T) {
	must := require.New(t)
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

	must.EqualValues(1, probeCount.Load(), "probe must run exactly once")
	must.Equal([]bool{true, true, true}, []bool{first, second, third},
		"all detect() calls must return the cached value")
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
	must := require.New(t)
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc Hello() {}\n"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "b.txt"), []byte("nothing here\n"), 0o644))

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "Hello"})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.Contains(out, "a.go:2:func Hello() {}", "expected path:line:match in output")
}

func TestGrepFilesTool_InvalidRegex(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, false)
	grep := GrepFilesTool(&fsExecutor{root: t.TempDir()})
	// `[` is an unbalanced character class — a common mistake for models
	// that confuse shell globs with regexes.
	input, _ := json.Marshal(map[string]any{"pattern": "["})
	_, err := invokeText(context.Background(), grep, input)
	must.ErrorContains(err, "invalid regex")
}

func TestGrepFilesTool_MaxResultsBound(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	// 5 matching files, ask for at most 2.
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, "file"+string(rune('a'+i))+".txt")
		must.NoError(os.WriteFile(path, []byte("needle\n"), 0o644))
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "needle", "max_results": 2})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.Len(strings.Split(out, "\n"), 2, "max_results must cap the output at two matches")
}

func TestGrepFilesTool_IncludeExclude(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "skip.go"), []byte("needle\n"), 0o644))

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{
		"pattern": "needle",
		"include": []string{"*.go"},
		"exclude": []string{"skip.go"},
	})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.Contains(out, "a.go:", "expected a.go in result")
	must.NotContains(out, "a.txt", "a.txt must be excluded by the include filter")
	must.NotContains(out, "skip.go", "skip.go must be excluded by the exclude filter")
}

func TestGrepFilesTool_NoMatches(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("hello\n"), 0o644))

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "absent_pattern"})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.Contains(out, "No matches found")
}

func TestGrepFilesTool_BinaryFilesSkipped(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	// Binary file contains the literal bytes of "match" but also a NUL —
	// the heuristic should skip it so we don't dump garbage at the model.
	must.NoError(os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("match\x00more"), 0o644))
	// Plain text file with the same word; this one should match.
	must.NoError(os.WriteFile(filepath.Join(dir, "text.txt"), []byte("match\n"), 0o644))

	grep := GrepFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "match"})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.NotContains(out, "blob.bin", "binary file must be skipped")
	must.Contains(out, "text.txt", "text file must match")
}

func TestGrepFilesTool_RipgrepPathSelected(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, true)
	var capturedCmd string
	fe := &fsExecutor{
		root:    t.TempDir(),
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			capturedCmd = command
			return &executor.ExecResult{
				ExitCode: 0,
				Stdout:   `{"type":"match","data":{"path":{"text":"x.go"},"lines":{"text":"hit\n"},"line_number":1}}` + "\n",
			}, nil
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{
		"pattern": "hit",
		"include": []string{"*.go"},
		"exclude": []string{"vendor/**"},
	})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.True(strings.HasPrefix(capturedCmd, "rg "), "expected rg command, got %q", capturedCmd)
	must.Contains(capturedCmd, "--json", "rg must run with --json")
	// The rendered text is reconstructed from the JSON fields and must match
	// the historical "path:line:text" form byte-for-byte.
	must.Equal("x.go:1:hit", out, "rg output must be reconstructed as path:line:text")
	// Each include glob must be passed to rg as --glob 'pattern' and each
	// exclude glob as --glob '!pattern'.
	must.Contains(capturedCmd, "--glob '*.go'", "include glob must be passed to rg")
	must.Contains(capturedCmd, "--glob '!vendor/**'", "exclude glob must be passed to rg negated")
}

func TestGrepFilesTool_RipgrepFallsBackOnHardError(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, true)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("foo\n"), 0o644))
	fe := &fsExecutor{
		root:    dir,
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 2, Stderr: "rg: borken"}, nil
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "foo"})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	// Native fallback should have found the match in a.go.
	must.Contains(out, "a.go", "native fallback must surface the result")
}

// TestGrepNative_SkipsSymlinks pins that grep_files never reads through a
// workspace symlink to a file outside the workspace (CWE-59). exec.ResolvePath
// only validates the search root, so the WalkDir callback must skip symlink
// entries before os.ReadFile, which would otherwise follow the link.
func TestGrepNative_SkipsSymlinks(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, false)
	workspace := t.TempDir()
	outside := t.TempDir()

	must.NoError(os.WriteFile(filepath.Join(workspace, "regular.txt"), []byte("match-target\n"), 0o644))
	externalPath := filepath.Join(outside, "secret.txt")
	must.NoError(os.WriteFile(externalPath, []byte("match-target\n"), 0o644))
	symlinkPath := filepath.Join(workspace, "escape.txt")
	if err := os.Symlink(externalPath, symlinkPath); err != nil {
		t.Skipf("symlink not supported on this platform: %v", err)
	}

	grep := GrepFilesTool(&fsExecutor{root: workspace})
	input, _ := json.Marshal(map[string]any{"pattern": "match-target"})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.Contains(out, "regular.txt", "regular file must match")
	must.NotContains(out, "escape.txt", "symlinked path must not appear in results (CWE-59)")
	must.NotContains(out, "secret.txt", "external symlink target must not be surfaced")
}

// TestGrepViaRipgrep_TransportErrorFallsBackToNative pins the resilience
// contract: an executor transport failure (Docker socket flake, container
// restart, microVM hiccup) must NOT escalate to a hard tool-call error.
// rg exit code >= 2 already falls back to native; transport failure
// preventing rg from launching is less severe and should behave the same.
func TestGrepViaRipgrep_TransportErrorFallsBackToNative(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, true)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644))
	fe := &fsExecutor{
		root:    dir,
		canExec: true,
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return nil, errors.New("docker socket: connection refused")
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err, "transport error must not surface; expected native fallback")
	must.Contains(out, "a.go", "native walker must surface a.go")
}

// TestGrepViaRipgrep_ContextCancelPropagates pins the other half: when the
// executor returns context.Canceled (the caller asked to stop), we must
// NOT silently re-walk natively. The cancellation must propagate.
func TestGrepViaRipgrep_ContextCancelPropagates(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, true)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644))
	fe := &fsExecutor{
		root:    dir,
		canExec: true,
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return nil, context.Canceled
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	_, err := invokeText(context.Background(), grep, input)
	must.ErrorIs(err, context.Canceled)
}

func TestGrepFilesTool_PathTraversalRejected(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		resolvePathFunc: func(relativePath string) (string, error) {
			return "", errors.New("path escapes workspace")
		},
	}
	grep := GrepFilesTool(mock)
	input, _ := json.Marshal(map[string]any{"pattern": "secret", "path": "../../etc"})
	_, err := invokeText(context.Background(), grep, input)
	must.ErrorContains(err, "resolve search path")
}

// --- FindFilesTool tests ---

func TestFindFilesTool_GlobMatch(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644))

	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	out, err := invokeText(context.Background(), find, input)
	must.NoError(err)
	must.Contains(out, "a.go", "expected a.go in result")
	must.NotContains(out, "b.txt", "b.txt must not match *.go")
}

func TestFindFilesTool_RecursesIntoSubdirs(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	must.NoError(os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755))
	must.NoError(os.WriteFile(filepath.Join(dir, "sub", "deep", "handler_user.go"), []byte("x"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "noise.txt"), []byte("x"), 0o644))

	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "handler_*.go"})
	out, err := invokeText(context.Background(), find, input)
	must.NoError(err)
	must.Contains(out, filepath.Join("sub", "deep", "handler_user.go"), "expected nested file in result")
}

func TestFindFilesTool_MaxResultsBound(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		must.NoError(os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))+".go"), []byte("x"), 0o644))
	}
	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": 2})
	out, err := invokeText(context.Background(), find, input)
	must.NoError(err)
	must.Len(strings.Split(out, "\n"), 2, "max_results must cap the output at two matches")
}

func TestFindFilesTool_NoMatches(t *testing.T) {
	must := require.New(t)
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	input, _ := json.Marshal(map[string]any{"name": "*.nope"})
	out, err := invokeText(context.Background(), find, input)
	must.NoError(err)
	must.Contains(out, "No matches found")
}

func TestFindFilesTool_InvalidGlob(t *testing.T) {
	must := require.New(t)
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	// An unbalanced bracket is rejected by filepath.Match.
	input, _ := json.Marshal(map[string]any{"name": "[broken"})
	_, err := invokeText(context.Background(), find, input)
	must.ErrorContains(err, "invalid glob")
}

func TestFindFilesTool_PathTraversalRejected(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		resolvePathFunc: func(string) (string, error) {
			return "", errors.New("path escapes workspace")
		},
	}
	find := FindFilesTool(mock)
	input, _ := json.Marshal(map[string]any{"name": "*.go", "path": "../../etc"})
	_, err := invokeText(context.Background(), find, input)
	must.ErrorContains(err, "resolve search path")
}

func TestFindFilesTool_MaxResultsValidation(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})

	t.Run("below minimum is rejected", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": 0})
		_, err := invokeText(context.Background(), find, input)
		must.ErrorContains(err, "max_results must be >=")
	})

	t.Run("above maximum is rejected", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": 5000})
		_, err := invokeText(context.Background(), find, input)
		must.ErrorContains(err, "max_results must be <=")
	})
}

func TestGrepFilesTool_MaxResultsValidation(t *testing.T) {
	withRipgrepProbe(t, false)
	grep := GrepFilesTool(&fsExecutor{root: t.TempDir()})

	t.Run("below minimum is rejected", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"pattern": "x", "max_results": 0})
		_, err := invokeText(context.Background(), grep, input)
		must.ErrorContains(err, "max_results must be >=")
	})

	t.Run("above maximum is rejected", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"pattern": "x", "max_results": 5000})
		_, err := invokeText(context.Background(), grep, input)
		must.ErrorContains(err, "max_results must be <=")
	})
}

func TestGrepFilesTool_EmptyPattern(t *testing.T) {
	must := require.New(t)
	grep := GrepFilesTool(&fsExecutor{root: t.TempDir()})
	input, _ := json.Marshal(map[string]any{"pattern": ""})
	_, err := invokeText(context.Background(), grep, input)
	must.ErrorContains(err, "pattern is required")
}

// TestFindFilesTool_SchemaBasenameSemantics pins the name-field description
// to make basename-only matching explicit: filepath.Match does not
// understand `**` and the matcher is basename-only, so an example implying
// otherwise would lead models to silent zero-match results.
func TestFindFilesTool_SchemaBasenameSemantics(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	schema := string(find.InputSchema)

	t.Run("states basename-only semantics", func(t *testing.T) {
		must := require.New(t)
		must.Contains(schema, "basename only")
	})
	t.Run("calls out missing ** support", func(t *testing.T) {
		must := require.New(t)
		must.Contains(schema, "Does not support '**'")
	})
	t.Run("omits the misleading path example", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(schema, "**/handler_*.ts")
	})
	t.Run("directs path matching to the include field", func(t *testing.T) {
		must := require.New(t)
		must.Contains(schema, "include field")
	})
}

func TestFindFilesTool_EmptyName(t *testing.T) {
	must := require.New(t)
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})
	input, _ := json.Marshal(map[string]any{"name": ""})
	_, err := invokeText(context.Background(), find, input)
	must.ErrorContains(err, "name is required")
}

func TestGrepFilesTool_RipgrepNoMatches(t *testing.T) {
	must := require.New(t)
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
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.Contains(out, "No matches found")
}

// TestDoubleStarMatch_EscapesMetacharacters pins the regex-injection guard: a
// glob containing a regex metacharacter must translate to a regex matching
// it literally rather than a malformed pattern (CWE-185, filter fails open)
// or unintended semantics (capturing groups, quantifiers). It also covers
// rune-safe iteration over multi-byte UTF-8 path segments.
func TestDoubleStarMatch_EscapesMetacharacters(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// Square brackets in the glob must be treated as literal, not as a
		// regex character class.
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

		// Non-ASCII path: iteration must be rune-safe, not byte-indexed.
		{"utf8 match", "café/**/x.go", "café/sub/x.go", true},
		{"utf8 prefix mismatch", "café/**/x.go", "cafe/sub/x.go", false},

		// `**` and `*` still behave as wildcards (regression guard).
		{"double star match", "src/**/x.go", "src/a/b/x.go", true},
		{"double star segment matches zero directories", "src/**/x.go", "src/x.go", true},
		{"leading double star matches a root file", "**/*.txt", "a.txt", true},
		{"leading double star matches a nested file", "**/*.txt", "notes/b.txt", true},
		{"leading double star keeps the suffix", "**/*.txt", "notes/b.go", false},
		{"double star inside a segment needs its slash", "foo**/bar", "foobar", false},
		{"single star match", "src/*.go", "src/foo.go", true},
		{"single star no cross segment", "src/*.go", "src/sub/foo.go", false},

		// `[!.]*` — leading `!` and `.` must be literals (filepath.Match
		// already handles this via globHit's basename/rel fall-throughs;
		// doubleStarMatch's job is to not crash). The regex must compile.
		{"bang dot star literal", "[!.]*", "[!.]xyz", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			must := require.New(t)
			got := doubleStarMatch(c.pattern, c.path)
			must.Equal(c.want, got, "doubleStarMatch(%q, %q)", c.pattern, c.path)
		})
	}
}

func TestFindFilesTool_IncludeDoubleStarGlob(t *testing.T) {
	must := require.New(t)
	// `**` is not supported by filepath.Match (which is shell-style) — the
	// model is more likely to pass a basename like "handler_*.go" and rely
	// on the walker recursing. But include filters DO honour `**`. Verify
	// that path.
	dir := t.TempDir()
	must.NoError(os.MkdirAll(filepath.Join(dir, "pkg", "deep"), 0o755))
	_ = os.WriteFile(filepath.Join(dir, "pkg", "deep", "x.go"), []byte("y"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "top.go"), []byte("y"), 0o644)

	find := FindFilesTool(&fsExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{
		"name":    "*.go",
		"include": []string{"pkg/**"},
	})
	out, err := invokeText(context.Background(), find, input)
	must.NoError(err)

	t.Run("excludes files outside the include", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(out, "top.go", "got %q", out)
	})
	t.Run("includes nested files under the include", func(t *testing.T) {
		must := require.New(t)
		must.Contains(out, filepath.Join("pkg", "deep", "x.go"), "got %q", out)
	})
}

func TestMatchesFilters_LeadingDoubleStarMatchesZeroDirectories(t *testing.T) {
	cases := []struct {
		name    string
		rel     string
		include []string
		exclude []string
		want    bool
	}{
		{"include matches a root file", "a.txt", []string{"**/*.txt"}, nil, true},
		{"include matches a nested file", "notes/b.txt", []string{"**/*.txt"}, nil, true},
		{"include matches the directory itself", "src/x.go", []string{"src/**/x.go"}, nil, true},
		{"exclude vetoes a root file", "a.txt", nil, []string{"**/*.txt"}, false},
		{"exclude vetoes a nested file", "notes/b.txt", nil, []string{"**/*.txt"}, false},
		{"exclude leaves other files", "main.go", nil, []string{"**/*.txt"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(c.want, matchesFilters(path.Base(c.rel), c.rel, c.include, c.exclude))
		})
	}
}

func TestFindFilesTool_LeadingDoubleStarIncludesRootFiles(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	must.NoError(os.MkdirAll(filepath.Join(dir, "notes"), 0o755))
	must.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("y"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "notes", "b.txt"), []byte("y"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "main.go"), []byte("y"), 0o644))
	find := FindFilesTool(&fsExecutor{root: dir})

	t.Run("include", func(t *testing.T) {
		must := require.New(t)
		input, err := json.Marshal(map[string]any{"name": "*", "include": []string{"**/*.txt"}})
		must.NoError(err)
		fr := decodeFindResult(t, find, input)
		must.ElementsMatch([]string{"a.txt", filepath.Join("notes", "b.txt")}, fr.Paths)
	})
	t.Run("exclude", func(t *testing.T) {
		must := require.New(t)
		input, err := json.Marshal(map[string]any{"name": "*", "exclude": []string{"**/*.txt"}})
		must.NoError(err)
		fr := decodeFindResult(t, find, input)
		must.Equal([]string{"main.go"}, fr.Paths)
	})
}

func TestGrepFilesTool_LeadingDoubleStarIncludesRootFiles(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	must.NoError(os.MkdirAll(filepath.Join(dir, "notes"), 0o755))
	must.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "notes", "b.txt"), []byte("needle\n"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "main.go"), []byte("needle\n"), 0o644))
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, err := json.Marshal(map[string]any{"pattern": "needle", "include": []string{"**/*.txt"}})
	must.NoError(err)
	out, err := invokeText(context.Background(), grep, input)
	must.NoError(err)
	must.Contains(out, "a.txt")
	must.Contains(out, filepath.Join("notes", "b.txt"))
	must.NotContains(out, "main.go")
}

// --- Truncated boundary coverage ---

// decodeSearchResult pulls the structured searchResult envelope out of a grep
// invocation, asserting the Kind is the search-result kind.
func decodeSearchResult(t *testing.T, tl *tool.Tool, input json.RawMessage) searchResult {
	t.Helper()
	must := require.New(t)
	res, err := tl.StructuredHandler(context.Background(), input)
	must.NoError(err)
	must.Equal(kindSearchResult, res.Kind)
	var sr searchResult
	must.NoError(json.Unmarshal(res.Structured, &sr))
	return sr
}

// decodeFindResult is the find-side analogue of decodeSearchResult.
func decodeFindResult(t *testing.T, tl *tool.Tool, input json.RawMessage) findResult {
	t.Helper()
	must := require.New(t)
	res, err := tl.StructuredHandler(context.Background(), input)
	must.NoError(err)
	must.Equal(kindFindResult, res.Kind)
	var fr findResult
	must.NoError(json.Unmarshal(res.Structured, &fr))
	return fr
}

// rgMatchEvents builds an rg --json stdout stream of n distinct match events,
// one per synthetic file, so a stubbed executor can drive the count the
// look-ahead probe sees on the ripgrep path.
func rgMatchEvents(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		// Distinct paths keep the events from collapsing into one file name.
		fmt.Fprintf(&b, `{"type":"match","data":{"path":{"text":"f%d.go"},"lines":{"text":"hit\n"},"line_number":1}}`+"\n", i)
	}
	return b.String()
}

// TestGrepFilesTool_TruncatedBoundary_Native exercises the look-ahead probe on
// the Go-native walker: a count landing exactly on max_results must report
// Truncated:false, one past must report true and trim the probe element, and a
// count below the cap must report false.
func TestGrepFilesTool_TruncatedBoundary_Native(t *testing.T) {
	withRipgrepProbe(t, false)
	const maxResults = 3
	cases := []struct {
		name          string
		files         int
		wantTruncated bool
		wantMatches   int
	}{
		{"below cap", maxResults - 1, false, maxResults - 1},
		{"exactly at cap", maxResults, false, maxResults},
		{"one past cap", maxResults + 1, true, maxResults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			dir := t.TempDir()
			for i := 0; i < tc.files; i++ {
				path := filepath.Join(dir, "file"+string(rune('a'+i))+".txt")
				must.NoError(os.WriteFile(path, []byte("needle\n"), 0o644))
			}
			grep := GrepFilesTool(&fsExecutor{root: dir})
			input, _ := json.Marshal(map[string]any{"pattern": "needle", "max_results": maxResults})
			sr := decodeSearchResult(t, grep, input)
			must.Equal(tc.wantTruncated, sr.Truncated)
			must.Len(sr.Matches, tc.wantMatches)
		})
	}
}

// TestGrepFilesTool_TruncatedBoundary_Ripgrep is the symmetric boundary
// coverage for the ripgrep path. The stubbed executor returns a configurable
// number of match events so the look-ahead probe (parseRipgrepJSON capped at
// maxResults+1) is what distinguishes a clean fit from genuine truncation.
func TestGrepFilesTool_TruncatedBoundary_Ripgrep(t *testing.T) {
	withRipgrepProbe(t, true)
	const maxResults = 3
	cases := []struct {
		name          string
		events        int
		wantTruncated bool
		wantMatches   int
	}{
		{"below cap", maxResults - 1, false, maxResults - 1},
		{"exactly at cap", maxResults, false, maxResults},
		{"one past cap", maxResults + 1, true, maxResults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var gotCmd string
			fe := &fsExecutor{
				root:    t.TempDir(),
				canExec: true,
				execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
					gotCmd = command
					return &executor.ExecResult{ExitCode: 0, Stdout: rgMatchEvents(tc.events)}, nil
				},
			}
			grep := GrepFilesTool(fe)
			input, _ := json.Marshal(map[string]any{"pattern": "hit", "max_results": maxResults})
			sr := decodeSearchResult(t, grep, input)
			// The look-ahead probe lives in --max-count: rg must be asked for
			// maxResults+1 so a count landing exactly on the cap stays
			// distinguishable from genuine truncation.
			wantMaxCount := fmt.Sprintf("--max-count %d", maxResults+1)
			must.Contains(gotCmd, wantMaxCount)
			must.Equal(tc.wantTruncated, sr.Truncated)
			must.Len(sr.Matches, tc.wantMatches)
		})
	}
}

// TestGrepFilesTool_HostRipgrepOutputCapMarksIncomplete pins the
// HostPathWorkspace branch's OutputTruncated wiring: an executor-level
// output cap that cuts off rg's stdout must mark the scan incomplete, even
// when the invocation succeeds and the matches before the cut parse cleanly.
func TestGrepFilesTool_HostRipgrepOutputCapMarksIncomplete(t *testing.T) {
	must := require.New(t)
	withRipgrepProbe(t, true)
	fe := &fsExecutor{
		root:    t.TempDir(),
		canExec: true,
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				ExitCode:        0,
				Stdout:          `{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"hit\n"},"line_number":1}}` + "\n",
				OutputTruncated: true,
			}, nil
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "hit"})
	sr := decodeSearchResult(t, grep, input)
	must.Len(sr.Matches, 1, "the one parsed match must be kept, got %+v", sr.Matches)
	must.True(sr.Truncated, "an executor-level output cap must mark the scan incomplete")
}

// TestFindFilesTool_TruncatedBoundary exercises the look-ahead probe on the
// find walker across the same three boundary cases.
func TestFindFilesTool_TruncatedBoundary(t *testing.T) {
	const maxResults = 3
	cases := []struct {
		name          string
		files         int
		wantTruncated bool
		wantPaths     int
	}{
		{"below cap", maxResults - 1, false, maxResults - 1},
		{"exactly at cap", maxResults, false, maxResults},
		{"one past cap", maxResults + 1, true, maxResults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			dir := t.TempDir()
			for i := 0; i < tc.files; i++ {
				must.NoError(os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))+".go"), []byte("x"), 0o644))
			}
			find := FindFilesTool(&fsExecutor{root: dir})
			input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": maxResults})
			fr := decodeFindResult(t, find, input)
			must.Equal(tc.wantTruncated, fr.Truncated)
			must.Len(fr.Paths, tc.wantPaths)
		})
	}
}

// --- Schema bound tests ---

// TestGrepFilesTool_SchemaBoundsPatternAndGlobs pins the maxLength/maxItems
// bounds on pattern and include/exclude, enforced by the loop's
// security.ValidateJSONSchema call before the handler ever runs — these are
// runtime-enforced limits, not documentation.
func TestGrepFilesTool_SchemaBoundsPatternAndGlobs(t *testing.T) {
	grep := GrepFilesTool(&fsExecutor{root: t.TempDir()})

	t.Run("pattern within bound", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"pattern": strings.Repeat("a", 1000)})
		must.NoError(security.ValidateJSONSchema(input, grep.InputSchema))
	})
	t.Run("pattern over bound rejected", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"pattern": strings.Repeat("a", 1001)})
		must.Error(security.ValidateJSONSchema(input, grep.InputSchema))
	})
	t.Run("too many include globs rejected", func(t *testing.T) {
		must := require.New(t)
		globs := make([]string, 51)
		for i := range globs {
			globs[i] = "*.go"
		}
		input, _ := json.Marshal(map[string]any{"pattern": "x", "include": globs})
		must.Error(security.ValidateJSONSchema(input, grep.InputSchema))
	})
	t.Run("over-length glob rejected", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"pattern": "x", "include": []string{strings.Repeat("a", 501)}})
		must.Error(security.ValidateJSONSchema(input, grep.InputSchema))
	})
}

// TestFindFilesTool_SchemaBoundsNameAndGlobs is the find_files analogue.
func TestFindFilesTool_SchemaBoundsNameAndGlobs(t *testing.T) {
	find := FindFilesTool(&fsExecutor{root: t.TempDir()})

	t.Run("name within bound", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"name": strings.Repeat("a", 255)})
		must.NoError(security.ValidateJSONSchema(input, find.InputSchema))
	})
	t.Run("name over bound rejected", func(t *testing.T) {
		must := require.New(t)
		input, _ := json.Marshal(map[string]any{"name": strings.Repeat("a", 256)})
		must.Error(security.ValidateJSONSchema(input, find.InputSchema))
	})
}
