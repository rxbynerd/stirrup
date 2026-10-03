package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/types"
)

func TestCaptureWorkspaceDiff_IncludesUntrackedModifiedAndDeleted(t *testing.T) {
	ws, base := changedWorkspace(t)

	got := capture(t, ws, base, 1<<20)
	for _, want := range []string{"+two", "new.txt", "+brand new", "gone.txt", "-bye", "--- a/a.txt", "+++ b/a.txt"} {
		if !strings.Contains(got.Head, want) {
			t.Errorf("diff missing %q:\n%s", want, got.Head)
		}
	}
	for _, file := range []string{"a.txt", "new.txt", "gone.txt"} {
		if !strings.Contains(got.Stat, file) {
			t.Errorf("stat missing %q:\n%s", file, got.Stat)
		}
	}
	if got.Truncated {
		t.Error("small diff reported as truncated")
	}
	if got.Size != len(got.Head) {
		t.Errorf("Size = %d, want len(Head) = %d", got.Size, len(got.Head))
	}
	sum := sha256.Sum256([]byte(got.Head))
	if got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %s, want hash of the untruncated diff", got.SHA256)
	}
}

func TestCaptureWorkspaceDiff_FilesOnlyWorkspaceGetsNoGitDir(t *testing.T) {
	ws, base := changedWorkspace(t)

	capture(t, ws, base, 1<<20)
	if fileExists(filepath.Join(ws, ".git")) {
		t.Error("baseline or capture created a .git inside the workspace")
	}
}

func TestCaptureWorkspaceDiff_LeavesWorkspaceRepositoryUntouched(t *testing.T) {
	ws := gitRepoWorkspace(t, map[string]string{"a.txt": "one\n"})
	base := baselineOf(t, ws)
	writeFiles(t, ws, map[string]string{"a.txt": "one\ntwo\n", "new.txt": "brand new\n"})
	statusBefore := gitCmd(t, ws, "status", "--porcelain")
	objectsBefore := countObjects(t, filepath.Join(ws, ".git"))

	got := capture(t, ws, base, 1<<20)

	if !strings.Contains(got.Head, "+brand new") || !strings.Contains(got.Head, "+two") {
		t.Errorf("diff missing the change:\n%s", got.Head)
	}
	if after := gitCmd(t, ws, "status", "--porcelain"); after != statusBefore {
		t.Errorf("workspace git status changed:\nbefore:\n%s\nafter:\n%s", statusBefore, after)
	}
	if staged := gitCmd(t, ws, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("workspace index has staged changes: %q", staged)
	}
	if after := countObjects(t, filepath.Join(ws, ".git")); after != objectsBefore {
		t.Errorf("workspace object store grew from %d to %d files", objectsBefore, after)
	}
	if strings.Contains(got.Head, "upstream") || strings.Contains(got.Stat, ".git") {
		t.Errorf("diff reflects the workspace's own repository:\n%s", got.Head)
	}
}

func TestCaptureWorkspaceDiff_IncludesIgnoredFiles(t *testing.T) {
	ws := gitRepoWorkspace(t, map[string]string{".gitignore": "*.log\n", "a.txt": "one\n"})
	base := baselineOf(t, ws)
	writeFiles(t, ws, map[string]string{
		".gitignore":        "*.log\nhidden.go\n",
		".git/info/exclude": "excluded.go\n",
		"debug.log":         "log line\n",
		"hidden.go":         "package hidden\n",
		"excluded.go":       "package excluded\n",
	})

	got := capture(t, ws, base, 1<<20)
	for _, want := range []string{"+log line", "+package hidden", "+package excluded"} {
		if !strings.Contains(got.Head, want) {
			t.Errorf("ignored file content %q missing from the diff:\n%s", want, got.Head)
		}
	}
}

func TestCreateBaseline_IncludesIgnoredSeeds(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{".gitignore": "*.log\n", "seed.log": "seeded\n"})

	if got := capture(t, ws, base, 1<<20); got.Size != 0 {
		t.Errorf("an ignored seed file is attributed to the agent:\n%s", got.Head)
	}
}

func TestCaptureWorkspaceDiff_NoChanges(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})

	got := capture(t, ws, base, 1<<20)
	if got.Head != "" || got.Size != 0 || got.Truncated || got.Stat != "" {
		t.Errorf("unchanged workspace produced a diff: %+v", got)
	}
}

