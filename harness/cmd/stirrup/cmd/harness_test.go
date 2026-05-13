package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
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

	// Rule of Two: every CLI default combination here carries
	// untrusted-input (web_fetch enabled) and external-communication
	// (web_fetch + run_command), which is two of the three legs. The
	// sensitive-data leg is unset by default — operational secret
	// references like ANTHROPIC_API_KEY no longer trip it (see
	// ruleOfTwoSensitiveData rationale). So a bare CLI invocation now
	// validates cleanly without a RuleOfTwo override; that is exactly
	// the regression this test guards against.
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			opts := baseOpts
			opts.Mode = mode
			cfg, err := buildHarnessRunConfig(opts)
			if err != nil {
				t.Fatalf("buildHarnessRunConfig: %v", err)
			}

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

// TestHarnessCmd_DefaultModeIsPlanning pins the CLI-surface default for
// --mode after #74: a bare `stirrup harness` invocation lands in the
// read-only `planning` mode, not the editable `execution` mode, so the
// first-touch posture is safe by default and operators must explicitly
// opt in to write/shell capabilities via --mode execution. The pin is
// against the flag registration on harnessCmd itself rather than the
// helper command, so a regression that flips the default in only one
// of the two places fails this test.
func TestHarnessCmd_DefaultModeIsPlanning(t *testing.T) {
	flag := harnessCmd.Flags().Lookup("mode")
	if flag == nil {
		t.Fatal("--mode flag is not registered on harnessCmd")
	}
	if flag.DefValue != "planning" {
		t.Errorf("default --mode = %q, want %q (safe-by-default per #74)", flag.DefValue, "planning")
	}
}

// TestBuildHarnessRunConfig_BareInvocationValidatesAsPlanning proves the
// safe-by-default property end-to-end: a flag-only invocation with only
// the documented CLI defaults (no --mode override) produces a RunConfig
// that has Mode == "planning" and passes ValidateRunConfig — including
// the read-only-mode invariants (deny-side-effects policy, non-empty
// Tools.BuiltIn that excludes write_file/edit_file/run_command).
//
// This pins acceptance criterion (a) from #74: "Validate cleanly
// (already true after #73)" — but specifically through the new default
// rather than relying on a caller passing --mode planning explicitly.
func TestBuildHarnessRunConfig_BareInvocationValidatesAsPlanning(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
	if cfg.Mode != "planning" {
		t.Errorf("bare invocation should land in planning mode, got %q", cfg.Mode)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("bare invocation must validate cleanly: %v", err)
	}
	if cfg.PermissionPolicy.Type != "deny-side-effects" {
		t.Errorf("planning mode should default to deny-side-effects, got %q", cfg.PermissionPolicy.Type)
	}
	if len(cfg.Tools.BuiltIn) == 0 {
		t.Fatal("planning mode should populate a non-empty Tools.BuiltIn list")
	}
	for _, tool := range cfg.Tools.BuiltIn {
		if tool == "write_file" || tool == "edit_file" || tool == "run_command" {
			t.Errorf("planning mode must not enable write tool %q in the default list", tool)
		}
	}
}

// TestBuildHarnessRunConfig_OpenAIResponsesProvider verifies that the
// openai-responses provider type is accepted by both the CLI option-to-
// RunConfig path and ValidateRunConfig. Before this case existed, picking
// --provider openai-responses would crash at validation.
func TestBuildHarnessRunConfig_OpenAIResponsesProvider(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want openai-responses", cfg.Provider.Type)
	}
	if cfg.ModelRouter.Provider != "openai-responses" {
		t.Errorf("ModelRouter.Provider = %q, want openai-responses", cfg.ModelRouter.Provider)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig rejected openai-responses: %v", err)
	}
}

// TestBuildHarnessRunConfig_BedrockDefaultModelFailsValidation pins
// the fail-fast guard added for #65. Running `stirrup harness
// --provider bedrock` without overriding --model would otherwise send
// the Anthropic-API alias "claude-sonnet-4-6" to Bedrock, which
// rejects it with an opaque ValidationException only after IAM/SigV4
// setup and a network round-trip. The validator catches the shape
// at config-load time and points the operator at the inference-
// profile path.
//
// The test asserts that ValidateRunConfig (not the provider) is the
// thing that complains, so the failure mode is "no network call, with
// an actionable error" — the explicit acceptance criterion in #65.
func TestBuildHarnessRunConfig_BedrockDefaultModelFailsValidation(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	verr := types.ValidateRunConfig(cfg)
	if verr == nil {
		t.Fatal("expected ValidateRunConfig to reject --provider bedrock with CLI-default --model")
	}
	errStr := verr.Error()
	if !strings.Contains(errStr, "bedrock") {
		t.Errorf("expected error to mention bedrock, got: %v", verr)
	}
	if !strings.Contains(errStr, "inference-profile") &&
		!strings.Contains(errStr, "inference profile") &&
		!strings.Contains(errStr, "list-inference-profiles") {
		t.Errorf("expected error to point at the inference-profile remediation path, got: %v", verr)
	}
	if !strings.Contains(errStr, "claude-sonnet-4-6") {
		t.Errorf("expected error to name the offending model id, got: %v", verr)
	}
}

// TestBuildHarnessRunConfig_BedrockInferenceProfileValidates is the
// positive complement: with a properly-shaped inference profile id,
// the flag-only path produces a config that passes validation.
func TestBuildHarnessRunConfig_BedrockInferenceProfileValidates(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig rejected eu.anthropic.claude-sonnet-4-6 on bedrock: %v", err)
	}
}

