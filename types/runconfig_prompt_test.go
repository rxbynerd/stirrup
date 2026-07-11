package types

import (
	"strings"
	"testing"
)

func TestEffectivePromptModel(t *testing.T) {
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
					CheapModel:     "claude-haiku-4-5",
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
			if got := c.EffectivePromptModel(); got != tt.want {
				t.Errorf("EffectivePromptModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectivePromptModel_NilReceiver(t *testing.T) {
	var c *RunConfig
	if got := c.EffectivePromptModel(); got != DefaultModel {
		t.Errorf("EffectivePromptModel() on nil = %q, want %q", got, DefaultModel)
	}
}

func TestValidateRunConfig_PromptModelWithSystemPromptOverride(t *testing.T) {
	c := validConfig()
	c.SystemPromptOverride = "You are a helpful agent."
	c.PromptBuilder.PromptModel = "claude-fable-5"
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for promptModel combined with systemPromptOverride")
	}
	if !strings.Contains(err.Error(), "promptBuilder.promptModel") {
		t.Errorf("expected error to mention promptBuilder.promptModel, got: %v", err)
	}
}

func TestValidateRunConfig_TemplateWithSystemPromptOverride(t *testing.T) {
	c := validConfig()
	c.SystemPromptOverride = "You are a helpful agent."
	c.PromptBuilder.Template = "You are an agent for {{.Model}}."
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for template combined with systemPromptOverride")
	}
	if !strings.Contains(err.Error(), "promptBuilder.template") {
		t.Errorf("expected error to mention promptBuilder.template, got: %v", err)
	}
}

func TestValidateRunConfig_TemplateSyntaxError(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.Template = "broken {{if .Tier}} unclosed"
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unparseable template")
	}
	if !strings.Contains(err.Error(), "text/template") {
		t.Errorf("expected error to mention text/template, got: %v", err)
	}
}

// An uncompiled prompt from an external prompt-management system uses
// "{{var}}" placeholders, which Go's template parser rejects as an
// undefined function. Validation must catch this before run start.
func TestValidateRunConfig_TemplateLangfusePlaceholder(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.Template = "You are an {{criticlevel}} movie critic"
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for mustache-style placeholder in template")
	}
	if !strings.Contains(err.Error(), "promptBuilder.template") {
		t.Errorf("expected error to mention promptBuilder.template, got: %v", err)
	}
}

func TestValidateRunConfig_ValidTemplateAccepted(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.Template = `You are a coding agent.{{if eq .Tier "frontier"}} Act when ready.{{end}}`
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected valid template to be accepted, got: %v", err)
	}
}

func TestValidateRunConfig_PromptModelAloneAccepted(t *testing.T) {
	c := validConfig()
	c.PromptBuilder.PromptModel = "claude-fable-5"
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected promptModel without override to be accepted, got: %v", err)
	}
}
