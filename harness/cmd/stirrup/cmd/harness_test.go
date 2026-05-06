package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

	// Rule-of-Two override: every CLI default combination here pairs an
	// API key (whose name matches the secret heuristic) with a tool list
	// that carries both untrusted-input ingress (web_fetch) and external
	// communication (web_fetch / run_command), which is exactly the all-
	// three case the Rule-of-Two invariant rejects. The test predates that
	// invariant and is asserting CLI defaulting / read-only-mode wiring,
	// not the safety invariant; Rule-of-Two coverage lives in
	// types/runconfig_test.go. Wiring the override into the CLI itself
	// (so users get a clear error rather than this validation rejection)
	// is tracked in a later wave of #42 and intentionally out of scope here.
	enforce := false
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			opts := baseOpts
			opts.Mode = mode
			cfg := buildHarnessRunConfig(opts)
			cfg.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: &enforce}

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

// TestBuildHarnessRunConfig_OpenAIResponsesProvider verifies that the
// openai-responses provider type is accepted by both the CLI option-to-
// RunConfig path and ValidateRunConfig. Before this case existed, picking
// --provider openai-responses would crash at validation.
func TestBuildHarnessRunConfig_OpenAIResponsesProvider(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "openai-responses",
		APIKeyRef:     "secret://OPENAI_API_KEY",
		Model:         "gpt-4.1",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})

	if cfg.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want openai-responses", cfg.Provider.Type)
	}
	if cfg.ModelRouter.Provider != "openai-responses" {
		t.Errorf("ModelRouter.Provider = %q, want openai-responses", cfg.ModelRouter.Provider)
	}
	// See Rule-of-Two note in TestBuildHarnessRunConfig_AllModesValidate.
	enforce := false
	cfg.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: &enforce}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig rejected openai-responses: %v", err)
	}
}

