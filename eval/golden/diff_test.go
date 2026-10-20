package golden

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDiff_CreateModifyDelete(t *testing.T) {
	must := require.New(t)
	diff := `diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+one
+two
diff --git a/mod.txt b/mod.txt
index 2222222..3333333 100644
--- a/mod.txt
+++ b/mod.txt
@@ -1,3 +1,3 @@
 keep
-old
+new
 tail
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 4444444..0000000
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
`
	files, err := ParseDiff(diff)
	must.NoError(err)
	wantBefore := Tree{"mod.txt": "keep\nold\ntail\n", "gone.txt": "bye\n"}
	wantAfter := Tree{"new.txt": "one\ntwo\n", "mod.txt": "keep\nnew\ntail\n"}
	must.Equal(wantBefore, files.Before)
	must.Equal(wantAfter, files.After)
}

func TestParseDiff_FillsUnshownLines(t *testing.T) {
	must := require.New(t)
	diff := `--- a/f.txt
+++ b/f.txt
@@ -4,3 +4,4 @@
 c4
+added
 c5
 c6
@@ -10,2 +11,1 @@
 c10
-c11
@@ -20,0 +21,1 @@
+appended
`
	files, err := ParseDiff(diff)
	must.NoError(err)
	before := strings.Split(strings.TrimSuffix(files.Before["f.txt"], "\n"), "\n")
	after := strings.Split(strings.TrimSuffix(files.After["f.txt"], "\n"), "\n")
	must.Len(before, 20, "before line count")
	must.Len(after, 21, "after line count")
	checks := []struct {
		name  string
		lines []string
		line  int
		want  string
	}{
		{"before line 1 is blank", before, 1, ""},
		{"before line 4 is context c4", before, 4, "c4"},
		{"before line 6 is context c6", before, 6, "c6"},
		{"before line 7 is blank", before, 7, ""},
		{"before line 10 is context c10", before, 10, "c10"},
		{"before line 11 is removed c11", before, 11, "c11"},
		{"before line 20 is blank", before, 20, ""},
		{"after line 5 is added", after, 5, "added"},
		{"after line 11 is context c10", after, 11, "c10"},
		{"after line 12 is blank", after, 12, ""},
		{"after line 20 is blank", after, 20, ""},
		{"after line 21 is appended", after, 21, "appended"},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(c.want, c.lines[c.line-1])
		})
	}
}

func TestParseDiff_NoNewlineAtEndOfFile(t *testing.T) {
	cases := map[string]struct {
		diff          string
		before, after string
	}{
		"added newline": {
			diff:   "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n\\ No newline at end of file\n+x\n",
			before: "x", after: "x\n",
		},
		"removed newline": {
			diff:   "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n+x\n\\ No newline at end of file\n",
			before: "x\n", after: "x",
		},
		"neither side": {
			diff:   "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n-a\n+b\n z\n\\ No newline at end of file\n",
			before: "a\nz", after: "b\nz",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			files, err := ParseDiff(tc.diff)
			must.NoError(err)
			must.Equal(tc.before, files.Before["f"])
			must.Equal(tc.after, files.After["f"])
		})
	}
}

func TestParseDiff_EmptyContextLineIsAccepted(t *testing.T) {
	must := require.New(t)
	files, err := ParseDiff("--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n")
	must.NoError(err)
	must.Equal("a\n\nb\n", files.Before["f"])
	must.Equal("a\n\nc\n", files.After["f"])
}

