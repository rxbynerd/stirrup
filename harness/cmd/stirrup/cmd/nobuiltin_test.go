package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestApplyModeDefaults_NoBuiltInSkipsReadOnlyDefault(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{
		Mode:     "review",
		Executor: types.ExecutorConfig{Type: "none"},
		Tools:    types.ToolsConfig{NoBuiltIn: true},
	}
	applyModeDefaults(cfg)
	must.Empty(cfg.Tools.BuiltIn)
	must.True(cfg.Tools.NoBuiltIn)
}

func TestBuildRunConfig_NoBuiltInToolsFlagYieldsValidToollessRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "review",
		"--executor", "none",
		"--prompt", "classify",
		"--no-builtin-tools",
	}))
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	must.NoError(err)
	check.True(cfg.Tools.NoBuiltIn)
	check.Empty(cfg.Tools.BuiltIn)
	must.NoError(types.ValidateRunConfig(cfg))
}

func TestApplyOverrides_NoBuiltInToolsFlagOverridesFileConfig(t *testing.T) {
	must := require.New(t)

	unset := newTestHarnessCommand()
	fileOn := baseFileConfig()
	fileOn.Tools.NoBuiltIn = true
	must.NoError(applyOverrides(unset, fileOn, nil))
	must.True(fileOn.Tools.NoBuiltIn, "an unset flag leaves the file value")

	off := newTestHarnessCommand()
	must.NoError(off.ParseFlags([]string{"--no-builtin-tools=false"}))
	must.NoError(applyOverrides(off, fileOn, nil))
	must.False(fileOn.Tools.NoBuiltIn, "an explicit false clears the file value")
}
