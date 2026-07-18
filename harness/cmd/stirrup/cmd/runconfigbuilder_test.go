package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// minimalRunConfigJSON is the smallest base RunConfig the tests pipe
// through BuildRunConfig, so assertions about flag overrides aren't
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

// TestBuildRunConfig_FlagOnlyMatchesBuildHarnessRunConfig pins that the
// shared builder produces a config equivalent to buildHarnessRunConfig
// for the no-base case.
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

// TestBuildRunConfig_ToolsProfileFlag pins that --tools-profile threads
// onto RunConfig.Tools.Profile on the flag-only path and that omitting
// it leaves the default (empty) identity profile.
func TestBuildRunConfig_ToolsProfileFlag(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "x",
		"--tools-profile", "coding-classic",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Tools.Profile != "coding-classic" {
		t.Errorf("Tools.Profile = %q, want coding-classic", cfg.Tools.Profile)
	}
	if err := types.ValidateRunConfig(cfg); err != nil {
		t.Errorf("config with tools-profile should validate, got: %v", err)
	}

	// Without the flag, the profile stays empty (the default identity
	// presentation).
	bare := newTestHarnessCommand()
	if err := bare.ParseFlags([]string{"--mode", "execution", "--prompt", "x"}); err != nil {
		t.Fatalf("ParseFlags(bare): %v", err)
	}
	bareCfg, err := BuildRunConfig(RunConfigSources{Cmd: bare, Resolve: ResolveAll})
	if err != nil {
		t.Fatalf("BuildRunConfig(bare): %v", err)
	}
	if bareCfg.Tools.Profile != "" {
		t.Errorf("bare Tools.Profile = %q, want empty (default)", bareCfg.Tools.Profile)
	}
}

