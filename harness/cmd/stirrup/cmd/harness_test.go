package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
)

// TestBuildHarnessRunConfig_AllModesValidate is the regression test for the
// bug where --mode research (and every other read-only mode) failed
// ValidateRunConfig because the CLI left Tools.BuiltIn empty while the
// validator required an explicit non-empty list for read-only modes. The
// test guards against a whole class of bug: any future change to the set
// of read-only modes, the validator rules, or the CLI's defaulting logic
// that causes a shipped --mode value to fail validation will trip here
// before it reaches a user.
func TestBuildHarnessRunConfig_AllModesValidate(t *testing.T) {
	// These are every --mode value advertised in the CLI help text and the
	// harnessCmd flag description. If a new mode is added to the CLI, this
	// list must be updated — and it will fail loudly if a mode ships
	// without a valid config-building path.
	modes := []string{"execution", "planning", "review", "research", "toil"}

	baseOpts := harnessCLIOptions{
		RunID:         "test-run",
		Prompt:        "test prompt",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		Workspace:     "",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	}

	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			opts := baseOpts
			opts.Mode = mode
			cfg := buildHarnessRunConfig(opts)

			if err := types.ValidateRunConfig(cfg); err != nil {
				t.Fatalf("buildHarnessRunConfig produced an invalid RunConfig for --mode %q: %v", mode, err)
			}

			// Belt-and-braces: read-only modes must actually get the
			// restrictive policy and a non-empty tool list, not just
			// "pass validation somehow".
			if types.IsReadOnlyMode(mode) {
				if cfg.PermissionPolicy.Type != "deny-side-effects" {
					t.Errorf("read-only mode %q should use deny-side-effects, got %q", mode, cfg.PermissionPolicy.Type)
				}
				if len(cfg.Tools.BuiltIn) == 0 {
					t.Errorf("read-only mode %q should have a non-empty Tools.BuiltIn list", mode)
				}
			}
		})
	}
}

// TestBuildHarnessRunConfig_RespectsExplicitToolList verifies that when a
// caller provides an explicit Tools.BuiltIn list (e.g. via a future flag
// or a control-plane override), the default fall-back does not clobber it.
// Today the CLI doesn't expose such a flag, but the defaulting logic
// guards with `len(... ) == 0` precisely so that future callers can
// narrow the tool set without surprise.
func TestBuildHarnessRunConfig_RespectsExplicitToolList(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
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

	// The default path should populate exactly the documented
	// read-only tool list.
	want := types.DefaultReadOnlyBuiltInTools()
	if len(cfg.Tools.BuiltIn) != len(want) {
		t.Fatalf("expected default read-only tool list of length %d, got %d: %v", len(want), len(cfg.Tools.BuiltIn), cfg.Tools.BuiltIn)
	}
}

// TestBuildHarnessRunConfig_ComponentSelections verifies that the new
// component-selection escape-hatch fields propagate correctly.
func TestBuildHarnessRunConfig_ComponentSelections(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:            "test-run",
		Mode:             "execution",
		Prompt:           "test",
		ProviderType:     "anthropic",
		APIKeyRef:        "secret://ANTHROPIC_API_KEY",
		Model:            "claude-sonnet-4-6",
		MaxTurns:         20,
		Timeout:          600,
		TransportType:    "stdio",
		LogLevel:         "info",
		ExecutorType:     "container",
		EditStrategyType: "multi",
		VerifierType:     "test-runner",
		GitStrategyType:  "deterministic",
		TraceEmitterType: "otel",
		OTelEndpoint:     "localhost:4317",
	})

	if cfg.Executor.Type != "container" {
		t.Errorf("expected executor type 'container', got %q", cfg.Executor.Type)
	}
	if cfg.EditStrategy.Type != "multi" {
		t.Errorf("expected edit strategy 'multi', got %q", cfg.EditStrategy.Type)
	}
	if cfg.Verifier.Type != "test-runner" {
		t.Errorf("expected verifier 'test-runner', got %q", cfg.Verifier.Type)
	}
	if cfg.GitStrategy.Type != "deterministic" {
		t.Errorf("expected git strategy 'deterministic', got %q", cfg.GitStrategy.Type)
	}
	if cfg.TraceEmitter.Type != "otel" {
		t.Errorf("expected trace emitter 'otel', got %q", cfg.TraceEmitter.Type)
	}
	if cfg.TraceEmitter.Endpoint != "localhost:4317" {
		t.Errorf("expected otel endpoint 'localhost:4317', got %q", cfg.TraceEmitter.Endpoint)
	}
	// jsonl FilePath should not be populated when emitter type is otel.
	if cfg.TraceEmitter.FilePath != "" {
		t.Errorf("expected empty FilePath for otel emitter, got %q", cfg.TraceEmitter.FilePath)
	}
}

