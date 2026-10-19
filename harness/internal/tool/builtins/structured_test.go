package builtins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NotNil(t, runTool.StructuredHandler, "run_command must expose a StructuredHandler")

	input, _ := json.Marshal(map[string]any{"command": "go test ./...", "timeout": 120})
	res, err := runTool.StructuredHandler(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, "command_result", res.Kind)

	// Text fallback must match the legacy concatenation exactly.
	require.Equal(t, "out-data\nSTDERR:\nerr-data\n[exit code: 3]", res.Text)

	got := decodeStructured[commandResult](t, res.Structured)
	want := commandResult{Stdout: "out-data", Stderr: "err-data", ExitCode: 3, TimedOut: false, TimeoutSeconds: 120}
	require.Equal(t, want, got)
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
	require.NoError(t, err)

	got := decodeStructured[commandResult](t, res.Structured)
	require.Equal(t, 30, got.TimeoutSeconds, "omitted timeout must default to 30 seconds")
}

func TestReadFileTool_StructuredAndText(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "one\ntwo\nthree\nfour\nfive\n", nil
		},
	}
	readTool := ReadFileTool(mock)
	require.NotNil(t, readTool.StructuredHandler, "read_file must expose a StructuredHandler")

	input, _ := json.Marshal(map[string]any{"path": "f.go", "start_line": 2, "limit": 2})
	res, err := readTool.StructuredHandler(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, "file_excerpt", res.Kind)

	// Text fallback must remain the line-numbered rendering.
	require.Equal(t, "2\ttwo\n3\tthree", res.Text)

	got := decodeStructured[fileExcerpt](t, res.Structured)
	require.Equal(t, "f.go", got.Path)
	require.Equal(t, 2, got.StartLine)
	require.Equal(t, 3, got.EndLine)
	require.True(t, got.Truncated, "a 5-line file read with a 2-line window stops short of EOF")
	require.Equal(t, []string{"two", "three"}, got.Lines)
}

func TestReadFileTool_PastEOFReturnsNoLines(t *testing.T) {
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "only one line\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "f.txt", "start_line": 500})
	res, err := readTool.StructuredHandler(context.Background(), input)
	require.NoError(t, err, "a read past EOF is not an error")

	got := decodeStructured[fileExcerpt](t, res.Structured)
	require.True(t, got.PastEOF, "a read past EOF must set past_eof")
	require.Empty(t, got.Lines)
}

func TestGrepFilesTool_StructuredMatches(t *testing.T) {
	withRipgrepProbe(t, false) // force the Go-native walker for determinism
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha needle\nplain\nneedle again\n"), 0o644), "write fixture")
	grep := GrepFilesTool(&fsExecutor{root: dir})
	require.NotNil(t, grep.StructuredHandler, "grep_files must expose a StructuredHandler")

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := grep.StructuredHandler(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, "search_result", res.Kind)

	got := decodeStructured[searchResult](t, res.Structured)
	require.Len(t, got.Matches, 2, "matches: %+v", got.Matches)
	require.Equal(t, searchMatch{Path: "a.go", Line: 1, Text: "alpha needle"}, got.Matches[0])
	require.Equal(t, searchMatch{Path: "a.go", Line: 3, Text: "needle again"}, got.Matches[1])
	require.False(t, got.Truncated, "2 matches are under the default cap")
	// The text rendering is the canonical "path:line:match" form.
	require.Equal(t, "a.go:1:alpha needle\na.go:3:needle again", res.Text)
}

// TestGrepFilesTool_ColonInPathAndText: a path AND a matched line that both
// contain colons must round-trip into searchMatch fields exactly, with the
// rendered text still byte-identical.
func TestGrepFilesTool_ColonInPathAndText(t *testing.T) {
	withRipgrepProbe(t, false) // exercise the native walker explicitly
	dir := t.TempDir()
	// A subdirectory whose name contains a colon (legal on Linux/macOS).
	sub := filepath.Join(dir, "a:b")
	require.NoError(t, os.MkdirAll(sub, 0o755), "mkdir colon dir")
	// The matched line also contains colons.
	require.NoError(t, os.WriteFile(filepath.Join(sub, "c.go"), []byte("key: needle: value\n"), 0o644), "write fixture")
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := grep.StructuredHandler(context.Background(), input)
	require.NoError(t, err)

	got := decodeStructured[searchResult](t, res.Structured)
	require.Len(t, got.Matches, 1, "colon-bearing match was dropped: %+v", got.Matches)
	want := searchMatch{Path: filepath.Join("a:b", "c.go"), Line: 1, Text: "key: needle: value"}
	require.Equal(t, want, got.Matches[0])
	// Text rendering must still be the exact "path:line:text" form.
	require.Equal(t, want.Path+":1:key: needle: value", res.Text)
}

func TestGrepFilesTool_StructuredNoMatches(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("nothing here\n"), 0o644), "write fixture")
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "absent_pattern"})
	res, err := grep.StructuredHandler(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, noMatchesText, res.Text)

	got := decodeStructured[searchResult](t, res.Structured)
	require.NotNil(t, got.Matches, "matches must be an empty array, not null")
	require.Empty(t, got.Matches)
}

