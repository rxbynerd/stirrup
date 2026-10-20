package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// newTestRunConfigCommand returns a fresh cobra command preloaded with
// the run-config flag surface, scoped per test so Changed() state does
// not leak into the process-global runConfigCmd.
func newTestRunConfigCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "run-config"}
	addRunConfigFlags(cmd)
	cmd.Flags().Bool("validate", false, "")
	cmd.Flags().Bool("compact", false, "")
	cmd.Flags().Bool("redact", false, "")
	return cmd
}

// runRunConfigForTest invokes the run-config command through its real
// runRunConfigWithIO entry point so the cobra flag-reading wiring
// (validate, redact, compact) is exercised end-to-end.
func runRunConfigForTest(t *testing.T, args []string, stdin string) (string, error) {
	t.Helper()
	must := require.New(t)
	cmd := newTestRunConfigCommand()
	must.NoError(cmd.ParseFlags(args))
	var reader io.Reader
	if stdin != "" {
		reader = strings.NewReader(stdin)
	}
	var buf bytes.Buffer
	if err := runRunConfigWithIO(cmd, cmd.Flags().Args(), reader, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// decodeRunConfigForTest parses command output as a RunConfig and fails
// the test when the output is not valid JSON.
func decodeRunConfigForTest(t *testing.T, out string) types.RunConfig {
	t.Helper()
	must := require.New(t)
	var cfg types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &cfg), "output is not parseable JSON:\n%s", out)
	return cfg
}

// TestRunRunConfig_FlagOnlyEmitsValidJSON pins that
// `stirrup run-config --mode execution --provider anthropic` produces
// a parseable RunConfig.
func TestRunRunConfig_FlagOnlyEmitsValidJSON(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	out, err := runRunConfigForTest(t, []string{
		"--mode", "execution",
		"--provider", "anthropic",
	}, "")
	must.NoError(err)
	cfg := decodeRunConfigForTest(t, out)
	check.Equal("execution", cfg.Mode)
	check.Equal("anthropic", cfg.Provider.Type)
}

func TestRunRunConfig_ReasoningSummaryFlagSetsField(t *testing.T) {
	must := require.New(t)
	out, err := runRunConfigForTest(t, []string{
		"--mode", "execution",
		"--provider", "anthropic",
		"--reasoning-summary", "off",
	}, "")
	must.NoError(err)
	cfg := decodeRunConfigForTest(t, out)
	must.Equal(types.ReasoningSummaryOff, cfg.ReasoningSummary)
}

// TestRunRunConfig_Idempotency pins that piping a config through
// `stirrup run-config` twice produces byte-identical output the second
// time: the first pass may apply default mutations, the second must not.
func TestRunRunConfig_Idempotency(t *testing.T) {
	must := require.New(t)
	pass1, err := runRunConfigForTest(t, []string{
		"--mode", "execution",
		"--model", "claude-opus-4-7",
	}, "")
	must.NoError(err, "pass 1")
	pass2, err := runRunConfigForTest(t, nil, pass1)
	must.NoError(err, "pass 2")
	must.Equal(pass1, pass2)
}

// TestRunRunConfig_ValidateRejectsInvalidConfig pins that --validate
// exits non-zero on a config the validator rejects: bedrock + the
// Anthropic API alias `claude-sonnet-4-6`, which validateBedrockProviderFields
// rejects with an inference-profile remediation hint.
func TestRunRunConfig_ValidateRejectsInvalidConfig(t *testing.T) {
	must := require.New(t)
	_, err := runRunConfigForTest(t, []string{
		"--validate",
		"--mode", "execution",
		"--provider", "bedrock",
		"--model", "claude-sonnet-4-6",
		"--prompt", "x",
	}, "")
	must.Error(err, "--validate should reject bedrock with the sonnet-4-6 alias")
	must.ErrorContains(err, "bedrock")
}

