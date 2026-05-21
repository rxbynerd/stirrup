package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// minimalRunConfigJSON is the smallest base RunConfig the tests pipe
// through BuildRunConfig. It carries only the fields a downstream
// pipeline stage cannot reasonably default — provider/credential
// shape, prompt — so the assertions about flag overrides aren't
// confused by mode-default mutations.
func minimalRunConfigJSON(t *testing.T) string {
	t.Helper()
	cfg := types.RunConfig{
		RunID:  "from-file",
		Mode:   "planning",
		Prompt: "prompt from file",
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://FILE_KEY",
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
		LogLevel: "info",
	}
	t300 := 300
	cfg.Timeout = &t300
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal minimal RunConfig: %v", err)
	}
	return string(b)
}

// TestBuildRunConfig_FlagOnlyMatchesBuildHarnessRunConfig pins that
// the shared builder produces a config equivalent to today's
// buildHarnessRunConfig for the no-base case. Without this assertion
// the refactor could silently drift the flag-only path (the
// pre-refactor Path 2) from the rest of the resolver.
func TestBuildRunConfig_FlagOnlyMatchesBuildHarnessRunConfig(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--mode", "execution",
		"--model", "claude-opus-4-7",
		"--max-turns", "42",
		"--prompt", "do the thing",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   nil,
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}

	if cfg.Mode != "execution" {
		t.Errorf("Mode = %q, want execution", cfg.Mode)
	}
	if cfg.ModelRouter.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", cfg.ModelRouter.Model)
	}
	if cfg.MaxTurns != 42 {
		t.Errorf("MaxTurns = %d, want 42", cfg.MaxTurns)
	}
	if cfg.Prompt != "do the thing" {
		t.Errorf("Prompt = %q, want %q", cfg.Prompt, "do the thing")
	}
	if cfg.RunID == "" {
		t.Errorf("ResolveAll should generate a RunID, got empty")
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Errorf("ResolveAll output should validate, got: %v", err)
	}
}

// TestBuildRunConfig_StdinBaseWithFlagOverride exercises the canonical
// pipeline shape: a JSON RunConfig arrives on stdin and an explicit
// flag overrides one field. The base survives intact for every field
// the flag did not touch.
func TestBuildRunConfig_StdinBaseWithFlagOverride(t *testing.T) {
	base := minimalRunConfigJSON(t)
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--max-turns", "99",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(base),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}

	if cfg.MaxTurns != 99 {
		t.Errorf("MaxTurns = %d, want 99 (flag override)", cfg.MaxTurns)
	}
	if cfg.Provider.APIKeyRef != "secret://FILE_KEY" {
		t.Errorf("APIKeyRef = %q, want secret://FILE_KEY (base preserved)", cfg.Provider.APIKeyRef)
	}
	if cfg.Prompt != "prompt from file" {
		t.Errorf("Prompt = %q, want %q (base preserved)", cfg.Prompt, "prompt from file")
	}
	if cfg.RunID != "from-file" {
		t.Errorf("RunID = %q, want from-file (ResolveBase must not mint a new ID)", cfg.RunID)
	}
}

// TestBuildRunConfig_FilePath verifies the file-loading branch. The
// path coverage matters because loadRunConfigFile already had a code
// path for file reads pre-refactor — the test confirms BuildRunConfig
// reaches it the same way.
func TestBuildRunConfig_FilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		ConfigPath: path,
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.RunID != "from-file" {
		t.Errorf("RunID = %q, want from-file", cfg.RunID)
	}
}

// TestBuildRunConfig_StdinPlusConfigFileAreAmbiguous pins acceptance
// criterion 6: --config <path> with piped stdin must fail loudly
// rather than silently picking one base. The error mentions both
// sources so the operator can fix whichever was unintentional.
func TestBuildRunConfig_StdinPlusConfigFileAreAmbiguous(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:      strings.NewReader(minimalRunConfigJSON(t)),
		ConfigPath: path,
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	if err == nil {
		t.Fatal("expected error for --config + piped stdin combination")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should describe ambiguity, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should mention config path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "stdin") {
		t.Errorf("error should mention stdin, got: %v", err)
	}
}

