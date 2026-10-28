package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/resultsink"
	"github.com/rxbynerd/stirrup/types"
)

// TestBuildHarnessRunConfig_AllModesValidate pins that every --mode value
// produces a RunConfig that passes ValidateRunConfig.
func TestBuildHarnessRunConfig_AllModesValidate(t *testing.T) {
	// Every --mode value advertised in the CLI help text; update when a
	// mode is added.
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
			must := require.New(t)
			opts := baseOpts
			opts.Mode = mode
			cfg, err := buildHarnessRunConfig(opts)
			must.NoError(err)
			must.NoError(types.ValidateRunConfig(cfg))

			if types.IsReadOnlyMode(mode) {
				must.Equal("deny-side-effects", cfg.PermissionPolicy.Type)
				must.NotEmpty(cfg.Tools.BuiltIn)
			}
		})
	}
}

// TestHarnessCmd_DefaultModeIsPlanning pins that harnessCmd's --mode flag
// defaults to the read-only "planning" mode, not "execution".
func TestHarnessCmd_DefaultModeIsPlanning(t *testing.T) {
	must := require.New(t)
	modeFlag := harnessCmd.Flags().Lookup("mode")
	must.NotNil(modeFlag, "--mode flag is not registered on harnessCmd")
	must.Equal("planning", modeFlag.DefValue)
}

// TestBuildHarnessRunConfig_BareInvocationValidatesAsPlanning proves that a
// flag-only invocation with no --mode override lands in "planning" and
// passes ValidateRunConfig, including the read-only-mode invariants.
func TestBuildHarnessRunConfig_BareInvocationValidatesAsPlanning(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	defaultMode := harnessCmd.Flags().Lookup("mode").DefValue

	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          defaultMode,
		Prompt:        "test prompt",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})
	must.NoError(err)
	check.Equal("planning", cfg.Mode, "bare invocation should land in planning mode")
	must.NoError(types.ValidateRunConfig(cfg), "bare invocation must validate cleanly")
	check.Equal("deny-side-effects", cfg.PermissionPolicy.Type, "planning mode should default to deny-side-effects")
	must.NotEmpty(cfg.Tools.BuiltIn, "planning mode should populate the default tool list")
	for _, writeTool := range []string{"write_file", "edit_file", "run_command"} {
		check.NotContains(cfg.Tools.BuiltIn, writeTool, "planning mode must not enable a write tool by default")
	}
}

// TestBuildHarnessRunConfig_AcceptsOpenAIResponsesProvider verifies that the
// openai-responses provider type is accepted by both the CLI option-to-
// RunConfig path and ValidateRunConfig.
func TestBuildHarnessRunConfig_AcceptsOpenAIResponsesProvider(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	must.NoError(err)

	check.Equal("openai-responses", cfg.Provider.Type, "provider type")
	check.Equal("openai-responses", cfg.ModelRouter.Provider, "model router provider")
	must.NoError(types.ValidateRunConfig(cfg))
}

// TestBuildHarnessRunConfig_BedrockDefaultModelFailsValidation pins that
// ValidateRunConfig (not the provider) rejects the Anthropic-API model
// alias on Bedrock, before any network round-trip.
func TestBuildHarnessRunConfig_BedrockDefaultModelFailsValidation(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "bedrock",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY", // CLI default; ignored by bedrock auth
		Model:         "claude-sonnet-4-6",          // CLI default
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})
	must.NoError(err)

	verr := types.ValidateRunConfig(cfg)
	must.Error(verr, "ValidateRunConfig should reject --provider bedrock with the CLI-default --model")
	errStr := verr.Error()
	check.Contains(errStr, "bedrock")
	check.True(strings.Contains(errStr, "inference-profile") ||
		strings.Contains(errStr, "inference profile") ||
		strings.Contains(errStr, "list-inference-profiles"),
		"error should point at the inference-profile remediation path, got: %v", verr)
	check.Contains(errStr, "claude-sonnet-4-6", "error should name the offending model id")
}

// TestBuildHarnessRunConfig_BedrockInferenceProfileValidates is the
// positive complement: with a properly-shaped inference profile id,
// the flag-only path produces a config that passes validation.
func TestBuildHarnessRunConfig_BedrockInferenceProfileValidates(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "bedrock",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "eu.anthropic.claude-sonnet-4-6",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})
	must.NoError(err)
	must.NoError(types.ValidateRunConfig(cfg), "eu.anthropic.claude-sonnet-4-6 should validate on bedrock")
}

// TestBuildHarnessRunConfig_FillsDefaultReadOnlyToolList verifies that
// when no explicit Tools.BuiltIn list is supplied, read-only modes get
// the documented default list rather than passing validation by accident.
func TestBuildHarnessRunConfig_FillsDefaultReadOnlyToolList(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	want := types.DefaultReadOnlyBuiltInTools()
	must.Len(cfg.Tools.BuiltIn, len(want), "read-only mode should fill the default tool list")
}

// TestBuildHarnessRunConfig_NoneExecutorReadOnlyDefaultsToUngatedTools pins
// that a read-only mode with executor.type=none resolves Tools.BuiltIn to
// exactly the capability-ungated subset, and the result passes
// ValidateRunConfig.
func TestBuildHarnessRunConfig_NoneExecutorReadOnlyDefaultsToUngatedTools(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "planning",
		ExecutorType:  "none",
		Prompt:        "test",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})
	must.NoError(err)

	must.Equal([]string{"web_fetch", "spawn_agent"}, cfg.Tools.BuiltIn)
	must.NoError(types.ValidateRunConfig(cfg), "mode-injected default should pass validation for executor.type=none")
}

// TestApplyModeDefaults_NoneExecutorPopulatesUngatedTools pins the exact
// tool list applyModeDefaults produces for executor.type=none.
func TestApplyModeDefaults_NoneExecutorPopulatesUngatedTools(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{
		Mode:     "planning",
		Executor: types.ExecutorConfig{Type: "none"},
	}
	applyModeDefaults(cfg)
	must.Equal([]string{"web_fetch", "spawn_agent"}, cfg.Tools.BuiltIn)
}

// TestApplyModeDefaults_ExecutionModeNoneExecutorLeavesToolsEmpty confirms
// applyModeDefaults never applies the read-only default outside a
// read-only mode: an empty Tools.BuiltIn in execution mode means "all
// built-ins" and must stay empty.
func TestApplyModeDefaults_ExecutionModeNoneExecutorLeavesToolsEmpty(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	timeout := 600
	cfg := &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic"},
		MaxTurns: 20,
		Timeout:  &timeout,
		Executor: types.ExecutorConfig{Type: "none"},
	}
	applyModeDefaults(cfg)
	check.Empty(cfg.Tools.BuiltIn, "execution mode should not apply a default tool list")
	must.NoError(types.ValidateRunConfig(cfg), "execution mode with executor.type=none and no tools should validate")
}

// TestApplyModeDefaults_NoneExecutorExplicitToolStillRejected pins that
// applyModeDefaults filters only the mode-injected tool default, never an
// operator's explicit tools.builtIn entry: planning + none + an explicit
// ["read_file"] must still fail ValidateRunConfig's fail-fast.
func TestApplyModeDefaults_NoneExecutorExplicitToolStillRejected(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{
		Mode:             "planning",
		Executor:         types.ExecutorConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	applyModeDefaults(cfg)
	must.Equal([]string{"read_file"}, cfg.Tools.BuiltIn, "explicit tool list should survive applyModeDefaults")

	err := types.ValidateRunConfig(cfg)
	must.ErrorContains(err, `tools.builtIn entry "read_file" requires an execution capability`)
}

// TestApplyModeDefaults_RespectsExplicitTools pins that applyModeDefaults
// must not clobber an explicit Tools.BuiltIn list with the read-only
// defaults.
func TestApplyModeDefaults_RespectsExplicitTools(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{
		Mode:  "research",
		Tools: types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	applyModeDefaults(cfg)
	must.Equal([]string{"read_file"}, cfg.Tools.BuiltIn)
}

// TestApplyModeDefaults_RespectsExplicitPolicy verifies that an explicit
// PermissionPolicy survives applyModeDefaults, even one that will later
// fail validation: applyModeDefaults never rewrites an operator's choice.
func TestApplyModeDefaults_RespectsExplicitPolicy(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{
		Mode:             "research",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "ask-upstream"},
	}
	applyModeDefaults(cfg)
	must.Equal("ask-upstream", cfg.PermissionPolicy.Type)
}

// TestBuildHarnessRunConfig_PropagatesComponentSelections verifies that the
// component-selection escape-hatch fields reach the RunConfig.
func TestBuildHarnessRunConfig_PropagatesComponentSelections(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
		OTelProtocol:     "http/protobuf",
	})
	must.NoError(err)

	check.Equal("container", cfg.Executor.Type, "executor type")
	check.Equal("multi", cfg.EditStrategy.Type, "edit strategy type")
	check.Equal("test-runner", cfg.Verifier.Type, "verifier type")
	check.Equal("deterministic", cfg.GitStrategy.Type, "git strategy type")
	check.Equal("otel", cfg.TraceEmitter.Type, "trace emitter type")
	check.Equal("localhost:4317", cfg.TraceEmitter.Endpoint, "otel endpoint")
	check.Equal("http/protobuf", cfg.TraceEmitter.Protocol, "otel protocol")
	check.Empty(cfg.TraceEmitter.FilePath, "file path should stay empty for the otel emitter")
}

// TestBuildHarnessRunConfig_EmptyComponentsUseDefaults pins the fallback
// values buildHarnessRunConfig applies for empty component-selection
// fields. EditStrategy.Type is not asserted here: it is defaulted by
// types.ValidateRunConfig, not by this layer (see
// TestBuildRunConfig_EmptyEditStrategyResolvesToMulti).
func TestBuildHarnessRunConfig_EmptyComponentsUseDefaults(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	})
	must.NoError(err)
	check.Equal("local", cfg.Executor.Type, "default executor type")
	check.Equal("none", cfg.Verifier.Type, "default verifier type")
	check.Equal("none", cfg.GitStrategy.Type, "default git strategy type")
	check.Equal("jsonl", cfg.TraceEmitter.Type, "default trace emitter type")
}

// TestBuildHarnessRunConfig_ObservabilityFallsBackToEnv pins that
// buildHarnessRunConfig leaves Observability empty when no flag is set,
// so observability.BuildResource's env-var fallback chain applies.
func TestBuildHarnessRunConfig_ObservabilityFallsBackToEnv(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("OTEL_DEPLOYMENT_ENVIRONMENT", "production-eu")
	t.Setenv("OTEL_SERVICE_NAMESPACE", "")

	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	})
	must.NoError(err)

	check.Empty(cfg.Observability.Environment, "Observability.Environment should stay empty when the flag is unset")

	res := observability.BuildResource(observability.ResourceOptions{
		Environment:      cfg.Observability.Environment,
		ServiceNamespace: cfg.Observability.ServiceNamespace,
		RunMode:          cfg.Mode,
	})
	got := make(map[string]string)
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	check.Equal("production-eu", got["deployment.environment.name"], "deployment.environment.name should fall through to the env var")
}

// TestLoadRunConfigFile_RoundTrip writes a minimal RunConfig JSON to a
// tempfile, loads it through loadRunConfigFile, and asserts the parsed
// fields match. This is the core happy-path for the --config loader.
func TestLoadRunConfigFile_RoundTrip(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.json")

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
	must.NoError(err)
	must.NoError(os.WriteFile(path, data, 0o600))

	loaded, err := loadRunConfigFile(path)
	must.NoError(err)
	check.Equal("from-file", loaded.RunID, "RunID")
	check.Equal("planning", loaded.Mode, "Mode")
	check.Equal("prompt-from-file", loaded.Prompt, "Prompt")
	check.Equal("multi", loaded.EditStrategy.Type, "EditStrategy.Type")
	must.NoError(types.ValidateRunConfig(loaded))
}

// TestLoadRunConfigFile_MissingFileErrorNamesPath verifies the error path for
// a missing file: the error must mention the path, not just bubble up a
// generic "no such file" without context.
func TestLoadRunConfigFile_MissingFileErrorNamesPath(t *testing.T) {
	must := require.New(t)
	const missing = "/this/path/does/not/exist.json"
	_, err := loadRunConfigFile(missing)
	must.ErrorContains(err, missing)
}