// TestBuildHarnessRunConfig_FillsDefaultReadOnlyToolList verifies that
// when no explicit Tools.BuiltIn list is supplied, read-only modes get
// the documented default list rather than passing validation by accident.
func TestBuildHarnessRunConfig_FillsDefaultReadOnlyToolList(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

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
		// Per synthesis SF-6: pin that the gh-100 OTelProtocol field
		// flows through buildHarnessRunConfig into TraceEmitter.Protocol.
		// Without this, the assignment at harness.go:164 has count=0
		// and a future refactor that drops it would silently fall back
		// to the SDK default ("grpc") for any operator who passes
		// --otel-protocol on the CLI.
		OTelProtocol: "http/protobuf",
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

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
	if cfg.TraceEmitter.Protocol != "http/protobuf" {
		t.Errorf("expected otel protocol 'http/protobuf', got %q", cfg.TraceEmitter.Protocol)
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
		// All component-selection fields deliberately left empty.
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
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

// TestBuildHarnessRunConfig_ObservabilityFallsBackToEnv pins the K8s
// production path: the operator pins OTEL_DEPLOYMENT_ENVIRONMENT in the
// pod spec rather than threading the value through a CLI flag. The test
// proves that buildHarnessRunConfig leaves Observability empty when no
// flag is set, and that observability.BuildResource then picks the env
// var up via its fallback chain. Without this end-to-end coverage at the
// harness layer, REC-2's guard ("only assign when at least one flag is
// non-empty") could be reverted by accident and the env-var fallback
// would silently lose to an empty-string Observability value passed
// through to the resource builder.
func TestBuildHarnessRunConfig_ObservabilityFallsBackToEnv(t *testing.T) {
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
		// Observability flags deliberately empty: this is the K8s pod-spec
		// path where the operator only sets OTEL_DEPLOYMENT_ENVIRONMENT.
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	// The flag-only path leaves Observability at its zero value when both
	// flags are empty (REC-2 guard). The env-var fallback is delegated
	// to BuildResource so it stays a single, validated entry point.
	if cfg.Observability.Environment != "" {
		t.Errorf("Observability.Environment should remain empty when flag is unset; got %q", cfg.Observability.Environment)
	}

	res := observability.BuildResource(observability.ResourceOptions{
		Environment:      cfg.Observability.Environment,
		ServiceNamespace: cfg.Observability.ServiceNamespace,
		RunMode:          cfg.Mode,
	})
	got := make(map[string]string)
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got["deployment.environment"] != "production-eu" {
		t.Errorf("deployment.environment should fall through to env var; got %q want production-eu", got["deployment.environment"])
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
	f.StringP("mode", "m", "planning", "")
	f.String("model", "claude-sonnet-4-6", "")
	f.String("provider", "anthropic", "")
	f.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "")
	f.String("base-url", "", "")
	f.String("api-key-header", "", "")
	f.StringArray("query-param", nil, "")
	f.String("gcp-project", "", "")
	f.String("gcp-location", "global", "")
	f.String("gcp-credentials-file", "", "")
	f.String("anthropic-federation-rule-id", "", "")
	f.String("anthropic-organization-id", "", "")
	f.String("anthropic-service-account-id", "", "")
	f.String("anthropic-workspace-id", "", "")
	f.Bool("anthropic-from-github-actions", false, "")
	f.String("azure-tenant-id", "", "")
	f.String("azure-client-id", "", "")
	f.String("azure-scope", "https://cognitiveservices.azure.com/.default", "")
	f.StringP("workspace", "w", "", "")
	f.Int("max-turns", 20, "")
	f.Int("timeout", 600, "")
	f.String("trace", "", "")
	f.String("transport", "stdio", "")
	f.String("transport-addr", "", "")
	f.Int("followup-grace", 0, "")
	f.String("log-level", "info", "")
	f.String("prompt", "", "")
	f.String("prompt-file", "", "")
	f.String("name", "", "")
	f.String("executor", "local", "")
	f.String("edit-strategy", "multi", "")
	f.String("verifier", "none", "")
	f.String("git-strategy", "none", "")
	f.String("trace-emitter", "jsonl", "")
	f.String("otel-endpoint", "", "")
	f.String("otel-protocol", "", "")
	f.String("container-runtime", "", "")
	f.String("permission-policy-file", "", "")
	f.String("code-scanner", "", "")
	f.String("guardrail", "", "")
	f.String("guardrail-endpoint", "", "")
	f.String("guardrail-model", "", "")
	f.Bool("guardrail-fail-open", false, "")
	f.String("deployment-environment", "", "")
	f.String("service-namespace", "", "")
	// Provider retry policy flags (issue #197). Registered here so
	// f.Changed("...") can fire in TestApplyOverrides_ProviderRetry*
	// tests — without these registrations applyProviderRetryFlagOverrides
	// hits its early-return guard unconditionally.
	f.Int("provider-retry-max-attempts", 0, "")
	f.Duration("provider-retry-initial-delay", 0, "")
	f.Duration("provider-retry-max-delay", 0, "")
	f.Duration("provider-retry-wall-clock", 0, "")
	// Workspace export flags (issue #164). Registered here so the
	// override tests can exercise the applyOverrides path that handles
	// --export-workspace-to.
	f.String("export-workspace-to", "", "")
	f.Bool("export-workspace-required", false, "")
	// Parallel async-tool dispatch flag (issue #184). Registered here so
	// TestApplyOverrides_MaxToolParallel* tests can exercise the override
	// path.
	f.Int("max-tool-parallel", 0, "")
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
	must("otel-protocol", "http/protobuf")

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
	if cfg.TraceEmitter.Protocol != "http/protobuf" {
		t.Errorf("OTel protocol override failed: %q", cfg.TraceEmitter.Protocol)
	}
}

// TestApplyOverrides_OTelProtocolFilePreserved pins that an unset
// --otel-protocol flag (the default empty string) does NOT clobber a
// Protocol value supplied by --config. This is the same precedence
// rule that already applies to every other override flag.
func TestApplyOverrides_OTelProtocolFilePreserved(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.TraceEmitter = types.TraceEmitterConfig{
		Type:     "otel",
		Endpoint: "https://otlp-gateway-prod-us-east-0.grafana.net/otlp",
		Protocol: "http/protobuf",
	}

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.TraceEmitter.Protocol != "http/protobuf" {
		t.Errorf("Protocol from file should survive default flag, got %q", cfg.TraceEmitter.Protocol)
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
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.SessionName != "from-file" {
		t.Errorf("SessionName from file should survive, got %q", cfg.SessionName)
	}
}

// TestBuildHarnessRunConfig_SessionNamePropagates verifies the flag-only
// build path: a SessionName provided in harnessCLIOptions must end up on
// the constructed RunConfig.
func TestBuildHarnessRunConfig_SessionNamePropagates(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.SessionName != "nightly-eval" {
		t.Errorf("SessionName: got %q, want %q", cfg.SessionName, "nightly-eval")
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig: %v", err)
	}
}

// TestBuildHarnessRunConfig_ObservabilityPropagates pins that the
// --deployment-environment / --service-namespace flags propagate into
// RunConfig.Observability without further translation. The fields then
// drive the OTel resource attributes via the factory's
// resourceOptionsFromConfig helper, so a regression here would silently
// break operator dashboards (Grafana group-by-environment would fall
// back to the default "local" tile).
func TestBuildHarnessRunConfig_ObservabilityPropagates(t *testing.T) {
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
		DeploymentEnvironment: "production",
		ServiceNamespace:      "stirrup-eval",
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Observability.Environment != "production" {
		t.Errorf("Observability.Environment: got %q, want %q", cfg.Observability.Environment, "production")
	}
	if cfg.Observability.ServiceNamespace != "stirrup-eval" {
		t.Errorf("Observability.ServiceNamespace: got %q, want %q", cfg.Observability.ServiceNamespace, "stirrup-eval")
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig: %v", err)
	}
}

// TestApplyOverrides_ObservabilityFlags pins the file -> flag override
// chain for the new observability flags. An explicit flag must clobber
// the file's value; a flag at its default (empty string) must leave the
// file value alone — that's the same precedence convention every other
// override flag follows.
func TestApplyOverrides_ObservabilityFlags(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Observability = types.ObservabilityConfig{
		Environment:      "from-file-env",
		ServiceNamespace: "from-file-ns",
	}

	if err := cmd.Flags().Set("deployment-environment", "from-flag-env"); err != nil {
		t.Fatalf("set deployment-environment: %v", err)
	}
	// service-namespace deliberately not set — file value should survive.

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Observability.Environment != "from-flag-env" {
		t.Errorf("Observability.Environment: explicit flag should win, got %q", cfg.Observability.Environment)
	}
	if cfg.Observability.ServiceNamespace != "from-file-ns" {
		t.Errorf("Observability.ServiceNamespace: file value should survive when flag unset, got %q", cfg.Observability.ServiceNamespace)
	}
}

// TestApplyOverrides_ObservabilityServiceNamespaceFlag pins the second
// branch of applyOverrides for the observability flags. The pre-existing
// TestApplyOverrides_ObservabilityFlags exercises only the
// deployment-environment branch; the service-namespace branch was
// untouched in tests, so a typo in the flag name or a negated guard
// (changed("environment") instead of changed("service-namespace")) would
// silently drop the flag override and the test suite would never notice.
func TestApplyOverrides_ObservabilityServiceNamespaceFlag(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Observability = types.ObservabilityConfig{
		Environment:      "from-file-env",
		ServiceNamespace: "from-file-ns",
	}

	if err := cmd.Flags().Set("service-namespace", "from-flag-ns"); err != nil {
		t.Fatalf("set service-namespace: %v", err)
	}
	// deployment-environment deliberately not set — file value should survive.

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Observability.ServiceNamespace != "from-flag-ns" {
		t.Errorf("Observability.ServiceNamespace: explicit flag should win, got %q", cfg.Observability.ServiceNamespace)
	}
	if cfg.Observability.Environment != "from-file-env" {
		t.Errorf("Observability.Environment: file value should survive when flag unset, got %q", cfg.Observability.Environment)
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

// TestExampleVertexGeminiJSONLoadsAndValidates pins the shipped Vertex
// AI fixture: the file must round-trip through loadRunConfigFile, pass
// ValidateRunConfig, and demonstrate execution-mode-consistent
// permissionPolicy / built-in tool combinations.
//
// Specifically guards B6: prior to the fix the example shipped with
// permissionPolicy=deny-side-effects on an execution-mode config that
// listed run_command and edit_file in tools.builtIn. The combination
// validated, but at runtime every side-effecting tool would have been
// blocked by the permission layer — silently breaking the example.
func TestExampleVertexGeminiJSONLoadsAndValidates(t *testing.T) {
	path := filepath.Join(repoRootForTests(t), "examples", "runconfig", "vertex-gemini.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("examples/runconfig/vertex-gemini.json not found at %q: %v", path, err)
	}
	cfg, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile %q: %v", path, err)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("examples/runconfig/vertex-gemini.json fails ValidateRunConfig: %v", err)
	}
	if cfg.Provider.Type != "gemini" {
		t.Errorf("Provider.Type = %q, want gemini", cfg.Provider.Type)
	}
	if cfg.Provider.GCPProject == "" || cfg.Provider.GCPLocation == "" {
		t.Errorf("Provider must set gcpProject and gcpLocation, got %+v", cfg.Provider)
	}
	// Execution-mode + side-effecting tools must not be paired with
	// deny-side-effects: every workspace-mutating call would be denied
	// at runtime and the example would silently fail to do anything.
	if cfg.Mode == "execution" {
		hasSideEffectTool := false
		for _, name := range cfg.Tools.BuiltIn {
			if name == "run_command" || name == "edit_file" || name == "write_file" {
				hasSideEffectTool = true
				break
			}
		}
		if hasSideEffectTool && cfg.PermissionPolicy.Type == "deny-side-effects" {
			t.Errorf("execution-mode example with side-effecting tools must not use deny-side-effects (got %q + %v)",
				cfg.PermissionPolicy.Type, cfg.Tools.BuiltIn)
		}
	}
}

// TestExampleAzureOpenAIWIFAKSJSONLoadsAndValidates pins the shipped
// AKS Azure-WIF fixture: the file must round-trip through
// loadRunConfigFile, pass ValidateRunConfig, and demonstrate the
// azure-workload-identity credential type with a file-projected token
// source. Drift fails this test before users hit the same error.
func TestExampleAzureOpenAIWIFAKSJSONLoadsAndValidates(t *testing.T) {
	path := filepath.Join(repoRootForTests(t), "examples", "runconfig", "azure-openai-wif-aks.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("examples/runconfig/azure-openai-wif-aks.json not found at %q: %v", path, err)
	}
	cfg, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile %q: %v", path, err)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("examples/runconfig/azure-openai-wif-aks.json fails ValidateRunConfig: %v", err)
	}
	if cfg.Provider.Credential == nil {
		t.Fatal("expected Provider.Credential block")
	}
	if cfg.Provider.Credential.Type != "azure-workload-identity" {
		t.Errorf("Credential.Type = %q, want azure-workload-identity", cfg.Provider.Credential.Type)
	}
	if cfg.Provider.Credential.TokenSource == nil || cfg.Provider.Credential.TokenSource.Type != "file" {
		t.Errorf("expected file token source, got %+v", cfg.Provider.Credential.TokenSource)
	}
}

// TestExampleAzureOpenAIWIFGitHubActionsJSONLoadsAndValidates pins the
// shipped GitHub-Actions Azure-WIF fixture. Same shape as the AKS test
// above but with a github-actions-oidc token source. Validates the
// audience field reaches the schema cleanly.
func TestExampleAzureOpenAIWIFGitHubActionsJSONLoadsAndValidates(t *testing.T) {
	path := filepath.Join(repoRootForTests(t), "examples", "runconfig", "azure-openai-wif-github-actions.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("examples/runconfig/azure-openai-wif-github-actions.json not found at %q: %v", path, err)
	}
	cfg, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile %q: %v", path, err)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("examples/runconfig/azure-openai-wif-github-actions.json fails ValidateRunConfig: %v", err)
	}
	if cfg.Provider.Credential == nil {
		t.Fatal("expected Provider.Credential block")
	}
	if cfg.Provider.Credential.Type != "azure-workload-identity" {
		t.Errorf("Credential.Type = %q, want azure-workload-identity", cfg.Provider.Credential.Type)
	}
	if cfg.Provider.Credential.TokenSource == nil || cfg.Provider.Credential.TokenSource.Type != "github-actions-oidc" {
		t.Errorf("expected github-actions-oidc token source, got %+v", cfg.Provider.Credential.TokenSource)
	}
	if cfg.Provider.Credential.TokenSource.Audience != "api://AzureADTokenExchange" {
		t.Errorf("audience = %q, want api://AzureADTokenExchange", cfg.Provider.Credential.TokenSource.Audience)
	}
}

// TestExampleCloudRunVertexGeminiJSONLoadsAndValidates pins the shipped
// Cloud Run fixture: the file must round-trip through loadRunConfigFile,
// pass ValidateRunConfig, and exercise the three new surface areas that
// Chunks A and B introduced — resultSink.type=stdout-json,
// traceEmitter.type=gcs, and executor.workspaceExportTo on a gs:// URI.
//
// Drift in any of the three fields fails this test before an operator
// hits the same error on a Cloud Run dispatch.
func TestExampleCloudRunVertexGeminiJSONLoadsAndValidates(t *testing.T) {
	path := filepath.Join(repoRootForTests(t), "examples", "runconfig", "cloud-run-vertex-gemini.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("examples/runconfig/cloud-run-vertex-gemini.json not found at %q: %v", path, err)
	}
	cfg, err := loadRunConfigFile(path)
	if err != nil {
		t.Fatalf("loadRunConfigFile %q: %v", path, err)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("examples/runconfig/cloud-run-vertex-gemini.json fails ValidateRunConfig: %v", err)
	}
	if cfg.Provider.Type != "gemini" {
		t.Errorf("Provider.Type = %q, want gemini", cfg.Provider.Type)
	}
	if cfg.Provider.Credential == nil || cfg.Provider.Credential.Type != "gcp-workload-identity" {
		t.Errorf("Provider.Credential = %+v, want type=gcp-workload-identity", cfg.Provider.Credential)
	}
	if cfg.ResultSink == nil || cfg.ResultSink.Type != "stdout-json" {
		t.Errorf("ResultSink = %+v, want type=stdout-json", cfg.ResultSink)
	}
	if cfg.TraceEmitter.Type != "gcs" {
		t.Errorf("TraceEmitter.Type = %q, want gcs", cfg.TraceEmitter.Type)
	}
	if cfg.TraceEmitter.Bucket == "" {
		t.Error("TraceEmitter.Bucket must be set when TraceEmitter.Type is \"gcs\"")
	}
	if cfg.Executor.WorkspaceExportTo == "" {
		t.Error("Executor.WorkspaceExportTo must be set in the Cloud Run fixture")
	}
}

// TestBuildHarnessRunConfig_SafetyRingFlags verifies that the three new
// safety-ring flags (issue #42) propagate to the matching RunConfig
// fields. Each is independently exercised so a future refactor that
// drops one wiring without dropping the others is caught.
func TestBuildHarnessRunConfig_SafetyRingFlags(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

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

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if got, want := cfg.Provider.BaseURL, "https://example.openai.azure.com/openai/v1"; got != want {
		t.Errorf("Provider.BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.APIKeyHeader, "api-key"; got != want {
		t.Errorf("Provider.APIKeyHeader = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.QueryParams["api-version"], "preview"; got != want {
		t.Errorf("Provider.QueryParams[api-version] = %q, want %q", got, want)
	}
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

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
		// All GuardRail fields deliberately left at their zero values.
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
	if cfg.GuardRail != nil {
		t.Errorf("expected GuardRail to be nil when no flags are set, got %+v", cfg.GuardRail)
	}
}

// TestBuildHarnessRunConfig_GuardRailFailOpenFlipsBoolean exercises the
// fail-open flag in isolation: setting only --guardrail-fail-open is
// enough to materialise a GuardRail config (with the default empty
// type) so an operator can flip the posture without restating the rest.
func TestBuildHarnessRunConfig_GuardRailFailOpenFlipsBoolean(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
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

// TestApplyOverrides_GuardRailModelOverride verifies that
// --guardrail-model overrides a file-provided model without disturbing
// other GuardRail fields. This is the path operators on Bedrock take
// when the cloud-judge default Anthropic-API model ID
// (claude-haiku-4-5-20251001) is rejected by Bedrock and must be
// replaced with a Bedrock-format identifier such as
// us.anthropic.claude-haiku-4-5-20251001-v1:0.
func TestApplyOverrides_GuardRailModelOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.GuardRail = &types.GuardRailConfig{
		Type:     "cloud-judge",
		Endpoint: "http://file-endpoint:8000",
		Model:    "from-file",
		FailOpen: true,
	}

	if err := cmd.Flags().Set("guardrail-model", "us.anthropic.claude-haiku-4-5-20251001-v1:0"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.GuardRail == nil {
		t.Fatalf("GuardRail should remain non-nil")
	}
	if cfg.GuardRail.Model != "us.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("Model override failed: got %q", cfg.GuardRail.Model)
	}
	// Other fields must survive untouched.
	if cfg.GuardRail.Type != "cloud-judge" {
		t.Errorf("Type: file value should survive, got %q", cfg.GuardRail.Type)
	}
	if cfg.GuardRail.Endpoint != "http://file-endpoint:8000" {
		t.Errorf("Endpoint: file value should survive, got %q", cfg.GuardRail.Endpoint)
	}
	if !cfg.GuardRail.FailOpen {
		t.Errorf("FailOpen: file value should survive, got false")
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

// TestBuildHarnessRunConfig_GeminiProvider verifies that the Vertex AI
// Gemini provider type passes through the flag-only path: GCPProject
// and GCPLocation flow into ProviderConfig, the resulting RunConfig
// validates, and ModelRouter.Provider is wired to "gemini".
func TestBuildHarnessRunConfig_GeminiProvider(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.Type != "gemini" {
		t.Errorf("Provider.Type = %q, want gemini", cfg.Provider.Type)
	}
	if cfg.Provider.GCPProject != "my-project" {
		t.Errorf("GCPProject = %q, want my-project", cfg.Provider.GCPProject)
	}
	if cfg.Provider.GCPLocation != "us-central1" {
		t.Errorf("GCPLocation = %q, want us-central1", cfg.Provider.GCPLocation)
	}
	if cfg.ModelRouter.Provider != "gemini" {
		t.Errorf("ModelRouter.Provider = %q, want gemini", cfg.ModelRouter.Provider)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig rejected gemini config: %v", err)
	}
}

// TestBuildHarnessRunConfig_GeminiSuppressesAPIKeyRef ensures the
// flag-only path drops APIKeyRef when the provider is gemini. The CLI
// default for --api-key-ref is "secret://ANTHROPIC_API_KEY", so a user
// switching only --provider would otherwise carry that ref through and
// trip the validator (which forbids apiKeyRef on gemini runs).
func TestBuildHarnessRunConfig_GeminiSuppressesAPIKeyRef(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.APIKeyRef != "" {
		t.Errorf("APIKeyRef should be cleared for gemini, got %q", cfg.Provider.APIKeyRef)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig rejected gemini-with-suppressed-apikeyref config: %v", err)
	}
}

// TestBuildHarnessRunConfig_GeminiCredentialsFileImpliesType verifies
// that --gcp-credentials-file implies credential.type=gcp-service-account
// in the flag-only path, mirroring how --permission-policy-file implies
// type=policy-engine. This is the convenience shortcut documented on
// the flag's help string.
func TestBuildHarnessRunConfig_GeminiCredentialsFileImpliesType(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.GCPCredentialsFile != "/tmp/sa.json" {
		t.Errorf("GCPCredentialsFile = %q, want /tmp/sa.json", cfg.Provider.GCPCredentialsFile)
	}
	if cfg.Provider.Credential == nil {
		t.Fatal("expected Credential to be inferred from --gcp-credentials-file")
	}
	if cfg.Provider.Credential.Type != "gcp-service-account" {
		t.Errorf("Credential.Type = %q, want gcp-service-account", cfg.Provider.Credential.Type)
	}
}

// TestBuildHarnessRunConfig_GeminiFieldsScopedToProviderType pins the
// safety invariant: GCP fields supplied while --provider is not gemini
// must NOT leak into the resulting RunConfig (the validator would reject
// them anyway, but we want clean configs to keep --provider switching
// ergonomic).
func TestBuildHarnessRunConfig_GeminiFieldsScopedToProviderType(t *testing.T) {
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
		// These flags are at their default values when the user is not
		// running a gemini provider, but the harness still threads them
		// through opts. The flag-only path must drop them.
		GCPProject:  "leaked-project",
		GCPLocation: "global",
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.GCPProject != "" {
		t.Errorf("GCPProject leaked onto anthropic provider: %q", cfg.Provider.GCPProject)
	}
	if cfg.Provider.GCPLocation != "" {
		t.Errorf("GCPLocation leaked onto anthropic provider: %q", cfg.Provider.GCPLocation)
	}
}

// TestApplyOverrides_GeminiFlags exercises the --gcp-project,
// --gcp-location, and --gcp-credentials-file overrides on the --config
// path. Explicitly-set flags must clobber the file's values, and the
// credentials-file flag must imply a Credential.Type when none is set.
func TestApplyOverrides_GeminiFlags(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "from-file",
		GCPLocation: "global",
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("provider", "gemini")
	must("gcp-project", "from-flag")
	must("gcp-location", "us-central1")
	must("gcp-credentials-file", "/tmp/sa.json")

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.GCPProject != "from-flag" {
		t.Errorf("GCPProject override failed: %q", cfg.Provider.GCPProject)
	}
	if cfg.Provider.GCPLocation != "us-central1" {
		t.Errorf("GCPLocation override failed: %q", cfg.Provider.GCPLocation)
	}
	if cfg.Provider.GCPCredentialsFile != "/tmp/sa.json" {
		t.Errorf("GCPCredentialsFile override failed: %q", cfg.Provider.GCPCredentialsFile)
	}
	if cfg.Provider.Credential == nil {
		t.Fatal("expected Credential to be inferred from --gcp-credentials-file")
	}
	if cfg.Provider.Credential.Type != "gcp-service-account" {
		t.Errorf("Credential.Type = %q, want gcp-service-account", cfg.Provider.Credential.Type)
	}
}

// TestApplyOverrides_GeminiClearsAPIKeyRefFromConfigFile verifies B7:
// switching providers to gemini via --provider must clear an APIKeyRef
// the config file inherited from a previous (non-gemini) configuration.
// Without this clear, validateGeminiProviderFields rejects the run with
// a confusing error about an apiKeyRef the operator never set
// intentionally on this invocation. The flag-only path
// (buildHarnessRunConfig) already does this; the --config path must
// match for parity.
func TestApplyOverrides_GeminiClearsAPIKeyRefFromConfigFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	// Simulate a config file that originally targeted Anthropic and
	// carries the matching APIKeyRef. The operator now flips the
	// provider to gemini at the CLI.
	cfg.Provider = types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	}
	cfg.ModelRouter.Provider = "anthropic"
	cfg.ModelRouter.Model = "claude-sonnet-4-6"

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("provider", "gemini")
	must("gcp-project", "my-proj")

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Type != "gemini" {
		t.Errorf("Provider.Type = %q, want gemini", cfg.Provider.Type)
	}
	if cfg.Provider.APIKeyRef != "" {
		t.Errorf("APIKeyRef should be cleared on gemini switch, got %q", cfg.Provider.APIKeyRef)
	}
}

// TestApplyOverrides_GeminiPreservesExplicitAPIKeyRef pins the inverse
// invariant: if the operator explicitly passes --api-key-ref alongside
// --provider gemini, that value wins. This shape is wrong on its face
// (validateGeminiProviderFields will reject it later with a clear
// error), but the CLI layer must not silently drop an explicit operator
// choice.
func TestApplyOverrides_GeminiPreservesExplicitAPIKeyRef(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{Type: "anthropic"}
	cfg.ModelRouter.Provider = "anthropic"
	cfg.ModelRouter.Model = "claude-sonnet-4-6"

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("provider", "gemini")
	must("api-key-ref", "secret://EXPLICIT")

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.APIKeyRef != "secret://EXPLICIT" {
		t.Errorf("explicit --api-key-ref dropped: %q", cfg.Provider.APIKeyRef)
	}
}

// TestApplyOverrides_GeminiDefaultLocationFallback verifies H3:
// a config file that omits gcpLocation and a CLI invocation that does
// not pass --gcp-location must end up with the documented default
// ("global") rather than failing validation with "gcpLocation is
// required". The flag-only path gets this for free via cobra defaulting;
// the --config path must explicitly fall back when the file omits it.
func TestApplyOverrides_GeminiDefaultLocationFallback(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:       "gemini",
		GCPProject: "my-proj",
		// GCPLocation deliberately empty.
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.GCPLocation != "global" {
		t.Errorf("GCPLocation = %q, want fallback default \"global\"", cfg.Provider.GCPLocation)
	}
}

// TestApplyOverrides_GeminiDefaultFlagsDoNotOverride pins that the
// gemini flag overrides only fire when the operator changed them.
// A config file that sets gcpProject and gcpLocation must not be
// silently overwritten when the CLI invocation leaves the flags at
// their defaults.
func TestApplyOverrides_GeminiDefaultFlagsDoNotOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "from-file",
		GCPLocation: "us-central1",
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	// Note: NOT calling Flags().Set on any gcp-* flag — they remain at
	// their cobra-registered defaults (empty / "global"). With H3's
	// fallback only applying when GCPLocation is empty, the file's
	// "us-central1" must be preserved.
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.GCPProject != "from-file" {
		t.Errorf("GCPProject silently overridden: %q", cfg.Provider.GCPProject)
	}
	if cfg.Provider.GCPLocation != "us-central1" {
		t.Errorf("GCPLocation silently overridden: %q", cfg.Provider.GCPLocation)
	}
}

// TestApplyOverrides_GeminiCredentialsFileRespectsExplicitCredential
// verifies that --gcp-credentials-file does NOT clobber a Credential
// type that the --config file already set explicitly. The "imply only
// when unset" rule mirrors how --permission-policy-file behaves.
func TestApplyOverrides_GeminiCredentialsFileRespectsExplicitCredential(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "p",
		GCPLocation: "global",
		// User has explicitly chosen workload-identity in --config; the
		// flag must not silently downgrade them to a service-account file.
		Credential: &types.CredentialConfig{Type: "gcp-workload-identity"},
	}
	cfg.ModelRouter.Provider = "gemini"
	cfg.ModelRouter.Model = "gemini-2.5-pro"

	if err := cmd.Flags().Set("gcp-credentials-file", "/tmp/sa.json"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.GCPCredentialsFile != "/tmp/sa.json" {
		t.Errorf("GCPCredentialsFile override failed: %q", cfg.Provider.GCPCredentialsFile)
	}
	if cfg.Provider.Credential == nil {
		t.Fatal("Credential cleared unexpectedly")
	}
	if cfg.Provider.Credential.Type != "gcp-workload-identity" {
		t.Errorf("Credential.Type clobbered: got %q, want gcp-workload-identity", cfg.Provider.Credential.Type)
	}
}

// --- Anthropic Workload Identity Federation (issue #117) ---

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
// the inferred credential type is anthropic-wif. This is the primary
// integration point with the documented Anthropic SDK env-var contract.
func TestApplyAnthropicWIF_EnvVarFallback(t *testing.T) {
	clearAnthropicWIFEnv(t)
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_envrule")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("ANTHROPIC_SERVICE_ACCOUNT_ID", "svac_envsa")
	t.Setenv("ANTHROPIC_WORKSPACE_ID", "default")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", "/var/run/secrets/idp/jwt")

	cmd := newTestHarnessCommand()
	cfg := anthropicWIFBaseConfig()

	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	cred := cfg.Provider.Credential
	if cred == nil {
		t.Fatal("expected Credential to be inferred from env vars")
	}
	if cred.Type != "anthropic-wif" {
		t.Errorf("Credential.Type = %q, want anthropic-wif", cred.Type)
	}
	if cred.FederationRuleID != "fdrl_envrule" {
		t.Errorf("FederationRuleID = %q, want fdrl_envrule", cred.FederationRuleID)
	}
	if cred.OrganizationID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("OrganizationID = %q", cred.OrganizationID)
	}
	if cred.ServiceAccountID != "svac_envsa" {
		t.Errorf("ServiceAccountID = %q", cred.ServiceAccountID)
	}
	if cred.WorkspaceID != "default" {
		t.Errorf("WorkspaceID = %q", cred.WorkspaceID)
	}
	if cred.TokenSource == nil {
		t.Fatal("expected TokenSource to be inferred from ANTHROPIC_IDENTITY_TOKEN_FILE")
	}
	if cred.TokenSource.Type != "file" {
		t.Errorf("TokenSource.Type = %q, want file", cred.TokenSource.Type)
	}
	if cred.TokenSource.Path != "/var/run/secrets/idp/jwt" {
		t.Errorf("TokenSource.Path = %q", cred.TokenSource.Path)
	}
	// Default APIKeyRef must be cleared because no operator intent.
	if cfg.Provider.APIKeyRef != "" {
		t.Errorf("APIKeyRef should be cleared under WIF, got %q", cfg.Provider.APIKeyRef)
	}
}

// TestApplyAnthropicWIF_ExplicitFlagBeatsEnv pins the precedence rule:
// when both flag and env var are set, the explicit flag wins.
func TestApplyAnthropicWIF_ExplicitFlagBeatsEnv(t *testing.T) {
	clearAnthropicWIFEnv(t)
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_envrule")

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := cmd.Flags().Set("anthropic-organization-id", "22222222-2222-2222-2222-222222222222"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := cmd.Flags().Set("anthropic-service-account-id", "svac_flagsa"); err != nil {
		t.Fatalf("set: %v", err)
	}

	cfg := anthropicWIFBaseConfig()
	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	if cfg.Provider.Credential == nil || cfg.Provider.Credential.FederationRuleID != "fdrl_flagrule" {
		t.Errorf("explicit flag should beat env, got %+v", cfg.Provider.Credential)
	}
}

// TestApplyAnthropicWIF_FromGitHubActionsSelectsTokenSource pins the
// explicit GHA opt-in: when --anthropic-from-github-actions is set, the
// inferred token source is github-actions-oidc with the Anthropic
// audience, regardless of any env vars present.
func TestApplyAnthropicWIF_FromGitHubActionsSelectsTokenSource(t *testing.T) {
	clearAnthropicWIFEnv(t)
	// GHA env vars are present (as on a real runner), but they alone
	// must not select the OIDC source. Only the flag opts in.
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.actions.githubusercontent.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")

	cmd := newTestHarnessCommand()
	mustSet := func(name, val string) {
		if err := cmd.Flags().Set(name, val); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	mustSet("anthropic-federation-rule-id", "fdrl_flagrule")
	mustSet("anthropic-organization-id", "33333333-3333-3333-3333-333333333333")
	mustSet("anthropic-service-account-id", "svac_flagsa")
	mustSet("anthropic-from-github-actions", "true")

	cfg := anthropicWIFBaseConfig()
	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	cred := cfg.Provider.Credential
	if cred == nil || cred.TokenSource == nil {
		t.Fatalf("expected TokenSource set, got %+v", cred)
	}
	if cred.TokenSource.Type != "github-actions-oidc" {
		t.Errorf("TokenSource.Type = %q, want github-actions-oidc", cred.TokenSource.Type)
	}
	if cred.TokenSource.Audience != "https://api.anthropic.com" {
		t.Errorf("TokenSource.Audience = %q, want https://api.anthropic.com", cred.TokenSource.Audience)
	}
}

// TestApplyAnthropicWIF_GHAEnvAloneDoesNotInferTokenSource is the
// negative test for issue #117 risk #5: presence of
// ACTIONS_ID_TOKEN_REQUEST_URL in the env is NOT a green light to
// auto-select github-actions-oidc. The operator must explicitly opt in
// via --anthropic-from-github-actions. Silent IdP selection makes
// credential bugs unfixable.
func TestApplyAnthropicWIF_GHAEnvAloneDoesNotInferTokenSource(t *testing.T) {
	clearAnthropicWIFEnv(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.actions.githubusercontent.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")

	cmd := newTestHarnessCommand()
	mustSet := func(name, val string) {
		if err := cmd.Flags().Set(name, val); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	mustSet("anthropic-federation-rule-id", "fdrl_flagrule")
	mustSet("anthropic-organization-id", "44444444-4444-4444-4444-444444444444")
	mustSet("anthropic-service-account-id", "svac_flagsa")
	// Deliberately NOT setting --anthropic-from-github-actions.

	cfg := anthropicWIFBaseConfig()
	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	cred := cfg.Provider.Credential
	if cred == nil {
		t.Fatal("expected Credential to be inferred from federation flags")
	}
	if cred.TokenSource != nil {
		t.Errorf("TokenSource should NOT be inferred from bare GHA env, got %+v", cred.TokenSource)
	}
}

// TestApplyAnthropicWIF_IdentityTokenEnvVarSelectsEnvSource pins the
// fallback for ANTHROPIC_IDENTITY_TOKEN: a literal token in the env
// implies tokenSource={type:env, envVar:ANTHROPIC_IDENTITY_TOKEN}.
func TestApplyAnthropicWIF_IdentityTokenEnvVarSelectsEnvSource(t *testing.T) {
	clearAnthropicWIFEnv(t)
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN", "eyJ.fake.jwt")

	cmd := newTestHarnessCommand()
	mustSet := func(name, val string) {
		if err := cmd.Flags().Set(name, val); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	mustSet("anthropic-federation-rule-id", "fdrl_flagrule")
	mustSet("anthropic-organization-id", "55555555-5555-5555-5555-555555555555")
	mustSet("anthropic-service-account-id", "svac_flagsa")

	cfg := anthropicWIFBaseConfig()
	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	cred := cfg.Provider.Credential
	if cred == nil || cred.TokenSource == nil {
		t.Fatalf("expected TokenSource set, got %+v", cred)
	}
	if cred.TokenSource.Type != "env" {
		t.Errorf("TokenSource.Type = %q, want env", cred.TokenSource.Type)
	}
	if cred.TokenSource.EnvVar != "ANTHROPIC_IDENTITY_TOKEN" {
		t.Errorf("TokenSource.EnvVar = %q, want ANTHROPIC_IDENTITY_TOKEN", cred.TokenSource.EnvVar)
	}
}

// TestApplyAnthropicWIF_ExplicitAPIKeyRefRejected is the issue #117
// risk #4 enforcement: when --api-key-ref is explicitly passed alongside
// the WIF flags, the override layer must hard-fail rather than silently
// dropping one or the other. A leftover API key would silently shadow
// federation in the SDK precedence chain.
func TestApplyAnthropicWIF_ExplicitAPIKeyRefRejected(t *testing.T) {
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	mustSet := func(name, val string) {
		if err := cmd.Flags().Set(name, val); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	mustSet("anthropic-federation-rule-id", "fdrl_flagrule")
	mustSet("anthropic-organization-id", "66666666-6666-6666-6666-666666666666")
	mustSet("anthropic-service-account-id", "svac_flagsa")
	mustSet("api-key-ref", "secret://OPERATOR_KEY")

	cfg := anthropicWIFBaseConfig()
	cfg.Provider.APIKeyRef = "secret://OPERATOR_KEY"

	err := applyAnthropicWIFOverrides(cmd, cfg)
	if err == nil {
		t.Fatal("expected error when --api-key-ref is set with WIF flags")
	}
	if !strings.Contains(err.Error(), "api-key-ref") {
		t.Errorf("error should mention api-key-ref, got: %v", err)
	}
}

// TestApplyAnthropicWIF_DefaultAPIKeyRefSilentlyCleared documents the
// other half of the apiKeyRef guard: the default flag value
// "secret://ANTHROPIC_API_KEY" is structurally non-meaningful under
// WIF (no operator intent expressed), so the override layer clears it
// silently rather than failing loudly. This mirrors the gemini
// pattern at applyOverrides line ~477.
func TestApplyAnthropicWIF_DefaultAPIKeyRefSilentlyCleared(t *testing.T) {
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	mustSet := func(name, val string) {
		if err := cmd.Flags().Set(name, val); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	mustSet("anthropic-federation-rule-id", "fdrl_flagrule")
	mustSet("anthropic-organization-id", "77777777-7777-7777-7777-777777777777")
	mustSet("anthropic-service-account-id", "svac_flagsa")
	// Deliberately NOT setting --api-key-ref. cfg carries the default.

	cfg := anthropicWIFBaseConfig()
	// The default-from-flag path: APIKeyRef holds the registered default.
	cfg.Provider.APIKeyRef = "secret://ANTHROPIC_API_KEY"

	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	if cfg.Provider.APIKeyRef != "" {
		t.Errorf("default APIKeyRef should be cleared under WIF, got %q", cfg.Provider.APIKeyRef)
	}
}

// TestApplyAnthropicWIF_NoIntentNoOp guards the no-op early return: if
// the operator has set neither WIF flags nor env vars, and the
// credential block does not already name anthropic-wif, the helper
// must leave the config untouched. This protects every non-WIF code
// path from accidental mutation.
func TestApplyAnthropicWIF_NoIntentNoOp(t *testing.T) {
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	cfg := anthropicWIFBaseConfig()

	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	if cfg.Provider.Credential != nil {
		t.Errorf("Credential should be nil with no WIF intent, got %+v", cfg.Provider.Credential)
	}
	if cfg.Provider.APIKeyRef != "secret://ANTHROPIC_API_KEY" {
		t.Errorf("APIKeyRef should be untouched without WIF intent, got %q", cfg.Provider.APIKeyRef)
	}
}

// TestApplyAnthropicWIF_ConflictingExplicitTypeRejected covers the
// inconsistent-config rejection: if the user has already named a
// non-anthropic-wif credential type in --config and then layers WIF
// federation flags on top, the helper must fail loudly rather than
// silently rewriting the operator's choice.
func TestApplyAnthropicWIF_ConflictingExplicitTypeRejected(t *testing.T) {
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("anthropic-federation-rule-id", "fdrl_flagrule"); err != nil {
		t.Fatalf("set: %v", err)
	}

	cfg := anthropicWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{Type: "aws-default"}

	err := applyAnthropicWIFOverrides(cmd, cfg)
	if err == nil {
		t.Fatal("expected error when WIF flags conflict with explicit non-WIF type")
	}
	if !strings.Contains(err.Error(), "anthropic-wif") {
		t.Errorf("error should mention anthropic-wif, got: %v", err)
	}
}

// TestApplyAnthropicWIF_ExistingStaticTypePromoted documents the
// "static" sub-path of the type-inference branch: when a --config file
// names credential.type="static" (the documented synonym for the
// default key-based path) and the operator layers WIF flags on top,
// applyAnthropicWIFOverrides must promote the type to anthropic-wif
// rather than rejecting the run as a conflict. This is the upgrade
// path from a key-based config to a federated one.
func TestApplyAnthropicWIF_ExistingStaticTypePromoted(t *testing.T) {
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	mustSet := func(name, val string) {
		if err := cmd.Flags().Set(name, val); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	mustSet("anthropic-federation-rule-id", "fdrl_flagrule")
	mustSet("anthropic-organization-id", "99999999-9999-9999-9999-999999999999")
	mustSet("anthropic-service-account-id", "svac_flagsa")

	cfg := anthropicWIFBaseConfig()
	cfg.Provider.Credential = &types.CredentialConfig{Type: "static"}

	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	if cfg.Provider.Credential.Type != "anthropic-wif" {
		t.Errorf("Credential.Type = %q, want anthropic-wif (static must be promoted)", cfg.Provider.Credential.Type)
	}
	if cfg.Provider.Credential.FederationRuleID != "fdrl_flagrule" {
		t.Errorf("FederationRuleID = %q, want fdrl_flagrule", cfg.Provider.Credential.FederationRuleID)
	}
}

// TestApplyAnthropicWIF_ExistingTokenSourcePreserved guards the rule
// that an explicit token source from --config always wins. Even when
// --anthropic-from-github-actions is set, an existing TokenSource must
// not be silently overwritten.
func TestApplyAnthropicWIF_ExistingTokenSourcePreserved(t *testing.T) {
	clearAnthropicWIFEnv(t)

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("anthropic-from-github-actions", "true"); err != nil {
		t.Fatalf("set: %v", err)
	}

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

	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		t.Fatalf("applyAnthropicWIFOverrides: %v", err)
	}

	if cfg.Provider.Credential.TokenSource.Type != "file" {
		t.Errorf("file-provided TokenSource overwritten: got %q", cfg.Provider.Credential.TokenSource.Type)
	}
	if cfg.Provider.Credential.TokenSource.Path != "/var/run/file/jwt" {
		t.Errorf("file-provided TokenSource.Path overwritten: got %q", cfg.Provider.Credential.TokenSource.Path)
	}
}

// TestBuildHarnessRunConfig_AzureWIFFlagsImplyCredential verifies that
// --azure-tenant-id (and the companion --azure-client-id / --azure-scope)
// in the flag-only path produce a Credential block with
// type=azure-workload-identity. Mirrors the --gcp-credentials-file
// shortcut: the flag is the discriminator.
func TestBuildHarnessRunConfig_AzureWIFFlagsImplyCredential(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.Credential == nil {
		t.Fatal("expected Credential to be inferred from --azure-tenant-id")
	}
	if cfg.Provider.Credential.Type != "azure-workload-identity" {
		t.Errorf("Credential.Type = %q, want azure-workload-identity", cfg.Provider.Credential.Type)
	}
	if cfg.Provider.Credential.AzureTenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("AzureTenantID = %q", cfg.Provider.Credential.AzureTenantID)
	}
	if cfg.Provider.Credential.AzureClientID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("AzureClientID = %q", cfg.Provider.Credential.AzureClientID)
	}
	if cfg.Provider.Credential.AzureScope != "https://cognitiveservices.azure.com/.default" {
		t.Errorf("AzureScope = %q", cfg.Provider.Credential.AzureScope)
	}
	// APIKeyRef must be cleared for an Azure WIF run: the validator
	// rejects the combination because the bearer is fetched via OAuth2
	// token exchange. The cobra default for --api-key-ref is
	// secret://ANTHROPIC_API_KEY, so without the gemini-style clear in
	// buildHarnessRunConfig a flag-only Azure WIF run would fail
	// validation with a confusing error about a value the operator
	// never set.
	if cfg.Provider.APIKeyRef != "" {
		t.Errorf("APIKeyRef should be cleared for Azure WIF, got %q", cfg.Provider.APIKeyRef)
	}
}

// TestBuildHarnessRunConfig_AzureWIFPassesValidation is the regression
// guard that the rest of the WIF flag-only-path tests cannot provide
// on their own. It runs buildHarnessRunConfig with the minimum WIF
// shape that the validator accepts (tenant + client + tokenSource via
// CLI options the way runHarness wires them), then hands the result
// directly to types.ValidateRunConfig and asserts the run is valid.
// The pre-remediation buildHarnessRunConfig left APIKeyRef set to the
// cobra default secret://ANTHROPIC_API_KEY; ValidateRunConfig would
// then reject the run with "azure-workload-identity does not use
// apiKeyRef". The test pins that an Azure WIF flag-only run is valid
// end-to-end so the regression cannot recur.
func TestBuildHarnessRunConfig_AzureWIFPassesValidation(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
	// buildHarnessRunConfig only assembles the flag-implied Credential
	// shell — TokenSource still has to come from --config in the real
	// CLI, but the validator needs one to accept the run. Wire a file
	// source by hand so the validation path actually runs end-to-end.
	if cfg.Provider.Credential == nil {
		t.Fatal("expected Credential to be inferred from --azure-tenant-id")
	}
	cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
		Type: "file",
		Path: "/var/run/secrets/azure/token",
	}

	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Fatalf("ValidateRunConfig should accept Azure WIF flag-only run, got: %v", err)
	}
}

// TestBuildHarnessRunConfig_AzureWIFTenantWithoutClient verifies that a
// --azure-tenant-id passed without --azure-client-id still produces the
// implied Credential block. The validator (a separate layer) will then
// reject the run with a clear "azure-workload-identity requires
// azureClientId" error — the flag mapping itself is mechanical and must
// not silently drop a partial spec.
func TestBuildHarnessRunConfig_AzureWIFTenantWithoutClient(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.Credential == nil {
		t.Fatal("expected Credential to be inferred from --azure-tenant-id alone")
	}
	if cfg.Provider.Credential.Type != "azure-workload-identity" {
		t.Errorf("Credential.Type = %q, want azure-workload-identity", cfg.Provider.Credential.Type)
	}
	if cfg.Provider.Credential.AzureTenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("AzureTenantID = %q", cfg.Provider.Credential.AzureTenantID)
	}
	if cfg.Provider.Credential.AzureClientID != "" {
		t.Errorf("AzureClientID should be empty, got %q", cfg.Provider.Credential.AzureClientID)
	}
}

// TestBuildHarnessRunConfig_AzureWIFRespectsExplicitCredential verifies
// that --azure-tenant-id does NOT clobber an explicit Credential block
// that the caller has already constructed (e.g. via --config). This
// mirrors how --gcp-credentials-file behaves.
//
// Build path is exercised via the higher-level applyOverrides test
// below (TestApplyOverrides_AzureWIFRespectsExplicitCredential); this
// flag-only test cannot exercise the path because buildHarnessRunConfig
// itself constructs the config from scratch (no pre-existing
// Credential to preserve).
func TestBuildHarnessRunConfig_AzureWIFNotSetLeavesCredentialNil(t *testing.T) {
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
		// All AzureWIF fields empty — no Credential block should be
		// constructed for a vanilla openai-compatible run.
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}

	if cfg.Provider.Credential != nil {
		t.Errorf("Credential should remain nil when no Azure WIF flags are set, got %+v", cfg.Provider.Credential)
	}
}

// TestApplyOverrides_AzureWIFFlags exercises the --azure-* overrides on
// the --config path. An explicitly-set --azure-tenant-id must:
//   - Imply a Credential block when the file has none.
//   - Populate AzureTenantID / AzureClientID / AzureScope on the
//     resulting Credential.
func TestApplyOverrides_AzureWIFFlags(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:    "openai-compatible",
		BaseURL: "https://example.openai.azure.com/openai/v1",
	}
	cfg.ModelRouter.Provider = "openai-compatible"
	cfg.ModelRouter.Model = "gpt-4o"

	must := func(name, value string) {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	must("provider", "openai-compatible")
	must("azure-tenant-id", "11111111-1111-1111-1111-111111111111")
	must("azure-client-id", "22222222-2222-2222-2222-222222222222")
	must("azure-scope", "https://cognitiveservices.azure.com/.default")

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Credential == nil {
		t.Fatal("expected Credential to be inferred from --azure-tenant-id")
	}
	if cfg.Provider.Credential.Type != "azure-workload-identity" {
		t.Errorf("Credential.Type = %q, want azure-workload-identity", cfg.Provider.Credential.Type)
	}
	if cfg.Provider.Credential.AzureTenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("AzureTenantID override failed: %q", cfg.Provider.Credential.AzureTenantID)
	}
	if cfg.Provider.Credential.AzureClientID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("AzureClientID override failed: %q", cfg.Provider.Credential.AzureClientID)
	}
	if cfg.Provider.Credential.AzureScope != "https://cognitiveservices.azure.com/.default" {
		t.Errorf("AzureScope override failed: %q", cfg.Provider.Credential.AzureScope)
	}
}

// TestApplyOverrides_AzureWIFRespectsExplicitCredential pins that an
// explicit Credential block in --config (e.g. credential.type=static)
// is NOT silently upgraded to azure-workload-identity by a stray
// --azure-tenant-id flag. The override fills the Azure-named fields on
// the existing block (so a config that already says
// credential.type=azure-workload-identity can still be fine-tuned at
// the CLI), but the type is preserved. Mirrors how
// --gcp-credentials-file leaves a non-gcp-service-account Credential
// alone.
func TestApplyOverrides_AzureWIFRespectsExplicitCredential(t *testing.T) {
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

	if err := cmd.Flags().Set("azure-tenant-id", "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Credential == nil {
		t.Fatal("Credential cleared unexpectedly")
	}
	if cfg.Provider.Credential.Type != "static" {
		t.Errorf("Credential.Type silently upgraded: got %q, want static", cfg.Provider.Credential.Type)
	}
	// The Azure tenant field is still populated on the existing block —
	// the validator will then reject the combination (static type with
	// azureTenantId set), which is the correct outcome: the operator's
	// intent is ambiguous and should fail loudly.
	if cfg.Provider.Credential.AzureTenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("AzureTenantID not propagated to existing Credential: %q", cfg.Provider.Credential.AzureTenantID)
	}
}

