package router

import (
	"context"
	"testing"
)

func TestPerModeRouter_KnownMode(t *testing.T) {
	r := NewPerModeRouter(
		ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		map[string]ModelSelection{
			"planning": {Provider: "anthropic", Model: "claude-opus-4-6"},
		},
	)

	sel := r.Select(context.Background(), RouterContext{Mode: "planning", Turn: 1})

	if sel.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", sel.Provider)
	}
	if sel.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6", sel.Model)
	}
}

func TestPerModeRouter_UnknownModeFallsBackToDefault(t *testing.T) {
	r := NewPerModeRouter(
		ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		map[string]ModelSelection{
			"planning": {Provider: "anthropic", Model: "claude-opus-4-6"},
		},
	)

	sel := r.Select(context.Background(), RouterContext{Mode: "unknown-mode", Turn: 3})

	if sel.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", sel.Provider)
	}
	if sel.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", sel.Model)
	}
}

func TestPerModeRouter_EmptyMapAlwaysReturnsDefault(t *testing.T) {
	def := ModelSelection{Provider: "bedrock", Model: "claude-haiku-3"}
	r := NewPerModeRouter(def, nil)

	modes := []string{"execution", "planning", "review", "research", "toil"}
	for _, mode := range modes {
		sel := r.Select(context.Background(), RouterContext{Mode: mode, Turn: 1})
		if sel != def {
			t.Errorf("Select(mode=%q) = %+v, want %+v", mode, sel, def)
		}
	}
}

func TestPerModeRouter_MultipleModesConfigured(t *testing.T) {
	r := NewPerModeRouter(
		ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		map[string]ModelSelection{
			"planning":  {Provider: "anthropic", Model: "claude-opus-4-6"},
			"toil":      {Provider: "anthropic", Model: "claude-haiku-3"},
			"execution": {Provider: "bedrock", Model: "claude-sonnet-4-6"},
		},
	)

	tests := []struct {
		mode     string
		wantProv string
		wantModel string
	}{
		{"planning", "anthropic", "claude-opus-4-6"},
		{"toil", "anthropic", "claude-haiku-3"},
		{"execution", "bedrock", "claude-sonnet-4-6"},
		{"review", "anthropic", "claude-sonnet-4-6"},   // unmapped, falls back to default
		{"research", "anthropic", "claude-sonnet-4-6"}, // unmapped, falls back to default
	}

	for _, tt := range tests {
		sel := r.Select(context.Background(), RouterContext{Mode: tt.mode, Turn: 1})
		if sel.Provider != tt.wantProv || sel.Model != tt.wantModel {
			t.Errorf("Select(mode=%q) = %s/%s, want %s/%s",
				tt.mode, sel.Provider, sel.Model, tt.wantProv, tt.wantModel)
		}
	}
}

func TestPerModeRouter_ImplementsInterface(t *testing.T) {
	var _ ModelRouter = (*PerModeRouter)(nil)
}

func TestPerModeRouter_DoesNotAliasModeMap(t *testing.T) {
	modeMap := map[string]ModelSelection{
		"planning": {Provider: "anthropic", Model: "claude-opus-4-6"},
	}
	r := NewPerModeRouter(
		ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		modeMap,
	)

	// Mutate the original map after construction.
	modeMap["planning"] = ModelSelection{Provider: "changed", Model: "changed"}

	sel := r.Select(context.Background(), RouterContext{Mode: "planning", Turn: 1})
	if sel.Provider != "anthropic" || sel.Model != "claude-opus-4-6" {
		t.Errorf("router was affected by mutation of input map: got %s/%s", sel.Provider, sel.Model)
	}
}
