package prompt

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultPromptBuilder_AllModes(t *testing.T) {
	b := NewDefaultPromptBuilder()

	modes := []struct {
		mode     string
		contains string
	}{
		{"execution", "coding agent"},
		{"planning", "step-by-step implementation plan"},
		{"review", "Review the following changes"},
		{"research", "Research the following topic"},
		{"toil", "trigger"},
	}

	for _, tc := range modes {
		t.Run(tc.mode, func(t *testing.T) {
			result, err := b.Build(context.Background(), PromptContext{Mode: tc.mode})
			if err != nil {
				t.Fatalf("Build() error: %v", err)
			}
			if !strings.Contains(result, tc.contains) {
				t.Errorf("prompt for mode %q does not contain %q:\n%s", tc.mode, tc.contains, result)
			}
		})
	}
}

func TestDefaultPromptBuilder_UnknownMode(t *testing.T) {
	b := NewDefaultPromptBuilder()
	_, err := b.Build(context.Background(), PromptContext{Mode: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown mode, got nil")
	}
}

func TestDefaultPromptBuilder_DynamicContext(t *testing.T) {
	b := NewDefaultPromptBuilder()

	result, err := b.Build(context.Background(), PromptContext{
		Mode: "execution",
		DynamicContext: map[string]string{
			"issue":   "Fix the login bug",
			"pr_diff": "--- a/main.go\n+++ b/main.go",
		},
	})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	if !strings.Contains(result, `<untrusted_context name="issue">Fix the login bug</untrusted_context>`) {
		t.Error("missing issue untrusted_context block")
	}
	if !strings.Contains(result, `<untrusted_context name="pr_diff">`) {
		t.Error("missing pr_diff untrusted_context block")
	}
}

func TestDefaultPromptBuilder_DynamicContextOrder(t *testing.T) {
	b := NewDefaultPromptBuilder()

	result, err := b.Build(context.Background(), PromptContext{
		Mode: "execution",
		DynamicContext: map[string]string{
			"zebra": "z",
			"alpha": "a",
		},
	})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	alphaIdx := strings.Index(result, `name="alpha"`)
	zebraIdx := strings.Index(result, `name="zebra"`)
	if alphaIdx == -1 || zebraIdx == -1 {
		t.Fatal("missing context blocks")
	}
	if alphaIdx > zebraIdx {
		t.Error("dynamic context keys should be sorted alphabetically")
	}
}

func TestDefaultPromptBuilder_NoDynamicContext(t *testing.T) {
	b := NewDefaultPromptBuilder()

	result, err := b.Build(context.Background(), PromptContext{Mode: "execution"})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	if strings.Contains(result, "untrusted_context") {
		t.Error("should not contain untrusted_context when DynamicContext is nil")
	}
}

func TestDefaultPromptBuilder_ImplementsInterface(t *testing.T) {
	var _ PromptBuilder = (*DefaultPromptBuilder)(nil)
}