func TestCaptureWorkspaceDiff_ScrubsInheritedRepositoryEnv(t *testing.T) {
	other := gitRepoWorkspace(t, map[string]string{"other.txt": "other\n"})
	ws, base := changedWorkspace(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "diff.noprefix")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_DIFF_OPTS", "--unified=0")

	got := capture(t, ws, base, 1<<20)
	if !strings.Contains(got.Head, "+brand new") || strings.Contains(got.Head, "other") {
		t.Errorf("diff came from the wrong repository:\n%s", got.Head)
	}
	if !strings.Contains(got.Head, "--- a/a.txt") || !strings.Contains(got.Head, " one\n") {
		t.Errorf("inherited diff configuration reshaped the patch:\n%s", got.Head)
	}
}

func TestCaptureWorkspaceDiff_IndependentOfHostGitConfig(t *testing.T) {
	ws, base := changedWorkspace(t)
	clean := capture(t, ws, base, 1<<20)

	dir := tempDir(t)
	excludes := filepath.Join(dir, "excludes")
	global := filepath.Join(dir, "gitconfig")
	writeFiles(t, dir, map[string]string{
		"excludes":  "*.txt\n",
		"gitconfig": "[core]\n\texcludesFile = " + excludes + "\n[diff]\n\tnoprefix = true\n\tmnemonicPrefix = true\n\tcontext = 0\n",
	})
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	home := tempDir(t)
	writeFiles(t, home, map[string]string{".gitconfig": "[include]\n\tpath = " + global + "\n"})
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	hostile := capture(t, ws, base, 1<<20)
	if hostile.SHA256 != clean.SHA256 || hostile.Head != clean.Head {
		t.Errorf("host git config changed the capture:\nclean:\n%s\nwith host config:\n%s", clean.Head, hostile.Head)
	}
	if again := baselineOf(t, ws); capture(t, ws, again, 1<<20).Size != 0 {
		t.Error("host excludesFile kept files out of the baseline")
	}
}

func TestCaptureWorkspaceDiff_TruncatesHeadButHashesWholeDiff(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	writeFiles(t, ws, map[string]string{"big.txt": strings.Repeat("line of text\n", 2000)})

	full := capture(t, ws, base, 1<<20)
	cut := capture(t, ws, base, 500)

	if !cut.Truncated {
		t.Fatal("expected truncation")
	}
	if len(cut.Head) > 500 {
		t.Errorf("Head is %d bytes, want <= 500", len(cut.Head))
	}
	if !strings.HasPrefix(full.Head, cut.Head) {
		t.Error("truncated head is not a prefix of the full diff")
	}
	if cut.Size != full.Size || cut.SHA256 != full.SHA256 {
		t.Errorf("truncated capture identity (%d, %s) differs from full diff (%d, %s)", cut.Size, cut.SHA256, full.Size, full.SHA256)
	}
}

func TestCaptureWorkspaceDiff_TruncationBoundary(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	writeFiles(t, ws, map[string]string{"a.txt": "one\ntwo\n"})
	full := capture(t, ws, base, 1<<20)

	exact := capture(t, ws, base, full.Size)
	if exact.Truncated || exact.Head != full.Head {
		t.Errorf("a diff of exactly max bytes was truncated: %+v", exact)
	}
	over := capture(t, ws, base, full.Size-1)
	if !over.Truncated || len(over.Head) != full.Size-1 {
		t.Errorf("a diff one byte over max bytes was not truncated: truncated=%v head=%d", over.Truncated, len(over.Head))
	}
}

func TestCaptureWorkspaceDiff_CutsOnRuneBoundary(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	writeFiles(t, ws, map[string]string{"u.txt": strings.Repeat("é", 400) + "\n"})

	for limit := 120; limit < 126; limit++ {
		got := capture(t, ws, base, limit)
		if !utf8.ValidString(got.Head) {
			t.Errorf("limit %d: head is not valid UTF-8: %q", limit, got.Head[len(got.Head)-3:])
		}
		if !got.Truncated {
			t.Errorf("limit %d: expected truncation", limit)
		}
	}
}

func TestCaptureWorkspaceDiff_RejectsMalformedRevision(t *testing.T) {
	ws, base := newWorkspace(t, nil)
	base.Rev = "--output=/tmp/x"
	if _, err := captureWorkspaceDiff(context.Background(), ws, base, 1<<20); err == nil {
		t.Fatal("expected an error for a revision that is not an object name")
	}
}

// hostileVector is an agent action taken after the baseline that must
// neither run a command on the eval host nor hide the agent's change.
type hostileVector struct {
	name string

	// script runs in the workspace; MARKER and HOOKS expand to a marker
	// path and a directory outside the workspace.
	script string

	// want must appear in the captured diff.
	want []string
}

