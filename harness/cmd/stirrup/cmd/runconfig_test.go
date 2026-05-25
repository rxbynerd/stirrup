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

	"github.com/rxbynerd/stirrup/types"
)

// newTestRunConfigCommand returns a fresh cobra command preloaded with
// the run-config flag surface. The real runConfigCmd is process-global
// and a test that calls SetArgs / ParseFlags on it would pollute
// neighbour tests; a per-test command keeps the Changed() state
// scoped.
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
// (validate, redact, compact) is exercised end-to-end. The previous
// implementation replicated the function body inline, which left the
// production glue at 0% coverage.
func runRunConfigForTest(t *testing.T, args []string, stdin string) (string, error) {
	t.Helper()
	cmd := newTestRunConfigCommand()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
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

// TestRunRunConfig_FlagOnlyEmitsValidJSON pins acceptance criterion 1:
// `stirrup run-config --mode execution --provider anthropic` produces
// a parseable RunConfig.
func TestRunRunConfig_FlagOnlyEmitsValidJSON(t *testing.T) {
	out, err := runRunConfigForTest(t, []string{
		"--mode", "execution",
		"--provider", "anthropic",
	}, "")
	if err != nil {
		t.Fatalf("runRunConfig: %v", err)
	}
	var cfg types.RunConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("emitted output is not parseable JSON: %v\n%s", err, out)
	}
	if cfg.Mode != "execution" {
		t.Errorf("Mode = %q, want execution", cfg.Mode)
	}
	if cfg.Provider.Type != "anthropic" {
		t.Errorf("Provider.Type = %q, want anthropic", cfg.Provider.Type)
	}
}

// TestRunRunConfig_Idempotency pins acceptance criterion 3: piping a
// config through `stirrup run-config` twice produces byte-identical
// output the second time. The first pass may apply default mutations;
// the second pass must not move the document any further.
func TestRunRunConfig_Idempotency(t *testing.T) {
	pass1, err := runRunConfigForTest(t, []string{
		"--mode", "execution",
		"--model", "claude-opus-4-7",
	}, "")
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	pass2, err := runRunConfigForTest(t, nil, pass1)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if pass1 != pass2 {
		t.Errorf("run-config is not idempotent\npass1:\n%s\npass2:\n%s", pass1, pass2)
	}
}

// TestRunRunConfig_ValidateRejectsInvalidConfig pins acceptance
// criterion 2: --validate causes the subcommand to exit non-zero on a
// config the validator rejects. We pick a contradiction the validator
// flags reliably: bedrock + the CLI-default `claude-sonnet-4-6` is
// the issue #65 trap, which validateBedrockProviderFields rejects
// with an inference-profile remediation hint.
func TestRunRunConfig_ValidateRejectsInvalidConfig(t *testing.T) {
	_, err := runRunConfigForTest(t, []string{
		"--validate",
		"--mode", "execution",
		"--provider", "bedrock",
		"--prompt", "x",
	}, "")
	if err == nil {
		t.Fatal("expected --validate to reject bedrock + sonnet-4-6 alias")
	}
	if !strings.Contains(err.Error(), "bedrock") {
		t.Errorf("error should mention bedrock, got: %v", err)
	}
}

// TestRunRunConfig_ValidateAcceptsValidConfig is the positive
// complement: --validate on a valid config emits the document with
// the validator's default-application mutations applied.
func TestRunRunConfig_ValidateAcceptsValidConfig(t *testing.T) {
	out, err := runRunConfigForTest(t, []string{
		"--validate",
		"--mode", "execution",
		"--prompt", "x",
	}, "")
	if err != nil {
		t.Fatalf("runRunConfig: %v", err)
	}
	var cfg types.RunConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.RunID == "" {
		t.Error("--validate path should mint a RunID (ResolveAll)")
	}
}

