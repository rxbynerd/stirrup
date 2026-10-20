package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// writeRecorder counts WriteFile calls so tests can assert a hint-only
// failure never touches the file.
type writeRecorder struct {
	executor.Executor
	writes int
}

func (w *writeRecorder) WriteFile(ctx context.Context, path, content string) error {
	w.writes++
	return w.Executor.WriteFile(ctx, path, content)
}

func applySearchReplace(t *testing.T, path, content, oldString string) (string, *writeRecorder) {
	t.Helper()
	must := require.New(t)
	dir := t.TempDir()
	writeTestFile(t, dir, path, content)
	rec := &writeRecorder{Executor: newTestExecutor(t, dir)}

	input, err := json.Marshal(map[string]string{"path": path, "old_string": oldString, "new_string": "replacement"})
	must.NoError(err)
	result, err := NewSearchReplaceStrategy().Apply(context.Background(), input, rec)
	must.NoError(err)
	must.False(result.Applied, "expected Applied=false")

	after, _ := rec.ReadFile(context.Background(), path)
	must.Equal(content, after, "file modified")
	return result.Error, rec
}

func TestSearchReplace_MismatchHint(t *testing.T) {
	goFile := "package main\n\nfunc a() {\n\tx := 1\n\ty := 2\n\treturn\n}\n\nfunc b() {\n}\n"
	tests := []struct {
		name      string
		content   string
		oldString string
		want      []string
		wantNot   []string
	}{
		{
			name:      "spaces instead of tabs",
			content:   goFile,
			oldString: "    x := 1\n    y := 2",
			want:      []string{"not found in file", "lines 4-5", "indentation (the file indents with tabs, old_string uses spaces)"},
		},
		{
			name:      "tabs instead of spaces",
			content:   "def f():\n    x = 1\n    y = 2\n",
			oldString: "\tx = 1\n\ty = 2",
			want:      []string{"lines 2-3", "the file indents with spaces, old_string uses tabs"},
		},
		{
			name:      "indentation width",
			content:   "if a {\n  b()\n}\n",
			oldString: "    b()",
			want:      []string{"line 2", "indentation width"},
		},
		{
			name:      "file has CRLF",
			content:   "alpha\r\nbeta\r\ngamma\r\n",
			oldString: "alpha\nbeta\ngamma",
			want:      []string{"lines 1-3", "line endings (the file uses CRLF, old_string uses LF)"},
		},
		{
			name:      "old_string has CRLF",
			content:   "alpha\nbeta\ngamma\n",
			oldString: "beta\r\ngamma",
			want:      []string{"lines 2-3", "line endings (the file uses LF, old_string uses CRLF)"},
		},
		{
			name:      "trailing whitespace in file",
			content:   "one\ntwo  \nthree\n",
			oldString: "one\ntwo\nthree",
			want:      []string{"lines 1-3", "trailing whitespace"},
			wantNot:   []string{"indentation", "line endings"},
		},
		{
			name:      "trailing whitespace in old_string",
			content:   "one\ntwo\nthree\n",
			oldString: "two \nthree",
			want:      []string{"lines 2-3", "trailing whitespace"},
		},
		{
			name:      "spacing within a line",
			content:   "x  :=   compute(a,   b)\n",
			oldString: "x := compute(a, b)",
			want:      []string{"line 1", "spacing within lines"},
		},
		{
			name:      "old_string starts and ends mid-line",
			content:   "func f() {\n\tx := foo(bar)\n\treturn x + 1\n}\n",
			oldString: "foo(bar)\n    return x",
			want:      []string{"lines 2-3", "indentation (the file indents with tabs, old_string uses spaces)"},
			wantNot:   []string{"spacing within lines", "trailing whitespace"},
		},
		{
			name:      "spacing within a partial line",
			content:   "func f() {\n\tx := foo(a,  b)\n\treturn x\n}\n",
			oldString: "foo(a, b)\n\treturn x",
			want:      []string{"lines 2-3", "spacing within lines"},
		},
		{
			name:      "moved and edited block",
			content:   "// header\n// header\n// header\nfunc target() {\n\tstep1()\n\tstep2Changed()\n\tstep3()\n}\nfunc other() {}\n",
			oldString: "func target() {\n\tstep1()\n\tstep2()\n\tstep3()\n}",
			want:      []string{"closest region is lines 4-8", "% similar", "6: \tstep2Changed()", "nothing was changed"},
		},
		{
			name:      "multiple whitespace-equivalent matches",
			content:   "a  b\nx\na   b\n",
			oldString: "a b",
			want:      []string{"2 such locations", "line 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			msg, rec := applySearchReplace(t, "f.txt", tt.content, tt.oldString)
			for _, w := range tt.want {
				must.Contains(msg, w)
			}
			for _, w := range tt.wantNot {
				must.NotContains(msg, w)
			}
			must.Zero(rec.writes, "hint-only failure must not write the file")
		})
	}
}

