package judge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitTestIdentity...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh %q: %v\n%s", script, err, out)
	}
}

// gitCmd runs git in dir and returns its output.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitTestIdentity...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
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
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	base, err := CreateBaseline(context.Background(), ws, filepath.Join(tempDir(t), "judge.git"))
	if err != nil {
		t.Fatalf("CreateBaseline: %v", err)
	}
	return base
}

// changedWorkspace has one modified, one deleted and one untracked file
// relative to its baseline.
func changedWorkspace(t *testing.T) (string, Baseline) {
	t.Helper()
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n", "gone.txt": "bye\n"})
	writeFiles(t, ws, map[string]string{"a.txt": "one\ntwo\n", "new.txt": "brand new\n"})
	if err := os.Remove(filepath.Join(ws, "gone.txt")); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func capture(t *testing.T, ws string, base Baseline, maxBytes int) workspaceDiff {
	t.Helper()
	got, err := captureWorkspaceDiff(context.Background(), ws, base, maxBytes)
	if err != nil {
		t.Fatalf("captureWorkspaceDiff: %v", err)
	}
	return got
}
