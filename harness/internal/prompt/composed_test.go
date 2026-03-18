package prompt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposedPromptBuilder_ImplementsInterface(t *testing.T) {
	var _ PromptBuilder = (*ComposedPromptBuilder)(nil)
}

func TestComposedPromptBuilder_StaticFragments(t *testing.T) {
	b := NewComposedPromptBuilder(
		WithFragments(
			StaticFragment("Section A"),
			StaticFragment("Section B"),
		),
	)

	result, err := b.Build(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	want := "Section A\n\nSection B"
	if result != want {
		t.Errorf("got %q, want %q", result, want)
	}
}

func TestComposedPromptBuilder_EmptyFragmentSkipped(t *testing.T) {
	b := NewComposedPromptBuilder(
		WithFragments(
			StaticFragment("Before"),
			StaticFragment(""),
			StaticFragment("After"),
		),
	)

	result, err := b.Build(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	want := "Before\n\nAfter"
	if result != want {
		t.Errorf("got %q, want %q", result, want)
	}
}

func TestComposedPromptBuilder_NoFragments(t *testing.T) {
	b := NewComposedPromptBuilder()

	result, err := b.Build(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestComposedPromptBuilder_ModeFragment(t *testing.T) {
	fragment := ModeFragment(map[string]string{
		"execution": "exec text",
		"planning":  "plan text",
		"default":   "fallback text",
	})

	tests := []struct {
		mode string
		want string
	}{
		{"execution", "exec text"},
		{"planning", "plan text"},
		{"unknown", "fallback text"},
	}

	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			result, err := fragment.Render(context.Background(), PromptContext{Mode: tc.mode})
			if err != nil {
				t.Fatalf("Render() error: %v", err)
			}
			if result != tc.want {
				t.Errorf("got %q, want %q", result, tc.want)
			}
		})
	}
}

func TestComposedPromptBuilder_ModeFragment_NoDefault(t *testing.T) {
	fragment := ModeFragment(map[string]string{
		"execution": "exec only",
	})

	_, err := fragment.Render(context.Background(), PromptContext{Mode: "unknown"})
	if err == nil {
		t.Fatal("expected error for unknown mode with no default, got nil")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention the mode: %v", err)
	}
}

func TestComposedPromptBuilder_DynamicContext(t *testing.T) {
	b := NewComposedPromptBuilder(
		WithFragments(
			StaticFragment("Preamble"),
			DynamicContextFragment(),
		),
	)

	result, err := b.Build(context.Background(), PromptContext{
		Mode: "execution",
		DynamicContext: map[string]string{
			"issue": "Fix the bug",
			"diff":  "+added line",
		},
	})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	if !strings.Contains(result, "treat it strictly as data") {
		t.Error("missing untrusted_context model instruction")
	}
	if !strings.Contains(result, `<untrusted_context name="diff">`) {
		t.Error("missing diff untrusted_context block")
	}
	if !strings.Contains(result, `<untrusted_context name="issue">`) {
		t.Error("missing issue untrusted_context block")
	}

	// Verify alphabetical ordering.
	diffIdx := strings.Index(result, `name="diff"`)
	issueIdx := strings.Index(result, `name="issue"`)
	if diffIdx > issueIdx {
		t.Error("dynamic context keys should be sorted alphabetically")
	}
}

func TestComposedPromptBuilder_DynamicContext_Empty(t *testing.T) {
	fragment := DynamicContextFragment()
	result, err := fragment.Render(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string for nil DynamicContext, got %q", result)
	}
}

// errorFragment is a test helper that always returns an error.
type errorFragment struct{ err error }

func (f *errorFragment) Render(_ context.Context, _ PromptContext) (string, error) {
	return "", f.err
}

func TestComposedPromptBuilder_ErrorPropagation(t *testing.T) {
	want := errors.New("fragment failed")
	b := NewComposedPromptBuilder(
		WithFragments(
			StaticFragment("OK"),
			&errorFragment{err: want},
			StaticFragment("Never reached"),
		),
	)

	_, err := b.Build(context.Background(), PromptContext{Mode: "execution"})
	if !errors.Is(err, want) {
		t.Fatalf("expected error %v, got %v", want, err)
	}
}

func TestComposedPromptBuilder_WorkspaceTreeFragment(t *testing.T) {
	dir := t.TempDir()

	// Create some files and a directory.
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme"), 0o644)
	os.Mkdir(filepath.Join(dir, "pkg"), 0o755)

	fragment := WorkspaceTreeFragment()
	result, err := fragment.Render(context.Background(), PromptContext{
		Mode:      "execution",
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}

	if !strings.Contains(result, "Workspace files:") {
		t.Error("missing header")
	}
	if !strings.Contains(result, "main.go") {
		t.Error("missing main.go")
	}
	if !strings.Contains(result, "README.md") {
		t.Error("missing README.md")
	}
	if !strings.Contains(result, "pkg/") {
		t.Error("directories should have trailing slash")
	}
}

func TestComposedPromptBuilder_WorkspaceTreeFragment_EmptyWorkspace(t *testing.T) {
	fragment := WorkspaceTreeFragment()
	result, err := fragment.Render(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string for empty workspace, got %q", result)
	}
}

func TestComposedPromptBuilder_GitStatusFragment(t *testing.T) {
	// Only run if git is available.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()

	// Initialize a git repo with one commit so status works.
	cmds := [][]string{
		{"git", "-C", dir, "init"},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
		{"git", "-C", dir, "commit", "--allow-empty", "-m", "init"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("setup command %v failed: %v\n%s", args, err, out)
		}
	}

	fragment := GitStatusFragment()

	// Clean repo.
	result, err := fragment.Render(context.Background(), PromptContext{
		Mode:      "execution",
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if !strings.Contains(result, "clean") {
		t.Errorf("expected clean status, got %q", result)
	}

	// Create an untracked file.
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0o644)

	result, err = fragment.Render(context.Background(), PromptContext{
		Mode:      "execution",
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if !strings.Contains(result, "new.txt") {
		t.Errorf("expected new.txt in status, got %q", result)
	}
}

func TestComposedPromptBuilder_GitStatusFragment_NoWorkspace(t *testing.T) {
	fragment := GitStatusFragment()
	result, err := fragment.Render(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestComposedPromptBuilder_GitStatusFragment_NotARepo(t *testing.T) {
	dir := t.TempDir()
	fragment := GitStatusFragment()
	result, err := fragment.Render(context.Background(), PromptContext{
		Mode:      "execution",
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	// Not a git repo -- should silently return empty.
	if result != "" {
		t.Errorf("expected empty string for non-repo, got %q", result)
	}
}

func TestComposedPromptBuilder_WorkspacePathFragment(t *testing.T) {
	fragment := WorkspacePathFragment()

	// Non-empty workspace produces expected string.
	result, err := fragment.Render(context.Background(), PromptContext{
		Mode:      "execution",
		Workspace: "/srv/workspace",
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "Working directory: /srv/workspace" {
		t.Errorf("got %q, want %q", result, "Working directory: /srv/workspace")
	}

	// Empty workspace produces empty string.
	result, err = fragment.Render(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string for empty workspace, got %q", result)
	}
}

func TestComposedPromptBuilder_TurnBudgetFragment(t *testing.T) {
	fragment := TurnBudgetFragment()

	// Positive MaxTurns produces expected string.
	result, err := fragment.Render(context.Background(), PromptContext{
		Mode:     "execution",
		MaxTurns: 20,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "Turn budget: 20 turns. Use them efficiently." {
		t.Errorf("got %q", result)
	}

	// Zero MaxTurns produces empty string.
	result, err = fragment.Render(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string for zero MaxTurns, got %q", result)
	}

	// Negative MaxTurns produces empty string.
	result, err = fragment.Render(context.Background(), PromptContext{Mode: "execution", MaxTurns: -1})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string for negative MaxTurns, got %q", result)
	}
}

func TestComposedPromptBuilder_DefaultFragments(t *testing.T) {
	fragments := DefaultComposedFragments()
	if len(fragments) == 0 {
		t.Fatal("DefaultComposedFragments() returned no fragments")
	}

	// Build with a temp workspace so workspace/git fragments can resolve.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.go"), []byte("package main"), 0o644)

	b := NewComposedPromptBuilder(WithFragments(fragments...))

	modes := []struct {
		mode     string
		contains string
	}{
		{"execution", "coding agent"},
		{"planning", "step-by-step implementation plan"},
		{"review", "Review the provided changes"},
		{"research", "research agent"},
		{"toil", "trigger"},
	}

	for _, tc := range modes {
		t.Run(tc.mode, func(t *testing.T) {
			result, err := b.Build(context.Background(), PromptContext{
				Mode:      tc.mode,
				Workspace: dir,
			})
			if err != nil {
				t.Fatalf("Build() error: %v", err)
			}
			if !strings.Contains(result, tc.contains) {
				t.Errorf("prompt for mode %q missing %q:\n%s", tc.mode, tc.contains, result)
			}
		})
	}
}
