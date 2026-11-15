package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestApplyOverrides_ExecutorSandboxFlags(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--image", "registry.example/sandbox:1",
		"--cpus", "0.5",
		"--memory-mb", "2048",
		"--disk-mb", "10240",
		"--network-mode", "none",
	}))
	cfg := baseFileConfig()

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("registry.example/sandbox:1", cfg.Executor.Image)
	must.NotNil(cfg.Executor.Resources)
	check.InDelta(0.5, cfg.Executor.Resources.CPUs, 0)
	check.Equal(2048, cfg.Executor.Resources.MemoryMB)
	check.Equal(10240, cfg.Executor.Resources.DiskMB)
	must.NotNil(cfg.Executor.Network)
	check.Equal("none", cfg.Executor.Network.Mode)
}

func TestApplyOverrides_ExecutorSandboxFlagsUnsetKeepFile(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags(nil))
	cfg := baseFileConfig()
	cfg.Executor.Image = "from-file:1"
	cfg.Executor.Resources = &types.ResourceLimits{CPUs: 2, MemoryMB: 4096, DiskMB: 20480, PIDs: 512}
	cfg.Executor.Network = &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com:443"}}

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("from-file:1", cfg.Executor.Image)
	check.Equal(&types.ResourceLimits{CPUs: 2, MemoryMB: 4096, DiskMB: 20480, PIDs: 512}, cfg.Executor.Resources)
	check.Equal(&types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com:443"}}, cfg.Executor.Network)
}

func TestApplyOverrides_ExecutorSandboxFlagsMergeIntoFile(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--memory-mb", "8192", "--network-mode", "allowlist"}))
	cfg := baseFileConfig()
	cfg.Executor.Resources = &types.ResourceLimits{CPUs: 2, MemoryMB: 4096, DiskMB: 20480, PIDs: 512}
	cfg.Executor.Network = &types.NetworkConfig{Mode: "none", Allowlist: []string{"example.com:443"}}

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal(&types.ResourceLimits{CPUs: 2, MemoryMB: 8192, DiskMB: 20480, PIDs: 512}, cfg.Executor.Resources,
		"only the flagged limit changes; pids has no flag and survives")
	check.Equal(&types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com:443"}}, cfg.Executor.Network,
		"only the mode changes; the allowlist entries survive")
}

func TestApplyOverrides_ExecutorSandboxFlagsExplicitZeroClearsLimit(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--cpus", "0"}))
	cfg := baseFileConfig()
	cfg.Executor.Resources = &types.ResourceLimits{CPUs: 2, MemoryMB: 4096}

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal(&types.ResourceLimits{MemoryMB: 4096}, cfg.Executor.Resources)
}

func TestApplyOverrides_NetworkModeRejectsUnknownValue(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--network-mode", "bridge"}))

	err := applyOverrides(cmd, baseFileConfig(), nil)

	must.ErrorContains(err, `--network-mode: invalid value "bridge" (expected one of: allowlist, none)`)
}

func TestFlagCompletion_ContainerRuntimeFollowsExecutor(t *testing.T) {
	for _, tc := range []struct {
		executor string
		want     []string
	}{
		{executor: "", want: types.ValidExecutorRuntimeValues()},
		{executor: "local", want: types.ValidExecutorRuntimeValues()},
		{executor: "container", want: types.ValidExecutorRuntimeValues()},
		{executor: "k8s", want: types.ValidK8sExecutorRuntimeValues()},
		{executor: "k8s-sandbox", want: types.ValidK8sExecutorRuntimeValues()},
	} {
		t.Run("executor_"+tc.executor, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestHarnessCommand()
			if tc.executor != "" {
				must.NoError(cmd.ParseFlags([]string{"--executor", tc.executor}))
			}
			got, directive := runFlagCompletion(t, cmd, "container-runtime")
			must.Equal(cobra.ShellCompDirectiveNoFileComp, directive)
			must.ElementsMatch(tc.want, got)
		})
	}
}
