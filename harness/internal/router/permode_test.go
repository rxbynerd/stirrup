package router

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPerModeRouter_ConfiguredModeOverridesDefault(t *testing.T) {
	must := require.New(t)
	r := NewPerModeRouter(
		ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		map[string]ModelSelection{
			"planning": {Provider: "anthropic", Model: "claude-opus-4-6"},
		},
	)

	sel := r.Select(context.Background(), RouterContext{Mode: "planning", Turn: 1})

	must.Equal(ModelSelection{Provider: "anthropic", Model: "claude-opus-4-6"}, sel)
}

func TestPerModeRouter_UnknownModeFallsBackToDefault(t *testing.T) {
	must := require.New(t)
	r := NewPerModeRouter(
		ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		map[string]ModelSelection{
			"planning": {Provider: "anthropic", Model: "claude-opus-4-6"},
		},
	)

	sel := r.Select(context.Background(), RouterContext{Mode: "unknown-mode", Turn: 3})

	must.Equal(ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"}, sel)
}

func TestPerModeRouter_EmptyMapAlwaysReturnsDefault(t *testing.T) {
	def := ModelSelection{Provider: "bedrock", Model: "claude-haiku-3"}
	r := NewPerModeRouter(def, nil)

	modes := []string{"execution", "planning", "review", "research", "toil"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			sel := r.Select(context.Background(), RouterContext{Mode: mode, Turn: 1})

			must.Equal(def, sel)
		})
	}
}

func TestPerModeRouter_RoutesEachConfiguredMode(t *testing.T) {
	r := NewPerModeRouter(
		ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		map[string]ModelSelection{
			"planning":  {Provider: "anthropic", Model: "claude-opus-4-6"},
			"toil":      {Provider: "anthropic", Model: "claude-haiku-3"},
			"execution": {Provider: "bedrock", Model: "claude-sonnet-4-6"},
		},
	)

	tests := []struct {
		name string
		mode string
		want ModelSelection
	}{
		{
			name: "planning uses its mapped selection",
			mode: "planning",
			want: ModelSelection{Provider: "anthropic", Model: "claude-opus-4-6"},
		},
		{
			name: "toil uses its mapped selection",
			mode: "toil",
			want: ModelSelection{Provider: "anthropic", Model: "claude-haiku-3"},
		},
		{
			name: "execution uses its mapped selection",
			mode: "execution",
			want: ModelSelection{Provider: "bedrock", Model: "claude-sonnet-4-6"},
		},
		{
			name: "unmapped review falls back to default",
			mode: "review",
			want: ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
		{
			name: "unmapped research falls back to default",
			mode: "research",
			want: ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			sel := r.Select(context.Background(), RouterContext{Mode: tt.mode, Turn: 1})

			must.Equal(tt.want, sel)
		})
	}
}

func TestPerModeRouter_ImplementsInterface(t *testing.T) {
	var _ ModelRouter = (*PerModeRouter)(nil)
}

func TestPerModeRouter_DoesNotAliasModeMap(t *testing.T) {
	must := require.New(t)
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
	must.Equal(ModelSelection{Provider: "anthropic", Model: "claude-opus-4-6"},
		sel,
		"router was affected by mutation of input map",
	)
}

func TestPerModeRouter_SelectionsListsTheDefaultThenEachModeByName(t *testing.T) {
	must := require.New(t)
	r := NewPerModeRouter(defaultSel, map[string]ModelSelection{"review": cheapSel, "planning": expensiveSel})

	must.Equal([]ModelSelection{defaultSel, expensiveSel, cheapSel}, r.Selections())
}
