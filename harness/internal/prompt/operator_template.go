package prompt

import (
	"context"
	"fmt"
	"strings"
	"text/template"
)

// NewTemplatePromptBuilder returns a ComposedPromptBuilder whose preamble
// is an operator-supplied Go text/template (promptBuilder.template),
// replacing the shipped mode prompt. The template renders against the
// same TemplateData surface as the shipped prompts, so model-conditional
// content and the promptModel override keep working for tuned prompts.
// The structural fragments (workspace path, turn budget, workspace tree,
// git status, and dynamic context with untrusted_context wrapping) are
// still appended, matching NewOverridePromptBuilder.
//
// The template is parsed and trial-rendered against trial — the run's
// resolved prompt model and mode — so execution-time errors (an unknown
// field, a misspelled method) surface at component construction instead
// of at run start. Rendering is pure string work: TemplateData exposes no
// filesystem, environment, or network reach.
func NewTemplatePromptBuilder(text string, trial TemplateData) (*ComposedPromptBuilder, error) {
	tmpl, err := template.New("promptBuilder.template").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse promptBuilder.template: %w", err)
	}
	if err := tmpl.Execute(&strings.Builder{}, trial); err != nil {
		return nil, fmt.Errorf("render promptBuilder.template for model %q, mode %q: %w", trial.Model, trial.Mode, err)
	}
	return NewComposedPromptBuilder(WithFragments(
		&operatorTemplateFragment{tmpl: tmpl},
		WorkspacePathFragment(),
		TurnBudgetFragment(),
		WorkspaceTreeFragment(),
		GitStatusFragment(),
		DynamicContextFragment(),
	)), nil
}

type operatorTemplateFragment struct {
	tmpl *template.Template
}

func (f *operatorTemplateFragment) Render(_ context.Context, pc PromptContext) (string, error) {
	var sb strings.Builder
	if err := f.tmpl.Execute(&sb, TemplateData{Model: pc.Model, Mode: pc.Mode}); err != nil {
		return "", fmt.Errorf("render promptBuilder.template: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
}
