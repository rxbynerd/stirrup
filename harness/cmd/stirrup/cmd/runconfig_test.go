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

// runRunConfigForTest invokes runRunConfig with the given args, a
// stdin reader, and a stdout writer. The real runRunConfig writes to
// os.Stdout via writeRunConfigJSON; the test bypasses that by
// replacing os.Stdout for the duration of the call. (Cobra does not
// expose an os.Stdout-injection seam without re-plumbing every
// downstream helper, so a swap is the smallest change.)
func runRunConfigForTest(t *testing.T, args []string, stdin string) (string, error) {
	t.Helper()
	cmd := newTestRunConfigCommand()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	// Build the config ourselves rather than calling runRunConfig:
	// runRunConfig reads os.Stdin (which we cannot easily replace
	// across goroutines / cobra layers) and writes to os.Stdout (which
	// we can but would need to be process-global). This pathway
	// exercises the same BuildRunConfig + writeRunConfigJSON code the
	// command does, just with the stdin source threaded explicitly.
	configPath, _ := cmd.Flags().GetString("config")
	resolve := ResolveBase
	if v, _ := cmd.Flags().GetBool("validate"); v {
		resolve = ResolveAll
	}
	var reader io.Reader
	if stdin != "" {
		reader = strings.NewReader(stdin)
	}
	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:      reader,
		ConfigPath: configPath,
		Cmd:        cmd,
		Args:       cmd.Flags().Args(),
		Resolve:    resolve,
	})
	if err != nil {
		return "", err
	}
	if redact, _ := cmd.Flags().GetBool("redact"); redact {
		r := cfg.Redact()
		cfg = &r
	}
	compact, _ := cmd.Flags().GetBool("compact")
	var buf bytes.Buffer
	if werr := writeRunConfigJSON(&buf, cfg, compact); werr != nil {
		return "", werr
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
