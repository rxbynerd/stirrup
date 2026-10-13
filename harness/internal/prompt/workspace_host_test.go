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

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// TestPackageDoesNotTouchHost pins that no production file in this package
// can read the host filesystem or start a host process: the workspace
// fragments must read through the executor.
func TestPackageDoesNotTouchHost(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	checked := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			if path == "os" || path == "os/exec" {
				t.Errorf("%s imports %s; workspace reads must go through the executor", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no production files checked; the import guard would be vacuous")
	}
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
	if err := os.WriteFile(filepath.Join(repo, "host-tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostGit(t, repo, "add", "host-tracked.txt")
	hostGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "seed")
	if err := os.WriteFile(filepath.Join(repo, "host-untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostGit(t, repo, "config", "core.fsmonitor", "touch '"+marker+"'; echo /")
	return repo, marker
}

func hostGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists: the planted fsmonitor payload ran on the host", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func TestComposedPrompt_PlantedFSMonitorDoesNotRunForSandboxExecutor(t *testing.T) {
	repo, marker := hostRepo(t)

	// Host git consults the planted command, so the plant is armed.
	hostGit(t, repo, "status", "--short")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("planted fsmonitor did not run under host git status (%v); the test would prove nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	ws := newFakeWorkspace("/workspace")
	ws.entries = []string{"sandbox-file.go"}
	ws.toplevel = "/workspace"
	ws.status = "?? sandbox-file.go\n"

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Workspace: repo, Executor: ws})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	assertNoFile(t, marker)
	for _, want := range []string{"Workspace files:\n  sandbox-file.go", "Git status:\n?? sandbox-file.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("composed prompt missing sandbox content %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "host-tracked") || strings.Contains(got, "host-untracked") {
		t.Errorf("composed prompt carries host workspace file names:\n%s", got)
	}
}

func TestGitStatusFragment_LocalExecutorIgnoresPlantedFSMonitor(t *testing.T) {
	repo, marker := hostRepo(t)
	local, err := executor.NewLocalExecutor(repo)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	got, err := GitStatusFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: local})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	assertNoFile(t, marker)
	if got != "Git status:\n?? host-untracked.txt" {
		t.Errorf("Render() = %q, want the untracked file", got)
	}
}

func TestComposedPrompt_LocalExecutorInsideEnclosingRepository(t *testing.T) {
	repo, marker := hostRepo(t)
	sub := filepath.Join(repo, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested.go"), []byte("package nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := executor.NewLocalExecutor(sub)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Workspace: sub, Executor: local})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	assertNoFile(t, marker)
	if !strings.Contains(got, "Workspace files:\n  nested.go") {
		t.Errorf("composed prompt missing the workspace listing:\n%s", got)
	}
	if strings.Contains(got, "Git status") || strings.Contains(got, "host-untracked") || strings.Contains(got, "host-tracked") {
		t.Errorf("composed prompt carries the enclosing repository's status:\n%s", got)
	}
}
