package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestCaptureWorkspaceDiff_IncludesUntrackedModifiedAndDeleted(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	ws, base := changedWorkspace(t)

	got := capture(t, ws, base, 1<<20)
	for _, want := range []string{"+two", "new.txt", "+brand new", "gone.txt", "-bye", "--- a/a.txt", "+++ b/a.txt"} {
		check.Contains(got.Head, want)
	}
	for _, file := range []string{"a.txt", "new.txt", "gone.txt"} {
		check.Contains(got.Stat, file)
	}
	must.False(got.Truncated, "small diff reported as truncated")
	must.Equal(len(got.Head), got.Size)
	sum := sha256.Sum256([]byte(got.Head))
	must.Equal(hex.EncodeToString(sum[:]), got.SHA256, "SHA256 should hash the untruncated diff")
}

func TestCaptureWorkspaceDiff_FilesOnlyWorkspaceGetsNoGitDir(t *testing.T) {
	must := require.New(t)
	ws, base := changedWorkspace(t)

	capture(t, ws, base, 1<<20)
	must.False(fileExists(filepath.Join(ws, ".git")), "baseline or capture created a .git inside the workspace")
}

func TestCaptureWorkspaceDiff_LeavesWorkspaceRepositoryUntouched(t *testing.T) {
	check := assert.New(t)
	ws := gitRepoWorkspace(t, map[string]string{"a.txt": "one\n"})
	base := baselineOf(t, ws)
	writeFiles(t, ws, map[string]string{"a.txt": "one\ntwo\n", "new.txt": "brand new\n"})
	statusBefore := gitCmd(t, ws, "status", "--porcelain")
	objectsBefore := countObjects(t, filepath.Join(ws, ".git"))

	got := capture(t, ws, base, 1<<20)

	check.Contains(got.Head, "+brand new")
	check.Contains(got.Head, "+two")
	check.Equal(statusBefore, gitCmd(t, ws, "status", "--porcelain"), "workspace git status changed")
	check.Empty(gitCmd(t, ws, "diff", "--cached", "--name-only"), "workspace index has staged changes")
	check.Equal(objectsBefore, countObjects(t, filepath.Join(ws, ".git")), "workspace object store changed")
	check.NotContains(got.Head, "upstream", "diff reflects the workspace's own repository")
	check.NotContains(got.Stat, ".git", "diff reflects the workspace's own repository")
}

func TestCaptureWorkspaceDiff_IncludesIgnoredFiles(t *testing.T) {
	check := assert.New(t)
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
		check.Contains(got.Head, want, "ignored file content missing from the diff")
	}
}

func TestCreateBaseline_IncludesIgnoredSeeds(t *testing.T) {
	must := require.New(t)
	ws, base := newWorkspace(t, map[string]string{".gitignore": "*.log\n", "seed.log": "seeded\n"})

	got := capture(t, ws, base, 1<<20)
	must.Zero(got.Size, "an ignored seed file is attributed to the agent:\n%s", got.Head)
}

func TestCaptureWorkspaceDiff_NoChanges(t *testing.T) {
	check := assert.New(t)
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})

	got := capture(t, ws, base, 1<<20)
	check.Empty(got.Head, "unchanged workspace produced a diff: %+v", got)
	check.Zero(got.Size)
	check.False(got.Truncated)
	check.Empty(got.Stat)
}

func TestCaptureWorkspaceDiff_ScrubsInheritedRepositoryEnv(t *testing.T) {
	check := assert.New(t)
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
	check.Contains(got.Head, "+brand new")
	check.NotContains(got.Head, "other", "diff came from the wrong repository")
	check.Contains(got.Head, "--- a/a.txt")
	check.Contains(got.Head, " one\n", "inherited diff configuration reshaped the patch")
}

func TestCaptureWorkspaceDiff_IndependentOfHostGitConfig(t *testing.T) {
	check := assert.New(t)
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
	check.Equal(clean.Head, hostile.Head, "host git config changed the capture")
	check.Equal(clean.SHA256, hostile.SHA256, "host git config changed the capture")

	again := baselineOf(t, ws)
	check.Zero(capture(t, ws, again, 1<<20).Size, "host excludesFile kept files out of the baseline")
}

func TestCaptureWorkspaceDiff_TruncatesHeadButHashesWholeDiff(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	writeFiles(t, ws, map[string]string{"big.txt": strings.Repeat("line of text\n", 2000)})

	full := capture(t, ws, base, 1<<20)
	cut := capture(t, ws, base, 500)

	must.True(cut.Truncated, "expected truncation")
	check.LessOrEqual(len(cut.Head), 500)
	check.True(strings.HasPrefix(full.Head, cut.Head), "truncated head is not a prefix of the full diff")
	check.Equal(full.Size, cut.Size, "truncated capture size differs from the full diff")
	check.Equal(full.SHA256, cut.SHA256, "truncated capture hash differs from the full diff")
}

