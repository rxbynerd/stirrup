package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// mockExecutor implements executor.Executor for unit testing tool handlers
// without touching the real filesystem or shell.
type mockExecutor struct {
	readFileFunc      func(ctx context.Context, path string) (string, error)
	writeFileFunc     func(ctx context.Context, path string, content string) error
	listDirectoryFunc func(ctx context.Context, path string) ([]string, error)
	execFunc          func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
	resolvePathFunc   func(relativePath string) (string, error)
}

func (m *mockExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	if m.readFileFunc != nil {
		return m.readFileFunc(ctx, path)
	}
	return "", fmt.Errorf("ReadFile not mocked")
}

func (m *mockExecutor) WriteFile(ctx context.Context, path string, content string) error {
	if m.writeFileFunc != nil {
		return m.writeFileFunc(ctx, path, content)
	}
	return fmt.Errorf("WriteFile not mocked")
}

func (m *mockExecutor) ListDirectory(ctx context.Context, path string) ([]string, error) {
	if m.listDirectoryFunc != nil {
		return m.listDirectoryFunc(ctx, path)
	}
	return nil, fmt.Errorf("ListDirectory not mocked")
}

func (m *mockExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	if m.execFunc != nil {
		return m.execFunc(ctx, command, timeout)
	}
	return nil, fmt.Errorf("Exec not mocked")
}

func (m *mockExecutor) ResolvePath(relativePath string) (string, error) {
	if m.resolvePathFunc != nil {
		return m.resolvePathFunc(relativePath)
	}
	return "/workspace/" + relativePath, nil
}

func (m *mockExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   true,
		CanExec:    true,
		CanNetwork: true,
		MaxTimeout: 5 * time.Minute,
	}
}

// invokeText dispatches a tool the way the agentic loop does — preferring a
// StructuredHandler over a plain Handler — and returns just the canonical
// text output and error. Tests that only assert the text fallback use this so
// they remain agnostic to which handler form a tool exposes; tests that
// assert the structured payload call StructuredHandler directly.
func invokeText(ctx context.Context, tl *tool.Tool, input json.RawMessage) (string, error) {
	if tl.StructuredHandler != nil {
		res, err := tl.StructuredHandler(ctx, input)
		return res.Text, err
	}
	return tl.Handler(ctx, input)
}

// --- shellQuote tests ---

func TestShellQuote_EmbeddedSingleQuote(t *testing.T) {
	must := require.New(t)
	// A single quote in the input must not terminate the quoted word.
	must.Equal(`'a'\''b'`, shellQuote("a'b"))
}

// --- WebFetchTool tests ---

func TestWebFetchTool_SchemeRejection(t *testing.T) {
	fetchTool := WebFetchTool()

	cases := []struct {
		name string
		url  string
	}{
		{"file scheme", "file:///etc/passwd"},
		{"ftp scheme", "ftp://example.com/file"},
		{"javascript scheme", "javascript:alert(1)"},
		{"data scheme", "data:text/html,<h1>Hi</h1>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			input, _ := json.Marshal(map[string]string{"url": tc.url})
			_, err := fetchTool.Handler(context.Background(), input)
			must.Error(err)
			must.True(strings.Contains(err.Error(), "http://") || strings.Contains(err.Error(), "https://"),
				"error should mention allowed schemes, got: %v", err)
		})
	}
}

func TestWebFetchTool_PrivateHostRejection(t *testing.T) {
	must := require.New(t)
	fetchTool := WebFetchTool()
	input, _ := json.Marshal(map[string]string{"url": "http://127.0.0.1/secret"})

	_, err := fetchTool.Handler(context.Background(), input)
	must.ErrorContains(err, "private host")
}

func TestWebFetchTool_ReportsHTTPErrorStatus(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	fetchTool := newWebFetchTool(webFetchOptions{
		client:            srv.Client(),
		allowPrivateHosts: true,
	})
	input, _ := json.Marshal(map[string]string{"url": srv.URL})

	_, err := fetchTool.Handler(context.Background(), input)
	must.ErrorContains(err, "403")
}