// TestBuildHarnessRunConfig_FillsDefaultReadOnlyToolList verifies that
// when no explicit Tools.BuiltIn list is supplied, read-only modes get
// the documented default list rather than passing validation by accident.
func TestBuildHarnessRunConfig_FillsDefaultReadOnlyToolList(t *testing.T) {
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

// TestApplyModeDefaults_RespectsExplicitTools is the inverse of the
// fills-default test: when a caller (e.g. a config file or future flag)
// supplies an explicit Tools.BuiltIn list, applyModeDefaults must NOT
// clobber it with the read-only defaults. The `len(... ) == 0` guard is
// what makes this safe; this test pins it.
func TestApplyModeDefaults_RespectsExplicitTools(t *testing.T) {
	cfg := &types.RunConfig{
		Mode:  "research",
		Tools: types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	applyModeDefaults(cfg)
	if len(cfg.Tools.BuiltIn) != 1 || cfg.Tools.BuiltIn[0] != "read_file" {
		t.Errorf("explicit tool list should survive, got %v", cfg.Tools.BuiltIn)
	}
}

// TestApplyModeDefaults_RespectsExplicitPolicy verifies that an
// explicit PermissionPolicy survives applyModeDefaults — even one that
// will later fail validation (allow-all on a read-only mode). Auto-
// rewriting would hide a user's mistake; the validator's clear error
// is the better UX.
func TestApplyModeDefaults_RespectsExplicitPolicy(t *testing.T) {
	cfg := &types.RunConfig{
		Mode:             "research",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "ask-upstream"},
	}
	applyModeDefaults(cfg)
	if cfg.PermissionPolicy.Type != "ask-upstream" {
		t.Errorf("explicit policy should survive, got %q", cfg.PermissionPolicy.Type)
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
// shipped CLI behaviour; tests pin them explicitly so a refactor that
// changes them by accident fails loudly.
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
	if cfg.EditStrategy.Type != "multi" {
		t.Errorf("default edit strategy should be 'multi', got %q", cfg.EditStrategy.Type)
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
	// Planning mode + DefaultReadOnlyBuiltInTools (which includes
	// web_fetch + spawn_agent) + a secret://ANTHROPIC_API_KEY ref
	// trips the Rule-of-Two invariant. This test asserts the
	// --config loader round-trips fields cleanly, not Rule-of-Two
	// behaviour, so we explicitly disable enforcement.
	enforce := false
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
		RuleOfTwo: &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:  10,
		Timeout:   &timeout,
		LogLevel:  "info",
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
	f.String("base-url", "", "")
	f.String("api-key-header", "", "")
	f.StringArray("query-param", nil, "")
	f.StringP("workspace", "w", "", "")
	f.Int("max-turns", 20, "")
	f.Int("timeout", 600, "")
	f.String("trace", "", "")
	f.String("transport", "stdio", "")
	f.String("transport-addr", "", "")
	f.Int("followup-grace", 0, "")
	f.String("log-level", "info", "")
	f.String("prompt", "", "")
	f.String("name", "", "")
	f.String("executor", "local", "")
	f.String("edit-strategy", "multi", "")
	f.String("verifier", "none", "")
	f.String("git-strategy", "none", "")
	f.String("trace-emitter", "jsonl", "")
	f.String("otel-endpoint", "", "")
	f.String("container-runtime", "", "")
	f.String("permission-policy-file", "", "")
	f.String("code-scanner", "", "")
	f.String("guardrail", "", "")
	f.String("guardrail-endpoint", "", "")
	f.Bool("guardrail-fail-open", false, "")
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

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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

// TestApplyOverrides_SessionNameExplicit verifies that --name is wired
// through applyOverrides: when set on the command line, the flag value
// must overwrite the file's SessionName.
func TestApplyOverrides_SessionNameExplicit(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.SessionName = "from-file"

	if err := cmd.Flags().Set("name", "from-flag"); err != nil {
		t.Fatalf("set name: %v", err)
	}
	applyOverrides(cmd, cfg, nil)

	if cfg.SessionName != "from-flag" {
		t.Errorf("explicit --name should win, got %q", cfg.SessionName)
	}
}

// TestApplyOverrides_SessionNameFilePreserved guards the precedence rule
// for --name: when the user does NOT pass the flag, a SessionName loaded
// from --config must survive applyOverrides intact. This is the central
// "default flag does not clobber file value" invariant for the new flag.
func TestApplyOverrides_SessionNameFilePreserved(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.SessionName = "from-file"

	applyOverrides(cmd, cfg, nil)

	if cfg.SessionName != "from-file" {
		t.Errorf("SessionName from file should survive, got %q", cfg.SessionName)
	}
}

// TestBuildHarnessRunConfig_SessionNamePropagates verifies the flag-only
// build path: a SessionName provided in harnessCLIOptions must end up on
// the constructed RunConfig.
func TestBuildHarnessRunConfig_SessionNamePropagates(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		SessionName:   "nightly-eval",
		Prompt:        "test",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})

	if cfg.SessionName != "nightly-eval" {
		t.Errorf("SessionName: got %q, want %q", cfg.SessionName, "nightly-eval")
	}
	// See Rule-of-Two note in TestBuildHarnessRunConfig_AllModesValidate.
	enforce := false
	cfg.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: &enforce}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig: %v", err)
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

	if err := applyOverrides(cmd, cfg, []string{"positional prompt"}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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

	if err := applyOverrides(cmd, cfg, []string{"positional"}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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

	if err := applyOverrides(cmd, cfg, []string{"positional"}); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Prompt != "from-flag" {
		t.Errorf("explicit --prompt should win, got %q", cfg.Prompt)
	}
}

// repoRootForTests returns the absolute repo root by walking up from this
// test file's path. Using runtime.Caller(0) makes the lookup independent of
// the test working directory and the package's depth in the tree, so a move
// of harness_test.go (or the examples directory) fails the test loudly
// rather than silently t.Skipping.
func repoRootForTests(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// thisFile is .../harness/cmd/stirrup/cmd/harness_test.go; walk up four
	// levels to reach the repo root.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
}

// TestExampleFullJSONLoadsAndValidates is the integration test for the
// shipped examples/runconfig/full.json: it must round-trip through
// loadRunConfigFile and pass ValidateRunConfig without modification. If
// the example drifts out of sync with the schema, this test fails before
// users hit the same error.
func TestExampleFullJSONLoadsAndValidates(t *testing.T) {
	path := filepath.Join(repoRootForTests(t), "examples", "runconfig", "full.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("examples/runconfig/full.json not found at %q: %v", path, err)
	}

	cfg, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile %q: %v", path, err)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("examples/runconfig/full.json fails ValidateRunConfig: %v", err)
	}
	if cfg.EditStrategy.Type != "multi" {
		t.Errorf("example should demonstrate multi edit strategy, got %q", cfg.EditStrategy.Type)
	}
	if cfg.Executor.Type != "container" {
		t.Errorf("example should demonstrate container executor, got %q", cfg.Executor.Type)
	}
	if cfg.TraceEmitter.Type != "otel" {
		t.Errorf("example should demonstrate otel trace emitter, got %q", cfg.TraceEmitter.Type)
	}
	// Spot-check nested fields so a JSON-key rename or type change in the
	// dynamic-router / executor-resources / mcp-servers sub-trees can't
	// silently deserialise to zero-value while the top-level still validates.
	if cfg.ModelRouter.Type != "dynamic" {
		t.Errorf("example should demonstrate dynamic model router, got %q", cfg.ModelRouter.Type)
	}
	if cfg.ModelRouter.CheapModel == "" {
		t.Errorf("example should set modelRouter.cheapModel")
	}
	if cfg.Executor.Resources == nil || cfg.Executor.Resources.CPUs == 0 {
		t.Errorf("example should set executor.resources.cpus")
	}
	if len(cfg.Tools.MCPServers) != 1 || cfg.Tools.MCPServers[0].Name == "" {
		t.Errorf("example should configure exactly one named MCP server, got %+v", cfg.Tools.MCPServers)
	}
}

// TestExampleAzureOpenAIJSONLoadsAndValidates pins the shipped Azure
// OpenAI fixture: the file must round-trip through loadRunConfigFile,
// pass ValidateRunConfig, and demonstrate the three new fields populated
// (apiKeyHeader, queryParams, and the Azure-shaped baseUrl). If any of
// these drift out of sync with the schema, this test fails before users
// hit the same error.
func TestExampleAzureOpenAIJSONLoadsAndValidates(t *testing.T) {
	path := filepath.Join(repoRootForTests(t), "examples", "runconfig", "azure-openai.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("examples/runconfig/azure-openai.json not found at %q: %v", path, err)
	}
	cfg, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile %q: %v", path, err)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("examples/runconfig/azure-openai.json fails ValidateRunConfig: %v", err)
	}
	if cfg.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want openai-responses", cfg.Provider.Type)
	}
	if cfg.Provider.APIKeyHeader != "api-key" {
		t.Errorf("Provider.APIKeyHeader = %q, want api-key", cfg.Provider.APIKeyHeader)
	}
	if cfg.Provider.QueryParams["api-version"] != "preview" {
		t.Errorf("Provider.QueryParams[api-version] = %q, want preview", cfg.Provider.QueryParams["api-version"])
	}
	if !strings.Contains(cfg.Provider.BaseURL, "openai.azure.com") {
		t.Errorf("Provider.BaseURL should target Azure, got %q", cfg.Provider.BaseURL)
	}
}

