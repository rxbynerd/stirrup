package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
)

const autonomousPresetRelPath = "examples/runconfig/autonomous-preset.json"

func loadAutonomousPreset(t *testing.T) (*types.RunConfig, string) {
	t.Helper()
	must := require.New(t)
	root := repoRootForTests(t)
	path := filepath.Join(root, filepath.FromSlash(autonomousPresetRelPath))
	cfg, err := loadRunConfigFile(path)
	must.NoError(err, "loadRunConfigFile %q", path)
	return cfg, root
}

// TestAutonomousPreset_RunConfigValidate_Accepts pins that
// `stirrup run-config --config examples/runconfig/autonomous-preset.json
// --validate` succeeds.
func TestAutonomousPreset_RunConfigValidate_Accepts(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(repoRootForTests(t), filepath.FromSlash(autonomousPresetRelPath))
	_, err := runRunConfigForTest(t, []string{"--config", path, "--validate"}, "")
	must.NoError(err, "run-config --validate rejected %s", autonomousPresetRelPath)
}

// TestAutonomousPreset_UnattendedSafeguards_AreKept fails when the preset
// drops a component an operator copying it relies on to bound an
// unattended run.
func TestAutonomousPreset_UnattendedSafeguards_AreKept(t *testing.T) {
	must := require.New(t)
	cfg, root := loadAutonomousPreset(t)
	must.NoError(types.ValidateRunConfig(cfg), "%s fails ValidateRunConfig", autonomousPresetRelPath)

	t.Run("runs in execution mode", func(t *testing.T) {
		must := require.New(t)
		must.Equal("execution", cfg.Mode)
	})

	t.Run("executor is a sandboxing executor", func(t *testing.T) {
		must := require.New(t)
		must.Contains([]string{"container", "k8s-sandbox"}, cfg.Executor.Type)
	})

	t.Run("executor image is the sandbox image", func(t *testing.T) {
		must := require.New(t)
		must.True(strings.HasPrefix(cfg.Executor.Image, "ghcr.io/rxbynerd/stirrup-sandbox"),
			"executor.image %q is not the stirrup-sandbox image", cfg.Executor.Image)
	})

	t.Run("executor runtime isolates the kernel", func(t *testing.T) {
		must := require.New(t)
		must.NotEmpty(cfg.Executor.Runtime, "executor.runtime is empty")
		must.NotEqual("runc", cfg.Executor.Runtime, "executor.runtime must be a kernel-isolating runtime such as runsc")
	})

	t.Run("executor network is an allowlist", func(t *testing.T) {
		must := require.New(t)
		network := cfg.Executor.Network
		must.NotNil(network, "executor.network is not set")
		must.Equal("allowlist", network.Mode)
		must.NotEmpty(network.Allowlist, "executor.network lists no hosts")
	})

	t.Run("workspace export targets Cloud Storage", func(t *testing.T) {
		must := require.New(t)
		must.True(strings.HasPrefix(cfg.Executor.WorkspaceExportTo, "gs://"),
			"executor.workspaceExportTo %q is not a gs:// destination", cfg.Executor.WorkspaceExportTo)
	})

	t.Run("permission policy uses the policy engine", func(t *testing.T) {
		must := require.New(t)
		must.Equal("policy-engine", cfg.PermissionPolicy.Type)
	})

	t.Run("permission policy falls back to deny-all", func(t *testing.T) {
		must := require.New(t)
		must.Equal("deny-all", cfg.PermissionPolicy.Fallback)
	})

	t.Run("permission policy file exists in the repository", func(t *testing.T) {
		must := require.New(t)
		policy := filepath.Join(root, filepath.FromSlash(cfg.PermissionPolicy.PolicyFile))
		_, err := os.Stat(policy)
		must.NoError(err, "permissionPolicy.policyFile %q is not a file in the repository", cfg.PermissionPolicy.PolicyFile)
	})

	t.Run("verifier is a real verifier", func(t *testing.T) {
		must := require.New(t)
		must.NotEmpty(cfg.Verifier.Type, "verifier.type is empty")
		must.NotEqual("none", cfg.Verifier.Type, "verifier.type must not be none")
		if cfg.Verifier.Type == "test-runner" {
			must.NotEmpty(cfg.Verifier.Command, "verifier.command is empty")
		}
	})

	t.Run("git strategy is deterministic", func(t *testing.T) {
		must := require.New(t)
		must.Equal("deterministic", cfg.GitStrategy.Type)
	})

	t.Run("code scanner uses patterns", func(t *testing.T) {
		must := require.New(t)
		must.Equal("patterns", cfg.CodeScanner.Type)
	})

	t.Run("rule of two enforcement is on", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(cfg.RuleOfTwo, "ruleOfTwo is not set")
		must.NotNil(cfg.RuleOfTwo.Enforce, "ruleOfTwo.enforce is not set")
		must.True(*cfg.RuleOfTwo.Enforce, "ruleOfTwo.enforce must be true")
	})

	t.Run("max turns is positive", func(t *testing.T) {
		must := require.New(t)
		must.Positive(cfg.MaxTurns)
	})

	t.Run("timeout is set", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(cfg.Timeout)
	})

	t.Run("max token budget is set", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(cfg.MaxTokenBudget)
	})

	t.Run("result sink writes JSON to stdout", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(cfg.ResultSink, "resultSink is not set")
		must.Equal("stdout-json", cfg.ResultSink.Type)
	})

	t.Run("trace emitter is configured", func(t *testing.T) {
		must := require.New(t)
		must.NotEmpty(cfg.TraceEmitter.Type, "traceEmitter.type is empty")
	})
}

// TestAutonomousPreset_DryRunPreflight_ConstructsComponents runs the preflight
// entry point with every network probe gated off, so it needs neither a
// container engine nor provider credentials, and checks that each component
// builds and that no step fails.
//
// The workspace-export probe has no gate and contacts Google Cloud, so the
// export destination is cleared for this test only; the destination itself
// is pinned by TestAutonomousPreset_UnattendedSafeguards_AreKept.
func TestAutonomousPreset_DryRunPreflight_ConstructsComponents(t *testing.T) {
	must := require.New(t)
	cfg, root := loadAutonomousPreset(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-preset-test")

	cfg.Executor.WorkspaceExportTo = ""
	cfg.PermissionPolicy.PolicyFile = filepath.Join(root, filepath.FromSlash(cfg.PermissionPolicy.PolicyFile))

	report, err := core.Preflight(context.Background(), cfg, core.PreflightOptions{
		SkipProvider: true,
		SkipMCP:      true,
		SkipTrace:    true,
		SkipEgress:   true,
		SkipExecutor: true,
	})
	must.NoError(err)

	for _, step := range report.Steps {
		t.Run(step.Name, func(t *testing.T) {
			must := require.New(t)
			must.NotEqual(core.PreflightFail, step.Status, "%s (hint: %s)", step.Detail, step.Hint)
		})
	}

	must.True(report.OK, "preflight report not OK: %+v", report.Steps)

	constructedPolicy := slices.ContainsFunc(report.Steps, func(step core.PreflightStep) bool {
		return step.Name == "permission-policy" && step.Status == core.PreflightOK
	})
	must.True(constructedPolicy, "preflight did not construct the permission policy: %+v", report.Steps)
}