// TestRunRunConfig_ProviderDefaults pins the provider-derived defaults
// through the subcommand: the OpenAI key reference is filled in, and a
// non-Anthropic provider without --model is refused.
func TestRunRunConfig_ProviderDefaults(t *testing.T) {
	must := require.New(t)
	out, err := runRunConfigForTest(t, []string{
		"--provider", "openai-responses",
		"--model", "gpt-5.4-nano",
		"--compact",
	}, "")
	must.NoError(err)
	must.Contains(out, `"apiKeyRef":"secret://OPENAI_API_KEY"`)

	_, err = runRunConfigForTest(t, []string{"--provider", "openai-responses", "--compact"}, "")
	must.ErrorContains(err, "--provider=openai-responses", "missing --model error should name the provider")
}

// TestRunRunConfig_ValidateAcceptsValidConfig is the positive
// complement: --validate on a valid config emits the document with
// the validator's default-application mutations applied.
func TestRunRunConfig_ValidateAcceptsValidConfig(t *testing.T) {
	must := require.New(t)
	out, err := runRunConfigForTest(t, []string{
		"--validate",
		"--mode", "execution",
		"--prompt", "x",
	}, "")
	must.NoError(err)
	cfg := decodeRunConfigForTest(t, out)
	must.NotEmpty(cfg.RunID, "--validate should mint a RunID")
}

// TestRunRunConfig_ValidateInteractiveNoPromptShowsPlainError pins that
// run-config reports the actionable prompt-required error when no prompt
// is supplied, and never the interactive harness usage-hint sentinel.
func TestRunRunConfig_ValidateInteractiveNoPromptShowsPlainError(t *testing.T) {
	must := require.New(t)
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return true }
	defer func() { stderrIsInteractive = orig }()
	t.Setenv("STIRRUP_PROMPT", "")

	_, err := runRunConfigForTest(t, []string{
		"--validate",
		"--mode", "execution",
	}, "")
	must.Error(err)
	must.ErrorContains(err, "prompt is required")
	must.NotContains(err.Error(), "interactive prompt hint requested")
}

// TestRunRunConfig_CompactProducesSingleLine pins that --compact emits
// a single JSON line (no indentation), still newline-terminated.
func TestRunRunConfig_CompactProducesSingleLine(t *testing.T) {
	must := require.New(t)
	out, err := runRunConfigForTest(t, []string{
		"--compact",
		"--mode", "planning",
	}, "")
	must.NoError(err)
	body := strings.TrimRight(out, "\n")
	must.NotContains(body, "\n", "--compact output should be single-line")
	decodeRunConfigForTest(t, body)
}

// TestRunRunConfig_RedactScrubsSecretRefs pins that --redact rewrites
// secret:// references but leaves every other field intact. The
// captured config is no longer runnable as-is — that's the point.
func TestRunRunConfig_RedactScrubsSecretRefs(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	out, err := runRunConfigForTest(t, []string{
		"--redact",
		"--mode", "planning",
		"--api-key-ref", "secret://CUSTOM_KEY",
	}, "")
	must.NoError(err)
	check.Contains(out, "secret://[REDACTED]", "--redact should produce REDACTED placeholder")
	check.NotContains(out, "CUSTOM_KEY", "--redact should scrub the original secret reference")
}

// TestRunRunConfig_MalformedStdinJSONErrors pins that malformed stdin JSON
// is reported as a config parse error.
func TestRunRunConfig_MalformedStdinJSONErrors(t *testing.T) {
	must := require.New(t)
	_, err := runRunConfigForTest(t, nil, `{"this is not valid JSON`)
	must.ErrorContains(err, "parsing config")
}

// TestRunRunConfig_RoundTripFromExample pins idempotency against a real
// example file, mirroring the `cat config.json | stirrup run-config |
// stirrup run-config | diff - config.json` smoke-suite shape.
func TestRunRunConfig_RoundTripFromExample(t *testing.T) {
	must := require.New(t)
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "examples", "runconfig", "openai_responses.json"))
	if err != nil {
		t.Skipf("openai_responses.json example not present: %v", err)
	}
	pass1, err := runRunConfigForTest(t, nil, string(body))
	must.NoError(err, "pass 1")
	pass2, err := runRunConfigForTest(t, nil, pass1)
	must.NoError(err, "pass 2")
	must.Equal(pass1, pass2)
}

