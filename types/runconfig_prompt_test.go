package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunConfig_EffectivePromptModel(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RunConfig)
		want   string
	}{
		{
			name:   "empty config falls back to DefaultModel",
			mutate: func(c *RunConfig) {},
			want:   DefaultModel,
		},
		{
			name: "static router model",
			mutate: func(c *RunConfig) {
				c.ModelRouter = ModelRouterConfig{Type: "static", Model: "claude-fable-5"}
			},
			want: "claude-fable-5",
		},
		{
			name: "promptModel override wins over router model",
			mutate: func(c *RunConfig) {
				c.ModelRouter = ModelRouterConfig{Type: "static", Model: "claude-fable-6"}
				c.PromptBuilder.PromptModel = "claude-fable-5"
			},
			want: "claude-fable-5",
		},
		{
			name: "per-mode router uses the mode's model, provider stripped",
			mutate: func(c *RunConfig) {
				c.Mode = "execution"
				c.ModelRouter = ModelRouterConfig{
					Type:       "per-mode",
					Model:      "claude-sonnet-5",
					ModeModels: map[string]string{"execution": "openai/gpt-5.6"},
				}
			},
			want: "gpt-5.6",
		},
		{
			name: "per-mode router bare model value",
			mutate: func(c *RunConfig) {
				c.Mode = "execution"
				c.ModelRouter = ModelRouterConfig{
					Type:       "per-mode",
					Model:      "claude-sonnet-5",
					ModeModels: map[string]string{"execution": "gpt-5.6"},
				}
			},
			want: "gpt-5.6",
		},
		{
			name: "per-mode router without an entry for the mode uses the default model",
			mutate: func(c *RunConfig) {
				c.Mode = "execution"
				c.ModelRouter = ModelRouterConfig{
					Type:       "per-mode",
					Model:      "claude-sonnet-5",
					ModeModels: map[string]string{"planning": "openai/gpt-5.6"},
				}
			},
			want: "claude-sonnet-5",
		},
		{
			name: "dynamic router uses the default model, not cheap or expensive",
			mutate: func(c *RunConfig) {
				c.ModelRouter = ModelRouterConfig{
					Type:           "dynamic",
					Model:          "claude-sonnet-5",
					CheapModel:     "claude-haiku-5-5",
					ExpensiveModel: "claude-fable-5",
				}
			},
			want: "claude-sonnet-5",
		},
		{
			name: "promptModel override wins over per-mode entry",
			mutate: func(c *RunConfig) {
				c.Mode = "execution"
				c.ModelRouter = ModelRouterConfig{
					Type:       "per-mode",
					ModeModels: map[string]string{"execution": "openai/gpt-5.6"},
				}
				c.PromptBuilder.PromptModel = "claude-fable-5"
			},
			want: "claude-fable-5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			require.Equal(t, tt.want, c.EffectivePromptModel())
		})
	}
}

func TestRunConfig_EffectivePromptModel_NilReceiverUsesDefault(t *testing.T) {
	var c *RunConfig
	require.Equal(t, DefaultModel, c.EffectivePromptModel())
}

func TestValidateRunConfig_RejectsPromptModelWithSystemPromptOverride(t *testing.T) {
	c := validConfig()
	c.SystemPromptOverride = "You are a helpful agent."
	c.PromptBuilder.PromptModel = "claude-fable-5"
	err := ValidateRunConfig(c)
	require.Error(t, err)
	require.ErrorContains(t, err, "promptBuilder.promptModel")
}

func TestValidateRunConfig_RejectsTemplateWithSystemPromptOverride(t *testing.T) {
	c := validConfig()
	c.SystemPromptOverride = "You are a helpful agent."
	c.PromptBuilder.Template = "You are an agent for {{.Model}}."
	err := ValidateRunConfig(c)
	require.Error(t, err)
	require.ErrorContains(t, err, "promptBuilder.template")
}

func TestValidateRunConfig_RejectsTemplateSyntaxError(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.Template = "broken {{if .Tier}} unclosed"
	err := ValidateRunConfig(c)
	require.Error(t, err)
	require.ErrorContains(t, err, "text/template")
}

// An uncompiled prompt from an external prompt-management system uses
// "{{var}}" placeholders, which Go's template parser rejects as an
// undefined function. Validation must catch this before run start.
func TestValidateRunConfig_RejectsMustachePlaceholderInTemplate(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.Template = "You are an {{criticlevel}} movie critic"
	err := ValidateRunConfig(c)
	require.Error(t, err)
	require.ErrorContains(t, err, "promptBuilder.template")
}

func TestValidateRunConfig_AcceptsValidTemplate(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.Template = `You are a coding agent.{{if eq .Tier "frontier"}} Act when ready.{{end}}`
	require.NoError(t, ValidateRunConfig(c))
}

func TestValidateRunConfig_AcceptsPromptModelAlone(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.PromptModel = "claude-fable-5"
	require.NoError(t, ValidateRunConfig(c))
}