// TestLoadRunConfigFile_RejectsUnknownFields ensures typos in the config
// file surface as errors rather than being silently dropped, which is a
// classic source of "I configured X but it didn't take effect" bugs.
func TestLoadRunConfigFile_RejectsUnknownFields(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"runId":"x","mode":"execution","unknownField":42}`
	must.NoError(os.WriteFile(path, []byte(body), 0o600))

	_, err := loadRunConfigFile(path)
	must.Error(err)
}

// TestLoadRunConfigFile_RejectsMalformedJSON pins the error path for
// malformed JSON.
func TestLoadRunConfigFile_RejectsMalformedJSON(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "config.json")
	must.NoError(os.WriteFile(path, []byte("not json"), 0o600))

	_, err := loadRunConfigFile(path)
	must.ErrorContains(err, "parsing config file")
}

// newTestHarnessCommand builds a cobra command with the same flag surface
// as the real harnessCmd, so applyOverrides sees realistic Changed()
// behaviour. Delegates the RunConfig-producing flag surface to
// addRunConfigFlags, the same factory the run-config subcommand uses.
func newTestHarnessCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "harness"}
	addRunConfigFlags(cmd)
	f := cmd.Flags()
	// Harness-only behaviour flags that do not round-trip through
	// RunConfig. Mirrors the registration in harness.go init().
	f.String("output-runconfig", "", "")
	f.StringP("output", "o", "text", "")
	addTraceParentFlags(f)
	// Dry-run flags. Mirrors the registration in harness.go init() so
	// flag-combination tests can exercise validateDryRunFlags.
	f.Bool("dry-run", false, "")
	f.Bool("no-probe-provider", false, "")
	f.Bool("no-probe-mcp", false, "")
	f.Bool("no-probe-trace", false, "")
	f.Bool("no-probe-egress", false, "")
	f.Bool("no-probe-executor", false, "")
	f.Duration("dry-run-timeout", core.DefaultPreflightTimeout, "")
	// Debug-build-only flags. Mirrors the registration in harness.go init()
	// so flag-combination tests can exercise validateDebugBuildFlags.
	f.Bool("debug", false, "")
	f.Bool("trace-wire", false, "")
	addTransportTLSFlags(f)
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
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	must.NoError(applyOverrides(cmd, cfg, nil))

	t.Run("mode", func(t *testing.T) {
		must := require.New(t)
		must.Equal("planning", cfg.Mode)
	})
	t.Run("prompt", func(t *testing.T) {
		must := require.New(t)
		must.Equal("prompt-from-file", cfg.Prompt)
	})
	t.Run("max turns", func(t *testing.T) {
		must := require.New(t)
		must.Equal(10, cfg.MaxTurns)
	})
	t.Run("timeout", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(cfg.Timeout)
		must.Equal(300, *cfg.Timeout)
	})
	t.Run("api key ref", func(t *testing.T) {
		must := require.New(t)
		must.Equal("secret://FILE_KEY", cfg.Provider.APIKeyRef)
	})
	t.Run("workspace", func(t *testing.T) {
		must := require.New(t)
		must.Equal("/file/workspace", cfg.Executor.Workspace)
	})
	t.Run("model", func(t *testing.T) {
		must := require.New(t)
		must.Equal("claude-sonnet-4-6-from-file", cfg.ModelRouter.Model)
	})
	t.Run("edit strategy", func(t *testing.T) {
		must := require.New(t)
		must.Equal("multi", cfg.EditStrategy.Type)
	})
	t.Run("executor type", func(t *testing.T) {
		must := require.New(t)
		must.Equal("local", cfg.Executor.Type)
	})
	t.Run("verifier type", func(t *testing.T) {
		must := require.New(t)
		must.Equal("none", cfg.Verifier.Type)
	})
}

// TestApplyOverrides_ExplicitFlagsOverride verifies that a flag set on
// the command line clobbers the file-provided value, for every override
// flag.
func TestApplyOverrides_ExplicitFlagsOverride(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	setFlag := func(name, value string) {
		must.NoError(cmd.Flags().Set(name, value), "set %s", name)
	}
	setFlag("mode", "execution")
	setFlag("prompt", "prompt-from-flag")
	setFlag("max-turns", "5")
	setFlag("timeout", "60")
	setFlag("trace", "/flag/trace.jsonl")
	setFlag("workspace", "/flag/workspace")
	setFlag("transport", "grpc")
	setFlag("transport-addr", "1.2.3.4:5")
	setFlag("followup-grace", "30")
	setFlag("log-level", "debug")
	setFlag("provider", "openai-compatible")
	setFlag("model", "gpt-flag")
	setFlag("api-key-ref", "secret://FLAG_KEY")
	setFlag("executor", "container")
	setFlag("edit-strategy", "udiff")
	setFlag("verifier", "test-runner")
	setFlag("git-strategy", "deterministic")
	setFlag("trace-emitter", "otel")
	setFlag("otel-endpoint", "otel.flag:4317")
	setFlag("otel-protocol", "http/protobuf")

	must.NoError(applyOverrides(cmd, cfg, nil))

	t.Run("mode", func(t *testing.T) {
		must := require.New(t)
		must.Equal("execution", cfg.Mode)
	})
	t.Run("prompt", func(t *testing.T) {
		must := require.New(t)
		must.Equal("prompt-from-flag", cfg.Prompt)
	})
	t.Run("max turns", func(t *testing.T) {
		must := require.New(t)
		must.Equal(5, cfg.MaxTurns)
	})
	t.Run("timeout", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(cfg.Timeout)
		must.Equal(60, *cfg.Timeout)
	})
	t.Run("trace path", func(t *testing.T) {
		must := require.New(t)
		must.Equal("/flag/trace.jsonl", cfg.TraceEmitter.FilePath)
	})
	t.Run("workspace", func(t *testing.T) {
		must := require.New(t)
		must.Equal("/flag/workspace", cfg.Executor.Workspace)
	})
	t.Run("transport type", func(t *testing.T) {
		must := require.New(t)
		must.Equal("grpc", cfg.Transport.Type)
	})
	t.Run("transport address", func(t *testing.T) {
		must := require.New(t)
		must.Equal("1.2.3.4:5", cfg.Transport.Address)
	})
	t.Run("followup grace", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(cfg.FollowUpGrace)
		must.Equal(30, *cfg.FollowUpGrace)
	})
	t.Run("log level", func(t *testing.T) {
		must := require.New(t)
		must.Equal("debug", cfg.LogLevel)
	})
	t.Run("provider type", func(t *testing.T) {
		must := require.New(t)
		must.Equal("openai-compatible", cfg.Provider.Type)
	})
	t.Run("model", func(t *testing.T) {
		must := require.New(t)
		must.Equal("gpt-flag", cfg.ModelRouter.Model)
	})
	t.Run("api key ref", func(t *testing.T) {
		must := require.New(t)
		must.Equal("secret://FLAG_KEY", cfg.Provider.APIKeyRef)
	})
	t.Run("executor type", func(t *testing.T) {
		must := require.New(t)
		must.Equal("container", cfg.Executor.Type)
	})
	t.Run("edit strategy", func(t *testing.T) {
		must := require.New(t)
		must.Equal("udiff", cfg.EditStrategy.Type)
	})
	t.Run("verifier type", func(t *testing.T) {
		must := require.New(t)
		must.Equal("test-runner", cfg.Verifier.Type)
	})
	t.Run("git strategy", func(t *testing.T) {
		must := require.New(t)
		must.Equal("deterministic", cfg.GitStrategy.Type)
	})
	t.Run("trace emitter type", func(t *testing.T) {
		must := require.New(t)
		must.Equal("otel", cfg.TraceEmitter.Type)
	})
	t.Run("otel endpoint", func(t *testing.T) {
		must := require.New(t)
		must.Equal("otel.flag:4317", cfg.TraceEmitter.Endpoint)
	})
	t.Run("otel protocol", func(t *testing.T) {
		must := require.New(t)
		must.Equal("http/protobuf", cfg.TraceEmitter.Protocol)
	})
}

// TestApplyOverrides_OTelProtocolFilePreserved pins that an unset
// --otel-protocol flag (the default empty string) does NOT clobber a
// Protocol value supplied by --config. This is the same precedence
// rule that already applies to every other override flag.
func TestApplyOverrides_OTelProtocolFilePreserved(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.TraceEmitter = types.TraceEmitterConfig{
		Type:     "otel",
		Endpoint: "https://otlp-gateway-prod-us-east-0.grafana.net/otlp",
		Protocol: "http/protobuf",
	}

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("http/protobuf", cfg.TraceEmitter.Protocol)
}

// TestApplyOverrides_OTelHeaderReplacesFileHeaders pins the replace-not-
// merge semantics for --otel-header: explicit flags clear any headers
// from --config (mirroring --query-param), repeatable entries accumulate,
// and values keep everything after the first '=' so a Basic credential's
// base64 padding survives.
func TestApplyOverrides_OTelHeaderReplacesFileHeaders(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.TraceEmitter = types.TraceEmitterConfig{
		Type:     "otel",
		Protocol: "http/protobuf",
		Headers:  map[string]string{"X-Stale": "from-file"},
	}

	for _, entry := range []string{
		"Authorization=secret://LANGFUSE_AUTH",
		"X-Tenant=team=a", // '=' in the value must survive the split
	} {
		must.NoError(cmd.Flags().Set("otel-header", entry))
	}
	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotContains(cfg.TraceEmitter.Headers, "X-Stale", "explicit --otel-header should clear file headers")
	must.Equal("secret://LANGFUSE_AUTH", cfg.TraceEmitter.Headers["Authorization"])
	must.Equal("team=a", cfg.TraceEmitter.Headers["X-Tenant"], "value split must be first '=' only")
}

// TestApplyOverrides_OTelHeaderMalformedRejected pins the hard-fail on a
// missing '=': silently dropping a malformed auth header would let the
// run proceed and surface as an opaque 401 from the OTLP gateway at
// first export, mirroring the --query-param rationale.
func TestApplyOverrides_OTelHeaderMalformedRejected(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	must.NoError(cmd.Flags().Set("otel-header", "no-equals-sign"))
	err := applyOverrides(cmd, cfg, nil)
	must.ErrorContains(err, "--otel-header")
}

// TestApplyOverrides_OTelPlatformFlags pins that --otel-platform overrides
// the file's platform and that --otel-platform-option replaces, rather than
// merges with, the file's options, as --otel-header does.
func TestApplyOverrides_OTelPlatformFlags(t *testing.T) {
	platform := types.ValidTraceEmitterPlatformValues()[0]

	t.Run("flags replace file values", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.TraceEmitter = types.TraceEmitterConfig{
			Type:            "otel",
			Platform:        "from-file",
			PlatformOptions: map[string]string{"stale": "from-file"},
		}
		must.NoError(cmd.Flags().Set("otel-platform", platform))
		for _, entry := range []string{"region=us-east-1", "logGroup=/a=b"} {
			must.NoError(cmd.Flags().Set("otel-platform-option", entry))
		}
		must.NoError(applyOverrides(cmd, cfg, nil))

		must.Equal(platform, cfg.TraceEmitter.Platform)
		must.Equal(map[string]string{"region": "us-east-1", "logGroup": "/a=b"}, cfg.TraceEmitter.PlatformOptions)
	})

	t.Run("file values survive unset flags", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.TraceEmitter = types.TraceEmitterConfig{
			Type:            "otel",
			Platform:        platform,
			PlatformOptions: map[string]string{"project": "agents"},
		}

		must.NoError(applyOverrides(cmd, cfg, nil))

		must.Equal(platform, cfg.TraceEmitter.Platform)
		must.Equal("agents", cfg.TraceEmitter.PlatformOptions["project"])
	})

	t.Run("malformed option rejected", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		must.NoError(cmd.Flags().Set("otel-platform-option", "no-equals-sign"))

		err := applyOverrides(cmd, cfg, nil)
		must.ErrorContains(err, "--otel-platform-option")
	})
}

// TestBuildFlagOnlyRunConfig_OTelPlatformOptions pins that the flag-only
// builder parses --otel-platform-option entries onto the otel emitter.
func TestBuildFlagOnlyRunConfig_OTelPlatformOptions(t *testing.T) {
	must := require.New(t)
	platform := types.ValidTraceEmitterPlatformValues()[0]
	cmd := newTestHarnessCommand()
	for k, v := range map[string]string{
		"mode": "execution", "prompt": "test", "provider": "anthropic",
		"api-key-ref": "secret://ANTHROPIC_API_KEY", "model": "claude-sonnet-4-6",
		"transport": "stdio", "log-level": "info", "trace-emitter": "otel",
		"otel-protocol": "http/protobuf", "otel-platform": platform,
		"otel-platform-option": "project=agents",
	} {
		must.NoError(cmd.Flags().Set(k, v), "set %s", k)
	}

	cfg, err := buildFlagOnlyRunConfig(cmd, nil)
	must.NoError(err)
	must.Equal(platform, cfg.TraceEmitter.Platform)
	must.Equal("agents", cfg.TraceEmitter.PlatformOptions["project"])

	malformed := newTestHarnessCommand()
	must.NoError(malformed.Flags().Set("otel-platform-option", "no-equals-sign"))
	_, err = buildFlagOnlyRunConfig(malformed, nil)
	must.ErrorContains(err, "--otel-platform-option")
}

// TestApplyOverrides_OTelHeaderFilePreserved guards the precedence rule:
// when --otel-header is not passed, headers loaded from --config survive
// applyOverrides intact.
func TestApplyOverrides_OTelHeaderFilePreserved(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.TraceEmitter = types.TraceEmitterConfig{
		Type:     "otel",
		Protocol: "http/protobuf",
		Headers:  map[string]string{"Authorization": "secret://GRAFANA_CLOUD_AUTH"},
	}

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("secret://GRAFANA_CLOUD_AUTH", cfg.TraceEmitter.Headers["Authorization"])
}

// TestApplyOverrides_OTelCaptureContentAndMetricsEndpoint pins the
// override wiring for the two scalar otel flags: an explicitly-set flag
// clobbers the file value, and the unset default does not.
func TestApplyOverrides_OTelCaptureContentAndMetricsEndpoint(t *testing.T) {
	t.Run("explicit flags override", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()

		must.NoError(cmd.Flags().Set("otel-capture-content", "true"))
		must.NoError(cmd.Flags().Set("otel-metrics-endpoint", "https://metrics.example/otlp"))
		must.NoError(cmd.Flags().Set("otel-metrics-temporality", "delta"))
		must.NoError(cmd.Flags().Set("otel-capture-content-max-bytes", "2048"))
		must.NoError(applyOverrides(cmd, cfg, nil))

		must.Equal("delta", cfg.TraceEmitter.MetricsTemporality)
		must.Equal(2048, cfg.TraceEmitter.CaptureContentMaxBytes)
		must.True(cfg.TraceEmitter.CaptureContent)
		must.Equal("https://metrics.example/otlp", cfg.TraceEmitter.MetricsEndpoint)
	})

	t.Run("file values preserved when flags unset", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.TraceEmitter = types.TraceEmitterConfig{
			Type:                   "otel",
			CaptureContent:         true,
			MetricsEndpoint:        "https://metrics.from-file/otlp",
			MetricsTemporality:     "cumulative",
			CaptureContentMaxBytes: 4096,
		}

		must.NoError(applyOverrides(cmd, cfg, nil))

		must.Equal(4096, cfg.TraceEmitter.CaptureContentMaxBytes)
		must.Equal("cumulative", cfg.TraceEmitter.MetricsTemporality)
		must.True(cfg.TraceEmitter.CaptureContent)
		must.Equal("https://metrics.from-file/otlp", cfg.TraceEmitter.MetricsEndpoint)
	})
}

// TestBuildHarnessRunConfig_OTelHeadersPropagate verifies the flag-only
// build path: headers, metrics endpoint, and the capture toggle land on
// TraceEmitter when the emitter type is otel, and are scoped away from
// the jsonl emitter (matching how OTelEndpoint is already scoped).
func TestBuildHarnessRunConfig_OTelHeadersPropagate(t *testing.T) {
	must := require.New(t)
	base := harnessCLIOptions{
		RunID:                      "test-run",
		Mode:                       "execution",
		Prompt:                     "test",
		ProviderType:               "anthropic",
		APIKeyRef:                  "secret://ANTHROPIC_API_KEY",
		Model:                      "claude-sonnet-4-6",
		MaxTurns:                   20,
		Timeout:                    600,
		TransportType:              "stdio",
		LogLevel:                   "info",
		TraceEmitterType:           "otel",
		OTelEndpoint:               "https://gw.example/otlp",
		OTelProtocol:               "http/protobuf",
		OTelHeaders:                map[string]string{"Authorization": "secret://LANGFUSE_AUTH"},
		OTelMetricsEndpoint:        "https://gw.example/otlp",
		OTelMetricsTemporality:     "delta",
		OTelCaptureContent:         true,
		OTelCaptureContentMaxBytes: 2048,
	}

	cfg, err := buildHarnessRunConfig(base)
	must.NoError(err)
	must.Equal("secret://LANGFUSE_AUTH", cfg.TraceEmitter.Headers["Authorization"])
	must.Equal("https://gw.example/otlp", cfg.TraceEmitter.MetricsEndpoint)
	must.True(cfg.TraceEmitter.CaptureContent)
	must.Equal("delta", cfg.TraceEmitter.MetricsTemporality)
	must.Equal(2048, cfg.TraceEmitter.CaptureContentMaxBytes)

	t.Run("jsonl emitter drops otel-only fields", func(t *testing.T) {
		must := require.New(t)
		jsonlOpts := base
		jsonlOpts.TraceEmitterType = "jsonl"
		cfg, err := buildHarnessRunConfig(jsonlOpts)
		must.NoError(err)

		must.Empty(cfg.TraceEmitter.Headers)
		must.False(cfg.TraceEmitter.CaptureContent)
		must.Empty(cfg.TraceEmitter.MetricsEndpoint)
		must.Empty(cfg.TraceEmitter.MetricsTemporality)
		must.Zero(cfg.TraceEmitter.CaptureContentMaxBytes)
	})
}

// TestApplyOverrides_SessionNameExplicit verifies that --name is wired
// through applyOverrides: when set on the command line, the flag value
// must overwrite the file's SessionName.
func TestApplyOverrides_SessionNameExplicit(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.SessionName = "from-file"

	must.NoError(cmd.Flags().Set("name", "from-flag"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("from-flag", cfg.SessionName)
}

// TestApplyOverrides_SessionNameFilePreserved guards the precedence rule
// for --name: when the user does NOT pass the flag, a SessionName loaded
// from --config must survive applyOverrides intact. This is the central
// "default flag does not clobber file value" invariant for the new flag.
func TestApplyOverrides_SessionNameFilePreserved(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.SessionName = "from-file"

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("from-file", cfg.SessionName)
}

// TestBuildHarnessRunConfig_SessionNamePropagates verifies the flag-only
// build path: a SessionName provided in harnessCLIOptions must end up on
// the constructed RunConfig.
func TestBuildHarnessRunConfig_SessionNamePropagates(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	must.NoError(err)

	must.Equal("nightly-eval", cfg.SessionName)
	must.NoError(types.ValidateRunConfig(cfg))
}

// TestBuildHarnessRunConfig_ObservabilityPropagates pins that the
// --deployment-environment / --service-namespace flags propagate into
// RunConfig.Observability without further translation.
func TestBuildHarnessRunConfig_ObservabilityPropagates(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:                 "test-run",
		Mode:                  "execution",
		Prompt:                "test",
		ProviderType:          "anthropic",
		APIKeyRef:             "secret://ANTHROPIC_API_KEY",
		Model:                 "claude-sonnet-4-6",
		MaxTurns:              20,
		Timeout:               600,
		TransportType:         "stdio",
		LogLevel:              "info",
		ServiceName:           "triage-agent",
		DeploymentEnvironment: "production",
		ServiceNamespace:      "stirrup-eval",
	})
	must.NoError(err)

	must.Equal("triage-agent", cfg.Observability.ServiceName)
	must.Equal("production", cfg.Observability.Environment)
	must.Equal("stirrup-eval", cfg.Observability.ServiceNamespace)
	must.NoError(types.ValidateRunConfig(cfg))
}

// TestApplyOverrides_DeploymentEnvironmentFlag pins the file -> flag
// override chain for --deployment-environment. An explicit flag must
// clobber the file's value; an unset flag must leave the file value alone.
func TestApplyOverrides_DeploymentEnvironmentFlag(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Observability = types.ObservabilityConfig{
		Environment:      "from-file-env",
		ServiceNamespace: "from-file-ns",
	}

	must.NoError(cmd.Flags().Set("deployment-environment", "from-flag-env"))

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("from-flag-env", cfg.Observability.Environment)
	must.Equal("from-file-ns", cfg.Observability.ServiceNamespace)
}

// TestApplyOverrides_ServiceNameAndNamespaceFlags pins that --service-name
// and --service-namespace override the file's observability values, and
// that an unset --deployment-environment keeps the file value.
func TestApplyOverrides_ServiceNameAndNamespaceFlags(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Observability = types.ObservabilityConfig{
		ServiceName:      "from-file-name",
		Environment:      "from-file-env",
		ServiceNamespace: "from-file-ns",
	}

	must.NoError(cmd.Flags().Set("service-namespace", "from-flag-ns"))
	must.NoError(cmd.Flags().Set("service-name", "from-flag-name"))

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("from-flag-ns", cfg.Observability.ServiceNamespace)
	must.Equal("from-flag-name", cfg.Observability.ServiceName)
	must.Equal("from-file-env", cfg.Observability.Environment)
}

// TestBuildHarnessRunConfig_LogExportPropagates pins that --log-export otlp
// (with the default endpoint) flows into RunConfig.Observability.LogsExport
// and validates. The empty Endpoint is intentional: the factory falls back
// to the trace emitter's endpoint when it is unset.
func TestBuildHarnessRunConfig_LogExportPropagates(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
		LogExport:     "otlp",
	})
	must.NoError(err)

	must.Equal("otlp", cfg.Observability.LogsExport.Type)
	must.Empty(cfg.Observability.LogsExport.Endpoint, "factory falls back to the trace emitter endpoint")
	must.NoError(types.ValidateRunConfig(cfg))
}

// TestBuildHarnessRunConfig_LogExportNoneStaysStderrOnly pins that the
// default --log-export none does not materialise an "otlp" type — the
// stderr-only behaviour is the zero value, and the factory only opts in on
// Type=="otlp".
func TestBuildHarnessRunConfig_LogExportNoneStaysStderrOnly(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
		LogExport:     "none",
	})
	must.NoError(err)

	must.Empty(cfg.Observability.LogsExport.Type)
}

// TestApplyOverrides_LogExportFlag pins the file -> flag override chain for
// --log-export: an explicit "otlp" must clobber the file value, and a
// explicit "none" must clear a file's "otlp" back to stderr-only.
func TestApplyOverrides_LogExportFlag(t *testing.T) {
	t.Run("otlp overrides file", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		must.NoError(cmd.Flags().Set("log-export", "otlp"))

		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal("otlp", cfg.Observability.LogsExport.Type, "explicit flag should win")
	})

	t.Run("none clears file otlp", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.Observability.LogsExport = types.LogsExportConfig{Type: "otlp", Endpoint: "localhost:4317"}
		must.NoError(cmd.Flags().Set("log-export", "none"))

		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Empty(cfg.Observability.LogsExport.Type, "--log-export none should clear to stderr-only")
	})
}

// TestApplyOverrides_LogExportEndpointEnvVar pins that
// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, when set, pins the log endpoint on the
// override path regardless of the file value — the per-signal env override
// the OTel SDK honours.
func TestApplyOverrides_LogExportEndpointEnvVar(t *testing.T) {
	must := require.New(t)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "logs-collector:4317")
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Observability.LogsExport = types.LogsExportConfig{Type: "otlp", Endpoint: "from-file:4317"}

	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal("logs-collector:4317", cfg.Observability.LogsExport.Endpoint, "env var should pin the endpoint")
}

// TestBuildHarnessRunConfig_LogExportEndpointFromEnv pins that the builder
// path reads OTEL_EXPORTER_OTLP_LOGS_ENDPOINT into LogsExport.Endpoint so a
// flag-only run (no --config) can still pin a distinct logs endpoint.
func TestBuildHarnessRunConfig_LogExportEndpointFromEnv(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "logs-collector:4317")
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("log-export", "otlp"))
	// Minimum flags so the builder produces a valid config.
	for k, v := range map[string]string{
		"mode": "execution", "prompt": "test", "provider": "anthropic",
		"api-key-ref": "secret://ANTHROPIC_API_KEY", "model": "claude-sonnet-4-6",
		"transport": "stdio", "log-level": "info",
	} {
		must.NoError(cmd.Flags().Set(k, v), "set %s", k)
	}

	cfg, err := buildFlagOnlyRunConfig(cmd, nil)
	must.NoError(err)
	check.Equal("otlp", cfg.Observability.LogsExport.Type)
	check.Equal("logs-collector:4317", cfg.Observability.LogsExport.Endpoint)
}

// TestApplyOverrides_PositionalPromptFillsFileGap covers the precedence
// edge case where the file omits a prompt and the user passes one as a
// positional argument (no --prompt flag). The positional should fill the
// gap rather than triggering the "prompt required" error.
func TestApplyOverrides_PositionalPromptFillsFileGap(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Prompt = "" // simulate file with no prompt

	must.NoError(applyOverrides(cmd, cfg, []string{"positional prompt"}))
	must.Equal("positional prompt", cfg.Prompt, "positional prompt should fill the gap")
}

// TestApplyOverrides_FilePromptBeatsPositional verifies that a positional
// arg does NOT clobber a prompt set in the config file. The positional
// is a fallback, not an override (use --prompt for that).
func TestApplyOverrides_FilePromptBeatsPositional(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig() // Prompt = "prompt-from-file"

	must.NoError(applyOverrides(cmd, cfg, []string{"positional"}))
	must.Equal("prompt-from-file", cfg.Prompt, "file prompt should win over positional")
}

// TestApplyOverrides_ExplicitFlagBeatsPositional pins the precedence:
// --prompt > positional > file.
func TestApplyOverrides_ExplicitFlagBeatsPositional(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Prompt = "" // file has no prompt
	must.NoError(cmd.Flags().Set("prompt", "from-flag"))

	must.NoError(applyOverrides(cmd, cfg, []string{"positional"}))
	must.Equal("from-flag", cfg.Prompt, "explicit --prompt should win")
}

// repoRootForTests returns the absolute repo root by walking up from this
// test file's path via runtime.Caller(0), independent of the working
// directory.
func repoRootForTests(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	_, thisFile, _, ok := runtime.Caller(0)
	must.True(ok, "runtime.Caller(0) failed")
	// thisFile is .../harness/cmd/stirrup/cmd/harness_test.go; walk up four
	// levels to reach the repo root.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
}

// loadExampleRunConfig loads a shipped fixture from examples/runconfig and
// requires that it validates.
func loadExampleRunConfig(t *testing.T, name string) *types.RunConfig {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(repoRootForTests(t), "examples", "runconfig", name)
	must.FileExists(path)
	cfg, err := loadRunConfigFile(path)
	must.NoError(err, "loadRunConfigFile %q", path)
	must.NoError(types.ValidateRunConfig(cfg), "%s fails ValidateRunConfig", name)
	return cfg
}

// TestExampleRunConfig_LoadsAndValidates_Full pins that examples/runconfig/full.json
// round-trips through loadRunConfigFile and passes ValidateRunConfig
// unmodified.
func TestExampleRunConfig_LoadsAndValidates_Full(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "full.json")

	check.Equal("multi", cfg.EditStrategy.Type, "example should demonstrate multi edit strategy")
	check.Equal("container", cfg.Executor.Type, "example should demonstrate container executor")
	check.Equal("otel", cfg.TraceEmitter.Type, "example should demonstrate otel trace emitter")
	check.Equal("dynamic", cfg.ModelRouter.Type, "example should demonstrate dynamic model router")
	check.NotEmpty(cfg.ModelRouter.CheapModel, "example should set modelRouter.cheapModel")
	must.NotNil(cfg.Executor.Resources, "example should set executor.resources")
	check.NotZero(cfg.Executor.Resources.CPUs, "example should set executor.resources.cpus")
	must.Len(cfg.Tools.MCPServers, 1, "example should configure exactly one named MCP server")
	check.NotEmpty(cfg.Tools.MCPServers[0].Name, "example MCP server should be named")
}

// TestExampleRunConfig_LoadsAndValidates_NoneMCPOnly pins the shipped
// none-mcp-only.json fixture: the MCP-only, no-execution-surface use case
// executor.type="none" exists for, pairing a read-only mode with an MCP
// server and only the capability-ungated web_fetch built-in tool.
func TestExampleRunConfig_LoadsAndValidates_NoneMCPOnly(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg := loadExampleRunConfig(t, "none-mcp-only.json")

	check.Equal("none", cfg.Executor.Type, "example should demonstrate the none executor")
	check.True(types.IsReadOnlyMode(cfg.Mode), "example should demonstrate a read-only mode")
	check.Equal([]string{"web_fetch"}, cfg.Tools.BuiltIn, "example should enable only web_fetch")
	must.Len(cfg.Tools.MCPServers, 1, "example should configure exactly one named MCP server")
	check.NotEmpty(cfg.Tools.MCPServers[0].Name, "example MCP server should be named")
}

// TestExampleRunConfig_LoadsAndValidates_AzureOpenAI pins the shipped Azure
// OpenAI fixture: it must round-trip and validate, demonstrating
// apiKeyHeader, queryParams, and the Azure-shaped baseUrl.
func TestExampleRunConfig_LoadsAndValidates_AzureOpenAI(t *testing.T) {
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "azure-openai.json")

	check.Equal("openai-responses", cfg.Provider.Type)
	check.Equal("api-key", cfg.Provider.APIKeyHeader)
	check.Equal("preview", cfg.Provider.QueryParams["api-version"])
	check.Contains(cfg.Provider.BaseURL, "openai.azure.com", "Provider.BaseURL should target Azure")
}

// TestExampleRunConfig_LoadsAndValidates_VertexGemini pins the shipped Vertex AI
// fixture: it must round-trip and validate, and its permissionPolicy /
// built-in tool combination must be execution-mode-consistent — a
// deny-side-effects policy paired with a side-effecting tool would pass
// validation but silently block the example at runtime.
func TestExampleRunConfig_LoadsAndValidates_VertexGemini(t *testing.T) {
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "vertex-gemini.json")

	check.Equal("gemini", cfg.Provider.Type)
	check.NotEmpty(cfg.Provider.GCPProject, "Provider must set gcpProject")
	check.NotEmpty(cfg.Provider.GCPLocation, "Provider must set gcpLocation")

	if cfg.Mode == "execution" {
		hasSideEffectTool := false
		for _, name := range cfg.Tools.BuiltIn {
			if name == "run_command" || name == "edit_file" || name == "write_file" {
				hasSideEffectTool = true
				break
			}
		}
		check.False(hasSideEffectTool && cfg.PermissionPolicy.Type == "deny-side-effects",
			"execution-mode example with side-effecting tools must not use deny-side-effects (got %q + %v)",
			cfg.PermissionPolicy.Type, cfg.Tools.BuiltIn)
	}
}

// TestExampleRunConfig_LoadsAndValidates_AzureOpenAIWIFAKS pins the shipped AKS
// Azure-WIF fixture: it must round-trip and validate, demonstrating the
// azure-workload-identity credential type with a file-projected token
// source.
func TestExampleRunConfig_LoadsAndValidates_AzureOpenAIWIFAKS(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "azure-openai-wif-aks.json")

	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Provider.Credential block")
	check.Equal("azure-workload-identity", cred.Type)
	must.NotNil(cred.TokenSource, "expected a token source")
	check.Equal("file", cred.TokenSource.Type)
}

// TestExampleRunConfig_LoadsAndValidates_AzureOpenAIWIFGitHubActions pins the
// shipped GitHub-Actions Azure-WIF fixture: same shape as the AKS test
// above but with a github-actions-oidc token source.
func TestExampleRunConfig_LoadsAndValidates_AzureOpenAIWIFGitHubActions(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "azure-openai-wif-github-actions.json")

	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Provider.Credential block")
	check.Equal("azure-workload-identity", cred.Type)
	must.NotNil(cred.TokenSource, "expected a token source")
	check.Equal("github-actions-oidc", cred.TokenSource.Type)
	check.Equal("api://AzureADTokenExchange", cred.TokenSource.Audience)
}

// TestExampleRunConfig_LoadsAndValidates_AzureOpenAIWIFSmoke pins the pre-wired
// smoke-test fixture consumed by .github/workflows/smoke-azure-openai.yml,
// which hardcodes the stirrup test tenant's IDs and the AI Foundry
// (cognitiveservices.azure.com) host.
func TestExampleRunConfig_LoadsAndValidates_AzureOpenAIWIFSmoke(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "azure-openai-wif-smoke.json")

	check.Equal("openai-responses", cfg.Provider.Type)
	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Provider.Credential block")
	check.Equal("azure-workload-identity", cred.Type)
	must.NotNil(cred.TokenSource, "expected a token source")
	check.Equal("github-actions-oidc", cred.TokenSource.Type)
	check.Equal("api://AzureADTokenExchange", cred.TokenSource.Audience)
	check.Equal("https://stirrup-eval-resource.cognitiveservices.azure.com/openai/v1", cfg.Provider.BaseURL,
		"smoke fixture should target the stirrup test tenant AI Foundry host")
	check.Equal("gpt-5.4-nano", cfg.ModelRouter.Model)
}

// TestExampleRunConfig_LoadsAndValidates_OpenAIWIFGitHubActions pins the shipped
// OpenAI-WIF GitHub-Actions fixture, demonstrating the openai-wif
// credential type with a github-actions-oidc token source carrying the
// OpenAI API audience.
func TestExampleRunConfig_LoadsAndValidates_OpenAIWIFGitHubActions(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg := loadExampleRunConfig(t, "openai-wif-github-actions.json")

	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Provider.Credential block")
	check.Equal("openai-wif", cred.Type)
	must.NotNil(cred.TokenSource, "expected a token source")
	check.Equal("github-actions-oidc", cred.TokenSource.Type)
	check.Equal("https://api.openai.com/v1", cred.TokenSource.Audience)
}

// TestExampleRunConfig_LoadsAndValidates_OpenAIWIFEKSIRSA pins the shipped
// OpenAI-WIF EKS/IRSA fixture: same shape as the GitHub-Actions test above
// but on an openai-compatible provider with an aws-irsa token source.
func TestExampleRunConfig_LoadsAndValidates_OpenAIWIFEKSIRSA(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "openai-wif-eks-irsa.json")

	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Provider.Credential block")
	check.Equal("openai-wif", cred.Type)
	must.NotNil(cred.TokenSource, "expected a token source")
	check.Equal("aws-irsa", cred.TokenSource.Type)
}

// TestExampleRunConfig_LoadsAndValidates_BedrockWIFSmoke pins the pre-wired
// smoke-test fixture consumed by .github/workflows/smoke-bedrock.yml,
// which hardcodes the stirrup sandbox AWS account's role ARN and the
// us-west-2 source region for the Haiku 4.5 cross-region inference
// profile.
func TestExampleRunConfig_LoadsAndValidates_BedrockWIFSmoke(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg := loadExampleRunConfig(t, "bedrock-wif-smoke.json")

	check.Equal("bedrock", cfg.Provider.Type)
	check.Equal("us-west-2", cfg.Provider.Region)
	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Provider.Credential block")
	check.Equal("web-identity", cred.Type)
	check.Equal("arn:aws:iam::786874932855:role/stirrup-smoke-bedrock", cred.RoleARN, "stirrup sandbox role ARN")
	must.NotNil(cred.TokenSource, "expected a token source")
	check.Equal("github-actions-oidc", cred.TokenSource.Type)
	check.Equal("sts.amazonaws.com", cred.TokenSource.Audience)
	check.Equal("us.anthropic.claude-haiku-4-5-20251001-v1:0", cfg.ModelRouter.Model,
		"Haiku 4.5 cross-region inference profile")
}

// TestExampleRunConfig_LoadsAndValidates_VertexGeminiWIFSmoke pins the pre-wired
// smoke-test fixture consumed by .github/workflows/smoke-vertex-gemini.yml,
// which hardcodes the rubynerd-net project, the shared stirrup-gha WIF
// pool's double-slash audience, and the stirrup-testing SA. The
// Credential.Audience and TokenSource.Audience must match, since the GHA
// OIDC `aud` claim must equal the WIF provider's expected audience.
func TestExampleRunConfig_LoadsAndValidates_VertexGeminiWIFSmoke(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := loadExampleRunConfig(t, "vertex-gemini-wif-smoke.json")

	const wantAudience = "//iam.googleapis.com/projects/163317929648/locations/global/workloadIdentityPools/stirrup-gha/providers/stirrup-gha-provider"
	check.Equal("gemini", cfg.Provider.Type)
	check.Equal("rubynerd-net", cfg.Provider.GCPProject)
	check.Equal("global", cfg.Provider.GCPLocation)
	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Provider.Credential block")
	check.Equal("gcp-workload-identity-federation", cred.Type)
	check.Equal(wantAudience, cred.Audience, "Credential.Audience should be the stirrup-gha provider audience")
	check.Equal("stirrup-testing@rubynerd-net.iam.gserviceaccount.com", cred.ServiceAccount)
	must.NotNil(cred.TokenSource, "expected a token source")
	check.Equal("github-actions-oidc", cred.TokenSource.Type)
	check.Equal(wantAudience, cred.TokenSource.Audience,
		"GHA aud claim must match the WIF provider audience, so it must equal Credential.Audience")
	check.Equal("gemini-2.5-flash-lite", cfg.ModelRouter.Model)
}

// TestExampleRunConfig_LoadsAndValidates_CloudRunVertexGemini pins the shipped
// Cloud Run fixture: it must round-trip and validate, exercising
// resultSink.type=stdout-json, traceEmitter.type=gcs, and
// executor.workspaceExportTo on a gs:// URI.
func TestExampleRunConfig_LoadsAndValidates_CloudRunVertexGemini(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg := loadExampleRunConfig(t, "cloud-run-vertex-gemini.json")

	check.Equal("gemini", cfg.Provider.Type)
	must.NotNil(cfg.Provider.Credential, "expected Provider.Credential block")
	check.Equal("gcp-workload-identity", cfg.Provider.Credential.Type)
	must.NotNil(cfg.ResultSink, "expected ResultSink block")
	check.Equal("stdout-json", cfg.ResultSink.Type)
	check.Equal("gcs", cfg.TraceEmitter.Type)
	check.NotEmpty(cfg.TraceEmitter.Bucket, "TraceEmitter.Bucket must be set when TraceEmitter.Type is gcs")
	check.NotEmpty(cfg.Executor.WorkspaceExportTo, "Executor.WorkspaceExportTo must be set in the Cloud Run fixture")
}

// TestBuildHarnessRunConfig_SafetyRingFlags verifies that each safety-ring
// flag propagates to its matching RunConfig field.
func TestBuildHarnessRunConfig_SafetyRingFlags(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	must.NoError(err)

	check.Equal("runsc", cfg.Executor.Runtime)
	check.Equal("/tmp/policy.cedar", cfg.PermissionPolicy.PolicyFile)
	// The convenience shortcut auto-sets type=policy-engine when the
	// caller didn't pick a type elsewhere.
	check.Equal("policy-engine", cfg.PermissionPolicy.Type)
	must.NotNil(cfg.CodeScanner, "expected CodeScanner block")
	check.Equal("patterns", cfg.CodeScanner.Type)
}

// TestApplyOverrides_SafetyRingFlagsOverride verifies the override path:
// safety-ring flags set on the command line clobber file-provided values.
func TestApplyOverrides_SafetyRingFlagsOverride(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.Runtime = "runc"
	cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	cfg.CodeScanner = &types.CodeScannerConfig{Type: "none"}

	setFlag := func(name, value string) {
		must.NoError(cmd.Flags().Set(name, value), "set %s", name)
	}
	setFlag("container-runtime", "runsc")
	setFlag("permission-policy-file", "/tmp/p.cedar")
	setFlag("code-scanner", "patterns")

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("runsc", cfg.Executor.Runtime, "container-runtime should override the file value")
	check.Equal("/tmp/p.cedar", cfg.PermissionPolicy.PolicyFile, "permission-policy-file should override the file value")
	// File set type=deny-side-effects so --permission-policy-file
	// should NOT switch the type — only the path.
	check.Equal("deny-side-effects", cfg.PermissionPolicy.Type, "file type should survive when the file set it")
	must.NotNil(cfg.CodeScanner, "expected CodeScanner block")
	check.Equal("patterns", cfg.CodeScanner.Type, "code-scanner should override the file value")
}

// TestApplyOverrides_PermissionPolicyFileImpliesPolicyEngine verifies
// the convenience shortcut: when the file leaves PermissionPolicy.Type
// unset and the user passes --permission-policy-file, type is bumped
// to "policy-engine" so the single flag is enough to use the new
// policy implementation.
func TestApplyOverrides_PermissionPolicyFileImpliesPolicyEngine(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.PermissionPolicy = types.PermissionPolicyConfig{} // file did not set type

	must.NoError(cmd.Flags().Set("permission-policy-file", "/tmp/p.cedar"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("policy-engine", cfg.PermissionPolicy.Type, "type should be implied when the file omits it")
	check.Equal("/tmp/p.cedar", cfg.PermissionPolicy.PolicyFile)
}

// TestApplyOverrides_DefaultSafetyRingFlagsDoNotOverride pins the
// precedence rule for the new flags: a flag left at its default
// (empty string) MUST NOT clobber a file-provided value.
func TestApplyOverrides_DefaultSafetyRingFlagsDoNotOverride(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.Runtime = "kata"
	cfg.PermissionPolicy = types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/file/policy.cedar",
	}
	cfg.CodeScanner = &types.CodeScannerConfig{Type: "semgrep"}

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("kata", cfg.Executor.Runtime, "Runtime: file value should survive")
	check.Equal("/file/policy.cedar", cfg.PermissionPolicy.PolicyFile, "PolicyFile: file value should survive")
	must.NotNil(cfg.CodeScanner, "CodeScanner: file value should survive")
	check.Equal("semgrep", cfg.CodeScanner.Type, "CodeScanner: file value should survive")
}

// TestRunHarness_ConfigValidationFailurePropagates writes a config that
// fails ValidateRunConfig (read-only mode + write tool) and asserts the
// CLI surfaces the error clearly.
func TestRunHarness_ConfigValidationFailurePropagates(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.NoError(os.WriteFile(path, data, 0o600))

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", path))

	err = runHarness(cmd, nil)
	must.ErrorContains(err, "invalid config")
}

// TestLoadRunConfigFile_EmptyFile pins the error path for an empty (zero-
// byte) file. encoding/json would otherwise return io.EOF, which is
// unhelpful out of context — we want a message that names the path so
// the user can find the mistake. The prefix is "reading" (not
// "parsing"): an empty file never reached the decoder, so the wording
// matches its I/O exit class (3).
func TestLoadRunConfigFile_EmptyFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	must.NoError(os.WriteFile(path, nil, 0o600))
	_, err := loadRunConfigFile(path)
	must.ErrorContains(err, "reading config file")
	must.ErrorContains(err, "empty")
}

// TestLoadRunConfigFile_DirectoryArg verifies that pointing --config at
// a directory yields a clear error rather than a confusing read failure
// further down the stack.
func TestLoadRunConfigFile_DirectoryArg(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	_, err := loadRunConfigFile(dir)
	must.ErrorContains(err, "is a directory")
}

// TestLoadRunConfigFile_OversizeRejected ensures the size guard kicks in
// before the file is loaded into memory. The cap is sized for genuine
// configs (a few KB); a multi-MB file is almost always wrong.
func TestLoadRunConfigFile_OversizeRejected(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	// 2 MiB of zero bytes is far past the 1 MiB cap.
	must.NoError(os.WriteFile(path, make([]byte, 2*1024*1024), 0o600))
	_, err := loadRunConfigFile(path)
	must.ErrorContains(err, "byte cap")
}

// TestApplyOverrides_TraceCoercesEmitterToJSONL verifies that passing
// --trace on an otel-emitter config rewrites the emitter type to jsonl,
// since FilePath is meaningless for the otel emitter. This is the
// "user's intent stands" behaviour: --trace is a JSONL flag, and the
// user reaching for it is reaching for JSONL output.
func TestApplyOverrides_TraceCoercesEmitterToJSONL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.TraceEmitter = types.TraceEmitterConfig{Type: "otel", Endpoint: "localhost:4317"}

	must.NoError(cmd.Flags().Set("trace", "/tmp/out.jsonl"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("jsonl", cfg.TraceEmitter.Type, "emitter type should be coerced to jsonl when --trace is set")
	check.Equal("/tmp/out.jsonl", cfg.TraceEmitter.FilePath, "trace path should be set")
}

// TestApplyOverrides_TraceRespectsExplicitEmitter verifies the inverse:
// when both --trace and --trace-emitter are explicitly set, the user's
// explicit emitter choice wins (even if the FilePath becomes ignored).
func TestApplyOverrides_TraceRespectsExplicitEmitter(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	must.NoError(cmd.Flags().Set("trace", "/tmp/out.jsonl"))
	must.NoError(cmd.Flags().Set("trace-emitter", "otel"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("otel", cfg.TraceEmitter.Type, "explicit --trace-emitter=otel should win over coercion")
}

// TestApplyOverrides_FollowupGraceZeroClears verifies that explicitly
// passing --followup-grace=0 clears a non-nil FollowUpGrace from the
// file. This is the "I want to disable follow-ups" intent that the
// `g > 0` else-branch at applyOverrides supports.
func TestApplyOverrides_FollowupGraceZeroClears(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	g := 120
	cfg.FollowUpGrace = &g

	must.NoError(cmd.Flags().Set("followup-grace", "0"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Nil(cfg.FollowUpGrace, "explicit --followup-grace=0 should clear FollowUpGrace")
}

// TestApplyOverrides_FollowupHistoryChangedGated pins that a file's
// followUpHistory survives an invocation that does not pass
// --followup-history, and that an explicit flag replaces it.
func TestApplyOverrides_FollowupHistoryChangedGated(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.FollowUpHistory = types.FollowUpHistoryContinue
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal(types.FollowUpHistoryContinue, cfg.FollowUpHistory, "unset --followup-history must keep the file value")

	must.NoError(cmd.Flags().Set("followup-history", types.FollowUpHistoryReset))
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal(types.FollowUpHistoryReset, cfg.FollowUpHistory)
}

// TestBuildFlagOnlyRunConfig_FollowupHistory pins the flag-only path for
// --followup-history onto the top-level RunConfig field.
func TestBuildFlagOnlyRunConfig_FollowupHistory(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	for k, v := range map[string]string{
		"mode": "execution", "prompt": "test", "provider": "anthropic",
		"api-key-ref": "secret://ANTHROPIC_API_KEY", "model": "claude-sonnet-4-6",
		"transport": "stdio", "log-level": "info",
		"followup-grace": "30", "followup-history": types.FollowUpHistoryContinue,
	} {
		must.NoError(cmd.Flags().Set(k, v), "set %s", k)
	}
	cfg, err := buildFlagOnlyRunConfig(cmd, nil)
	must.NoError(err)
	must.Equal(types.FollowUpHistoryContinue, cfg.FollowUpHistory)
}

// TestBuildFlagOnlyRunConfig_ReasoningSummary pins the flag-only path for
// --reasoning-summary onto the top-level RunConfig field.
func TestBuildFlagOnlyRunConfig_ReasoningSummary(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	for k, v := range map[string]string{
		"mode": "execution", "prompt": "test", "provider": "anthropic",
		"api-key-ref": "secret://ANTHROPIC_API_KEY", "model": "claude-sonnet-4-6",
		"transport": "stdio", "log-level": "info",
		"reasoning-summary": types.ReasoningSummaryOff,
	} {
		must.NoError(cmd.Flags().Set(k, v), "set %s", k)
	}
	cfg, err := buildFlagOnlyRunConfig(cmd, nil)
	must.NoError(err)
	must.Equal(types.ReasoningSummaryOff, cfg.ReasoningSummary)
}

// TestApplyOverrides_PromptModelChangedGated pins that --prompt-model
// follows the Changed() discipline: a config file's
// promptBuilder.promptModel survives when the flag is unset, and an
// explicit flag overrides it.
func TestApplyOverrides_PromptModelChangedGated(t *testing.T) {
	t.Run("unset leaves file value alone", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.PromptBuilder.PromptModel = "claude-fable-5"

		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal("claude-fable-5", cfg.PromptBuilder.PromptModel)
	})

	t.Run("explicit flag overrides file value", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.PromptBuilder.PromptModel = "claude-fable-5"

		must.NoError(cmd.Flags().Set("prompt-model", "claude-fable-6"))
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal("claude-fable-6", cfg.PromptBuilder.PromptModel)
	})
}

// TestApplyOverrides_TemperatureChangedDisambiguatesZero exercises the
// unset-vs-explicit-zero distinction for --temperature: cobra cannot
// represent flag absence for a plain Float64, so the override path relies
// on flags.Changed() instead.
func TestApplyOverrides_TemperatureChangedDisambiguatesZero(t *testing.T) {
	t.Run("unset leaves file value alone", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		fileTemp := 0.5
		cfg.Temperature = &fileTemp

		must.NoError(applyOverrides(cmd, cfg, nil))
		must.NotNil(cfg.Temperature)
		must.InDelta(0.5, *cfg.Temperature, 0)
	})

	t.Run("explicit zero is greedy override", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		fileTemp := 0.5
		cfg.Temperature = &fileTemp

		must.NoError(cmd.Flags().Set("temperature", "0"))
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.NotNil(cfg.Temperature, "explicit --temperature=0 should set greedy decoding")
		must.InDelta(0.0, *cfg.Temperature, 0)
	})

	t.Run("explicit non-zero overrides file value", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		fileTemp := 0.5
		cfg.Temperature = &fileTemp

		must.NoError(cmd.Flags().Set("temperature", "1.2"))
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.NotNil(cfg.Temperature)
		must.InDelta(1.2, *cfg.Temperature, 0)
	})
}

// TestBuildHarnessRunConfig_Temperature pins the temperature propagation
// inside buildHarnessRunConfig's flag-only path, distinct from the
// --config-path coverage in the applyOverrides tests.
func TestBuildHarnessRunConfig_Temperature(t *testing.T) {
	base := func() harnessCLIOptions {
		return harnessCLIOptions{
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
		}
	}

	t.Run("nil opts.Temperature stays nil on the config", func(t *testing.T) {
		must := require.New(t)
		opts := base()
		opts.Temperature = nil
		cfg, err := buildHarnessRunConfig(opts)
		must.NoError(err)
		must.Nil(cfg.Temperature)
	})

	t.Run("explicit zero propagates as *0.0", func(t *testing.T) {
		must := require.New(t)
		opts := base()
		opts.Temperature = types.Float64Ptr(0.0)
		cfg, err := buildHarnessRunConfig(opts)
		must.NoError(err)
		must.NotNil(cfg.Temperature)
		must.InDelta(0.0, *cfg.Temperature, 0)
	})

	t.Run("non-zero value propagates", func(t *testing.T) {
		must := require.New(t)
		opts := base()
		opts.Temperature = types.Float64Ptr(0.7)
		cfg, err := buildHarnessRunConfig(opts)
		must.NoError(err)
		must.NotNil(cfg.Temperature)
		must.InDelta(0.7, *cfg.Temperature, 0)
	})

	t.Run("opts.Temperature is copied, not aliased", func(t *testing.T) {
		must := require.New(t)
		opts := base()
		opts.Temperature = types.Float64Ptr(0.4)
		cfg, err := buildHarnessRunConfig(opts)
		must.NoError(err)

		*opts.Temperature = 1.5
		must.NotNil(cfg.Temperature)
		must.InDelta(0.4, *cfg.Temperature, 0, "cfg.Temperature should be a copy snapshot")
	})
}

// TestRunHarness_ConfigPathFollowupGraceFromEnv verifies that
// STIRRUP_FOLLOWUP_GRACE populates FollowUpGrace in the --config path
// when the file omits the field, driven through BuildRunConfig's
// ResolveAll branch rather than the resolution logic reimplemented
// inline.
func TestRunHarness_ConfigPathFollowupGraceFromEnv(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.NoError(os.WriteFile(path, data, 0o600))

	t.Setenv("STIRRUP_FOLLOWUP_GRACE", "45")

	cmd := newTestHarnessCommand()
	resolved, err := BuildRunConfig(RunConfigSources{
		ConfigPath: path,
		Cmd:        cmd,
		Resolve:    ResolveAll,
	})
	must.NoError(err)
	must.NotNil(resolved.FollowUpGrace, "STIRRUP_FOLLOWUP_GRACE should fill FollowUpGrace when file omits it")
	must.Equal(45, *resolved.FollowUpGrace)
}

// TestApplyModeDefaults_FillsAfterModeOverride pins that when --config
// sets a sparse RunConfig (no policy/tools) and --mode is then
// overridden, the post-override defaulting step fills in the new mode's
// defaults.
func TestApplyModeDefaults_FillsAfterModeOverride(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	must.NoError(cmd.Flags().Set("mode", "planning"))
	must.NoError(applyOverrides(cmd, cfg, nil))
	applyModeDefaults(cfg)

	must.Equal("planning", cfg.Mode)
	check.NotEmpty(cfg.Tools.BuiltIn, "read-only mode should default Tools.BuiltIn to a non-empty list")
	must.NoError(types.ValidateRunConfig(cfg), "post-defaulted config should validate")
	check.Equal("deny-side-effects", cfg.PermissionPolicy.Type, "read-only mode should resolve to deny-side-effects")
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
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.BaseURL = "https://file-base-url.example/v1"
	cfg.Provider.APIKeyHeader = "x-stale-header"
	cfg.Provider.QueryParams = map[string]string{"api-version": "stale", "deployment-id": "stale"}

	setFlag := func(name, value string) {
		t.Helper()
		must.NoError(cmd.Flags().Set(name, value), "set %s", name)
	}
	setFlag("base-url", "https://example.openai.azure.com/openai/v1")
	setFlag("api-key-header", "api-key")
	setFlag("query-param", "api-version=preview")
	setFlag("query-param", "deployment-id=gpt4-prod")

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("https://example.openai.azure.com/openai/v1", cfg.Provider.BaseURL, "Provider.BaseURL")
	check.Equal("api-key", cfg.Provider.APIKeyHeader, "Provider.APIKeyHeader")
	check.Equal("preview", cfg.Provider.QueryParams["api-version"], "QueryParams[api-version]")
	check.Equal("gpt4-prod", cfg.Provider.QueryParams["deployment-id"], "QueryParams[deployment-id]")
}

func TestApplyOverrides_ProviderEndpointProfileFlag(t *testing.T) {
	t.Run("flag overrides file value", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.Provider.EndpointProfile = types.EndpointProfileGeneric
		must.NoError(cmd.Flags().Set("provider-endpoint-profile", "azure-openai"))

		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal(types.EndpointProfileAzureOpenAI, cfg.Provider.EndpointProfile)
	})
	t.Run("unset flag keeps file value", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.Provider.EndpointProfile = types.EndpointProfileGeneric

		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal(types.EndpointProfileGeneric, cfg.Provider.EndpointProfile)
	})
}

// TestApplyOverrides_AzureFlagsDoNotOverrideWhenUnset verifies the
// precedence rule for the new flags: a flag that the user did not pass
// MUST NOT clobber a file-provided value.
func TestApplyOverrides_AzureFlagsDoNotOverrideWhenUnset(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.BaseURL = "https://file-base-url.example/v1"
	cfg.Provider.APIKeyHeader = "api-key"
	cfg.Provider.QueryParams = map[string]string{"api-version": "preview"}

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("https://file-base-url.example/v1", cfg.Provider.BaseURL, "Provider.BaseURL: file value should survive")
	check.Equal("api-key", cfg.Provider.APIKeyHeader, "Provider.APIKeyHeader: file value should survive")
	check.Equal("preview", cfg.Provider.QueryParams["api-version"], "QueryParams: file value should survive")
}

// TestApplyOverrides_QueryParamMalformedReturnsError pins that a
// malformed --query-param entry on the --config path returns a non-nil
// error rather than warning-and-continuing, matching the flag-only
// path's hard failure for the same input.
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
			must := require.New(t)
			cmd := newTestHarnessCommand()
			cfg := baseFileConfig()
			must.NoError(cmd.Flags().Set("query-param", tc.entry))

			err := applyOverrides(cmd, cfg, nil)
			must.ErrorContains(err, "--query-param")
		})
	}
}

// TestParseQueryParam_ValidAndInvalid pins the syntactic split rule used
// by the --query-param flag parser. Empty keys and missing "=" are rejected.
// Charset/length validation lives in ValidateRunConfig — this helper only
// owns the syntax.
func TestParseQueryParam_ValidAndInvalid(t *testing.T) {
	cases := []struct {
		name    string
		entry   string
		wantK   string
		wantV   string
		wantErr bool
	}{
		{"key and value", "api-version=preview", "api-version", "preview", false},
		{"empty value", "empty-value=", "empty-value", "", false},
		{"value containing equals", "with=equals=in=value", "with", "equals=in=value", false},
		{"missing key", "=missing-key", "", "", true},
		{"missing equals", "no-equals", "", "", true},
		{"empty entry", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			k, v, err := parseQueryParam(tc.entry)
			if tc.wantErr {
				must.Error(err)
				return
			}
			must.NoError(err)
			must.Equal(tc.wantK, k)
			must.Equal(tc.wantV, v)
		})
	}
}

// TestBuildHarnessRunConfig_AzureProviderFields verifies that the Azure
// OpenAI connection options (base URL, API key header, query parameters)
// propagate from harnessCLIOptions into the generated ProviderConfig.
func TestBuildHarnessRunConfig_AzureProviderFields(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	must.NoError(err)

	check.Equal("https://example.openai.azure.com/openai/v1", cfg.Provider.BaseURL)
	check.Equal("api-key", cfg.Provider.APIKeyHeader)
	check.Equal("preview", cfg.Provider.QueryParams["api-version"])
	must.NoError(types.ValidateRunConfig(cfg))
}

// TestBuildHarnessRunConfig_GuardRailFlags verifies that the GuardRail
// flags propagate from harnessCLIOptions into RunConfig.GuardRail, which
// is only constructed when at least one flag is non-zero.
func TestBuildHarnessRunConfig_GuardRailFlags(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	must.NoError(err)

	must.NotNil(cfg.GuardRail)
	check.Equal("granite-guardian", cfg.GuardRail.Type)
	check.Equal("http://localhost:8000", cfg.GuardRail.Endpoint)
	check.False(cfg.GuardRail.FailOpen, "fail-open should default to false")
}

// TestBuildHarnessRunConfig_ShieldstralFlags verifies that Shieldstral
// flags (including APIKeyRef) propagate from harnessCLIOptions into RunConfig.GuardRail.
func TestBuildHarnessRunConfig_ShieldstralFlags(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:              "test-run",
		Mode:               "execution",
		Prompt:             "test",
		ProviderType:       "anthropic",
		APIKeyRef:          "secret://ANTHROPIC_API_KEY",
		Model:              "claude-sonnet-4-6",
		MaxTurns:           20,
		Timeout:            600,
		TransportType:      "stdio",
		LogLevel:           "info",
		GuardRailType:      "shieldstral",
		GuardRailEndpoint:  "https://api.mistral.ai",
		GuardRailAPIKeyRef: "secret://MISTRAL_API_KEY",
		GuardRailModel:     "mistralai/Shieldstral-1.0-3B",
	})
	must.NoError(err)

	must.NotNil(cfg.GuardRail)
	check.Equal("shieldstral", cfg.GuardRail.Type)
	check.Equal("https://api.mistral.ai", cfg.GuardRail.Endpoint)
	check.Equal("secret://MISTRAL_API_KEY", cfg.GuardRail.APIKeyRef)
	check.Equal("mistralai/Shieldstral-1.0-3B", cfg.GuardRail.Model)
}

// TestBuildHarnessRunConfig_GuardRailDefaultNil verifies that the
// flag-only build path leaves config.GuardRail nil when no GuardRail
// flags are set, so the factory installs the no-op "none" guard.
func TestBuildHarnessRunConfig_GuardRailDefaultNil(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	})
	must.NoError(err)
	must.Nil(cfg.GuardRail, "GuardRail should be nil when no flags are set")
}

// TestBuildHarnessRunConfig_GuardRailFailOpenFlipsBoolean pins that
// --guardrail-fail-open alone is enough to materialise a GuardRail
// config, without requiring the other flags.
func TestBuildHarnessRunConfig_GuardRailFailOpenFlipsBoolean(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
	must.NoError(err)
	must.NotNil(cfg.GuardRail)
	must.True(cfg.GuardRail.FailOpen)
}

// TestApplyOverrides_GuardRailFlagsOverride verifies the override path
// for the GuardRail flags: each component is exercised independently
// since GuardRailConfig has multiple fields.
func TestApplyOverrides_GuardRailFlagsOverride(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://file-endpoint:8000",
		FailOpen: false,
	}

	setFlag := func(name, value string) {
		must.NoError(cmd.Flags().Set(name, value), "set %s", name)
	}
	setFlag("guardrail-endpoint", "http://flag-endpoint:1234")
	setFlag("guardrail-fail-open", "true")
	setFlag("guardrail-api-key-ref", "secret://FLAG_KEY")

	must.NoError(applyOverrides(cmd, cfg, nil))
	must.NotNil(cfg.GuardRail, "GuardRail should remain non-nil after override")
	check.Equal("granite-guardian", cfg.GuardRail.Type, "file type should survive when --guardrail is not set")
	check.Equal("http://flag-endpoint:1234", cfg.GuardRail.Endpoint)
	check.Equal("secret://FLAG_KEY", cfg.GuardRail.APIKeyRef)
	check.True(cfg.GuardRail.FailOpen, "--guardrail-fail-open should override the file value")
}

// TestApplyOverrides_GuardRailEndpointPreservesStages verifies that
// overriding only the endpoint leaves a composite stage list intact.
func TestApplyOverrides_GuardRailEndpointPreservesStages(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type: "composite",
		Stages: []types.GuardRailConfig{
			{Type: "granite-guardian", Endpoint: "http://stage-1:8000"},
			{Type: "cloud-judge"},
		},
	}

	must.NoError(cmd.Flags().Set("guardrail-endpoint", "http://flag-endpoint:1234"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.GuardRail)
	check.Equal("composite", cfg.GuardRail.Type, "composite type should survive")
	check.Len(cfg.GuardRail.Stages, 2, "composite stages should survive")
	check.Equal("http://flag-endpoint:1234", cfg.GuardRail.Endpoint)
}

// TestApplyOverrides_GuardRailModelOverride verifies that
// --guardrail-model overrides a file-provided model without disturbing
// other GuardRail fields.
func TestApplyOverrides_GuardRailModelOverride(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "cloud-judge",
		Endpoint: "http://file-endpoint:8000",
		Model:    "from-file",
		FailOpen: true,
	}

	must.NoError(cmd.Flags().Set("guardrail-model", "anthropic.claude-haiku-5-5"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.GuardRail, "GuardRail should remain non-nil")
	check.Equal("anthropic.claude-haiku-5-5", cfg.GuardRail.Model)
	check.Equal("cloud-judge", cfg.GuardRail.Type, "file type should survive the model override")
	check.Equal("http://file-endpoint:8000", cfg.GuardRail.Endpoint, "file endpoint should survive the model override")
	check.True(cfg.GuardRail.FailOpen, "file fail-open should survive the model override")
}

// TestApplyOverrides_GuardRailDefaultFlagsDoNotOverride pins the
// precedence rule for the GuardRail flags: if the user did not pass
// any of them, a file-provided GuardRail block must survive intact.
func TestApplyOverrides_GuardRailDefaultFlagsDoNotOverride(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://file-endpoint:8000",
		FailOpen: true,
	}

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.GuardRail, "GuardRail should remain non-nil")
	check.Equal("granite-guardian", cfg.GuardRail.Type, "file type should survive")
	check.Equal("http://file-endpoint:8000", cfg.GuardRail.Endpoint, "file endpoint should survive")
	check.True(cfg.GuardRail.FailOpen, "file fail-open should survive")
}

// TestApplyOverrides_GuardRailEmptyTypeClears verifies the
// "set type to empty string clears the GuardRail block" convention
// that mirrors --code-scanner. Operators use this to disable a
// guardrail block declared in a shared --config file without having
// to maintain a separate file.
func TestApplyOverrides_GuardRailEmptyTypeClears(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://file-endpoint:8000",
	}

	must.NoError(cmd.Flags().Set("guardrail", ""))
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Nil(cfg.GuardRail, "--guardrail='' should clear the GuardRail block")
}

// TestBuildHarnessRunConfig_GeminiProviderFields verifies that the Vertex AI
// Gemini provider type passes through the flag-only path: GCPProject
// and GCPLocation flow into ProviderConfig, the resulting RunConfig
// validates, and ModelRouter.Provider is wired to "gemini".
func TestBuildHarnessRunConfig_GeminiProviderFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "gemini",
		Model:         "gemini-2.5-pro",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
		GCPProject:    "my-project",
		GCPLocation:   "us-central1",
	})
	must.NoError(err)

	check.Equal("gemini", cfg.Provider.Type)
	check.Equal("my-project", cfg.Provider.GCPProject)
	check.Equal("us-central1", cfg.Provider.GCPLocation)
	check.Equal("gemini", cfg.ModelRouter.Provider)
	must.NoError(types.ValidateRunConfig(cfg), "ValidateRunConfig should accept the gemini config")
}

// TestBuildHarnessRunConfig_GeminiSuppressesAPIKeyRef ensures the
// flag-only path drops APIKeyRef when the provider is gemini, since the
// validator forbids apiKeyRef on gemini runs.
func TestBuildHarnessRunConfig_GeminiSuppressesAPIKeyRef(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "gemini",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "gemini-2.5-pro",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
		GCPProject:    "my-project",
		GCPLocation:   "global",
	})
	must.NoError(err)

	check.Empty(cfg.Provider.APIKeyRef, "APIKeyRef should be cleared for gemini")
	must.NoError(types.ValidateRunConfig(cfg), "ValidateRunConfig should accept gemini without apiKeyRef")
}

// TestBuildHarnessRunConfig_GeminiCredentialsFileImpliesType verifies
// that --gcp-credentials-file implies credential.type=gcp-service-account
// in the flag-only path, mirroring how --permission-policy-file implies
// type=policy-engine.
func TestBuildHarnessRunConfig_GeminiCredentialsFileImpliesType(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:              "test-run",
		Mode:               "execution",
		Prompt:             "test",
		ProviderType:       "gemini",
		Model:              "gemini-2.5-pro",
		MaxTurns:           20,
		Timeout:            600,
		TransportType:      "stdio",
		LogLevel:           "info",
		GCPProject:         "my-project",
		GCPLocation:        "global",
		GCPCredentialsFile: "/tmp/sa.json",
	})
	must.NoError(err)

	check.Equal("/tmp/sa.json", cfg.Provider.GCPCredentialsFile)
	must.NotNil(cfg.Provider.Credential, "credential should be inferred from --gcp-credentials-file")
	check.Equal("gcp-service-account", cfg.Provider.Credential.Type)
}

// TestBuildHarnessRunConfig_GeminiFieldsScopedToProviderType pins that
// GCP fields supplied while --provider is not gemini must not leak into
// the resulting RunConfig.
func TestBuildHarnessRunConfig_GeminiFieldsScopedToProviderType(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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

		GCPProject:  "leaked-project",
		GCPLocation: "global",
	})
	must.NoError(err)

	check.Empty(cfg.Provider.GCPProject, "GCPProject leaked onto anthropic provider")
	check.Empty(cfg.Provider.GCPLocation, "GCPLocation leaked onto anthropic provider")
}

// TestBuildHarnessRunConfig_ReasoningEffortPassesThrough pins the flag-only
// path for --reasoning-effort: the value lands on the top-level RunConfig
// field (not any provider entry), in the short lowercase form the config
// schema validates, rather than any provider's wire spelling.
func TestBuildHarnessRunConfig_ReasoningEffortPassesThrough(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:           "test-run",
		Mode:            "execution",
		Prompt:          "test",
		ProviderType:    "gemini",
		Model:           "gemini-3.7-flash",
		MaxTurns:        20,
		Timeout:         600,
		TransportType:   "stdio",
		LogLevel:        "info",
		GCPProject:      "my-project",
		GCPLocation:     "global",
		ReasoningEffort: "high",
	})
	must.NoError(err)
	must.Equal("high", cfg.ReasoningEffort)
}

// TestBuildHarnessRunConfig_ReasoningEffortInvalid confirms that a value the
// flag accepts but the schema rejects fails ValidateRunConfig on the built
// config, so an unknown level never reaches the Gemini per-model allow-list.
func TestBuildHarnessRunConfig_ReasoningEffortInvalid(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:           "test-run",
		Mode:            "execution",
		Prompt:          "test",
		ProviderType:    "gemini",
		Model:           "gemini-3.7-flash",
		MaxTurns:        20,
		Timeout:         600,
		TransportType:   "stdio",
		LogLevel:        "info",
		GCPProject:      "my-project",
		GCPLocation:     "global",
		ReasoningEffort: "THINKING_LEVEL_HIGH",
	})
	must.NoError(err)
	must.ErrorContains(types.ValidateRunConfig(cfg), "reasoningEffort")
}

// TestBuildHarnessRunConfig_ReasoningSummaryPassesThrough verifies that
// --reasoning-summary lands on the top-level RunConfig field.
func TestBuildHarnessRunConfig_ReasoningSummaryPassesThrough(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:            "test-run",
		Mode:             "execution",
		Prompt:           "test",
		ProviderType:     "anthropic",
		Model:            "claude-sonnet-5-5",
		MaxTurns:         20,
		Timeout:          600,
		TransportType:    "stdio",
		LogLevel:         "info",
		ReasoningSummary: "off",
	})
	must.NoError(err)
	must.Equal("off", cfg.ReasoningSummary)
}

// TestBuildHarnessRunConfig_ReasoningSummaryInvalid confirms a value the
// flag accepts but the schema does not is rejected before the run starts.
func TestBuildHarnessRunConfig_ReasoningSummaryInvalid(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:            "test-run",
		Mode:             "execution",
		Prompt:           "test",
		ProviderType:     "anthropic",
		Model:            "claude-sonnet-5-5",
		MaxTurns:         20,
		Timeout:          600,
		TransportType:    "stdio",
		LogLevel:         "info",
		ReasoningSummary: "detailed",
	})
	must.NoError(err)
	must.ErrorContains(types.ValidateRunConfig(cfg), "reasoningSummary")
}

func TestApplyOverrides_ReasoningSummaryFlagPrecedence(t *testing.T) {
	t.Run("flag overrides file", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.ReasoningSummary = "auto"
		must.NoError(cmd.Flags().Set("reasoning-summary", "off"))
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal("off", cfg.ReasoningSummary)
	})
	t.Run("unset flag keeps file value", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.ReasoningSummary = "off"
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal("off", cfg.ReasoningSummary)
	})
}

// TestApplyOverrides_GeminiFlagsReplaceFileValues exercises the --gcp-project,
// --gcp-location, and --gcp-credentials-file overrides on the --config
// path. Explicitly-set flags must clobber the file's values, and the
// credentials-file flag must imply a Credential.Type when none is set.
func TestApplyOverrides_GeminiFlagsReplaceFileValues(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "from-file",
		GCPLocation: "global",
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	setFlag := func(name, value string) {
		must.NoError(cmd.Flags().Set(name, value), "set %s", name)
	}
	setFlag("provider", "gemini")
	setFlag("gcp-project", "from-flag")
	setFlag("gcp-location", "us-central1")
	setFlag("gcp-credentials-file", "/tmp/sa.json")
	setFlag("reasoning-effort", "low")

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("low", cfg.ReasoningEffort)
	check.Equal("from-flag", cfg.Provider.GCPProject)
	check.Equal("us-central1", cfg.Provider.GCPLocation)
	check.Equal("/tmp/sa.json", cfg.Provider.GCPCredentialsFile)
	must.NotNil(cfg.Provider.Credential, "credential should be inferred from --gcp-credentials-file")
	check.Equal("gcp-service-account", cfg.Provider.Credential.Type)
}

// TestApplyOverrides_GeminiClearsAPIKeyRefFromConfigFile verifies that
// switching providers to gemini via --provider clears an APIKeyRef the
// config file inherited from a previous (non-gemini) configuration,
// matching the flag-only path's behaviour.
func TestApplyOverrides_GeminiClearsAPIKeyRefFromConfigFile(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	cfg.Provider = types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	}
	cfg.ModelRouter.Provider = "anthropic"
	cfg.ModelRouter.Model = "claude-sonnet-4-6"

	setFlag := func(name, value string) {
		must.NoError(cmd.Flags().Set(name, value), "set %s", name)
	}
	setFlag("provider", "gemini")
	setFlag("gcp-project", "my-proj")

	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("gemini", cfg.Provider.Type)
	check.Empty(cfg.Provider.APIKeyRef, "APIKeyRef should be cleared on gemini switch")
}

// TestApplyOverrides_GeminiPreservesExplicitAPIKeyRef pins that an
// operator explicitly passing --api-key-ref alongside --provider gemini
// wins: the CLI layer must not silently drop an explicit choice, even
// though the validator later rejects the combination.
func TestApplyOverrides_GeminiPreservesExplicitAPIKeyRef(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{Type: "anthropic"}
	cfg.ModelRouter.Provider = "anthropic"
	cfg.ModelRouter.Model = "claude-sonnet-4-6"

	must.NoError(cmd.Flags().Set("provider", "gemini"))
	must.NoError(cmd.Flags().Set("api-key-ref", "secret://EXPLICIT"))

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("secret://EXPLICIT", cfg.Provider.APIKeyRef, "explicit --api-key-ref dropped")
}

// TestApplyOverrides_GeminiDefaultLocationFallback verifies that a
// config file that omits gcpLocation, with no --gcp-location passed,
// falls back to the documented default ("global") on the --config path.
func TestApplyOverrides_GeminiDefaultLocationFallback(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:       "gemini",
		GCPProject: "my-proj",
		// GCPLocation deliberately empty.
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Equal("global", cfg.Provider.GCPLocation, "empty gcpLocation should fall back to the default")
}

// TestApplyOverrides_GeminiDefaultFlagsDoNotOverride pins that the
// gemini flag overrides only fire when the operator changed them.
// A config file that sets gcpProject and gcpLocation must not be
// silently overwritten when the CLI invocation leaves the flags at
// their defaults.
func TestApplyOverrides_GeminiDefaultFlagsDoNotOverride(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "from-file",
		GCPLocation: "us-central1",
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	// gcp-* flags stay at their cobra-registered defaults; the fallback
	// only applies when GCPLocation is empty, so "us-central1" survives.
	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("from-file", cfg.Provider.GCPProject, "config file gcpProject was overridden")
	check.Equal("us-central1", cfg.Provider.GCPLocation, "config file gcpLocation was overridden")
}

// TestApplyOverrides_GeminiCredentialsFileRespectsExplicitCredential
// verifies that --gcp-credentials-file does NOT clobber a Credential
// type that the --config file already set explicitly.
func TestApplyOverrides_GeminiCredentialsFileRespectsExplicitCredential(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "p",
		GCPLocation: "global",

		Credential: &types.CredentialConfig{Type: "gcp-workload-identity"},
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	must.NoError(cmd.Flags().Set("gcp-credentials-file", "/tmp/sa.json"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	check.Equal("/tmp/sa.json", cfg.Provider.GCPCredentialsFile, "gcp-credentials-file override failed")
	must.NotNil(cfg.Provider.Credential, "Credential cleared unexpectedly")
	check.Equal("gcp-workload-identity", cfg.Provider.Credential.Type, "Credential.Type clobbered")
}

// --- Anthropic Workload Identity Federation ---

// anthropicWIFBaseConfig produces a RunConfig stand-in for tests in this
// section. Anthropic provider, no federation block — each test layers on
// the WIF flags / env vars it wants and asserts the resulting Credential
// shape.
func anthropicWIFBaseConfig() *types.RunConfig {
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	}
	return cfg
}

// clearAnthropicWIFEnv hermetically scrubs the env vars
// applyAnthropicWIFOverrides reads. Tests that do not set them
// explicitly must still see them as empty so a contaminated CI runner
// does not flip an inference branch under our feet.
func clearAnthropicWIFEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ANTHROPIC_FEDERATION_RULE_ID",
		"ANTHROPIC_ORGANIZATION_ID",
		"ANTHROPIC_SERVICE_ACCOUNT_ID",
		"ANTHROPIC_WORKSPACE_ID",
		"ANTHROPIC_IDENTITY_TOKEN_FILE",
		"ANTHROPIC_IDENTITY_TOKEN",
		"ACTIONS_ID_TOKEN_REQUEST_URL",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN",
	} {
		t.Setenv(name, "")
	}
}

// TestApplyAnthropicWIF_EnvVarFallback verifies that ANTHROPIC_*_ID env
// vars fill in the four federation fields when no flag is set, and that
// the inferred credential type is anthropic-wif.
func TestApplyAnthropicWIF_EnvVarFallback(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	clearAnthropicWIFEnv(t)
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_envrule")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("ANTHROPIC_SERVICE_ACCOUNT_ID", "svac_envsa")
	t.Setenv("ANTHROPIC_WORKSPACE_ID", "default")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", "/var/run/secrets/idp/jwt")

	cmd := newTestHarnessCommand()
	cfg := anthropicWIFBaseConfig()

	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Credential to be inferred from env vars")
	check.Equal("anthropic-wif", cred.Type)
	check.Equal("fdrl_envrule", cred.FederationRuleID)
	check.Equal("11111111-1111-1111-1111-111111111111", cred.OrganizationID)
	check.Equal("svac_envsa", cred.ServiceAccountID)
	check.Equal("default", cred.WorkspaceID)

	must.NotNil(cred.TokenSource, "expected TokenSource to be inferred from ANTHROPIC_IDENTITY_TOKEN_FILE")
	check.Equal("file", cred.TokenSource.Type)
	check.Equal("/var/run/secrets/idp/jwt", cred.TokenSource.Path)

	// Default APIKeyRef must be cleared because no operator intent.
	check.Empty(cfg.Provider.APIKeyRef, "APIKeyRef should be cleared under WIF")
}

// TestApplyAnthropicWIF_ExplicitFlagBeatsEnv pins the precedence rule:
// when both flag and env var are set, the explicit flag wins.
func TestApplyAnthropicWIF_ExplicitFlagBeatsEnv(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	clearAnthropicWIFEnv(t)
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_envrule")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))
	must.NoError(cmd.Flags().Set("anthropic-organization-id", "22222222-2222-2222-2222-222222222222"))
	must.NoError(cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"))

	cfg := anthropicWIFBaseConfig()
	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	must.NotNil(cfg.Provider.Credential)
	check.Equal("fdrl_flagrule", cfg.Provider.Credential.FederationRuleID, "explicit flag should beat env")
}

// TestApplyAnthropicWIF_FromGitHubActionsSelectsTokenSource pins the
// explicit GHA opt-in: when --anthropic-from-github-actions is set, the
// inferred token source is github-actions-oidc with the Anthropic
// audience, regardless of any env vars present.
func TestApplyAnthropicWIF_FromGitHubActionsSelectsTokenSource(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	clearAnthropicWIFEnv(t)

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.actions.githubusercontent.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))
	must.NoError(cmd.Flags().Set("anthropic-organization-id", "33333333-3333-3333-3333-333333333333"))
	must.NoError(cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"))
	must.NoError(cmd.Flags().Set("anthropic-from-github-actions", "true"))

	cfg := anthropicWIFBaseConfig()
	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	cred := cfg.Provider.Credential
	must.NotNil(cred)
	must.NotNil(cred.TokenSource, "expected TokenSource set")
	check.Equal("github-actions-oidc", cred.TokenSource.Type)
	check.Equal("https://api.anthropic.com", cred.TokenSource.Audience)
}

// TestApplyAnthropicWIF_GHAEnvAloneDoesNotInferTokenSource pins that
// ACTIONS_ID_TOKEN_REQUEST_URL alone is not a green light to auto-select
// github-actions-oidc; the operator must explicitly opt in via
// --anthropic-from-github-actions.
func TestApplyAnthropicWIF_GHAEnvAloneDoesNotInferTokenSource(t *testing.T) {
	must := require.New(t)
	clearAnthropicWIFEnv(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.actions.githubusercontent.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))
	must.NoError(cmd.Flags().Set("anthropic-organization-id", "44444444-4444-4444-4444-444444444444"))
	must.NoError(cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"))
	// Deliberately NOT setting --anthropic-from-github-actions.

	cfg := anthropicWIFBaseConfig()
	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Credential to be inferred from federation flags")
	must.Nil(cred.TokenSource, "TokenSource should not be inferred from bare GHA env")
}

// TestApplyAnthropicWIF_IdentityTokenEnvVarSelectsEnvSource pins the
// fallback for ANTHROPIC_IDENTITY_TOKEN: a literal token in the env
// implies tokenSource={type:env, envVar:ANTHROPIC_IDENTITY_TOKEN}.
func TestApplyAnthropicWIF_IdentityTokenEnvVarSelectsEnvSource(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	clearAnthropicWIFEnv(t)
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN", "eyJ.fake.jwt")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))
	must.NoError(cmd.Flags().Set("anthropic-organization-id", "55555555-5555-5555-5555-555555555555"))
	must.NoError(cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"))

	cfg := anthropicWIFBaseConfig()
	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	cred := cfg.Provider.Credential
	must.NotNil(cred)
	must.NotNil(cred.TokenSource, "expected TokenSource set")
	check.Equal("env", cred.TokenSource.Type)
	check.Equal("ANTHROPIC_IDENTITY_TOKEN", cred.TokenSource.EnvVar)
}

// TestApplyAnthropicWIF_ExplicitAPIKeyRefRejected pins that when
// --api-key-ref is explicitly passed alongside the WIF flags, the
// override layer hard-fails rather than silently dropping one or the
// other.
func TestApplyAnthropicWIF_ExplicitAPIKeyRefRejected(t *testing.T) {
	must := require.New(t)
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))
	must.NoError(cmd.Flags().Set("anthropic-organization-id", "66666666-6666-6666-6666-666666666666"))
	must.NoError(cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"))
	must.NoError(cmd.Flags().Set("api-key-ref", "secret://OPERATOR_KEY"))

	cfg := anthropicWIFBaseConfig()
	cfg.Provider.APIKeyRef = "secret://OPERATOR_KEY"

	err := applyAnthropicWIFOverrides(cmd, cfg)
	must.ErrorContains(err, "api-key-ref")
}

// TestApplyAnthropicWIF_DefaultAPIKeyRefSilentlyCleared documents the
// other half of the apiKeyRef guard: the default flag value
// "secret://ANTHROPIC_API_KEY" carries no operator intent under WIF, so
// the override layer clears it silently rather than failing loudly.
func TestApplyAnthropicWIF_DefaultAPIKeyRefSilentlyCleared(t *testing.T) {
	must := require.New(t)
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))
	must.NoError(cmd.Flags().Set("anthropic-organization-id", "77777777-7777-7777-7777-777777777777"))
	must.NoError(cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"))
	// Deliberately NOT setting --api-key-ref. cfg carries the default.

	cfg := anthropicWIFBaseConfig()
	// The default-from-flag path: APIKeyRef holds the registered default.
	cfg.Provider.APIKeyRef = "secret://ANTHROPIC_API_KEY"

	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	must.Empty(cfg.Provider.APIKeyRef, "default APIKeyRef should be cleared under WIF")
}

// TestApplyAnthropicWIF_NoIntentLeavesConfigUnchanged verifies that with no
// WIF flags, env vars, or existing anthropic-wif credential, the config is
// left untouched.
func TestApplyAnthropicWIF_NoIntentLeavesConfigUnchanged(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	cfg := anthropicWIFBaseConfig()

	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	check.Nil(cfg.Provider.Credential, "Credential should be nil with no WIF intent")
	check.Equal("secret://ANTHROPIC_API_KEY", cfg.Provider.APIKeyRef, "APIKeyRef should be untouched without WIF intent")
}

// TestApplyAnthropicWIF_ConflictingExplicitTypeRejected covers the
// inconsistent-config rejection: if the user has already named a
// non-anthropic-wif credential type in --config and then layers WIF
// federation flags on top, the helper must fail loudly rather than
// silently rewriting the operator's choice.
func TestApplyAnthropicWIF_ConflictingExplicitTypeRejected(t *testing.T) {
	must := require.New(t)
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))

	cfg := anthropicWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{Type: "aws-default"}

	err := applyAnthropicWIFOverrides(cmd, cfg)
	must.ErrorContains(err, "anthropic-wif")
}

// TestApplyAnthropicWIF_ExistingStaticTypePromoted pins that when a
// --config file names credential.type="static" and the operator layers
// WIF flags on top, applyAnthropicWIFOverrides promotes the type to
// anthropic-wif rather than rejecting the run as a conflict.
func TestApplyAnthropicWIF_ExistingStaticTypePromoted(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"))
	must.NoError(cmd.Flags().Set("anthropic-organization-id", "99999999-9999-9999-9999-999999999999"))
	must.NoError(cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"))

	cfg := anthropicWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{Type: "static"}

	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	check.Equal("anthropic-wif", cfg.Provider.Credential.Type, "static credential must be promoted")
	check.Equal("fdrl_flagrule", cfg.Provider.Credential.FederationRuleID)
}

// TestApplyAnthropicWIF_ExistingTokenSourcePreserved guards the rule
// that an explicit token source from --config always wins. Even when
// --anthropic-from-github-actions is set, an existing TokenSource must
// not be silently overwritten.
func TestApplyAnthropicWIF_ExistingTokenSourcePreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-from-github-actions", "true"))

	cfg := anthropicWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{
		Type:             "anthropic-wif",
		FederationRuleID: "fdrl_filerule",
		OrganizationID:   "88888888-8888-8888-8888-888888888888",
		ServiceAccountID: "svac_filesa",
		TokenSource: &types.TokenSourceConfig{
			Type: "file",
			Path: "/var/run/file/jwt",
		},
	}

	must.NoError(applyAnthropicWIFOverrides(cmd, cfg))

	check.Equal("file", cfg.Provider.Credential.TokenSource.Type, "file-provided TokenSource overwritten")
	check.Equal("/var/run/file/jwt", cfg.Provider.Credential.TokenSource.Path, "file-provided TokenSource.Path overwritten")
}

// openAIWIFBaseConfig is the OpenAI counterpart of anthropicWIFBaseConfig:
// an openai-compatible provider carrying the cobra-default ANTHROPIC api-key
// ref, so the apiKeyRef-clearing branch in applyOpenAIWIFOverrides is
// exercised.
func openAIWIFBaseConfig() *types.RunConfig {
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:      "openai-compatible",
		BaseURL:   "https://api.openai.com/v1",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	}
	return cfg
}

// clearOpenAIWIFEnv hermetically scrubs the env vars
// applyOpenAIWIFOverrides reads so a contaminated CI runner cannot flip an
// inference branch.
func clearOpenAIWIFEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OPENAI_IDENTITY_PROVIDER_ID",
		"OPENAI_SERVICE_ACCOUNT_ID",
		"OPENAI_SUBJECT_TOKEN_TYPE",
		"OPENAI_IDENTITY_TOKEN_FILE",
		"OPENAI_IDENTITY_TOKEN",
		"ACTIONS_ID_TOKEN_REQUEST_URL",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN",
	} {
		t.Setenv(name, "")
	}
}

// TestApplyOpenAIWIF_EnvVarFallback verifies the OPENAI_* env vars fill in
// the identifiers when no flag is set and the inferred type is openai-wif.
func TestApplyOpenAIWIF_EnvVarFallback(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	t.Setenv("OPENAI_IDENTITY_PROVIDER_ID", "idp_env")
	t.Setenv("OPENAI_SERVICE_ACCOUNT_ID", "sa_env")
	t.Setenv("OPENAI_IDENTITY_TOKEN_FILE", "/var/run/secrets/idp/jwt")

	cmd := newTestHarnessCommand()
	cfg := openAIWIFBaseConfig()

	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))

	cred := cfg.Provider.Credential
	must.NotNil(cred, "expected Credential to be inferred from env vars")
	check.Equal("openai-wif", cred.Type)
	check.Equal("idp_env", cred.OpenAIIdentityProviderID)
	check.Equal("sa_env", cred.OpenAIServiceAccountID)

	must.NotNil(cred.TokenSource, "expected file token source from OPENAI_IDENTITY_TOKEN_FILE")
	check.Equal("file", cred.TokenSource.Type)
	check.Equal("/var/run/secrets/idp/jwt", cred.TokenSource.Path)

	check.Empty(cfg.Provider.APIKeyRef, "APIKeyRef should be cleared under WIF")
}

// TestApplyOpenAIWIF_ExplicitFlagBeatsEnv pins flag > env precedence.
func TestApplyOpenAIWIF_ExplicitFlagBeatsEnv(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	clearOpenAIWIFEnv(t)
	t.Setenv("OPENAI_IDENTITY_PROVIDER_ID", "idp_env")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("openai-identity-provider-id", "idp_flag"))
	must.NoError(cmd.Flags().Set("openai-service-account-id", "sa_flag"))

	cfg := openAIWIFBaseConfig()
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))

	must.NotNil(cfg.Provider.Credential)
	check.Equal("idp_flag", cfg.Provider.Credential.OpenAIIdentityProviderID, "explicit flag should beat env")
}

// TestApplyOpenAIWIF_FromGitHubActionsSelectsTokenSource pins the GHA opt-in:
// the inferred token source is github-actions-oidc with the OpenAI API
// audience.
func TestApplyOpenAIWIF_FromGitHubActionsSelectsTokenSource(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	clearOpenAIWIFEnv(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.actions.githubusercontent.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("openai-identity-provider-id", "idp_flag"))
	must.NoError(cmd.Flags().Set("openai-service-account-id", "sa_flag"))
	must.NoError(cmd.Flags().Set("openai-from-github-actions", "true"))

	cfg := openAIWIFBaseConfig()
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))

	cred := cfg.Provider.Credential
	must.NotNil(cred)
	must.NotNil(cred.TokenSource, "expected TokenSource set")
	check.Equal("github-actions-oidc", cred.TokenSource.Type)
	check.Equal("https://api.openai.com/v1", cred.TokenSource.Audience)
}

// TestApplyOpenAIWIF_GHAEnvAloneDoesNotInferTokenSource is the negative test
// for silent IdP selection: bare ACTIONS_ID_TOKEN_REQUEST_URL must not
// auto-select github-actions-oidc.
func TestApplyOpenAIWIF_GHAEnvAloneDoesNotInferTokenSource(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.actions.githubusercontent.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")
	t.Setenv("OPENAI_IDENTITY_PROVIDER_ID", "idp_env")
	t.Setenv("OPENAI_SERVICE_ACCOUNT_ID", "sa_env")

	cmd := newTestHarnessCommand()
	cfg := openAIWIFBaseConfig()
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))
	if cfg.Provider.Credential != nil {
		check.Nil(cfg.Provider.Credential.TokenSource, "token source must not be inferred from bare GHA env")
	}
}

// TestApplyOpenAIWIF_IdentityTokenEnvVarSelectsEnvSource pins the env-source
// inference from OPENAI_IDENTITY_TOKEN.
func TestApplyOpenAIWIF_IdentityTokenEnvVarSelectsEnvSource(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	clearOpenAIWIFEnv(t)
	t.Setenv("OPENAI_IDENTITY_PROVIDER_ID", "idp_env")
	t.Setenv("OPENAI_SERVICE_ACCOUNT_ID", "sa_env")
	t.Setenv("OPENAI_IDENTITY_TOKEN", "the-jwt")

	cmd := newTestHarnessCommand()
	cfg := openAIWIFBaseConfig()
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))

	ts := cfg.Provider.Credential.TokenSource
	must.NotNil(ts, "expected env token source")
	check.Equal("env", ts.Type)
	check.Equal("OPENAI_IDENTITY_TOKEN", ts.EnvVar)
}

// TestApplyOpenAIWIF_ExplicitAPIKeyRefRejected pins the apiKeyRef guard: an
// explicit --api-key-ref alongside openai-wif flags is a hard error.
func TestApplyOpenAIWIF_ExplicitAPIKeyRefRejected(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("openai-identity-provider-id", "idp_flag"))
	must.NoError(cmd.Flags().Set("openai-service-account-id", "sa_flag"))
	must.NoError(cmd.Flags().Set("api-key-ref", "secret://OPENAI_KEY"))

	cfg := openAIWIFBaseConfig()
	err := applyOpenAIWIFOverrides(cmd, cfg)
	must.ErrorContains(err, "api-key-ref must not be set")
}

// TestApplyOpenAIWIF_DefaultAPIKeyRefSilentlyCleared pins that the cobra
// default api-key-ref is cleared (not error) when WIF intent is expressed.
func TestApplyOpenAIWIF_DefaultAPIKeyRefSilentlyCleared(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("openai-identity-provider-id", "idp_flag"))
	must.NoError(cmd.Flags().Set("openai-service-account-id", "sa_flag"))

	cfg := openAIWIFBaseConfig() // APIKeyRef = default ANTHROPIC ref, not changed
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))
	must.Empty(cfg.Provider.APIKeyRef, "default APIKeyRef should be cleared under WIF")
}

// TestApplyOpenAIWIF_NoIntentNoOp pins that the helper does nothing when no
// WIF intent is expressed — a plain openai-compatible run keeps its key.
func TestApplyOpenAIWIF_NoIntentNoOp(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	cmd := newTestHarnessCommand()
	cfg := openAIWIFBaseConfig()
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))
	must.Nil(cfg.Provider.Credential, "no WIF intent should leave Credential nil")
	must.Equal("secret://ANTHROPIC_API_KEY", cfg.Provider.APIKeyRef,
		"APIKeyRef should be untouched without WIF intent")
}

// TestApplyOpenAIWIF_ConflictingExplicitTypeRejected pins that layering
// --openai-* flags on a config that already names a different credential
// type is a hard error rather than a silent rewrite.
func TestApplyOpenAIWIF_ConflictingExplicitTypeRejected(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("openai-identity-provider-id", "idp_flag"))

	cfg := openAIWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{Type: "azure-workload-identity"}
	err := applyOpenAIWIFOverrides(cmd, cfg)
	must.ErrorContains(err, "conflicting type")
}

// TestApplyOpenAIWIF_StaticCredentialTypePromoted pins that an existing
// type=static credential block is promoted to openai-wif when WIF flags are
// present, rather than being treated as a conflicting type.
func TestApplyOpenAIWIF_StaticCredentialTypePromoted(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("openai-identity-provider-id", "idp_flag"))
	must.NoError(cmd.Flags().Set("openai-service-account-id", "sa_flag"))

	cfg := openAIWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{
		Type: "static",
		TokenSource: &types.TokenSourceConfig{
			Type: "file",
			Path: "/var/run/file/jwt",
		},
	}
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))
	must.Equal("openai-wif", cfg.Provider.Credential.Type, "static credential should be promoted")
	must.Equal("idp_flag", cfg.Provider.Credential.OpenAIIdentityProviderID)
}

// TestApplyOpenAIWIF_SubjectTokenTypeApplied pins that the
// OPENAI_SUBJECT_TOKEN_TYPE env var (and the flag) is written onto the
// credential once the WIF flow is established by a required ID.
func TestApplyOpenAIWIF_SubjectTokenTypeApplied(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	t.Setenv("OPENAI_IDENTITY_PROVIDER_ID", "idp_env")
	t.Setenv("OPENAI_SERVICE_ACCOUNT_ID", "sa_env")
	t.Setenv("OPENAI_SUBJECT_TOKEN_TYPE", "urn:ietf:params:oauth:token-type:id_token")

	cmd := newTestHarnessCommand()
	cfg := openAIWIFBaseConfig()
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))
	must.NotNil(cfg.Provider.Credential, "subject token type should be applied to a WIF credential")
	must.Equal("urn:ietf:params:oauth:token-type:id_token", cfg.Provider.Credential.OpenAISubjectTokenType)
}

// TestApplyOpenAIWIF_SubjectTokenTypeAloneNoOp pins that a lone
// OPENAI_SUBJECT_TOKEN_TYPE (no required ID, no GHA opt-in) must not
// flip a plain openai-compatible run into the WIF path: the optional
// modifier is not a WIF discriminator.
func TestApplyOpenAIWIF_SubjectTokenTypeAloneNoOp(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	t.Setenv("OPENAI_SUBJECT_TOKEN_TYPE", "urn:ietf:params:oauth:token-type:id_token")

	cmd := newTestHarnessCommand()
	cfg := openAIWIFBaseConfig()
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))
	must.Nil(cfg.Provider.Credential, "subject-token-type alone must not infer WIF")
	must.Equal("secret://ANTHROPIC_API_KEY", cfg.Provider.APIKeyRef,
		"APIKeyRef should be untouched without WIF intent")
}

// TestApplyOpenAIWIF_FromGHAIgnoredWhenTokenSourceSet pins that
// --openai-from-github-actions does not overwrite a config-file-supplied
// token source (the config source always wins).
func TestApplyOpenAIWIF_FromGHAIgnoredWhenTokenSourceSet(t *testing.T) {
	must := require.New(t)
	clearOpenAIWIFEnv(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("openai-from-github-actions", "true"))

	cfg := openAIWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{
		Type:                     "openai-wif",
		OpenAIIdentityProviderID: "idp_file",
		OpenAIServiceAccountID:   "sa_file",
		TokenSource: &types.TokenSourceConfig{
			Type: "file",
			Path: "/var/run/file/jwt",
		},
	}
	must.NoError(applyOpenAIWIFOverrides(cmd, cfg))
	must.Equal("file", cfg.Provider.Credential.TokenSource.Type,
		"config-file TokenSource should not be overwritten by the GHA flag")
	must.Equal("/var/run/file/jwt", cfg.Provider.Credential.TokenSource.Path,
		"config-file TokenSource.Path should not be overwritten")
}

// TestBuildHarnessRunConfig_AzureWIFFlagsImplyCredential verifies that
// --azure-tenant-id (and the companion --azure-client-id / --azure-scope)
// in the flag-only path produce a Credential block with
// type=azure-workload-identity. Mirrors the --gcp-credentials-file
// shortcut: the flag is the discriminator.
func TestBuildHarnessRunConfig_AzureWIFFlagsImplyCredential(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "openai-compatible",
		APIKeyRef:     "secret://UNUSED", // ignored for WIF; validator clears via mutual-exclusion check
		BaseURL:       "https://example.openai.azure.com/openai/v1",
		Model:         "gpt-4o",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
		AzureTenantID: "11111111-1111-1111-1111-111111111111",
		AzureClientID: "22222222-2222-2222-2222-222222222222",
		AzureScope:    "https://cognitiveservices.azure.com/.default",
	})
	must.NoError(err)

	must.NotNil(cfg.Provider.Credential, "Credential should be inferred from --azure-tenant-id")
	must.Equal("azure-workload-identity", cfg.Provider.Credential.Type)
	must.Equal("11111111-1111-1111-1111-111111111111", cfg.Provider.Credential.AzureTenantID)
	must.Equal("22222222-2222-2222-2222-222222222222", cfg.Provider.Credential.AzureClientID)
	must.Equal("https://cognitiveservices.azure.com/.default", cfg.Provider.Credential.AzureScope)

	must.Empty(cfg.Provider.APIKeyRef, "APIKeyRef should be cleared for Azure WIF")
}

// TestBuildHarnessRunConfig_AzureWIFPassesValidation pins that an Azure
// WIF flag-only run (tenant + client + tokenSource) is accepted by
// types.ValidateRunConfig end-to-end.
func TestBuildHarnessRunConfig_AzureWIFPassesValidation(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "openai-compatible",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY", // cobra default; should be cleared
		BaseURL:       "https://example.openai.azure.com/openai/v1",
		Model:         "gpt-4o",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
		AzureTenantID: "11111111-1111-1111-1111-111111111111",
		AzureClientID: "22222222-2222-2222-2222-222222222222",
		AzureScope:    "https://cognitiveservices.azure.com/.default",
	})
	must.NoError(err)
	// buildHarnessRunConfig only assembles the flag-implied Credential
	// shell; wire a TokenSource by hand so validation runs end-to-end.
	must.NotNil(cfg.Provider.Credential, "Credential should be inferred from --azure-tenant-id")
	cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
		Type: "file",
		Path: "/var/run/secrets/azure/token",
	}

	must.NoError(types.ValidateRunConfig(cfg), "ValidateRunConfig should accept Azure WIF flag-only run")
}

// TestBuildHarnessRunConfig_AzureWIFTenantWithoutClient verifies that a
// --azure-tenant-id passed without --azure-client-id still produces the
// implied Credential block. The validator (a separate layer) will then
// reject the run with a clear "azure-workload-identity requires
// azureClientId" error — the flag mapping itself is mechanical and must
// not silently drop a partial spec.
func TestBuildHarnessRunConfig_AzureWIFTenantWithoutClient(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "openai-compatible",
		BaseURL:       "https://example.openai.azure.com/openai/v1",
		Model:         "gpt-4o",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
		AzureTenantID: "11111111-1111-1111-1111-111111111111",
		// AzureClientID intentionally empty; validator's job to reject.
	})
	must.NoError(err)

	must.NotNil(cfg.Provider.Credential, "Credential should be inferred from --azure-tenant-id alone")
	must.Equal("azure-workload-identity", cfg.Provider.Credential.Type)
	must.Equal("11111111-1111-1111-1111-111111111111", cfg.Provider.Credential.AzureTenantID)
	must.Empty(cfg.Provider.Credential.AzureClientID)
}

// buildHarnessRunConfig infers no Credential when no --azure-* flag is set.
func TestBuildHarnessRunConfig_AzureWIFNotSetLeavesCredentialNil(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "execution",
		Prompt:        "test",
		ProviderType:  "openai-compatible",
		APIKeyRef:     "secret://OPENAI_KEY",
		BaseURL:       "https://api.openai.com/v1",
		Model:         "gpt-4o",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})
	must.NoError(err)
	must.Nil(cfg.Provider.Credential, "Credential should remain nil when no Azure WIF flags are set")
}

// TestApplyOverrides_AzureWIFFlags exercises the --azure-* overrides on
// the --config path. An explicitly-set --azure-tenant-id must:
//   - Imply a Credential block when the file has none.
//   - Populate AzureTenantID / AzureClientID / AzureScope on the
//     resulting Credential.
func TestApplyOverrides_AzureWIFFlags(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:    "openai-compatible",
		BaseURL: "https://example.openai.azure.com/openai/v1",
	}
	cfg.ModelRouter.Provider = "openai-compatible"
	cfg.ModelRouter.Model = "gpt-4o"

	must.NoError(cmd.Flags().Set("provider", "openai-compatible"))
	must.NoError(cmd.Flags().Set("azure-tenant-id", "11111111-1111-1111-1111-111111111111"))
	must.NoError(cmd.Flags().Set("azure-client-id", "22222222-2222-2222-2222-222222222222"))
	must.NoError(cmd.Flags().Set("azure-scope", "https://cognitiveservices.azure.com/.default"))

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.Provider.Credential, "Credential should be inferred from --azure-tenant-id")
	must.Equal("azure-workload-identity", cfg.Provider.Credential.Type)
	must.Equal("11111111-1111-1111-1111-111111111111", cfg.Provider.Credential.AzureTenantID)
	must.Equal("22222222-2222-2222-2222-222222222222", cfg.Provider.Credential.AzureClientID)
	must.Equal("https://cognitiveservices.azure.com/.default", cfg.Provider.Credential.AzureScope)
}

// TestApplyOverrides_AzureWIFRespectsExplicitCredential pins that an
// explicit Credential block in --config (e.g. credential.type=static)
// is NOT silently upgraded to azure-workload-identity by a stray
// --azure-tenant-id flag; the Azure-named fields are still filled in on
// the existing block, but the type is preserved.
func TestApplyOverrides_AzureWIFRespectsExplicitCredential(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:      "openai-compatible",
		BaseURL:   "https://example.openai.azure.com/openai/v1",
		APIKeyRef: "secret://OPENAI_KEY",
		Credential: &types.CredentialConfig{
			Type: "static",
		},
	}
	cfg.ModelRouter.Provider = "openai-compatible"
	cfg.ModelRouter.Model = "gpt-4o"

	must.NoError(cmd.Flags().Set("azure-tenant-id", "11111111-1111-1111-1111-111111111111"))
	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.Provider.Credential, "Credential should not be cleared")
	must.Equal("static", cfg.Provider.Credential.Type, "Credential type should not be upgraded")
	must.Equal("11111111-1111-1111-1111-111111111111", cfg.Provider.Credential.AzureTenantID,
		"AzureTenantID should be propagated to the existing Credential")
}

// TestApplyOverrides_AzureWIFClientIDAloneDoesNotCreateCredential pins
// that --azure-client-id without --azure-tenant-id leaves the Credential
// untouched: only --azure-tenant-id is the discriminator that
// materialises an azure-workload-identity Credential block.
func TestApplyOverrides_AzureWIFClientIDAloneDoesNotCreateCredential(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:      "openai-compatible",
		BaseURL:   "https://example.openai.azure.com/openai/v1",
		APIKeyRef: "secret://OPENAI_KEY",
	}

	must.NoError(cmd.Flags().Set("azure-client-id", "22222222-2222-2222-2222-222222222222"))
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Nil(cfg.Provider.Credential, "--azure-client-id alone must not create a Credential block")
}

// TestApplyOverrides_AzureWIFScopeAloneDoesNotCreateCredential is the
// companion to the client-id test above. --azure-scope without
// --azure-tenant-id must not produce an orphan Credential block.
func TestApplyOverrides_AzureWIFScopeAloneDoesNotCreateCredential(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:      "openai-compatible",
		BaseURL:   "https://example.openai.azure.com/openai/v1",
		APIKeyRef: "secret://OPENAI_KEY",
	}

	must.NoError(cmd.Flags().Set("azure-scope", "https://cognitiveservices.azure.com/.default"))
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Nil(cfg.Provider.Credential, "--azure-scope alone must not create a Credential block")
}

// TestApplyOverrides_AzureWIFDefaultFlagsDoNotOverride pins that when
// none of the --azure-* flags is passed, an existing Credential block
// from --config survives untouched.
func TestApplyOverrides_AzureWIFDefaultFlagsDoNotOverride(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:    "openai-compatible",
		BaseURL: "https://example.openai.azure.com/openai/v1",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "33333333-3333-3333-3333-333333333333",
			AzureClientID: "44444444-4444-4444-4444-444444444444",
			AzureScope:    "https://existing.example.com/.default",
		},
	}

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.Provider.Credential, "file-provided Credential should not be cleared")
	must.Equal("33333333-3333-3333-3333-333333333333", cfg.Provider.Credential.AzureTenantID)
	must.Equal("44444444-4444-4444-4444-444444444444", cfg.Provider.Credential.AzureClientID)
	must.Equal("https://existing.example.com/.default", cfg.Provider.Credential.AzureScope)
}

// writePromptResolutionConfig writes a JSON RunConfig with an empty
// prompt and an out-of-range MaxTurns, so callers can distinguish
// "prompt is required" (resolution never filled cfg.Prompt) from a
// deterministic "maxTurns exceeds maximum" validation failure (it did).
func writePromptResolutionConfig(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	timeout := 60
	cfg := types.RunConfig{
		RunID:            "x",
		Mode:             "execution",
		Prompt:           "", // deliberately empty — resolution chain must fill this
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
		MaxTurns:         9999, // out-of-range → predictable post-prompt validation failure
		Timeout:          &timeout,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	must.NoError(err)
	must.NoError(os.WriteFile(path, data, 0o600))
	return path
}

// TestRunHarness_PromptFromEnvVar pins the STIRRUP_PROMPT fallback: when
// no higher-priority source is set, the env var must populate
// cfg.Prompt, observed via the downstream maxTurns validation error.
func TestRunHarness_PromptFromEnvVar(t *testing.T) {
	must := require.New(t)
	path := writePromptResolutionConfig(t)
	t.Setenv("STIRRUP_PROMPT", "hello from env")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", path))

	err := runHarness(cmd, nil)
	must.Error(err, "expected validation error after prompt resolution")
	must.NotContains(err.Error(), "prompt is required", "STIRRUP_PROMPT should be consulted")
	must.ErrorContains(err, "maxTurns")
}

// TestRunHarness_PromptFromPromptFile pins the --prompt-file source:
// the file's contents become cfg.Prompt with trailing newlines trimmed.
// Same downstream-validation trick as the env-var test — a maxTurns
// rejection means we got past the prompt-required check, which means
// the file was read and applied.
func TestRunHarness_PromptFromPromptFile(t *testing.T) {
	must := require.New(t)
	path := writePromptResolutionConfig(t)

	promptDir := t.TempDir()
	promptPath := filepath.Join(promptDir, "brief.txt")
	// The trailing newline must be trimmed; readPromptFile is checked below.
	must.NoError(os.WriteFile(promptPath, []byte("hello from file\n"), 0o600))

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", path))
	must.NoError(cmd.Flags().Set("prompt-file", promptPath))

	err := runHarness(cmd, nil)
	must.Error(err, "expected validation error after prompt resolution")
	must.NotContains(err.Error(), "prompt is required", "--prompt-file should be consulted")
	must.ErrorContains(err, "maxTurns")

	got, err := readPromptFile(promptPath)
	must.NoError(err)
	must.Equal("hello from file", got, "readPromptFile should trim the trailing newline")
}

// TestRunHarness_PromptFlagBeatsLowerPrecedence pins the prompt-source
// precedence: --prompt outranks --prompt-file and STIRRUP_PROMPT, so
// runHarness must reach validation without ever reading the file (set
// to a nonexistent path) or consulting the env.
func TestRunHarness_PromptFlagBeatsLowerPrecedence(t *testing.T) {
	must := require.New(t)
	path := writePromptResolutionConfig(t)
	t.Setenv("STIRRUP_PROMPT", "from env (should lose)")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", path))
	must.NoError(cmd.Flags().Set("prompt", "from flag (should win)"))
	must.NoError(cmd.Flags().Set("prompt-file", "/does/not/exist/brief.txt"))

	err := runHarness(cmd, nil)
	must.Error(err, "expected validation error after prompt resolution")
	must.NotContains(err.Error(), "reading --prompt-file", "--prompt should short-circuit --prompt-file")
	must.NotContains(err.Error(), "prompt is required", "--prompt should be used")
	must.ErrorContains(err, "maxTurns")
}

// TestReadPromptFile_MissingFileNamesPath verifies the explicit error surface
// for a missing file: the path name must appear in the message so the
// operator can find the typo without re-running with --log-level=debug.
func TestReadPromptFile_MissingFileNamesPath(t *testing.T) {
	must := require.New(t)
	_, err := readPromptFile("/does/not/exist/brief.txt")
	must.ErrorContains(err, "/does/not/exist/brief.txt")
}

// TestReadPromptFile_EmptyFileIsError pins that a zero-byte prompt file is
// reported as an error, not as an empty prompt that later fails with
// "prompt is required".
func TestReadPromptFile_EmptyFileIsError(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	must.NoError(os.WriteFile(path, nil, 0o600))
	_, err := readPromptFile(path)
	must.ErrorContains(err, "is empty")
}

// TestReadPromptFile_DirectoryIsError pins the IsDir() guard: a directory path
// surfaces a clear "is a directory" error with the path baked in,
// rather than the opaque error os.ReadFile would return.
func TestReadPromptFile_DirectoryIsError(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	_, err := readPromptFile(dir)
	must.ErrorContains(err, "is a directory")
}

// TestReadPromptFile_OversizeRejected pins the maxPromptFileBytes cap,
// which guards both the stat-time check and the post-read
// io.LimitReader check.
func TestReadPromptFile_OversizeRejected(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	data := make([]byte, maxPromptFileBytes+1)
	must.NoError(os.WriteFile(path, data, 0o600))
	_, err := readPromptFile(path)
	must.ErrorContains(err, "exceeds")
}

// newFlagOnlyHarnessCommand builds a command for flag-only tests (no
// --config), so runHarness enters the buildHarnessRunConfig branch.
// --max-turns is set to 9999 so a successful prompt resolution surfaces
// a deterministic "maxTurns exceeds maximum" validator error.
func newFlagOnlyHarnessCommand(t *testing.T) *cobra.Command {
	t.Helper()
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("max-turns", "9999"))

	t.Setenv("STIRRUP_FLAG_ONLY_TEST_KEY", "x")
	must.NoError(cmd.Flags().Set("api-key-ref", "env://STIRRUP_FLAG_ONLY_TEST_KEY"))
	return cmd
}

// TestRunHarness_FlagOnly_PromptFromEnvVar covers the STIRRUP_PROMPT
// fallback on the flag-only path.
func TestRunHarness_FlagOnly_PromptFromEnvVar(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_PROMPT", "hello from env")

	err := runHarness(newFlagOnlyHarnessCommand(t), nil)
	must.Error(err, "validation should fail after the prompt is resolved")
	must.NotContains(err.Error(), "prompt is required", "STIRRUP_PROMPT should be consulted")
	must.ErrorContains(err, "maxTurns")
}

// TestRunHarness_FlagOnly_PromptFromPromptFile is the --prompt-file
// counterpart on the flag-only path. The trailing newline trim contract
// is pinned by TestRunHarness_PromptFromPromptFile against readPromptFile
// directly; this test only confirms the file reaches the resolution chain.
func TestRunHarness_FlagOnly_PromptFromPromptFile(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_PROMPT", "")

	promptPath := filepath.Join(t.TempDir(), "brief.txt")
	must.NoError(os.WriteFile(promptPath, []byte("hello from file\n"), 0o600))

	cmd := newFlagOnlyHarnessCommand(t)
	must.NoError(cmd.Flags().Set("prompt-file", promptPath))

	err := runHarness(cmd, nil)
	must.Error(err, "validation should fail after the prompt is resolved")
	must.NotContains(err.Error(), "prompt is required", "--prompt-file should be consulted")
	must.ErrorContains(err, "maxTurns")
}

// TestRunHarness_FlagOnly_AllSourcesEmpty asserts that the "prompt is
// required" error on the flag-only path names every prompt source, so an
// operator sees the full chain without grepping the source.
func TestRunHarness_FlagOnly_AllSourcesEmpty(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	t.Setenv("STIRRUP_PROMPT", "")

	err := runHarness(newFlagOnlyHarnessCommand(t), nil)
	must.Error(err, "prompt-required error")
	for _, source := range []string{"--prompt-file", "STIRRUP_PROMPT", "positional argument"} {
		check.ErrorContains(err, source, "prompt-required error should name every source")
	}
}

// --- provider-retry flag-to-config wiring ---

// TestApplyProviderRetryOverrides_SubMillisecondRejected pins the helper's
// refusal to silently truncate a sub-millisecond duration to zero: that
// would satisfy the ms-conversion's zero-guard and erase operator intent.
func TestApplyProviderRetryOverrides_SubMillisecondRejected(t *testing.T) {
	cases := []struct {
		name   string
		opts   harnessCLIOptions
		expect string
	}{
		{
			name:   "initial-delay below 1ms",
			opts:   harnessCLIOptions{ProviderRetryInitialDelay: 500 * time.Microsecond},
			expect: "--provider-retry-initial-delay",
		},
		{
			name:   "max-delay below 1ms",
			opts:   harnessCLIOptions{ProviderRetryMaxDelay: 100 * time.Microsecond},
			expect: "--provider-retry-max-delay",
		},
		{
			name:   "wall-clock below 1ms",
			opts:   harnessCLIOptions{ProviderRetryWallClockBudget: 250 * time.Microsecond},
			expect: "--provider-retry-wall-clock",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			pc := &types.ProviderConfig{}
			err := applyProviderRetryOverrides(pc, tc.opts)
			must.Error(err, "sub-millisecond duration should be rejected")
			must.ErrorContains(err, tc.expect)
			must.ErrorContains(err, "minimum resolution is 1ms")
		})
	}
}

// TestApplyProviderRetryFlagOverrides_SubMillisecondRejected covers the
// --config path's mirror of the same check.
func TestApplyProviderRetryFlagOverrides_SubMillisecondRejected(t *testing.T) {
	cases := []struct {
		name  string
		flag  string
		value string
	}{
		{name: "initial-delay below 1ms", flag: "provider-retry-initial-delay", value: "500us"},
		{name: "max-delay below 1ms", flag: "provider-retry-max-delay", value: "100us"},
		{name: "wall-clock below 1ms", flag: "provider-retry-wall-clock", value: "250us"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestHarnessCommand()
			must.NoError(cmd.Flags().Set(tc.flag, tc.value))
			pc := &types.ProviderConfig{}
			err := applyProviderRetryFlagOverrides(cmd, pc)
			must.Error(err, "sub-millisecond duration should be rejected")
			must.ErrorContains(err, "--"+tc.flag)
			must.ErrorContains(err, "minimum resolution is 1ms")
		})
	}
}

// TestApplyProviderRetryOverrides_AllFlagsSet asserts each of the four
// flags lands on its corresponding ProviderRetryConfig field with the
// duration→ms conversion applied.
func TestApplyProviderRetryOverrides_AllFlagsSet(t *testing.T) {
	must := require.New(t)
	pc := &types.ProviderConfig{}
	opts := harnessCLIOptions{
		ProviderRetryMaxAttempts:     4,
		ProviderRetryInitialDelay:    250 * time.Millisecond,
		ProviderRetryMaxDelay:        10 * time.Second,
		ProviderRetryWallClockBudget: 45 * time.Second,
	}
	must.NoError(applyProviderRetryOverrides(pc, opts))
	must.NotNil(pc.Retry, "retry block should be allocated")
	must.Equal(types.ProviderRetryConfig{
		MaxAttempts:       4,
		InitialDelayMs:    250,
		MaxDelayMs:        10000,
		WallClockBudgetMs: 45000,
	}, *pc.Retry)
}

// TestApplyProviderRetryOverrides_SingleFlagPartialOverride asserts that
// setting one flag does not implicitly zero the other slots — the
// per-field defaulter in ValidateRunConfig still fills in the remaining
// fields downstream.
func TestApplyProviderRetryOverrides_SingleFlagPartialOverride(t *testing.T) {
	must := require.New(t)
	pc := &types.ProviderConfig{}
	opts := harnessCLIOptions{ProviderRetryMaxAttempts: 5}
	must.NoError(applyProviderRetryOverrides(pc, opts))
	must.NotNil(pc.Retry, "retry block should be allocated for a single flag")
	must.Equal(types.ProviderRetryConfig{MaxAttempts: 5}, *pc.Retry)
}

// TestApplyProviderRetryOverrides_AllZeroIsNoop asserts that an entirely-
// untouched flag surface leaves pc.Retry nil, preserving the documented
// "no override" path (ValidateRunConfig then fills in all defaults).
func TestApplyProviderRetryOverrides_AllZeroIsNoop(t *testing.T) {
	must := require.New(t)
	pc := &types.ProviderConfig{}
	must.NoError(applyProviderRetryOverrides(pc, harnessCLIOptions{}))
	must.Nil(pc.Retry)
}

// TestApplyOverrides_ProviderRetryFlagOverridesFile asserts that a
// single Changed() flag rewrites only the corresponding file slot,
// leaving the other file-supplied retry values untouched. This is the
// "operator pins one knob" contract for the --config + flag combo.
func TestApplyOverrides_ProviderRetryFlagOverridesFile(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("provider-retry-wall-clock", "120s"))
	cfg := baseFileConfig()
	cfg.Provider.Retry = &types.ProviderRetryConfig{
		MaxAttempts:       2,
		InitialDelayMs:    750,
		MaxDelayMs:        20000,
		WallClockBudgetMs: 60000,
	}
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.NotNil(cfg.Provider.Retry, "retry must not be cleared")
	must.Equal(types.ProviderRetryConfig{
		MaxAttempts:       2,
		InitialDelayMs:    750,
		MaxDelayMs:        20000,
		WallClockBudgetMs: 120000,
	}, *cfg.Provider.Retry)
}

// TestApplyOverrides_ProviderRetryFlagAllocatesNilRetry asserts that
// when the file omits the retry block entirely, a single Changed() flag
// allocates the struct and writes the field; the rest of the slots stay
// zero so ValidateRunConfig fills them with the documented defaults.
func TestApplyOverrides_ProviderRetryFlagAllocatesNilRetry(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("provider-retry-max-attempts", "5"))
	cfg := baseFileConfig()
	cfg.Provider.Retry = nil
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.NotNil(cfg.Provider.Retry, "retry block should be allocated")
	must.Equal(types.ProviderRetryConfig{MaxAttempts: 5}, *cfg.Provider.Retry)
}

// TestApplyOverrides_ProviderRetryNoFlagsChangedDoesNotClobberFile is
// the symmetric "Changed-guards do their job" assertion: a
// fully-populated file retry block must survive an applyOverrides call
// where none of the four retry flags were touched.
func TestApplyOverrides_ProviderRetryNoFlagsChangedDoesNotClobberFile(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Retry = &types.ProviderRetryConfig{
		MaxAttempts:       2,
		InitialDelayMs:    750,
		MaxDelayMs:        20000,
		WallClockBudgetMs: 60000,
	}
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.NotNil(cfg.Provider.Retry, "retry must not be cleared")
	must.Equal(types.ProviderRetryConfig{
		MaxAttempts:       2,
		InitialDelayMs:    750,
		MaxDelayMs:        20000,
		WallClockBudgetMs: 60000,
	}, *cfg.Provider.Retry, "file retry block should not be mutated")
}

// TestApplyOverrides_WorkspaceExportToFlowsThrough pins that
// --export-workspace-to lands on ExecutorConfig.WorkspaceExportTo so
// the end-of-run exporter wiring in runWithConfig sees it.
func TestApplyOverrides_WorkspaceExportToFlowsThrough(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.WorkspaceExportTo = "gs://from-file/runs/old.tar.gz"

	must.NoError(cmd.Flags().Set("export-workspace-to", "gs://from-flag/runs/new.tar.gz"))
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal("gs://from-flag/runs/new.tar.gz", cfg.Executor.WorkspaceExportTo, "flag should override file")
}

// TestApplyOverrides_WorkspaceExportToDefaultPreservesFile mirrors the
// rest of the override surface: an unset flag must not clobber the
// file's value (the central precedence rule that the rest of
// applyOverrides tests pin).
func TestApplyOverrides_WorkspaceExportToDefaultPreservesFile(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.WorkspaceExportTo = "gs://from-file/runs/keep.tar.gz"

	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Equal("gs://from-file/runs/keep.tar.gz", cfg.Executor.WorkspaceExportTo, "unset flag should preserve file value")
}

// TestBuildHarnessRunConfig_WorkspaceExportToFlowsThrough pins that
// buildHarnessRunConfig threads WorkspaceExportTo onto ExecutorConfig
// on the flag-only path.
func TestBuildHarnessRunConfig_WorkspaceExportToFlowsThrough(t *testing.T) {
	must := require.New(t)
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
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
		WorkspaceExportTo: "gs://my-bucket/runs/abc/workspace.tar.gz",
	})
	must.NoError(err)
	must.Equal("gs://my-bucket/runs/abc/workspace.tar.gz", cfg.Executor.WorkspaceExportTo)
}

// TestApplyOverrides_ToolDispatchMaxParallelSetsField pins that
// --max-tool-parallel=N (with N > 0) populates cfg.ToolDispatch to
// {MaxParallel: N}.
func TestApplyOverrides_ToolDispatchMaxParallelSetsField(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	must.NoError(cmd.Flags().Set("max-tool-parallel", "8"))

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.ToolDispatch, "--max-tool-parallel=8 should populate ToolDispatch")
	must.Equal(8, cfg.ToolDispatch.MaxParallel)
}

// TestApplyOverrides_ToolDispatchMaxParallelDefaultDoesNotOverride pins
// that a config-file ToolDispatch value survives when the user has not
// passed --max-tool-parallel.
func TestApplyOverrides_ToolDispatchMaxParallelDefaultDoesNotOverride(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.ToolDispatch = &types.ToolDispatchConfig{MaxParallel: 8}

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.ToolDispatch, "ToolDispatch from file should survive default flag")
	must.Equal(8, cfg.ToolDispatch.MaxParallel)
}

// TestApplyOverrides_EscalateToolChoiceEnables pins that
// --escalate-tool-choice flips Enabled on cfg.ToolChoiceEscalation,
// allocating the sub-config when the file omitted it.
func TestApplyOverrides_EscalateToolChoiceEnables(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	must.NoError(cmd.Flags().Set("escalate-tool-choice", "true"))

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.ToolChoiceEscalation, "--escalate-tool-choice should allocate escalation")
	must.True(cfg.ToolChoiceEscalation.Enabled)
}

// TestApplyOverrides_EscalateToolChoiceDefaultDoesNotEnable pins the
// OFF-by-default safety: without the flag, no escalation sub-config is
// synthesised so a bare run stays inert.
func TestApplyOverrides_EscalateToolChoiceDefaultDoesNotEnable(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.Nil(cfg.ToolChoiceEscalation, "escalation must stay nil without the flag")
}

// TestApplyOverrides_EscalateToolChoiceMaxRetries pins that the retry-cap
// flag lands on cfg.ToolChoiceEscalation.MaxRetries, allocating the
// sub-config when needed.
func TestApplyOverrides_EscalateToolChoiceMaxRetries(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	must.NoError(cmd.Flags().Set("escalate-tool-choice", "true"))
	must.NoError(cmd.Flags().Set("escalate-tool-choice-max-retries", "2"))

	must.NoError(applyOverrides(cmd, cfg, nil))

	must.NotNil(cfg.ToolChoiceEscalation, "escalation sub-config should be populated")
	must.Equal(2, cfg.ToolChoiceEscalation.MaxRetries)
}

// TestRunHarness_OutputRunConfigWritesFile pins the dry-run capture
// surface: --output-runconfig writes the resolved RunConfig to disk, the
// loop is not invoked, and the process exits cleanly.
func TestRunHarness_OutputRunConfigWritesFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.json")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("output-runconfig", outPath))
	must.NoError(cmd.Flags().Set("prompt", "test prompt"))

	must.NoError(runHarness(cmd, nil), "--output-runconfig should exit cleanly")

	info, err := os.Stat(outPath)
	must.NoError(err)
	// 0600 is the documented contract: captured configs may carry
	// secret:// references whose names are operationally sensitive even
	// though the references themselves are not secrets.
	if runtime.GOOS != "windows" {
		must.Equal(os.FileMode(0o600), info.Mode().Perm())
	}

	cfg, err := loadRunConfigFile(outPath)
	must.NoError(err, "captured config should round-trip through loadRunConfigFile")
	must.Equal("test prompt", cfg.Prompt)
	must.NotEmpty(cfg.RunID, "ResolveAll should mint a RunID")
}

// TestRunHarness_OutputRunConfigToStdout pins the "-" sentinel: a
// captured config can flow straight back into another `stirrup
// run-config` or `stirrup harness --config -` without a temp file hop.
func TestRunHarness_OutputRunConfigToStdout(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("output-runconfig", "-"))
	must.NoError(cmd.Flags().Set("prompt", "test prompt"))

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	must.NoError(err)
	os.Stdout = w
	defer func() {
		os.Stdout = origStdout
		_ = r.Close()
	}()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()

	runErr := runHarness(cmd, nil)
	_ = w.Close()
	out := <-done

	must.NoError(runErr, "--output-runconfig=- should exit cleanly")
	var cfg types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &cfg), "stdout should be parseable JSON:\n%s", out)
	must.Equal("test prompt", cfg.Prompt)
}