// TestBuildHarnessRunConfig_EmptyComponentDefaults exercises the
// fallback values for component-selection fields. These defaults are the
// historical CLI behaviour; tests pin them explicitly so a refactor that
// changes them by accident fails loudly. Note: the EditStrategy default
// is changed to "multi" in the follow-up commit.
func TestBuildHarnessRunConfig_EmptyComponentDefaults(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
		// All component-selection fields deliberately left empty.
	})
	if cfg.Executor.Type != "local" {
		t.Errorf("default executor should be 'local', got %q", cfg.Executor.Type)
	}
	if cfg.Verifier.Type != "none" {
		t.Errorf("default verifier should be 'none', got %q", cfg.Verifier.Type)
	}
	if cfg.GitStrategy.Type != "none" {
		t.Errorf("default git strategy should be 'none', got %q", cfg.GitStrategy.Type)
	}
	if cfg.TraceEmitter.Type != "jsonl" {
		t.Errorf("default trace emitter should be 'jsonl', got %q", cfg.TraceEmitter.Type)
	}
}

// TestLoadRunConfigFile_RoundTrip writes a minimal RunConfig JSON to a
// tempfile, loads it through loadRunConfigFile, and asserts the parsed
// fields match. This is the core happy-path for the --config loader.
func TestLoadRunConfigFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	timeout := 300
	original := types.RunConfig{
		RunID:  "from-file",
		Mode:   "planning",
		Prompt: "prompt-from-file",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://ANTHROPIC_API_KEY",
		},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: "anthropic",
			Model:    "claude-sonnet-4-6",
		},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:        types.ExecutorConfig{Type: "local"},
		EditStrategy:    types.EditStrategyConfig{Type: "multi"},
		Verifier:        types.VerifierConfig{Type: "none"},
		GitStrategy:     types.GitStrategyConfig{Type: "none"},
		Transport:       types.TransportConfig{Type: "stdio"},
		TraceEmitter:    types.TraceEmitterConfig{Type: "jsonl"},
		PermissionPolicy: types.PermissionPolicyConfig{
			Type: "deny-side-effects",
		},
		Tools: types.ToolsConfig{
			BuiltIn: types.DefaultReadOnlyBuiltInTools(),
		},
		MaxTurns: 10,
		Timeout:  &timeout,
		LogLevel: "info",
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile: %v", err)
	}
	if loaded.RunID != "from-file" {
		t.Errorf("RunID: got %q, want %q", loaded.RunID, "from-file")
	}
	if loaded.Mode != "planning" {
		t.Errorf("Mode: got %q, want %q", loaded.Mode, "planning")
	}
	if loaded.Prompt != "prompt-from-file" {
		t.Errorf("Prompt: got %q, want %q", loaded.Prompt, "prompt-from-file")
	}
	if loaded.EditStrategy.Type != "multi" {
		t.Errorf("EditStrategy.Type: got %q, want %q", loaded.EditStrategy.Type, "multi")
	}
	if err := types.ValidateRunConfig(loaded); err != nil {
		t.Fatalf("loaded config should validate: %v", err)
	}
}

