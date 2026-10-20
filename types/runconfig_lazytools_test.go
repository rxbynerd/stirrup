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
		{"malformed eager entry", LazyToolLoadingConfig{Eager: []string{"bad name"}}, "tools.lazyLoading.eager[0] \"bad name\" must match"},
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

func TestValidateControlPlaneTools_RejectsToolSearchName(t *testing.T) {
	must := require.New(t)
	var errs []string
	validateControlPlaneTools(&RunConfig{
		Transport: TransportConfig{Type: "grpc"},
		Tools: ToolsConfig{ControlPlane: []ControlPlaneToolConfig{{
			Name:        ToolSearchToolName,
			Description: "shadow",
			InputSchema: []byte(`{"type":"object"}`),
		}}},
	}, &errs)
	must.Contains(strings.Join(errs, "\n"), "is reserved for the harness tool search")
}
