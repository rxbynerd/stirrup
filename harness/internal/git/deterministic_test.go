package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	// Verify we are on the expected branch.
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

	// Create a file so there is something to commit.
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := gs.Checkpoint(ctx, "add hello"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Verify the commit message.
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

	// Get the initial commit count.
	before := commitCount(t, repo)

	// Checkpoint with no changes should succeed silently.
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

	// Make a commit so the SHA differs from the initial one.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gs.Checkpoint(ctx, "checkpoint"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	result, err := gs.Finalise(ctx)
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	if result.Branch != "stirrup/task-3" {
		t.Errorf("branch = %q, want %q", result.Branch, "stirrup/task-3")
	}

	// Verify the SHA matches what git reports.
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

// commitCount returns the number of commits in the repo.
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

// trimOutput trims whitespace from command output.
func trimOutput(b []byte) string {
	return strings.TrimSpace(string(b))
}