func TestCaptureWorkspaceDiff_IgnoresAgentGitConfiguration(t *testing.T) {
	hookScript := `printf '#!/bin/sh\ntouch MARKER\n' > %s && chmod +x %s`
	vectors := []hostileVector{
		{
			name:   "clean filter",
			script: `git init -q && git config filter.x.clean "sh -c 'touch MARKER; cat'" && printf '* filter=x\n' > .gitattributes && echo evil > b.txt`,
			want:   []string{"+evil", "+* filter=x"},
		},
		{
			name: "hooks",
			script: `git init -q && for h in post-index-change pre-auto-gc reference-transaction post-commit; do ` +
				strings.ReplaceAll(hookScript, "%s", ".git/hooks/$h") + `; done && echo evil > b.txt`,
			want: []string{"+evil"},
		},
		{
			name:   "core.hooksPath",
			script: `git init -q && ` + strings.ReplaceAll(hookScript, "%s", "HOOKS/post-index-change") + ` && git config core.hooksPath HOOKS && echo evil > b.txt`,
			want:   []string{"+evil"},
		},
		{
			name:   "core.fsmonitor",
			script: `git init -q && git config core.fsmonitor "touch MARKER; echo" && echo evil > b.txt`,
			want:   []string{"+evil"},
		},
		{
			name: "external diff and textconv",
			script: `git init -q && printf '* diff=x\n' > .gitattributes && git config diff.x.command "touch MARKER; true" && ` +
				`git config diff.x.textconv "touch MARKER; cat" && git config diff.external "touch MARKER" && echo changed > a.txt`,
			want: []string{"-base", "+changed"},
		},
		{
			name:   "core.worktree",
			script: `git init -q && git config core.worktree HOOKS && echo evil > b.txt`,
			want:   []string{"+evil"},
		},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			ws, base := newWorkspace(t, map[string]string{"a.txt": "base\n"})
			runHostileVector(t, ws, base, v)
		})
		t.Run(v.name+" in a repo workspace", func(t *testing.T) {
			ws := gitRepoWorkspace(t, map[string]string{"a.txt": "base\n"})
			base := baselineOf(t, ws)
			v := v
			v.script = strings.TrimPrefix(v.script, "git init -q && ")
			runHostileVector(t, ws, base, v)
		})
	}
}

func runHostileVector(t *testing.T, ws string, base Baseline, v hostileVector) {
	t.Helper()
	marker := filepath.Join(tempDir(t), "marker")
	hooks := tempDir(t)
	shIn(t, ws, strings.NewReplacer("MARKER", marker, "HOOKS", hooks).Replace(v.script))

	got := capture(t, ws, base, 1<<20)
	if fileExists(marker) {
		t.Errorf("agent-written git configuration ran a command on the eval host")
	}
	for _, want := range v.want {
		if !strings.Contains(got.Head, want) {
			t.Errorf("diff missing %q:\n%s", want, got.Head)
		}
	}
}

func TestCaptureWorkspaceDiff_HardeningOverridesJudgeRepositoryConfig(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{"a.txt": "base\n"})
	marker := filepath.Join(tempDir(t), "marker")
	writeFiles(t, ws, map[string]string{".gitattributes": "* diff=x\n", "a.txt": "changed\n"})
	writeFiles(t, base.GitDir, map[string]string{
		"hooks/post-index-change": "#!/bin/sh\ntouch " + marker + "-hook\n",
	})
	if err := os.Chmod(filepath.Join(base.GitDir, "hooks", "post-index-change"), 0o755); err != nil {
		t.Fatal(err)
	}
	replaceBaselineWithWorkspace(t, ws, base)
	for _, kv := range [][2]string{
		{"core.hooksPath", filepath.Join(base.GitDir, "hooks")},
		{"core.fsmonitor", "touch " + marker + "-fsmonitor; echo"},
		{"diff.external", "touch " + marker + "-external"},
		{"diff.x.command", "touch " + marker + "-command"},
		{"diff.x.textconv", "touch " + marker + "-textconv; cat"},
		{"diff.noprefix", "true"},
		{"diff.mnemonicPrefix", "true"},
	} {
		gitCmd(t, ws, "--git-dir", base.GitDir, "config", kv[0], kv[1])
	}

	got := capture(t, ws, base, 1<<20)
	for _, suffix := range []string{"-hook", "-fsmonitor", "-external", "-command", "-textconv"} {
		if fileExists(marker + suffix) {
			t.Errorf("judge repository configuration ran %s", strings.TrimPrefix(suffix, "-"))
		}
	}
	if !strings.Contains(got.Head, "--- a/a.txt") || !strings.Contains(got.Head, "-base") || !strings.Contains(got.Head, "+changed") {
		t.Errorf("diff missing the change or reshaped by judge repository config:\n%s", got.Head)
	}
}