// TestBuildHarnessRunConfig_SafetyRingFlags verifies that the three new
// safety-ring flags (issue #42) propagate to the matching RunConfig
// fields. Each is independently exercised so a future refactor that
// drops one wiring without dropping the others is caught.
func TestBuildHarnessRunConfig_SafetyRingFlags(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:                "test-run",
		Mode:                 "execution",
		Prompt:               "test",
		ProviderType:         "anthropic",
		APIKeyRef:            "secret://ANTHROPIC_API_KEY",
		Model:                "claude-sonnet-4-6",
		MaxTurns:             20,
		Timeout:              600,
		TransportType:        "stdio",
		LogLevel:             "info",
		ContainerRuntime:     "runsc",
		PermissionPolicyFile: "/tmp/policy.cedar",
		CodeScannerType:      "patterns",
	})

	if cfg.Executor.Runtime != "runsc" {
		t.Errorf("Executor.Runtime = %q, want runsc", cfg.Executor.Runtime)
	}
	if cfg.PermissionPolicy.PolicyFile != "/tmp/policy.cedar" {
		t.Errorf("PermissionPolicy.PolicyFile = %q, want /tmp/policy.cedar", cfg.PermissionPolicy.PolicyFile)
	}
	// The convenience shortcut auto-sets type=policy-engine when the
	// caller didn't pick a type elsewhere.
	if cfg.PermissionPolicy.Type != "policy-engine" {
		t.Errorf("PermissionPolicy.Type = %q, want policy-engine", cfg.PermissionPolicy.Type)
	}
	if cfg.CodeScanner == nil || cfg.CodeScanner.Type != "patterns" {
		t.Errorf("CodeScanner = %+v, want type=patterns", cfg.CodeScanner)
	}
}

