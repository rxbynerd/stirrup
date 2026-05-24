package builtins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// These tests cover issue #231 B1: every structured built-in must populate a
// correct, typed structured payload with the right Kind discriminator AND leave
// the text fallback byte-identical to the pre-#231 rendering.

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
	res, err := runTool.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Kind != "command_result" {
		t.Errorf("expected kind command_result, got %q", res.Kind)
	}

	// Text fallback must match the legacy concatenation exactly.
	wantText := "out-data\nSTDERR:\nerr-data\n[exit code: 3]"
	if res.Text != wantText {
		t.Errorf("text mismatch\n got: %q\nwant: %q", res.Text, wantText)
	}

	var got commandResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a commandResult: %v\nraw: %s", err, res.Structured)
	}
	want := commandResult{Stdout: "out-data", Stderr: "err-data", ExitCode: 3, TimedOut: false, TimeoutSeconds: 120}
	if got != want {
		t.Errorf("structured mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestRunCommandTool_StructuredDefaultTimeout exercises the params.Timeout==nil
// branch (R2): omitting timeout must default TimeoutSeconds to 30.
func TestRunCommandTool_StructuredDefaultTimeout(t *testing.T) {
	mock := &mockExecutor{
		execFunc: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
		},
	}
	runTool := RunCommandTool(mock)

	input, _ := json.Marshal(map[string]any{"command": "ls"})
	res, err := runTool.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got commandResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a commandResult: %v", err)
	}
	if got.TimeoutSeconds != 30 {
		t.Errorf("expected default TimeoutSeconds 30, got %d", got.TimeoutSeconds)
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
	res, err := readTool.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Kind != "file_excerpt" {
		t.Errorf("expected kind file_excerpt, got %q", res.Kind)
	}

	// Text fallback must remain the line-numbered rendering.
	if want := "2\ttwo\n3\tthree"; res.Text != want {
		t.Errorf("text mismatch\n got: %q\nwant: %q", res.Text, want)
	}

	var got fileExcerpt
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a fileExcerpt: %v\nraw: %s", err, res.Structured)
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
	res, err := readTool.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("expected non-error for past-EOF, got %v", err)
	}
	var got fileExcerpt
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a fileExcerpt: %v", err)
	}
	if !got.PastEOF || len(got.Lines) != 0 {
		t.Errorf("expected past_eof with empty lines, got: %+v", got)
	}
}

func TestGrepFilesTool_StructuredMatches(t *testing.T) {
	withRipgrepProbe(t, false) // force the Go-native walker for determinism
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha needle\nplain\nneedle again\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})
	if grep.StructuredHandler == nil {
		t.Fatal("grep_files must expose a StructuredHandler")
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Kind != "search_result" {
		t.Errorf("expected kind search_result, got %q", res.Kind)
	}

	var got searchResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v\nraw: %s", err, res.Structured)
	}
	if len(got.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d (%+v)", len(got.Matches), got.Matches)
	}
	if got.Matches[0] != (searchMatch{Path: "a.go", Line: 1, Text: "alpha needle"}) {
		t.Errorf("first match wrong: %+v", got.Matches[0])
	}
	if got.Matches[1] != (searchMatch{Path: "a.go", Line: 3, Text: "needle again"}) {
		t.Errorf("second match wrong: %+v", got.Matches[1])
	}
	if got.Truncated {
		t.Errorf("did not expect truncation for 2 matches under the default cap")
	}
	// The text rendering is the canonical "path:line:match" form.
	if want := "a.go:1:alpha needle\na.go:3:needle again"; res.Text != want {
		t.Errorf("text mismatch\n got: %q\nwant: %q", res.Text, want)
	}
}