func TestCaptureWorkspaceDiff_TruncationBoundary(t *testing.T) {
	check := assert.New(t)
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	writeFiles(t, ws, map[string]string{"a.txt": "one\ntwo\n"})
	full := capture(t, ws, base, 1<<20)

	exact := capture(t, ws, base, full.Size)
	check.False(exact.Truncated, "a diff of exactly max bytes was truncated")
	check.Equal(full.Head, exact.Head, "a diff of exactly max bytes changed")

	over := capture(t, ws, base, full.Size-1)
	check.True(over.Truncated, "a diff one byte over max bytes was not truncated")
	check.Len(over.Head, full.Size-1)
}

func TestCaptureWorkspaceDiff_CutsOnRuneBoundary(t *testing.T) {
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	writeFiles(t, ws, map[string]string{"u.txt": strings.Repeat("é", 400) + "\n"})

	for limit := 120; limit < 126; limit++ {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			must := require.New(t)
			got := capture(t, ws, base, limit)
			must.True(utf8.ValidString(got.Head), "head is not valid UTF-8: %q", got.Head)
			must.True(got.Truncated, "expected truncation")
		})
	}
}

func TestCaptureWorkspaceDiff_RejectsMalformedRevision(t *testing.T) {
	must := require.New(t)
	ws, base := newWorkspace(t, nil)
	base.Rev = "--output=/tmp/x"

	_, err := captureWorkspaceDiff(context.Background(), ws, base, 1<<20)
	must.Error(err, "expected an error for a revision that is not an object name")
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
	must := require.New(t)
	marker := filepath.Join(tempDir(t), "marker")
	hooks := tempDir(t)
	shIn(t, ws, strings.NewReplacer("MARKER", marker, "HOOKS", hooks).Replace(v.script))

	got := capture(t, ws, base, 1<<20)
	must.False(fileExists(marker), "agent-written git configuration ran a command on the eval host")
	for _, want := range v.want {
		must.Contains(got.Head, want)
	}
}

func TestCaptureWorkspaceDiff_HardeningOverridesJudgeRepositoryConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	ws, base := newWorkspace(t, map[string]string{"a.txt": "base\n"})
	marker := filepath.Join(tempDir(t), "marker")
	writeFiles(t, ws, map[string]string{".gitattributes": "* diff=x\n", "a.txt": "changed\n"})
	writeFiles(t, base.GitDir, map[string]string{
		"hooks/post-index-change": "#!/bin/sh\ntouch " + marker + "-hook\n",
	})
	must.NoError(os.Chmod(filepath.Join(base.GitDir, "hooks", "post-index-change"), 0o755))
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
	for _, name := range []string{"hook", "fsmonitor", "external", "command", "textconv"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.False(fileExists(marker+"-"+name), "judge repository configuration ran %s", name)
		})
	}
	check.Contains(got.Head, "--- a/a.txt", "diff missing the change or reshaped by judge repository config")
	check.Contains(got.Head, "-base", "diff missing the change or reshaped by judge repository config")
	check.Contains(got.Head, "+changed", "diff missing the change or reshaped by judge repository config")
}

// replaceBaselineWithWorkspace plants a replace ref in the judge-owned
// repository that substitutes the workspace's current content for the
// baseline commit.
func replaceBaselineWithWorkspace(t *testing.T, ws string, base Baseline) {
	t.Helper()
	must := require.New(t)
	index := filepath.Join(tempDir(t), "index")
	git := func(args ...string) string {
		return strings.TrimSpace(gitCmd(t, ws, append([]string{"--git-dir", base.GitDir, "--work-tree", ws}, args...)...))
	}
	t.Setenv("GIT_INDEX_FILE", index)
	git("add", "-A", "-f")
	tree := git("write-tree")
	must.NoError(os.Unsetenv("GIT_INDEX_FILE"))
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
			must := require.New(t)
			ws := gitRepoWorkspace(t, map[string]string{"a.txt": "base\n"})
			base := baselineOf(t, ws)
			shIn(t, ws, script)

			got := capture(t, ws, base, 1<<20)
			must.Contains(got.Head, "-base", "diff does not show the agent's change against the recorded baseline")
			must.Contains(got.Head, "+evil")
		})
	}
}

func TestCaptureWorkspaceDiff_AttributesCannotHideContent(t *testing.T) {
	for name, attrs := range map[string]string{
		"negated diff attribute": "* -diff\n",
		"binary attribute":       "a.txt binary\n",
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			ws, base := newWorkspace(t, map[string]string{"a.txt": "base\n"})
			writeFiles(t, ws, map[string]string{".gitattributes": attrs, "a.txt": "rm -rf / evil\n"})

			got := capture(t, ws, base, 1<<20)
			must.Contains(got.Head, "+rm -rf / evil", "attribute hid the change from the judge")
			must.NotContains(got.Head, "Binary files", "attribute hid the change from the judge")
		})
	}
}

func TestCaptureWorkspaceDiff_NestedRepositoryIsAnError(t *testing.T) {
	must := require.New(t)
	ws, base := newWorkspace(t, map[string]string{"a.txt": "base\n"})
	shIn(t, ws, `mkdir sub && cd sub && git init -q && echo payload > e.txt && git add -A && git commit -q -m x`)

	_, err := captureWorkspaceDiff(context.Background(), ws, base, 1<<20)
	must.ErrorIs(err, errGitlink)
}

