package prompt

import (
	"strings"
	"testing"
)

func trialData() TemplateData {
	return TemplateData{Model: "claude-fable-5", Mode: "execution"}
}

func TestNewTemplatePromptBuilder_RendersModelConditionals(t *testing.T) {
	b, err := NewTemplatePromptBuilder(
		`You are a tuned agent.{{if eq .Tier "frontier"}} Act when ready.{{end}}{{if .ModelIs "gemma*"}} Follow the loop.{{end}}`,
		trialData(),
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := b.Build(t.Context(), PromptContext{Mode: "execution", Model: "claude-fable-5"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "You are a tuned agent. Act when ready.") {
		t.Errorf("frontier branch not rendered: %q", got)
	}
	if strings.Contains(got, "Follow the loop.") {
		t.Errorf("open-weight branch rendered for a frontier model: %q", got)
	}

	got, err = b.Build(t.Context(), PromptContext{Mode: "execution", Model: "gemma-4"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Follow the loop.") {
		t.Errorf("ModelIs branch not rendered for gemma-4: %q", got)
	}
}

// The operator template replaces the mode prompt but keeps the structural
// fragments, matching NewOverridePromptBuilder's contract.
func TestNewTemplatePromptBuilder_KeepsStructuralFragments(t *testing.T) {
	b, err := NewTemplatePromptBuilder("Tuned preamble.", trialData())
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Build(t.Context(), PromptContext{
		Mode:           "execution",
		Model:          "claude-fable-5",
		Workspace:      t.TempDir(),
		MaxTurns:       7,
		DynamicContext: map[string]string{"ticket": "fix the bug"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Tuned preamble.") {
		t.Errorf("preamble missing: %q", got)
	}
	execBase := ModePrompts()["execution"]
	if strings.Contains(got, execBase) {
		t.Error("mode prompt should be replaced, not appended")
	}
	for _, want := range []string{"Working directory:", "Turn budget: 7", "<untrusted_context"} {
		if !strings.Contains(got, want) {
			t.Errorf("structural fragment %q missing from output", want)
		}
	}
}

func TestNewTemplatePromptBuilder_ParseError(t *testing.T) {
	_, err := NewTemplatePromptBuilder("broken {{if .Tier}} unclosed", trialData())
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// Mustache-style placeholders from an uncompiled external prompt (e.g.
// Langfuse "{{var}}") must fail at construction, not at run start.
func TestNewTemplatePromptBuilder_LangfusePlaceholderFails(t *testing.T) {
	_, err := NewTemplatePromptBuilder("You are an {{criticlevel}} movie critic", trialData())
	if err == nil {
		t.Fatal("expected error for mustache-style placeholder")
	}
}

// Execution errors (a field TemplateData does not have) surface from the
// trial render at construction.
func TestNewTemplatePromptBuilder_TrialRenderCatchesExecError(t *testing.T) {
	_, err := NewTemplatePromptBuilder("hello {{.NoSuchField}}", trialData())
	if err == nil {
		t.Fatal("expected trial-render error for unknown field")
	}
	if !strings.Contains(err.Error(), "claude-fable-5") {
		t.Errorf("expected error to name the trial model, got: %v", err)
	}
}
