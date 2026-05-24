package builtins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// These tests cover issue #231 B1: every structured built-in must populate a
// correct, typed structured payload AND leave the text fallback byte-identical
// to the pre-#231 rendering.

func TestRunCommandTool_StructuredAndText(t *testing.T) {
	mock := &mockExecutor{
		execFunc: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 3, Stdout: "out-data", Stderr: "err-data"}, nil
		},
	}
	runTool := RunCommandTool(mock)
	if runTool.StructuredHandler == nil {
		t.Fatal("run_command must expose a StructuredHandler")
	}

	input, _ := json.Marshal(map[string]any{"command": "go test ./...", "timeout": 120})
	text, structured, err := runTool.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text fallback must match the legacy concatenation exactly.
	wantText := "out-data\nSTDERR:\nerr-data\n[exit code: 3]"
	if text != wantText {
		t.Errorf("text mismatch\n got: %q\nwant: %q", text, wantText)
	}

	var got commandResult
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured payload is not a commandResult: %v\nraw: %s", err, structured)
	}
	want := commandResult{Stdout: "out-data", Stderr: "err-data", ExitCode: 3, TimedOut: false, TimeoutSeconds: 120}
	if got != want {
		t.Errorf("structured mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestReadFileTool_StructuredAndText(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "one\ntwo\nthree\nfour\nfive\n", nil
		},
	}
	readTool := ReadFileTool(mock)
	if readTool.StructuredHandler == nil {
		t.Fatal("read_file must expose a StructuredHandler")
	}

	input, _ := json.Marshal(map[string]any{"path": "f.go", "start_line": 2, "limit": 2})
	text, structured, err := readTool.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text fallback must remain the line-numbered rendering.
	if want := "2\ttwo\n3\tthree"; text != want {
		t.Errorf("text mismatch\n got: %q\nwant: %q", text, want)
	}

	var got fileExcerpt
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured payload is not a fileExcerpt: %v\nraw: %s", err, structured)
	}
	if got.Path != "f.go" || got.StartLine != 2 || got.EndLine != 3 {
		t.Errorf("window mismatch: %+v", got)
	}
	if !got.Truncated {
		t.Errorf("expected truncated=true: a 5-line file read with a 2-line window stops short of EOF")
	}
	if want := []string{"two", "three"}; !equalStrings(got.Lines, want) {
		t.Errorf("lines mismatch\n got: %v\nwant: %v", got.Lines, want)
	}
}

func TestReadFileTool_StructuredPastEOF(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "only one line\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "f.txt", "start_line": 500})
	_, structured, err := readTool.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("expected non-error for past-EOF, got %v", err)
	}
	var got fileExcerpt
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured payload is not a fileExcerpt: %v", err)
	}
	if !got.PastEOF || len(got.Lines) != 0 {
		t.Errorf("expected past_eof with empty lines, got: %+v", got)
	}
}

func TestGrepFilesTool_StructuredMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha needle\nplain\nneedle again\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})
	if grep.StructuredHandler == nil {
		t.Fatal("grep_files must expose a StructuredHandler")
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	text, structured, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got searchResult
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v\nraw: %s", err, structured)
	}
	if len(got.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d (%+v)", len(got.Matches), got.Matches)
	}
	// Structured matches must agree with the text rendering line for line.
	for _, m := range got.Matches {
		if m.Path != "a.go" {
			t.Errorf("unexpected path %q", m.Path)
		}
		if m.Text == "" || m.Line == 0 {
			t.Errorf("match missing line/text: %+v", m)
		}
	}
	if got.Matches[0].Line != 1 || got.Matches[0].Text != "alpha needle" {
		t.Errorf("first match wrong: %+v", got.Matches[0])
	}
	if got.Matches[1].Line != 3 || got.Matches[1].Text != "needle again" {
		t.Errorf("second match wrong: %+v", got.Matches[1])
	}
	if got.Truncated {
		t.Errorf("did not expect truncation for 2 matches under the default cap")
	}
	// The text rendering is the canonical "path:line:match" form.
	if want := "a.go:1:alpha needle\na.go:3:needle again"; text != want {
		t.Errorf("text mismatch\n got: %q\nwant: %q", text, want)
	}
}

func TestGrepFilesTool_StructuredNoMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "absent_pattern"})
	text, structured, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != noMatchesText {
		t.Errorf("expected no-matches sentinel, got %q", text)
	}
	var got searchResult
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v", err)
	}
	if got.Matches == nil {
		t.Error("matches must be an empty array, not null")
	}
	if len(got.Matches) != 0 {
		t.Errorf("expected zero matches, got %+v", got.Matches)
	}
}

func TestGrepFilesTool_StructuredTruncated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("hit one\nhit two\nhit three\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "hit", "max_results": 2})
	_, structured, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got searchResult
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v", err)
	}
	if len(got.Matches) != 2 || !got.Truncated {
		t.Errorf("expected 2 matches with truncated=true, got %d matches truncated=%v", len(got.Matches), got.Truncated)
	}
}

func TestFindFilesTool_StructuredPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.go", "two.go", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	find := FindFilesTool(&fsExecutor{root: dir})
	if find.StructuredHandler == nil {
		t.Fatal("find_files must expose a StructuredHandler")
	}

	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	text, structured, err := find.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got findResult
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured payload is not a findResult: %v\nraw: %s", err, structured)
	}
	if len(got.Paths) != 2 {
		t.Errorf("expected 2 paths, got %v", got.Paths)
	}
	// The structured paths must be exactly the lines of the text rendering.
	if want := parseFindPaths(text); !equalStrings(got.Paths, want) {
		t.Errorf("structured paths diverge from text\n got: %v\ntext: %v", got.Paths, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