func TestWebFetchTool_TruncatesLargeBody(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Return a body larger than maxFetchSize (100 KB).
	bigBody := strings.Repeat("A", 120*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, bigBody)
	}))
	defer srv.Close()

	fetchTool := newWebFetchTool(webFetchOptions{
		client:            srv.Client(),
		allowPrivateHosts: true,
	})
	input, _ := json.Marshal(map[string]string{"url": srv.URL})

	result, err := fetchTool.Handler(context.Background(), input)
	must.NoError(err)
	check.Contains(result, truncatedNotice)
	maxExpected := maxFetchSize + len(truncatedNotice)
	check.LessOrEqual(len(result), maxExpected)
}

// --- ReadFileTool tests ---

func TestReadFileTool_RejectsEmptyPath(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]string{"path": ""})
	_, err := invokeText(context.Background(), readTool, input)
	must.ErrorContains(err, "path is required")
}

func TestReadFileTool_LineNumberedOutput(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "alpha\nbeta\ngamma\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "file.txt"})
	result, err := invokeText(context.Background(), readTool, input)
	must.NoError(err)
	must.Equal("1\talpha\n2\tbeta\n3\tgamma", result)
}

func TestReadFileTool_LineRange(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "one\ntwo\nthree\nfour\nfive\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":       "file.txt",
		"start_line": 2,
		"limit":      2,
	})
	result, err := invokeText(context.Background(), readTool, input)
	must.NoError(err)
	must.Equal("2\ttwo\n3\tthree", result)
}

func TestReadFileTool_StartLinePastEOF(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "only one line\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":       "file.txt",
		"start_line": 500,
	})
	result, err := invokeText(context.Background(), readTool, input)
	must.NoError(err, "start_line past EOF should not be an error")
	must.Contains(result, "past end of file")
}

func TestReadFileTool_RejectsLimitAboveMax(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":  "file.txt",
		"limit": 100000,
	})
	_, err := invokeText(context.Background(), readTool, input)
	must.ErrorContains(err, "limit must be <=")
}

func TestReadFileTool_PaddingAlignsColumns(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	// A 12-line range crosses single→double digit boundaries; line numbers
	// must be right-aligned to a width of 2 so columns line up.
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = "x"
	}
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return strings.Join(lines, "\n") + "\n", nil
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "file.txt"})
	result, err := invokeText(context.Background(), readTool, input)
	must.NoError(err)
	check.Contains(result, " 1\tx", "line 1 should be right-aligned")
	check.Contains(result, "12\tx", "line 12 should be unpadded")
}

// --- ListDirectoryTool tests ---

func TestListDirectoryTool_FlatByDefault(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			return []string{"file.go", "sub/"}, nil
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "."})
	result, err := listTool.Handler(context.Background(), input)
	must.NoError(err)
	must.Equal("file.go\nsub/", result)
}

func TestListDirectoryTool_Recursive(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			switch path {
			case ".":
				return []string{"a.go", "sub/"}, nil
			case "./sub":
				return []string{"b.go", "nested/"}, nil
			case "./sub/nested":
				return []string{"c.go"}, nil
			}
			return nil, fmt.Errorf("unexpected path %q", path)
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 5,
	})
	result, err := listTool.Handler(context.Background(), input)
	must.NoError(err)
	for _, want := range []string{"a.go", "sub/", "sub/b.go", "sub/nested/", "sub/nested/c.go"} {
		t.Run(want, func(t *testing.T) {
			must := require.New(t)
			must.Contains(result, want)
		})
	}
}

