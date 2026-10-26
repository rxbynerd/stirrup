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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// minimalRunConfigJSON is the smallest base RunConfig the tests pipe
// through BuildRunConfig, so assertions about flag overrides aren't
// confused by mode-default mutations.
func minimalRunConfigJSON(t *testing.T) string {
	t.Helper()
	must := require.New(t)
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
	must.NoError(err, "marshal minimal RunConfig")
	return string(b)
}

// TestBuildRunConfig_FlagOnlyMatchesBuildHarnessRunConfig pins that the
// shared builder produces a config equivalent to buildHarnessRunConfig
// for the no-base case.
func TestBuildRunConfig_FlagOnlyMatchesBuildHarnessRunConfig(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "execution",
		"--model", "claude-opus-4-7",
		"--max-turns", "42",
		"--prompt", "do the thing",
	}))

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   nil,
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	must.NoError(err)

	check.Equal("execution", cfg.Mode)
	check.Equal("claude-opus-4-7", cfg.ModelRouter.Model)
	check.Equal(42, cfg.MaxTurns)
	check.Equal("do the thing", cfg.Prompt)
	check.NotEmpty(cfg.RunID, "ResolveAll should generate a RunID")
	check.NoError(types.ValidateRunConfig(cfg), "ResolveAll output should validate")
}

// TestBuildRunConfig_ToolsProfileFlag pins that --tools-profile threads
// onto RunConfig.Tools.Profile on the flag-only path and that omitting
// it leaves the default (empty) identity profile.
func TestBuildRunConfig_ToolsProfileFlag(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "x",
		"--tools-profile", "coding-classic",
	}))
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	must.NoError(err)
	must.Equal("coding-classic", cfg.Tools.Profile)
	must.NoError(types.ValidateRunConfig(cfg), "config with tools-profile should validate")

	// Without the flag, the profile stays empty (the default identity
	// presentation).
	bare := newTestHarnessCommand()
	must.NoError(bare.ParseFlags([]string{"--mode", "execution", "--prompt", "x"}))
	bareCfg, err := BuildRunConfig(RunConfigSources{Cmd: bare, Resolve: ResolveAll})
	must.NoError(err)
	must.Empty(bareCfg.Tools.Profile, "profile should default to empty without --tools-profile")
}

func TestBuildRunConfig_ProviderEndpointProfileFlag(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "x",
		"--provider", "openai-responses",
		"--model", "gpt-5.5",
		"--api-key-ref", "secret://OPENAI_API_KEY",
		"--base-url", "https://relay.example.com/v1",
		"--provider-endpoint-profile", "openai",
	}))
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	must.NoError(err)
	must.Equal(types.EndpointProfileOpenAI, cfg.Provider.EndpointProfile)
	must.NoError(types.ValidateRunConfig(cfg))

	bare := newTestHarnessCommand()
	must.NoError(bare.ParseFlags([]string{"--mode", "execution", "--prompt", "x"}))
	bareCfg, err := BuildRunConfig(RunConfigSources{Cmd: bare, Resolve: ResolveAll})
	must.NoError(err)
	must.Empty(bareCfg.Provider.EndpointProfile)
}

// TestBuildRunConfig_ToolsProfileOverridesBase pins that --tools-profile
// overrides a base config's tools.profile and that omitting the flag
// preserves the base value (the applyOverrides Changed() guard).
func TestBuildRunConfig_ToolsProfileOverridesBase(t *testing.T) {
	must := require.New(t)
	var base types.RunConfig
	must.NoError(json.Unmarshal([]byte(minimalRunConfigJSON(t)), &base))
	base.Tools.Profile = "coding-classic"
	baseJSON, err := json.Marshal(base)
	must.NoError(err)

	t.Run("base profile preserved without flag", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		must.NoError(cmd.ParseFlags([]string{"--max-turns", "5"}))
		cfg, err := BuildRunConfig(RunConfigSources{
			Stdin: strings.NewReader(string(baseJSON)), Cmd: cmd, Resolve: ResolveBase,
		})
		must.NoError(err)
		must.Equal("coding-classic", cfg.Tools.Profile)
	})

	t.Run("flag overrides base profile", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		must.NoError(cmd.ParseFlags([]string{"--tools-profile", "default"}))
		cfg, err := BuildRunConfig(RunConfigSources{
			Stdin: strings.NewReader(string(baseJSON)), Cmd: cmd, Resolve: ResolveBase,
		})
		must.NoError(err)
		must.Equal("default", cfg.Tools.Profile)
	})
}

