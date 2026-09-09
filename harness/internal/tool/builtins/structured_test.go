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

// These tests assert every structured built-in populates a correct, typed
// structured payload with the right Kind discriminator, while leaving the
// text fallback rendering unchanged.

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

// TestRunCommandTool_StructuredDefaultTimeout: omitting timeout must default
// TimeoutSeconds to 30.
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

// TestGrepFilesTool_ColonInPathAndText: a path AND a matched line that both
// contain colons must round-trip into searchMatch fields exactly, with the
// rendered text still byte-identical.
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

// TestFindFilesTool_StructuredTruncated mirrors the grep truncation test for
// find_files.
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
		`{"type":"match","data":{"path":{"text":"/ws/a:b/c.go"},"lines":{"text":"key: needle: value\n"},"line_number":7,` +
			`"submatches":[{"match":{"text":"needle"},"start":5,"end":11}]}}`,
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
	want := searchMatch{Path: "/ws/a:b/c.go", Line: 7, Column: 6, Text: "key: needle: value"}
	if len(got.Matches) != 1 || got.Matches[0] != want {
		t.Fatalf("rg --json match wrong: %+v", got.Matches)
	}
	if wantText := "/ws/a:b/c.go:7:key: needle: value"; res.Text != wantText {
		t.Errorf("rg text mismatch\n got: %q\nwant: %q", res.Text, wantText)
	}
}

// TestGrepFilesTool_RipgrepJSONColumnIsByteOffset pins searchMatch.Column to a
// 1-indexed *byte* column. The matched line carries a multi-byte "é" before the
// match, so a rune-indexed implementation would report a smaller column.
func TestGrepFilesTool_RipgrepJSONColumnIsByteOffset(t *testing.T) {
	withRipgrepProbe(t, true)
	// "héllo " is 7 bytes but 6 runes, so rg's 0-based byte start is 7 and the
	// 1-indexed byte column is 8; a rune column would be 7.
	rgJSON := strings.Join([]string{
		`{"type":"begin","data":{"path":{"text":"/ws/u.go"}}}`,
		`{"type":"match","data":{"path":{"text":"/ws/u.go"},"lines":{"text":"héllo needle here\n"},"line_number":3,` +
			`"submatches":[{"match":{"text":"needle"},"start":7,"end":13}]}}`,
		`{"type":"end","data":{"path":{"text":"/ws/u.go"}}}`,
	}, "\n")
	exec := &fsExecutor{
		root:    "/ws",
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 0, Stdout: rgJSON}, nil
		},
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := GrepFilesTool(exec).StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got searchResult
	if err := json.Unmarshal(res.Structured, &got); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v", err)
	}
	want := searchMatch{Path: "/ws/u.go", Line: 3, Column: 8, Text: "héllo needle here"}
	if len(got.Matches) != 1 || got.Matches[0] != want {
		t.Fatalf("rg --json match wrong\n got: %+v\nwant: %+v", got.Matches, want)
	}
	// The byte column must index into the raw bytes of Text, not its runes.
	if idx := got.Matches[0].Column - 1; !strings.HasPrefix(got.Matches[0].Text[idx:], "needle") {
		t.Errorf("column %d does not point at the match in %q", got.Matches[0].Column, got.Matches[0].Text)
	}
}

// TestGrepFilesTool_RipgrepJSONNoSubmatchesOmitsColumn asserts a match event
// carrying no submatches leaves "column" out of the payload entirely rather
// than emitting a meaningless 0 or 1.
func TestGrepFilesTool_RipgrepJSONNoSubmatchesOmitsColumn(t *testing.T) {
	withRipgrepProbe(t, true)
	rgJSON := `{"type":"match","data":{"path":{"text":"/ws/a.go"},"lines":{"text":"needle\n"},"line_number":1}}`
	exec := &fsExecutor{
		root:    "/ws",
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 0, Stdout: rgJSON}, nil
		},
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := GrepFilesTool(exec).StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNoColumnKey(t, res.Structured)
}

// TestGrepFilesTool_NativeWalkerOmitsColumn pins the Go-native walker's half of
// the searchMatch.Column contract: it has no match-offset tracking, so the
// field must be absent rather than defaulted.
func TestGrepFilesTool_NativeWalkerOmitsColumn(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nvar needle = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := GrepFilesTool(&fsExecutor{root: dir}).StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNoColumnKey(t, res.Structured)
}