func TestListDirectoryTool_RecursiveDepthLimit(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			switch path {
			case ".":
				return []string{"sub/"}, nil
			case "./sub":
				return []string{"deeper/"}, nil
			case "./sub/deeper":
				return []string{"file.go"}, nil
			}
			return nil, nil
		},
	}
	listTool := ListDirectoryTool(mock)

	// max_depth=1 should list "sub/" and "sub/deeper/" but NOT "sub/deeper/file.go".
	input, _ := json.Marshal(map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 1,
	})
	result, err := listTool.Handler(context.Background(), input)
	must.NoError(err)
	check.Contains(result, "sub/")
	check.Contains(result, "sub/deeper/", "max_depth=1 should list the directory at depth 1")
	check.NotContains(result, "file.go", "max_depth=1 should not list files below depth 1")
}

func TestListDirectoryTool_MaxEntriesTruncation(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			return []string{"a", "b", "c", "d", "e"}, nil
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":        ".",
		"max_entries": 3,
	})
	result, err := listTool.Handler(context.Background(), input)
	must.NoError(err)
	must.Contains(result, "[truncated")
}

func TestListDirectoryTool_RejectsEmptyPath(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{"path": ""})
	_, err := listTool.Handler(context.Background(), input)
	must.ErrorContains(err, "path is required")
}

func TestListDirectoryTool_RejectsOutOfRangeMaxEntries(t *testing.T) {
	mock := &mockExecutor{}
	listTool := ListDirectoryTool(mock)

	cases := []struct {
		name       string
		maxEntries int
		wantErr    string
	}{
		{"below minimum", 0, "max_entries must be >="},
		{"above maximum", 100000, "max_entries must be <="},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			input, _ := json.Marshal(map[string]any{"path": ".", "max_entries": tc.maxEntries})
			_, err := listTool.Handler(context.Background(), input)
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestListDirectoryTool_RecursiveFailsWhenRootInaccessible(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		listDirectoryFunc: func(ctx context.Context, path string) ([]string, error) {
			return nil, fmt.Errorf("root inaccessible")
		},
	}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{"path": ".", "recursive": true})
	_, err := listTool.Handler(context.Background(), input)
	must.Error(err)
}

func TestReadFileTool_RejectsStartLineBelowOne(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":       "file.txt",
		"start_line": 0,
	})
	_, err := invokeText(context.Background(), readTool, input)
	must.ErrorContains(err, "start_line must be >=")
}

func TestReadFileTool_RejectsLimitBelowOne(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":  "file.txt",
		"limit": 0,
	})
	_, err := invokeText(context.Background(), readTool, input)
	must.ErrorContains(err, "limit must be >=")
}

func TestReadFileTool_PropagatesReadError(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		readFileFunc: func(ctx context.Context, path string) (string, error) {
			return "", fmt.Errorf("file not found")
		},
	}
	readTool := ReadFileTool(mock)

	input, _ := json.Marshal(map[string]any{"path": "missing.txt"})
	_, err := invokeText(context.Background(), readTool, input)
	must.ErrorContains(err, "file not found")
}

func TestListDirectoryTool_RejectsMaxDepthAboveLimit(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	listTool := ListDirectoryTool(mock)

	input, _ := json.Marshal(map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 11,
	})
	_, err := listTool.Handler(context.Background(), input)
	must.ErrorContains(err, "max_depth must be <=")
}

// --- WriteFileTool tests ---

func TestWriteFileTool_WritesContentAndReportsBytes(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var writtenPath, writtenContent string
	mock := &mockExecutor{
		writeFileFunc: func(ctx context.Context, path string, content string) error {
			writtenPath = path
			writtenContent = content
			return nil
		},
	}

	writeTool := WriteFileTool(mock)
	input, _ := json.Marshal(map[string]any{
		"path":    "src/main.go",
		"content": "package main",
	})

	result, err := writeTool.Handler(context.Background(), input)
	must.NoError(err)

	check.Equal("src/main.go", writtenPath)
	check.Equal("package main", writtenContent)
	check.Contains(result, "12 bytes")
	check.Contains(result, "src/main.go")
}

// --- RunCommandTool tests ---

