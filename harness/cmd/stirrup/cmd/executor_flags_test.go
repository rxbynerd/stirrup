package cmd

import (
	"path/filepath"
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

func TestApplyOverrides_ExecutorSandboxFlags_UnsetKeepFile(t *testing.T) {
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

func TestApplyOverrides_ExecutorSandboxFlags_MergeIntoFile(t *testing.T) {
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

func TestApplyOverrides_ExecutorSandboxFlags_ExplicitZeroClearsLimit(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--cpus", "0"}))
	cfg := baseFileConfig()
	cfg.Executor.Resources = &types.ResourceLimits{CPUs: 2, MemoryMB: 4096}

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal(&types.ResourceLimits{MemoryMB: 4096}, cfg.Executor.Resources)
}

func TestApplyOverrides_ExecutorSandboxFlags_ExplicitZeroClearsMemoryAndDisk(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--memory-mb", "0", "--disk-mb", "0"}))
	cfg := baseFileConfig()
	cfg.Executor.Resources = &types.ResourceLimits{CPUs: 2, MemoryMB: 4096, DiskMB: 20480}

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal(&types.ResourceLimits{CPUs: 2}, cfg.Executor.Resources)
}

// resolveExecutorFlags resolves a minimal read-only config file together
// with args through the shared builder, the path both `harness` and
// `run-config` take, so the executor validation runs on the flag result.
func resolveExecutorFlags(t *testing.T, args ...string) (*types.RunConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, `{"runId":"r","mode":"planning","prompt":"do a thing",`+
		`"maxTurns":10,"timeout":600,`+
		`"permissionPolicy":{"type":"deny-side-effects"},`+
		`"provider":{"type":"anthropic","apiKeyRef":"secret://ANTHROPIC_API_KEY"},`+
		`"modelRouter":{"type":"static","provider":"anthropic","model":"claude-x"}}`)
	cmd := newTestHarnessCommand()
	must := require.New(t)
	must.NoError(cmd.ParseFlags(args))
	return BuildRunConfig(RunConfigSources{ConfigPath: path, Cmd: cmd, Resolve: ResolveAll})
}

func TestBuildRunConfig_SandboxFlags_RejectedOnUnsandboxedExecutor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		field string
	}{
		{name: "network mode on the default executor", args: []string{"--network-mode", "none"}, field: "network"},
		{name: "image on local", args: []string{"--executor", "local", "--image", "img"}, field: "image"},
		{name: "cpus on local", args: []string{"--executor", "local", "--cpus", "1"}, field: "resources"},
		{name: "memory on local", args: []string{"--executor", "local", "--memory-mb", "512"}, field: "resources"},
		{name: "disk on local", args: []string{"--executor", "local", "--disk-mb", "512"}, field: "resources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)

			_, err := resolveExecutorFlags(t, tc.args...)

			must.ErrorContains(err, "executor."+tc.field+" is not valid for executor.type=\"local\"")
			must.Equal(exitValidation, classifyExitCode(err), "err=%v", err)
		})
	}
}

func TestBuildRunConfig_SandboxFlags_AcceptedOnContainerExecutor(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	cfg, err := resolveExecutorFlags(t, "--executor", "container", "--image", "img",
		"--network-mode", "none", "--cpus", "1", "--memory-mb", "512")

	must.NoError(err)
	check.Equal("none", cfg.Executor.Network.Mode)
	check.Equal(512, cfg.Executor.Resources.MemoryMB)
}

func TestApplyOverrides_NetworkMode_RejectsUnknownValue(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--network-mode", "bridge"}))

	err := applyOverrides(cmd, baseFileConfig(), nil)

	must.ErrorContains(err, `--network-mode: invalid value "bridge" (expected one of: allowlist, none)`)
}

func TestFlagCompletion_ContainerRuntime_FollowsExecutor(t *testing.T) {
	for _, tc := range []struct {
		executor string
		want     []string
	}{
		{executor: "", want: types.ValidExecutorRuntimeValues()},
		{executor: "local", want: types.ValidExecutorRuntimeValues()},
		{executor: "container", want: types.ValidExecutorRuntimeValues()},
		{executor: "k8s", want: types.ValidK8sExecutorRuntimeValues()},
		{executor: "k8s-sandbox", want: types.ValidK8sSandboxExecutorRuntimeValues()},
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
