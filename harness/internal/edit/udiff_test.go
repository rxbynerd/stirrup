package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestUdiffStrategy_ToolDefinition(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	s := NewUdiffStrategy(defaultFuzzyThreshold)
	def := s.ToolDefinition()

	check.Equal("apply_diff", def.Name)
	check.NotEmpty(def.Description)
	check.NotEmpty(def.InputSchema)

	var schema map[string]interface{}
	must.NoError(json.Unmarshal(def.InputSchema, &schema))
	props, ok := schema["properties"].(map[string]interface{})
	must.True(ok, "schema missing properties")
	check.Contains(props, "path")
	check.Contains(props, "diff")
}

func TestUdiffStrategy_ExactMatch_SingleHunk(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	original := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

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
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)
	check.Equal("file.txt", result.Path)

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	must.Equal("line 1\nline 2\nline THREE\nline 4\nline 5\n", content)
}

func TestUdiffStrategy_ExactMatch_MultiHunk(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	original := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

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
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	must.Equal("a\nB\nc\nd\ne\nf\ng\nH\ni\nj\n", content)
}

func TestUdiffStrategy_MultiHunk_OffsetAdjustment(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	original := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

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
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	must.Equal("a\nb\nb2\nc\nd\ne\nf\ng\nH\ni\nj\n", content)
}

func TestUdiffStrategy_WhitespaceInsensitiveFallback(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	// File uses tabs, diff uses spaces — whitespace-insensitive should match.
	original := "\tline 1\n\tline 2\n\tline 3\n"
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

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
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	// The replacement text from the diff is used (with spaces), not the
	// original file indentation. Context lines keep the original file text.
	must.Equal("\tline 1\n  line TWO\n\tline 3\n", content)
}

func TestUdiffStrategy_FuzzyFallback(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	// Context lines slightly differ from the diff (within 80% similarity).
	original := "function calculateTotal(items) {\n  let total = 0;\n  for (const item of items) {\n    total += item.price;\n  }\n  return total;\n}\n"
	must.NoError(exec.WriteFile(context.Background(), "calc.js", original))

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
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true via fuzzy match; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "calc.js")
	must.NoError(err)
	must.Contains(content, "for (const entry of items)")
}

func TestUdiffStrategy_FuzzyFailure_BelowThreshold(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	original := "completely different content\nanother line\nmore stuff\n"
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

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
	must.NoError(err)
	check.False(result.Applied, "context lines are too different to match")
	check.Contains(result.Error, "no match within lines")

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	must.Equal(original, content)
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

// TestUdiffStrategy_AnchoredFallback_IgnoresEarlyDuplicateBlock pins that
// the whitespace-insensitive fallback must not apply a hunk to an early
// whitespace-equal block far from the hunk's declared line when a
// plausible match exists near the declared location.
func TestUdiffStrategy_AnchoredFallback_IgnoresEarlyDuplicateBlock(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	original, targetLine := buildAnchorTestFile(60, true)
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// The file uses 4 spaces near the top and a tab at the true target;
	// the diff carries neither, so this can only resolve via the
	// whitespace-insensitive fallback.
	diff := fmt.Sprintf(`--- a/file.txt
+++ b/file.txt
@@ -%d,3 +%d,3 @@
 line A
-line B
+line B changed
 line C`, targetLine, targetLine)

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	must.NoError(err)

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)

	// The early duplicate must never be touched, whether the fix landed the
	// edit on the true target or failed closed.
	must.Contains(content, "    line A\n    line B\n    line C\n",
		"early duplicate block was modified (silent wrong-region edit)")

	if result.Applied {
		must.Contains(content, "\tline A\nline B changed\n\tline C\n",
			"expected the edit to land on the true target")
	} else {
		must.Equal(original, content, "file should be unmodified when Applied=false")
	}
}