// replaceBaselineWithWorkspace plants a replace ref in the judge-owned
// repository that substitutes the workspace's current content for the
// baseline commit.
func replaceBaselineWithWorkspace(t *testing.T, ws string, base Baseline) {
	t.Helper()
	index := filepath.Join(tempDir(t), "index")
	git := func(args ...string) string {
		return strings.TrimSpace(gitCmd(t, ws, append([]string{"--git-dir", base.GitDir, "--work-tree", ws}, args...)...))
	}
	t.Setenv("GIT_INDEX_FILE", index)
	git("add", "-A", "-f")
	tree := git("write-tree")
	if err := os.Unsetenv("GIT_INDEX_FILE"); err != nil {
		t.Fatal(err)
	}
	replacement := git("commit-tree", tree, "-m", "replacement")
	git("replace", base.Rev, replacement)
}

func TestCaptureWorkspaceDiff_AgentHistoryDoesNotMoveBaseline(t *testing.T) {
	cases := map[string]string{
		"commit":           `echo evil > a.txt && git add -A && git commit -q -m agent`,
		"commit and reset": `echo evil > a.txt && git add -A && git commit -q -m agent && git reset -q --hard HEAD`,
		"replace":          `base=$(git rev-parse HEAD) && echo evil > a.txt && git add -A && git commit -q -m agent && git replace "$base" HEAD`,
		"gitfile":          `echo evil > a.txt && git add -A && git commit -q -m agent && mv .git ../moved.git && printf 'gitdir: %s\n' "$(cd .. && pwd)/moved.git" > .git`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			ws := gitRepoWorkspace(t, map[string]string{"a.txt": "base\n"})
			base := baselineOf(t, ws)
			shIn(t, ws, script)

			got := capture(t, ws, base, 1<<20)
			if !strings.Contains(got.Head, "-base") || !strings.Contains(got.Head, "+evil") {
				t.Errorf("diff does not show the agent's change against the recorded baseline:\n%s", got.Head)
			}
		})
	}
}

func TestCaptureWorkspaceDiff_AttributesCannotHideContent(t *testing.T) {
	for name, attrs := range map[string]string{
		"-diff":  "* -diff\n",
		"binary": "a.txt binary\n",
	} {
		t.Run(name, func(t *testing.T) {
			ws, base := newWorkspace(t, map[string]string{"a.txt": "base\n"})
			writeFiles(t, ws, map[string]string{".gitattributes": attrs, "a.txt": "rm -rf / evil\n"})

			got := capture(t, ws, base, 1<<20)
			if !strings.Contains(got.Head, "+rm -rf / evil") || strings.Contains(got.Head, "Binary files") {
				t.Errorf("attribute hid the change from the judge:\n%s", got.Head)
			}
		})
	}
}

func TestCaptureWorkspaceDiff_NestedRepositoryIsAnError(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{"a.txt": "base\n"})
	shIn(t, ws, `mkdir sub && cd sub && git init -q && echo payload > e.txt && git add -A && git commit -q -m x`)

	_, err := captureWorkspaceDiff(context.Background(), ws, base, 1<<20)
	if !errors.Is(err, errGitlink) {
		t.Fatalf("err = %v, want errGitlink", err)
	}
}

func TestGitlinkScanner(t *testing.T) {
	cases := map[string]bool{
		":100644 100644 aaaaaaa bbbbbbb M\ta.txt\n":                 false,
		":000000 160000 0000000 bbbbbbb A\tsub\n":                   true,
		":160000 000000 aaaaaaa 0000000 D\tsub\n":                   true,
		":100644 100644 a b M\t\"odd\\n:160000 160000 x\"\n":        false,
		":000000 100644 0000000 bbbbbbb A\tx\n:000000 160000 0 b A": true,
		"": false,
	}
	for raw, want := range cases {
		for _, chunk := range []int{1, 7, len(raw) + 1} {
			s := &gitlinkScanner{}
			for i := 0; i < len(raw); i += chunk {
				_, _ = s.Write([]byte(raw[i:min(i+chunk, len(raw))]))
			}
			if got := s.found(); got != want {
				t.Errorf("found(%q, chunk %d) = %v, want %v", raw, chunk, got, want)
			}
		}
	}
}

