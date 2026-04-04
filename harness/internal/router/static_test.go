package router

import (
	"context"
	"testing"
)

func TestStaticRouter_Select(t *testing.T) {
	r := NewStaticRouter("anthropic", "claude-sonnet-4-6")

	sel := r.Select(context.Background(), RouterContext{
		Mode: "execution",
		Turn: 1,
	})

	if sel.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", sel.Provider)
	}
	if sel.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", sel.Model)
	}
}

func TestStaticRouter_IgnoresContext(t *testing.T) {
	r := NewStaticRouter("bedrock", "claude-opus-4-6")

	contexts := []RouterContext{
		{Mode: "execution", Turn: 1},
		{Mode: "planning", Turn: 5, LastStopReason: "tool_use"},
		{Mode: "review", Turn: 20, TokenUsage: TokenUsage{Input: 100000, Output: 50000}},
	}

	for _, rc := range contexts {
		sel := r.Select(context.Background(), rc)
		if sel.Provider != "bedrock" || sel.Model != "claude-opus-4-6" {
			t.Errorf("Select(%+v) = %+v, want bedrock/claude-opus-4-6", rc, sel)
		}
	}
}

func TestStaticRouter_ImplementsInterface(t *testing.T) {
	var _ ModelRouter = (*StaticRouter)(nil)
}
