package golden

import (
	"strings"
	"testing"
)

func TestParseDiff_CreateModifyDelete(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	wantBefore := Tree{"mod.txt": "keep\nold\ntail\n", "gone.txt": "bye\n"}
	wantAfter := Tree{"new.txt": "one\ntwo\n", "mod.txt": "keep\nnew\ntail\n"}
	if !treesEqual(files.Before, wantBefore) || !treesEqual(files.After, wantAfter) {
		t.Errorf("files = %#v\nwant before %#v\nwant after %#v", files, wantBefore, wantAfter)
	}
}

func TestParseDiff_FillsUnshownLines(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	before := strings.Split(strings.TrimSuffix(files.Before["f.txt"], "\n"), "\n")
	after := strings.Split(strings.TrimSuffix(files.After["f.txt"], "\n"), "\n")
	if len(before) != 20 || len(after) != 21 {
		t.Fatalf("before has %d lines, after %d; want 20 and 21", len(before), len(after))
	}
	checks := []struct {
		lines []string
		line  int
		want  string
	}{
		{before, 1, ""}, {before, 4, "c4"}, {before, 6, "c6"}, {before, 7, ""}, {before, 10, "c10"}, {before, 11, "c11"}, {before, 20, ""},
		{after, 5, "added"}, {after, 11, "c10"}, {after, 12, ""}, {after, 20, ""}, {after, 21, "appended"},
	}
	for _, c := range checks {
		if got := c.lines[c.line-1]; got != c.want {
			t.Errorf("line %d = %q, want %q", c.line, got, c.want)
		}
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
			files, err := ParseDiff(tc.diff)
			if err != nil {
				t.Fatalf("ParseDiff: %v", err)
			}
			if files.Before["f"] != tc.before || files.After["f"] != tc.after {
				t.Errorf("before %q after %q, want %q and %q", files.Before["f"], files.After["f"], tc.before, tc.after)
			}
		})
	}
}

func TestParseDiff_EmptyContextLineIsAccepted(t *testing.T) {
	files, err := ParseDiff("--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n")
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	if files.Before["f"] != "a\n\nb\n" || files.After["f"] != "a\n\nc\n" {
		t.Errorf("files = %#v", files)
	}
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
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDiff(tc.diff)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseDiff error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseDiff_TimestampedPaths(t *testing.T) {
	files, err := ParseDiff("--- a/x\t2026-01-01 00:00:00\n+++ b/x\t2026-01-02 00:00:00\n@@ -1 +1 @@\n-a\n+b\n")
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	if files.After["x"] != "b\n" {
		t.Errorf("files = %#v", files)
	}
}