func TestSearchReplace_NoSimilarRegion(t *testing.T) {
	must := require.New(t)
	msg, _ := applySearchReplace(t, "f.go",
		"package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n",
		"zebra giraffe\nantelope wildebeest")

	must.Contains(msg, "no similar region")
	must.Contains(msg, "Read the file again")
	for _, leaked := range []string{"package main", "println", "func main"} {
		must.NotContains(msg, leaked, "message quotes unrelated file text")
	}
}

func TestSearchReplace_ClosestRegionQuoteBounds(t *testing.T) {
	must := require.New(t)
	long := strings.Repeat("w", 400)
	content := "a1 a2 a3 " + long + "\nb1 b2 b3 b4\nc1 c2 c3 c4\nd1 d2 d3 d4\ne1 e2 e3 e4\n"
	msg, _ := applySearchReplace(t, "f.txt", content,
		"a1 a2 a3 "+long+"X\nb1 b2 b3 b4\nc1 c2 c3 c4\nd1 d2 d3 d4\ne1 e2 e3 e4")

	must.LessOrEqual(strings.Count(msg, "\n"), maxQuotedLines+1, "message quotes too many lines:\n%s", msg)
	must.LessOrEqual(len(msg), maxMismatchHintBytes, "message exceeds size bound:\n%s", msg)
}

func TestSearchReplace_CredentialPathWithholdsQuote(t *testing.T) {
	must := require.New(t)
	content := "API_ENDPOINT=https://example.test/v1\nAPI_TOKEN=supersecretvalue\nREGION=eu\n"
	old := "API_ENDPOINT=https://example.test/v1\nAPI_TOKEN=differentvalue\nREGION=eu"

	plain, _ := applySearchReplace(t, "settings.txt", content, old)
	must.Contains(plain, "supersecretvalue", "control: non-credential path should quote file text")

	for _, path := range []string{".env", "deploy/.env", ".ssh/config", "keys/server.pem"} {
		t.Run(path, func(t *testing.T) {
			must := require.New(t)
			msg, _ := applySearchReplace(t, path, content, old)
			must.NotContains(msg, "supersecretvalue", "credential path should not quote file text")
			must.Contains(msg, "closest region is lines 1-3")
		})
	}
}

func TestSearchReplace_MismatchHintBoundedForLargeFile(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, "\tline %d := compute(%d)\n", i, i)
	}
	content := b.String()

	hugeOld := strings.Repeat("    padding line that matches nothing in the file at all\n", 2000)
	tests := []struct {
		name string
		old  string
	}{
		{name: "near match", old: "    line 40000 := compute(40000)\n    line 40001 := compute(40001)"},
		{name: "long old", old: hugeOld},
		{name: "similar huge", old: strings.Repeat("\tline 7 := compute(7)\n", 3000) + "tail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			msg, _ := applySearchReplace(t, "big.go", content, tt.old)
			must.LessOrEqual(len(msg), maxMismatchHintBytes)
			must.True(strings.HasPrefix(msg, notFoundPrefix), "message lost its prefix: %.80s", msg)
		})
	}
}

func TestBoundMessage_TruncatesOnRuneBoundary(t *testing.T) {
	must := require.New(t)
	msg := boundMessage(strings.Repeat("é", maxMismatchHintBytes))
	must.LessOrEqual(len(msg), maxMismatchHintBytes)
	must.True(strings.HasSuffix(msg, "..."), "expected truncation marker, got suffix %q", msg[max(0, len(msg)-6):])
	must.NotContains(msg, "�", "truncation split a multi-byte rune")
}

func TestSearchReplace_WhitespaceOnlyOldStringHasNoHint(t *testing.T) {
	must := require.New(t)
	msg, _ := applySearchReplace(t, "f.txt", "a\nb\n", "   \n  ")
	must.Equal(notFoundPrefix, msg)
}

func TestMultiStrategy_ReplaceCarriesMismatchHint(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeTestFile(t, dir, "f.go", "func a() {\n\tx := 1\n\ty := 2\n}\n")
	rec := &writeRecorder{Executor: newTestExecutor(t, dir)}

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "f.go",
		"operation": "replace",
		"old_string": "    x := 1\n    y := 2",
		"new_string": "z := 3"
	}`)
	result, err := m.Apply(context.Background(), input, rec)
	must.NoError(err)
	must.False(result.Applied, "expected Applied=false")
	for _, want := range []string{"all strategies failed", "lines 2-3", "the file indents with tabs"} {
		must.Contains(result.Error, want)
	}
	must.Zero(rec.writes, "hint-only failure must not write the file")
}