func TestRunCommandTool_NonZeroExitCode(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	mock := &mockExecutor{
		execFunc: func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				ExitCode: 1,
				Stdout:   "some output",
				Stderr:   "error occurred",
			}, nil
		},
	}

	runTool := RunCommandTool(mock)
	input, _ := json.Marshal(map[string]string{"command": "false"})

	result, err := invokeText(context.Background(), runTool, input)
	must.NoError(err)
	check.Contains(result, "[exit code: 1]")
	check.Contains(result, "some output")
	check.Contains(result, "STDERR:")
}

func TestRunCommandTool_RejectsEmptyCommand(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	runTool := RunCommandTool(mock)

	input, _ := json.Marshal(map[string]string{"command": ""})
	_, err := invokeText(context.Background(), runTool, input)
	must.ErrorContains(err, "command is required")
}

// --- registerAllForTest tests ---

func TestRegisterAll_RegistersExpectedTools(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{}
	registry := tool.NewRegistry()
	registerAllForTest(registry, mock)

	expectedTools := []string{
		"read_file",
		"write_file",
		"list_directory",
		"grep_files",
		"find_files",
		"run_command",
		"git_status",
		"git_changed_files",
		"git_diff",
		"git_show",
		"web_fetch",
	}

	defs := registry.List()
	must.Len(defs, len(expectedTools))

	registered := make(map[string]bool, len(defs))
	for _, def := range defs {
		registered[def.Name] = true
	}

	for _, name := range expectedTools {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.True(registered[name], "tool %q should be registered", name)
		})
	}

	must.False(registered["search_files"],
		"search_files must not be registered (split into grep_files and find_files)")
}

// maxToolDescriptionLen caps each enriched description so a worst-case
// system prompt with every tool registered stays bounded. 2000 chars per
// tool is generous — the longest description today is well under 1000.
const maxToolDescriptionLen = 2000

// hasPositiveUseThis reports whether desc contains a "Use this" clause
// that is NOT negated. A description containing only "Do not use this..."
// or "Don't use this..." would slip past a plain substring check on
// "Use this"; this helper walks every occurrence and rejects those whose
// immediately preceding text ends in "not " or "'t ", returning true
// only if at least one *positive* when-to-use clause is present.
// Case-insensitive: both "Use this" and "use this" count.
func hasPositiveUseThis(desc string) bool {
	lower := strings.ToLower(desc)
	idx := 0
	for {
		hit := strings.Index(lower[idx:], "use this")
		if hit < 0 {
			return false
		}
		pos := idx + hit
		// Inspect the immediately preceding context (up to 8 chars) for
		// negative-guidance prefixes. "Do not " / "don't " / " not "
		// all end in "not " or "'t " before "use this".
		start := pos - 8
		if start < 0 {
			start = 0
		}
		prefix := lower[start:pos]
		if !strings.HasSuffix(prefix, "not ") && !strings.HasSuffix(prefix, "'t ") {
			return true
		}
		idx = pos + len("use this")
	}
}

// extractJSONExample pulls the JSON object that follows the rightmost
// "Example" marker in a tool description and returns the raw bytes.
// Matching from the rightmost marker keeps tools with multiple worked
// examples (e.g. edit_file's "Example (replace): ...") working without
// the test having to know each tool's example label. The boundary is the
// matching '}' for the first '{' after the marker, walking the brace
// balance so embedded objects do not terminate the scan early.
//
// Contract: descriptions must contain at least one capital-`E` "Example"
// marker followed by a valid JSON object. The rightmost such marker is the
// one parsed. Each built-in carries the same example as its structured
// tool.Tool.InputExamples; this helper is the oracle
// TestBuiltinInputExamples_MatchDescription uses to pin the two together.
func extractJSONExample(desc string) (string, bool) {
	marker := strings.LastIndex(desc, "Example")
	if marker < 0 {
		return "", false
	}
	rest := desc[marker:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := open; i < len(rest); i++ {
		c := rest[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open : i+1], true
			}
		}
	}
	return "", false
}