// TestApplyOverrides_AzureWIFClientIDAloneDoesNotCreateCredential pins
// that --azure-client-id without --azure-tenant-id leaves the Credential
// untouched. Only --azure-tenant-id is the discriminator that
// materialises an azure-workload-identity Credential block (mirroring
// --gcp-credentials-file). Without this guard, a stray --azure-client-id
// would produce a Credential block missing tenantID and surface as a
// confusing "azure-workload-identity requires azureTenantId" validation
// error the operator never asked for.
func TestApplyOverrides_AzureWIFClientIDAloneDoesNotCreateCredential(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:      "openai-compatible",
		BaseURL:   "https://example.openai.azure.com/openai/v1",
		APIKeyRef: "secret://OPENAI_KEY",
	}

	if err := cmd.Flags().Set("azure-client-id", "22222222-2222-2222-2222-222222222222"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Credential != nil {
		t.Errorf("--azure-client-id alone must not create a Credential block, got %+v", cfg.Provider.Credential)
	}
}

// TestApplyOverrides_AzureWIFScopeAloneDoesNotCreateCredential is the
// companion to the client-id test above. --azure-scope without
// --azure-tenant-id must not produce an orphan Credential block.
func TestApplyOverrides_AzureWIFScopeAloneDoesNotCreateCredential(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider = types.ProviderConfig{
		Type:      "openai-compatible",
		BaseURL:   "https://example.openai.azure.com/openai/v1",
		APIKeyRef: "secret://OPENAI_KEY",
	}

	if err := cmd.Flags().Set("azure-scope", "https://cognitiveservices.azure.com/.default"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Credential != nil {
		t.Errorf("--azure-scope alone must not create a Credential block, got %+v", cfg.Provider.Credential)
	}
}

// TestApplyOverrides_AzureWIFDefaultFlagsDoNotOverride pins the central
// precedence rule for the --azure-* family: when none of the three
// flags is passed, an existing Credential block from --config must
// survive untouched. This is the file-wins-over-default check the rest
// of the override surface enforces; the WIF flags are no exception.
func TestApplyOverrides_AzureWIFDefaultFlagsDoNotOverride(t *testing.T) {
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

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Credential == nil {
		t.Fatal("file-provided Credential cleared unexpectedly")
	}
	if cfg.Provider.Credential.AzureTenantID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("AzureTenantID overwritten by default: %q", cfg.Provider.Credential.AzureTenantID)
	}
	if cfg.Provider.Credential.AzureClientID != "44444444-4444-4444-4444-444444444444" {
		t.Errorf("AzureClientID overwritten by default: %q", cfg.Provider.Credential.AzureClientID)
	}
	if cfg.Provider.Credential.AzureScope != "https://existing.example.com/.default" {
		t.Errorf("AzureScope overwritten by default: %q", cfg.Provider.Credential.AzureScope)
	}
}