// TestUdiffStrategy_AnchoredFallback_SmallDriftStillSucceeds pins that a
// hunk whose declared line number is off by a few lines must still
// resolve via the whitespace-insensitive fallback when the true target
// is within the search window.
func TestUdiffStrategy_AnchoredFallback_SmallDriftStillSucceeds(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	// The real block sits 3 lines below the declared start (line 1),
	// inside the anchoring window.
	original := "\tpad 0\n\tpad 1\n\tpad 2\n\tline 1\n\tline 2\n\tline 3\n"
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

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
	must.NoError(err)
	must.True(result.Applied, "expected small line-number drift to still resolve; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	must.Equal("\tpad 0\n\tpad 1\n\tpad 2\n\tline 1\n  line TWO\n\tline 3\n", content)
}

// TestUdiffStrategy_AnchoredFallback_OutOfRegionMatchFailsClosed pins the
// fail-closed side: when the only whitespace-equal match is far outside
// the hunk's declared region, the strategy must report Applied=false
// with an actionable error rather than reaching past the window.
func TestUdiffStrategy_AnchoredFallback_OutOfRegionMatchFailsClosed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	// No true target: the only whitespace-equal match is the early
	// duplicate, outside the window around the declared line.
	original, targetLine := buildAnchorTestFile(60, false)
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

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
	must.NoError(err)
	must.False(result.Applied, "expected Applied=false for an out-of-region-only match")
	check.Contains(result.Error, "no match within lines")
	check.Contains(result.Error, fmt.Sprintf("declared start line %d", targetLine))

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	check.Equal(original, content, "file should be unmodified when Applied=false")
	// The early duplicate is the only match in the file and must stay untouched.
	must.Contains(content, "    line A\n    line B\n    line C\n")
}

// TestUdiffStrategy_AnchoredFuzzyFallback_IgnoresEarlyLookalike is the
// fuzzy-strategy counterpart to the whitespace-fallback test above: an
// early block that merely resembles the pattern must not outscore the
// true, drifted target outside the search window.
func TestUdiffStrategy_AnchoredFuzzyFallback_IgnoresEarlyLookalike(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	// Both blocks are near-misses on the pattern's first context line, so
	// this can only resolve via fuzzy matching. The early block scores
	// higher than the true target, so an unbounded scan would pick it.
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

	must.NoError(exec.WriteFile(context.Background(), "calc.js", original))

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
	must.NoError(err)

	content, err := exec.ReadFile(context.Background(), "calc.js")
	must.NoError(err)

	// The early block's own removed-line text ("item") must survive
	// untouched; only the true target's copy may become "entry".
	must.Contains(content, "let totale = 0;\n  for (const item of items) {",
		"early lookalike block was modified (silent wrong-region edit)")
	if result.Applied {
		must.Contains(content, "for (const entry of items)",
			"expected the edit to land on the true target")
	} else {
		must.Equal(original, content, "file should be unmodified when Applied=false")
	}
}

func TestUdiffStrategy_NewFileCreation(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- /dev/null
+++ b/new_file.txt
@@ -0,0 +1,3 @@
+first line
+second line
+third line`

	input, _ := json.Marshal(map[string]string{"path": "new_file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true for new file; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "new_file.txt")
	must.NoError(err)
	must.Equal("first line\nsecond line\nthird line\n", content)
}

func TestUdiffStrategy_FileDeletion(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	original := "line 1\nline 2\nline 3\n"
	must.NoError(exec.WriteFile(context.Background(), "doomed.txt", original))

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- a/doomed.txt
+++ /dev/null
@@ -1,3 +0,0 @@
-line 1
-line 2
-line 3`

	input, _ := json.Marshal(map[string]string{"path": "doomed.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true for deletion; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "doomed.txt")
	must.NoError(err)
	must.Empty(content, "expected empty file after deletion")
}

func TestUdiffStrategy_HunkLineMismatch(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	must.NoError(exec.WriteFile(context.Background(), "file.txt", "a\nb\nc\n"))

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
	must.NoError(err)
	check.False(result.Applied, "expected Applied=false for mismatched line counts")
	must.Contains(result.Error, "parse diff")
}

func TestUdiffStrategy_MalformedDiff(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)
	must.NoError(exec.WriteFile(context.Background(), "file.txt", "content\n"))

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
			must := require.New(t)
			input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": tc.diff})
			result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
			must.NoError(err)
			must.False(result.Applied)
			must.Contains(result.Error, tc.want)
		})
	}
}

