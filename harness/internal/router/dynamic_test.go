package router

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Shared fixtures for all dynamic router tests.
var (
	cheapSel     = ModelSelection{Provider: "anthropic", Model: "claude-haiku-5-5"}
	defaultSel   = ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	expensiveSel = ModelSelection{Provider: "anthropic", Model: "claude-opus-4-6"}

	standardConfig = DynamicRouterConfig{
		DefaultSelection:        defaultSel,
		CheapSelection:          cheapSel,
		ExpensiveSelection:      expensiveSel,
		ExpensiveTurnThreshold:  10,
		ExpensiveTokenThreshold: 50000,
		CheapStopReasons:        []string{"tool_use"},
	}
)

func TestDynamicRouter_CheapStopReason(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           2,
		LastStopReason: "tool_use",
	})

	require.Equal(t, cheapSel, sel)
}

func TestDynamicRouter_HighTurnCount(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           10,
		LastStopReason: "end_turn",
	})

	require.Equal(t, expensiveSel, sel)
}

func TestDynamicRouter_HighTokenUsage(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           3,
		LastStopReason: "end_turn",
		TokenUsage:     TokenUsage{Input: 10000, Output: 55000},
	})

	require.Equal(t, expensiveSel, sel)
}

func TestDynamicRouter_BelowThresholds_SelectsDefault(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           3,
		LastStopReason: "end_turn",
		TokenUsage:     TokenUsage{Input: 5000, Output: 2000},
	})

	require.Equal(t, defaultSel, sel)
}

func TestDynamicRouter_CheapStopReason_WinsOverHighTurn(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	// Turn 15 would normally trigger expensive, but tool_use stop reason
	// means the model is just processing tool results — genuinely cheap.
	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           15,
		LastStopReason: "tool_use",
		TokenUsage:     TokenUsage{Input: 80000, Output: 60000},
	})

	require.Equal(t, cheapSel, sel)
}

func TestDynamicRouter_ZeroThresholds_AreDisabled(t *testing.T) {
	cfg := DynamicRouterConfig{
		DefaultSelection:        defaultSel,
		CheapSelection:          cheapSel,
		ExpensiveSelection:      expensiveSel,
		ExpensiveTurnThreshold:  0,
		ExpensiveTokenThreshold: 0,
		CheapStopReasons:        []string{"tool_use"},
	}
	r := NewDynamicRouter(cfg)

	// Zero thresholds are disabled, not "always trigger".
	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           0,
		LastStopReason: "end_turn",
	})

	require.Equal(t, defaultSel, sel)
}

func TestDynamicRouter_NoCheapStopReasons_NeverSelectsCheap(t *testing.T) {
	cfg := DynamicRouterConfig{
		DefaultSelection:        defaultSel,
		CheapSelection:          cheapSel,
		ExpensiveSelection:      expensiveSel,
		ExpensiveTurnThreshold:  10,
		ExpensiveTokenThreshold: 50000,
		CheapStopReasons:        nil, // no cheap stop reasons
	}
	r := NewDynamicRouter(cfg)

	// Even with tool_use, no cheap stop reasons configured means no cheap routing.
	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           2,
		LastStopReason: "tool_use",
	})

	require.Equal(t, defaultSel, sel)
}

func TestDynamicRouter_CustomSelections(t *testing.T) {
	custom := DynamicRouterConfig{
		DefaultSelection:   ModelSelection{Provider: "bedrock", Model: "custom-default"},
		CheapSelection:     ModelSelection{Provider: "openai", Model: "gpt-4o-mini"},
		ExpensiveSelection: ModelSelection{Provider: "bedrock", Model: "custom-expensive"},

		ExpensiveTurnThreshold:  5,
		ExpensiveTokenThreshold: 10000,
		CheapStopReasons:        []string{"tool_use", "content_filter"},
	}
	r := NewDynamicRouter(custom)

	tests := []struct {
		name string
		rc   RouterContext
		want ModelSelection
	}{
		{
			name: "custom cheap on tool_use",
			rc:   RouterContext{Turn: 1, LastStopReason: "tool_use"},
			want: custom.CheapSelection,
		},
		{
			name: "custom cheap on content_filter",
			rc:   RouterContext{Turn: 1, LastStopReason: "content_filter"},
			want: custom.CheapSelection,
		},
		{
			name: "custom expensive on high turn",
			rc:   RouterContext{Turn: 5, LastStopReason: "end_turn"},
			want: custom.ExpensiveSelection,
		},
		{
			name: "custom expensive on high tokens",
			rc:   RouterContext{Turn: 2, LastStopReason: "end_turn", TokenUsage: TokenUsage{Output: 10000}},
			want: custom.ExpensiveSelection,
		},
		{
			name: "custom default",
			rc:   RouterContext{Turn: 2, LastStopReason: "end_turn", TokenUsage: TokenUsage{Output: 500}},
			want: custom.DefaultSelection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel := r.Select(context.Background(), tt.rc)
			require.Equal(t, tt.want, sel)
		})
	}
}

func TestDynamicRouter_FirstTurn_SelectsDefault(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode: "execution",
		Turn: 0,
	})

	require.Equal(t, defaultSel, sel)
}

func TestDynamicRouter_ImplementsInterface(t *testing.T) {
	var _ ModelRouter = (*DynamicRouter)(nil)
}
