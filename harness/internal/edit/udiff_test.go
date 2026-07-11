package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestUdiffStrategy_ToolDefinition(t *testing.T) {
	s := NewUdiffStrategy(defaultFuzzyThreshold)
	def := s.ToolDefinition()

	if def.Name != "apply_diff" {
		t.Errorf("name: got %q, want %q", def.Name, "apply_diff")
	}
	if def.Description == "" {
		t.Error("description should not be empty")
	}
	if len(def.InputSchema) == 0 {
		t.Error("input schema should not be empty")
	}

	// Verify schema contains required fields.
	var schema map[string]interface{}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema missing properties")
	}
	if _, ok := props["path"]; !ok {
		t.Error("schema missing 'path' property")
	}
	if _, ok := props["diff"]; !ok {
		t.Error("schema missing 'diff' property")
	}
}

func TestUdiffStrategy_ExactMatch_SingleHunk(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- a/file.txt
+++ b/file.txt
@@ -2,3 +2,3 @@
 line 2
-line 3
+line THREE
 line 4`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}
	if result.Path != "file.txt" {
		t.Errorf("path: got %q, want %q", result.Path, "file.txt")
	}

	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	expected := "line 1\nline 2\nline THREE\nline 4\nline 5\n"
	if content != expected {
		t.Errorf("content:\ngot:  %q\nwant: %q", content, expected)
	}
}

func TestUdiffStrategy_ExactMatch_MultiHunk(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// First hunk replaces line 2 (b->B), second hunk replaces line 8 (h->H).
	// After first hunk, line count is unchanged, so offset stays 0.
	diff := `--- a/file.txt
+++ b/file.txt
@@ -1,3 +1,3 @@
 a
-b
+B
 c
@@ -7,3 +7,3 @@
 g
-h
+H
 i`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	expected := "a\nB\nc\nd\ne\nf\ng\nH\ni\nj\n"
	if content != expected {
		t.Errorf("content:\ngot:  %q\nwant: %q", content, expected)
	}
}

func TestUdiffStrategy_MultiHunk_OffsetAdjustment(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// First hunk adds a line after b (3 old -> 4 new = +1 offset).
	// Second hunk at original line 8 should still find 'h' thanks to offset.
	diff := `--- a/file.txt
+++ b/file.txt
@@ -1,3 +1,4 @@
 a
 b
+b2
 c
@@ -7,3 +8,3 @@
 g
-h
+H
 i`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	expected := "a\nb\nb2\nc\nd\ne\nf\ng\nH\ni\nj\n"
	if content != expected {
		t.Errorf("content:\ngot:  %q\nwant: %q", content, expected)
	}
}

func TestUdiffStrategy_WhitespaceInsensitiveFallback(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	// File uses tabs, diff uses spaces — whitespace-insensitive should match.
	original := "\tline 1\n\tline 2\n\tline 3\n"
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- a/file.txt
+++ b/file.txt
@@ -1,3 +1,3 @@
   line 1
-  line 2
+  line TWO
   line 3`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// The replacement text from the diff is used (with spaces), not the
	// original file indentation. Context lines keep the original file text.
	expected := "\tline 1\n  line TWO\n\tline 3\n"
	if content != expected {
		t.Errorf("content:\ngot:  %q\nwant: %q", content, expected)
	}
}

