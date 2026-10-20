package router

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStaticRouter_Select(t *testing.T) {
	must := require.New(t)
	r := NewStaticRouter("anthropic", "claude-sonnet-4-6")

	sel := r.Select(context.Background(), RouterContext{
		Mode: "execution",
		Turn: 1,
	})

	must.Equal(ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"}, sel)
}

func TestStaticRouter_IgnoresContext(t *testing.T) {
	r := NewStaticRouter("bedrock", "claude-opus-4-6")

	cases := []struct {
		name string
		rc   RouterContext
	}{
		{
			name: "execution first turn",
			rc:   RouterContext{Mode: "execution", Turn: 1},
		},
		{
			name: "planning after tool use",
			rc:   RouterContext{Mode: "planning", Turn: 5, LastStopReason: "tool_use"},
		},
		{
			name: "review with high token usage",
			rc:   RouterContext{Mode: "review", Turn: 20, TokenUsage: TokenUsage{Input: 100000, Output: 50000}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			sel := r.Select(context.Background(), tc.rc)
			must.Equal(ModelSelection{Provider: "bedrock", Model: "claude-opus-4-6"}, sel)
		})
	}
}

func TestStaticRouter_ImplementsInterface(t *testing.T) {
	var _ ModelRouter = (*StaticRouter)(nil)
}
