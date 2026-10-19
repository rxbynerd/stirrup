package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestCapLine_BoundsLineLength(t *testing.T) {
	t.Run("passes a line at the cap through unchanged", func(t *testing.T) {
		short := strings.Repeat("a", maxLineBytes)
		require.Equal(t, short, capLine(short))
	})

	t.Run("caps a long multibyte line on a rune boundary", func(t *testing.T) {
		long := strings.Repeat("ü", maxLineBytes) // two bytes per rune
		got := capLine(long)
		require.LessOrEqual(t, len(got), maxLineBytes)
		require.True(t, utf8.ValidString(got), "capped line split a UTF-8 rune")
		marker := fmt.Sprintf(" of %d bytes shown]", len(long))
		require.Truef(t, strings.HasSuffix(got, marker), "marker must name the original length: %q", got[len(got)-60:])
	})

	t.Run("re-capping a capped line stays within the cap", func(t *testing.T) {
		got := capLine(strings.Repeat("ü", maxLineBytes))
		require.LessOrEqual(t, len(capLine(got)), maxLineBytes)
	})
}

func TestReadFileTool_CapsSingleLongLine(t *testing.T) {
	dir := t.TempDir()
	const size = 3 << 20
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bundle.min.js"), []byte(strings.Repeat("x", size)), 0o644))
	exec, err := executor.NewLocalExecutor(dir)
	require.NoError(t, err)

	res, err := ReadFileTool(exec).StructuredHandler(context.Background(), json.RawMessage(`{"path":"bundle.min.js"}`))
	require.NoError(t, err)
	require.Truef(t, strings.HasPrefix(res.Text, "1\t"), "expected a line-numbered rendering, got %.40q", res.Text)
	line := strings.TrimPrefix(res.Text, "1\t")
	require.LessOrEqual(t, len(line), maxLineBytes)

	marker := fmt.Sprintf("of %d bytes shown]", size)
	require.Truef(t, strings.HasSuffix(res.Text, marker), "text must end with a marker naming the original size: %q", res.Text[len(res.Text)-60:])

	var excerpt fileExcerpt
	require.NoError(t, json.Unmarshal(res.Structured, &excerpt))
	require.Len(t, excerpt.Lines, 1, "structured lines must carry the same capped line")
	require.LessOrEqual(t, len(excerpt.Lines[0]), maxLineBytes)
	require.True(t, strings.HasSuffix(excerpt.Lines[0], marker), "structured line must carry the same marker")
}

func TestReadFileTool_ShortLinesUnchanged(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644))
	exec, err := executor.NewLocalExecutor(dir)
	require.NoError(t, err)

	out, err := invokeText(context.Background(), ReadFileTool(exec), json.RawMessage(`{"path":"a.txt"}`))
	require.NoError(t, err)
	require.Equal(t, "1\tone\n2\ttwo", out)
}

// hostRipgrepExecutor is a HostPathWorkspace executor whose Exec answers
// the host ripgrep invocation with canned rg --json output.
type hostRipgrepExecutor struct {
	hostAndExecExecutor
	rgStdout string
}

func (e *hostRipgrepExecutor) Exec(context.Context, string, time.Duration) (*executor.ExecResult, error) {
	return &executor.ExecResult{ExitCode: 0, Stdout: e.rgStdout}, nil
}

func rgMatchJSON(path, line string) string {
	ev := map[string]any{
		"type": "match",
		"data": map[string]any{
			"path":        map[string]string{"text": path},
			"lines":       map[string]string{"text": line + "\n"},
			"line_number": 1,
		},
	}
	raw, _ := json.Marshal(ev)
	return string(raw) + "\n"
}

func TestGrepFilesTool_CapsLongMatchedLinesOnEverySearchPath(t *testing.T) {
	longLine := "needle" + strings.Repeat("y", 512<<10)
	shortLine := "needle short"

	tests := []struct {
		name  string
		setup func(t *testing.T) executor.Executor
	}{
		{
			name: "host native walker",
			setup: func(t *testing.T) executor.Executor {
				withRipgrepProbe(t, false)
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte(longLine+"\n"+shortLine+"\n"), 0o644))
				return &hostAndExecExecutor{root: dir}
			},
		},
		{
			name: "host ripgrep",
			setup: func(t *testing.T) executor.Executor {
				withRipgrepProbe(t, true)
				return &hostRipgrepExecutor{
					hostAndExecExecutor: hostAndExecExecutor{root: t.TempDir()},
					rgStdout:            rgMatchJSON("a.txt", longLine) + rgMatchJSON("a.txt", shortLine),
				}
			},
		},
		{
			name: "sandbox ripgrep",
			setup: func(*testing.T) executor.Executor {
				return &sandboxExecExecutor{
					resolvedDir: "/workspace",
					execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
						if strings.HasPrefix(command, "rg --version") {
							return &executor.ExecResult{ExitCode: 0}, nil
						}
						return &executor.ExecResult{ExitCode: 0, Stdout: rgMatchJSON("a.txt", longLine) + rgMatchJSON("a.txt", shortLine)}, nil
					},
				}
			},
		},
		{
			name: "sandbox shell grep",
			setup: func(*testing.T) executor.Executor {
				return &sandboxExecExecutor{
					resolvedDir: "/workspace",
					execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
						if strings.HasPrefix(command, "rg --version") {
							return &executor.ExecResult{ExitCode: 127}, nil
						}
						return &executor.ExecResult{ExitCode: 0, Stdout: "/workspace/a.txt:1:" + longLine + "\n/workspace/a.txt:2:" + shortLine + "\n"}, nil
					},
				}
			},
		},
		{
			name: "tree lister",
			setup: func(*testing.T) executor.Executor {
				return newTreeExecutor(map[string]string{"a.txt": longLine + "\n" + shortLine + "\n"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runSearchTool(t, GrepFilesTool(tt.setup(t)), map[string]any{"pattern": "needle"})

			var result searchResult
			require.NoError(t, json.Unmarshal(res.Structured, &result))
			require.Len(t, result.Matches, 2)

			long, short := result.Matches[0].Text, result.Matches[1].Text
			require.LessOrEqual(t, len(long), maxLineBytes)
			require.True(t, strings.HasPrefix(long, "needle"), "shortened line must keep its head")
			marker := fmt.Sprintf("of %d bytes shown]", len(longLine))
			require.Truef(t, strings.HasSuffix(long, marker), "shortened line must carry the marker: %q", long[len(long)-60:])
			require.Equal(t, shortLine, short)

			for _, line := range strings.Split(res.Text, "\n") {
				require.LessOrEqual(t, len(line), maxLineBytes+len("a.txt:1:"), "rendered match line is too long")
			}
		})
	}
}

var _ executor.HostPathWorkspace = (*hostRipgrepExecutor)(nil)
