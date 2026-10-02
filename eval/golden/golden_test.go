package golden

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validDiff = "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-old\n+new\n"

func validCase(id string) Case {
	return Case{ID: id, Criteria: "f.txt says new", Diff: validDiff, Label: "pass"}
}

func encodeSet(t *testing.T, cases ...Case) []byte {
	t.Helper()
	data, err := json.Marshal(Set{Version: FormatVersion, Name: "t", Cases: cases})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_DiffAndWorkspaceCases(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Cases) != 3 || !s.Cases[1].Adversarial() || s.Cases[0].Adversarial() {
		t.Fatalf("cases = %+v", s.Cases)
	}
	files, err := s.Files(s.Cases[0])
	if err != nil || files.Before["f.txt"] != "old\n" || files.After["f.txt"] != "new\n" {
		t.Errorf("diff case files = %#v, %v", files, err)
	}
	files, err = s.Files(s.Cases[2])
	if err != nil {
		t.Fatalf("workspace case files: %v", err)
	}
	wantBefore := Tree{"a.txt": "one\n", "gone.txt": "bye\n"}
	wantAfter := Tree{"a.txt": "one\ntwo\n", "sub/new.txt": "hi\n"}
	if !treesEqual(files.Before, wantBefore) || !treesEqual(files.After, wantAfter) {
		t.Errorf("workspace case files = %#v", files)
	}
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
		"missing workspace":   {func(c *Case) { c.Diff, c.Workspace = "", "absent" }, "workspace before/"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCase("c1")
			tc.mutate(&c)
			_, err := Parse(encodeSet(t, c), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "case 1 (") {
				t.Fatalf("Parse error = %v, want a case-scoped error containing %q", err, tc.want)
			}
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
			_, err := Parse([]byte(tc.data), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParse_ReportsEveryInvalidCase(t *testing.T) {
	first, second := validCase("dup"), validCase("dup")
	second.Label = "maybe"
	_, err := Parse(encodeSet(t, first, second), t.TempDir())
	if err == nil {
		t.Fatal("Parse accepted an invalid set")
	}
	for _, want := range []string{`case 2: duplicate id "dup"`, `case 2 ("dup"): label`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestFiles_WorkspaceFixtureRules(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "same/before/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "same/after/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "nested-git/before/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "nested-git/after/.git/config"), "[core]\n")
	writeFile(t, filepath.Join(dir, "link/before/a.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "link/after/a.txt"), "y\n")
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "link/after/hosts")); err != nil {
		t.Fatal(err)
	}
	s := &Set{dir: dir}
	for ws, want := range map[string]string{
		"same":       "identical",
		"nested-git": ".git directory",
		"link":       "only regular files",
	} {
		_, err := s.Files(Case{Workspace: ws})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: Files error = %v, want it to contain %q", ws, err, want)
		}
	}
}

func TestLoad_RejectsOversizedSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	writeFile(t, path, strings.Repeat(" ", maxSetBytes+1))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load error = %v, want a size error", err)
	}
}

func TestSeedSet(t *testing.T) {
	s, err := Load("diff-review-seed.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
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
	if len(s.Cases) != 24 || labels["pass"] != 12 || labels["fail"] != 12 {
		t.Errorf("seed has %d cases labelled %v, want 24 balanced 12/12", len(s.Cases), labels)
	}
	if adversarialLabels["pass"]+adversarialLabels["fail"] != 8 || adversarialLabels["pass"] == 0 || adversarialLabels["fail"] == 0 {
		t.Errorf("adversarial labels = %v, want 8 cases with both labels represented", adversarialLabels)
	}
	for _, tag := range []string{"planted-verdict", "fake-fence", "fake-truncation", "judge-instruction", "secret-string"} {
		if !techniques[tag] {
			t.Errorf("no adversarial case is tagged %q", tag)
		}
	}
	for _, c := range s.Cases {
		if strings.TrimSpace(c.Notes) == "" {
			t.Errorf("case %s has no notes explaining its label", c.ID)
		}
	}
}