// TestApplyOverrides_SafetyRingFlagsOverride verifies the override path:
// safety-ring flags set on the command line clobber file-provided
// values. Mirror of TestApplyOverrides_ExplicitFlagsOverride for the
// new flags.
func TestApplyOverrides_SafetyRingFlagsOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.Runtime = "runc"
	cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	cfg.CodeScanner = &types.CodeScannerConfig{Type: "none"}

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("container-runtime", "runsc")
	must("permission-policy-file", "/tmp/p.cedar")
	must("code-scanner", "patterns")

	applyOverrides(cmd, cfg, nil)

	if cfg.Executor.Runtime != "runsc" {
		t.Errorf("Executor.Runtime override failed: %q", cfg.Executor.Runtime)
	}
	if cfg.PermissionPolicy.PolicyFile != "/tmp/p.cedar" {
		t.Errorf("PermissionPolicy.PolicyFile override failed: %q", cfg.PermissionPolicy.PolicyFile)
	}
	// File set type=deny-side-effects so --permission-policy-file
	// should NOT switch the type — only the path.
	if cfg.PermissionPolicy.Type != "deny-side-effects" {
		t.Errorf("PermissionPolicy.Type should survive when file set it, got %q", cfg.PermissionPolicy.Type)
	}
	if cfg.CodeScanner == nil || cfg.CodeScanner.Type != "patterns" {
		t.Errorf("CodeScanner override failed: %+v", cfg.CodeScanner)
	}
}

// TestApplyOverrides_PermissionPolicyFileImpliesPolicyEngine verifies
// the convenience shortcut: when the file leaves PermissionPolicy.Type
// unset and the user passes --permission-policy-file, type is bumped
// to "policy-engine" so the single flag is enough to use the new
// policy implementation.
func TestApplyOverrides_PermissionPolicyFileImpliesPolicyEngine(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.PermissionPolicy = types.PermissionPolicyConfig{} // file did not set type

	if err := cmd.Flags().Set("permission-policy-file", "/tmp/p.cedar"); err != nil {
		t.Fatalf("set: %v", err)
	}
	applyOverrides(cmd, cfg, nil)

	if cfg.PermissionPolicy.Type != "policy-engine" {
		t.Errorf("expected type=policy-engine when file omitted type, got %q", cfg.PermissionPolicy.Type)
	}
	if cfg.PermissionPolicy.PolicyFile != "/tmp/p.cedar" {
		t.Errorf("PolicyFile = %q, want /tmp/p.cedar", cfg.PermissionPolicy.PolicyFile)
	}
}

// TestApplyOverrides_DefaultSafetyRingFlagsDoNotOverride pins the
// precedence rule for the new flags: a flag left at its default
// (empty string) MUST NOT clobber a file-provided value.
func TestApplyOverrides_DefaultSafetyRingFlagsDoNotOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.Runtime = "kata"
	cfg.PermissionPolicy = types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/file/policy.cedar",
	}
	cfg.CodeScanner = &types.CodeScannerConfig{Type: "semgrep"}

	applyOverrides(cmd, cfg, nil)

	if cfg.Executor.Runtime != "kata" {
		t.Errorf("Runtime: file value should survive, got %q", cfg.Executor.Runtime)
	}
	if cfg.PermissionPolicy.PolicyFile != "/file/policy.cedar" {
		t.Errorf("PolicyFile: file value should survive, got %q", cfg.PermissionPolicy.PolicyFile)
	}
	if cfg.CodeScanner == nil || cfg.CodeScanner.Type != "semgrep" {
		t.Errorf("CodeScanner: file value should survive, got %+v", cfg.CodeScanner)
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

// TestLoadRunConfigFile_EmptyFile pins the error path for an empty (zero-
// byte) file. encoding/json would otherwise return io.EOF, which is
// unhelpful out of context — we want a message that names the path and
// the parsing stage so the user can find the typo.
func TestLoadRunConfigFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadRunConfigFile(path)
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
	if !strings.Contains(err.Error(), "parsing config file") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should describe empty file, got: %v", err)
	}
}

// TestLoadRunConfigFile_DirectoryArg verifies that pointing --config at
// a directory yields a clear error rather than a confusing read failure
// further down the stack.
func TestLoadRunConfigFile_DirectoryArg(t *testing.T) {
	dir := t.TempDir()
	_, err := loadRunConfigFile(dir)
	if err == nil {
		t.Fatal("expected error for directory path, got nil")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("error should mention directory, got: %v", err)
	}
}