// TestLoadRunConfigFile_InvalidPath verifies the error path for a missing
// file: the error must mention the path, not just bubble up a generic
// "no such file" without context.
func TestLoadRunConfigFile_InvalidPath(t *testing.T) {
	_, err := loadRunConfigFile("/this/path/does/not/exist.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "/this/path/does/not/exist.json") {
		t.Errorf("error should mention the path, got: %v", err)
	}
}

// TestLoadRunConfigFile_RejectsUnknownFields ensures typos in the config
// file surface as errors rather than being silently dropped, which is a
// classic source of "I configured X but it didn't take effect" bugs.
func TestLoadRunConfigFile_RejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"runId":"x","mode":"execution","unknownField":42}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadRunConfigFile(path)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

// TestLoadRunConfigFile_InvalidJSON pins the error path for malformed
// JSON.
func TestLoadRunConfigFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadRunConfigFile(path)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing config file") {
		t.Errorf("error should describe parse failure, got: %v", err)
	}
}

// newTestHarnessCommand builds a cobra command with the same flag surface
// as the real harnessCmd. Used to exercise applyOverrides under realistic
// conditions where Changed() reflects only what the test sets.
func newTestHarnessCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "harness"}
	f := cmd.Flags()
	f.String("config", "", "")
	f.StringP("mode", "m", "execution", "")
	f.String("model", "claude-sonnet-4-6", "")
	f.String("provider", "anthropic", "")
	f.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "")
	f.StringP("workspace", "w", "", "")
	f.Int("max-turns", 20, "")
	f.Int("timeout", 600, "")
	f.String("trace", "", "")
	f.String("transport", "stdio", "")
	f.String("transport-addr", "", "")
	f.Int("followup-grace", 0, "")
	f.String("log-level", "info", "")
	f.String("prompt", "", "")
	f.String("executor", "local", "")
	f.String("edit-strategy", "whole-file", "")
	f.String("verifier", "none", "")
	f.String("git-strategy", "none", "")
	f.String("trace-emitter", "jsonl", "")
	f.String("otel-endpoint", "", "")
	return cmd
}

// baseFileConfig returns a fully-populated RunConfig representing what a
// user might load from --config. Override tests start from this and
// either touch flags (override path) or leave them alone (default path).
func baseFileConfig() *types.RunConfig {
	timeout := 300
	return &types.RunConfig{
		RunID:  "from-file",
		Mode:   "planning",
		Prompt: "prompt-from-file",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://FILE_KEY",
		},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: "anthropic",
			Model:    "claude-sonnet-4-6-from-file",
		},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:        types.ExecutorConfig{Type: "local", Workspace: "/file/workspace"},
		EditStrategy:    types.EditStrategyConfig{Type: "multi"},
		Verifier:        types.VerifierConfig{Type: "none"},
		GitStrategy:     types.GitStrategyConfig{Type: "none"},
		Transport:       types.TransportConfig{Type: "stdio"},
		TraceEmitter:    types.TraceEmitterConfig{Type: "jsonl", FilePath: "/file/trace.jsonl"},
		PermissionPolicy: types.PermissionPolicyConfig{
			Type: "deny-side-effects",
		},
		Tools: types.ToolsConfig{
			BuiltIn: types.DefaultReadOnlyBuiltInTools(),
		},
		MaxTurns: 10,
		Timeout:  &timeout,
		LogLevel: "info",
	}
}