// TestBuildRunConfig_ExplicitDashRequiresStdin pins the explicit
// `--config -` failure mode the spec calls out: when stdin is a TTY
// (or, in our test harness, simulated with a nil Stdin), reading the
// dash literal as a path is nonsense. The error tells the operator
// the redirection is missing.
func TestBuildRunConfig_ExplicitDashRequiresStdin(t *testing.T) {
	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:      nil,
		ConfigPath: "-",
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	if err == nil {
		t.Fatal("expected error for --config - with no piped stdin")
	}
	if !strings.Contains(err.Error(), "--config -") {
		t.Errorf("error should cite --config -, got: %v", err)
	}
}

// TestBuildRunConfig_ResolveBaseSkipsModeDefaults pins the
// pipeline-friendly contract: ResolveBase leaves PermissionPolicy and
// Tools.BuiltIn empty even for a read-only mode because a later
// pipeline stage may pivot to --mode execution where those defaults
// would be wrong. ResolveAll, by contrast, populates them — that's
// what the harness path needs before invoking the loop.
func TestBuildRunConfig_ResolveBaseSkipsModeDefaults(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--mode", "planning",
		"--prompt", "x",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	base, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig(ResolveBase): %v", err)
	}
	if base.PermissionPolicy.Type != "" {
		t.Errorf("ResolveBase should leave PermissionPolicy unset on planning, got %q", base.PermissionPolicy.Type)
	}
	if len(base.Tools.BuiltIn) != 0 {
		t.Errorf("ResolveBase should leave Tools.BuiltIn empty on planning, got %v", base.Tools.BuiltIn)
	}

	all, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig(ResolveAll): %v", err)
	}
	if all.PermissionPolicy.Type != "deny-side-effects" {
		t.Errorf("ResolveAll planning should default to deny-side-effects, got %q", all.PermissionPolicy.Type)
	}
	if len(all.Tools.BuiltIn) == 0 {
		t.Error("ResolveAll planning should populate Tools.BuiltIn")
	}
}

