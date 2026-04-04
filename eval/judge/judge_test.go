package judge

import (
	"context"
	"os"
	"path/filepath"
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

func TestDiffReview_NotImplemented(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "diff-review"}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail for unimplemented diff-review")
	}
	if v.Reason != "diff-review judge not yet implemented" {
		t.Fatalf("unexpected reason: %s", v.Reason)
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
