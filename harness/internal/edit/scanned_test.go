package edit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security/codescanner"
	"github.com/rxbynerd/stirrup/types"
)

// fakeEditStrategy stands in for an EditStrategy that we control
// completely: a fixed result, an optional error, and a tool definition
// the wrapper does not actually inspect.
type fakeEditStrategy struct {
	result   *EditResult
	applyErr error
}

func (f *fakeEditStrategy) ToolDefinition() types.ToolDefinition {
	return types.ToolDefinition{Name: "edit_file"}
}

func (f *fakeEditStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return f.result, nil
}

// fakeScanner returns canned findings or an error and counts calls so
// tests can assert the wrapper invoked it (or did not).
type fakeScanner struct {
	findings []codescanner.Finding
	err      error
	calls    int
}

func (f *fakeScanner) Scan(ctx context.Context, path string, content []byte) (*codescanner.ScanResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &codescanner.ScanResult{Findings: f.findings}, nil
}

// readFailExec satisfies executor.Executor with a controllable ReadFile
// failure. WriteFile is a no-op (the rollback path may invoke it after
// a block) and the rest are stubs. We do not embed a real LocalExecutor
// because we need the read failure to be deterministic.
type readFailExec struct {
	readErr error
}

func (r *readFailExec) ReadFile(ctx context.Context, path string) (string, error) {
	return "", r.readErr
}
func (r *readFailExec) WriteFile(ctx context.Context, path string, content string) error {
	return nil
}
func (r *readFailExec) ListDirectory(ctx context.Context, path string) ([]string, error) {
	return nil, nil
}
func (r *readFailExec) Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	return nil, errors.New("exec unsupported in test")
}
func (r *readFailExec) ResolvePath(p string) (string, error) { return p, nil }
func (r *readFailExec) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true, CanWrite: true}
}

// recordingEmitter captures Emit calls for assertions.
type recordingEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
}

type emittedEvent struct {
	level string
	event string
	data  map[string]any
}

func (r *recordingEmitter) Emit(level, event string, data map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, emittedEvent{level: level, event: event, data: data})
}

func (r *recordingEmitter) snapshot() []emittedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]emittedEvent, len(r.events))
	copy(out, r.events)
	return out
}

// codeScanWarnings returns the code_scan_warning events among events.
func codeScanWarnings(events []emittedEvent) []emittedEvent {
	var out []emittedEvent
	for _, ev := range events {
		if ev.event == "code_scan_warning" {
			out = append(out, ev)
		}
	}
	return out
}

func TestNewScannedStrategy_NilScannerReturnsInner(t *testing.T) {
	must := require.New(t)
	inner := NewWholeFileStrategy()
	got := NewScannedStrategy(inner, nil, nil, nil)
	must.Same(inner, got, "nil scanner must return inner unchanged")
}

func TestNewScannedStrategy_NoneScannerReturnsInner(t *testing.T) {
	must := require.New(t)
	inner := NewWholeFileStrategy()
	got := NewScannedStrategy(inner, codescanner.NoneScanner{}, nil, nil)
	must.Same(inner, got, "NoneScanner must return inner unchanged")
}

func TestScannedStrategy_BlockOnPlantedSecret_RestoresOriginal(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "config.js", "const x = 1;\n")

	emitter := &recordingEmitter{}
	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(NewWholeFileStrategy(), scanner, &types.CodeScannerConfig{Type: "patterns"}, emitter)

	input := json.RawMessage(`{
		"path": "config.js",
		"content": "const apiKey = 'sk-ant-1234567890abcdef';\n"
	}`)

	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied, "block must not mark the edit applied")
	must.Contains(result.Error, "code scan blocked edit")
	must.Contains(result.Error, "secret/anthropic_api_key", "error must name the rule")

	// The secret-bearing content must not survive a block.
	got, err := exec.ReadFile(context.Background(), "config.js")
	must.NoError(err)
	must.Equal("const x = 1;\n", got, "rollback must restore the original file")

	must.Empty(codeScanWarnings(emitter.snapshot()), "block path must not emit code_scan_warning")
}

func TestScannedStrategy_BlockOnNewFile_ZerosFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(NewWholeFileStrategy(), scanner, &types.CodeScannerConfig{Type: "patterns"}, &recordingEmitter{})

	input := json.RawMessage(`{
		"path": "secrets.txt",
		"content": "ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"
	}`)

	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)

	// The executor has no delete primitive, so a blocked new file is emptied.
	got, err := exec.ReadFile(context.Background(), "secrets.txt")
	must.NoError(err)
	must.Empty(got, "blocked new file must be emptied on rollback")
}

func TestScannedStrategy_WarnOnEvalSink_AppliesAndEmits(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "run.py", "def run():\n    pass\n")

	emitter := &recordingEmitter{}
	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(NewWholeFileStrategy(), scanner, &types.CodeScannerConfig{Type: "patterns"}, emitter)

	newContent := "def run(expr):\n    return eval(expr)\n"
	input, _ := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: "run.py", Content: newContent})

	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true on warn-only finding, got error: %s", result.Error)

	got, err := exec.ReadFile(context.Background(), "run.py")
	must.NoError(err)
	must.Equal(newContent, got, "warn must not roll back")

	events := emitter.snapshot()
	warnings := codeScanWarnings(events)
	must.NotEmpty(warnings, "expected at least one code_scan_warning event, got: %+v", events)
	for _, ev := range warnings {
		must.Equal("warn", ev.level)
		must.Equal("sink/dynamic_eval", ev.data["rule"])
	}
}

