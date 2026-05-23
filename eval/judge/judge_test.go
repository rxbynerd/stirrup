package judge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

func TestTestCommand_Pass(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command", Command: "echo ok"}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
}

func TestTestCommand_Fail(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command", Command: "exit 1"}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail, got pass")
	}
}

func TestTestCommand_Timeout(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	j := types.EvalJudge{Type: "test-command", Command: "sleep 60"}
	v, err := Evaluate(ctx, j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail on timeout, got pass")
	}
}

func TestTestCommand_EmptyCommand(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestFileExists_AllExist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")

	j := types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt", "b.txt"}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
}

func TestFileExists_SomeMissing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")

	j := types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt", "missing.txt"}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail for missing file")
	}
	if v.Reason != "missing paths: missing.txt" {
		t.Fatalf("unexpected reason: %s", v.Reason)
	}
}

func TestFileExists_EmptyPaths(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-exists", Paths: []string{}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass for empty paths, got fail: %s", v.Reason)
	}
}

func TestFileContains_Match(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello world 123")

	j := types.EvalJudge{Type: "file-contains", Path: "hello.txt", Pattern: `world \d+`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
}

func TestFileContains_NoMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello world")

	j := types.EvalJudge{Type: "file-contains", Path: "hello.txt", Pattern: `goodbye`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail for non-matching pattern")
	}
}

func TestFileContains_FileNotExist(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-contains", Path: "nope.txt", Pattern: `anything`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail for missing file")
	}
}

func TestComposite_AllPass(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "content")

	j := types.EvalJudge{
		Type:    "composite",
		Require: "all",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "true"},
			{Type: "file-exists", Paths: []string{"a.txt"}},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
	if len(v.Details) != 2 {
		t.Fatalf("expected 2 details, got %d", len(v.Details))
	}
}

func TestComposite_AllRequiredOneFails(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type:    "composite",
		Require: "all",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "true"},
			{Type: "test-command", Command: "false"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when one sub-judge fails with require=all")
	}
}

func TestComposite_AnyOnePasses(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type:    "composite",
		Require: "any",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "false"},
			{Type: "test-command", Command: "true"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass with require=any, got fail: %s", v.Reason)
	}
}

func TestComposite_AnyAllFail(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type:    "composite",
		Require: "any",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "false"},
			{Type: "test-command", Command: "exit 1"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when all sub-judges fail with require=any")
	}
}

func TestComposite_DefaultRequireAll(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type: "composite",
		// Require omitted, should default to "all"
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "true"},
			{Type: "test-command", Command: "false"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail with default require=all and one failing sub-judge")
	}
}

// TestDiffReview_RequiresCriteria pins the input-validation contract:
// the diff-review judge fails fast when its criteria string is empty,
// without trying to make any API call. This is the "operator forgot
// to fill in the judge block" error path.
func TestDiffReview_RequiresCriteria(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "diff-review"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error when criteria is empty")
	}
	if !strings.Contains(err.Error(), "criteria") {
		t.Errorf("error should mention criteria: %v", err)
	}
}

// TestDiffReview_RequiresAPIKey pins the missing-secret error path:
// with criteria set but ANTHROPIC_API_KEY unset, the judge returns
// a clear error rather than crashing or silently passing.
func TestDiffReview_RequiresAPIKey(t *testing.T) {
	// Initialize a workspace as a git repo so captureDiff succeeds
	// before the api-key check fires. Without `git init` the
	// `git diff HEAD` call fails first; we want the test to pin
	// the api-key error, not the missing-repo error.
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Skipf("git init unavailable: %v", err)
	}
	if err := exec.Command("git", "-C", dir, "commit", "--allow-empty", "-m", "init", "--quiet").Run(); err != nil {
		// commit may fail on a system without git user config;
		// give the test a chance to still exercise the api-key
		// check by trying without it.
		t.Logf("git commit failed (continuing): %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	j := types.EvalJudge{Type: "diff-review", Criteria: "be good"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error when ANTHROPIC_API_KEY is unset")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error should mention the env var: %v", err)
	}
}

func TestPathTraversal_FileExists(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-exists", Paths: []string{"../../../etc/passwd"}}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
}

func TestPathTraversal_FileContains(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-contains", Path: "../../../etc/passwd", Pattern: "root"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
}

func TestUnknownJudgeType(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "nonexistent"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for unknown judge type")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating parent dirs: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
