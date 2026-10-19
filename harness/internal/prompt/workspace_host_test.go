package prompt

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// TestPackage_ProductionFilesAvoidHostImports pins that no production file in this package
// can read the host filesystem or start a host process: the workspace
// fragments must read through the executor.
func TestPackage_ProductionFilesAvoidHostImports(t *testing.T) {
	hostPackages := []string{"os", "os/exec"}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		require.NoError(t, err, "parse %s", name)
		checked++
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err, "%s: unquote import %s", name, imp.Path.Value)
			assert.NotContains(t, hostPackages, path, "%s imports %s; workspace reads must go through the executor", name, path)
		}
	}
	require.NotZero(t, checked, "no production files checked; the import guard would be vacuous")
}

// hostRepo creates a host git repository holding one committed file and
// one untracked file, whose core.fsmonitor command creates the returned
// marker path whenever host git consults it.
func hostRepo(t *testing.T) (repo, marker string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo = t.TempDir()
	marker = filepath.Join(t.TempDir(), "fsmonitor-ran")
	hostGit(t, repo, "init", "-q")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "host-tracked.txt"), []byte("tracked\n"), 0o644))
	hostGit(t, repo, "add", "host-tracked.txt")
	hostGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "seed")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "host-untracked.txt"), []byte("untracked\n"), 0o644))
	hostGit(t, repo, "config", "core.fsmonitor", "touch '"+marker+"'; echo /")
	return repo, marker
}

func hostGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v\n%s", args, out)
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "%s must not exist; a planted fsmonitor payload would create it on the host", path)
}

func TestComposedPrompt_PlantedFSMonitorDoesNotRunForSandboxExecutor(t *testing.T) {
	repo, marker := hostRepo(t)

	// Host git consults the planted command, so the plant is armed.
	hostGit(t, repo, "status", "--short")
	_, err := os.Stat(marker)
	require.NoError(t, err, "planted fsmonitor did not run under host git status; the test would prove nothing")
	require.NoError(t, os.Remove(marker))

	ws := newFakeWorkspace("/workspace")
	ws.entries = []string{"sandbox-file.go"}
	ws.toplevel = "/workspace"
	ws.status = "?? sandbox-file.go\n"

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Workspace: repo, Executor: ws})
	require.NoError(t, err)

	assertNoFile(t, marker)
	for _, tc := range []struct{ name, want string }{
		{"workspace listing", "Workspace files:\n  sandbox-file.go"},
		{"git status", "Git status:\n?? sandbox-file.go"},
	} {
		t.Run("includes "+tc.name, func(t *testing.T) {
			require.Contains(t, got, tc.want)
		})
	}
	for _, hostFile := range []string{"host-tracked", "host-untracked"} {
		t.Run("omits "+hostFile, func(t *testing.T) {
			require.NotContains(t, got, hostFile)
		})
	}
}

func TestGitStatusFragment_LocalExecutorIgnoresPlantedFSMonitor(t *testing.T) {
	repo, marker := hostRepo(t)
	local, err := executor.NewLocalExecutor(repo)
	require.NoError(t, err)

	got, err := GitStatusFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: local})
	require.NoError(t, err)
	assertNoFile(t, marker)
	require.Equal(t, "Git status:\n?? host-untracked.txt", got)
}

func TestComposedPrompt_LocalExecutorIgnoresEnclosingRepository(t *testing.T) {
	repo, marker := hostRepo(t)
	sub := filepath.Join(repo, "nested")
	require.NoError(t, os.Mkdir(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "nested.go"), []byte("package nested\n"), 0o644))
	local, err := executor.NewLocalExecutor(sub)
	require.NoError(t, err)

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Workspace: sub, Executor: local})
	require.NoError(t, err)
	assertNoFile(t, marker)
	require.Contains(t, got, "Workspace files:\n  nested.go")
	for _, enclosing := range []string{"Git status", "host-untracked", "host-tracked"} {
		t.Run("omits "+enclosing, func(t *testing.T) {
			require.NotContains(t, got, enclosing)
		})
	}
}
