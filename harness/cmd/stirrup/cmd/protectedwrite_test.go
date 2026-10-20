package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyOverrides_ProtectedWriteTargetDefaultLeavesItEmpty(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Nil(cfg.Tools.ProtectedWriteTargets, "protected write targets must stay nil without the flag")
}

func TestApplyOverrides_ProtectedWriteTargetReplacesFileList(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Tools.ProtectedWriteTargets = []string{"CLAUDE.md"}
	for _, v := range []string{"AGENTS.md", "config/policies/"} {
		must.NoError(cmd.Flags().Set("protected-write-target", v))
	}
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal([]string{"AGENTS.md", "config/policies/"}, cfg.Tools.ProtectedWriteTargets)
}

func TestBuildRunConfig_ProtectedWriteTargetValidates(t *testing.T) {
	t.Run("rejects path outside the workspace", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		for _, kv := range [][2]string{
			{"prompt", "do the thing"},
			{"protected-write-target", "../outside.md"},
		} {
			must.NoError(cmd.Flags().Set(kv[0], kv[1]))
		}
		_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		must.ErrorContains(err, "tools.protectedWriteTargets[0]")
	})

	t.Run("accepts a relative file path", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		for _, kv := range [][2]string{
			{"prompt", "do the thing"},
			{"protected-write-target", "AGENTS.md"},
		} {
			must.NoError(cmd.Flags().Set(kv[0], kv[1]))
		}
		cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		must.NoError(err)
		must.Equal([]string{"AGENTS.md"}, cfg.Tools.ProtectedWriteTargets)
	})
}