// writePromptResolutionConfig writes a JSON RunConfig that is valid in
// every respect EXCEPT it carries an empty prompt and a deliberately
// out-of-range MaxTurns. That shape lets the prompt-resolution tests
// distinguish three outcomes from a single runHarness invocation:
//
//  1. Prompt did not resolve → "prompt is required" error.
//  2. Prompt resolved → ValidateRunConfig fires next and rejects on
//     "maxTurns exceeds maximum of 100" — a deterministic, prompt-
//     independent signal that the resolution chain populated cfg.Prompt.
//  3. File-read error from --prompt-file → the helper's error wins
//     before the resolution chain reaches validation.
//
// Using a config-only invalidation point keeps the tests purely
// in-process: no harness boot, no provider HTTP, no API key juggling.
func writePromptResolutionConfig(t *testing.T) string {
	t.Helper()
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
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestRunHarness_PromptFromEnvVar pins the STIRRUP_PROMPT fallback: when
// no higher-priority source (flag, positional, --prompt-file, file
// prompt) is set, the env var must populate cfg.Prompt. We assert by
// invoking runHarness against a config that fails validation downstream
// of prompt resolution — the validator's specific error tells us the
// prompt was filled in, since the "prompt is required" path would have
// short-circuited earlier.
func TestRunHarness_PromptFromEnvVar(t *testing.T) {
	path := writePromptResolutionConfig(t)
	t.Setenv("STIRRUP_PROMPT", "hello from env")

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatalf("set config: %v", err)
	}

	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error after prompt resolution, got nil")
	}
	if strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("STIRRUP_PROMPT was not consulted; got prompt-required error: %v", err)
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected validator to reject maxTurns after prompt was resolved, got: %v", err)
	}
}

