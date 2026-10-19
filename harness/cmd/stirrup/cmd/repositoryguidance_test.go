package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

const cliGuidanceDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestApplyOverrides_RepositoryGuidanceDefaultLeavesItOff(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	require.NoError(t, applyOverrides(cmd, cfg, nil))
	require.Nil(t, cfg.RepositoryGuidance, "must stay nil without the repository-guidance flags")
}

func TestApplyOverrides_RepositoryGuidanceFlags(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	for _, kv := range [][2]string{
		{"repository-guidance-file", "AGENTS.md"},
		{"repository-guidance-file", "CLAUDE.md"},
		{"repository-guidance-max-bytes", "4096"},
		{"repository-guidance-pin", "AGENTS.md=" + cliGuidanceDigest},
	} {
		require.NoError(t, cmd.Flags().Set(kv[0], kv[1]), "set %s", kv[0])
	}
	require.NoError(t, applyOverrides(cmd, cfg, nil))

	want := &types.RepositoryGuidanceConfig{
		Files:        []string{"AGENTS.md", "CLAUDE.md"},
		MaxBytes:     4096,
		PinnedSHA256: map[string]string{"AGENTS.md": cliGuidanceDigest},
	}
	require.Equal(t, want, cfg.RepositoryGuidance)
}

func TestApplyOverrides_RepositoryGuidanceFileReplacesAndPinMerges(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.RepositoryGuidance = &types.RepositoryGuidanceConfig{
		Files:        []string{"CLAUDE.md"},
		MaxBytes:     1000,
		PinnedSHA256: map[string]string{"CLAUDE.md": strings.Repeat("a", 64)},
	}
	require.NoError(t, cmd.Flags().Set("repository-guidance-file", "AGENTS.md"))
	require.NoError(t, cmd.Flags().Set("repository-guidance-pin", "AGENTS.md="+cliGuidanceDigest))
	require.NoError(t, applyOverrides(cmd, cfg, nil))

	rg := cfg.RepositoryGuidance
	require.NotNil(t, rg)
	require.Equal(t, []string{"AGENTS.md"}, rg.Files, "the flag list replaces the file list")
	require.Equal(t, 1000, rg.MaxBytes, "the file value survives")
	require.Len(t, rg.PinnedSHA256, 2, "the flag pin merges over the file pins")
	require.Equal(t, cliGuidanceDigest, rg.PinnedSHA256["AGENTS.md"])
}

func TestApplyOverrides_RepositoryGuidancePinRejectsMalformedEntry(t *testing.T) {
	cmd := newTestHarnessCommand()
	require.NoError(t, cmd.Flags().Set("repository-guidance-pin", "AGENTS.md"))
	err := applyOverrides(cmd, baseFileConfig(), nil)
	require.ErrorContains(t, err, "--repository-guidance-pin")
}

func TestBuildRunConfig_RepositoryGuidanceFlagsValidate(t *testing.T) {
	t.Run("rejects a pin for a file missing from the list", func(t *testing.T) {
		cmd := newTestHarnessCommand()
		for _, kv := range [][2]string{
			{"prompt", "do the thing"},
			{"repository-guidance-file", "AGENTS.md"},
			{"repository-guidance-pin", "CLAUDE.md=" + cliGuidanceDigest},
		} {
			require.NoError(t, cmd.Flags().Set(kv[0], kv[1]), "set %s", kv[0])
		}

		_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		require.ErrorContains(t, err, "must also appear in repositoryGuidance.files")
	})

	t.Run("accepts guidance files without pins", func(t *testing.T) {
		cmd := newTestHarnessCommand()
		for _, kv := range [][2]string{
			{"prompt", "do the thing"},
			{"repository-guidance-file", "AGENTS.md"},
		} {
			require.NoError(t, cmd.Flags().Set(kv[0], kv[1]), "set %s", kv[0])
		}

		cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		require.NoError(t, err)
		require.NotNil(t, cfg.RepositoryGuidance)
		require.Equal(t, []string{"AGENTS.md"}, cfg.RepositoryGuidance.Files)
	})
}