// assertNoColumnKey fails unless every match in a marshalled searchResult
// leaves the "column" key out. Decoding into searchMatch cannot tell an absent
// key from a zero value, so this inspects the raw JSON.
func assertNoColumnKey(t *testing.T, structured json.RawMessage) {
	t.Helper()
	var raw struct {
		Matches []map[string]json.RawMessage `json:"matches"`
	}
	if err := json.Unmarshal(structured, &raw); err != nil {
		t.Fatalf("structured payload is not a searchResult: %v", err)
	}
	if len(raw.Matches) == 0 {
		t.Fatalf("expected at least one match, got payload %s", structured)
	}
	for i, m := range raw.Matches {
		if _, ok := m["column"]; ok {
			t.Errorf("match %d unexpectedly carries a column: %s", i, structured)
		}
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

// TestGrepFilesTool_RealRipgrepColumn drives the rg --json path against the
// real ripgrep binary rather than a hand-written fixture, so the byte-offset
// contract is pinned to rg's actual output rather than an assumption about it.
func TestGrepFilesTool_RealRipgrepColumn(t *testing.T) {
	if _, err := lookPath("rg"); err != nil {
		t.Skip("ripgrep not on PATH")
	}
	withRipgrepProbe(t, true)
	dir := t.TempDir()
	// "héllo " occupies 7 bytes but 6 runes.
	if err := os.WriteFile(filepath.Join(dir, "u.txt"), []byte("héllo needle here\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	got := decodeSearchResult(t, GrepFilesTool(exec), input)
	if len(got.Matches) != 1 {
		t.Fatalf("expected exactly one match, got %+v", got.Matches)
	}
	m := got.Matches[0]
	if m.Column != 8 {
		t.Errorf("expected 1-indexed byte column 8, got %d (7 would be a rune column)", m.Column)
	}
	if idx := m.Column - 1; idx < 0 || idx > len(m.Text) || !strings.HasPrefix(m.Text[idx:], "needle") {
		t.Errorf("column %d does not point at the match in %q", m.Column, m.Text)
	}
}

// TestGrepFilesTool_RipgrepJSONMultipleSubmatches pins Column to the leftmost
// span when a line matches several times. rg emits one match event per line
// carrying every span, ordered by position, so taking the first must not drift
// to the last or the widest.
func TestGrepFilesTool_RipgrepJSONMultipleSubmatches(t *testing.T) {
	withRipgrepProbe(t, true)
	// Offsets copied from real rg output for the line below.
	rgJSON := `{"type":"match","data":{"path":{"text":"/ws/m.txt"},"lines":{"text":"needle and needle\n"},"line_number":1,` +
		`"submatches":[{"match":{"text":"needle"},"start":0,"end":6},{"match":{"text":"needle"},"start":11,"end":17}]}}`
	exec := &fsExecutor{
		root:    "/ws",
		canExec: true,
		execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 0, Stdout: rgJSON}, nil
		},
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	got := decodeSearchResult(t, GrepFilesTool(exec), input)
	want := searchMatch{Path: "/ws/m.txt", Line: 1, Column: 1, Text: "needle and needle"}
	if len(got.Matches) != 1 || got.Matches[0] != want {
		t.Fatalf("expected one match at the leftmost span\n got: %+v\nwant: %+v", got.Matches, want)
	}
}

// TestGrepFilesTool_RipgrepJSONColumnBounds covers both edges of the offset
// guard: a zero-width match at end of line legitimately reports len(Text)+1,
// while an offset outside the line can only be malformed output and is dropped
// rather than emitted as a nonsense column.
func TestGrepFilesTool_RipgrepJSONColumnBounds(t *testing.T) {
	tests := []struct {
		name       string
		submatches string
		wantColumn int
	}{
		// Offsets copied from real rg output for pattern "$" on this line.
		{"zero width at end of line", `[{"match":{"text":""},"start":17,"end":17}]`, 18},
		{"offset past end of line", `[{"match":{"text":""},"start":99,"end":99}]`, 0},
		{"negative offset", `[{"match":{"text":""},"start":-1,"end":-1}]`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withRipgrepProbe(t, true)
			rgJSON := `{"type":"match","data":{"path":{"text":"/ws/m.txt"},"lines":{"text":"needle and needle\n"},` +
				`"line_number":1,"submatches":` + tc.submatches + `}}`
			exec := &fsExecutor{
				root:    "/ws",
				canExec: true,
				execFn: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
					return &executor.ExecResult{ExitCode: 0, Stdout: rgJSON}, nil
				},
			}

			input, _ := json.Marshal(map[string]any{"pattern": "needle"})
			got := decodeSearchResult(t, GrepFilesTool(exec), input)
			if len(got.Matches) != 1 {
				t.Fatalf("expected one match, got %+v", got.Matches)
			}
			if got.Matches[0].Column != tc.wantColumn {
				t.Errorf("expected column %d, got %d", tc.wantColumn, got.Matches[0].Column)
			}
		})
	}
}
