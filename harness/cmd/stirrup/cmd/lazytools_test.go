package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestApplyOverrides_LazyToolsFlagsLeaveFileConfigWhenUnset(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Tools.LazyLoading = types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness, Eager: []string{"read_file"}, MaxResults: 3}
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal(types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness, Eager: []string{"read_file"}, MaxResults: 3}, cfg.Tools.LazyLoading)
}

func TestApplyOverrides_LazyToolsFlagsReplaceFileConfig(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--lazy-tools=false",
		"--lazy-tools-strategy", "auto",
		"--lazy-tools-eager", "grep_files",
		"--lazy-tools-eager", "mcp_github_search",
		"--lazy-tools-max-results", "9",
	}))
	cfg := baseFileConfig()
	cfg.Tools.LazyLoading = types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness, Eager: []string{"read_file"}, MaxResults: 3}
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal(types.LazyToolLoadingConfig{
		Enabled:    false,
		Strategy:   types.LazyToolStrategyAuto,
		Eager:      []string{"grep_files", "mcp_github_search"},
		MaxResults: 9,
	}, cfg.Tools.LazyLoading)
}

func TestBuildRunConfig_LazyToolsFlagsOnTheFlagOnlyPath(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "x",
		"--lazy-tools",
		"--lazy-tools-strategy", "harness",
		"--lazy-tools-eager", "read_file",
		"--lazy-tools-max-results", "7",
	}))
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	must.NoError(err)
	must.Equal(types.LazyToolLoadingConfig{
		Enabled:    true,
		Strategy:   types.LazyToolStrategyHarness,
		Eager:      []string{"read_file"},
		MaxResults: 7,
	}, cfg.Tools.LazyLoading)

	bare := newTestHarnessCommand()
	must.NoError(bare.ParseFlags([]string{"--mode", "execution", "--prompt", "x"}))
	bareCfg, err := BuildRunConfig(RunConfigSources{Cmd: bare, Resolve: ResolveAll})
	must.NoError(err)
	must.Zero(bareCfg.Tools.LazyLoading, "lazy loading stays off without the flags")
}

func TestBuildRunConfig_LazyToolsFlagsOverrideABaseConfig(t *testing.T) {
	must := require.New(t)
	var base types.RunConfig
	must.NoError(json.Unmarshal([]byte(minimalRunConfigJSON(t)), &base))
	base.Tools.LazyLoading = types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file"}}
	baseJSON, err := json.Marshal(base)
	must.NoError(err)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--lazy-tools-max-results", "12"}))
	cfg, err := BuildRunConfig(RunConfigSources{Stdin: strings.NewReader(string(baseJSON)), Cmd: cmd, Resolve: ResolveBase})
	must.NoError(err)
	must.Equal(types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file"}, MaxResults: 12}, cfg.Tools.LazyLoading)
}

func TestBuildRunConfig_LazyToolsStrategyIsValidated(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--prompt", "x", "--lazy-tools", "--lazy-tools-strategy", "native"}))
	_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	must.ErrorContains(err, `tools.lazyLoading.strategy "native"`)
}