// TestRunRunConfig_ValidateInteractiveNoPromptShowsPlainError pins the
// fix for the run-config sentinel leak (issue #249 review): the
// interactive harness usage-hint must NOT fire for run-config. When
// `stirrup run-config --validate` reaches the prompt-required gate on a
// TTY with no prompt, resolvePromptForRun returns errPromptHintRequested
// — runRunConfigWithIO must substitute the actionable plain
// "prompt is required: ..." error rather than leaking the sentinel's
// internal "interactive prompt hint requested" string to the operator.
func TestRunRunConfig_ValidateInteractiveNoPromptShowsPlainError(t *testing.T) {
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return true }
	defer func() { stderrIsInteractive = orig }()
	t.Setenv("STIRRUP_PROMPT", "")

	_, err := runRunConfigForTest(t, []string{
		"--validate",
		"--mode", "execution",
	}, "")
	if err == nil {
		t.Fatal("run-config --validate with no prompt should fail")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("error should be the actionable prompt-required message, got: %v", err)
	}
	// The sentinel's internal string must never reach the operator.
	if strings.Contains(err.Error(), "interactive prompt hint requested") {
		t.Errorf("run-config leaked the harness usage-hint sentinel: %v", err)
	}
}

// TestRunRunConfig_CompactProducesSingleLine pins acceptance of the
// --compact flag: the body is a single JSON line (no indentation),
// still terminated by a newline so shell pipelines see a clean EOF.
func TestRunRunConfig_CompactProducesSingleLine(t *testing.T) {
	out, err := runRunConfigForTest(t, []string{
		"--compact",
		"--mode", "planning",
	}, "")
	if err != nil {
		t.Fatalf("runRunConfig: %v", err)
	}
	body := strings.TrimRight(out, "\n")
	if strings.Contains(body, "\n") {
		t.Errorf("--compact output should be single-line, got: %q", out)
	}
	var cfg types.RunConfig
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("compact output not parseable: %v\n%s", err, out)
	}
}

// TestRunRunConfig_RedactScrubsSecretRefs pins that --redact rewrites
// secret:// references but leaves every other field intact. The
// captured config is no longer runnable as-is — that's the point.
func TestRunRunConfig_RedactScrubsSecretRefs(t *testing.T) {
	out, err := runRunConfigForTest(t, []string{
		"--redact",
		"--mode", "planning",
		"--api-key-ref", "secret://CUSTOM_KEY",
	}, "")
	if err != nil {
		t.Fatalf("runRunConfig: %v", err)
	}
	if !strings.Contains(out, "secret://[REDACTED]") {
		t.Errorf("--redact should produce REDACTED placeholder, got: %s", out)
	}
	if strings.Contains(out, "CUSTOM_KEY") {
		t.Errorf("--redact should scrub the original secret reference, got: %s", out)
	}
}

// TestRunRunConfig_MalformedStdinJSONErrors pins the parse-error
// surface. The DisallowUnknownFields decoder reports unknown fields
// by name; the test asserts on that name to catch a regression that
// swallows the underlying error.
func TestRunRunConfig_MalformedStdinJSONErrors(t *testing.T) {
	_, err := runRunConfigForTest(t, nil, `{"this is not valid JSON`)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parsing config") {
		t.Errorf("error should describe parse failure, got: %v", err)
	}
}

// TestRunRunConfig_RoundTripFromExample is the spec's acceptance
// criterion 3 against a real example file. It picks the smallest
// example to keep the diff window narrow if the test breaks; the
// `cat config.json | stirrup run-config | stirrup run-config | diff -
// config.json` shape is what the spec calls out for the smoke suite.
func TestRunRunConfig_RoundTripFromExample(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "examples", "runconfig", "openai_responses.json"))
	if err != nil {
		t.Skipf("openai_responses.json example not present: %v", err)
	}
	pass1, err := runRunConfigForTest(t, nil, string(body))
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	pass2, err := runRunConfigForTest(t, nil, pass1)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if pass1 != pass2 {
		t.Errorf("normalisation pass is not idempotent")
	}
}

