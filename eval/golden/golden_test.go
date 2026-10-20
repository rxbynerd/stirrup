package golden

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const validDiff = "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-old\n+new\n"

func validCase(id string) Case {
	return Case{ID: id, Criteria: "f.txt says new", Diff: validDiff, Label: "pass"}
}

func encodeSet(t *testing.T, cases ...Case) []byte {
	t.Helper()
	must := require.New(t)
	data, err := json.Marshal(Set{Version: FormatVersion, Name: "t", Cases: cases})
	must.NoError(err)
	return data
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	must := require.New(t)
	must.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	must.NoError(os.WriteFile(path, []byte(content), 0o644))
}

func TestLoad_ResolvesDiffAndWorkspaceCases(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fixtures/ws/before/a.txt"), "one\n")
	writeFile(t, filepath.Join(dir, "fixtures/ws/before/gone.txt"), "bye\n")
	writeFile(t, filepath.Join(dir, "fixtures/ws/after/a.txt"), "one\ntwo\n")
	writeFile(t, filepath.Join(dir, "fixtures/ws/after/sub/new.txt"), "hi\n")
	adversarial := validCase("adv")
	adversarial.Label, adversarial.InjectionTarget, adversarial.Tags = "fail", "pass", []string{TagAdversarial, "planted-verdict"}
	fixture := Case{ID: "ws", Criteria: "c", Workspace: "fixtures/ws", Label: "fail", Notes: "n"}
	path := filepath.Join(dir, "set.json")
	writeFile(t, path, string(encodeSet(t, validCase("plain"), adversarial, fixture)))

	s, err := Load(path)
	must.NoError(err)
	must.Len(s.Cases, 3)
	must.False(s.Cases[0].Adversarial())
	must.True(s.Cases[1].Adversarial())

	t.Run("diff case", func(t *testing.T) {
		must := require.New(t)
		files, err := s.Files(s.Cases[0])
		must.NoError(err)
		must.Equal("old\n", files.Before["f.txt"])
		must.Equal("new\n", files.After["f.txt"])
	})

	t.Run("workspace case", func(t *testing.T) {
		must := require.New(t)
		files, err := s.Files(s.Cases[2])
		must.NoError(err)
		must.Equal(Tree{"a.txt": "one\n", "gone.txt": "bye\n"}, files.Before)
		must.Equal(Tree{"a.txt": "one\ntwo\n", "sub/new.txt": "hi\n"}, files.After)
	})
}

func TestParse_RejectsInvalidCases(t *testing.T) {
	cases := map[string]struct {
		mutate func(c *Case)
		want   string
	}{
		"bad id":                {func(c *Case) { c.ID = "Has Space" }, "id must match"},
		"empty criteria":        {func(c *Case) { c.Criteria = "  " }, "criteria is required"},
		"bad label":             {func(c *Case) { c.Label = "maybe" }, "label"},
		"bad tag":               {func(c *Case) { c.Tags = []string{"Bad Tag"} }, "tag"},
		"duplicate tag":         {func(c *Case) { c.Tags = []string{"go", "go"} }, "duplicate tag"},
		"adversarial no target": {func(c *Case) { c.Tags = []string{TagAdversarial} }, "needs injectionTarget"},
		"target equals label": {func(c *Case) {
			c.Tags, c.InjectionTarget = []string{TagAdversarial}, c.Label
		}, "must differ from label"},
		"target without tag":  {func(c *Case) { c.InjectionTarget = "fail" }, "only valid with"},
		"diff and workspace":  {func(c *Case) { c.Workspace = "ws" }, "exactly one of diff and workspace"},
		"neither":             {func(c *Case) { c.Diff = "" }, "exactly one of diff and workspace"},
		"unparseable diff":    {func(c *Case) { c.Diff = "nonsense\n" }, "diff:"},
		"workspace traversal": {func(c *Case) { c.Diff, c.Workspace = "", "../elsewhere" }, "workspace:"},
		"missing workspace":   {func(c *Case) { c.Diff, c.Workspace = "", "absent" }, "workspace: stat"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			c := validCase("c1")
			tc.mutate(&c)
			_, err := Parse(encodeSet(t, c), t.TempDir())
			must.ErrorContains(err, "case 1 (", "error should name the failing case")
			must.ErrorContains(err, tc.want)
		})
	}
}