// A JavaScript template literal is not shell command substitution, so
// the edit pipeline must apply it without emitting a scan warning.
func TestScannedStrategy_JavaScriptTemplateLiteral_NoWarning(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "springboard.js", "const x = 1;\n")

	emitter := &recordingEmitter{}
	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(NewWholeFileStrategy(), scanner, &types.CodeScannerConfig{Type: "patterns"}, emitter)

	newContent := "debug(`springboard.js loaded: ${chrome.runtime.id}`);\n"
	input, _ := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: "springboard.js", Content: newContent})

	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true, got error: %s", result.Error)
	must.Empty(codeScanWarnings(emitter.snapshot()), "template literal must not emit code_scan_warning")
}

// The scanner sees the path the tool was given, while the executor
// writes through symlinks, so a shell payload can reach a shell file
// under a name that says otherwise. Content carrying a shell shebang
// must still be scanned as shell.
func TestScannedStrategy_ShebangPayloadScannedUnderForeignExtension(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "deploy.sh", "echo hi\n")
	must.NoError(os.Symlink(filepath.Join(dir, "deploy.sh"), filepath.Join(dir, "notes.txt")))

	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(NewWholeFileStrategy(), scanner,
		&types.CodeScannerConfig{Type: "patterns", BlockOnWarn: true}, &recordingEmitter{})

	input, _ := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: "notes.txt", Content: "#!/bin/bash\nUSER=`whoami`\n"})

	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied, "expected the shell payload to be blocked")
	must.Contains(result.Error, "sink/shell_backtick")

	got, err := exec.ReadFile(context.Background(), "deploy.sh")
	must.NoError(err)
	must.Equal("echo hi\n", got, "symlink target must be restored")
}

func TestScannedStrategy_BlockOnWarn_PromotesAndRollsBack(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)
	writeTestFile(t, dir, "run.py", "x = 1\n")

	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(
		NewWholeFileStrategy(),
		scanner,
		&types.CodeScannerConfig{Type: "patterns", BlockOnWarn: true},
		&recordingEmitter{},
	)

	input := json.RawMessage(`{
		"path": "run.py",
		"content": "result = eval(thing)\n"
	}`)

	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied, "BlockOnWarn must promote warn to block")

	got, _ := exec.ReadFile(context.Background(), "run.py")
	must.Equal("x = 1\n", got, "rollback must restore the original under BlockOnWarn")
}

func TestScannedStrategy_CleanContent_NoEvents(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	emitter := &recordingEmitter{}
	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(NewWholeFileStrategy(), scanner, nil, emitter)

	input := json.RawMessage(`{
		"path": "main.go",
		"content": "package main\n\nfunc main() {}\n"
	}`)

	result, err := strat.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true: %s", result.Error)
	must.Empty(emitter.snapshot(), "clean edit must not emit events")
}

// TestScannedStrategy_InnerApplyError_PropagatesAndSkipsScanner asserts
// that an error from the inner strategy propagates verbatim and the
// scanner does not run (which would otherwise scan stale or nonexistent
// content).
func TestScannedStrategy_InnerApplyError_PropagatesAndSkipsScanner(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	innerErr := errors.New("inner failed")
	inner := &fakeEditStrategy{applyErr: innerErr}
	scanner := &fakeScanner{}
	strat := NewScannedStrategy(inner, scanner, &types.CodeScannerConfig{Type: "patterns"}, nil)

	_, err := strat.Apply(context.Background(), json.RawMessage(`{"path":"x.go"}`), exec)
	must.ErrorIs(err, innerErr)
	must.Zero(scanner.calls, "scanner must not run after inner failure")
}

// TestScannedStrategy_ScannerError_Propagates asserts that a hard scanner
// error surfaces as a hard error rather than a silent passthrough of the
// inner result.
func TestScannedStrategy_ScannerError_Propagates(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	inner := &fakeEditStrategy{
		result: &EditResult{Path: "x.go", Applied: true},
	}
	scanErr := errors.New("scan failed")
	scanner := &fakeScanner{err: scanErr}
	// Plant a file so inner.Apply's "successful" result has something
	// for ScannedStrategy to read back before invoking the scanner.
	writeTestFile(t, dir, "x.go", "package x\n")
	strat := NewScannedStrategy(inner, scanner, &types.CodeScannerConfig{Type: "patterns"}, nil)

	_, err := strat.Apply(context.Background(), json.RawMessage(`{"path":"x.go"}`), exec)
	must.ErrorIs(err, scanErr)
}

// TestScannedStrategy_PostApplyReadError_FailsClosed asserts that if the
// executor can no longer read the file just written, the wrapper fails
// closed rather than skipping the scan.
func TestScannedStrategy_PostApplyReadError_FailsClosed(t *testing.T) {
	must := require.New(t)
	inner := &fakeEditStrategy{
		result: &EditResult{Path: "missing.go", Applied: true},
	}
	scanner := &fakeScanner{}
	exec := &readFailExec{readErr: errors.New("read flaked")}
	strat := NewScannedStrategy(inner, scanner, &types.CodeScannerConfig{Type: "patterns"}, nil)

	_, err := strat.Apply(context.Background(), json.RawMessage(`{"path":"missing.go"}`), exec)
	must.ErrorContains(err, "read", "expected error to mention the read failure")
	must.Zero(scanner.calls, "scanner must not run when post-apply read fails")
}
