package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestBuildHarnessRunConfig_BatchFlagEnabledOnly verifies that --batch
// alone produces a BatchProviderConfig with Enabled=true and every
// other field at its zero value; operators reach for --config to set
// the rest.
func TestBuildHarnessRunConfig_BatchFlagEnabledOnly(t *testing.T) {
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "research",
		Prompt:        "test",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      5,
		Timeout:       600,
		TransportType: "grpc",
		TransportAddr: "control-plane:443",
		LogLevel:      "info",
		Batch:         true,
	})
	require.NoError(t, err)

	batch := cfg.Provider.Batch
	require.NotNil(t, batch, "expected non-nil Provider.Batch when --batch is set")
	assert.True(t, batch.Enabled, "Provider.Batch.Enabled")
	assert.Nil(t, batch.MaxWaitSeconds, "Provider.Batch.MaxWaitSeconds is defaulted by ValidateRunConfig")
	assert.False(t, batch.HarnessSidePolling, "Provider.Batch.HarnessSidePolling is set via --config")
	assert.False(t, batch.FallbackOnTimeout, "Provider.Batch.FallbackOnTimeout")
	assert.False(t, batch.CancelBundleOnRunCancel, "Provider.Batch.CancelBundleOnRunCancel")
	assert.False(t, batch.AllowInteractiveModes, "Provider.Batch.AllowInteractiveModes")
}

// TestBuildHarnessRunConfig_BatchFlagUnsetLeavesNil verifies the
// default-off posture: a build without --batch leaves Provider.Batch
// nil so the validator's MaxWaitSeconds default does not fire and the
// factory routes through the streaming adapter.
func TestBuildHarnessRunConfig_BatchFlagUnsetLeavesNil(t *testing.T) {
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "research",
		Prompt:        "test",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})
	require.NoError(t, err)
	require.Nil(t, cfg.Provider.Batch, "Provider.Batch without --batch")
}

// TestApplyOverrides_BatchFlagMergesWithFile pins the merge semantics
// against a --config file that already carries a partial Batch block:
// setting --batch on top must flip Enabled to true without disturbing
// the file's other fields.
func TestApplyOverrides_BatchFlagMergesWithFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Batch = &types.BatchProviderConfig{
		Enabled:            false,
		HarnessSidePolling: true,
	}

	require.NoError(t, cmd.Flags().Set("batch", "true"), "set --batch")
	require.NoError(t, applyOverrides(cmd, cfg, nil))

	batch := cfg.Provider.Batch
	require.NotNil(t, batch, "Provider.Batch should remain non-nil after override")
	assert.True(t, batch.Enabled, "--batch override should set Provider.Batch.Enabled")
	assert.True(t, batch.HarnessSidePolling, "file value should survive for Provider.Batch.HarnessSidePolling")
}

// TestApplyOverrides_BatchFlagAllocatesWhenFileOmits covers the path
// where the --config file omits the Batch block entirely. The flag
// alone must allocate a fresh BatchProviderConfig with Enabled=true.
func TestApplyOverrides_BatchFlagAllocatesWhenFileOmits(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	require.Nil(t, cfg.Provider.Batch, "baseFileConfig should not set Batch")

	require.NoError(t, cmd.Flags().Set("batch", "true"), "set --batch")
	require.NoError(t, applyOverrides(cmd, cfg, nil))

	require.NotNil(t, cfg.Provider.Batch, "expected Provider.Batch to be allocated when --batch flips on an empty slot")
	assert.True(t, cfg.Provider.Batch.Enabled, "Provider.Batch.Enabled")
}

// TestApplyOverrides_BatchFlagUnsetPreservesFile pins the precedence
// rule: an unset --batch flag must not clobber a Batch block the file
// supplied.
func TestApplyOverrides_BatchFlagUnsetPreservesFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Batch = &types.BatchProviderConfig{
		Enabled:            true,
		HarnessSidePolling: true,
	}

	require.NoError(t, applyOverrides(cmd, cfg, nil))

	batch := cfg.Provider.Batch
	require.NotNil(t, batch, "expected Provider.Batch to survive when --batch is unset")
	assert.True(t, batch.Enabled, "file value should survive for Provider.Batch.Enabled")
	assert.True(t, batch.HarnessSidePolling, "file value should survive for Provider.Batch.HarnessSidePolling")
}

// TestHarnessCmd_BatchFlagHelpText pins the operator-facing flag
// description so a future cosmetic edit does not silently drop the
// cost/latency tradeoff statement or the doc link that operators rely
// on to find the cross-field invariants.
func TestHarnessCmd_BatchFlagHelpText(t *testing.T) {
	flag := harnessCmd.Flags().Lookup("batch")
	require.NotNil(t, flag, "expected --batch flag to be registered on harnessCmd")

	usage := flag.Usage
	wantSubstrings := []string{
		"async batch submission",
		"50% cost reduction",
		"waits up to --timeout",
		"3600s",
		"transport=grpc",
		"harnessSidePolling=true",
		"docs/batch.md",
	}
	for _, want := range wantSubstrings {
		t.Run("mentions "+want, func(t *testing.T) {
			require.Contains(t, usage, want)
		})
	}
	require.Equal(t, "false", flag.DefValue, "--batch default")
}

// TestApplyOverrides_BatchFlagExplicitFalseClears pins that
// --batch=false explicitly passed on top of a file with
// Batch.Enabled=true flips Enabled to false while the rest of the
// struct (e.g. HarnessSidePolling) survives.
func TestApplyOverrides_BatchFlagExplicitFalseClears(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Batch = &types.BatchProviderConfig{
		Enabled:            true,
		HarnessSidePolling: true,
	}

	require.NoError(t, cmd.Flags().Set("batch", "false"), "set --batch=false")
	require.NoError(t, applyOverrides(cmd, cfg, nil))

	batch := cfg.Provider.Batch
	require.NotNil(t, batch, "expected Provider.Batch to remain non-nil after --batch=false")
	assert.False(t, batch.Enabled, "--batch=false should clear Provider.Batch.Enabled")
	assert.True(t, batch.HarnessSidePolling, "file value should survive --batch=false for Provider.Batch.HarnessSidePolling")
}
