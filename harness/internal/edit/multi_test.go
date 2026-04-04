package edit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

	// Verify the schema is valid JSON containing expected fields.
	var schema map[string]interface{}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema missing properties")
	}
	for _, field := range []string{"path", "content", "diff", "old_string", "new_string"} {
		if _, exists := props[field]; !exists {
			t.Errorf("schema missing property %q", field)
		}
	}
}

func TestMultiStrategy_RoutesToUdiff(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	// Seed a file so the diff has something to apply against.
	writeTestFile(t, dir, "test.txt", "line1\nline2\nline3\n")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
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

func TestMultiStrategy_RoutesToSearchReplace(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "hello world")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "test.txt",
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

func TestMultiStrategy_RoutesToWholeFile(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{
		"path": "new.txt",
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

func TestMultiStrategy_FallbackOnUdiffFailure(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	// Seed a file whose content does not match the diff's context lines,
	// so udiff will fail, but whole-file will succeed as fallback.
	writeTestFile(t, dir, "test.txt", "original content")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// Provide both diff (which will fail) and content (which will succeed).
	input := json.RawMessage(`{
		"path": "test.txt",
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

func TestMultiStrategy_FallbackSearchReplaceToWholeFile(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "hello world")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// old_string won't be found, so search-replace fails; content fallback succeeds.
	input := json.RawMessage(`{
		"path": "test.txt",
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

func TestMultiStrategy_NoApplicableStrategy(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// Only path provided, no strategy-specific fields.
	input := json.RawMessage(`{"path": "test.txt"}`)

	result, err := m.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when no strategy fields present")
	}
	if result.Error == "" {
		t.Error("expected an error message")
	}
}

func TestMultiStrategy_MissingPath(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	m := NewMultiStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{"content": "hello"}`)

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
	// diff will fail (bad context), old_string will fail (not found).
	// No content field, so no whole-file fallback.
	input := json.RawMessage(`{
		"path": "test.txt",
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
	// Error should mention both strategies.
	if !contains(result.Error, "udiff") || !contains(result.Error, "search-replace") {
		t.Errorf("error should mention failed strategies, got: %s", result.Error)
	}
}

func TestMultiStrategy_PriorityOrder(t *testing.T) {
	dir := t.TempDir()
	exec := newTestExecutor(t, dir)

	writeTestFile(t, dir, "test.txt", "line1\nline2\nline3\n")

	m := NewMultiStrategy(defaultFuzzyThreshold)
	// Provide both diff and content. Udiff should be tried first and succeed,
	// so whole-file should never be reached.
	input := json.RawMessage(`{
		"path": "test.txt",
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
		t.Errorf("udiff should have been preferred, got content: %q", content)
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