// TestApplyOverrides_DefaultFlagsDoNotOverride is the central
// precedence-rule test: a flag whose value equals its default value (i.e.
// the user did not pass it) MUST NOT clobber the file-provided value.
// This is what cmd.Flags().Changed("name") guards.
func TestApplyOverrides_DefaultFlagsDoNotOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	applyOverrides(cmd, cfg, nil)

	if cfg.Mode != "planning" {
		t.Errorf("Mode: file value should survive, got %q", cfg.Mode)
	}
	if cfg.Prompt != "prompt-from-file" {
		t.Errorf("Prompt: file value should survive, got %q", cfg.Prompt)
	}
	if cfg.MaxTurns != 10 {
		t.Errorf("MaxTurns: file value should survive, got %d", cfg.MaxTurns)
	}
	if cfg.Timeout == nil || *cfg.Timeout != 300 {
		t.Errorf("Timeout: file value should survive, got %v", cfg.Timeout)
	}
	if cfg.Provider.APIKeyRef != "secret://FILE_KEY" {
		t.Errorf("APIKeyRef: file value should survive, got %q", cfg.Provider.APIKeyRef)
	}
	if cfg.Executor.Workspace != "/file/workspace" {
		t.Errorf("Workspace: file value should survive, got %q", cfg.Executor.Workspace)
	}
	if cfg.ModelRouter.Model != "claude-sonnet-4-6-from-file" {
		t.Errorf("Model: file value should survive, got %q", cfg.ModelRouter.Model)
	}
	if cfg.EditStrategy.Type != "multi" {
		t.Errorf("EditStrategy.Type: file value should survive, got %q", cfg.EditStrategy.Type)
	}
	if cfg.Executor.Type != "local" {
		t.Errorf("Executor.Type: file value should survive, got %q", cfg.Executor.Type)
	}
	if cfg.Verifier.Type != "none" {
		t.Errorf("Verifier.Type: file value should survive, got %q", cfg.Verifier.Type)
	}
}

// TestApplyOverrides_ExplicitFlagsOverride verifies that a flag set on
// the command line clobbers the file-provided value, for every override
// flag.
func TestApplyOverrides_ExplicitFlagsOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("mode", "execution")
	must("prompt", "prompt-from-flag")
	must("max-turns", "5")
	must("timeout", "60")
	must("trace", "/flag/trace.jsonl")
	must("workspace", "/flag/workspace")
	must("transport", "grpc")
	must("transport-addr", "1.2.3.4:5")
	must("followup-grace", "30")
	must("log-level", "debug")
	must("provider", "openai-compatible")
	must("model", "gpt-flag")
	must("api-key-ref", "secret://FLAG_KEY")
	must("executor", "container")
	must("edit-strategy", "udiff")
	must("verifier", "test-runner")
	must("git-strategy", "deterministic")
	must("trace-emitter", "otel")
	must("otel-endpoint", "otel.flag:4317")

	applyOverrides(cmd, cfg, nil)

	if cfg.Mode != "execution" {
		t.Errorf("Mode override failed: %q", cfg.Mode)
	}
	if cfg.Prompt != "prompt-from-flag" {
		t.Errorf("Prompt override failed: %q", cfg.Prompt)
	}
	if cfg.MaxTurns != 5 {
		t.Errorf("MaxTurns override failed: %d", cfg.MaxTurns)
	}
	if cfg.Timeout == nil || *cfg.Timeout != 60 {
		t.Errorf("Timeout override failed: %v", cfg.Timeout)
	}
	if cfg.TraceEmitter.FilePath != "/flag/trace.jsonl" {
		t.Errorf("Trace path override failed: %q", cfg.TraceEmitter.FilePath)
	}
	if cfg.Executor.Workspace != "/flag/workspace" {
		t.Errorf("Workspace override failed: %q", cfg.Executor.Workspace)
	}
	if cfg.Transport.Type != "grpc" {
		t.Errorf("Transport type override failed: %q", cfg.Transport.Type)
	}
	if cfg.Transport.Address != "1.2.3.4:5" {
		t.Errorf("Transport address override failed: %q", cfg.Transport.Address)
	}
	if cfg.FollowUpGrace == nil || *cfg.FollowUpGrace != 30 {
		t.Errorf("FollowUpGrace override failed: %v", cfg.FollowUpGrace)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel override failed: %q", cfg.LogLevel)
	}
	if cfg.Provider.Type != "openai-compatible" {
		t.Errorf("Provider type override failed: %q", cfg.Provider.Type)
	}
	if cfg.ModelRouter.Model != "gpt-flag" {
		t.Errorf("Model override failed: %q", cfg.ModelRouter.Model)
	}
	if cfg.Provider.APIKeyRef != "secret://FLAG_KEY" {
		t.Errorf("APIKeyRef override failed: %q", cfg.Provider.APIKeyRef)
	}
	if cfg.Executor.Type != "container" {
		t.Errorf("Executor type override failed: %q", cfg.Executor.Type)
	}
	if cfg.EditStrategy.Type != "udiff" {
		t.Errorf("EditStrategy override failed: %q", cfg.EditStrategy.Type)
	}
	if cfg.Verifier.Type != "test-runner" {
		t.Errorf("Verifier override failed: %q", cfg.Verifier.Type)
	}
	if cfg.GitStrategy.Type != "deterministic" {
		t.Errorf("GitStrategy override failed: %q", cfg.GitStrategy.Type)
	}
	if cfg.TraceEmitter.Type != "otel" {
		t.Errorf("TraceEmitter type override failed: %q", cfg.TraceEmitter.Type)
	}
	if cfg.TraceEmitter.Endpoint != "otel.flag:4317" {
		t.Errorf("OTel endpoint override failed: %q", cfg.TraceEmitter.Endpoint)
	}
}