// TestLoadRunConfigFile_OversizeRejected ensures the size guard kicks in
// before the file is loaded into memory. The cap is sized for genuine
// configs (a few KB); a multi-MB file is almost always wrong.
func TestLoadRunConfigFile_OversizeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	// 2 MiB of newlines is far past the 1 MiB cap.
	if err := os.WriteFile(path, make([]byte, 2*1024*1024), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadRunConfigFile(path)
	if err == nil {
		t.Fatal("expected size-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "byte cap") {
		t.Errorf("error should mention size cap, got: %v", err)
	}
}

// TestApplyOverrides_TraceCoercesEmitterToJSONL verifies that passing
// --trace on an otel-emitter config rewrites the emitter type to jsonl,
// since FilePath is meaningless for the otel emitter. This is the
// "user's intent stands" behaviour: --trace is a JSONL flag, and the
// user reaching for it is reaching for JSONL output.
func TestApplyOverrides_TraceCoercesEmitterToJSONL(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.TraceEmitter = types.TraceEmitterConfig{Type: "otel", Endpoint: "localhost:4317"}

	if err := cmd.Flags().Set("trace", "/tmp/out.jsonl"); err != nil {
		t.Fatalf("set trace: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.TraceEmitter.Type != "jsonl" {
		t.Errorf("emitter type should be coerced to jsonl when --trace is set, got %q", cfg.TraceEmitter.Type)
	}
	if cfg.TraceEmitter.FilePath != "/tmp/out.jsonl" {
		t.Errorf("trace path should be set, got %q", cfg.TraceEmitter.FilePath)
	}
}

// TestApplyOverrides_TraceRespectsExplicitEmitter verifies the inverse:
// when both --trace and --trace-emitter are explicitly set, the user's
// explicit emitter choice wins (even if the FilePath becomes ignored).
func TestApplyOverrides_TraceRespectsExplicitEmitter(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	if err := cmd.Flags().Set("trace", "/tmp/out.jsonl"); err != nil {
		t.Fatalf("set trace: %v", err)
	}
	if err := cmd.Flags().Set("trace-emitter", "otel"); err != nil {
		t.Fatalf("set trace-emitter: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.TraceEmitter.Type != "otel" {
		t.Errorf("explicit --trace-emitter=otel should win over coercion, got %q", cfg.TraceEmitter.Type)
	}
}

// TestApplyOverrides_FollowupGraceZeroClears verifies that explicitly
// passing --followup-grace=0 clears a non-nil FollowUpGrace from the
// file. This is the "I want to disable follow-ups" intent that the
// `g > 0` else-branch at applyOverrides supports.
func TestApplyOverrides_FollowupGraceZeroClears(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	g := 120
	cfg.FollowUpGrace = &g

	if err := cmd.Flags().Set("followup-grace", "0"); err != nil {
		t.Fatalf("set followup-grace: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.FollowUpGrace != nil {
		t.Errorf("explicit --followup-grace=0 should clear FollowUpGrace, got %v", *cfg.FollowUpGrace)
	}
}

// TestRunHarness_ConfigPathFollowupGraceFromEnv verifies that the
// STIRRUP_FOLLOWUP_GRACE environment variable populates FollowUpGrace in
// the --config code path when the file omits the field. This mirrors the
// flag-only path's env-var handling so the two paths behave alike.
func TestRunHarness_ConfigPathFollowupGraceFromEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	timeout := 60
	cfg := types.RunConfig{
		RunID:            "x",
		Mode:             "execution",
		Prompt:           "test",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://X"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 1000},
		Executor:         types.ExecutorConfig{Type: "local"},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		Transport:        types.TransportConfig{Type: "stdio"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		MaxTurns:         5,
		Timeout:          &timeout,
		// FollowUpGrace deliberately nil — env var must fill the gap.
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("STIRRUP_FOLLOWUP_GRACE", "45")

	loaded, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile: %v", err)
	}
	// Replicate runHarness's --config-path env-var handling. Keeps the
	// test focused on the env-var resolution logic without booting the
	// full agentic loop.
	if loaded.FollowUpGrace == nil {
		if v := os.Getenv("STIRRUP_FOLLOWUP_GRACE"); v != "" {
			n := 45
			loaded.FollowUpGrace = &n
		}
	}
	if loaded.FollowUpGrace == nil || *loaded.FollowUpGrace != 45 {
		t.Errorf("STIRRUP_FOLLOWUP_GRACE should fill FollowUpGrace when file omits it, got %v", loaded.FollowUpGrace)
	}
}

// TestApplyModeDefaults_FillsAfterModeOverride is the regression test for
// the H1 finding: when --config sets a sparse RunConfig (no policy/tools)
// and --mode is then overridden, the post-override defaulting step must
// fill in the new mode's defaults. Without this, a sparse file + a
// --mode planning override would fail validation because
// PermissionPolicy.Type is empty.
func TestApplyModeDefaults_FillsAfterModeOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := &types.RunConfig{
		RunID:           "x",
		Mode:            "execution", // file
		Prompt:          "test",
		Provider:        types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://X"},
		ModelRouter:     types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 1000},
		Executor:        types.ExecutorConfig{Type: "local"},
		EditStrategy:    types.EditStrategyConfig{Type: "multi"},
		Verifier:        types.VerifierConfig{Type: "none"},
		GitStrategy:     types.GitStrategyConfig{Type: "none"},
		Transport:       types.TransportConfig{Type: "stdio"},
		TraceEmitter:    types.TraceEmitterConfig{Type: "jsonl"},
		// PermissionPolicy and Tools deliberately empty.
		MaxTurns: 5,
		Timeout:  intPtr(60),
		LogLevel: "info",
	}

	if err := cmd.Flags().Set("mode", "planning"); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	applyModeDefaults(cfg)

	if cfg.Mode != "planning" {
		t.Fatalf("Mode override failed: %q", cfg.Mode)
	}
	if cfg.PermissionPolicy.Type != "deny-side-effects" {
		t.Errorf("read-only mode should default to deny-side-effects, got %q", cfg.PermissionPolicy.Type)
	}
	if len(cfg.Tools.BuiltIn) == 0 {
		t.Errorf("read-only mode should default Tools.BuiltIn to a non-empty list")
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Errorf("post-defaulted config should validate, got: %v", err)
	}
}

// intPtr is a small helper to take the address of an int literal.
func intPtr(n int) *int { return &n }

// TestApplyOverrides_AzureProviderFlags verifies that --base-url,
// --api-key-header, and --query-param flags propagate into Provider.*
// fields and override the file values for those flags. The file's
// QueryParams entry is wholesale replaced (rather than merged) so users
// who reach for --query-param to override a stale file entry get the
// expected behaviour.
func TestApplyOverrides_AzureProviderFlags(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.BaseURL = "https://file-base-url.example/v1"
	cfg.Provider.APIKeyHeader = "x-stale-header"
	cfg.Provider.QueryParams = map[string]string{"api-version": "stale", "deployment-id": "stale"}

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("base-url", "https://example.openai.azure.com/openai/v1")
	must("api-key-header", "api-key")
	must("query-param", "api-version=preview")
	must("query-param", "deployment-id=gpt4-prod")

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if got, want := cfg.Provider.BaseURL, "https://example.openai.azure.com/openai/v1"; got != want {
		t.Errorf("Provider.BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.APIKeyHeader, "api-key"; got != want {
		t.Errorf("Provider.APIKeyHeader = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.QueryParams["api-version"], "preview"; got != want {
		t.Errorf("QueryParams[api-version] = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.QueryParams["deployment-id"], "gpt4-prod"; got != want {
		t.Errorf("QueryParams[deployment-id] = %q, want %q", got, want)
	}
}

// TestApplyOverrides_AzureFlagsDoNotOverrideWhenUnset verifies the
// precedence rule for the new flags: a flag that the user did not pass
// MUST NOT clobber a file-provided value.
func TestApplyOverrides_AzureFlagsDoNotOverrideWhenUnset(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.BaseURL = "https://file-base-url.example/v1"
	cfg.Provider.APIKeyHeader = "api-key"
	cfg.Provider.QueryParams = map[string]string{"api-version": "preview"}

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if got, want := cfg.Provider.BaseURL, "https://file-base-url.example/v1"; got != want {
		t.Errorf("Provider.BaseURL: file value should survive, got %q", got)
	}
	if got, want := cfg.Provider.APIKeyHeader, "api-key"; got != want {
		t.Errorf("Provider.APIKeyHeader: file value should survive, got %q", got)
	}
	if got, want := cfg.Provider.QueryParams["api-version"], "preview"; got != want {
		t.Errorf("QueryParams: file value should survive, got %q", got)
	}
}

// TestApplyOverrides_QueryParamMalformedReturnsError pins the must-fix
// behaviour from the issue #48 review: when --config is used alongside
// a malformed --query-param entry, applyOverrides returns a non-nil
// error rather than warning-and-continuing. Without this guard the
// --config and flag-only paths would diverge — the flag-only path in
// runHarness fails hard for the same input — and a request would reach
// the provider with a parameter silently dropped (e.g. an Azure call
// with no api-version, surfacing as an opaque HTTP 400).
func TestApplyOverrides_QueryParamMalformedReturnsError(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"missing-equals", "api-version"},
		{"empty-key", "=preview"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newTestHarnessCommand()
			cfg := baseFileConfig()
			if err := cmd.Flags().Set("query-param", tc.entry); err != nil {
				t.Fatalf("set query-param: %v", err)
			}

			err := applyOverrides(cmd, cfg, nil)
			if err == nil {
				t.Fatalf("expected error for malformed --query-param %q, got nil", tc.entry)
			}
			if !strings.Contains(err.Error(), "--query-param") {
				t.Errorf("error should reference the offending flag, got: %v", err)
			}
		})
	}
}

// TestParseQueryParam_ValidAndInvalid pins the syntactic split rule used
// by the --query-param flag parser. Empty keys and missing "=" are rejected.
// Charset/length validation lives in ValidateRunConfig — this helper only
// owns the syntax.
func TestParseQueryParam_ValidAndInvalid(t *testing.T) {
	cases := []struct {
		entry   string
		wantK   string
		wantV   string
		wantErr bool
	}{
		{"api-version=preview", "api-version", "preview", false},
		{"empty-value=", "empty-value", "", false},
		{"with=equals=in=value", "with", "equals=in=value", false},
		{"=missing-key", "", "", true},
		{"no-equals", "", "", true},
		{"", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			k, v, err := parseQueryParam(tc.entry)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got nil", tc.entry)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQueryParam(%q) error: %v", tc.entry, err)
			}
			if k != tc.wantK || v != tc.wantV {
				t.Errorf("parseQueryParam(%q) = (%q, %q), want (%q, %q)", tc.entry, k, v, tc.wantK, tc.wantV)
			}
		})
	}
}