// TestBuildRunConfig_StdinBaseWithFlagOverride exercises the canonical
// pipeline shape: a JSON RunConfig arrives on stdin and an explicit
// flag overrides one field. The base survives intact for every field
// the flag did not touch.
func TestBuildRunConfig_StdinBaseWithFlagOverride(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	base := minimalRunConfigJSON(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--max-turns", "99",
	}))

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(base),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)

	check.Equal(99, cfg.MaxTurns, "flag override")
	check.Equal("secret://FILE_KEY", cfg.Provider.APIKeyRef, "base preserved")
	check.Equal("prompt from file", cfg.Prompt, "base preserved")
	check.Equal("from-file", cfg.RunID, "ResolveBase must not mint a new ID")
}

// TestBuildRunConfig_EscalationMaxRetriesBasePreserved pins the
// Changed() guard: a base config that sets
// toolChoiceEscalation.maxRetries must survive when the operator
// passes no --escalate-tool-choice-max-retries flag.
func TestBuildRunConfig_EscalationMaxRetriesBasePreserved(t *testing.T) {
	must := require.New(t)
	var base types.RunConfig
	must.NoError(json.Unmarshal([]byte(minimalRunConfigJSON(t)), &base))
	base.ToolChoiceEscalation = &types.ToolChoiceEscalationConfig{Enabled: true, MaxRetries: 3}
	raw, err := json.Marshal(base)
	must.NoError(err)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--max-turns", "5"}))

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(string(raw)),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)
	must.NotNil(cfg.ToolChoiceEscalation, "base ToolChoiceEscalation must survive")
	must.True(cfg.ToolChoiceEscalation.Enabled, "base Enabled=true must survive")
	must.Equal(3, cfg.ToolChoiceEscalation.MaxRetries, "base preserved, no flag passed")
}

// TestBuildRunConfig_ConfigPathLoadsBase pins that --config <path> loads
// the base RunConfig from that file.
func TestBuildRunConfig_ConfigPathLoadsBase(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	must.NoError(os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600))

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		ConfigPath: path,
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	must.NoError(err)
	must.Equal("from-file", cfg.RunID)
}

// TestBuildRunConfig_StdinPlusConfigFileAreAmbiguous pins that
// --config <path> with piped stdin fails loudly rather than silently
// picking one base; the error mentions both sources.
func TestBuildRunConfig_StdinPlusConfigFileAreAmbiguous(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	must.NoError(os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600))

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:      strings.NewReader(minimalRunConfigJSON(t)),
		ConfigPath: path,
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	must.ErrorContains(err, "ambiguous")
	must.ErrorContains(err, path)
	must.ErrorContains(err, "stdin")
}

// TestBuildRunConfig_ExplicitDashRequiresStdin pins that `--config -`
// with no piped stdin (simulated with a nil Stdin) errors rather than
// reading the dash literal as a path.
func TestBuildRunConfig_ExplicitDashRequiresStdin(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:      nil,
		ConfigPath: "-",
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	must.ErrorContains(err, "--config -")
}

// TestBuildRunConfig_ResolveBaseSkipsModeDefaults pins that
// ResolveBase leaves PermissionPolicy and Tools.BuiltIn empty even for
// a read-only mode, since a later pipeline stage may pivot to --mode
// execution; ResolveAll populates them.
func TestBuildRunConfig_ResolveBaseSkipsModeDefaults(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "planning",
		"--prompt", "x",
	}))

	t.Run("resolve base leaves mode defaults unset", func(t *testing.T) {
		must := require.New(t)
		base, err := BuildRunConfig(RunConfigSources{
			Cmd:     cmd,
			Resolve: ResolveBase,
		})
		must.NoError(err)
		must.Empty(base.PermissionPolicy.Type, "ResolveBase should leave PermissionPolicy unset on planning")
		must.Empty(base.Tools.BuiltIn, "ResolveBase should leave Tools.BuiltIn empty on planning")
	})

	t.Run("resolve all populates mode defaults", func(t *testing.T) {
		must := require.New(t)
		all, err := BuildRunConfig(RunConfigSources{
			Cmd:     cmd,
			Resolve: ResolveAll,
		})
		must.NoError(err)
		must.Equal("deny-side-effects", all.PermissionPolicy.Type)
		must.NotEmpty(all.Tools.BuiltIn, "ResolveAll planning should populate Tools.BuiltIn")
	})
}