// TestBuildRunConfig_ResolveAllRequiresPrompt pins that the harness
// path's existing prompt-required error fires through the shared
// builder. The message text matches harness_test.go's existing prompt
// fixtures so the error string is part of the API contract.
func TestBuildRunConfig_ResolveAllRequiresPrompt(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{"--mode", "execution"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	t.Setenv("STIRRUP_PROMPT", "")

	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	if err == nil {
		t.Fatal("ResolveAll with no prompt should fail")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("error should be the documented prompt-required message, got: %v", err)
	}
}

// TestBuildRunConfig_ResolveBaseAllowsEmptyPrompt pins the
// pipeline-stage contract: a chained `run-config` stage must be
// allowed to emit a config without a prompt because the final
// `harness` stage will supply one via --prompt / positional /
// --prompt-file / STIRRUP_PROMPT.
func TestBuildRunConfig_ResolveBaseAllowsEmptyPrompt(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{"--mode", "execution"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	t.Setenv("STIRRUP_PROMPT", "")

	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Prompt != "" {
		t.Errorf("ResolveBase should leave empty prompt as empty, got %q", cfg.Prompt)
	}
}

// TestBuildRunConfig_RejectsUnknownStdinFields exercises the
// DisallowUnknownFields safeguard on the stdin reader. Typos in a
// piped JSON should fail loudly with a parse error, not silently
// drop the unknown field.
func TestBuildRunConfig_RejectsUnknownStdinFields(t *testing.T) {
	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(`{"mode":"planning","not_a_field":true}`),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err == nil {
		t.Fatal("expected DisallowUnknownFields rejection")
	}
	if !strings.Contains(err.Error(), "not_a_field") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

// TestBuildRunConfig_StdinSizeCap pins that the 1 MiB cap on
// loadRunConfigFile is reused for stdin so a hostile / runaway piped
// input cannot OOM the harness.
func TestBuildRunConfig_StdinSizeCap(t *testing.T) {
	// 1 MiB + 1 KiB of whitespace-padded JSON. The DisallowUnknownFields
	// decoder rejects the padding anyway, but the size guard must trip
	// first — that's what we're pinning.
	padding := bytes.Repeat([]byte(" "), int(maxConfigFileBytes)+1024)
	body := append([]byte(`{"mode":"planning"}`), padding...)

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:   bytes.NewReader(body),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err == nil {
		t.Fatal("expected size-cap rejection")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention the size cap, got: %v", err)
	}
}

// TestBuildRunConfig_EmptyStdinErrors pins the spec's "non-TTY but
// empty" failure mode for explicit --config -. With ConfigPath = "-"
// and an empty reader, the builder must error loudly rather than
// silently falling through to flag-only.
func TestBuildRunConfig_EmptyStdinErrors(t *testing.T) {
	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:      strings.NewReader(""),
		ConfigPath: "-",
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	if err == nil {
		t.Fatal("expected empty-stdin rejection")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty input, got: %v", err)
	}
}

// TestBuildRunConfig_FlagOverridesBaseWIFInference exercises one of
// the cross-field invariants the spec calls out: --azure-tenant-id
// implies credential.type=azure-workload-identity. Reaching this from
// a stdin-base path proves the WIF folding step runs for both file/
// stdin and flag-only bases — without that, an Azure operator piping
// `run-config --azure-tenant-id ...` into `harness --prompt ...`
// would get a config without the implied credential block.
func TestBuildRunConfig_FlagOverridesBaseWIFInference(t *testing.T) {
	base := minimalRunConfigJSON(t)
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--provider", "openai-responses",
		"--azure-tenant-id", "11111111-1111-1111-1111-111111111111",
		"--azure-client-id", "22222222-2222-2222-2222-222222222222",
		"--base-url", "https://example.openai.azure.com/openai/v1",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(base),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Provider.Credential == nil {
		t.Fatal("--azure-tenant-id should imply a Credential block")
	}
	if cfg.Provider.Credential.Type != "azure-workload-identity" {
		t.Errorf("Credential.Type = %q, want azure-workload-identity", cfg.Provider.Credential.Type)
	}
	if cfg.Provider.Credential.AzureTenantID == "" {
		t.Error("Credential.AzureTenantID should be populated")
	}
}

// TestBuildRunConfig_PositionalPromptOverlay confirms the positional
// prompt argument is consumed by ResolveAll's prompt chain even when
// it arrives via Sources.Args (no --prompt flag, no stdin prompt).
func TestBuildRunConfig_PositionalPromptOverlay(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{"--mode", "execution"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Args:    []string{"positional prompt"},
		Resolve: ResolveAll,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Prompt != "positional prompt" {
		t.Errorf("Prompt = %q, want %q", cfg.Prompt, "positional prompt")
	}
}

// TestWriteRunConfigJSON_RoundTrip pins that the writer emits a
// document the existing loader can read back. Belt-and-braces against
// a hypothetical future tweak to the marshal indent / trailing newline
// that breaks downstream `stirrup run-config` consumers.
func TestWriteRunConfigJSON_RoundTrip(t *testing.T) {
	cfg := &types.RunConfig{
		RunID:        "test-run",
		Mode:         "planning",
		Prompt:       "x",
		Provider:     types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		ModelRouter:  types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		Transport:    types.TransportConfig{Type: "stdio"},
		Executor:     types.ExecutorConfig{Type: "local"},
		EditStrategy: types.EditStrategyConfig{Type: "multi"},
		MaxTurns:     20,
	}
	var buf bytes.Buffer
	if err := writeRunConfigJSON(&buf, cfg, false); err != nil {
		t.Fatalf("writeRunConfigJSON: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output should end with a trailing newline")
	}

	// Re-read it.
	var rc types.RunConfig
	if err := json.Unmarshal(buf.Bytes(), &rc); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if rc.RunID != cfg.RunID {
		t.Errorf("RunID lost in round-trip: got %q want %q", rc.RunID, cfg.RunID)
	}

	// Compact variant: must NOT contain the indent string, but must
	// still parse and still end with a newline.
	buf.Reset()
	if err := writeRunConfigJSON(&buf, cfg, true); err != nil {
		t.Fatalf("writeRunConfigJSON(compact): %v", err)
	}
	if strings.Contains(buf.String(), "\n  ") {
		t.Errorf("compact output should not be indented, got %q", buf.String())
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("compact output should still end with a trailing newline")
	}
}