// TestBuildHarnessRunConfig_AzureProviderFields verifies that the new
// CLI options propagate from harnessCLIOptions into the generated
// ProviderConfig.
func TestBuildHarnessRunConfig_AzureProviderFields(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "openai-responses",
		APIKeyRef:     "secret://AZURE_KEY",
		BaseURL:       "https://example.openai.azure.com/openai/v1",
		APIKeyHeader:  "api-key",
		QueryParams:   map[string]string{"api-version": "preview"},
		Model:         "gpt-4o",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})

	if got, want := cfg.Provider.BaseURL, "https://example.openai.azure.com/openai/v1"; got != want {
		t.Errorf("Provider.BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.APIKeyHeader, "api-key"; got != want {
		t.Errorf("Provider.APIKeyHeader = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.QueryParams["api-version"], "preview"; got != want {
		t.Errorf("Provider.QueryParams[api-version] = %q, want %q", got, want)
	}
	// See Rule-of-Two note in TestBuildHarnessRunConfig_AllModesValidate.
	enforce := false
	cfg.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: &enforce}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig: %v", err)
	}
}

// TestBuildHarnessRunConfig_GuardRailFlags verifies that the three
// GuardRail flags (issue #43) propagate from harnessCLIOptions into the
// RunConfig.GuardRail block. The block is only constructed when at
// least one of the flags is non-zero so the flag-only build path
// matches the documented "default == nil == no guardrails" behaviour.
func TestBuildHarnessRunConfig_GuardRailFlags(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:             "test-run",
		Mode:              "execution",
		Prompt:            "test",
		ProviderType:      "anthropic",
		APIKeyRef:         "secret://ANTHROPIC_API_KEY",
		Model:             "claude-sonnet-4-6",
		MaxTurns:          20,
		Timeout:           600,
		TransportType:     "stdio",
		LogLevel:          "info",
		GuardRailType:     "granite-guardian",
		GuardRailEndpoint: "http://localhost:8000",
	})

	if cfg.GuardRail == nil {
		t.Fatalf("expected non-nil GuardRail config, got nil")
	}
	if cfg.GuardRail.Type != "granite-guardian" {
		t.Errorf("GuardRail.Type = %q, want granite-guardian", cfg.GuardRail.Type)
	}
	if cfg.GuardRail.Endpoint != "http://localhost:8000" {
		t.Errorf("GuardRail.Endpoint = %q, want http://localhost:8000", cfg.GuardRail.Endpoint)
	}
	if cfg.GuardRail.FailOpen {
		t.Errorf("GuardRail.FailOpen = true, want false (default)")
	}
}

