package edit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
)

func TestMultiStrategy_ToolDefinition_DeclaresOperationSchema(t *testing.T) {
	must := require.New(t)
	m := NewMultiStrategy(defaultFuzzyThreshold)
	def := m.ToolDefinition()

	must.Equal("edit_file", def.Name)
	must.NotEmpty(def.InputSchema, "input schema should not be empty")

	var schema map[string]interface{}
	must.NoError(json.Unmarshal(def.InputSchema, &schema), "schema is not valid JSON")

	props, ok := schema["properties"].(map[string]interface{})
	must.True(ok, "schema missing properties")
	for _, field := range []string{"path", "operation", "content", "diff", "old_string", "new_string"} {
		must.Contains(props, field, "schema missing property %q", field)
	}

	opProp, ok := props["operation"].(map[string]interface{})
	must.True(ok, "operation property missing or wrong type")
	enum, ok := opProp["enum"].([]interface{})
	must.True(ok, "operation enum missing")
	for _, want := range []string{"replace", "delete", "rewrite", "patch"} {
		must.Contains(enum, want, "operation enum missing %q", want)
	}

	required, _ := schema["required"].([]interface{})
	must.Contains(required, "operation", "operation should be listed in required")
}

// maxEditFileDescriptionLen mirrors the cap used for built-in tool
// descriptions in harness/internal/tool/builtins. Kept local rather than
// reaching across packages so the two test suites stay independently
// runnable.
const maxEditFileDescriptionLen = 2000

// TestMultiStrategy_Description_IncludesGuidanceAndExample asserts that the
// edit_file description carries when-to-use guidance, an example labelled
// "Example", and a bounded length. The example is extracted by finding
// the rightmost "Example" marker and walking the brace balance to
// capture the JSON object, matching the helper in
// harness/internal/tool/builtins/builtins_test.go.
func TestMultiStrategy_Description_IncludesGuidanceAndExample(t *testing.T) {
	must := require.New(t)
	m := NewMultiStrategy(defaultFuzzyThreshold)
	def := m.ToolDefinition()

	must.LessOrEqual(len(def.Description), maxEditFileDescriptionLen, "description exceeds length cap")
	// hasPositiveUseThis rejects negated matches (a description
	// containing only "Do not use this..." would otherwise pass an
	// unguarded strings.Contains check). Mirrors the helper in
	// harness/internal/tool/builtins/builtins_test.go.
	must.True(hasPositiveUseThis(def.Description),
		"description missing positive when-to-use guidance (expected non-negated \"Use this\" clause)")

	example, ok := extractEditFileJSONExample(def.Description)
	must.True(ok, "description missing JSON example after \"Example\" marker; got: %s", def.Description)
	var probe map[string]any
	must.NoError(json.Unmarshal([]byte(example), &probe), "example is not valid JSON: %s", example)

	// The structured InputExamples must mirror the description example
	// byte-for-byte; adapters send it natively or fold it into the schema
	// `examples` keyword for providers that support it.
	must.NotNil(def.Presentation, "ToolDefinition().Presentation is nil")
	must.Len(def.Presentation.InputExamples, 1, "want exactly one InputExample")
	must.Equal(example, string(def.Presentation.InputExamples[0]), "InputExamples[0] drifted from description")
}

// TestEditStrategyInputExamples_PassDispatchValidation pins that every edit
// strategy's worked example validates against the strategy's own schema
// with the validator dispatch uses, mirroring the built-in tools test.
func TestEditStrategyInputExamples_PassDispatchValidation(t *testing.T) {
	must := require.New(t)
	strategies := []EditStrategy{
		NewMultiStrategy(defaultFuzzyThreshold),
		NewUdiffStrategy(defaultFuzzyThreshold),
		NewSearchReplaceStrategy(),
		NewWholeFileStrategy(),
	}
	validated := 0
	for _, s := range strategies {
		def := s.ToolDefinition()
		if def.Presentation == nil {
			continue
		}
		for i, example := range def.Presentation.InputExamples {
			err := security.ValidateJSONSchema(example, def.InputSchema)
			must.NoError(err, "%T: InputExamples[%d] fails its InputSchema\nexample: %s", s, i, example)
			validated++
		}
	}
	must.NotZero(validated, "no edit strategy input examples were validated")
}

// hasPositiveUseThis reports whether desc contains a "Use this" clause
// that is NOT negated. Mirrors the helper in
// harness/internal/tool/builtins/builtins_test.go; the two packages
// keep independent copies so each test suite stays self-contained.
func hasPositiveUseThis(desc string) bool {
	lower := strings.ToLower(desc)
	idx := 0
	for {
		hit := strings.Index(lower[idx:], "use this")
		if hit < 0 {
			return false
		}
		pos := idx + hit
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

// extractEditFileJSONExample locates the rightmost "Example" marker in
// desc, then walks brace balance from the first '{' that follows to find
// the matching '}'. String literals are skipped so embedded braces in
// quoted values cannot terminate the scan early.
//
// Contract: descriptions must contain at least one capital-`E` "Example"
// marker followed by a valid JSON object; the rightmost marker is parsed.
// The duplicate of this helper in
// harness/internal/tool/builtins/builtins_test.go carries the same
// contract.
func extractEditFileJSONExample(desc string) (string, bool) {
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

// TestExtractEditFileJSONExample_ReturnsRightmostObject locks the contract of
// the extractEditFileJSONExample helper, the oracle
// TestMultiStrategy_Description_IncludesGuidanceAndExample uses to pin the
// description's example. It duplicates the extractJSONExample helper in
// harness/internal/tool/builtins intentionally, so the two suites run
// independently.
func TestExtractEditFileJSONExample_ReturnsRightmostObject(t *testing.T) {
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
			input:  "Use this for targeted edits. Example: {\"path\": \"x\"}",
			want:   "{\"path\": \"x\"}",
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, ok := extractEditFileJSONExample(tc.input)
			must.Equal(tc.wantOK, ok)
			must.Equal(tc.want, got)
		})
	}
}