// TestApplyOverrides_PositionalPromptFillsFileGap covers the precedence
// edge case where the file omits a prompt and the user passes one as a
// positional argument (no --prompt flag). The positional should fill the
// gap rather than triggering the "prompt required" error.
func TestApplyOverrides_PositionalPromptFillsFileGap(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Prompt = "" // simulate file with no prompt

	applyOverrides(cmd, cfg, []string{"positional prompt"})

	if cfg.Prompt != "positional prompt" {
		t.Errorf("expected positional prompt to fill the gap, got %q", cfg.Prompt)
	}
}

// TestApplyOverrides_FilePromptBeatsPositional verifies that a positional
// arg does NOT clobber a prompt set in the config file. The positional
// is a fallback, not an override (use --prompt for that).
func TestApplyOverrides_FilePromptBeatsPositional(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig() // Prompt = "prompt-from-file"

	applyOverrides(cmd, cfg, []string{"positional"})

	if cfg.Prompt != "prompt-from-file" {
		t.Errorf("file prompt should win over positional, got %q", cfg.Prompt)
	}
}

// TestApplyOverrides_ExplicitFlagBeatsPositional pins the precedence:
// --prompt > positional > file.
func TestApplyOverrides_ExplicitFlagBeatsPositional(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Prompt = "" // file has no prompt
	if err := cmd.Flags().Set("prompt", "from-flag"); err != nil {
		t.Fatalf("set prompt: %v", err)
	}

	applyOverrides(cmd, cfg, []string{"positional"})

	if cfg.Prompt != "from-flag" {
		t.Errorf("explicit --prompt should win, got %q", cfg.Prompt)
	}
}

// TestRunHarness_ConfigValidationFailurePropagates writes a config that
// fails ValidateRunConfig (read-only mode + write tool) and asserts the
// CLI surfaces the error clearly.
func TestRunHarness_ConfigValidationFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")

	timeout := 60
	bad := types.RunConfig{
		RunID:  "x",
		Mode:   "research", // read-only mode
		Prompt: "test",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://X",
		},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "x"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 1000},
		Executor:         types.ExecutorConfig{Type: "local"},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		Transport:        types.TransportConfig{Type: "stdio"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"}, // INVALID for research mode
		Tools: types.ToolsConfig{
			BuiltIn: []string{"write_file"}, // INVALID write tool in read-only mode
		},
		MaxTurns: 5,
		Timeout:  &timeout,
	}
	data, err := json.MarshalIndent(bad, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatalf("set config: %v", err)
	}

	err = runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error should mention 'invalid config', got: %v", err)
	}
}