// TestRunHarness_OutputRunConfigDoesNotWriteOnValidationFailure pins the
// "never writes on validation failure" contract for --output-runconfig.
func TestRunHarness_OutputRunConfigDoesNotWriteOnValidationFailure(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.json")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("output-runconfig", outPath))
	must.NoError(cmd.Flags().Set("prompt", "test prompt"))
	must.NoError(cmd.Flags().Set("provider", "bedrock"))

	must.Error(runHarness(cmd, nil), "validator should reject bedrock with the sonnet-4-6 alias")
	_, statErr := os.Stat(outPath)
	must.ErrorIs(statErr, os.ErrNotExist, "output file should not exist on validation failure")
}

// TestRunHarness_OutputRunConfigReplaysIdentically pins the "save and
// replay" workflow: a config captured via --output-runconfig, fed back
// into `stirrup harness --config <path>`, produces an equivalent
// resolved config (RunID excepted, since a fresh run mints a new one).
func TestRunHarness_OutputRunConfigReplaysIdentically(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "captured.json")

	cmd1 := newTestHarnessCommand()
	must.NoError(cmd1.Flags().Set("output-runconfig", outPath))
	must.NoError(cmd1.Flags().Set("prompt", "captured prompt"))
	must.NoError(cmd1.Flags().Set("mode", "execution"))
	must.NoError(runHarness(cmd1, nil), "first run capture")

	captured, err := loadRunConfigFile(outPath)
	must.NoError(err)

	replayPath := filepath.Join(dir, "replay.json")
	cmd2 := newTestHarnessCommand()
	must.NoError(cmd2.Flags().Set("config", outPath))
	must.NoError(cmd2.Flags().Set("output-runconfig", replayPath))
	must.NoError(runHarness(cmd2, nil), "second run capture")
	replayed, err := loadRunConfigFile(replayPath)
	must.NoError(err)

	must.Equal(captured.Prompt, replayed.Prompt, "Prompt drifted on replay")
	must.Equal(captured.Mode, replayed.Mode, "Mode drifted on replay")

	if captured.RunID == replayed.RunID {
		t.Logf("RunIDs happen to match (clock resolution); harmless")
	}
}