// TestBuiltinDescriptions_HaveWhenToUseAndExample asserts that every registered
// built-in tool description carries when-to-use guidance, a syntactically
// valid JSON example, and a length below the system-prompt budget cap.
func TestBuiltinDescriptions_HaveWhenToUseAndExample(t *testing.T) {
	mock := &mockExecutor{}
	registry := tool.NewRegistry()
	registerAllForTest(registry, mock)

	for _, def := range registry.List() {
		t.Run(def.Name, func(t *testing.T) {
			must := require.New(t)
			must.LessOrEqual(len(def.Description), maxToolDescriptionLen)
			// "Use this" is the canonical when-to-use opener across enriched
			// descriptions; asserting on it forces future tools to adopt the
			// same convention.
			must.True(hasPositiveUseThis(def.Description),
				"description needs a non-negated \"Use this\" when-to-use clause")
			example, ok := extractJSONExample(def.Description)
			must.True(ok, "description missing JSON example after \"Example\" marker; got: %s", def.Description)
			var probe map[string]any
			must.NoError(json.Unmarshal([]byte(example), &probe), "example is not valid JSON: %s", example)
		})
	}
}

// TestBuiltinInputExamples_MatchDescription pins the invariant that every
// built-in tool's structured InputExamples is byte-identical to the worked
// example embedded in its description: the description carries the
// human-readable example for providers whose schema dialect rejects the
// `examples` keyword (Gemini), while InputExamples carries the structured
// form adapters send natively or fold into the schema. edit_file is covered
// separately in the edit package (it is registered via the factory's
// strategy wrapper).
func TestBuiltinInputExamples_MatchDescription(t *testing.T) {
	mock := &mockExecutor{}
	// Construct directly rather than via registerAllForTest so spawn_agent
	// (registered by the factory in production) is covered here too. The nil
	// spawner is fine: only Definition() is exercised, not the handler.
	tools := []*tool.Tool{
		ReadFileTool(mock),
		WriteFileTool(mock),
		ListDirectoryTool(mock),
		GrepFilesTool(mock),
		FindFilesTool(mock),
		RunCommandTool(mock),
		GitStatusTool(mock),
		GitChangedFilesTool(mock),
		GitDiffTool(mock),
		GitShowTool(mock),
		WebFetchTool(),
		SpawnAgentTool(nil),
	}
	for _, tl := range tools {
		t.Run(tl.Name, func(t *testing.T) {
			must := require.New(t)
			want, ok := extractJSONExample(tl.Description)
			must.True(ok, "description has no Example marker to mirror")
			def := tl.Definition()
			must.NotNil(def.Presentation, "Definition() must set Presentation")
			must.Len(def.Presentation.InputExamples, 1, "want exactly one InputExample")
			got := string(def.Presentation.InputExamples[0])
			must.Equal(want, got, "InputExamples[0] drifted from description")
			var probe map[string]any
			must.NoError(json.Unmarshal(def.Presentation.InputExamples[0], &probe), "InputExamples[0] is not valid JSON")
		})
	}
}

// TestBuiltinInputExamples_PassDispatchValidation pins that every built-in
// worked example is an input the harness would accept: the Anthropic API
// requires each input_examples entry to validate against the tool's
// schema, and a model imitating an invalid example would have its call
// rejected at dispatch.
func TestBuiltinInputExamples_PassDispatchValidation(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	registerAllForTest(registry, &mockExecutor{})
	defs := append(registry.List(), SpawnAgentTool(nil).Definition())

	validated := 0
	for _, def := range defs {
		if def.Presentation == nil {
			continue
		}
		t.Run(def.Name, func(t *testing.T) {
			must := require.New(t)
			for i, example := range def.Presentation.InputExamples {
				must.NoError(security.ValidateJSONSchema(example, def.InputSchema),
					"InputExamples[%d] fails its InputSchema; example: %s", i, example)
				validated++
			}
		})
	}
	must.NotZero(validated, "no built-in input examples were validated")
}