// TestGrepFilesTool_ColonInPathAndText is the [B1] regression: a path AND a
// matched line that both contain colons must round-trip into searchMatch
// fields exactly, with the rendered text still byte-identical. The old
// re-parse-the-text approach silently dropped or corrupted such matches.
func TestGrepFilesTool_ColonInPathAndText(t *testing.T) {
	withRipgrepProbe(t, false) // exercise the native walker explicitly
	dir := t.TempDir()
	// A subdirectory whose name contains a colon (legal on Linux/macOS).
	sub := filepath.Join(dir, "a:b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir colon dir: %v", err)
	}
	// The matched line also contains colons.
	if err := os.WriteFile(filepath.Join(sub, "c.go"), []byte("key: needle: value\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got searchResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v", err)
	}
	if len(got.Matches) != 1 {
		t.Fatalf("colon-bearing match was dropped: got %d matches (%+v)", len(got.Matches), got.Matches)
	}
	want := searchMatch{Path: filepath.Join("a:b", "c.go"), Line: 1, Text: "key: needle: value"}
	if got.Matches[0] != want {
		t.Errorf("colon match corrupted\n got: %+v\nwant: %+v", got.Matches[0], want)
	}
	// Text rendering must still be the exact "path:line:text" form.
	if wantText := want.Path + ":1:key: needle: value"; res.Text != wantText {
		t.Errorf("text mismatch\n got: %q\nwant: %q", res.Text, wantText)
	}
}

func TestGrepFilesTool_StructuredNoMatches(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "absent_pattern"})
	res, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != noMatchesText {
		t.Errorf("expected no-matches sentinel, got %q", res.Text)
	}
	var got searchResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
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
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("hit one\nhit two\nhit three\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "hit", "max_results": 2})
	res, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got searchResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
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
	res, err := find.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Kind != "find_result" {
		t.Errorf("expected kind find_result, got %q", res.Kind)
	}
	var got findResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a findResult: %v\nraw: %s", err, res.Structured)
	}
	if len(got.Paths) != 2 {
		t.Errorf("expected 2 paths, got %v", got.Paths)
	}
	// The structured paths must be exactly the lines of the text rendering.
	var textLines []string
	if res.Text != noMatchesText {
		textLines = strings.Split(res.Text, "\n")
	}
	if !equalStrings(got.Paths, textLines) {
		t.Errorf("structured paths diverge from text\n got: %v\ntext: %v", got.Paths, textLines)
	}
}

// TestFindFilesTool_StructuredTruncated (R1): mirror the grep truncation test
// for find_files.
func TestFindFilesTool_StructuredTruncated(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.go", "two.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	find := FindFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": 1})
	res, err := find.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got findResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a findResult: %v", err)
	}
	if len(got.Paths) != 1 || !got.Truncated {
		t.Errorf("expected 1 path with truncated=true, got %d paths truncated=%v", len(got.Paths), got.Truncated)
	}
}

// TestGrepFilesTool_RipgrepJSONPath exercises the rg --json path explicitly via
// a stubbed executor, asserting matches are built from JSON (not re-parsed
// text) and that a colon in path/text survives, and that the rendered text is
// byte-identical to rg's historical "path:line:text" output.
func TestGrepFilesTool_RipgrepJSONPath(t *testing.T) {
	withRipgrepProbe(t, true)
	rgJSON := strings.Join([]string{
		`{"type":"begin","data":{"path":{"text":"/ws/a:b/c.go"}}}`,
		`{"type":"match","data":{"path":{"text":"/ws/a:b/c.go"},"lines":{"text":"key: needle: value\n"},"line_number":7}}`,
		`{"type":"end","data":{"path":{"text":"/ws/a:b/c.go"}}}`,
		`{"type":"summary","data":{}}`,
	}, "\n")
	exec := &fsExecutor{
		root:    "/ws",
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 0, Stdout: rgJSON}, nil
		},
	}
	grep := GrepFilesTool(exec)

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := grep.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got searchResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v", err)
	}
	want := searchMatch{Path: "/ws/a:b/c.go", Line: 7, Text: "key: needle: value"}
	if len(got.Matches) != 1 || got.Matches[0] != want {
		t.Fatalf("rg --json match wrong: %+v", got.Matches)
	}
	if wantText := "/ws/a:b/c.go:7:key: needle: value"; res.Text != wantText {
		t.Errorf("rg text mismatch\n got: %q\nwant: %q", res.Text, wantText)
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
