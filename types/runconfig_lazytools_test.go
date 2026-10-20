package types

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLazyToolLoadingConfig_Defaults(t *testing.T) {
	must := require.New(t)
	var cfg LazyToolLoadingConfig
	must.False(cfg.Enabled)
	must.Equal(LazyToolStrategyAuto, cfg.EffectiveStrategy())
	must.Equal(DefaultLazyToolSearchResults, cfg.EffectiveMaxResults())

	cfg = LazyToolLoadingConfig{Strategy: LazyToolStrategyHarness, MaxResults: 7}
	must.Equal(LazyToolStrategyHarness, cfg.EffectiveStrategy())
	must.Equal(7, cfg.EffectiveMaxResults())
}

func TestValidLazyToolStrategyValues_ListsEveryAcceptedStrategy(t *testing.T) {
	must := require.New(t)
	values := ValidLazyToolStrategyValues()
	must.Equal([]string{LazyToolStrategyAuto, LazyToolStrategyHarness}, values)
	for _, v := range values {
		var errs []string
		validateLazyToolLoading(LazyToolLoadingConfig{Strategy: v}, &errs)
		must.Empty(errs, "strategy %q", v)
	}
	values[0] = "mutated"
	must.Equal(LazyToolStrategyAuto, ValidLazyToolStrategyValues()[0], "each call returns a fresh slice")
}

func TestValidateLazyToolLoading_AcceptsWellFormedConfig(t *testing.T) {
	must := require.New(t)
	var errs []string
	validateLazyToolLoading(LazyToolLoadingConfig{
		Enabled:    true,
		Strategy:   LazyToolStrategyHarness,
		Eager:      []string{"read_file", "mcp_github_search", "lookup-order"},
		MaxResults: MaxLazyToolSearchResults,
	}, &errs)
	must.Empty(errs)
}

func TestValidateLazyToolLoading_AcceptsMCPShapedEagerNames(t *testing.T) {
	for _, name := range []string{
		"mcp_atlassian_getConfluencePageFooterCommentsByLabel",
		"mcp_gh.enterprise_create_issue",
		"mcp_" + strings.Repeat("x", maxLazyToolEagerNameBytes-len("mcp_")),
	} {
		t.Run(name[:min(len(name), 40)], func(t *testing.T) {
			must := require.New(t)
			var errs []string
			validateLazyToolLoading(LazyToolLoadingConfig{Eager: []string{name}}, &errs)
			must.Empty(errs)
		})
	}
}

func TestValidateLazyToolLoading_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		cfg  LazyToolLoadingConfig
		want string
	}{
		{"unknown strategy", LazyToolLoadingConfig{Strategy: "native"}, "tools.lazyLoading.strategy \"native\""},
		{"negative max results", LazyToolLoadingConfig{MaxResults: -1}, "tools.lazyLoading.maxResults -1"},
		{"max results above ceiling", LazyToolLoadingConfig{MaxResults: MaxLazyToolSearchResults + 1}, "tools.lazyLoading.maxResults 21"},
		{"empty eager entry", LazyToolLoadingConfig{Eager: []string{""}}, "tools.lazyLoading.eager[0] must not be empty"},
		{"eager entry over 256 bytes", LazyToolLoadingConfig{Eager: []string{strings.Repeat("x", maxLazyToolEagerNameBytes+1)}}, "is 257 bytes; at most 256"},
		{"eager entry with a space", LazyToolLoadingConfig{Eager: []string{"read_file", "bad name"}}, "tools.lazyLoading.eager[1] \"bad name\" must not contain whitespace"},
		{"eager entry with a tab", LazyToolLoadingConfig{Eager: []string{"bad\tname"}}, "must not contain whitespace"},
		{"eager entry with a newline", LazyToolLoadingConfig{Eager: []string{"bad\nname"}}, "must not contain whitespace"},
		{"eager entry with a NUL", LazyToolLoadingConfig{Eager: []string{"bad\x00name"}}, "must not contain control characters"},
		{"eager entry with a star", LazyToolLoadingConfig{Eager: []string{"mcp_github_*"}}, "must not contain *, ? or [, which are reserved"},
		{"eager entry with a question mark", LazyToolLoadingConfig{Eager: []string{"mcp_github_?"}}, "which are reserved"},
		{"eager entry with a bracket", LazyToolLoadingConfig{Eager: []string{"mcp_github_[a]"}}, "which are reserved"},
		{"eager names the search tool", LazyToolLoadingConfig{Eager: []string{ToolSearchToolName}}, "is the harness tool search"},
		{"duplicate eager entry", LazyToolLoadingConfig{Eager: []string{"read_file", "read_file"}}, "tools.lazyLoading.eager[1] \"read_file\" duplicates tools.lazyLoading.eager[0]"},
		{"too many eager entries", LazyToolLoadingConfig{Eager: make([]string, maxLazyToolEagerEntries+1)}, "at most 256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var errs []string
			validateLazyToolLoading(tc.cfg, &errs)
			must.NotEmpty(errs)
			must.Contains(strings.Join(errs, "\n"), tc.want)
		})
	}
}

func TestValidateRunConfig_ChecksLazyLoadingWhenDisabled(t *testing.T) {
	must := require.New(t)
	cfg := &RunConfig{
		RunID:  "run-1",
		Prompt: "hello",
		Mode:   "planning",
		Tools: ToolsConfig{
			LazyLoading: LazyToolLoadingConfig{Strategy: "bogus"},
		},
	}
	err := ValidateRunConfig(cfg)
	must.Error(err)
	must.Contains(err.Error(), "tools.lazyLoading.strategy \"bogus\"")
}

func TestValidateControlPlaneTools_ReservesToolSearchNames(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{ToolSearchToolName, "is reserved for the harness tool search"},
		{"tool_search_tool_bm25", "is reserved for native tool search"},
		{"tool_search_tool_regex", "is reserved for native tool search"},
		{"tool_search_tool_other", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var errs []string
			validateControlPlaneTools(&RunConfig{
				Transport: TransportConfig{Type: "grpc"},
				Tools: ToolsConfig{ControlPlane: []ControlPlaneToolConfig{{
					Name:        tc.name,
					Description: "shadow",
					InputSchema: []byte(`{"type":"object"}`),
				}}},
			}, &errs)
			if tc.want == "" {
				must.Empty(errs)
				return
			}
			must.Contains(strings.Join(errs, "\n"), tc.want)
		})
	}
}
