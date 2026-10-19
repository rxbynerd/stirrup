package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyOverrides_ProtectedWriteTargetDefaultLeavesItEmpty(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	require.NoError(t, applyOverrides(cmd, cfg, nil))
	require.Nil(t, cfg.Tools.ProtectedWriteTargets, "protected write targets must stay nil without the flag")
}

func TestApplyOverrides_ProtectedWriteTargetReplacesFileList(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Tools.ProtectedWriteTargets = []string{"CLAUDE.md"}
	for _, v := range []string{"AGENTS.md", "config/policies/"} {
		require.NoError(t, cmd.Flags().Set("protected-write-target", v))
	}
	require.NoError(t, applyOverrides(cmd, cfg, nil))
	require.Equal(t, []string{"AGENTS.md", "config/policies/"}, cfg.Tools.ProtectedWriteTargets)
}

func TestBuildRunConfig_ProtectedWriteTargetValidates(t *testing.T) {
	t.Run("rejects path outside the workspace", func(t *testing.T) {
		cmd := newTestHarnessCommand()
		for _, kv := range [][2]string{
			{"prompt", "do the thing"},
			{"protected-write-target", "../outside.md"},
		} {
			require.NoError(t, cmd.Flags().Set(kv[0], kv[1]))
		}
		_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		require.ErrorContains(t, err, "tools.protectedWriteTargets[0]")
	})

	t.Run("accepts a relative file path", func(t *testing.T) {
		cmd := newTestHarnessCommand()
		for _, kv := range [][2]string{
			{"prompt", "do the thing"},
			{"protected-write-target", "AGENTS.md"},
		} {
			require.NoError(t, cmd.Flags().Set(kv[0], kv[1]))
		}
		cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		require.NoError(t, err)
		require.Equal(t, []string{"AGENTS.md"}, cfg.Tools.ProtectedWriteTargets)
	})
}