func TestGrepFilesTool_StructuredTruncated(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("hit one\nhit two\nhit three\n"), 0o644), "write fixture")
	grep := GrepFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"pattern": "hit", "max_results": 2})
	res, err := grep.StructuredHandler(context.Background(), input)
	require.NoError(t, err)

	got := decodeStructured[searchResult](t, res.Structured)
	require.Len(t, got.Matches, 2)
	require.True(t, got.Truncated, "max_results cap must set truncated")
}

func TestFindFilesTool_StructuredPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.go", "two.go", "skip.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644), "write fixture")
	}
	find := FindFilesTool(&fsExecutor{root: dir})
	require.NotNil(t, find.StructuredHandler, "find_files must expose a StructuredHandler")

	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	res, err := find.StructuredHandler(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, "find_result", res.Kind)

	got := decodeStructured[findResult](t, res.Structured)
	require.Len(t, got.Paths, 2)
	// The structured paths must be exactly the lines of the text rendering.
	var textLines []string
	if res.Text != noMatchesText {
		textLines = strings.Split(res.Text, "\n")
	}
	require.Equal(t, textLines, got.Paths, "structured paths diverge from text")
}

// TestFindFilesTool_StructuredTruncated mirrors the grep truncation test for
// find_files.
func TestFindFilesTool_StructuredTruncated(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.go", "two.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644), "write fixture")
	}
	find := FindFilesTool(&fsExecutor{root: dir})

	input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": 1})
	res, err := find.StructuredHandler(context.Background(), input)
	require.NoError(t, err)

	got := decodeStructured[findResult](t, res.Structured)
	require.Len(t, got.Paths, 1)
	require.True(t, got.Truncated, "max_results cap must set truncated")
}

// TestGrepFilesTool_RipgrepJSONBuildsMatches exercises the rg --json path via a
// stubbed executor. Matches must be built from the JSON rather than re-parsed
// from text, a colon in path or text must survive, and the rendered text must
// be byte-identical to rg's "path:line:text" output.
func TestGrepFilesTool_RipgrepJSONBuildsMatches(t *testing.T) {
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
	require.NoError(t, err)

	got := decodeStructured[searchResult](t, res.Structured)
	want := searchMatch{Path: "/ws/a:b/c.go", Line: 7, Column: 6, Text: "key: needle: value"}
	require.Equal(t, []searchMatch{want}, got.Matches)
	require.Equal(t, "/ws/a:b/c.go:7:key: needle: value", res.Text, "rg text must match rg's path:line:text output")
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
	require.NoError(t, err)

	got := decodeStructured[searchResult](t, res.Structured)
	want := searchMatch{Path: "/ws/u.go", Line: 3, Column: 8, Text: "héllo needle here"}
	require.Equal(t, []searchMatch{want}, got.Matches)
	// The byte column must index into the raw bytes of Text, not its runes.
	idx := got.Matches[0].Column - 1
	require.True(t, strings.HasPrefix(got.Matches[0].Text[idx:], "needle"),
		"column %d does not point at the match in %q", got.Matches[0].Column, got.Matches[0].Text)
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
	require.NoError(t, err)
	assertNoColumnKey(t, res.Structured)
}

// TestGrepFilesTool_NativeWalkerOmitsColumn pins the Go-native walker's half of
// the searchMatch.Column contract: it has no match-offset tracking, so the
// field must be absent rather than defaulted.
func TestGrepFilesTool_NativeWalkerOmitsColumn(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nvar needle = 1\n"), 0o644), "write fixture")

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	res, err := GrepFilesTool(&fsExecutor{root: dir}).StructuredHandler(context.Background(), input)
	require.NoError(t, err)
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
	require.NoError(t, json.Unmarshal(structured, &raw), "structured payload is not a searchResult")
	require.NotEmpty(t, raw.Matches, "expected at least one match, got payload %s", structured)
	for i, m := range raw.Matches {
		assert.NotContains(t, m, "column", "match %d unexpectedly carries a column: %s", i, structured)
	}
}

// decodeStructured unmarshals a structured payload into T and fails the test
// with the raw payload when it does not decode.
func decodeStructured[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(raw, &v), "structured payload: %s", raw)
	return v
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "u.txt"), []byte("héllo needle here\n"), 0o644), "write fixture")
	exec, err := executor.NewLocalExecutor(dir)
	require.NoError(t, err, "NewLocalExecutor")

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	got := decodeSearchResult(t, GrepFilesTool(exec), input)
	require.Len(t, got.Matches, 1, "expected exactly one match")
	m := got.Matches[0]
	require.Equal(t, 8, m.Column, "expected 1-indexed byte column 8 (7 would be a rune column)")
	idx := m.Column - 1
	require.True(t, idx >= 0 && idx <= len(m.Text) && strings.HasPrefix(m.Text[idx:], "needle"),
		"column %d does not point at the match in %q", m.Column, m.Text)
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
	require.Equal(t, []searchMatch{want}, got.Matches, "expected one match at the leftmost span")
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
			require.Len(t, got.Matches, 1)
			require.Equal(t, tc.wantColumn, got.Matches[0].Column)
		})
	}
}