// withPipedStdin swaps os.Stdin for a pipe, writes content to it, and
// closes the writer so EOF reaches the reader; restore is registered
// with t.Cleanup.
func withPipedStdin(t *testing.T, content string) {
	t.Helper()
	must := require.New(t)
	r, w, err := os.Pipe()
	must.NoError(err, "create pipe")
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
	_, writeErr := w.Write([]byte(content))
	closeErr := w.Close()
	must.NoError(writeErr, "write to pipe")
	must.NoError(closeErr, "close pipe writer")
}

// captureStdout swaps os.Stdout for a pipe and returns a function the
// caller invokes once the test action has completed to retrieve the
// captured bytes.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	must := require.New(t)
	r, w, err := os.Pipe()
	must.NoError(err, "create pipe")
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	return func() string {
		os.Stdout = orig
		_ = w.Close()
		out := <-done
		_ = r.Close()
		return out
	}
}

// minimalStdinRunConfig is the smallest RunConfig the runHarness stdin
// integration tests pipe through os.Stdin. ResolveAll still runs after
// the read, so every field ResolveAll (not just ResolveBase) requires
// must be set.
func minimalStdinRunConfig(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	timeout := 300
	cfg := types.RunConfig{
		RunID:  "from-stdin",
		Mode:   "planning",
		Prompt: "prompt from stdin",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://STDIN_KEY",
		},
		ModelRouter:     types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
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
	body, err := json.Marshal(cfg)
	must.NoError(err, "marshal stdin RunConfig")
	return string(body)
}