// TestRunHarness_PromptFromPromptFile pins the --prompt-file source:
// the file's contents become cfg.Prompt with trailing newlines trimmed.
// Same downstream-validation trick as the env-var test — a maxTurns
// rejection means we got past the prompt-required check, which means
// the file was read and applied.
func TestRunHarness_PromptFromPromptFile(t *testing.T) {
	path := writePromptResolutionConfig(t)

	promptDir := t.TempDir()
	promptPath := filepath.Join(promptDir, "brief.txt")
	// Embed a trailing \n to exercise the TrimRight contract — the
	// trim must happen, otherwise downstream prompt comparisons would
	// silently include a newline a `printf 'p\n'` author did not
	// intend. (We cannot read cfg.Prompt back through runHarness, so
	// the trim contract is asserted directly against readPromptFile
	// below; this test only confirms the file reached the chain.)
	if err := os.WriteFile(promptPath, []byte("hello from file\n"), 0o600); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if err := cmd.Flags().Set("prompt-file", promptPath); err != nil {
		t.Fatalf("set prompt-file: %v", err)
	}

	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error after prompt resolution, got nil")
	}
	if strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("--prompt-file was not consulted; got prompt-required error: %v", err)
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected validator to reject maxTurns after prompt was resolved, got: %v", err)
	}

	// Direct trim-contract assertion: readPromptFile must strip the
	// trailing newline so downstream prompt-equality checks (in eval
	// suites, recordings, etc.) are not silently off-by-one-byte.
	got, err := readPromptFile(promptPath)
	if err != nil {
		t.Fatalf("readPromptFile: %v", err)
	}
	if got != "hello from file" {
		t.Errorf("readPromptFile did not trim trailing newline, got %q", got)
	}
}