func TestUdiffStrategy_MissingPath(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	input := json.RawMessage(`{"diff": "@@ -1,1 +1,1 @@\n-old\n+new"}`)

	result, err := s.Apply(context.Background(), input, exec)
	must.NoError(err)
	must.False(result.Applied)
	must.NotEmpty(result.Error)
}

func TestUdiffStrategy_InvalidJSON(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	_, err = s.Apply(context.Background(), json.RawMessage(`{invalid`), exec)
	must.Error(err)
}

func TestUdiffStrategy_NonexistentFile(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	diff := `--- a/missing.txt
+++ b/missing.txt
@@ -1,1 +1,1 @@
-old
+new`

	input, _ := json.Marshal(map[string]string{"path": "missing.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	must.NoError(err)
	must.False(result.Applied)
	must.Contains(result.Error, "read file")
}

func TestParseHunkHeader_ParsesRangesAndRejectsMalformed(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantOldS int
		wantOldC int
		wantNewS int
		wantNewC int
		wantErr  bool
	}{
		{"both ranges with counts", "@@ -1,4 +1,6 @@", 1, 4, 1, 6, false},
		{"trailing function context", "@@ -10,3 +12,5 @@ func main()", 10, 3, 12, 5, false},
		{"omitted counts default to one", "@@ -1 +1 @@", 1, 1, 1, 1, false},
		{"empty old range", "@@ -0,0 +1,3 @@", 0, 0, 1, 3, false},
		{"non-numeric ranges", "@@ invalid @@", 0, 0, 0, 0, true},
		{"missing new range", "@@ -1,4 @@", 0, 0, 0, 0, true},
		{"no hunk marker", "not a header", 0, 0, 0, 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			h, err := parseHunkHeader(tc.input)
			if tc.wantErr {
				must.Error(err)
				return
			}
			must.NoError(err)
			must.Equal(tc.wantOldS, h.oldStart, "old start")
			must.Equal(tc.wantOldC, h.oldCount, "old count")
			must.Equal(tc.wantNewS, h.newStart, "new start")
			must.Equal(tc.wantNewC, h.newCount, "new count")
		})
	}
}

func TestLevenshtein_EditDistance(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"both empty", "", "", 0},
		{"identical strings", "abc", "abc", 0},
		{"single substitution", "abc", "abd", 1},
		{"kitten to sitting", "kitten", "sitting", 3},
		{"empty first operand", "", "abc", 3},
		{"empty second operand", "abc", "", 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, levenshtein(tc.a, tc.b))
		})
	}
}

func TestLineSimilarity_ScoreBounds(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		wantMin float64
		wantMax float64
	}{
		{"identical lines", "abc", "abc", 1.0, 1.0},
		{"both empty", "", "", 1.0, 1.0},
		{"disjoint lines", "abc", "xyz", 0.0, 0.01},
		{"one character dropped", "calculateTotal", "calculateTotl", 0.9, 1.0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := lineSimilarity(tc.a, tc.b)
			must.GreaterOrEqual(got, tc.wantMin)
			must.LessOrEqual(got, tc.wantMax)
		})
	}
}

func TestUdiffStrategy_PureAddition_InsertsLines(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)

	original := "line 1\nline 2\nline 3\n"
	must.NoError(exec.WriteFile(context.Background(), "file.txt", original))

	s := NewUdiffStrategy(defaultFuzzyThreshold)
	// Hunk that only adds lines (no context, no removals).
	diff := `--- a/file.txt
+++ b/file.txt
@@ -2,0 +3,2 @@
+inserted 1
+inserted 2`

	input, _ := json.Marshal(map[string]string{"path": "file.txt", "diff": diff})
	result, err := s.Apply(context.Background(), json.RawMessage(input), exec)
	must.NoError(err)
	must.True(result.Applied, "expected Applied=true; error: %s", result.Error)

	content, err := exec.ReadFile(context.Background(), "file.txt")
	must.NoError(err)
	must.Contains(content, "inserted 1")
	must.Contains(content, "inserted 2")
}
