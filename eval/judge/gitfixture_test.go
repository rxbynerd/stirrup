package judge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireGit skips the test when git or sh is unavailable and isolates the
// test's own git invocations from the host's git configuration.
func requireGit(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
}

var gitTestIdentity = []string{
	"GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=agent@example.invalid",
	"GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=agent@example.invalid",
}

// shIn runs script with sh in dir, playing the agent.
func shIn(t *testing.T, dir, script string) {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitTestIdentity...)
	out, err := cmd.CombinedOutput()
	must.NoError(err, "sh %q\n%s", script, out)
}

// gitCmd runs git in dir and returns its output.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitTestIdentity...)
	out, err := cmd.CombinedOutput()
	must.NoError(err, "git %s\n%s", strings.Join(args, " "), out)
	return string(out)
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	must := require.New(t)
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		must.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(os.WriteFile(path, []byte(content), 0o644))
	}
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// tempDir is t.TempDir with symlinks resolved, so paths compare equal to
// the ones git reports.
func tempDir(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must.NoError(err)
	return dir
}

// newWorkspace seeds files into a fresh workspace with no .git and records
// them as a runner baseline in a judge-owned repository.
func newWorkspace(t *testing.T, files map[string]string) (string, Baseline) {
	t.Helper()
	requireGit(t)
	ws := tempDir(t)
	writeFiles(t, ws, files)
	return ws, baselineOf(t, ws)
}

// baselineOf records ws's current content as a runner baseline.
func baselineOf(t *testing.T, ws string) Baseline {
	t.Helper()
	must := require.New(t)
	base, err := CreateBaseline(context.Background(), ws, filepath.Join(tempDir(t), "judge.git"))
	must.NoError(err, "CreateBaseline(%s)", ws)
	return base
}

// changedWorkspace has one modified, one deleted and one untracked file
// relative to its baseline.
func changedWorkspace(t *testing.T) (string, Baseline) {
	t.Helper()
	must := require.New(t)
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n", "gone.txt": "bye\n"})
	writeFiles(t, ws, map[string]string{"a.txt": "one\ntwo\n", "new.txt": "brand new\n"})
	must.NoError(os.Remove(filepath.Join(ws, "gone.txt")))
	return ws, base
}

// gitRepoWorkspace is a workspace that is itself a git repository with one
// commit holding files, as after a `repo` task's clone.
func gitRepoWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	requireGit(t)
	ws := tempDir(t)
	writeFiles(t, ws, files)
	gitCmd(t, ws, "init", "-q")
	gitCmd(t, ws, "add", "-A")
	gitCmd(t, ws, "commit", "-q", "--allow-empty", "-m", "upstream")
	return ws
}

// countObjects returns the number of files under the repository's object
// store.
func countObjects(t *testing.T, gitDir string) int {
	t.Helper()
	must := require.New(t)
	n := 0
	err := filepath.WalkDir(filepath.Join(gitDir, "objects"), func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	must.NoError(err)
	return n
}

func capture(t *testing.T, ws string, base Baseline, maxBytes int) workspaceDiff {
	t.Helper()
	must := require.New(t)
	got, err := captureWorkspaceDiff(context.Background(), ws, base, maxBytes)
	must.NoError(err, "captureWorkspaceDiff(%s)", ws)
	return got
}