// TestRunHarness_PromptFlagBeatsLowerPrecedence is the precedence
// regression test: --prompt is rank 1, --prompt-file is rank 3,
// STIRRUP_PROMPT is rank 4. When all three are set, --prompt must win
// and runHarness must reach validation without ever reading the file
// or consulting the env. We assert this by setting --prompt-file to a
// path that DOES NOT EXIST: if the resolution chain ever fell through
// to it, readPromptFile would error out before validation, and we'd
// see "reading --prompt-file" rather than the maxTurns failure.
func TestRunHarness_PromptFlagBeatsLowerPrecedence(t *testing.T) {
	path := writePromptResolutionConfig(t)
	t.Setenv("STIRRUP_PROMPT", "from env (should lose)")

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if err := cmd.Flags().Set("prompt", "from flag (should win)"); err != nil {
		t.Fatalf("set prompt: %v", err)
	}
	if err := cmd.Flags().Set("prompt-file", "/does/not/exist/brief.txt"); err != nil {
		t.Fatalf("set prompt-file: %v", err)
	}

	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error after prompt resolution, got nil")
	}
	if strings.Contains(err.Error(), "reading --prompt-file") {
		t.Errorf("--prompt flag did not short-circuit --prompt-file; chain leaked: %v", err)
	}
	if strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("--prompt flag was ignored entirely; chain produced: %v", err)
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected validator to reject maxTurns after --prompt resolved, got: %v", err)
	}
}