// TestBuildRunConfig_ResolveAllRequiresPrompt pins that the harness
// path's prompt-required error fires through the shared builder.
func TestBuildRunConfig_ResolveAllRequiresPrompt(t *testing.T) {
	must := require.New(t)
	// Pin the interactive seam to false: on a TTY, resolvePromptForRun
	// returns the errPromptHintRequested sentinel instead, which would
	// make this test pass in CI but fail on a developer's terminal.
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return false }
	defer func() { stderrIsInteractive = orig }()

	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--mode", "execution"}))
	t.Setenv("STIRRUP_PROMPT", "")

	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	must.ErrorContains(err, "prompt is required")
}

// TestBuildRunConfig_ResolveBaseAllowsEmptyPrompt pins that a chained
// `run-config` stage may emit a config without a prompt, since the
// final `harness` stage supplies one.
func TestBuildRunConfig_ResolveBaseAllowsEmptyPrompt(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--mode", "execution"}))
	t.Setenv("STIRRUP_PROMPT", "")

	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)
	must.Empty(cfg.Prompt, "ResolveBase should leave an empty prompt empty")
}

// TestBuildRunConfig_RejectsUnknownStdinFields exercises the
// DisallowUnknownFields safeguard: a typo in piped JSON must fail
// loudly, not silently drop the unknown field.
func TestBuildRunConfig_RejectsUnknownStdinFields(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(`{"mode":"planning","not_a_field":true}`),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.ErrorContains(err, "not_a_field")
}

// TestBuildRunConfig_StdinSizeCap pins that the 1 MiB cap on
// loadRunConfigFile is reused for stdin so a hostile / runaway piped
// input cannot OOM the harness.
func TestBuildRunConfig_StdinSizeCap(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "exceeds")
}

// TestBuildRunConfig_EmptyStdinErrors pins that explicit --config -
// with a non-TTY but empty reader errors loudly rather than silently
// falling through to flag-only.
func TestBuildRunConfig_EmptyStdinErrors(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:      strings.NewReader(""),
		ConfigPath: "-",
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	must.ErrorContains(err, "empty")
}

// TestBuildRunConfig_FlagOverridesBaseWIFInference pins that
// --azure-tenant-id implies credential.type=azure-workload-identity,
// exercised from a stdin-base path to prove the WIF folding step runs
// for file/stdin bases too, not just flag-only.
func TestBuildRunConfig_FlagOverridesBaseWIFInference(t *testing.T) {
	must := require.New(t)
	base := minimalRunConfigJSON(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--provider", "openai-responses",
		"--azure-tenant-id", "11111111-1111-1111-1111-111111111111",
		"--azure-client-id", "22222222-2222-2222-2222-222222222222",
		"--base-url", "https://example.openai.azure.com/openai/v1",
	}))

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(base),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)
	must.NotNil(cfg.Provider.Credential, "--azure-tenant-id should imply a Credential block")
	must.Equal("azure-workload-identity", cfg.Provider.Credential.Type)
	must.NotEmpty(cfg.Provider.Credential.AzureTenantID, "Credential.AzureTenantID should be populated")
}

// TestBuildRunConfig_PositionalPromptOverlay confirms the positional
// prompt argument is consumed by ResolveAll's prompt chain even when
// it arrives via Sources.Args (no --prompt flag, no stdin prompt).
func TestBuildRunConfig_PositionalPromptOverlay(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{"--mode", "execution"}))
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Args:    []string{"positional prompt"},
		Resolve: ResolveAll,
	})
	must.NoError(err)
	must.Equal("positional prompt", cfg.Prompt)
}