func TestUdiffStrategy_FuzzyFallback(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	// Context lines slightly differ from the diff (within 80% similarity).
	original := "function calculateTotal(items) {\n  let total = 0;\n  for (const item of items) {\n    total += item.price;\n  }\n  return total;\n}\n"
	if err := exec.WriteFile(context.Background(), "calc.js", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// The context lines have minor differences: "calculateTotl" vs "calculateTotal"
	// but the removed/added lines should still apply.
	diff := `--- a/calc.js
+++ b/calc.js
@@ -1,5 +1,5 @@
 function calculateTotl(items) {
   let total = 0;
-  for (const item of items) {
+  for (const entry of items) {
     total += item.price;
   }`

	input, _ := json.Marshal(map[string]string{"path": "calc.js", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true via fuzzy match; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "calc.js")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(content, "for (const entry of items)") {
		t.Errorf("expected fuzzy-matched replacement; got:\n%s", content)
	}
}

func TestUdiffStrategy_FuzzyFailure_BelowThreshold(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original := "completely different content\nanother line\nmore stuff\n"
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// Context lines bear no resemblance to the actual file.
	diff := `--- a/file.txt
+++ b/file.txt
@@ -1,3 +1,3 @@
 this is nothing like the file
-some old line
+some new line
 also totally different`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false when context lines are too different")
	}
	if !strings.Contains(result.Error, "no match within lines") {
		t.Errorf("expected matching location error; got: %s", result.Error)
	}

	// Verify the file was not modified.
	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != original {
		t.Errorf("file should not have been modified; got:\n%s", content)
	}
}

// buildAnchorTestFile constructs a file with a whitespace-equal (but not
// byte-exact) duplicate of a 3-line "line A / line B / line C" pattern near
// the top, followed by fillerCount padding lines, optionally followed by
// the true (also whitespace-drifted) target block. It returns the file
// content and the 1-based line number of the true target's first line
// (valid even when includeTarget is false, for pointing a hunk header at an
// empty region).
func buildAnchorTestFile(fillerCount int, includeTarget bool) (content string, targetLine int) {
	var b strings.Builder
	b.WriteString("func header() {\n")
	b.WriteString("    line A\n") // early whitespace-equal duplicate: line 2
	b.WriteString("    line B\n") // line 3
	b.WriteString("    line C\n") // line 4
	for i := 0; i < fillerCount; i++ {
		fmt.Fprintf(&b, "    filler %d\n", i)
	}
	targetLine = 4 + fillerCount + 1
	if includeTarget {
		b.WriteString("\tline A\n")
		b.WriteString("\tline B\n")
		b.WriteString("\tline C\n")
	}
	b.WriteString("}\n")
	return b.String(), targetLine
}

// TestUdiffStrategy_AnchoredFallback_IgnoresEarlyDuplicateBlock is the
// regression test for the fallback-anchoring fix: the whitespace-
// insensitive fallback used to scan from position 0 and apply the hunk to
// the first whitespace-equal block it found, even when that block was far
// from the hunk's declared line and an equally-plausible (also drifted)
// match existed at the declared location. That silently corrupted the
// wrong region of the file while still reporting Applied=true.
func TestUdiffStrategy_AnchoredFallback_IgnoresEarlyDuplicateBlock(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original, targetLine := buildAnchorTestFile(60, true)
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// Context/removed lines match both the early duplicate and the true
	// target only after whitespace trimming (the file uses 4 spaces near
	// the top and a tab at the true target; the diff itself carries neither
	// indentation), so this can only apply via the whitespace-insensitive
	// fallback.
	diff := fmt.Sprintf(`--- a/file.txt
+++ b/file.txt
@@ -%d,3 +%d,3 @@
 line A
-line B
+line B changed
 line C`, targetLine, targetLine)

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	content, readErr := exec.ReadFile(context.Background(), "file.txt")
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}

	// The early duplicate must never be touched, whether the fix landed the
	// edit on the true target or failed closed.
	if !strings.Contains(content, "    line A\n    line B\n    line C\n") {
		t.Fatalf("early duplicate block was modified (silent wrong-region edit); got:\n%s", content)
	}

	if result.Applied {
		if !strings.Contains(content, "\tline A\nline B changed\n\tline C\n") {
			t.Errorf("expected the edit to land on the true target; got:\n%s", content)
		}
	} else if content != original {
		t.Errorf("file should be unmodified when Applied=false; got:\n%s", content)
	}
}