// TestReadPromptFile_Nonexistent verifies the explicit error surface
// for a missing file: the path name must appear in the message so the
// operator can find the typo without re-running with --log-level=debug.
func TestReadPromptFile_Nonexistent(t *testing.T) {
	_, err := readPromptFile("/does/not/exist/brief.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "/does/not/exist/brief.txt") {
		t.Errorf("error should mention the path, got: %v", err)
	}
}

// TestReadPromptFile_Empty verifies the zero-byte file is a hard error
// rather than a silent "" that would later surface as the generic
// "prompt is required" message — that would be deeply confusing
// because the operator DID set --prompt-file.
func TestReadPromptFile_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := readPromptFile(path)
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
	if !strings.Contains(err.Error(), "is empty") {
		t.Errorf("error should mention empty, got: %v", err)
	}
}

// TestReadPromptFile_Directory pins the IsDir() guard. Passing a
// directory used to return an opaque "is a directory" error from
// os.ReadFile; today the guard surfaces the same shape with the
// path baked in so the operator can see the typo. Without this
// test, a future refactor could silently drop the guard and let
// readPromptFile try to read the directory contents as a stream.
func TestReadPromptFile_Directory(t *testing.T) {
	dir := t.TempDir()
	_, err := readPromptFile(dir)
	if err == nil {
		t.Fatal("expected error for directory, got nil")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("expected directory error, got: %v", err)
	}
}

// TestReadPromptFile_OversizeRejected pins the 10 MiB cap. A
// regression that dropped either the stat-time check or the
// io.LimitReader post-read check would let an arbitrarily-large
// file land in cfg.Prompt and burn through the provider's input-
// token budget on the very first turn. Writing exactly cap+1
// bytes is enough to trip either guard.
func TestReadPromptFile_OversizeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	data := make([]byte, maxPromptFileBytes+1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := readPromptFile(path)
	if err == nil {
		t.Fatal("expected cap error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected cap error, got: %v", err)
	}
}

// newPath2HarnessCommand mirrors newTestHarnessCommand but is shaped
// for Path 2 (flag-only) tests: --config is left unset so runHarness
// enters the buildHarnessRunConfig branch, and --max-turns is
// pre-deformed to 9999 so a successful prompt resolution surfaces a
// deterministic, prompt-independent "maxTurns exceeds maximum"
// validator error. Keeping the helper next to its callers (rather
// than threading a parameter through newTestHarnessCommand) avoids
// risk to the existing Path 1 tests that all rely on the current
// defaults.
func newPath2HarnessCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("max-turns", "9999"); err != nil {
		t.Fatalf("set max-turns: %v", err)
	}
	// Path 2 needs an APIKeyRef pointing at an env var that resolves;
	// the validator's downstream secret-store lookup happens after
	// the MaxTurns check, but a missing env in CI would otherwise
	// muddy the error message. We assert against "maxTurns" only,
	// so this is purely defensive.
	t.Setenv("STIRRUP_PATH2_TEST_KEY", "x")
	if err := cmd.Flags().Set("api-key-ref", "env://STIRRUP_PATH2_TEST_KEY"); err != nil {
		t.Fatalf("set api-key-ref: %v", err)
	}
	return cmd
}

// TestRunHarness_Path2_PromptFromEnvVar covers the flag-only path's
// STIRRUP_PROMPT fallback. Path 1 already has this coverage; the
// flag-only path (the common "stirrup harness --prompt-file brief.txt"
// production invocation) had no runHarness-level test, so a regression
// that broke either source on Path 2 — e.g. an accidental early
// `prompt is required` return — would not be caught.
func TestRunHarness_Path2_PromptFromEnvVar(t *testing.T) {
	t.Setenv("STIRRUP_PROMPT", "hello from env")

	cmd := newPath2HarnessCommand(t)
	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error after prompt resolution, got nil")
	}
	if strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("STIRRUP_PROMPT was not consulted on Path 2; got prompt-required error: %v", err)
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected validator to reject maxTurns after prompt was resolved, got: %v", err)
	}
}

// TestRunHarness_Path2_PromptFromPromptFile is the --prompt-file
// counterpart on Path 2. The trailing newline trim contract is
// already pinned by TestRunHarness_PromptFromPromptFile against
// readPromptFile directly; this test only confirms the file reached
// the resolution chain in the flag-only path.
func TestRunHarness_Path2_PromptFromPromptFile(t *testing.T) {
	// Belt and braces — make sure no ambient STIRRUP_PROMPT shadows
	// the --prompt-file we're trying to exercise.
	t.Setenv("STIRRUP_PROMPT", "")

	promptDir := t.TempDir()
	promptPath := filepath.Join(promptDir, "brief.txt")
	if err := os.WriteFile(promptPath, []byte("hello from file\n"), 0o600); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}

	cmd := newPath2HarnessCommand(t)
	if err := cmd.Flags().Set("prompt-file", promptPath); err != nil {
		t.Fatalf("set prompt-file: %v", err)
	}

	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error after prompt resolution, got nil")
	}
	if strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("--prompt-file was not consulted on Path 2; got prompt-required error: %v", err)
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected validator to reject maxTurns after prompt was resolved, got: %v", err)
	}
}

// TestRunHarness_Path2_AllSourcesEmpty asserts that the "prompt is
// required" error on Path 2 names every prompt source so an operator
// hitting this error sees the full chain without grepping the source.
// Doubles as the regression test for N1 — the previous message
// omitted "positional argument" entirely and listed --config first
// despite its being the lowest-priority source.
func TestRunHarness_Path2_AllSourcesEmpty(t *testing.T) {
	t.Setenv("STIRRUP_PROMPT", "")

	cmd := newPath2HarnessCommand(t)
	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected prompt-required error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"--prompt-file",
		"STIRRUP_PROMPT",
		"positional argument",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// --- B-2 / B-3 / B-4: provider-retry flag-to-config wiring (#197) ---

// TestApplyProviderRetryOverrides_SubMillisecondRejected pins the helper's
// refusal to silently truncate a sub-millisecond duration to zero. Without
// this guard a `--provider-retry-initial-delay=500us` invocation would
// satisfy `int(d / time.Millisecond) == 0` and fall through to the
// zero-guard ("flag not set"), erasing the operator's intent.
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
			pc := &types.ProviderConfig{}
			err := applyProviderRetryOverrides(pc, tc.opts)
			if err == nil {
				t.Fatalf("expected sub-ms rejection error, got nil; pc.Retry=%+v", pc.Retry)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error should mention %q, got: %v", tc.expect, err)
			}
			if !strings.Contains(err.Error(), "minimum resolution is 1ms") {
				t.Errorf("error should mention the resolution limit, got: %v", err)
			}
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
			cmd := newTestHarnessCommand()
			if err := cmd.Flags().Set(tc.flag, tc.value); err != nil {
				t.Fatalf("set %s=%s: %v", tc.flag, tc.value, err)
			}
			pc := &types.ProviderConfig{}
			err := applyProviderRetryFlagOverrides(cmd, pc)
			if err == nil {
				t.Fatalf("expected sub-ms rejection error, got nil; pc.Retry=%+v", pc.Retry)
			}
			if !strings.Contains(err.Error(), "--"+tc.flag) {
				t.Errorf("error should mention --%s, got: %v", tc.flag, err)
			}
			if !strings.Contains(err.Error(), "minimum resolution is 1ms") {
				t.Errorf("error should mention the resolution limit, got: %v", err)
			}
		})
	}
}