func TestParse_RejectsInvalidSets(t *testing.T) {
	cases := map[string]struct{ data, want string }{
		"version":       {`{"version":2,"name":"t","cases":[]}`, "version 2"},
		"no cases":      {`{"version":1,"name":"t","cases":[]}`, "no cases"},
		"unknown field": {`{"version":1,"name":"t","cases":[{"id":"a","criteria":"c","diff":"x","lable":"pass"}]}`, `unknown field "lable"`},
		"trailing data": {`{"version":1,"name":"t","cases":[]} {}`, "data after"},
		"not json":      {`version: 1`, "decoding golden set"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := Parse([]byte(tc.data), t.TempDir())
			must.ErrorContains(err, tc.want)
		})
	}
}

func TestParse_ReportsEveryInvalidCase(t *testing.T) {
	must := require.New(t)
	first, second := validCase("dup"), validCase("dup")
	second.Label = "maybe"
	_, err := Parse(encodeSet(t, first, second), t.TempDir())
	must.Error(err, "Parse accepted an invalid set")
	for name, want := range map[string]string{
		"duplicate id":  `case 2: duplicate id "dup"`,
		"invalid label": `case 2 ("dup"): label`,
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.ErrorContains(err, want)
		})
	}
}

func TestFiles_WorkspaceFixtureRules(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "same/before/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "same/after/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "nested-git/before/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "nested-git/after/.git/config"), "[core]\n")
	writeFile(t, filepath.Join(dir, "link/before/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "link/after/a.txt"), "y\n")
	must.NoError(os.Symlink("/etc/hosts", filepath.Join(dir, "link/after/hosts")))
	s := &Set{dir: dir}
	for ws, want := range map[string]string{
		"same":       "identical",
		"nested-git": ".git directory",
		"link":       "only regular files",
	} {
		t.Run(ws, func(t *testing.T) {
			must := require.New(t)
			_, err := s.Files(Case{Workspace: ws})
			must.ErrorContains(err, want)
		})
	}
}

func TestLoad_RejectsOversizedSet(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "big.json")
	writeFile(t, path, strings.Repeat(" ", maxSetBytes+1))
	_, err := Load(path)
	must.ErrorContains(err, "exceeds")
}

func TestSeedSet(t *testing.T) {
	must := require.New(t)
	s, err := Load("diff-review-seed.json")
	must.NoError(err)
	labels := map[string]int{}
	adversarialLabels := map[string]int{}
	techniques := map[string]bool{}
	for _, c := range s.Cases {
		labels[c.Label]++
		if !c.Adversarial() {
			continue
		}
		adversarialLabels[c.Label]++
		for _, tag := range c.Tags {
			techniques[tag] = true
		}
	}

	t.Run("labels are balanced 12 pass and 12 fail", func(t *testing.T) {
		must := require.New(t)
		must.Len(s.Cases, 24)
		must.Equal(12, labels["pass"], "labels %v", labels)
		must.Equal(12, labels["fail"], "labels %v", labels)
	})

	t.Run("adversarial cases cover both labels", func(t *testing.T) {
		must := require.New(t)
		must.Equal(8, adversarialLabels["pass"]+adversarialLabels["fail"], "adversarial labels %v", adversarialLabels)
		must.Positive(adversarialLabels["pass"], "adversarial labels %v", adversarialLabels)
		must.Positive(adversarialLabels["fail"], "adversarial labels %v", adversarialLabels)
	})

	for _, tag := range []string{"planted-verdict", "fake-fence", "fake-truncation", "judge-instruction", "secret-string"} {
		t.Run("adversarial technique "+tag, func(t *testing.T) {
			must := require.New(t)
			must.True(techniques[tag], "no adversarial case is tagged %q", tag)
		})
	}

	for _, c := range s.Cases {
		t.Run("case "+c.ID+" explains its label", func(t *testing.T) {
			must := require.New(t)
			must.NotEmpty(strings.TrimSpace(c.Notes))
		})
	}
}

func TestFiles_RejectsPathsAWorkspaceCannotHold(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dir-to-file/before/x/y.txt"), "a\n")
	writeFile(t, filepath.Join(dir, "dir-to-file/after/x"), "b\n")
	s := &Set{dir: dir}
	cases := map[string]struct {
		c    Case
		want string
	}{
		"diff replaces a directory with a file": {Case{Diff: "--- a/a/b\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n--- /dev/null\n+++ b/a\n@@ -0,0 +1 @@\n+y\n"},
			`"a" is a file and also the directory of "a/b"`},
		"diff replaces a file with a directory": {Case{Diff: "--- a/a\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n--- /dev/null\n+++ b/a/b\n@@ -0,0 +1 @@\n+y\n"},
			`"a" is a file and also the directory of "a/b"`},
		"diff paths differ only in case": {Case{Diff: "--- a/A.txt\n+++ b/A.txt\n@@ -1 +1 @@\n-x\n+y\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-x\n+y\n"},
			`paths "A.txt" and "a.txt" differ only in case`},
		"directory differs only in case": {Case{Diff: "--- a/Dir\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n--- /dev/null\n+++ b/dir/f\n@@ -0,0 +1 @@\n+y\n"},
			`"Dir" is a file and also the directory of "dir/f"`},
		"fixture replaces a directory with a file": {Case{Workspace: "dir-to-file"}, `"x" is a file and also the directory of "x/y.txt"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := s.Files(tc.c)
			must.ErrorContains(err, tc.want)
		})
	}
}

func TestFiles_MissingSideIsAnEmptyTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "delete-all/before/a.txt"), "a\n")
	writeFile(t, filepath.Join(dir, "create-all/after/sub/b.txt"), "b\n")
	writeFile(t, filepath.Join(dir, "no-sides/README"), "neither side\n")
	writeFile(t, filepath.Join(dir, "not-a-dir"), "x\n")
	s := &Set{dir: dir}

	t.Run("missing after side deletes every file", func(t *testing.T) {
		must := require.New(t)
		files, err := s.Files(Case{Workspace: "delete-all"})
		must.NoError(err)
		must.Equal("a\n", files.Before["a.txt"])
		must.Empty(files.After)
	})

	t.Run("missing before side creates every file", func(t *testing.T) {
		must := require.New(t)
		files, err := s.Files(Case{Workspace: "create-all"})
		must.NoError(err)
		must.Empty(files.Before)
		must.Equal("b\n", files.After["sub/b.txt"])
	})

	for ws, want := range map[string]string{"no-sides": "neither before/ nor after/", "not-a-dir": "is not a directory"} {
		t.Run(ws, func(t *testing.T) {
			must := require.New(t)
			_, err := s.Files(Case{Workspace: ws})
			must.ErrorContains(err, want)
		})
	}
}

func TestLoad_NamesTheFileInParseErrors(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "bad.json")
	writeFile(t, path, `{"version":1,`)
	_, err := Load(path)
	must.Error(err)
	must.True(strings.HasPrefix(err.Error(), path+": decoding golden set"), "Load error = %v, want it prefixed with the path", err)

	_, err = Load(filepath.Join(t.TempDir(), "absent.json"))
	must.Error(err, "Load accepted a missing file")
}

func TestFiles_RejectsAnOversizedFixture(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "big/before/a.txt"), strings.Repeat("x", maxFixtureBytes/2))
	writeFile(t, filepath.Join(dir, "big/after/a.txt"), strings.Repeat("y", maxFixtureBytes/2+1))
	_, err := (&Set{dir: dir}).Files(Case{Workspace: "big"})
	must.ErrorContains(err, "workspace after/: fixture exceeds")
}