// TestBuildRunConfig_ToolsProfileOverridesBase pins that --tools-profile
// overrides a base config's tools.profile and that omitting the flag
// preserves the base value (the applyOverrides Changed() guard).
func TestBuildRunConfig_ToolsProfileOverridesBase(t *testing.T) {
	var base types.RunConfig
	if err := json.Unmarshal([]byte(minimalRunConfigJSON(t)), &base); err != nil {
		t.Fatalf("unmarshal base: %v", err)
	}
	base.Tools.Profile = "coding-classic"
	baseJSON, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}

	// No flag: base profile preserved.
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{"--max-turns", "5"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin: strings.NewReader(string(baseJSON)), Cmd: cmd, Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Tools.Profile != "coding-classic" {
		t.Errorf("base profile not preserved: got %q", cfg.Tools.Profile)
	}

	// Explicit flag wins over the base.
	cmd2 := newTestHarnessCommand()
	if err := cmd2.ParseFlags([]string{"--tools-profile", "default"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg2, err := BuildRunConfig(RunConfigSources{
		Stdin: strings.NewReader(string(baseJSON)), Cmd: cmd2, Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg2.Tools.Profile != "default" {
		t.Errorf("flag did not override base profile: got %q", cfg2.Tools.Profile)
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

// TestBuildRunConfig_EscalationMaxRetriesBasePreserved pins the
// Changed() guard: a base config that sets
// toolChoiceEscalation.maxRetries must survive when the operator
// passes no --escalate-tool-choice-max-retries flag.
func TestBuildRunConfig_EscalationMaxRetriesBasePreserved(t *testing.T) {
	var base types.RunConfig
	if err := json.Unmarshal([]byte(minimalRunConfigJSON(t)), &base); err != nil {
		t.Fatalf("unmarshal base: %v", err)
	}
	base.ToolChoiceEscalation = &types.ToolChoiceEscalationConfig{Enabled: true, MaxRetries: 3}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}

	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{"--max-turns", "5"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(string(raw)),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.ToolChoiceEscalation == nil {
		t.Fatal("base ToolChoiceEscalation must survive, got nil")
	}
	if !cfg.ToolChoiceEscalation.Enabled {
		t.Error("base Enabled=true must survive")
	}
	if cfg.ToolChoiceEscalation.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3 (base preserved, no flag passed)", cfg.ToolChoiceEscalation.MaxRetries)
	}
}

// TestBuildRunConfig_FilePath verifies the file-loading branch.
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

// TestBuildRunConfig_StdinPlusConfigFileAreAmbiguous pins that
// --config <path> with piped stdin fails loudly rather than silently
// picking one base; the error mentions both sources.
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

// TestBuildRunConfig_ExplicitDashRequiresStdin pins that `--config -`
// with no piped stdin (simulated with a nil Stdin) errors rather than
// reading the dash literal as a path.
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

// TestBuildRunConfig_ResolveBaseSkipsModeDefaults pins that
// ResolveBase leaves PermissionPolicy and Tools.BuiltIn empty even for
// a read-only mode, since a later pipeline stage may pivot to --mode
// execution; ResolveAll populates them.
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
// path's prompt-required error fires through the shared builder.
func TestBuildRunConfig_ResolveAllRequiresPrompt(t *testing.T) {
	// Pin the interactive seam to false: on a TTY, resolvePromptForRun
	// returns the errPromptHintRequested sentinel instead, which would
	// make this test pass in CI but fail on a developer's terminal.
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return false }
	defer func() { stderrIsInteractive = orig }()

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

// TestBuildRunConfig_ResolveBaseAllowsEmptyPrompt pins that a chained
// `run-config` stage may emit a config without a prompt, since the
// final `harness` stage supplies one.
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
// DisallowUnknownFields safeguard: a typo in piped JSON must fail
// loudly, not silently drop the unknown field.
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

// TestBuildRunConfig_EmptyStdinErrors pins that explicit --config -
// with a non-TTY but empty reader errors loudly rather than silently
// falling through to flag-only.
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

// TestBuildRunConfig_FlagOverridesBaseWIFInference pins that
// --azure-tenant-id implies credential.type=azure-workload-identity,
// exercised from a stdin-base path to prove the WIF folding step runs
// for file/stdin bases too, not just flag-only.
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
// document the existing loader can read back.
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

// TestBuildRunConfig_AnthropicWIFWarnFiresOnce pins that BuildRunConfig
// invokes applyAnthropicWIFOverrides exactly once, so the "config
// already specifies tokenSource" diagnostic is not emitted twice.
func TestBuildRunConfig_AnthropicWIFWarnFiresOnce(t *testing.T) {
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Base config carries an explicit credential.tokenSource so the
	// helper's "operator opt-in ignored because file already set source"
	// warning path fires.
	base := types.RunConfig{
		RunID:  "from-file",
		Mode:   "execution",
		Prompt: "test prompt",
		Provider: types.ProviderConfig{
			Type: "anthropic",
			Credential: &types.CredentialConfig{
				Type:             "anthropic-wif",
				FederationRuleID: "fdrl_filerule",
				OrganizationID:   "11111111-1111-1111-1111-111111111111",
				ServiceAccountID: "svac_filesa",
				TokenSource: &types.TokenSourceConfig{
					Type: "file",
					Path: "/var/run/file/jwt",
				},
			},
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
		MaxTurns:        10,
		LogLevel:        "info",
	}
	t300 := 300
	base.Timeout = &t300
	body, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("anthropic-from-github-actions", "true"); err != nil {
		t.Fatalf("set: %v", err)
	}

	_, err = BuildRunConfig(RunConfigSources{
		Stdin:   bytes.NewReader(body),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}

	count := strings.Count(logBuf.String(), "--anthropic-from-github-actions ignored")
	if count != 1 {
		t.Errorf("WIF warn should fire exactly once, got %d:\n%s", count, logBuf.String())
	}
}

// failWriter returns an error on the Nth write (1-based). Used to
// exercise writeRunConfigJSON's two distinct Write call sites.
type failWriter struct {
	calls    int
	failOn   int
	failWith error
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failOn {
		return 0, w.failWith
	}
	return len(p), nil
}

// TestWriteRunConfigJSON_WriteError pins that writeRunConfigJSON
// surfaces a write error from either of its two Write call sites
// (payload bytes and trailing newline).
func TestWriteRunConfigJSON_WriteError(t *testing.T) {
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

	t.Run("first write (payload) fails", func(t *testing.T) {
		w := &failWriter{failOn: 1, failWith: io.ErrClosedPipe}
		err := writeRunConfigJSON(w, cfg, false)
		if err == nil {
			t.Fatal("expected write error to propagate")
		}
		if !strings.Contains(err.Error(), "write RunConfig") {
			t.Errorf("error should describe the write failure, got: %v", err)
		}
		// A write failure is an I/O error (exit 3): the marshal succeeded
		// but the bytes did not reach the sink.
		if got := classifyExitCode(err); got != exitIO {
			t.Errorf("classifyExitCode = %d, want %d (I/O); err=%v", got, exitIO, err)
		}
	})

	t.Run("second write (newline) fails", func(t *testing.T) {
		w := &failWriter{failOn: 2, failWith: io.ErrClosedPipe}
		err := writeRunConfigJSON(w, cfg, false)
		if err == nil {
			t.Fatal("expected newline write error to propagate")
		}
		if !strings.Contains(err.Error(), "write RunConfig") {
			t.Errorf("error should describe the newline write failure, got: %v", err)
		}
		if got := classifyExitCode(err); got != exitIO {
			t.Errorf("classifyExitCode = %d, want %d (I/O); err=%v", got, exitIO, err)
		}
	})
}

// TestIsStdinPiped_NamedPipeReturnsTrue pins that an os.Pipe reader,
// whose Stat() reports os.ModeNamedPipe, is treated as piped.
func TestIsStdinPiped_NamedPipeReturnsTrue(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	if !isStdinPiped(r) {
		t.Error("named pipe should be detected as piped")
	}
}

// TestIsStdinPiped_RegularFileReturnsTrue pins that a redirected
// regular file (`< config.json`) is treated as piped.
func TestIsStdinPiped_RegularFileReturnsTrue(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "in-*.json")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if !isStdinPiped(f) {
		t.Error("regular file should be detected as piped")
	}
}

// TestIsStdinPiped_StatErrorReturnsFalse pins that a closed file fd
// whose Stat() errors is treated as non-piped — the conservative
// default: fall through to flag-only rather than read an unusable fd.
func TestIsStdinPiped_StatErrorReturnsFalse(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stat-err-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if isStdinPiped(f) {
		t.Error("closed file (Stat errors) should not be treated as piped")
	}
}

// TestIsStdinPiped_CharDeviceReturnsFalse pins that a character device
// (the shape `go test` hands its children as stdin) is NOT treated as
// piped — otherwise every harness_test.go fixture false-positives.
func TestIsStdinPiped_CharDeviceReturnsFalse(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isStdinPiped(f) {
		t.Error("character device (/dev/null) should not be treated as piped")
	}
}

// TestBuildRunConfig_ApplyOverridesError pins that a malformed
// --query-param entry surfaces through BuildRunConfig as a non-nil
// error.
func TestBuildRunConfig_ApplyOverridesError(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("query-param", "no-equals-sign"); err != nil {
		t.Fatalf("set query-param: %v", err)
	}
	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err == nil {
		t.Fatal("expected malformed --query-param to surface as error")
	}
	if !strings.Contains(err.Error(), "query-param") {
		t.Errorf("error should mention query-param, got: %v", err)
	}
}

// TestBuildRunConfig_WIFOverridesError pins that a WIF flag set
// alongside an explicit --api-key-ref surfaces through BuildRunConfig
// as a non-nil error.
func TestBuildRunConfig_WIFOverridesError(t *testing.T) {
	for _, name := range []string{
		"ANTHROPIC_FEDERATION_RULE_ID",
		"ANTHROPIC_ORGANIZATION_ID",
		"ANTHROPIC_SERVICE_ACCOUNT_ID",
		"ANTHROPIC_WORKSPACE_ID",
		"ANTHROPIC_IDENTITY_TOKEN_FILE",
		"ANTHROPIC_IDENTITY_TOKEN",
	} {
		t.Setenv(name, "")
	}

	cmd := newTestHarnessCommand()
	mustSet := func(name, val string) {
		if err := cmd.Flags().Set(name, val); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	mustSet("anthropic-federation-rule-id", "fdrl_rule")
	mustSet("anthropic-organization-id", "11111111-1111-1111-1111-111111111111")
	mustSet("anthropic-service-account-id", "svac_sa")
	mustSet("api-key-ref", "secret://EXPLICIT_KEY")

	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err == nil {
		t.Fatal("expected WIF + explicit api-key-ref to surface as error")
	}
	if !strings.Contains(err.Error(), "api-key-ref") {
		t.Errorf("error should mention api-key-ref, got: %v", err)
	}
}

// TestBuildRunConfig_EnvVarSuppliesBasePath pins that when --config is
// absent and stdin is not piped, STIRRUP_CONFIG=<path> loads the file
// the flag would have loaded.
func TestBuildRunConfig_EnvVarSuppliesBasePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	t.Setenv("STIRRUP_CONFIG", path)

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.RunID != "from-file" {
		t.Errorf("RunID = %q, want from-file (env-var base should load)", cfg.RunID)
	}
}

// TestBuildRunConfig_ExplicitConfigBeatsEnvVar pins that when both
// --config and STIRRUP_CONFIG name a file, the explicit flag wins.
func TestBuildRunConfig_ExplicitConfigBeatsEnvVar(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env.json")
	flagPath := filepath.Join(dir, "flag.json")

	envCfg := types.RunConfig{
		RunID:           "from-env",
		Mode:            "planning",
		Prompt:          "env prompt",
		Provider:        types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ENV_KEY"},
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
		Tools:    types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
		MaxTurns: 10,
		LogLevel: "info",
	}
	envBytes, err := json.Marshal(envCfg)
	if err != nil {
		t.Fatalf("marshal env cfg: %v", err)
	}
	if err := os.WriteFile(envPath, envBytes, 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	if err := os.WriteFile(flagPath, []byte(minimalRunConfigJSON(t)), 0o600); err != nil {
		t.Fatalf("write flag file: %v", err)
	}
	t.Setenv("STIRRUP_CONFIG", envPath)

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		ConfigPath: flagPath,
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.RunID != "from-file" {
		t.Errorf("RunID = %q, want from-file (explicit --config must beat STIRRUP_CONFIG)", cfg.RunID)
	}
}

// TestBuildRunConfig_EnvVarDashOptsIntoStdin pins that
// STIRRUP_CONFIG=- treats the env var as a stdin opt-in, mirroring
// `--config -`: combined with piped stdin, it loads the base from the
// pipe rather than failing as ambiguous.
func TestBuildRunConfig_EnvVarDashOptsIntoStdin(t *testing.T) {
	t.Setenv("STIRRUP_CONFIG", "-")

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(minimalRunConfigJSON(t)),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.RunID != "from-file" {
		t.Errorf("RunID = %q, want from-file (stdin should load via STIRRUP_CONFIG=-)", cfg.RunID)
	}
}

// TestBuildRunConfig_EnvVarPathPlusStdinIsAmbiguous pins that
// STIRRUP_CONFIG=<path> plus piped stdin fails loudly, citing both
// sources, the same way --config <path> + piped stdin does. Also
// pins that no debug log line claiming STIRRUP_CONFIG was chosen
// fires on the ambiguity path — that would contradict the error.
func TestBuildRunConfig_EnvVarPathPlusStdinIsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	t.Setenv("STIRRUP_CONFIG", path)

	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(minimalRunConfigJSON(t)),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err == nil {
		t.Fatal("expected STIRRUP_CONFIG=<path> + piped stdin to be rejected")
	}
	if !strings.Contains(err.Error(), "STIRRUP_CONFIG") {
		t.Errorf("error should name STIRRUP_CONFIG, got: %v", err)
	}
	if !strings.Contains(err.Error(), "stdin") {
		t.Errorf("error should cite stdin, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should cite the env-var path, got: %v", err)
	}
	if strings.Contains(logBuf.String(), "STIRRUP_CONFIG") {
		t.Errorf("ambiguity path must not emit a chosen-source debug trace, got:\n%s", logBuf.String())
	}
}

// TestBuildRunConfig_EnvVarDashRequiresStdin pins that
// STIRRUP_CONFIG=- with a TTY stdin errors, naming the env var as the
// source. When sources.Stdin is nil (embedded-API path with no reader
// threaded through), the error must NOT claim "stdin is a terminal" —
// nil means no reader was provided, not that a TTY is attached.
func TestBuildRunConfig_EnvVarDashRequiresStdin(t *testing.T) {
	t.Setenv("STIRRUP_CONFIG", "-")

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:   nil,
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err == nil {
		t.Fatal("expected STIRRUP_CONFIG=- with no piped stdin to be rejected")
	}
	if !strings.Contains(err.Error(), "STIRRUP_CONFIG=-") {
		t.Errorf("error should cite STIRRUP_CONFIG=-, got: %v", err)
	}
	if strings.Contains(err.Error(), "terminal") {
		t.Errorf("nil Stdin must not be diagnosed as a terminal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no stdin reader") {
		t.Errorf("error should describe the missing reader, got: %v", err)
	}
}

// TestBuildRunConfig_EnvVarEmptyIgnored pins that an empty
// STIRRUP_CONFIG="" behaves identically to an unset env var — the
// builder falls through to flag-only construction.
func TestBuildRunConfig_EnvVarEmptyIgnored(t *testing.T) {
	t.Setenv("STIRRUP_CONFIG", "")

	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "x",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.Mode != "execution" {
		t.Errorf("Mode = %q, want execution (empty env var should be ignored)", cfg.Mode)
	}
}

// TestBuildRunConfig_EnvVarDebugLogged pins that when the env var is
// the chosen source, BuildRunConfig emits a Debug log entry mentioning
// STIRRUP_CONFIG with a structured "path" field.
func TestBuildRunConfig_EnvVarDebugLogged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	t.Setenv("STIRRUP_CONFIG", path)

	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cmd := newTestHarnessCommand()
	if _, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	}); err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}

	out := logBuf.String()
	if !strings.Contains(out, "STIRRUP_CONFIG") {
		t.Errorf("debug log should mention STIRRUP_CONFIG, got:\n%s", out)
	}
	if !strings.Contains(out, "path=") {
		t.Errorf("debug log should carry structured path field, got:\n%s", out)
	}
}

// TestBuildRunConfig_EnvVarDashDebugLogged mirrors
// TestBuildRunConfig_EnvVarDebugLogged for the STIRRUP_CONFIG=- form:
// the same "path" key (value "-") must fire.
func TestBuildRunConfig_EnvVarDashDebugLogged(t *testing.T) {
	t.Setenv("STIRRUP_CONFIG", "-")

	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cmd := newTestHarnessCommand()
	if _, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(minimalRunConfigJSON(t)),
		Cmd:     cmd,
		Resolve: ResolveBase,
	}); err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}

	out := logBuf.String()
	if !strings.Contains(out, "STIRRUP_CONFIG") {
		t.Errorf("debug log should mention STIRRUP_CONFIG, got:\n%s", out)
	}
	if !strings.Contains(out, "path=-") {
		t.Errorf("debug log should carry path=- structured field, got:\n%s", out)
	}
}