// TestRunRunConfig_WrapperThreadsOSIO pins that the runRunConfig cobra
// RunE thunk threads os.Stdin / os.Stdout into the testable
// runRunConfigWithIO helper.
func TestRunRunConfig_WrapperThreadsOSIO(t *testing.T) {
	must := require.New(t)
	cmd := newTestRunConfigCommand()
	must.NoError(cmd.ParseFlags([]string{"--mode", "planning"}))

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

	runErr := runRunConfig(cmd, cmd.Flags().Args())
	_ = w.Close()
	out := <-done
	must.NoError(runErr)

	cfg := decodeRunConfigForTest(t, out)
	must.Equal("planning", cfg.Mode)
}

// TestRunRunConfigWithIO_FlagBranches exercises every cobra flag-reading
// branch inside runRunConfigWithIO (validate, redact, compact, plain)
// through the real entry point, one branch per row.
func TestRunRunConfigWithIO_FlagBranches(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(t *testing.T, out string)
	}{
		{
			name: "flag-only emits indented JSON",
			args: []string{"--mode", "planning", "--prompt", "x"},
			check: func(t *testing.T, out string) {
				must := require.New(t)
				must.Contains(out, "\n  ", "default output should be indented")
				cfg := decodeRunConfigForTest(t, out)
				must.Equal("planning", cfg.Mode)
			},
		},
		{
			name: "compact produces single-line JSON",
			args: []string{"--compact", "--mode", "planning"},
			check: func(t *testing.T, out string) {
				must := require.New(t)
				body := strings.TrimRight(out, "\n")
				must.NotContains(body, "\n", "--compact output should be single-line")
				decodeRunConfigForTest(t, body)
			},
		},
		{
			name: "redact rewrites secret references",
			args: []string{"--redact", "--mode", "planning", "--api-key-ref", "secret://CUSTOM_KEY"},
			check: func(t *testing.T, out string) {
				must := require.New(t)
				must.Contains(out, "secret://[REDACTED]", "--redact should produce REDACTED placeholder")
				must.NotContains(out, "CUSTOM_KEY", "--redact should scrub the original secret reference")
			},
		},
		{
			name:    "validate rejects invalid config",
			args:    []string{"--validate", "--mode", "execution", "--provider", "bedrock", "--prompt", "x"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestRunConfigCommand()
			must.NoError(cmd.ParseFlags(tc.args))
			var buf bytes.Buffer
			err := runRunConfigWithIO(cmd, cmd.Flags().Args(), nil, &buf)
			if tc.wantErr {
				must.Error(err, "output: %s", buf.String())
				return
			}
			must.NoError(err)
			if tc.check != nil {
				tc.check(t, buf.String())
			}
		})
	}
}

// TestRunRunConfig_EmptyEditStrategyDefaultsToMulti pins the
// edit-strategy default behaviour: --validate (ResolveAll) fills
// EditStrategy.Type = "multi", while without it (ResolveBase) the
// empty value is preserved so chained `run-config | run-config | ...`
// stages stay idempotent.
func TestRunRunConfig_EmptyEditStrategyDefaultsToMulti(t *testing.T) {
	t.Run("validate fills default", func(t *testing.T) {
		must := require.New(t)
		out, err := runRunConfigForTest(t, []string{
			"--validate",
			"--mode", "execution",
			"--prompt", "test",
		}, "")
		must.NoError(err)
		cfg := decodeRunConfigForTest(t, out)
		must.Equal("multi", cfg.EditStrategy.Type, "subcommand default must match CLI and validation")
	})

	t.Run("base mode preserves empty for chaining", func(t *testing.T) {
		must := require.New(t)
		out, err := runRunConfigForTest(t, []string{
			"--mode", "execution",
			"--prompt", "test",
		}, "")
		must.NoError(err)
		cfg := decodeRunConfigForTest(t, out)
		must.Empty(cfg.EditStrategy.Type, "ResolveBase must not apply defaults a downstream stage could override")
	})
}
