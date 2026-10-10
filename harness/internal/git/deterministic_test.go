package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// initTestRepo creates a temporary git repository with an initial commit
// and returns the path to it.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("init repo %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestSetup_CreatesBranch(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-42"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	got := trimOutput(out)
	if got != "stirrup/task-42" {
		t.Errorf("branch = %q, want %q", got, "stirrup/task-42")
	}
}

func TestCheckpoint_CommitsChanges(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-1"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := gs.Checkpoint(ctx, "add hello"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	cmd := exec.Command("git", "log", "-1", "--format=%s")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := trimOutput(out); got != "add hello" {
		t.Errorf("commit message = %q, want %q", got, "add hello")
	}
}

func TestCheckpoint_NoChanges_IsNoOp(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-2"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	before := commitCount(t, repo)

	if err := gs.Checkpoint(ctx, "nothing"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	after := commitCount(t, repo)
	if after != before {
		t.Errorf("commit count changed from %d to %d; expected no new commits", before, after)
	}
}

func TestFinalise_ReturnsBranchAndSHA(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-3"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gs.Checkpoint(ctx, "checkpoint"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	result, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	if result.Branch != "stirrup/task-3" {
		t.Errorf("branch = %q, want %q", result.Branch, "stirrup/task-3")
	}

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	wantSHA := trimOutput(out)
	if result.SHA != wantSHA {
		t.Errorf("SHA = %q, want %q", result.SHA, wantSHA)
	}
}

func TestFinalise_CommitsOutstandingWork(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	base := gitOut(t, repo, "rev-parse", "HEAD")

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-final"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "partial.txt"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	handoff, err := gs.Finalise(ctx, "budget_exceeded")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	head := gitOut(t, repo, "rev-parse", "HEAD")
	if head == base {
		t.Fatal("HEAD did not move; outstanding work was not committed")
	}
	if got := gitOut(t, repo, "log", "-1", "--format=%s"); got != "Final checkpoint (outcome: budget_exceeded)" {
		t.Errorf("commit message = %q", got)
	}
	if got := gitOut(t, repo, "ls-tree", "--name-only", "HEAD", "partial.txt"); got != "partial.txt" {
		t.Errorf("partial.txt not in HEAD tree (ls-tree = %q)", got)
	}
	want := types.GitHandoff{Branch: "stirrup/task-final", SHA: head, BaseSHA: base}
	if *handoff != want {
		t.Errorf("handoff = %+v, want %+v", *handoff, want)
	}
}

func TestFinalise_NothingOutstanding_ReportsHEADWithoutCommit(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-clean"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	before := commitCount(t, repo)

	handoff, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if after := commitCount(t, repo); after != before {
		t.Errorf("commit count changed from %d to %d on a clean tree", before, after)
	}
	if handoff.SHA != handoff.BaseSHA || handoff.SHA == "" {
		t.Errorf("handoff = %+v, want SHA equal to a non-empty BaseSHA", *handoff)
	}
	if handoff.Dirty {
		t.Error("Dirty = true on a clean tree")
	}
}

func TestFinalise_NoConfiguredIdentity_CommitsAsHarness(t *testing.T) {
	isolateGitConfig(t)
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.useConfigOnly", "true")
	runGit(t, dir, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "--allow-empty", "-m", "initial")

	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, dir, "add", "probe.txt")
	probe := exec.Command("git", "commit", "-m", "probe")
	probe.Dir = dir
	if out, err := probe.CombinedOutput(); err == nil {
		t.Fatalf("precondition: plain git commit succeeded without an identity:\n%s", out)
	}

	ctx := context.Background()
	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, dir, "task-noid"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gs.Checkpoint(ctx, "turn"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "more.txt"), []byte("more"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	handoff, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if handoff.Dirty {
		t.Errorf("Dirty = true after the final checkpoint")
	}
	want := HarnessIdentityName + " <" + HarnessIdentityEmail + ">"
	for _, rev := range []string{"HEAD", "HEAD~1"} {
		if got := gitOut(t, dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>", rev); got != want+"|"+want {
			t.Errorf("%s author|committer = %q, want %q for both", rev, got, want)
		}
	}
}

func TestFinalise_CommitFailure_ReturnsErrorWithState(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	base := gitOut(t, repo, "rev-parse", "HEAD")

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-fail"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stuck.txt"), []byte("stuck"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// A held index lock makes staging fail the way a concurrent git
	// process would.
	if err := os.WriteFile(filepath.Join(repo, ".git", "index.lock"), nil, 0o644); err != nil {
		t.Fatalf("write index.lock: %v", err)
	}

	handoff, err := gs.Finalise(ctx, "success")
	if err == nil {
		t.Fatal("Finalise returned nil error with the index locked")
	}
	if !strings.Contains(err.Error(), "final checkpoint") {
		t.Errorf("error = %q, want it to name the final checkpoint", err)
	}
	if handoff == nil {
		t.Fatal("Finalise returned a nil handoff alongside its error")
	}
	if handoff.SHA != base || handoff.BaseSHA != base || handoff.Branch != "stirrup/task-fail" {
		t.Errorf("handoff = %+v, want branch stirrup/task-fail at base %s", *handoff, base)
	}
	if !handoff.Dirty {
		t.Error("Dirty = false with an uncommitted file")
	}
}

func TestFinalise_EmbeddedRepositoryChanges_ReportDirty(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, repo, "task-dirty"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, nested, "init")
	runGit(t, nested, "-c", "user.name=n", "-c", "user.email=n@example.com", "commit", "--allow-empty", "-m", "nested")
	if err := os.WriteFile(filepath.Join(nested, "inner.txt"), []byte("inner"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	handoff, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if !handoff.Dirty {
		t.Error("Dirty = false although the embedded repository has uncommitted content")
	}
	if handoff.SHA == handoff.BaseSHA {
		t.Error("final checkpoint made no commit for the embedded repository")
	}
}

func TestFinalise_UnbornBranch_CommitsWithEmptyBase(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	ctx := context.Background()

	gs := NewDeterministicGitStrategy()
	if err := gs.Setup(ctx, dir, "task-unborn"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	handoff, err := gs.Finalise(ctx, "max_turns")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if handoff.BaseSHA != "" {
		t.Errorf("BaseSHA = %q, want empty for an unborn branch", handoff.BaseSHA)
	}
	if handoff.SHA != gitOut(t, dir, "rev-parse", "HEAD") || handoff.Branch != "stirrup/task-unborn" {
		t.Errorf("handoff = %+v, want the new HEAD on stirrup/task-unborn", *handoff)
	}
}

func TestFinalise_BeforeSetup_ReturnsError(t *testing.T) {
	handoff, err := NewDeterministicGitStrategy().Finalise(context.Background(), "success")
	if err == nil {
		t.Fatal("Finalise before Setup returned nil error")
	}
	if handoff == nil {
		t.Fatal("Finalise before Setup returned a nil handoff")
	}
}

// isolateGitConfig hides the user's and the system's git configuration, and
// any identity in the environment, from git commands the test runs.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return trimOutput(out)
}

func commitCount(t *testing.T, repo string) int {
	t.Helper()
	cmd := exec.Command("git", "rev-list", "--count", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-list --count: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(trimOutput(out), "%d", &n); err != nil {
		t.Fatalf("parse count: %v", err)
	}
	return n
}

func trimOutput(b []byte) string {
	return strings.TrimSpace(string(b))
}