// TestWriteRunConfigJSON_RoundTrip pins that the writer emits a
// document the existing loader can read back.
func TestWriteRunConfigJSON_RoundTrip(t *testing.T) {
	must := require.New(t)
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
	must.NoError(writeRunConfigJSON(&buf, cfg, false))
	must.True(strings.HasSuffix(buf.String(), "\n"), "output should end with a trailing newline")

	var rc types.RunConfig
	must.NoError(json.Unmarshal(buf.Bytes(), &rc), "round-trip unmarshal")
	must.Equal(cfg.RunID, rc.RunID, "RunID lost in round-trip")

	// Compact output must not be indented but must still end with a newline.
	buf.Reset()
	must.NoError(writeRunConfigJSON(&buf, cfg, true))
	must.NotContains(buf.String(), "\n  ", "compact output should not be indented")
	must.True(strings.HasSuffix(buf.String(), "\n"), "compact output should still end with a trailing newline")
}

// TestBuildRunConfig_AnthropicWIFWarnFiresOnce pins that BuildRunConfig
// invokes applyAnthropicWIFOverrides exactly once, so the "config
// already specifies tokenSource" diagnostic is not emitted twice.
func TestBuildRunConfig_AnthropicWIFWarnFiresOnce(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("anthropic-from-github-actions", "true"))

	_, err = BuildRunConfig(RunConfigSources{
		Stdin:   bytes.NewReader(body),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)

	count := strings.Count(logBuf.String(), "--anthropic-from-github-actions ignored")
	must.Equal(1, count, "WIF warn should fire exactly once, logs:\n%s", logBuf.String())
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
		must := require.New(t)
		w := &failWriter{failOn: 1, failWith: io.ErrClosedPipe}
		err := writeRunConfigJSON(w, cfg, false)
		must.Error(err, "payload write error should propagate")
		must.ErrorContains(err, "write RunConfig")
		// A write failure is an I/O error (exit 3): the marshal succeeded
		// but the bytes did not reach the sink.
		must.Equal(exitIO, classifyExitCode(err), "write failure should map to the I/O exit code; err=%v", err)
	})

	t.Run("second write (newline) fails", func(t *testing.T) {
		must := require.New(t)
		w := &failWriter{failOn: 2, failWith: io.ErrClosedPipe}
		err := writeRunConfigJSON(w, cfg, false)
		must.Error(err, "newline write error should propagate")
		must.ErrorContains(err, "write RunConfig")
		must.Equal(exitIO, classifyExitCode(err), "write failure should map to the I/O exit code; err=%v", err)
	})
}

// TestIsStdinPiped_NamedPipeReturnsTrue pins that an os.Pipe reader,
// whose Stat() reports os.ModeNamedPipe, is treated as piped.
func TestIsStdinPiped_NamedPipeReturnsTrue(t *testing.T) {
	must := require.New(t)
	r, w, err := os.Pipe()
	must.NoError(err, "create pipe")
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	must.True(isStdinPiped(r), "named pipe should be detected as piped")
}

// TestIsStdinPiped_RegularFileReturnsTrue pins that a redirected
// regular file (`< config.json`) is treated as piped.
func TestIsStdinPiped_RegularFileReturnsTrue(t *testing.T) {
	must := require.New(t)
	f, err := os.CreateTemp(t.TempDir(), "in-*.json")
	must.NoError(err, "create temp file")
	t.Cleanup(func() { _ = f.Close() })
	must.True(isStdinPiped(f), "regular file should be detected as piped")
}

// TestIsStdinPiped_StatErrorReturnsFalse pins that a closed file fd
// whose Stat() errors is treated as non-piped — the conservative
// default: fall through to flag-only rather than read an unusable fd.
func TestIsStdinPiped_StatErrorReturnsFalse(t *testing.T) {
	must := require.New(t)
	f, err := os.CreateTemp(t.TempDir(), "stat-err-*")
	must.NoError(err, "create temp file")
	must.NoError(f.Close(), "close temp file")
	must.False(isStdinPiped(f), "closed file (Stat errors) should not be treated as piped")
}

// TestIsStdinPiped_CharDeviceReturnsFalse pins that a character device
// (the shape `go test` hands its children as stdin) is NOT treated as
// piped — otherwise every harness_test.go fixture false-positives.
func TestIsStdinPiped_CharDeviceReturnsFalse(t *testing.T) {
	must := require.New(t)
	f, err := os.Open(os.DevNull)
	must.NoError(err, "open /dev/null")
	t.Cleanup(func() { _ = f.Close() })
	must.False(isStdinPiped(f), "character device (/dev/null) should not be treated as piped")
}

