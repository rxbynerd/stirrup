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
		must := require.New(t)
		short := strings.Repeat("a", maxLineBytes)
		must.Equal(short, capLine(short))
	})

	t.Run("caps a long multibyte line on a rune boundary", func(t *testing.T) {
		must := require.New(t)
		long := strings.Repeat("ü", maxLineBytes) // two bytes per rune
		got := capLine(long)
		must.LessOrEqual(len(got), maxLineBytes)
		must.True(utf8.ValidString(got), "capped line split a UTF-8 rune")
		marker := fmt.Sprintf(" of %d bytes shown]", len(long))
		must.Truef(strings.HasSuffix(got, marker), "marker must name the original length: %q", got[len(got)-60:])
	})

	t.Run("re-capping a capped line stays within the cap", func(t *testing.T) {
		must := require.New(t)
		got := capLine(strings.Repeat("ü", maxLineBytes))
		must.LessOrEqual(len(capLine(got)), maxLineBytes)
	})
}

func TestReadFileTool_CapsSingleLongLine(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	const size = 3 << 20
	must.NoError(os.WriteFile(filepath.Join(dir, "bundle.min.js"), []byte(strings.Repeat("x", size)), 0o644))
	exec, err := executor.NewLocalExecutor(dir)
	must.NoError(err)

	res, err := ReadFileTool(exec).StructuredHandler(context.Background(), json.RawMessage(`{"path":"bundle.min.js"}`))
	must.NoError(err)
	must.Truef(strings.HasPrefix(res.Text, "1\t"), "expected a line-numbered rendering, got %.40q", res.Text)
	line := strings.TrimPrefix(res.Text, "1\t")
	must.LessOrEqual(len(line), maxLineBytes)

	marker := fmt.Sprintf("of %d bytes shown]", size)
	must.Truef(strings.HasSuffix(res.Text, marker), "text must end with a marker naming the original size: %q", res.Text[len(res.Text)-60:])

	var excerpt fileExcerpt
	must.NoError(json.Unmarshal(res.Structured, &excerpt))
	must.Len(excerpt.Lines, 1, "structured lines must carry the same capped line")
	must.LessOrEqual(len(excerpt.Lines[0]), maxLineBytes)
	must.True(strings.HasSuffix(excerpt.Lines[0], marker), "structured line must carry the same marker")
}

func TestReadFileTool_ShortLinesUnchanged(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644))
	exec, err := executor.NewLocalExecutor(dir)
	must.NoError(err)

	out, err := invokeText(context.Background(), ReadFileTool(exec), json.RawMessage(`{"path":"a.txt"}`))
	must.NoError(err)
	must.Equal("1\tone\n2\ttwo", out)
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
				must := require.New(t)
				withRipgrepProbe(t, false)
				dir := t.TempDir()
				must.NoError(os.WriteFile(filepath.Join(dir, "a.txt"), []byte(longLine+"\n"+shortLine+"\n"), 0o644))
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
			must := require.New(t)
			res := runSearchTool(t, GrepFilesTool(tt.setup(t)), map[string]any{"pattern": "needle"})

			var result searchResult
			must.NoError(json.Unmarshal(res.Structured, &result))
			must.Len(result.Matches, 2)

			long, short := result.Matches[0].Text, result.Matches[1].Text
			must.LessOrEqual(len(long), maxLineBytes)
			must.True(strings.HasPrefix(long, "needle"), "shortened line must keep its head")
			marker := fmt.Sprintf("of %d bytes shown]", len(longLine))
			must.Truef(strings.HasSuffix(long, marker), "shortened line must carry the marker: %q", long[len(long)-60:])
			must.Equal(shortLine, short)

			for _, line := range strings.Split(res.Text, "\n") {
				must.LessOrEqual(len(line), maxLineBytes+len("a.txt:1:"), "rendered match line is too long")
			}
		})
	}
}

var _ executor.HostPathWorkspace = (*hostRipgrepExecutor)(nil)
