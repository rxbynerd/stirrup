package edit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestNewScannedStrategy_NilScannerReturnsInner(t *testing.T) {
	inner := NewWholeFileStrategy()
	got := NewScannedStrategy(inner, nil, nil, nil)
	if got != inner {
		t.Errorf("nil scanner must return inner unchanged")
	}
}

func TestNewScannedStrategy_NoneScannerReturnsInner(t *testing.T) {
	inner := NewWholeFileStrategy()
	got := NewScannedStrategy(inner, codescanner.NoneScanner{}, nil, nil)
	if got != inner {
		t.Errorf("NoneScanner must return inner unchanged")
	}
}

func TestScannedStrategy_BlockOnPlantedSecret_RestoresOriginal(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Errorf("expected Applied=false on block, got true")
	}
	if !strings.Contains(result.Error, "code scan blocked edit") {
		t.Errorf("expected blocking error message, got: %q", result.Error)
	}
	if !strings.Contains(result.Error, "secret/anthropic_api_key") {
		t.Errorf("expected error to name the rule, got: %q", result.Error)
	}

	// The original file content must be restored on disk — the
	// secret-bearing content must not survive a block.
	got, err := exec.ReadFile(context.Background(), "config.js")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != "const x = 1;\n" {
		t.Errorf("rollback failed: file contains %q", got)
	}

	// Block path does not emit code_scan_warning.
	for _, ev := range emitter.snapshot() {
		if ev.event == "code_scan_warning" {
			t.Errorf("did not expect code_scan_warning on block, got: %+v", ev)
		}
	}
}

func TestScannedStrategy_BlockOnNewFile_ZerosFile(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	scanner := codescanner.NewPatternScanner()
	strat := NewScannedStrategy(NewWholeFileStrategy(), scanner, &types.CodeScannerConfig{Type: "patterns"}, &recordingEmitter{})

	input := json.RawMessage(`{
		"path": "secrets.txt",
		"content": "ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"
	}`)

	result, err := strat.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Errorf("expected Applied=false, got true")
	}

	// File must end up empty (the executor has no delete primitive).
	got, err := exec.ReadFile(context.Background(), "secrets.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty file after rollback, got: %q", got)
	}
}

func TestScannedStrategy_WarnOnEvalSink_AppliesAndEmits(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true on warn-only finding, got error: %s", result.Error)
	}

	got, err := exec.ReadFile(context.Background(), "run.py")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != newContent {
		t.Errorf("warn must not roll back: file contains %q", got)
	}

	events := emitter.snapshot()
	var found bool
	for _, ev := range events {
		if ev.event == "code_scan_warning" {
			found = true
			if ev.level != "warn" {
				t.Errorf("expected warn level, got %q", ev.level)
			}
			if ev.data["rule"] != "sink/python_eval" {
				t.Errorf("expected rule sink/python_eval, got %v", ev.data["rule"])
			}
		}
	}
	if !found {
		t.Errorf("expected at least one code_scan_warning event, got: %+v", events)
	}
}

func TestScannedStrategy_BlockOnWarn_PromotesAndRollsBack(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Errorf("BlockOnWarn must promote warn to block")
	}
	got, _ := exec.ReadFile(context.Background(), "run.py")
	if got != "x = 1\n" {
		t.Errorf("rollback failed under BlockOnWarn: %q", got)
	}
}

func TestScannedStrategy_CleanContent_NoEvents(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true: %s", result.Error)
	}
	if len(emitter.snapshot()) != 0 {
		t.Errorf("clean edit must not emit events: %+v", emitter.snapshot())
	}
}

// TestScannedStrategy_InnerApplyError covers S5: an error from the
// inner strategy must propagate verbatim and the scanner must not
// run. A scanner that fires after a failed inner.Apply would scan
// stale (or nonexistent) content and either crash or report
// nonsense findings.
func TestScannedStrategy_InnerApplyError(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	innerErr := errors.New("inner failed")
	inner := &fakeEditStrategy{applyErr: innerErr}
	scanner := &fakeScanner{}
	strat := NewScannedStrategy(inner, scanner, &types.CodeScannerConfig{Type: "patterns"}, nil)

	_, err := strat.Apply(context.Background(), json.RawMessage(`{"path":"x.go"}`), exec)
	if !errors.Is(err, innerErr) {
		t.Fatalf("expected inner error to propagate, got: %v", err)
	}
	if scanner.calls != 0 {
		t.Errorf("scanner must not run after inner failure, got %d calls", scanner.calls)
	}
}

// TestScannedStrategy_ScannerError covers S5: a hard error from
// the scanner must surface as a hard error, not as a silent passthrough
// of the inner result. This is the path that fires when semgrep
// cannot reach its rule registry, when the patterns scanner panics
// on a malformed input, etc.
func TestScannedStrategy_ScannerError(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected scanner error to propagate, got nil")
	}
	if !errors.Is(err, scanErr) {
		t.Errorf("expected wrapped %v, got %v", scanErr, err)
	}
}

// TestScannedStrategy_PostApplyReadError covers S5: if the executor
// can no longer read the file just-written by the inner strategy,
// the wrapper must fail closed rather than skip the scan. A silent
// passthrough here would mean the operator's "every edit is scanned"
// guarantee depends on the underlying file system never glitching.
func TestScannedStrategy_PostApplyReadError(t *testing.T) {
	inner := &fakeEditStrategy{
		result: &EditResult{Path: "missing.go", Applied: true},
	}
	scanner := &fakeScanner{}
	exec := &readFailExec{readErr: errors.New("read flaked")}
	strat := NewScannedStrategy(inner, scanner, &types.CodeScannerConfig{Type: "patterns"}, nil)

	_, err := strat.Apply(context.Background(), json.RawMessage(`{"path":"missing.go"}`), exec)
	if err == nil {
		t.Fatal("expected read-back error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("expected error to mention the read failure, got %v", err)
	}
	if scanner.calls != 0 {
		t.Errorf("scanner must not run when post-apply read fails, got %d calls", scanner.calls)
	}
}