func TestMultiStrategy_RoutesPatchToUdiff(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	// Seed a file so the diff has something to apply against.
	writeTestFile(t, dir, "test.txt", "line1\nline2\nline3\n")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "patch",
		"diff": "@@ -1,3 +1,3 @@\n line1\n-line2\n+line2_modified\n line3"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	must.Equal("line1\nline2_modified\nline3\n", content)
}

func TestMultiStrategy_RoutesReplaceToSearchReplace(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "hello world")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "replace",
		"old_string": "world",
		"new_string": "universe"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	must.Equal("hello universe", content)
}

func TestMultiStrategy_RoutesDeleteToSearchReplace(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "hello world")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "delete",
		"old_string": " world"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	must.Equal("hello", content)
}

func TestMultiStrategy_RoutesRewriteToWholeFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "new.txt",
		"operation": "rewrite",
		"content": "brand new content"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, _ := exec.ReadFile(context.Background(), "new.txt")
	must.Equal("brand new content", content)
}

func TestMultiStrategy_FallbackPatchToWholeFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	// Seed a file whose content does not match the diff's context lines,
	// so udiff will fail, but whole-file will succeed as fallback.
	writeTestFile(t, dir, "test.txt", "original content")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// operation=patch is primary; content is provided so whole-file
	// queues as a soft fallback after the diff fails.
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "patch",
		"diff": "@@ -1,3 +1,3 @@\n nonexistent_line1\n-nonexistent_line2\n+replacement\n nonexistent_line3",
		"content": "fallback content"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true via fallback; error: %s", result.Error)

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	must.Equal("fallback content", content)
}

func TestMultiStrategy_FallbackReplaceToWholeFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "hello world")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// operation=replace is primary; old_string won't be found, so the
	// search-replace fails; the content field provides a whole-file
	// safety net that succeeds.
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "replace",
		"old_string": "nonexistent text",
		"new_string": "replacement",
		"content": "whole file fallback"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true via fallback; error: %s", result.Error)

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	must.Equal("whole file fallback", content)
}

func TestMultiStrategy_MissingOperation(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"old_string": "x",
		"new_string": "y"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "operation is required")
}

func TestMultiStrategy_ReplaceMissingNewString(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "replace",
		"old_string": "x"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "new_string")
}

// TestMultiStrategy_ReplaceMissingOldString covers operation=replace with
// new_string set but old_string absent.
func TestMultiStrategy_ReplaceMissingOldString(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "replace",
		"new_string": "y"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "old_string")
}

// TestMultiStrategy_DeleteMissingOldString covers operation=delete with
// both old_string and new_string absent.
func TestMultiStrategy_DeleteMissingOldString(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "delete"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "old_string")
}

func TestMultiStrategy_DeleteWithNewStringRejected(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "delete",
		"old_string": "x",
		"new_string": "y"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "delete")
}

func TestMultiStrategy_RewriteMissingContent(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "rewrite"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "content")
}

func TestMultiStrategy_PatchMissingDiff(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "patch"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "diff")
}

func TestMultiStrategy_UnknownOperation(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "destroy"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "unknown operation")
}

func TestMultiStrategy_MissingPath(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{"operation": "rewrite", "content": "hello"}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
}

func TestMultiStrategy_InvalidJSON_ReturnsError(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{invalid`)

	_, err := m.Apply(context.Background(), input, exec)
	must.Error(err)
}

func TestMultiStrategy_AllStrategiesFail(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "original content")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// operation=patch primary fails; old_string fallback also fails.
	// No content field, so no whole-file fallback. Error must name both
	// candidates that ran.
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "patch",
		"diff": "@@ -1,3 +1,3 @@\n bad1\n-bad2\n+replacement\n bad3",
		"old_string": "nonexistent",
		"new_string": "replacement"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.NotEmpty(result.Error, "expected an error message listing failures")
	must.Contains(result.Error, "udiff")
	must.Contains(result.Error, "search-replace")
}

func TestMultiStrategy_PrimaryWinsOverFallback(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "line1\nline2\nline3\n")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// operation=patch with a valid diff; content is also provided as a
	// safety net but should NOT be reached because the patch succeeds.
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "patch",
		"diff": "@@ -1,3 +1,3 @@\n line1\n-line2\n+line2_via_diff\n line3",
		"content": "whole file override"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.True(result.Applied, "error: %s", result.Error)

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	must.Equal("line1\nline2_via_diff\nline3\n", content)
}

func newTestExecutor(t *testing.T, dir string) executor.Executor {
	t.Helper()
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(dir)
	must.NoError(err)
	return exec
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(dir, name)
	must.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	must.NoError(os.WriteFile(path, []byte(content), 0o644))
}