// TestBuildRunConfig_ApplyOverridesError pins that a malformed
// --query-param entry surfaces through BuildRunConfig as a non-nil
// error.
func TestBuildRunConfig_ApplyOverridesError(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.Flags().Set("query-param", "no-equals-sign"), "set query-param")
	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.ErrorContains(err, "query-param")
}

// TestBuildRunConfig_WIFOverridesError pins that a WIF flag set
// alongside an explicit --api-key-ref surfaces through BuildRunConfig
// as a non-nil error.
func TestBuildRunConfig_WIFOverridesError(t *testing.T) {
	must := require.New(t)
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
		must.NoError(cmd.Flags().Set(name, val), "set %s", name)
	}
	mustSet("anthropic-federation-rule-id", "fdrl_rule")
	mustSet("anthropic-organization-id", "11111111-1111-1111-1111-111111111111")
	mustSet("anthropic-service-account-id", "svac_sa")
	mustSet("api-key-ref", "secret://EXPLICIT_KEY")

	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.ErrorContains(err, "api-key-ref")
}

// TestBuildRunConfig_EnvVarSuppliesBasePath pins that when --config is
// absent and stdin is not piped, STIRRUP_CONFIG=<path> loads the file
// the flag would have loaded.
func TestBuildRunConfig_EnvVarSuppliesBasePath(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	must.NoError(os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600), "write config file")
	t.Setenv("STIRRUP_CONFIG", path)

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)
	must.Equal("from-file", cfg.RunID, "env-var base should load")
}

// TestBuildRunConfig_ExplicitConfigBeatsEnvVar pins that when both
// --config and STIRRUP_CONFIG name a file, the explicit flag wins.
func TestBuildRunConfig_ExplicitConfigBeatsEnvVar(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err, "marshal env config")
	must.NoError(os.WriteFile(envPath, envBytes, 0o600), "write env config")
	must.NoError(os.WriteFile(flagPath, []byte(minimalRunConfigJSON(t)), 0o600), "write flag config")
	t.Setenv("STIRRUP_CONFIG", envPath)

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		ConfigPath: flagPath,
		Cmd:        cmd,
		Resolve:    ResolveBase,
	})
	must.NoError(err)
	must.Equal("from-file", cfg.RunID, "explicit --config must beat STIRRUP_CONFIG")
}

// TestBuildRunConfig_EnvVarDashOptsIntoStdin pins that
// STIRRUP_CONFIG=- treats the env var as a stdin opt-in, mirroring
// `--config -`: combined with piped stdin, it loads the base from the
// pipe rather than failing as ambiguous.
func TestBuildRunConfig_EnvVarDashOptsIntoStdin(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_CONFIG", "-")

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:   strings.NewReader(minimalRunConfigJSON(t)),
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)
	must.Equal("from-file", cfg.RunID, "stdin should load via STIRRUP_CONFIG=-")
}

// TestBuildRunConfig_EnvVarPathPlusStdinIsAmbiguous pins that
// STIRRUP_CONFIG=<path> plus piped stdin fails loudly, citing both
// sources, the same way --config <path> + piped stdin does. Also
// pins that no debug log line claiming STIRRUP_CONFIG was chosen
// fires on the ambiguity path — that would contradict the error.
func TestBuildRunConfig_EnvVarPathPlusStdinIsAmbiguous(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	must.NoError(os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600), "write config file")
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
	must.ErrorContains(err, "STIRRUP_CONFIG")
	must.ErrorContains(err, "stdin")
	must.ErrorContains(err, path)
	must.NotContains(logBuf.String(), "STIRRUP_CONFIG", "ambiguity path must not emit a chosen-source debug trace")
}

// TestBuildRunConfig_EnvVarDashRequiresStdin pins that
// STIRRUP_CONFIG=- with a TTY stdin errors, naming the env var as the
// source. When sources.Stdin is nil (embedded-API path with no reader
// threaded through), the error must NOT claim "stdin is a terminal" —
// nil means no reader was provided, not that a TTY is attached.
func TestBuildRunConfig_EnvVarDashRequiresStdin(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_CONFIG", "-")

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Stdin:   nil,
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.ErrorContains(err, "STIRRUP_CONFIG=-")
	must.NotContains(err.Error(), "terminal", "nil Stdin must not be diagnosed as a terminal")
	must.ErrorContains(err, "no stdin reader")
}