// writeConfigFile writes body to a new file in a temp directory and
// returns its path.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "cfg.json")
	must.NoError(os.WriteFile(path, []byte(body), 0o600), "write config file")
	return path
}

// TestRunHarness_StdinExplicitDashReadsConfig pins that
// `stirrup harness --config -` consumes a piped RunConfig from
// os.Stdin and resolves through ResolveAll into a writable capture.
func TestRunHarness_StdinExplicitDashReadsConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	withPipedStdin(t, minimalStdinRunConfig(t))
	getOut := captureStdout(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", "-"))
	must.NoError(cmd.Flags().Set("output-runconfig", "-"))

	runErr := runHarness(cmd, nil)
	out := getOut()

	must.NoError(runErr, "runHarness with --config - (stdout: %s)", out)

	var cfg types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &cfg), "captured output should be parseable JSON:\n%s", out)
	check.Equal("prompt from stdin", cfg.Prompt)
	check.Equal("secret://STDIN_KEY", cfg.Provider.APIKeyRef)
}

// TestRunHarness_StdinAutoDetectsPipe pins that a piped stdin (no
// --config flag) is auto-detected and treated as the base RunConfig,
// mirroring the canonical `run-config | harness` pipeline.
func TestRunHarness_StdinAutoDetectsPipe(t *testing.T) {
	must := require.New(t)
	withPipedStdin(t, minimalStdinRunConfig(t))
	getOut := captureStdout(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("output-runconfig", "-"))

	runErr := runHarness(cmd, nil)
	out := getOut()

	must.NoError(runErr, "runHarness with auto-stdin (stdout: %s)", out)

	var cfg types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &cfg), "captured output should be parseable JSON:\n%s", out)
	must.Equal("prompt from stdin", cfg.Prompt, "auto-detected stdin Prompt")
}