func TestParseDiff_Rejects(t *testing.T) {
	cases := map[string]struct{ diff, want string }{
		"empty":           {"", "no file sections"},
		"prose":           {"this is not a diff\n", "unexpected"},
		"context only":    {"--- a/f\n+++ b/f\n@@ -1 +1 @@\n same\n", "changes nothing"},
		"rename":          {"diff --git a/x b/y\nsimilarity index 90%\nrename from x\nrename to y\n", "renames"},
		"renamed paths":   {"--- a/x\n+++ b/y\n@@ -1 +1 @@\n-a\n+b\n", "renames are not supported"},
		"binary":          {"diff --git a/x b/x\nindex 1..2 100644\nBinary files a/x and b/x differ\n", "binary"},
		"mode only":       {"diff --git a/x b/x\nold mode 100644\nnew mode 100755\n", "no ---/+++"},
		"no hunks":        {"--- a/x\n+++ b/x\n", "no hunks"},
		"no plus line":    {"--- a/x\n@@ -1 +1 @@\n", "not followed by +++"},
		"both dev null":   {"--- /dev/null\n+++ /dev/null\n@@ -0,0 +1 @@\n+a\n", "both sides"},
		"too few lines":   {"--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n a\n-b\n+c\n", "ends before"},
		"too many lines":  {"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n+c\n", "unexpected"},
		"bad line":        {"--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a\n*b\n", "ends early"},
		"bad header":      {"--- a/x\n+++ b/x\n@@ -a +1 @@\n", "malformed hunk header"},
		"zero start":      {"--- a/x\n+++ b/x\n@@ -0,1 +1 @@\n-a\n+b\n", "line 0 must be empty"},
		"overlap":         {"--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n@@ -2 +2 @@\n-b\n+d\n", "overlaps"},
		"new start drift": {"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n@@ -5 +9 @@\n-e\n+f\n", "starts at new line 9"},
		"create removes":  {"--- /dev/null\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n", "creates"},
		"delete adds":     {"--- a/x\n+++ /dev/null\n@@ -1 +1 @@\n-a\n+b\n", "deletes"},
		"duplicate file":  {"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n--- a/x\n+++ b/x\n@@ -3 +3 @@\n-c\n+d\n", "more than one section"},
		"misplaced eol":   {"--- a/x\n+++ b/x\n@@ -1 +1 @@\n\\ No newline at end of file\n-a\n+b\n", "misplaced"},
		"eol mid-file":    {"--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-a\n\\ No newline at end of file\n-b\n+c\n+d\n", "not the file's last"},
		"traversal":       {"--- a/../x\n+++ b/../x\n@@ -1 +1 @@\n-a\n+b\n", "inside the workspace"},
		"git dir":         {"--- a/.git/config\n+++ b/.git/config\n@@ -1 +1 @@\n-a\n+b\n", "outside .git"},
		"absolute":        {"--- /etc/passwd\n+++ /etc/passwd\n@@ -1 +1 @@\n-a\n+b\n", `does not start with "a/"`},
		"unclean":         {"--- a/x//y\n+++ b/x//y\n@@ -1 +1 @@\n-a\n+b\n", "clean relative path"},
		"quoted":          {"--- \"a/x y\"\n+++ \"b/x y\"\n@@ -1 +1 @@\n-a\n+b\n", "quoted path"},
		"start overflows": {"--- a/x\n+++ b/x\n@@ -99999999999999999999 +1 @@\n-a\n+b\n", "hunk start"},
		"count overflows": {"--- a/x\n+++ b/x\n@@ -1,99999999999999999999 +1 @@\n-a\n+b\n", "hunk count"},
		"new start bad":   {"--- a/x\n+++ b/x\n@@ -1 +0,1 @@\n-a\n+b\n", "line 0 must be empty"},
		"bad new prefix":  {"--- a/x\n+++ c/x\n@@ -1 +1 @@\n-a\n+b\n", `does not start with "b/"`},
		"headers only":    {"diff --git a/x b/x\nindex 1..2 100644\n", "the last section has no ---/+++"},
		"two headers":     {"diff --git a/x b/x\ndiff --git a/y b/y\n--- a/y\n+++ b/y\n@@ -1 +1 @@\n-a\n+b\n", "the previous section has no ---/+++"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := ParseDiff(tc.diff)
			must.ErrorContains(err, tc.want)
		})
	}
}

func TestParseDiff_BoundsTheReconstruction(t *testing.T) {
	section := func(path string, start int) string {
		return fmt.Sprintf("--- a/%s\n+++ b/%s\n@@ -%d +%d @@\n-a\n+b\n", path, path, start, start)
	}
	cases := map[string]struct{ diff, want string }{
		"start past the limit": {section("x", 10_000_000), "exceeds the 8388608-byte limit"},
		"start at int max":     {"--- a/x\n+++ b/x\n@@ -9223372036854775807,0 +9223372036854775807 @@\n+b\n", "exceeds the 8388608-byte limit"},
		"count past the limit": {"--- a/x\n+++ b/x\n@@ -1,9000000 +1 @@\n-a\n+b\n", "exceeds the 8388608-byte limit"},
		"padding over budget":  {section("x", 5_000_000), "reconstructed files exceed 8388608 bytes"},
		"budget across files":  {section("x", 2_500_000) + section("y", 2_500_000), "y: reconstructed files exceed"},
		"drift before padding": {"--- a/x\n+++ b/x\n@@ -4000000 +1 @@\n-a\n+b\n", "starts at new line 1, but the hunks before it put it at 4000000"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := ParseDiff(tc.diff)
			must.ErrorContains(err, tc.want)
		})
	}
}

func TestParseDiff_PaddingWithinBudget(t *testing.T) {
	must := require.New(t)
	files, err := ParseDiff("--- a/x\n+++ b/x\n@@ -1000000 +1000000 @@\n-a\n+b\n")
	must.NoError(err)
	want := strings.Repeat("\n", 999_999) + "a\n"
	must.Len(files.Before["x"], len(want), "before size in bytes")
	must.Equal(want, files.Before["x"])
}

func TestParseDiff_TimestampedPaths(t *testing.T) {
	must := require.New(t)
	files, err := ParseDiff("--- a/x\t2026-01-01 00:00:00\n+++ b/x\t2026-01-02 00:00:00\n@@ -1 +1 @@\n-a\n+b\n")
	must.NoError(err)
	must.Equal("b\n", files.After["x"])
}
