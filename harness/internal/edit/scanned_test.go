package edit

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/security/codescanner"
	"github.com/rxbynerd/stirrup/types"
)

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