// TestRunRunConfig_WrapperThreadsOSIO pins MF-3's residual line: the
// runRunConfig cobra RunE thunk threads os.Stdin / os.Stdout into the
// testable runRunConfigWithIO helper. Without this assertion the
// one-liner thunk stays at 0% coverage and a future regression that
// swaps the wiring (e.g. passes nil for stdout) would land silently.
func TestRunRunConfig_WrapperThreadsOSIO(t *testing.T) {
	cmd := newTestRunConfigCommand()
	if err := cmd.ParseFlags([]string{"--mode", "planning"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
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

	if err := runRunConfig(cmd, cmd.Flags().Args()); err != nil {
		_ = w.Close()
		<-done
		t.Fatalf("runRunConfig: %v", err)
	}
	_ = w.Close()
	out := <-done

	var cfg types.RunConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("os.Stdout should receive parseable JSON: %v\n%s", err, out)
	}
	if cfg.Mode != "planning" {
		t.Errorf("Mode = %q, want planning", cfg.Mode)
	}
}

// TestRunRunConfigWithIO_TableDriven pins MF-3: every cobra
// flag-reading branch inside runRunConfigWithIO (validate, redact,
// compact, plain) reaches a passing assertion through the real entry
// point. Before the WithIO refactor, runRunConfigForTest replicated
// the function body inline, so a `f.GetBool("compact")` rename would
// have passed every test. The table exercises one branch per row.
func TestRunRunConfigWithIO_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		stdin   string
		wantErr bool
		check   func(t *testing.T, out string)
	}{
		{
			name: "flag-only emits indented JSON",
			args: []string{"--mode", "planning", "--prompt", "x"},
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, "\n  ") {
					t.Errorf("default output should be indented, got: %s", out)
				}
				var cfg types.RunConfig
				if err := json.Unmarshal([]byte(out), &cfg); err != nil {
					t.Fatalf("unparseable JSON: %v\n%s", err, out)
				}
				if cfg.Mode != "planning" {
					t.Errorf("Mode = %q, want planning", cfg.Mode)
				}
			},
		},
		{
			name: "compact produces single-line JSON",
			args: []string{"--compact", "--mode", "planning"},
			check: func(t *testing.T, out string) {
				body := strings.TrimRight(out, "\n")
				if strings.Contains(body, "\n") {
					t.Errorf("--compact output should be single-line, got: %q", out)
				}
				var cfg types.RunConfig
				if err := json.Unmarshal([]byte(body), &cfg); err != nil {
					t.Fatalf("compact output not parseable: %v\n%s", err, out)
				}
			},
		},
		{
			name: "redact rewrites secret references",
			args: []string{"--redact", "--mode", "planning", "--api-key-ref", "secret://CUSTOM_KEY"},
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, "secret://[REDACTED]") {
					t.Errorf("--redact should produce REDACTED placeholder, got: %s", out)
				}
				if strings.Contains(out, "CUSTOM_KEY") {
					t.Errorf("--redact should scrub the original secret reference, got: %s", out)
				}
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
			cmd := newTestRunConfigCommand()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			var reader io.Reader
			if tc.stdin != "" {
				reader = strings.NewReader(tc.stdin)
			}
			var buf bytes.Buffer
			err := runRunConfigWithIO(cmd, cmd.Flags().Args(), reader, &buf)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; output: %s", buf.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("runRunConfigWithIO: %v", err)
			}
			if tc.check != nil {
				tc.check(t, buf.String())
			}
		})
	}
}

// TestRunRunConfig_EmptyEditStrategyDefaultsToMulti pins the
// stirrup run-config subcommand's edit-strategy default behaviour.
// With --validate (ResolveAll), the emitted document must carry
// EditStrategy.Type = "multi" because the validation layer is the
// single normalization point. Without --validate (ResolveBase), the
// emitted document deliberately preserves the empty value so chained
// `run-config | run-config | ...` stages remain idempotent and a
// later stage can layer one more override on top.
func TestRunRunConfig_EmptyEditStrategyDefaultsToMulti(t *testing.T) {
	t.Run("validate fills default", func(t *testing.T) {
		out, err := runRunConfigForTest(t, []string{
			"--validate",
			"--mode", "execution",
			"--prompt", "test",
		}, "")
		if err != nil {
			t.Fatalf("runRunConfig: %v", err)
		}
		var cfg types.RunConfig
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("unparseable JSON: %v\n%s", err, out)
		}
		if cfg.EditStrategy.Type != "multi" {
			t.Errorf("EditStrategy.Type = %q, want multi (subcommand default must match CLI and validation)", cfg.EditStrategy.Type)
		}
	})

	t.Run("base mode preserves empty for chaining", func(t *testing.T) {
		out, err := runRunConfigForTest(t, []string{
			"--mode", "execution",
			"--prompt", "test",
		}, "")
		if err != nil {
			t.Fatalf("runRunConfig: %v", err)
		}
		var cfg types.RunConfig
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("unparseable JSON: %v\n%s", err, out)
		}
		if cfg.EditStrategy.Type != "" {
			t.Errorf("EditStrategy.Type = %q, want empty (ResolveBase must not apply defaults a downstream stage could override)", cfg.EditStrategy.Type)
		}
	})
}
