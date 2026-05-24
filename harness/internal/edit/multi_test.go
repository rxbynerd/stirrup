package edit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestMultiStrategy_ToolDefinition(t *testing.T) {
	m := NewMultiStrategy(defaultFuzzyThreshold)
	def := m.ToolDefinition()

	if def.Name != "edit_file" {
		t.Errorf("name: got %q, want %q", def.Name, "edit_file")
	}
	if len(def.InputSchema) == 0 {
		t.Error("input schema should not be empty")
	}

	// Verify the schema is valid JSON containing expected fields, the
	// operation enum is declared, and operation is on the required list.
	var schema map[string]interface{}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema missing properties")
	}
	for _, field := range []string{"path", "operation", "content", "diff", "old_string", "new_string"} {
		if _, exists := props[field]; !exists {
			t.Errorf("schema missing property %q", field)
		}
	}
	opProp, ok := props["operation"].(map[string]interface{})
	if !ok {
		t.Fatal("operation property missing or wrong type")
	}
	enum, ok := opProp["enum"].([]interface{})
	if !ok {
		t.Fatal("operation enum missing")
	}
	for _, want := range []string{"replace", "delete", "rewrite", "patch"} {
		found := false
		for _, v := range enum {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("operation enum missing %q", want)
		}
	}
	required, _ := schema["required"].([]interface{})
	hasOperation := false
	for _, v := range required {
		if v == "operation" {
			hasOperation = true
		}
	}
	if !hasOperation {
		t.Error("operation should be listed in required")
	}
}

// maxEditFileDescriptionLen mirrors the cap used for built-in tool
// descriptions in harness/internal/tool/builtins. Kept local rather than
// reaching across packages so the two test suites stay independently
// runnable.
const maxEditFileDescriptionLen = 2000

// TestMultiStrategy_DescriptionEnrichedShape asserts that the edit_file
// description carries the #227 contract — when-to-use guidance, an
// example labelled "Example", and a bounded length. The example is
// extracted by finding the rightmost "Example" marker and walking the
// brace balance to capture the JSON object, matching the helper in
// harness/internal/tool/builtins/builtins_test.go.
func TestMultiStrategy_DescriptionEnrichedShape(t *testing.T) {
	m := NewMultiStrategy(defaultFuzzyThreshold)
	def := m.ToolDefinition()

	if len(def.Description) > maxEditFileDescriptionLen {
		t.Errorf("description length %d exceeds cap %d", len(def.Description), maxEditFileDescriptionLen)
	}
	if !strings.Contains(def.Description, "Use this") {
		t.Errorf("description missing when-to-use guidance (expected substring %q)", "Use this")
	}
	example, ok := extractEditFileJSONExample(def.Description)
	if !ok {
		t.Fatalf("description missing JSON example after \"Example\" marker; got: %s", def.Description)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(example), &probe); err != nil {
		t.Errorf("example is not valid JSON: %v\nexample: %s", err, example)
	}
}

// extractEditFileJSONExample locates the rightmost "Example" marker in
// desc, then walks brace balance from the first '{' that follows to find
// the matching '}'. String literals are skipped so embedded braces in
// quoted values cannot terminate the scan early.
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

func TestMultiStrategy_RoutesPatchToUdiff(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true; error: %s", result.Error)
	}

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	if content != "line1\nline2_modified\nline3\n" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestMultiStrategy_RoutesReplaceToSearchReplace(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true; error: %s", result.Error)
	}

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	if content != "hello universe" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestMultiStrategy_RoutesDeleteToSearchReplace(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true; error: %s", result.Error)
	}

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	if content != "hello" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestMultiStrategy_RoutesRewriteToWholeFile(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "new.txt",
		"operation": "rewrite",
		"content": "brand new content"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true; error: %s", result.Error)
	}

	content, _ := exec.ReadFile(context.Background(), "new.txt")
	if content != "brand new content" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestMultiStrategy_FallbackPatchToWholeFile(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true via fallback; error: %s", result.Error)
	}

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	if content != "fallback content" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestMultiStrategy_FallbackReplaceToWholeFile(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Errorf("expected Applied=true via fallback; error: %s", result.Error)
	}

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	if content != "whole file fallback" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestMultiStrategy_MissingOperation(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// No operation field — the new contract requires one.
	input := json.RawMessage(`{
		"path": "test.txt",
		"old_string": "x",
		"new_string": "y"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when operation is missing")
	}
	if !contains(result.Error, "operation is required") {
		t.Errorf("error should mention required operation, got: %s", result.Error)
	}
}

func TestMultiStrategy_ReplaceMissingNewString(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "replace",
		"old_string": "x"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when replace omits new_string")
	}
	if !contains(result.Error, "new_string") {
		t.Errorf("error should mention new_string, got: %s", result.Error)
	}
}

// TestMultiStrategy_ReplaceMissingOldString covers the validateOperationFields
// branch at multi.go:225 — operation=replace with new_string set but
// old_string absent. The pre-existing tests covered missing new_string
// and the unknown-operation paths, leaving this one reachable production
// branch unexercised.
func TestMultiStrategy_ReplaceMissingOldString(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "replace",
		"new_string": "y"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when replace omits old_string")
	}
	if !contains(result.Error, "old_string") {
		t.Errorf("error should mention old_string, got: %s", result.Error)
	}
}

// TestMultiStrategy_DeleteMissingOldString covers the branch at multi.go:233
// — operation=delete with both old_string and new_string absent. The
// pre-existing delete tests covered the wrong-direction case (new_string
// supplied) but not the missing-required-field case.
func TestMultiStrategy_DeleteMissingOldString(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "delete"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when delete omits old_string")
	}
	if !contains(result.Error, "old_string") {
		t.Errorf("error should mention old_string, got: %s", result.Error)
	}
}

func TestMultiStrategy_DeleteWithNewStringRejected(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when delete supplies new_string")
	}
	if !contains(result.Error, "delete") {
		t.Errorf("error should mention delete operation, got: %s", result.Error)
	}
}

func TestMultiStrategy_RewriteMissingContent(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "rewrite"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when rewrite omits content")
	}
	if !contains(result.Error, "content") {
		t.Errorf("error should mention content, got: %s", result.Error)
	}
}

func TestMultiStrategy_PatchMissingDiff(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "patch"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when patch omits diff")
	}
	if !contains(result.Error, "diff") {
		t.Errorf("error should mention diff, got: %s", result.Error)
	}
}

func TestMultiStrategy_UnknownOperation(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
		"operation": "destroy"
	}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for unknown operation")
	}
	if !contains(result.Error, "unknown operation") {
		t.Errorf("error should mention unknown operation, got: %s", result.Error)
	}
}

func TestMultiStrategy_MissingPath(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{"operation": "rewrite", "content": "hello"}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for missing path")
	}
}

func TestMultiStrategy_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{invalid`)

	_, err := m.Apply(context.Background(), input, exec)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestMultiStrategy_AllStrategiesFail(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when all strategies fail")
	}
	if result.Error == "" {
		t.Error("expected an error message listing failures")
	}
	if !contains(result.Error, "udiff") || !contains(result.Error, "search-replace") {
		t.Errorf("error should mention failed strategies, got: %s", result.Error)
	}
}

func TestMultiStrategy_PrimaryWinsOverFallback(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	content, _ := exec.ReadFile(context.Background(), "test.txt")
	if content != "line1\nline2_via_diff\nline3\n" {
		t.Errorf("primary patch should have applied, got content: %q", content)
	}
}

// helpers

func newTestExecutor(t *testing.T, dir string) executor.Executor {
	t.Helper()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}
	return exec
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
