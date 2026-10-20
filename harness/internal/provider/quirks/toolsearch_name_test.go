package quirks

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolSearchCapability_NativeToolNameFollowsVariant(t *testing.T) {
	cases := []struct {
		variant string
		want    string
	}{
		{"hosted", "tool_search"},
		{"regex", "tool_search_tool_regex"},
		{"bm25", "tool_search_tool_bm25"},
		{"", "tool_search_tool_bm25"},
		{"semantic", "tool_search_tool_bm25"},
	}
	for _, tc := range cases {
		t.Run(tc.variant, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, ToolSearchCapability{Supported: true, Variant: tc.variant}.NativeToolName())
		})
	}
}
