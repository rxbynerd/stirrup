package prompt

import (
	"context"
	"fmt"
	"strings"
	"text/template"
)

// NewTemplatePromptBuilder returns a ComposedPromptBuilder whose preamble
// is an operator-supplied Go text/template (promptBuilder.template),
// replacing the shipped mode prompt but keeping the structural fragments.
// It is parsed and trial-rendered against trial so execution-time errors
// surface at construction rather than at run start. See "Operator
// templates and the override" in docs/configuration.md.
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