func TestWorkspaceHeadBaseline(t *testing.T) {
	t.Run("diffs against HEAD without running workspace configuration", func(t *testing.T) {
		ws := gitRepoWorkspace(t, map[string]string{"a.txt": "base\n"})
		marker := filepath.Join(tempDir(t), "marker")
		shIn(t, ws, `printf '#!/bin/sh\ntouch `+marker+`\n' > .git/hooks/post-index-change && chmod +x .git/hooks/post-index-change && `+
			`git config core.fsmonitor "touch `+marker+`; echo" && git config filter.x.clean "sh -c 'touch `+marker+`; cat'" && `+
			`printf '* filter=x\n' > .gitattributes && echo changed > a.txt`)
		objectsBefore := countObjects(t, filepath.Join(ws, ".git"))

		base, err := workspaceHeadBaseline(context.Background(), ws, filepath.Join(tempDir(t), "judge.git"))
		if err != nil {
			t.Fatalf("workspaceHeadBaseline: %v", err)
		}
		if base.Source != types.JudgeBaselineWorkspaceHead || base.recorded() {
			t.Errorf("baseline = %+v", base)
		}
		got := capture(t, ws, base, 1<<20)

		if fileExists(marker) {
			t.Error("resolving the workspace HEAD ran agent-written configuration")
		}
		if !strings.Contains(got.Head, "+changed") {
			t.Errorf("diff missing the change:\n%s", got.Head)
		}
		if after := countObjects(t, filepath.Join(ws, ".git")); after != objectsBefore {
			t.Errorf("workspace object store grew from %d to %d files", objectsBefore, after)
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		requireGit(t)
		_, err := workspaceHeadBaseline(context.Background(), tempDir(t), filepath.Join(tempDir(t), "judge.git"))
		if !errors.Is(err, errNotGitRepo) {
			t.Fatalf("err = %v, want errNotGitRepo", err)
		}
	})

	t.Run("subdirectory of a repository", func(t *testing.T) {
		ws := gitRepoWorkspace(t, map[string]string{"sub/a.txt": "one\n"})
		_, err := workspaceHeadBaseline(context.Background(), filepath.Join(ws, "sub"), filepath.Join(tempDir(t), "judge.git"))
		if !errors.Is(err, errNotGitRepo) {
			t.Fatalf("err = %v, want errNotGitRepo", err)
		}
	})

	t.Run("repository without commits", func(t *testing.T) {
		requireGit(t)
		ws := tempDir(t)
		gitCmd(t, ws, "init", "-q")
		_, err := workspaceHeadBaseline(context.Background(), ws, filepath.Join(tempDir(t), "judge.git"))
		if !errors.Is(err, errNoCommits) {
			t.Fatalf("err = %v, want errNoCommits", err)
		}
	})
}

func TestBaselineSidecar_RoundTrip(t *testing.T) {
	ws, base := changedWorkspace(t)
	want := capture(t, ws, base, 1<<20)
	artifacts := tempDir(t)

	if err := WriteBaselineSidecar(context.Background(), base, artifacts); err != nil {
		t.Fatalf("WriteBaselineSidecar: %v", err)
	}
	restored, err := LoadBaselineSidecar(context.Background(), filepath.Join(artifacts, BaselineSidecarName), filepath.Join(tempDir(t), "judge.git"))
	if err != nil {
		t.Fatalf("LoadBaselineSidecar: %v", err)
	}
	if restored.Rev != base.Rev || restored.Source != types.JudgeBaselineSidecar || !restored.recorded() {
		t.Errorf("restored = %+v, want rev %s from the sidecar", restored, base.Rev)
	}
	if got := capture(t, ws, restored, 1<<20); got.SHA256 != want.SHA256 {
		t.Errorf("restored baseline produced a different diff:\n%s\nwant:\n%s", got.Head, want.Head)
	}
}

func TestLoadBaselineSidecar_RejectsMalformedSidecars(t *testing.T) {
	requireGit(t)
	cases := map[string]string{
		"bad version":    `{"schemaVersion":2,"baselineRev":"` + strings.Repeat("a", 40) + `","bundle":"judge-baseline.bundle"}`,
		"short rev":      `{"schemaVersion":1,"baselineRev":"abc123","bundle":"judge-baseline.bundle"}`,
		"option rev":     `{"schemaVersion":1,"baselineRev":"--all","bundle":"judge-baseline.bundle"}`,
		"bundle path":    `{"schemaVersion":1,"baselineRev":"` + strings.Repeat("a", 40) + `","bundle":"../x.bundle"}`,
		"missing bundle": `{"schemaVersion":1,"baselineRev":"` + strings.Repeat("a", 40) + `","bundle":"judge-baseline.bundle"}`,
		"not json":       `baseline`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := tempDir(t)
			writeFiles(t, dir, map[string]string{BaselineSidecarName: body})
			if _, err := LoadBaselineSidecar(context.Background(), filepath.Join(dir, BaselineSidecarName), filepath.Join(tempDir(t), "judge.git")); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