// TestRunHarness_StdinAndConfigFileAreAmbiguous pins that
// `--config <path>` alongside a non-TTY stdin fails loudly rather than
// silently picking a precedence.
func TestRunHarness_StdinAndConfigFileAreAmbiguous(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	path := writeConfigFile(t, minimalStdinRunConfig(t))

	withPipedStdin(t, minimalStdinRunConfig(t))

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", path))

	err := runHarness(cmd, nil)
	must.Error(err, "expected runHarness to reject --config <path> + piped stdin")
	must.ErrorContains(err, "ambiguous")
	must.ErrorContains(err, path)
	check.ErrorContains(err, "stdin")
}

// TestRunHarness_StdinDashWithoutPipeErrors pins that `--config -` with
// no piped stdin (the TTY / `go test` char-device shape) returns a
// non-nil error rather than blocking on a phantom read.
func TestRunHarness_StdinDashWithoutPipeErrors(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", "-"))

	err := runHarness(cmd, nil)
	must.Error(err, "expected runHarness to reject --config - with no piped stdin")
	must.ErrorContains(err, "--config -")
	check.Regexp("terminal|piped", err.Error(), "error should explain the missing pipe")
}

// TestRunHarness_EmptyAutoDetectedStdinFallsThroughToFlags pins that an
// auto-detected piped stdin that turns out to be empty (a closed
// anonymous pipe, e.g. a GitHub Actions shell step or `docker exec`
// without -i) is treated as "no piped config", letting the flag-only
// base build proceed instead of aborting with "input is empty".
func TestRunHarness_EmptyAutoDetectedStdinFallsThroughToFlags(t *testing.T) {
	must := require.New(t)
	withPipedStdin(t, "")
	getOut := captureStdout(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("prompt", "prompt from flags"))
	must.NoError(cmd.Flags().Set("output-runconfig", "-"))

	runErr := runHarness(cmd, nil)
	out := getOut()

	must.NoError(runErr, "empty auto-detected stdin must fall through to flags (stdout: %s)", out)

	var cfg types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &cfg), "captured output should be parseable JSON:\n%s", out)
	must.Equal("prompt from flags", cfg.Prompt, "Prompt should come from the flag, not stdin")
}

