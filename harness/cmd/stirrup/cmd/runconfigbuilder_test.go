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

// TestBuildRunConfig_AnthropicWIFWarnFiresOnce pins MF-1: BuildRunConfig
// must invoke applyAnthropicWIFOverrides exactly once. The pre-fix code
// invoked it both inside applyOverrides AND directly in BuildRunConfig,
// so the "config already specifies tokenSource" diagnostic was emitted
// twice per invocation. This test captures slog output and asserts the
// warning appears once.
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

// TestWriteRunConfigJSON_WriteError pins SF-5: writeRunConfigJSON must
// surface a write error from either of its two Write call sites
// (payload bytes and trailing newline) rather than silently producing
// a half-written capture file.
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
	})
}

// TestIsStdinPiped_NamedPipeReturnsTrue pins SF-4: an os.Pipe reader,
// whose Stat() reports os.ModeNamedPipe, must be treated as piped.
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

// TestIsStdinPiped_RegularFileReturnsTrue pins SF-4: a redirected
// regular file (`< config.json`) must be treated as piped.
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

// TestIsStdinPiped_StatErrorReturnsFalse pins SF-4: a closed file fd
// whose Stat() errors must be treated as non-piped (the conservative
// default — better to fall through to flag-only than to attempt a
// read on an unusable fd).
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

// TestIsStdinPiped_CharDeviceReturnsFalse pins SF-4 and the documented
// `go test` deviation: a character device (the shape `go test` hands
// its children as stdin) must NOT be treated as piped. Removing this
// branch would re-introduce false-positive activation in every
// harness_test.go fixture.
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

// TestBuildRunConfig_ApplyOverridesError pins SF-6: a malformed
// --query-param entry surfaces through BuildRunConfig as a non-nil
// error. Before, applyOverrides was only tested directly; this
// exercises the error-propagation arm of BuildRunConfig itself.
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

// TestBuildRunConfig_WIFOverridesError pins SF-6: a WIF flag set
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

// TestBuildRunConfig_EnvVarSuppliesBasePath pins the #241 fourth-rank
// fallback: when --config is absent and stdin is not piped,
// STIRRUP_CONFIG=<path> loads the file the flag would have loaded.
// This is the "set once per shell session" ergonomic the env var
// exists to provide.
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

// TestBuildRunConfig_ExplicitConfigBeatsEnvVar pins acceptance
// criterion 2: when both --config and STIRRUP_CONFIG name a file, the
// explicit flag wins. The env var must not silently leak its base
// into a flag-driven invocation, or operators with the env var set
// in their shell profile cannot override it ad-hoc.
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

// TestBuildRunConfig_EnvVarDashOptsIntoStdin pins acceptance
// criterion 3: STIRRUP_CONFIG=- treats the env var as a stdin opt-in,
// mirroring `--config -`. Combined with a piped stdin, this loads the
// base from the pipe rather than failing as ambiguous — the env var
// is opting *into* the stdin path, not naming a separate source.
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

// TestBuildRunConfig_EnvVarPathPlusStdinIsAmbiguous pins acceptance
// criterion 4: STIRRUP_CONFIG=<path> plus piped stdin must fail
// loudly, the same way --config <path> + piped stdin already does.
// The error must cite both sources — the env var by name and the
// pipe — so the operator knows which source to remove.
//
// Additionally pins B1: no debug log line claiming STIRRUP_CONFIG
// was the chosen source may fire on the ambiguity path. Emitting
// such a trace immediately before returning the ambiguity error
// would directly contradict the error message.
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

// TestBuildRunConfig_EnvVarDashRequiresStdin pins the mirror of
// `--config -` without a pipe: STIRRUP_CONFIG=- with a TTY stdin is
// nonsense (nothing to read). The error must name the env var so the
// operator knows the env var, not the absent --config flag, is the
// source of the failure.
//
// Pins S2: when sources.Stdin is nil (embedded-API path with no
// reader threaded through), the error must NOT claim "stdin is a
// terminal" because that diagnosis is wrong — nil means no reader
// was provided, not that a TTY is attached.
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
// builder falls through to flag-only construction. An empty value
// must not be treated as a path (loadRunConfigFile would error on
// the empty string) nor as the stdin opt-in.
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

// TestBuildRunConfig_EnvVarDebugLogged pins the spec's "single
// slog.Debug line so operators can audit precedence" requirement: when
// the env var is the chosen source, BuildRunConfig must emit a Debug
// log entry that mentions STIRRUP_CONFIG and carries the structured
// "path" field so log consumers can filter on a stable key.
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

// TestBuildRunConfig_EnvVarDashDebugLogged mirrors the path-branch
// log assertion for the STIRRUP_CONFIG=- form: the same message and
// the same structured "path" key (value "-") must fire so log
// consumers can filter on a single key regardless of which env-var
// shape the operator chose.
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

// TestBuildRunConfig_EnvVarPathNotFound pins S1: when STIRRUP_CONFIG
// names a file that does not exist, the error must name the env var
// so the operator can tell whether the bad path came from --config or
// the env var. Without this wrap an operator with both --config in
// muscle memory and STIRRUP_CONFIG in their shell profile would chase
// the wrong source.
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

// TestBuildRunConfig_EnvVarLeadingWhitespaceTrimmed pins S5: a
// STIRRUP_CONFIG value with leading or trailing whitespace must be
// trimmed before use. Shell env-var editors and CI secret stores
// routinely smuggle in a stray newline or space; the alternative
// behaviour ("open  /path: no such file" with a spurious leading
// space) is a worse failure than treating the trimmed value as
// authoritative. This test pins the trim contract so the choice
// cannot silently regress.
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
// end-to-end CLI default for the edit strategy. A bare flag-only
// invocation (no --edit-strategy flag) must land on "multi" after the
// full Resolve == ResolveAll pipeline, matching direct RunConfig
// embedding (TestValidateRunConfig_EditStrategyDefaultsToMulti) and the
// run-config subcommand (TestRunRunConfig_EmptyEditStrategyDefaultsToMulti).
// Tests fail if CLI and validation defaults ever diverge again.
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