// TestBuildRunConfig_EnvVarEmptyIgnored pins that an empty
// STIRRUP_CONFIG="" behaves identically to an unset env var — the
// builder falls through to flag-only construction.
func TestBuildRunConfig_EnvVarEmptyIgnored(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_CONFIG", "")

	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "x",
	}))
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	must.NoError(err)
	must.Equal("execution", cfg.Mode, "empty env var should be ignored")
}

// TestBuildRunConfig_EnvVarDebugLogged pins that when the env var is
// the chosen source, BuildRunConfig emits a Debug log entry mentioning
// STIRRUP_CONFIG with a structured "path" field.
func TestBuildRunConfig_EnvVarDebugLogged(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	must.NoError(os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600), "write config file")
	t.Setenv("STIRRUP_CONFIG", path)

	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err)

	out := logBuf.String()
	must.Contains(out, "STIRRUP_CONFIG")
	must.Contains(out, "path=", "debug log should carry a structured path field")
}

// TestBuildRunConfig_EnvVarDashDebugLogged mirrors
// TestBuildRunConfig_EnvVarDebugLogged for the STIRRUP_CONFIG=- form:
// the same "path" key (value "-") must fire.
func TestBuildRunConfig_EnvVarDashDebugLogged(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_CONFIG", "-")

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
	must.NoError(err)

	out := logBuf.String()
	must.Contains(out, "STIRRUP_CONFIG")
	must.Contains(out, "path=-", "debug log should carry a path=- structured field")
}

// TestBuildRunConfig_EnvVarPathNotFound pins that when STIRRUP_CONFIG
// names a file that does not exist, the error names the env var so the
// operator can tell it apart from a --config failure.
func TestBuildRunConfig_EnvVarPathNotFound(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_CONFIG", "/no/such/stirrup/config/file.json")

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.ErrorContains(err, "STIRRUP_CONFIG")
}

// TestBuildRunConfig_EnvVarLeadingWhitespaceTrimmed pins that a
// STIRRUP_CONFIG value with leading or trailing whitespace is trimmed
// before use — shell env-var editors and CI secret stores routinely
// smuggle in a stray newline or space.
func TestBuildRunConfig_EnvVarLeadingWhitespaceTrimmed(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	must.NoError(os.WriteFile(path, []byte(minimalRunConfigJSON(t)), 0o600), "write config file")
	t.Setenv("STIRRUP_CONFIG", "  "+path+"\n")

	cmd := newTestHarnessCommand()
	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveBase,
	})
	must.NoError(err, "BuildRunConfig should trim whitespace and load the file")
	must.Equal("from-file", cfg.RunID, "trimmed env path should resolve")
}

// TestBuildRunConfig_EmptyEditStrategyResolvesToMulti pins the
// end-to-end CLI default for the edit strategy: a bare flag-only
// invocation lands on "multi" after ResolveAll, matching
// TestValidateRunConfig_EditStrategyDefaultsToMulti and
// TestRunRunConfig_EmptyEditStrategyDefaultsToMulti.
func TestBuildRunConfig_EmptyEditStrategyResolvesToMulti(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "test",
	}))

	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	must.NoError(err)
	must.Equal("multi", cfg.EditStrategy.Type, "CLI default must match the validation default")
}

// TestBuildRunConfig_ExplicitEditStrategyPreserved confirms the
// override path: an operator who selects --edit-strategy explicitly
// still gets their selection rather than being silently rewritten to
// the validation default.
func TestBuildRunConfig_ExplicitEditStrategyPreserved(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	must.NoError(cmd.ParseFlags([]string{
		"--mode", "execution",
		"--prompt", "test",
		"--edit-strategy", "whole-file",
	}))

	cfg, err := BuildRunConfig(RunConfigSources{
		Cmd:     cmd,
		Resolve: ResolveAll,
	})
	must.NoError(err)
	must.Equal("whole-file", cfg.EditStrategy.Type, "explicit selection must survive defaulting")
}