// TestHasPositiveUseThis_DetectsNonNegatedClause confirms the tightened
// when-to-use check rejects descriptions whose only "Use this" appears inside
// a negation (Do not / Don't / not) while still passing descriptions that carry
// both a negation and a genuine positive clause. Locks the contract so
// a future loosening of the helper trips a clear failure.
func TestHasPositiveUseThis_DetectsNonNegatedClause(t *testing.T) {
	cases := []struct {
		name string
		desc string
		want bool
	}{
		{
			name: "only negated do not",
			desc: "Do not use this for X.",
			want: false,
		},
		{
			name: "only negated don't",
			desc: "Don't use this for X.",
			want: false,
		},
		{
			name: "only negated bare not",
			desc: "We do not use this approach.",
			want: false,
		},
		{
			name: "positive only",
			desc: "Use this when reading a known file.",
			want: true,
		},
		{
			name: "both negated and positive",
			desc: "Use this for X. Do not use this for Y.",
			want: true,
		},
		{
			name: "no use this at all",
			desc: "Some unrelated description.",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, hasPositiveUseThis(tc.desc))
		})
	}
}

// TestExtractJSONExample_ParsesObjectAfterMarker locks the contract of the
// extractJSONExample helper directly, independent of any real tool description.
func TestExtractJSONExample_ParsesObjectAfterMarker(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			name:   "no marker",
			input:  "This description has no marker",
			want:   "",
			wantOK: false,
		},
		{
			name:   "two markers rightmost wins",
			input:  "Example: {\"a\": 1}\n\nExample: {\"b\": 2}",
			want:   "{\"b\": 2}",
			wantOK: true,
		},
		{
			name:   "malformed json after marker",
			input:  "Example: {not valid",
			want:   "",
			wantOK: false,
		},
		{
			name:   "standard happy path",
			input:  "Use this when reading a known file. Example: {\"path\": \"x\"}",
			want:   "{\"path\": \"x\"}",
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, ok := extractJSONExample(tc.input)
			must.Equal(tc.wantOK, ok)
			must.Equal(tc.want, got)
		})
	}
}

// TestSpawnAgentTool_DescriptionHasWhenToUseAndExample applies the same
// description contract to spawn_agent, which is wired separately from
// registerAllForTest (the factory injects a real spawner closure). A trivial
// spawner suffices because only the static tool definition is under test.
func TestSpawnAgentTool_DescriptionHasWhenToUseAndExample(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	noopSpawner := func(ctx context.Context, prompt, mode string, maxTurns int) (json.RawMessage, error) {
		return json.RawMessage("{}"), nil
	}
	def := SpawnAgentTool(noopSpawner)
	check.LessOrEqual(len(def.Description), maxToolDescriptionLen)
	check.True(hasPositiveUseThis(def.Description),
		"description needs a non-negated \"Use this\" when-to-use clause")
	example, ok := extractJSONExample(def.Description)
	must.True(ok, "description missing JSON example after \"Example\" marker")
	var probe map[string]any
	must.NoError(json.Unmarshal([]byte(example), &probe), "example is not valid JSON: %s", example)
}

// TestSpawnAgentTool_DescriptionStatesReadOnlyRule pins that the model is
// told which tools a read-only sub-agent loses and that a read-only run
// cannot request an execution sub-agent.
func TestSpawnAgentTool_DescriptionStatesReadOnlyRule(t *testing.T) {
	desc := SpawnAgentTool(nil).Description
	for _, want := range []string{
		"loses every workspace-mutating tool",
		"can only spawn read-only sub-agents",
		"requesting execution mode returns an error",
	} {
		t.Run(want, func(t *testing.T) {
			must := require.New(t)
			must.Contains(desc, want)
		})
	}
}