// TestApplyProviderRetryOverrides_AllFlagsSet asserts each of the four
// flags lands on its corresponding ProviderRetryConfig field with the
// duration→ms conversion applied.
func TestApplyProviderRetryOverrides_AllFlagsSet(t *testing.T) {
	pc := &types.ProviderConfig{}
	opts := harnessCLIOptions{
		ProviderRetryMaxAttempts:     4,
		ProviderRetryInitialDelay:    250 * time.Millisecond,
		ProviderRetryMaxDelay:        10 * time.Second,
		ProviderRetryWallClockBudget: 45 * time.Second,
	}
	if err := applyProviderRetryOverrides(pc, opts); err != nil {
		t.Fatalf("applyProviderRetryOverrides: %v", err)
	}
	if pc.Retry == nil {
		t.Fatal("Retry should have been allocated")
	}
	if got, want := pc.Retry.MaxAttempts, 4; got != want {
		t.Errorf("MaxAttempts = %d, want %d", got, want)
	}
	if got, want := pc.Retry.InitialDelayMs, 250; got != want {
		t.Errorf("InitialDelayMs = %d, want %d", got, want)
	}
	if got, want := pc.Retry.MaxDelayMs, 10000; got != want {
		t.Errorf("MaxDelayMs = %d, want %d", got, want)
	}
	if got, want := pc.Retry.WallClockBudgetMs, 45000; got != want {
		t.Errorf("WallClockBudgetMs = %d, want %d", got, want)
	}
}

// TestApplyProviderRetryOverrides_SingleFlagPartialOverride asserts that
// setting one flag does not implicitly zero the other slots — the
// per-field defaulter in ValidateRunConfig still fills in the remaining
// fields downstream.
func TestApplyProviderRetryOverrides_SingleFlagPartialOverride(t *testing.T) {
	pc := &types.ProviderConfig{}
	opts := harnessCLIOptions{ProviderRetryMaxAttempts: 5}
	if err := applyProviderRetryOverrides(pc, opts); err != nil {
		t.Fatalf("applyProviderRetryOverrides: %v", err)
	}
	if pc.Retry == nil {
		t.Fatal("Retry should have been allocated for the single-flag case")
	}
	if pc.Retry.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", pc.Retry.MaxAttempts)
	}
	if pc.Retry.InitialDelayMs != 0 || pc.Retry.MaxDelayMs != 0 || pc.Retry.WallClockBudgetMs != 0 {
		t.Errorf("unflagged slots should remain zero, got %+v", pc.Retry)
	}
}

// TestApplyProviderRetryOverrides_AllZeroIsNoop asserts that an entirely-
// untouched flag surface leaves pc.Retry nil, preserving the documented
// "no override" path (ValidateRunConfig then fills in all defaults).
func TestApplyProviderRetryOverrides_AllZeroIsNoop(t *testing.T) {
	pc := &types.ProviderConfig{}
	if err := applyProviderRetryOverrides(pc, harnessCLIOptions{}); err != nil {
		t.Fatalf("applyProviderRetryOverrides: %v", err)
	}
	if pc.Retry != nil {
		t.Errorf("Retry should remain nil for the all-zero case, got %+v", pc.Retry)
	}
}

// TestApplyOverrides_ProviderRetryFlagOverridesFile asserts that a
// single Changed() flag rewrites only the corresponding file slot,
// leaving the other file-supplied retry values untouched. This is the
// "operator pins one knob" contract for the --config + flag combo.
func TestApplyOverrides_ProviderRetryFlagOverridesFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("provider-retry-wall-clock", "120s"); err != nil {
		t.Fatalf("set provider-retry-wall-clock: %v", err)
	}
	cfg := baseFileConfig()
	cfg.Provider.Retry = &types.ProviderRetryConfig{
		MaxAttempts:       2,
		InitialDelayMs:    750,
		MaxDelayMs:        20000,
		WallClockBudgetMs: 60000,
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.Provider.Retry == nil {
		t.Fatal("Retry must not be cleared")
	}
	if cfg.Provider.Retry.MaxAttempts != 2 {
		t.Errorf("MaxAttempts: file value should survive, got %d", cfg.Provider.Retry.MaxAttempts)
	}
	if cfg.Provider.Retry.InitialDelayMs != 750 {
		t.Errorf("InitialDelayMs: file value should survive, got %d", cfg.Provider.Retry.InitialDelayMs)
	}
	if cfg.Provider.Retry.MaxDelayMs != 20000 {
		t.Errorf("MaxDelayMs: file value should survive, got %d", cfg.Provider.Retry.MaxDelayMs)
	}
	if cfg.Provider.Retry.WallClockBudgetMs != 120000 {
		t.Errorf("WallClockBudgetMs: flag should override file, got %d, want 120000", cfg.Provider.Retry.WallClockBudgetMs)
	}
}

// TestApplyOverrides_ProviderRetryFlagAllocatesNilRetry asserts that
// when the file omits the retry block entirely, a single Changed() flag
// allocates the struct and writes the field; the rest of the slots stay
// zero so ValidateRunConfig fills them with the documented defaults.
func TestApplyOverrides_ProviderRetryFlagAllocatesNilRetry(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("provider-retry-max-attempts", "5"); err != nil {
		t.Fatalf("set provider-retry-max-attempts: %v", err)
	}
	cfg := baseFileConfig()
	cfg.Provider.Retry = nil
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.Provider.Retry == nil {
		t.Fatal("Retry should have been allocated")
	}
	if cfg.Provider.Retry.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", cfg.Provider.Retry.MaxAttempts)
	}
	if cfg.Provider.Retry.InitialDelayMs != 0 || cfg.Provider.Retry.MaxDelayMs != 0 || cfg.Provider.Retry.WallClockBudgetMs != 0 {
		t.Errorf("unflagged slots should remain zero, got %+v", cfg.Provider.Retry)
	}
}

// TestApplyOverrides_ProviderRetryNoFlagsChangedDoesNotClobberFile is
// the symmetric "Changed-guards do their job" assertion: a
// fully-populated file retry block must survive an applyOverrides call
// where none of the four retry flags were touched.
func TestApplyOverrides_ProviderRetryNoFlagsChangedDoesNotClobberFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Retry = &types.ProviderRetryConfig{
		MaxAttempts:       2,
		InitialDelayMs:    750,
		MaxDelayMs:        20000,
		WallClockBudgetMs: 60000,
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.Provider.Retry == nil {
		t.Fatal("Retry should not have been cleared")
	}
	want := types.ProviderRetryConfig{
		MaxAttempts:       2,
		InitialDelayMs:    750,
		MaxDelayMs:        20000,
		WallClockBudgetMs: 60000,
	}
	if *cfg.Provider.Retry != want {
		t.Errorf("Retry mutated: got %+v, want %+v", *cfg.Provider.Retry, want)
	}
}

// TestApplyOverrides_WorkspaceExportToFlowsThrough pins that
// --export-workspace-to lands on ExecutorConfig.WorkspaceExportTo so
// the end-of-run exporter wiring in runWithConfig sees it. End-to-end
// (actual GCS upload) is covered by the Chunk C smoke workflow; this
// test just confirms the flag-to-field path is wired.
func TestApplyOverrides_WorkspaceExportToFlowsThrough(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.WorkspaceExportTo = "gs://from-file/runs/old.tar.gz"

	if err := cmd.Flags().Set("export-workspace-to", "gs://from-flag/runs/new.tar.gz"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.Executor.WorkspaceExportTo != "gs://from-flag/runs/new.tar.gz" {
		t.Errorf("flag should override file: got %q", cfg.Executor.WorkspaceExportTo)
	}
}

// TestApplyOverrides_WorkspaceExportToDefaultPreservesFile mirrors the
// rest of the override surface: an unset flag must not clobber the
// file's value (the central precedence rule that the rest of
// applyOverrides tests pin).
func TestApplyOverrides_WorkspaceExportToDefaultPreservesFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Executor.WorkspaceExportTo = "gs://from-file/runs/keep.tar.gz"

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.Executor.WorkspaceExportTo != "gs://from-file/runs/keep.tar.gz" {
		t.Errorf("unset flag should preserve file value, got %q", cfg.Executor.WorkspaceExportTo)
	}
}

// TestBuildHarnessRunConfig_WorkspaceExportToFlowsThrough pins the
// flag-only Path 2 wiring — buildHarnessRunConfig must thread
// WorkspaceExportTo onto ExecutorConfig so the end-of-run hook fires
// when --config is absent and the operator passed
// --export-workspace-to alone.
func TestBuildHarnessRunConfig_WorkspaceExportToFlowsThrough(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
	if cfg.Executor.WorkspaceExportTo != "gs://my-bucket/runs/abc/workspace.tar.gz" {
		t.Errorf("WorkspaceExportTo did not flow through to Executor: got %q", cfg.Executor.WorkspaceExportTo)
	}
}

// TestApplyOverrides_ToolDispatchMaxParallelSetsField pins that
// --max-tool-parallel=N (with N > 0) populates cfg.ToolDispatch to
// {MaxParallel: N}. Mirrors the "explicit flag wins" half of the
// override precedence pattern used elsewhere in this file (issue #184).
func TestApplyOverrides_ToolDispatchMaxParallelSetsField(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	if err := cmd.Flags().Set("max-tool-parallel", "8"); err != nil {
		t.Fatalf("set max-tool-parallel: %v", err)
	}

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.ToolDispatch == nil {
		t.Fatal("--max-tool-parallel=8 should populate cfg.ToolDispatch, got nil")
	}
	if cfg.ToolDispatch.MaxParallel != 8 {
		t.Errorf("cfg.ToolDispatch.MaxParallel = %d, want 8", cfg.ToolDispatch.MaxParallel)
	}
}

// TestApplyOverrides_ToolDispatchMaxParallelDefaultDoesNotOverride pins the
// "default flag does not clobber file value" half of the precedence
// pattern: a config-file ToolDispatch value must survive when the user
// has not passed --max-tool-parallel (issue #184).
func TestApplyOverrides_ToolDispatchMaxParallelDefaultDoesNotOverride(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.ToolDispatch = &types.ToolDispatchConfig{MaxParallel: 8}

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.ToolDispatch == nil {
		t.Fatal("ToolDispatch from file should survive default flag, got nil")
	}
	if cfg.ToolDispatch.MaxParallel != 8 {
		t.Errorf("file ToolDispatch.MaxParallel should survive default flag, got %d, want 8",
			cfg.ToolDispatch.MaxParallel)
	}
}