// TestUdiffStrategy_AnchoredFallback_SmallDriftStillSucceeds guards against
// over-tightening the anchor: a hunk whose declared line number is off by a
// few lines (typical when an earlier hunk in the same diff miscounts, or
// the model is working from a slightly stale view of the file) must still
// resolve via the whitespace-insensitive fallback as long as the true
// target is within the search window.
func TestUdiffStrategy_AnchoredFallback_SmallDriftStillSucceeds(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	// The real block sits 3 lines below where the hunk header claims it
	// starts (line 1) — comfortably inside the anchoring window.
	original := "\tpad 0\n\tpad 1\n\tpad 2\n\tline 1\n\tline 2\n\tline 3\n"
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- a/file.txt
+++ b/file.txt
@@ -1,3 +1,3 @@
   line 1
-  line 2
+  line TWO
   line 3`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected small line-number drift to still resolve; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	expected := "\tpad 0\n\tpad 1\n\tpad 2\n\tline 1\n  line TWO\n\tline 3\n"
	if content != expected {
		t.Errorf("content:\ngot:  %q\nwant: %q", content, expected)
	}
}

// TestUdiffStrategy_AnchoredFallback_OutOfRegionMatchFailsClosed covers the
// fail-closed side of the fix: when the only whitespace-equal match in the
// file is far outside the hunk's declared region, and nothing plausible
// exists near the declared line, the strategy must report Applied=false
// with an actionable error rather than reaching past the window.
func TestUdiffStrategy_AnchoredFallback_OutOfRegionMatchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	// No true target this time — the only whitespace-equal match is the
	// early duplicate, well outside the window around the declared line.
	original, targetLine := buildAnchorTestFile(60, false)
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := fmt.Sprintf(`--- a/file.txt
+++ b/file.txt
@@ -%d,3 +%d,3 @@
 line A
-line B
+line B changed
 line C`, targetLine, targetLine)

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Fatalf("expected Applied=false for an out-of-region-only match; got content modified")
	}
	if !strings.Contains(result.Error, "no match within lines") {
		t.Errorf("expected an actionable window error; got: %s", result.Error)
	}
	if !strings.Contains(result.Error, fmt.Sprintf("declared start line %d", targetLine)) {
		t.Errorf("expected the error to cite the declared start line; got: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != original {
		t.Errorf("file should be unmodified when Applied=false; got:\n%s", content)
	}
	// And, just as importantly, the early duplicate — the only match that
	// exists anywhere in the file — must be untouched.
	if !strings.Contains(content, "    line A\n    line B\n    line C\n") {
		t.Errorf("early duplicate block was modified; got:\n%s", content)
	}
}

// TestUdiffStrategy_AnchoredFuzzyFallback_IgnoresEarlyLookalike is the
// fuzzy-strategy counterpart to the whitespace-fallback regression above:
// the fuzzy scan also used to run unbounded from position 0, so an early
// block that merely resembles the pattern (rather than matching it after
// whitespace trimming) could still win fuzzy scoring over the true,
// drifted target.
func TestUdiffStrategy_AnchoredFuzzyFallback_IgnoresEarlyLookalike(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	// Both blocks are near-misses on the pattern's first context line, so
	// neither is caught by the exact or whitespace-insensitive strategies —
	// this can only resolve via fuzzy matching. The early block's line is a
	// closer (higher-scoring) match than the true target's, so an unbounded
	// scan that simply takes the best score in the file — rather than
	// respecting the declared region — picks the early block.
	var b strings.Builder
	b.WriteString("function calculateTotal(items) {\n")
	b.WriteString("  let totale = 0;\n") // 1-edit drift: higher fuzzy score
	b.WriteString("  for (const item of items) {\n")
	b.WriteString("    total += item.price;\n")
	b.WriteString("  }\n")
	const fillerCount = 60
	for i := 0; i < fillerCount; i++ {
		fmt.Fprintf(&b, "  // filler %d\n", i)
	}
	targetLine := 5 + fillerCount + 1
	b.WriteString("  let totalzz = 0;\n") // 2-edit drift: lower fuzzy score
	b.WriteString("  for (const item of items) {\n")
	b.WriteString("    total += item.price;\n")
	b.WriteString("  }\n")
	b.WriteString("  return total;\n")
	b.WriteString("}\n")
	original := b.String()

	if err := exec.WriteFile(context.Background(), "calc.js", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := fmt.Sprintf(`--- a/calc.js
+++ b/calc.js
@@ -%d,3 +%d,3 @@
   let total = 0;
-  for (const item of items) {
+  for (const entry of items) {
     total += item.price;`, targetLine, targetLine)

	input, _ := json.Marshal(map[string]string{"path": "calc.js", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	content, readErr := exec.ReadFile(context.Background(), "calc.js")
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}

	// The early block's own removed-line text ("item") must survive
	// untouched; only the true target's copy may become "entry".
	if !strings.Contains(content, "let totale = 0;\n  for (const item of items) {") {
		t.Fatalf("early lookalike block was modified (silent wrong-region edit); got:\n%s", content)
	}
	if result.Applied && !strings.Contains(content, "for (const entry of items)") {
		t.Errorf("expected the edit to land on the true target; got:\n%s", content)
	}
	if !result.Applied && content != original {
		t.Errorf("file should be unmodified when Applied=false; got:\n%s", content)
	}
}

func TestUdiffStrategy_NewFileCreation(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- /dev/null
+++ b/new_file.txt
@@ -0,0 +1,3 @@
+first line
+second line
+third line`

	input, _ := json.Marshal(map[string]string{"path": "new_file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true for new file; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "new_file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	expected := "first line\nsecond line\nthird line\n"
	if content != expected {
		t.Errorf("content:\ngot:  %q\nwant: %q", content, expected)
	}
}

func TestUdiffStrategy_FileDeletion(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original := "line 1\nline 2\nline 3\n"
	if err := exec.WriteFile(context.Background(), "doomed.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- a/doomed.txt
+++ /dev/null
@@ -1,3 +0,0 @@
-line 1
-line 2
-line 3`

	input, _ := json.Marshal(map[string]string{"path": "doomed.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true for deletion; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "doomed.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty file after deletion; got: %q", content)
	}
}

func TestUdiffStrategy_HunkLineMismatch(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	if err := exec.WriteFile(context.Background(), "file.txt", "a\nb\nc\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// Header claims 5 old lines but body only has 3.
	diff := `--- a/file.txt
+++ b/file.txt
@@ -1,5 +1,3 @@
 a
-b
+B
 c`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for mismatched line counts")
	}
	if !strings.Contains(result.Error, "parse diff") {
		t.Errorf("expected parse diff error; got: %s", result.Error)
	}
}

func TestUdiffStrategy_MalformedDiff(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	if err := exec.WriteFile(context.Background(), "file.txt", "content\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)

	tests := []struct {
		name string
		diff string
		want string // substring expected in error
	}{
		{
			name: "no hunks",
			diff: "just some random text\nwith no hunk headers",
			want: "no hunks",
		},
		{
			name: "malformed hunk header",
			diff: "@@ this is not valid\n+added",
			want: "parse diff",
		},
		{
			name: "empty diff",
			diff: "",
			want: "diff is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": tc.diff})
			result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if result.Applied {
				t.Error("expected Applied=false for malformed diff")
			}
			if !strings.Contains(result.Error, tc.want) {
				t.Errorf("expected error containing %q; got: %s", tc.want, result.Error)
			}
		})
	}
}

func TestUdiffStrategy_MissingPath(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{"diff": "@@ -1,1 +1,1 @@\n-old\n+new"}`)

	result, err := s.Apply(context.Background(), input, exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for missing path")
	}
	if result.Error == "" {
		t.Error("expected an error message")
	}
}

func TestUdiffStrategy_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	_, err = s.Apply(context.Background(), json.RawMessage(`{invalid`), exec)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestUdiffStrategy_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- a/missing.txt
+++ b/missing.txt
@@ -1,1 +1,1 @@
-old
+new`

	input, _ := json.Marshal(map[string]string{"path": "missing.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Error("expected Applied=false for nonexistent file")
	}
	if !strings.Contains(result.Error, "read file") {
		t.Errorf("expected read file error; got: %s", result.Error)
	}
}

// Internal unit tests for helper functions.

func TestParseHunkHeader(t *testing.T) {
	tests := []struct {
		input    string
		wantOldS int
		wantOldC int
		wantNewS int
		wantNewC int
		wantErr  bool
	}{
		{"@@ -1,4 +1,6 @@", 1, 4, 1, 6, false},
		{"@@ -10,3 +12,5 @@ func main()", 10, 3, 12, 5, false},
		{"@@ -1 +1 @@", 1, 1, 1, 1, false},
		{"@@ -0,0 +1,3 @@", 0, 0, 1, 3, false},
		{"@@ invalid @@", 0, 0, 0, 0, true},
		{"@@ -1,4 @@", 0, 0, 0, 0, true},
		{"not a header", 0, 0, 0, 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			h, err := parseHunkHeader(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h.oldStart != tc.wantOldS || h.oldCount != tc.wantOldC {
				t.Errorf("old: got %d,%d want %d,%d", h.oldStart, h.oldCount, tc.wantOldS, tc.wantOldC)
			}
			if h.newStart != tc.wantNewS || h.newCount != tc.wantNewC {
				t.Errorf("new: got %d,%d want %d,%d", h.newStart, h.newCount, tc.wantNewS, tc.wantNewC)
			}
		})
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"kitten", "sitting", 3},
		{"", "abc", 3},
		{"abc", "", 3},
	}

	for _, tc := range tests {
		got := levenshtein(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestLineSimilarity(t *testing.T) {
	tests := []struct {
		a, b    string
		wantMin float64
		wantMax float64
	}{
		{"abc", "abc", 1.0, 1.0},
		{"", "", 1.0, 1.0},
		{"abc", "xyz", 0.0, 0.01},
		{"calculateTotal", "calculateTotl", 0.9, 1.0},
	}

	for _, tc := range tests {
		got := lineSimilarity(tc.a, tc.b)
		if got < tc.wantMin || got > tc.wantMax {
			t.Errorf("lineSimilarity(%q, %q) = %f, want [%f, %f]",
				tc.a, tc.b, got, tc.wantMin, tc.wantMax)
		}
	}
}

func TestUdiffStrategy_PureAddition(t *testing.T) {
	dir := t.TempDir()
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	original := "line 1\nline 2\nline 3\n"
	if err := exec.WriteFile(context.Background(), "file.txt", original); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// Hunk that only adds lines (no context, no removals).
	diff := `--- a/file.txt
+++ b/file.txt
@@ -2,0 +3,2 @@
+inserted 1
+inserted 2`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected Applied=true; error: %s", result.Error)
	}

	content, err := exec.ReadFile(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(content, "inserted 1") || !strings.Contains(content, "inserted 2") {
		t.Errorf("expected inserted lines; got:\n%s", content)
	}
}