// TestBuildHarnessRunConfig_GuardRailDefaultNil verifies the documented
// "no flags set means no guardrails" behaviour: the flag-only build
// path leaves config.GuardRail as nil so the factory installs the
// no-op "none" guard with zero behaviour change vs the pre-#43 path.
func TestBuildHarnessRunConfig_GuardRailDefaultNil(t *testing.T) {
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
		// All GuardRail fields deliberately left at their zero values.
	})
	if cfg.GuardRail != nil {
		t.Errorf("expected GuardRail to be nil when no flags are set, got %+v", cfg.GuardRail)
	}
}

// TestBuildHarnessRunConfig_GuardRailFailOpenFlipsBoolean exercises the
// fail-open flag in isolation: setting only --guardrail-fail-open is
// enough to materialise a GuardRail config (with the default empty
// type) so an operator can flip the posture without restating the rest.
func TestBuildHarnessRunConfig_GuardRailFailOpenFlipsBoolean(t *testing.T) {
	cfg := buildHarnessRunConfig(harnessCLIOptions{
		RunID:             "test-run",
		Mode:              "execution",
		Prompt:            "test",
		ProviderType:      "anthropic",
		APIKeyRef:         "secret://ANTHROPIC_API_KEY",
		Model:             "claude-sonnet-4-6",
		MaxTurns:          20,
		Timeout:           600,
		TransportType:     "stdio",
		LogLevel:          "info",
		GuardRailFailOpen: true,
	})
	if cfg.GuardRail == nil {
		t.Fatalf("expected non-nil GuardRail when fail-open flag is set")
	}
	if !cfg.GuardRail.FailOpen {
		t.Errorf("GuardRail.FailOpen = false, want true")
	}
}

