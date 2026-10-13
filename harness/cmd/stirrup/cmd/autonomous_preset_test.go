package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
)

const autonomousPresetRelPath = "examples/runconfig/autonomous-preset.json"

func loadAutonomousPreset(t *testing.T) (*types.RunConfig, string) {
	t.Helper()
	root := repoRootForTests(t)
	path := filepath.Join(root, filepath.FromSlash(autonomousPresetRelPath))
	cfg, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile %q: %v", path, err)
	}
	return cfg, root
}

// TestAutonomousPreset_RunConfigValidatePasses pins that
// `stirrup run-config --config examples/runconfig/autonomous-preset.json
// --validate` succeeds.
func TestAutonomousPreset_RunConfigValidatePasses(t *testing.T) {
	path := filepath.Join(repoRootForTests(t), filepath.FromSlash(autonomousPresetRelPath))
	if _, err := runRunConfigForTest(t, []string{"--config", path, "--validate"}, ""); err != nil {
		t.Fatalf("run-config --validate rejected %s: %v", autonomousPresetRelPath, err)
	}
}

// TestAutonomousPreset_KeepsUnattendedSafeguards fails when the preset
// drops a component an operator copying it relies on to bound an
// unattended run.
func TestAutonomousPreset_KeepsUnattendedSafeguards(t *testing.T) {
	cfg, root := loadAutonomousPreset(t)

	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("%s fails ValidateRunConfig: %v", autonomousPresetRelPath, err)
	}
	if cfg.Mode != "execution" {
		t.Errorf("mode = %q, want execution", cfg.Mode)
	}

	if got := cfg.Executor.Type; got != "container" && got != "k8s-sandbox" {
		t.Errorf("executor.type = %q, want a sandboxing executor (container or k8s-sandbox)", got)
	}
	if !strings.HasPrefix(cfg.Executor.Image, "ghcr.io/rxbynerd/stirrup-sandbox") {
		t.Errorf("executor.image = %q, want the stirrup-sandbox image (not the harness image)", cfg.Executor.Image)
	}
	if cfg.Executor.Runtime == "" || cfg.Executor.Runtime == "runc" {
		t.Errorf("executor.runtime = %q, want a kernel-isolating runtime such as runsc", cfg.Executor.Runtime)
	}
	if n := cfg.Executor.Network; n == nil || n.Mode != "allowlist" || len(n.Allowlist) == 0 {
		t.Errorf("executor.network = %+v, want allowlist mode with at least one host", n)
	}
	if !strings.HasPrefix(cfg.Executor.WorkspaceExportTo, "gs://") {
		t.Errorf("executor.workspaceExportTo = %q, want a gs:// destination", cfg.Executor.WorkspaceExportTo)
	}

	if cfg.PermissionPolicy.Type != "policy-engine" {
		t.Errorf("permissionPolicy.type = %q, want policy-engine", cfg.PermissionPolicy.Type)
	}
	if cfg.PermissionPolicy.Fallback != "deny-all" {
		t.Errorf("permissionPolicy.fallback = %q, want deny-all", cfg.PermissionPolicy.Fallback)
	}
	policy := filepath.Join(root, filepath.FromSlash(cfg.PermissionPolicy.PolicyFile))
	if _, err := os.Stat(policy); err != nil {
		t.Errorf("permissionPolicy.policyFile %q is not a file in the repository: %v", cfg.PermissionPolicy.PolicyFile, err)
	}

	if v := cfg.Verifier.Type; v == "" || v == "none" {
		t.Errorf("verifier.type = %q, want a real verifier", v)
	}
	if cfg.Verifier.Type == "test-runner" && cfg.Verifier.Command == "" {
		t.Error("verifier.command is empty")
	}
	if cfg.GitStrategy.Type != "deterministic" {
		t.Errorf("gitStrategy.type = %q, want deterministic", cfg.GitStrategy.Type)
	}
	if cfg.CodeScanner.Type != "patterns" {
		t.Errorf("codeScanner.type = %q, want patterns", cfg.CodeScanner.Type)
	}
	if cfg.RuleOfTwo == nil || cfg.RuleOfTwo.Enforce == nil || !*cfg.RuleOfTwo.Enforce {
		t.Errorf("ruleOfTwo = %+v, want enforce=true", cfg.RuleOfTwo)
	}
	if cfg.MaxTurns <= 0 || cfg.Timeout == nil || cfg.MaxTokenBudget == nil {
		t.Errorf("maxTurns/timeout/maxTokenBudget must all be set, got %v/%v/%v", cfg.MaxTurns, cfg.Timeout, cfg.MaxTokenBudget)
	}
	if cfg.ResultSink == nil || cfg.ResultSink.Type != "stdout-json" {
		t.Errorf("resultSink = %+v, want stdout-json", cfg.ResultSink)
	}
	if cfg.TraceEmitter.Type == "" {
		t.Error("traceEmitter.type is empty")
	}
}

// TestAutonomousPreset_DryRunPreflightConstructs runs the preflight entry
// point with every network probe gated off, so it needs neither a container
// engine nor provider credentials, and checks that each component builds
// and that no step fails.
//
// The workspace-export probe has no gate and contacts Google Cloud, so the
// export destination is cleared for this test only; the destination itself
// is pinned by TestAutonomousPreset_KeepsUnattendedSafeguards.
func TestAutonomousPreset_DryRunPreflightConstructs(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	for _, step := range report.Steps {
		if step.Status == core.PreflightFail {
			t.Errorf("preflight step %q failed: %s (hint: %s)", step.Name, step.Detail, step.Hint)
		}
	}
	if !report.OK {
		t.Errorf("preflight report not OK: %+v", report.Steps)
	}
	var sawPolicy bool
	for _, step := range report.Steps {
		if step.Name == "permission-policy" && step.Status == core.PreflightOK {
			sawPolicy = true
		}
	}
	if !sawPolicy {
		t.Errorf("preflight did not construct the permission policy: %+v", report.Steps)
	}
}
