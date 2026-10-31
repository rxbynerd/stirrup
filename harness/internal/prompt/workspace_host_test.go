package prompt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// hostRepo creates a host git repository holding one committed file and
// one untracked file, whose core.fsmonitor command creates the returned
// marker path whenever host git consults it.
func hostRepo(t *testing.T) (repo, marker string) {
	t.Helper()
	must := require.New(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo = t.TempDir()
	marker = filepath.Join(t.TempDir(), "fsmonitor-ran")
	hostGit(t, repo, "init", "-q")
	must.NoError(os.WriteFile(filepath.Join(repo, "host-tracked.txt"), []byte("tracked\n"), 0o644))
	hostGit(t, repo, "add", "host-tracked.txt")
	hostGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "seed")
	must.NoError(os.WriteFile(filepath.Join(repo, "host-untracked.txt"), []byte("untracked\n"), 0o644))
	hostGit(t, repo, "config", "core.fsmonitor", "touch '"+marker+"'; echo /")
	return repo, marker
}

func hostGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	must.NoError(err, "git %v\n%s", args, out)
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	must := require.New(t)
	_, err := os.Stat(path)
	must.ErrorIs(err, os.ErrNotExist, "%s must not exist; a planted fsmonitor payload would create it on the host", path)
}

func TestComposedPrompt_PlantedFSMonitorDoesNotRunForSandboxExecutor(t *testing.T) {
	must := require.New(t)
	repo, marker := hostRepo(t)

	// Host git consults the planted command, so the plant is armed.
	hostGit(t, repo, "status", "--short")
	_, err := os.Stat(marker)
	must.NoError(err, "planted fsmonitor did not run under host git status; the test would prove nothing")
	must.NoError(os.Remove(marker))

	ws := newFakeWorkspace("/workspace")
	ws.entries = []string{"sandbox-file.go"}
	ws.toplevel = "/workspace"
	ws.status = "?? sandbox-file.go\n"

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Workspace: repo, Executor: ws})
	must.NoError(err)

	assertNoFile(t, marker)
	for _, tc := range []struct{ name, want string }{
		{"workspace listing", "Workspace files:\n  sandbox-file.go"},
		{"git status", "Git status:\n?? sandbox-file.go"},
	} {
		t.Run("includes "+tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(got, tc.want)
		})
	}
	for _, hostFile := range []string{"host-tracked", "host-untracked"} {
		t.Run("omits "+hostFile, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(got, hostFile)
		})
	}
}

func TestGitStatusFragment_LocalExecutorIgnoresPlantedFSMonitor(t *testing.T) {
	must := require.New(t)
	repo, marker := hostRepo(t)
	local, err := executor.NewLocalExecutor(repo)
	must.NoError(err)

	got, err := GitStatusFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: local})
	must.NoError(err)
	assertNoFile(t, marker)
	must.Equal("Git status:\n?? host-untracked.txt", got)
}

func TestComposedPrompt_LocalExecutorIgnoresEnclosingRepository(t *testing.T) {
	must := require.New(t)
	repo, marker := hostRepo(t)
	sub := filepath.Join(repo, "nested")
	must.NoError(os.Mkdir(sub, 0o755))
	must.NoError(os.WriteFile(filepath.Join(sub, "nested.go"), []byte("package nested\n"), 0o644))
	local, err := executor.NewLocalExecutor(sub)
	must.NoError(err)

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Workspace: sub, Executor: local})
	must.NoError(err)
	assertNoFile(t, marker)
	must.Contains(got, "Workspace files:\n  nested.go")
	for _, enclosing := range []string{"Git status", "host-untracked", "host-tracked"} {
		t.Run("omits "+enclosing, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(got, enclosing)
		})
	}
}