// TestBuildRunConfig_EnvVarPathNotFound pins that when STIRRUP_CONFIG
// names a file that does not exist, the error names the env var so the
// operator can tell it apart from a --config failure.
func TestBuildRunConfig_EnvVarPathNotFound(t *testing.T) {
	t.Setenv("STIRRUP_CONFIG", "/no/such/stirrup/config/file.json")

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err == nil {
		t.Fatal("expected STIRRUP_CONFIG with non-existent path to fail")
	}
	if !strings.Contains(err.Error(), "STIRRUP_CONFIG") {
		t.Errorf("error should name STIRRUP_CONFIG so operator knows the source, got: %v", err)
	}
}

// TestBuildRunConfig_EnvVarLeadingWhitespaceTrimmed pins that a
// STIRRUP_CONFIG value with leading or trailing whitespace is trimmed
// before use — shell env-var editors and CI secret stores routinely
// smuggle in a stray newline or space.
func TestBuildRunConfig_EnvVarLeadingWhitespaceTrimmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	t.Setenv("STIRRUP_CONFIG", "  "+path+"\n")

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig should trim whitespace and load file, got: %v", err)
	}
	if cfg.RunID != "from-file" {
		t.Errorf("RunID = %q, want from-file (trimmed env path should resolve)", cfg.RunID)
	}
}

// TestBuildRunConfig_EmptyEditStrategyResolvesToMulti pins the
// end-to-end CLI default for the edit strategy: a bare flag-only
// invocation lands on "multi" after ResolveAll, matching
// TestValidateRunConfig_EditStrategyDefaultsToMulti and
// TestRunRunConfig_EmptyEditStrategyDefaultsToMulti.
func TestBuildRunConfig_EmptyEditStrategyResolvesToMulti(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "test",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.EditStrategy.Type != "multi" {
		t.Errorf("EditStrategy.Type = %q, want multi (CLI default must match validation default)", cfg.EditStrategy.Type)
	}
}

// TestBuildRunConfig_ExplicitEditStrategyPreserved confirms the
// override path: an operator who selects --edit-strategy explicitly
// still gets their selection rather than being silently rewritten to
// the validation default.
func TestBuildRunConfig_ExplicitEditStrategyPreserved(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "test",
		"--edit-strategy", "whole-file",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.EditStrategy.Type != "whole-file" {
		t.Errorf("EditStrategy.Type = %q, want whole-file (explicit selection must survive defaulting)", cfg.EditStrategy.Type)
	}
}
