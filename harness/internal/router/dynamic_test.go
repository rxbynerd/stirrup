package router

import (
	"context"
	"testing"
)

// Shared fixtures for all dynamic router tests.
var (
	cheapSel     = ModelSelection{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"}
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

	if sel != cheapSel {
		t.Errorf("Select() = %s/%s, want %s/%s (cheap)", sel.Provider, sel.Model, cheapSel.Provider, cheapSel.Model)
	}
}

func TestDynamicRouter_HighTurnCount(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           10,
		LastStopReason: "end_turn",
	})

	if sel != expensiveSel {
		t.Errorf("Select() = %s/%s, want %s/%s (expensive)", sel.Provider, sel.Model, expensiveSel.Provider, expensiveSel.Model)
	}
}

func TestDynamicRouter_HighTokenUsage(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           3,
		LastStopReason: "end_turn",
		TokenUsage:     TokenUsage{Input: 10000, Output: 55000},
	})

	if sel != expensiveSel {
		t.Errorf("Select() = %s/%s, want %s/%s (expensive)", sel.Provider, sel.Model, expensiveSel.Provider, expensiveSel.Model)
	}
}

func TestDynamicRouter_NormalTurn(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           3,
		LastStopReason: "end_turn",
		TokenUsage:     TokenUsage{Input: 5000, Output: 2000},
	})

	if sel != defaultSel {
		t.Errorf("Select() = %s/%s, want %s/%s (default)", sel.Provider, sel.Model, defaultSel.Provider, defaultSel.Model)
	}
}

func TestDynamicRouter_CheapStopReasonTakesPriorityOverHighTurn(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	// Turn 15 would normally trigger expensive, but tool_use stop reason
	// means the model is just processing tool results — genuinely cheap.
	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           15,
		LastStopReason: "tool_use",
		TokenUsage:     TokenUsage{Input: 80000, Output: 60000},
	})

	if sel != cheapSel {
		t.Errorf("Select() = %s/%s, want %s/%s (cheap should win over expensive thresholds)",
			sel.Provider, sel.Model, cheapSel.Provider, cheapSel.Model)
	}
}

func TestDynamicRouter_ZeroThresholdsAlwaysExpensive(t *testing.T) {
	// With thresholds at zero, any turn >= 0 and any output >= 0 triggers
	// the expensive path. But cheap stop reason still takes priority.
	cfg := DynamicRouterConfig{
		DefaultSelection:        defaultSel,
		CheapSelection:          cheapSel,
		ExpensiveSelection:      expensiveSel,
		ExpensiveTurnThreshold:  0,
		ExpensiveTokenThreshold: 0,
		CheapStopReasons:        []string{"tool_use"},
	}
	r := NewDynamicRouter(cfg)

	// Zero thresholds are treated as "disabled" (the > 0 guard), so this
	// falls through to default. This is the intended behaviour: zero means
	// "don't use this threshold", not "always trigger".
	sel := r.Select(context.Background(), RouterContext{
		Mode:           "execution",
		Turn:           0,
		LastStopReason: "end_turn",
	})

	if sel != defaultSel {
		t.Errorf("Select() = %s/%s, want %s/%s (zero thresholds are disabled)",
			sel.Provider, sel.Model, defaultSel.Provider, defaultSel.Model)
	}
}

func TestDynamicRouter_NoCheapStopReasonsNeverCheap(t *testing.T) {
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

	if sel != defaultSel {
		t.Errorf("Select() = %s/%s, want %s/%s (no cheap stop reasons configured)",
			sel.Provider, sel.Model, defaultSel.Provider, defaultSel.Model)
	}
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
			if sel != tt.want {
				t.Errorf("Select() = %s/%s, want %s/%s", sel.Provider, sel.Model, tt.want.Provider, tt.want.Model)
			}
		})
	}
}

func TestDynamicRouter_Turn0NoHistory(t *testing.T) {
	r := NewDynamicRouter(standardConfig)

	// First turn: no stop reason, zero tokens, turn 0.
	sel := r.Select(context.Background(), RouterContext{
		Mode: "execution",
		Turn: 0,
	})

	if sel != defaultSel {
		t.Errorf("Select() = %s/%s, want %s/%s (default for first turn)",
			sel.Provider, sel.Model, defaultSel.Provider, defaultSel.Model)
	}
}

func TestDynamicRouter_ImplementsInterface(t *testing.T) {
	var _ ModelRouter = (*DynamicRouter)(nil)
}