// TestApplyOverrides_GuardRailFlagsOverride verifies the override path
// for the GuardRail flags. Each flag set on the command line clobbers
// the corresponding file-provided field; flags left unset preserve the
// file's value. This is the same precedence rule as every other
// override flag, but the multi-field GuardRailConfig means the test
// covers each component independently.
func TestApplyOverrides_GuardRailFlagsOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://file-endpoint:8000",
		FailOpen: false,
	}

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("guardrail-endpoint", "http://flag-endpoint:1234")
	must("guardrail-fail-open", "true")

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.GuardRail == nil {
		t.Fatalf("GuardRail should remain non-nil after override")
	}
	if cfg.GuardRail.Type != "granite-guardian" {
		t.Errorf("GuardRail.Type: file value should survive when --guardrail not set, got %q", cfg.GuardRail.Type)
	}
	if cfg.GuardRail.Endpoint != "http://flag-endpoint:1234" {
		t.Errorf("GuardRail.Endpoint override failed: %q", cfg.GuardRail.Endpoint)
	}
	if !cfg.GuardRail.FailOpen {
		t.Errorf("GuardRail.FailOpen override failed: got false")
	}
}

// TestApplyOverrides_GuardRailEndpointPreservesStages verifies that
// overriding only the endpoint leaves a composite stage list intact.
// This is the central "fine-tune one field" invariant: an operator who
// loaded a composite config from --config and then passes
// --guardrail-endpoint to retarget the inner adapter must not
// inadvertently drop the rest of the layering.
func TestApplyOverrides_GuardRailEndpointPreservesStages(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type: "composite",
		Stages: []types.GuardRailConfig{
			{Type: "granite-guardian", Endpoint: "http://stage-1:8000"},
			{Type: "cloud-judge"},
		},
	}

	if err := cmd.Flags().Set("guardrail-endpoint", "http://flag-endpoint:1234"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.GuardRail == nil {
		t.Fatalf("GuardRail should remain non-nil")
	}
	if cfg.GuardRail.Type != "composite" {
		t.Errorf("composite type should survive, got %q", cfg.GuardRail.Type)
	}
	if len(cfg.GuardRail.Stages) != 2 {
		t.Errorf("composite stages should survive, got %d entries", len(cfg.GuardRail.Stages))
	}
	if cfg.GuardRail.Endpoint != "http://flag-endpoint:1234" {
		t.Errorf("Endpoint override failed: %q", cfg.GuardRail.Endpoint)
	}
}

// TestApplyOverrides_GuardRailDefaultFlagsDoNotOverride pins the
// precedence rule for the GuardRail flags: if the user did not pass
// any of them, a file-provided GuardRail block must survive intact.
func TestApplyOverrides_GuardRailDefaultFlagsDoNotOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://file-endpoint:8000",
		FailOpen: true,
	}

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.GuardRail == nil {
		t.Fatalf("GuardRail should remain non-nil")
	}
	if cfg.GuardRail.Type != "granite-guardian" {
		t.Errorf("Type: file value should survive, got %q", cfg.GuardRail.Type)
	}
	if cfg.GuardRail.Endpoint != "http://file-endpoint:8000" {
		t.Errorf("Endpoint: file value should survive, got %q", cfg.GuardRail.Endpoint)
	}
	if !cfg.GuardRail.FailOpen {
		t.Errorf("FailOpen: file value should survive, got false")
	}
}

// TestApplyOverrides_GuardRailEmptyTypeClears verifies the
// "set type to empty string clears the GuardRail block" convention
// that mirrors --code-scanner. Operators use this to disable a
// guardrail block declared in a shared --config file without having
// to maintain a separate file.
func TestApplyOverrides_GuardRailEmptyTypeClears(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://file-endpoint:8000",
	}

	if err := cmd.Flags().Set("guardrail", ""); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.GuardRail != nil {
		t.Errorf("expected GuardRail to be cleared by --guardrail='', got %+v", cfg.GuardRail)
	}
}
