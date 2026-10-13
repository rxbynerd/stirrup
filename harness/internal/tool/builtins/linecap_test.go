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

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestCapLine(t *testing.T) {
	short := strings.Repeat("a", maxLineBytes)
	if got := capLine(short); got != short {
		t.Fatal("a line at the cap must pass through unchanged")
	}

	long := strings.Repeat("ü", maxLineBytes) // two bytes per rune
	got := capLine(long)
	if len(got) > maxLineBytes {
		t.Fatalf("capped line is %d bytes, want <= %d", len(got), maxLineBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("capped line split a UTF-8 rune")
	}
	if !strings.HasSuffix(got, fmt.Sprintf(" of %d bytes shown]", len(long))) {
		t.Fatalf("marker must name the original length: %q", got[len(got)-60:])
	}
	if got := capLine(got); len(got) > maxLineBytes {
		t.Fatal("re-capping must stay within the cap")
	}
}

func TestReadFileTool_CapsSingleLongLine(t *testing.T) {
	dir := t.TempDir()
	const size = 3 << 20
	if err := os.WriteFile(filepath.Join(dir, "bundle.min.js"), []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}

	res, err := ReadFileTool(exec).StructuredHandler(context.Background(), json.RawMessage(`{"path":"bundle.min.js"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "1\t") {
		t.Fatalf("expected a line-numbered rendering, got %.40q", res.Text)
	}
	if line := strings.TrimPrefix(res.Text, "1\t"); len(line) > maxLineBytes {
		t.Fatalf("rendered line is %d bytes, want <= %d", len(line), maxLineBytes)
	}
	marker := fmt.Sprintf("of %d bytes shown]", size)
	if !strings.HasSuffix(res.Text, marker) {
		t.Fatalf("text must end with a marker naming the original size: %q", res.Text[len(res.Text)-60:])
	}

	var excerpt fileExcerpt
	if err := json.Unmarshal(res.Structured, &excerpt); err != nil {
		t.Fatal(err)
	}
	if len(excerpt.Lines) != 1 || len(excerpt.Lines[0]) > maxLineBytes || !strings.HasSuffix(excerpt.Lines[0], marker) {
		t.Fatalf("structured lines must carry the same capped line (got %d lines)", len(excerpt.Lines))
	}
}

func TestReadFileTool_ShortLinesUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := invokeText(context.Background(), ReadFileTool(exec), json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "1\tone\n2\ttwo" {
		t.Fatalf("got %q", out)
	}
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
				if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(longLine+"\n"+shortLine+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
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
			if err := json.Unmarshal(res.Structured, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Matches) != 2 {
				t.Fatalf("expected 2 matches, got %d", len(result.Matches))
			}
			long, short := result.Matches[0].Text, result.Matches[1].Text
			if len(long) > maxLineBytes {
				t.Fatalf("matched line is %d bytes, want <= %d", len(long), maxLineBytes)
			}
			if !strings.HasPrefix(long, "needle") || !strings.HasSuffix(long, fmt.Sprintf("of %d bytes shown]", len(longLine))) {
				t.Fatalf("shortened line must keep its head and carry the marker: %q", long[len(long)-60:])
			}
			if short != shortLine {
				t.Fatalf("short line must be unchanged, got %q", short)
			}
			for _, line := range strings.Split(res.Text, "\n") {
				if len(line) > maxLineBytes+len("a.txt:1:") {
					t.Fatalf("rendered match line is %d bytes", len(line))
				}
			}
		})
	}
}

var _ executor.HostPathWorkspace = (*hostRipgrepExecutor)(nil)
