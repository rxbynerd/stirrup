package quirks

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolSearchCapability_ResolvesPerModel(t *testing.T) {
	cases := []struct {
		provider, model string
		supported       bool
		variant         string
	}{
		{"anthropic", "claude-opus-4-5-20251101", true, "bm25"},
		{"anthropic", "claude-opus-4-6", true, "bm25"},
		{"anthropic", "claude-opus-4-8", true, "bm25"},
		{"anthropic", "claude-sonnet-4-5-20250929", true, "bm25"},
		{"anthropic", "claude-sonnet-4-6", true, "bm25"},
		{"anthropic", "claude-haiku-4-5-20251001", true, "bm25"},
		{"anthropic", "claude-sonnet-5-5", true, "bm25"},
		{"anthropic", "claude-opus-5", true, "bm25"},
		{"anthropic", "claude-fable-5-1", true, "bm25"},
		{"anthropic", "claude-haiku-5-5", true, "bm25"},
		{"anthropic", "claude-mythos-5-1", true, "bm25"},
		{"anthropic", "claude-opus-4-1-20250805", false, ""},
		{"anthropic", "claude-sonnet-4-20250514", false, ""},
		{"anthropic", "claude-3-5-sonnet-20241022", false, ""},
		{"anthropic", "claude-3-5-haiku-20241022", false, ""},
		{"openai-responses", "gpt-5.4", true, "hosted"},
		{"openai-responses", "gpt-5.5-mini", true, "hosted"},
		{"openai-responses", "gpt-6-astra", true, "hosted"},
		{"openai-responses", "gpt-5", false, ""},
		{"openai-responses", "gpt-5.2", false, ""},
		{"openai-responses", "o3", false, ""},
		{"openai-compatible", "gpt-6", false, ""},
		{"gemini", "gemini-3.8-pro", false, ""},
		{"bedrock", "anthropic.claude-sonnet-5-5", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			must := require.New(t)
			got := DefaultRegistry().Resolve(tc.provider, tc.model).ToolSearch
			must.Equal(tc.supported, got.Supported)
			must.Equal(tc.variant, got.Variant)
		})
	}
}