// TestRunHarness_EmptyStdinWithConfigFileIsNotAmbiguous pins that an
// empty piped stdin is not a competing base, so `--config <path>`
// alongside it succeeds rather than tripping the ambiguous-sources
// guard.
func TestRunHarness_EmptyStdinWithConfigFileIsNotAmbiguous(t *testing.T) {
	must := require.New(t)
	path := writeConfigFile(t, minimalStdinRunConfig(t))

	withPipedStdin(t, "")
	getOut := captureStdout(t)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", path))
	must.NoError(cmd.Flags().Set("output-runconfig", "-"))

	runErr := runHarness(cmd, nil)
	out := getOut()

	must.NoError(runErr, "empty stdin must not make --config ambiguous (stdout: %s)", out)

	var cfg types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &cfg), "captured output should be parseable JSON:\n%s", out)
	must.Equal("prompt from stdin", cfg.Prompt, "Prompt should come from the --config file")
}

// TestRunHarness_ExplicitDashWithEmptyPipeStillErrors guards that the
// empty-read downgrade applies only to auto-detection: when the operator
// explicitly names stdin via --config -, an empty pipe stays a hard
// "input is empty" error.
func TestRunHarness_ExplicitDashWithEmptyPipeStillErrors(t *testing.T) {
	must := require.New(t)
	withPipedStdin(t, "")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("config", "-"))

	err := runHarness(cmd, nil)
	must.Error(err, "expected --config - against an empty pipe to error")
	must.ErrorContains(err, "input is empty")
}

// TestRunHarness_OutputRunConfigBadPathErrors pins that an
// --output-runconfig path whose parent directory does not exist
// surfaces as a non-nil error mentioning the path.
func TestRunHarness_OutputRunConfigBadPathErrors(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	badPath := filepath.Join(t.TempDir(), "does-not-exist", "out.json")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("output-runconfig", badPath))
	must.NoError(cmd.Flags().Set("prompt", "test prompt"))

	err := runHarness(cmd, nil)
	must.Error(err, "expected OpenFile error for missing parent directory")
	must.ErrorContains(err, badPath)
	check.ErrorContains(err, "opening")
}

// failCloseWriter writes successfully but its Close returns the
// configured error. Used to drive writeAndCloseRunConfig's
// deferred-flush diagnostic path without depending on filesystem
// conditions.
type failCloseWriter struct {
	bytes.Buffer
	closeErr error
}

func (f *failCloseWriter) Close() error { return f.closeErr }

// TestWriteOutputRunConfig_PropagatesCloseError pins that a Close error from
// the captured-config writer (e.g. ENOSPC manifesting only at kernel flush
// time) propagates as a non-nil error wrapping the underlying cause.
func TestWriteOutputRunConfig_PropagatesCloseError(t *testing.T) {
	cfg := &types.RunConfig{
		RunID:        "x",
		Mode:         "planning",
		Provider:     types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://K"},
		ModelRouter:  types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		Transport:    types.TransportConfig{Type: "stdio"},
		Executor:     types.ExecutorConfig{Type: "local"},
		EditStrategy: types.EditStrategyConfig{Type: "multi"},
		MaxTurns:     5,
	}

	t.Run("close error surfaces", func(t *testing.T) {
		must := require.New(t)
		w := &failCloseWriter{closeErr: io.ErrClosedPipe}
		err := writeAndCloseRunConfig(w, "/tmp/captured.json", cfg)
		must.Error(err, "expected close error to propagate")
		must.ErrorIs(err, io.ErrClosedPipe)
		must.ErrorContains(err, "/tmp/captured.json")
	})

	t.Run("write error wins over close error", func(t *testing.T) {
		must := require.New(t)
		w := &failWriteCloseWriter{closeErr: io.ErrClosedPipe, writeErr: io.ErrShortWrite}
		err := writeAndCloseRunConfig(w, "/tmp/captured.json", cfg)
		must.Error(err, "expected write error to propagate")
		must.ErrorIs(err, io.ErrShortWrite, "write error should take precedence over close error")
	})
}

// failWriteCloseWriter fails on Write with writeErr and on Close with
// closeErr; used to assert the write-error precedence in
// writeAndCloseRunConfig.
type failWriteCloseWriter struct {
	closeErr error
	writeErr error
}

func (f *failWriteCloseWriter) Write(p []byte) (int, error) { return 0, f.writeErr }
func (f *failWriteCloseWriter) Close() error                { return f.closeErr }

// TestRunHarness_EnvVarConfigLoadsBase pins the integration-level
// guarantee that STIRRUP_CONFIG, set with no --config flag, is threaded
// through runHarness -> BuildRunConfig -> os.Getenv and consumed as the
// base RunConfig.
func TestRunHarness_EnvVarConfigLoadsBase(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	envCfg := types.RunConfig{
		RunID:  "from-env-integration",
		Mode:   "planning",
		Prompt: "prompt from STIRRUP_CONFIG",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://ENV_INTEGRATION_KEY",
		},
		ModelRouter:     types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
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
		LogLevel: "info",
	}
	timeout := 300
	envCfg.Timeout = &timeout
	body, err := json.Marshal(envCfg)
	must.NoError(err, "marshal env cfg")
	t.Setenv("STIRRUP_CONFIG", writeConfigFile(t, string(body)))

	getOut := captureStdout(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("output-runconfig", "-"))

	runErr := runHarness(cmd, nil)
	out := getOut()

	must.NoError(runErr, "runHarness with STIRRUP_CONFIG (stdout: %s)", out)

	var captured types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &captured), "captured output should be parseable JSON:\n%s", out)
	check.Equal("prompt from STIRRUP_CONFIG", captured.Prompt, "env-var base should load")
	check.Equal("secret://ENV_INTEGRATION_KEY", captured.Provider.APIKeyRef)
}

// captureStderr mirrors captureStdout for fd 2.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	must := require.New(t)
	r, w, err := os.Pipe()
	must.NoError(err, "create pipe")
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	return func() string {
		os.Stderr = orig
		_ = w.Close()
		out := <-done
		_ = r.Close()
		return out
	}
}

// outputModeRunTrace builds a fixed RunTrace the emitRunOutput tests
// reuse so each test exercises the same payload through a different
// mode. The fields are deliberately non-zero (turn count, token usage,
// duration) so a buggy mode that drops the payload is detectable.
func outputModeRunTrace() *types.RunTrace {
	started := time.Now()
	return &types.RunTrace{
		ID:          "run-output-test",
		StartedAt:   started,
		CompletedAt: started.Add(2 * time.Second),
		Turns:       4,
		Outcome:     "success",
		TokenUsage:  types.TokenUsage{Input: 1234, Output: 567},
	}
}

// TestEmitRunOutput_TextDefaultMatchesLegacyBehaviour pins that
// --output=text prints the stderr summary and emits nothing on stdout
// when the default (nil) resultSink is configured.
func TestEmitRunOutput_TextDefaultMatchesLegacyBehaviour(t *testing.T) {
	check := assert.New(t)
	rt := outputModeRunTrace()
	cfg := &types.RunConfig{} // ResultSink nil ≡ NoneSink

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "text")
	stdout := stdoutDone()
	stderr := stderrDone()

	check.Contains(stderr, "--- Run complete ---", "stderr should contain the legacy summary header")
	check.Contains(stderr, "Outcome: success", "stderr should contain the Outcome line")
	check.NotContains(stdout, "STIRRUP_RESULT", "stdout should be empty when no stdout-json sink is configured")
}