func TestGitlinkScanner_DetectsGitlinkEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "regular file entry", raw: ":100644 100644 aaaaaaa bbbbbbb M\ta.txt\n", want: false},
		{name: "added gitlink", raw: ":000000 160000 0000000 bbbbbbb A\tsub\n", want: true},
		{name: "removed gitlink", raw: ":160000 000000 aaaaaaa 0000000 D\tsub\n", want: true},
		{name: "quoted path resembling a gitlink entry", raw: ":100644 100644 a b M\t\"odd\\n:160000 160000 x\"\n", want: false},
		{name: "gitlink after a regular entry", raw: ":000000 100644 0000000 bbbbbbb A\tx\n:000000 160000 0 b A", want: true},
		{name: "empty output", raw: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			for _, chunk := range []int{1, 7, len(tc.raw) + 1} {
				s := &gitlinkScanner{}
				for i := 0; i < len(tc.raw); i += chunk {
					_, _ = s.Write([]byte(tc.raw[i:min(i+chunk, len(tc.raw))]))
				}
				must.Equal(tc.want, s.found(), "chunk size %d", chunk)
			}
		})
	}
}

func TestWorkspaceHeadBaseline_UsesHeadOfRepositoryRoot(t *testing.T) {
	t.Run("diffs against HEAD without running workspace configuration", func(t *testing.T) {
		must := require.New(t)
		ws := gitRepoWorkspace(t, map[string]string{"a.txt": "base\n"})
		marker := filepath.Join(tempDir(t), "marker")
		shIn(t, ws, `printf '#!/bin/sh\ntouch `+marker+`\n' > .git/hooks/post-index-change && chmod +x .git/hooks/post-index-change && `+
			`git config core.fsmonitor "touch `+marker+`; echo" && git config filter.x.clean "sh -c 'touch `+marker+`; cat'" && `+
			`printf '* filter=x\n' > .gitattributes && echo changed > a.txt`)
		objectsBefore := countObjects(t, filepath.Join(ws, ".git"))

		base, err := workspaceHeadBaseline(context.Background(), ws, filepath.Join(tempDir(t), "judge.git"))
		must.NoError(err)
		must.Equal(types.JudgeBaselineWorkspaceHead, base.Source, "baseline = %+v", base)
		must.False(base.recorded(), "baseline = %+v", base)
		got := capture(t, ws, base, 1<<20)

		must.False(fileExists(marker), "resolving the workspace HEAD ran agent-written configuration")
		must.Contains(got.Head, "+changed")
		must.Equal(objectsBefore, countObjects(t, filepath.Join(ws, ".git")), "workspace object store changed")
	})

	t.Run("not a repository", func(t *testing.T) {
		must := require.New(t)
		requireGit(t)
		_, err := workspaceHeadBaseline(context.Background(), tempDir(t), filepath.Join(tempDir(t), "judge.git"))
		must.ErrorIs(err, errNotGitRepo)
	})

	t.Run("subdirectory of a repository", func(t *testing.T) {
		must := require.New(t)
		ws := gitRepoWorkspace(t, map[string]string{"sub/a.txt": "one\n"})
		_, err := workspaceHeadBaseline(context.Background(), filepath.Join(ws, "sub"), filepath.Join(tempDir(t), "judge.git"))
		must.ErrorIs(err, errNotGitRepo)
	})

	t.Run("repository without commits", func(t *testing.T) {
		must := require.New(t)
		requireGit(t)
		ws := tempDir(t)
		gitCmd(t, ws, "init", "-q")
		_, err := workspaceHeadBaseline(context.Background(), ws, filepath.Join(tempDir(t), "judge.git"))
		must.ErrorIs(err, errNoCommits)
	})
}

func TestBaselineSidecar_RoundTrip(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	ws, base := changedWorkspace(t)
	want := capture(t, ws, base, 1<<20)
	artifacts := tempDir(t)

	must.NoError(WriteBaselineSidecar(context.Background(), base, artifacts))
	restored, err := LoadBaselineSidecar(context.Background(), filepath.Join(artifacts, BaselineSidecarName), filepath.Join(tempDir(t), "judge.git"))
	must.NoError(err)
	check.Equal(base.Rev, restored.Rev, "restored = %+v", restored)
	check.Equal(types.JudgeBaselineSidecar, restored.Source, "restored = %+v", restored)
	check.True(restored.recorded(), "restored = %+v", restored)

	got := capture(t, ws, restored, 1<<20)
	check.Equal(want.SHA256, got.SHA256, "restored baseline produced a different diff:\n%s", got.Head)
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
			must := require.New(t)
			dir := tempDir(t)
			writeFiles(t, dir, map[string]string{BaselineSidecarName: body})
			_, err := LoadBaselineSidecar(context.Background(), filepath.Join(dir, BaselineSidecarName), filepath.Join(tempDir(t), "judge.git"))
			must.Error(err)
		})
	}
}