// TestEmitRunOutput_JSONEmitsSingleStdoutLine pins that --output=json
// writes exactly one STIRRUP_RESULT line on stdout, parseable as
// RunResult, and prints nothing on stderr.
func TestEmitRunOutput_JSONEmitsSingleStdoutLine(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	rt := outputModeRunTrace()
	cfg := &types.RunConfig{} // ResultSink nil ≡ NoneSink

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "json")
	stdout := stdoutDone()
	stderr := stderrDone()

	check.NotContains(stderr, "--- Run complete ---", "--output=json should not print the legacy summary")
	must.True(strings.HasPrefix(stdout, "STIRRUP_RESULT "), "stdout should start with STIRRUP_RESULT sentinel, got: %q", stdout)
	check.Equal(1, strings.Count(stdout, "STIRRUP_RESULT "), "expected exactly one STIRRUP_RESULT line, got: %q", stdout)

	payload := strings.TrimSpace(strings.TrimPrefix(stdout, "STIRRUP_RESULT "))
	var got types.RunResult
	must.NoError(json.Unmarshal([]byte(payload), &got), "STIRRUP_RESULT payload should parse as RunResult:\n%s", payload)
	check.Equal("run-output-test", got.RunID)
	check.Equal("success", got.Outcome)
	check.Equal(4, got.Turns)
}

// TestEmitRunOutput_NoneSuppressesBoth pins that --output=none produces
// no post-run summary on either stream.
func TestEmitRunOutput_NoneSuppressesBoth(t *testing.T) {
	check := assert.New(t)
	rt := outputModeRunTrace()
	cfg := &types.RunConfig{}

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "none")
	stdout := stdoutDone()
	stderr := stderrDone()

	check.Empty(stderr, "stderr should be empty when --output=none")
	check.Empty(stdout, "stdout should be empty when --output=none")
}

// TestEmitRunOutput_JSONWithStdoutJSONSinkEmitsOnce pins the
// deduplication rule: --output=json and resultSink.type=stdout-json
// together must produce exactly one STIRRUP_RESULT line, not two.
func TestEmitRunOutput_JSONWithStdoutJSONSinkEmitsOnce(t *testing.T) {
	check := assert.New(t)
	rt := outputModeRunTrace()
	cfg := &types.RunConfig{
		ResultSink: &types.ResultSinkConfig{Type: "stdout-json"},
	}

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "json")
	stdout := stdoutDone()
	stderr := stderrDone()

	check.NotContains(stderr, "--- Run complete ---", "dedup case should not print the legacy summary")
	check.Equal(1, strings.Count(stdout, "STIRRUP_RESULT "), "expected exactly one STIRRUP_RESULT line, got: %q", stdout)
}

// TestEmitRunOutput_TextWithStdoutJSONSinkUnchanged pins that when
// --output is left at "text" and resultSink.type=stdout-json is
// configured, the stderr summary prints and the configured sink still
// emits its STIRRUP_RESULT line.
func TestEmitRunOutput_TextWithStdoutJSONSinkUnchanged(t *testing.T) {
	check := assert.New(t)
	rt := outputModeRunTrace()
	cfg := &types.RunConfig{
		ResultSink: &types.ResultSinkConfig{Type: "stdout-json"},
	}

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "text")
	stdout := stdoutDone()
	stderr := stderrDone()

	check.Contains(stderr, "--- Run complete ---", "stderr should contain the legacy summary on --output=text")
	check.Equal(1, strings.Count(stdout, "STIRRUP_RESULT "), "expected exactly one STIRRUP_RESULT line from the configured sink, got: %q", stdout)
}

// TestEmitRunOutput_JSONPartialTraceEmitsCancellationOutcome pins that a
// run cancelled mid-flight produces a partial RunResult with the
// cancellation outcome rather than nothing.
func TestEmitRunOutput_JSONPartialTraceEmitsCancellationOutcome(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	started := time.Now()
	rt := &types.RunTrace{
		ID:          "run-cancelled",
		StartedAt:   started,
		CompletedAt: started.Add(750 * time.Millisecond),
		Turns:       1,
		Outcome:     "cancelled",
	}
	cfg := &types.RunConfig{}

	stdoutDone := captureStdout(t)
	emitRunOutput(context.Background(), cfg, rt, "json")
	stdout := stdoutDone()

	must.True(strings.HasPrefix(stdout, "STIRRUP_RESULT "), "stdout should start with STIRRUP_RESULT sentinel even on cancellation, got: %q", stdout)
	payload := strings.TrimSpace(strings.TrimPrefix(stdout, "STIRRUP_RESULT "))
	var got types.RunResult
	must.NoError(json.Unmarshal([]byte(payload), &got), "STIRRUP_RESULT payload should parse as RunResult on cancellation:\n%s", payload)
	check.Equal("cancelled", got.Outcome)
}

// TestEmitRunOutput_EmptyModeMatchesText pins that the runJob
// entry-point's outputMode="" (it never threads --output) falls through
// to the "print summary + emit configured sink" text behaviour.
func TestEmitRunOutput_EmptyModeMatchesText(t *testing.T) {
	check := assert.New(t)
	rt := outputModeRunTrace()
	cfg := &types.RunConfig{}

	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "")
	stderr := stderrDone()

	check.Contains(stderr, "--- Run complete ---", "empty outputMode should default to text behaviour")
}

// TestPrintRunSummary_NilTraceDoesNotPanic pins the nil guard: a nil
// RunTrace would otherwise dereference at the Outcome field and crash
// the process before any structured output is emitted.
func TestPrintRunSummary_NilTraceDoesNotPanic(t *testing.T) {
	must := require.New(t)
	stderrDone := captureStderr(t)
	must.NotPanics(func() { printRunSummary(nil) })
	stderr := stderrDone()
	must.Contains(stderr, "no trace", "stderr should describe the nil-trace condition")
}

// TestPrintRunSummary_HooksLinePrintedWhenPresent pins that the summary
// counts only executed (non-Skipped) HookResults entries, treating only
// those with a non-empty Error as failures.
func TestPrintRunSummary_HooksLinePrintedWhenPresent(t *testing.T) {
	must := require.New(t)
	rt := outputModeRunTrace()
	rt.HookResults = []types.HookExecution{
		{Phase: "preRun", Index: 0, Command: "true"},
		{Phase: "postRun", Index: 0, Command: "false", Error: "exit code 1"},
		{Phase: "postRun", Index: 1, Command: "true", Skipped: true},
	}

	stderrDone := captureStderr(t)
	printRunSummary(rt)
	stderr := stderrDone()

	must.Contains(stderr, "Hooks: 2 run, 1 failed")
}

func TestPrintRunSummary_TokenBreakdownShownWhenReported(t *testing.T) {
	t.Run("bare counts without breakdown", func(t *testing.T) {
		must := require.New(t)
		stderrDone := captureStderr(t)
		printRunSummary(outputModeRunTrace())
		stderr := stderrDone()

		must.Contains(stderr, "Tokens: 1234 in / 567 out\n")
	})

	t.Run("breakdown when cache or reasoning tokens are reported", func(t *testing.T) {
		must := require.New(t)
		rt := outputModeRunTrace()
		rt.TokenUsage = types.TokenUsage{Input: 1234, Output: 567, CacheRead: 1000, CacheWrite: 20, Reasoning: 12}
		stderrDone := captureStderr(t)
		printRunSummary(rt)
		stderr := stderrDone()

		must.Contains(stderr, "Tokens: 1234 in (cache read 1000, cache write 20) / 567 out (reasoning 12)\n")
	})
}

// TestPrintRunSummary_HooksLineOmittedWhenAbsent pins that a hookless
// run's summary carries no "Hooks:" line at all.
func TestPrintRunSummary_HooksLineOmittedWhenAbsent(t *testing.T) {
	must := require.New(t)
	rt := outputModeRunTrace()

	stderrDone := captureStderr(t)
	printRunSummary(rt)
	stderr := stderrDone()

	must.NotContains(stderr, "Hooks:")
}

// TestEmitRunOutput_CancelledContextStillEmits pins that emitRunOutput
// still produces the STIRRUP_RESULT line under --output=json when
// passed a pre-cancelled context, guarding a future remote sink that
// might honour ctx and otherwise drop signal-cancelled runs silently.
func TestEmitRunOutput_CancelledContextStillEmits(t *testing.T) {
	must := require.New(t)
	rt := outputModeRunTrace()
	cfg := &types.RunConfig{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stdoutDone := captureStdout(t)
	emitRunOutput(ctx, cfg, rt, "json")
	stdout := stdoutDone()

	must.Truef(strings.HasPrefix(stdout, "STIRRUP_RESULT "), "stdout should start with STIRRUP_RESULT, got: %q", stdout)
}

// TestEmitRunOutput_TextWithNilTracePrintsNoTrace pins the
// emitRunOutput dispatch path under --output=text when the loop
// produced no trace at all. The text branch must surface the nil-trace
// diagnostic on stderr rather than panicking.
func TestEmitRunOutput_TextWithNilTracePrintsNoTrace(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg := &types.RunConfig{}
	stderrDone := captureStderr(t)
	check.NotPanics(func() {
		emitRunOutput(context.Background(), cfg, nil, "text")
	})
	stderr := stderrDone()

	must.Contains(stderr, "no trace", "stderr should describe nil-trace condition")
}

// TestEmitRunOutput_JSONWithNilTraceLogsWarning pins that a nil trace
// under --output=json produces a structurally valid STIRRUP_RESULT line
// with Outcome="internal-error", and that emitRunOutput's slog.Warn
// surfaces the diagnostic in process logs.
func TestEmitRunOutput_JSONWithNilTraceLogsWarning(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cfg := &types.RunConfig{}
	stdoutDone := captureStdout(t)
	emitRunOutput(context.Background(), cfg, nil, "json")
	stdout := stdoutDone()

	must.Truef(strings.HasPrefix(stdout, "STIRRUP_RESULT "), "stdout should start with STIRRUP_RESULT, got: %q", stdout)
	payload := strings.TrimSpace(strings.TrimPrefix(stdout, "STIRRUP_RESULT "))
	var got types.RunResult
	must.NoError(json.Unmarshal([]byte(payload), &got), "payload: %s", payload)
	check.Equal("internal-error", got.Outcome)
	check.Contains(logBuf.String(), "nil RunTrace", "expected slog.Warn about nil RunTrace")
}

// stubResultSink records every Emit invocation so tests can assert that
// the configured resultSink fires, unlike resultsink.NoneSink which
// discards silently.
type stubResultSink struct {
	calls []types.RunResult
}

func (s *stubResultSink) Emit(_ context.Context, r types.RunResult) error {
	s.calls = append(s.calls, r)
	return nil
}

// installStubResultSink rewires the newResultSink seam to return the
// supplied stub for the duration of the test.
func installStubResultSink(t *testing.T, stub *stubResultSink) {
	t.Helper()
	prev := newResultSink
	t.Cleanup(func() { newResultSink = prev })
	newResultSink = func(_ *types.ResultSinkConfig) (resultsinkInterface, error) {
		return stub, nil
	}
}

// resultsinkInterface is a local alias of resultsink.ResultSink, kept
// so installStubResultSink's signature matches newResultSink's.
type resultsinkInterface = resultsink.ResultSink

// TestEmitRunOutput_JSONWithNonStdoutSinkAlsoFires pins the
// forward-compatibility surface for a future gcp-pubsub / gcs sink
// under --output=json: the flag's STIRRUP_RESULT line goes to stdout,
// and the configured sink also fires.
func TestEmitRunOutput_JSONWithNonStdoutSinkAlsoFires(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	stub := &stubResultSink{}
	installStubResultSink(t, stub)

	rt := outputModeRunTrace()

	cfg := &types.RunConfig{ResultSink: &types.ResultSinkConfig{Type: "gcp-pubsub"}}

	stdoutDone := captureStdout(t)
	emitRunOutput(context.Background(), cfg, rt, "json")
	stdout := stdoutDone()

	check.Truef(strings.HasPrefix(stdout, "STIRRUP_RESULT "), "stdout should still carry STIRRUP_RESULT under --output=json, got: %q", stdout)
	must.Len(stub.calls, 1, "configured sink should fire exactly once")
	check.Equal(rt.ID, stub.calls[0].RunID)
}

// TestEmitRunOutput_NoneWithNonStdoutSinkStillFires pins that under
// --output=none, the stderr summary and stdout STIRRUP_RESULT are both
// suppressed, but a sink targeting a non-stdout destination still fires.
func TestEmitRunOutput_NoneWithNonStdoutSinkStillFires(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	stub := &stubResultSink{}
	installStubResultSink(t, stub)

	rt := outputModeRunTrace()
	cfg := &types.RunConfig{ResultSink: &types.ResultSinkConfig{Type: "gcp-pubsub"}}

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "none")
	stdout := stdoutDone()
	stderr := stderrDone()

	check.Empty(stdout, "stdout should be empty under --output=none")
	check.Empty(stderr, "stderr should be empty under --output=none")
	must.Len(stub.calls, 1, "configured sink should fire exactly once")
	check.Equal(rt.ID, stub.calls[0].RunID)
}

// TestEmitRunOutput_UnrecognisedModeLogsAndDefaultsToText pins that an
// unrecognised mode reached at this layer surfaces a diagnostic and
// falls through to the text behaviour rather than silently dropping the
// summary.
func TestEmitRunOutput_UnrecognisedModeLogsAndDefaultsToText(t *testing.T) {
	check := assert.New(t)
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	rt := outputModeRunTrace()
	cfg := &types.RunConfig{}

	stderrDone := captureStderr(t)
	emitRunOutput(context.Background(), cfg, rt, "yaml")
	stderr := stderrDone()

	check.Contains(stderr, "--- Run complete ---", "unrecognised mode should fall through to text")
	check.Contains(logBuf.String(), "unrecognised mode", "expected slog.Warn about unrecognised mode")
}

// TestValidateOutputMode_AcceptsClosedSet pins the closed three-value
// set surfaced via --output.
func TestValidateOutputMode_AcceptsClosedSet(t *testing.T) {
	for _, mode := range []string{"text", "json", "none"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			must.NoError(validateOutputMode(mode))
		})
	}
}

// TestValidateOutputMode_RejectsUnsupported pins the operator-facing
// failure mode: a typo at the CLI surfaces as a clear error rather
// than being silently swallowed and ignored at the dispatch site.
func TestValidateOutputMode_RejectsUnsupported(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{name: "empty", mode: ""},
		{name: "json5", mode: "json5"},
		{name: "yaml", mode: "yaml"},
		{name: "uppercase text", mode: "TEXT"},
		{name: "jsonl", mode: "jsonl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Error(validateOutputMode(tc.mode))
		})
	}
}

// TestRunHarness_OutputFlagRejectsInvalidValue is the end-to-end
// integration test for the closed-set validation: an invalid --output
// value must surface as an error from runHarness rather than being
// silently dropped.
func TestRunHarness_OutputFlagRejectsInvalidValue(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("prompt", "test prompt"), "set prompt")
	must.NoError(cmd.Flags().Set("output", "yaml"), "set output")

	err := runHarness(cmd, nil)
	must.ErrorContains(err, "--output")
}

// TestRunHarness_OutputFlagRejectsInvalidValueBeforeOutputRunconfig pins
// that --output is validated before the --output-runconfig dry-run
// branch exits, so `--output-runconfig=- --output=yaml` cannot return 0
// and capture a config the operator cannot replay.
func TestRunHarness_OutputFlagRejectsInvalidValueBeforeOutputRunconfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("prompt", "test prompt"), "set prompt")
	must.NoError(cmd.Flags().Set("output", "yaml"), "set output")
	must.NoError(cmd.Flags().Set("output-runconfig", "-"), "set output-runconfig")

	// Stdout is captured so that a dry-run config written before the
	// --output check fails the Empty assertion below.
	getOut := captureStdout(t)
	err := runHarness(cmd, nil)
	stdout := getOut()

	must.Error(err, "runHarness should reject --output=yaml even with --output-runconfig=-")
	must.ErrorContains(err, "--output")
	check.Empty(stdout, "--output-runconfig=- should not write to stdout when --output is invalid")
}

// TestPrintHarnessUsageHint_ColorEmitsAnsi pins that the grouped hint
// uses ANSI for headings + dim example values when colour is enabled.
// Mirrors TestTraceShow_AlwaysEmitsAnsi: the renderer takes an explicit
// color bool so the test pins the formatting path without a PTY.
func TestPrintHarnessUsageHint_ColorEmitsAnsi(t *testing.T) {
	check := assert.New(t)
	var buf bytes.Buffer
	printHarnessUsageHint(&buf, true)
	out := buf.String()
	check.Contains(out, "\x1b[", "colour-enabled hint must contain ANSI escapes")
	check.Contains(out, ansiBold, "colour-enabled hint must bold its headings")
	for _, want := range []string{"USAGE", "REQUIRED", "RUN SHAPE", "PROVIDER", "CONFIGURATION", "EXAMPLES", "--prompt", "--mode"} {
		check.Contains(out, want)
	}
}

// TestPrintHarnessUsageHint_PlainStripsAnsi pins the plain path: with
// colour disabled (non-TTY, NO_COLOR, or a piped 2>&1 | cat) the hint
// carries no ANSI escapes but keeps the same grouped headings.
func TestPrintHarnessUsageHint_PlainStripsAnsi(t *testing.T) {
	check := assert.New(t)
	var buf bytes.Buffer
	printHarnessUsageHint(&buf, false)
	out := buf.String()
	check.NotContains(out, "\x1b[", "colour-disabled hint must not contain ANSI escapes")
	for _, want := range []string{"USAGE", "REQUIRED", "RUN SHAPE", "PROVIDER", "CONFIGURATION", "EXAMPLES"} {
		check.Contains(out, want)
	}
}

// TestResolvePromptForRun_InteractiveReturnsHintSentinel pins the TTY
// branch of the prompt-required gate: when stderr is interactive and no
// source supplied a prompt, resolvePromptForRun returns
// errPromptHintRequested so runHarness can show the grouped hint and
// exit 0.
func TestResolvePromptForRun_InteractiveReturnsHintSentinel(t *testing.T) {
	must := require.New(t)
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return true }
	defer func() { stderrIsInteractive = orig }()

	cmd := newTestHarnessCommand()
	cfg := &types.RunConfig{}
	err := resolvePromptForRun(cmd, cfg)
	must.ErrorIs(err, errPromptHintRequested)
}

// TestResolvePromptForRun_NonInteractiveReturnsOpaqueError pins the
// non-TTY branch: scripted callers keep the verbatim "prompt is
// required" error and a non-zero exit so log aggregators are not
// flooded with the multi-line hint.
func TestResolvePromptForRun_NonInteractiveReturnsOpaqueError(t *testing.T) {
	must := require.New(t)
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return false }
	defer func() { stderrIsInteractive = orig }()

	cmd := newTestHarnessCommand()
	cfg := &types.RunConfig{}
	err := resolvePromptForRun(cmd, cfg)
	must.Error(err)
	must.NotErrorIs(err, errPromptHintRequested, "non-TTY caller must not get the hint sentinel")
	must.ErrorContains(err, "prompt is required")
}

// TestResolvePromptForRun_InteractiveWithStirrupPromptNoHint pins that
// even on an interactive terminal, a resolvable prompt short-circuits
// before the hint gate: STIRRUP_PROMPT fills cfg.Prompt and the hint
// sentinel must not fire.
func TestResolvePromptForRun_InteractiveWithStirrupPromptNoHint(t *testing.T) {
	must := require.New(t)
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return true }
	defer func() { stderrIsInteractive = orig }()
	t.Setenv("STIRRUP_PROMPT", "task from env")

	cmd := newTestHarnessCommand()
	cfg := &types.RunConfig{}
	err := resolvePromptForRun(cmd, cfg)
	must.NoError(err, "STIRRUP_PROMPT on a TTY should not trigger the hint")
	must.Equal("task from env", cfg.Prompt)
}

// TestResolvePromptForRun_InteractiveWithConfigPromptNoHint pins that the
// hint sentinel must not fire on an interactive terminal when a prompt
// supplied by a --config file's `prompt` field is already on cfg.Prompt.
func TestResolvePromptForRun_InteractiveWithConfigPromptNoHint(t *testing.T) {
	must := require.New(t)
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return true }
	defer func() { stderrIsInteractive = orig }()
	t.Setenv("STIRRUP_PROMPT", "")

	cmd := newTestHarnessCommand()

	cfg := &types.RunConfig{Prompt: "prompt-from-config"}
	err := resolvePromptForRun(cmd, cfg)
	must.NoError(err, "a config-supplied prompt on a TTY should not trigger the hint")
	must.Equal("prompt-from-config", cfg.Prompt)
}

// TestRunHarness_BareInteractivePrintsHintAndExitsZero drives the full
// runHarness path on a TTY with no prompt: it must print the grouped
// hint to the command's stderr and return nil (exit 0).
func TestRunHarness_BareInteractivePrintsHintAndExitsZero(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return true }
	defer func() { stderrIsInteractive = orig }()

	cmd := newTestHarnessCommand()
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	// runHarness decides colour from cmd.ErrOrStderr(). The injected buffer
	// is not an *os.File, so the hint is plain regardless of the host's TTY
	// or NO_COLOR state. The colour branch is covered by
	// TestPrintHarnessUsageHint_ColorEmitsAnsi.

	getOut := captureStdout(t)
	err := runHarness(cmd, nil)
	stdout := getOut()

	must.NoError(err, "runHarness on a TTY with no prompt should exit 0")
	hint := errBuf.String()
	check.Contains(hint, "stirrup harness")
	check.Contains(hint, "USAGE")
	check.NotContains(hint, "\x1b[", "a non-*os.File writer must never receive ANSI escapes")
	check.Empty(stdout, "grouped hint must go to stderr, not stdout")
}

// TestRunHarness_BareNonInteractiveKeepsError pins the scripted path
// end-to-end: with a non-TTY stderr and no prompt, runHarness returns
// the opaque prompt-required error (non-zero exit) and prints no hint.
func TestRunHarness_BareNonInteractiveKeepsError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return false }
	defer func() { stderrIsInteractive = orig }()

	cmd := newTestHarnessCommand()
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	err := runHarness(cmd, nil)
	must.Error(err)
	must.ErrorContains(err, "prompt is required")
	check.NotContains(errBuf.String(), "USAGE", "non-TTY path must not print the grouped hint")
}
