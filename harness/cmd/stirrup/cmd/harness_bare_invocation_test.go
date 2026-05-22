package cmd

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestRunHarness_BareInvocation_GroupedHelpOnTTY pins the #249-B
// happy path: when stderr is a tty (interactive operator watching the
// terminal) and no prompt source resolves, runHarness must intercept
// the prompt-required error from BuildRunConfig, emit the grouped
// help block to its stderr seam, and return nil so the process
// exits 0.
//
// The test swaps stderrIsTTY to true (go test inherits a non-tty
// stderr, so the production check would short-circuit to the error
// path) and redirects bareHintStderr at a bytes.Buffer so the
// captured body can be asserted without rewriting the os.Stderr fd.
// NO_COLOR is set so the assertion can compare plain text directly
// without re-running the format pass.
func TestRunHarness_BareInvocation_GroupedHelpOnTTY(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("STIRRUP_PROMPT", "")

	restoreTTY := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = restoreTTY })

	var buf bytes.Buffer
	restoreW := bareHintStderr
	bareHintStderr = &buf
	t.Cleanup(func() { bareHintStderr = restoreW })

	cmd := newTestHarnessCommand()
	if err := runHarness(cmd, nil); err != nil {
		t.Fatalf("runHarness: want nil (intercepted bare invocation), got %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("grouped help body was empty; bare-invocation intercept did not fire")
	}
	for _, want := range []string{
		"stirrup harness — run the agentic loop",
		"USAGE:",
		"REQUIRED:",
		"--prompt",
		"RUN SHAPE:",
		"PROVIDER:",
		"CONFIGURATION:",
		"--output-runconfig",
		"EXAMPLES:",
		"stirrup run-config", // #240 pipeline composition reference
		"stirrup harness --help",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("grouped help missing %q\n--- full output ---\n%s", want, out)
		}
	}

	// NO_COLOR=1 must strip ANSI even though the writer-identity
	// check would also force plain text — assert the closed-set
	// stripper actually held.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR=1 path leaked ANSI escapes: %q", out)
	}
}

// TestRunHarness_BareInvocation_NonTTYStderrKeepsError pins the
// scripted-use guard. When stderr is not a tty the operator has
// either piped or discarded stderr (`stirrup harness 2>&1 | cat`,
// `stirrup harness 2>/dev/null`) and the grouped help would clutter
// logs or silently swallow the failure. runHarness must fall through
// to the original "prompt is required" error so a CI run still
// exits non-zero.
//
// The test pins stderrIsTTY to false explicitly rather than relying
// on the go-test inherited stderr: that inherited shape is platform-
// dependent (char device on macOS, the test harness's pipe on
// Linux), and we want a deterministic assertion regardless.
func TestRunHarness_BareInvocation_NonTTYStderrKeepsError(t *testing.T) {
	t.Setenv("STIRRUP_PROMPT", "")

	restore := stderrIsTTY
	stderrIsTTY = func() bool { return false }
	t.Cleanup(func() { stderrIsTTY = restore })

	cmd := newTestHarnessCommand()
	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("non-tty stderr must keep the prompt-required error path; got nil")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("expected prompt-required error, got: %v", err)
	}
}

// TestRunHarness_BareInvocation_WriterIdentityForcesPlainText pins
// the writer-identity short-circuit inside colorEnabled. The test
// forces both seams (stderrIsTTY, NO_COLOR) to claim a colour-capable
// terminal, then captures into a bytes.Buffer whose identity (not
// os.Stderr) is what colorEnabled actually keys off. The buffer must
// receive plain text — the same code path a piped real stderr would
// take.
//
// (The name describes what is varied — writer identity — rather than
// "non-tty stderr", which would be misleading given the seam is
// stubbed to true.)
func TestRunHarness_BareInvocation_WriterIdentityForcesPlainText(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("STIRRUP_PROMPT", "")

	// Force the stderr-tty seam to claim tty so NO_COLOR is the only
	// reason colorEnabled could refuse — and then verify that the
	// writer-identity check fires regardless because the buffer is
	// not os.Stderr.
	restoreStderrTTY := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = restoreStderrTTY })

	var buf bytes.Buffer
	restoreW := bareHintStderr
	bareHintStderr = &buf
	t.Cleanup(func() { bareHintStderr = restoreW })

	cmd := newTestHarnessCommand()
	if err := runHarness(cmd, nil); err != nil {
		t.Fatalf("runHarness: want nil, got %v", err)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("buffer writer (not os.Stderr) leaked ANSI escapes: %q", buf.String())
	}
}

// TestRunHarness_BareInvocation_STIRRUPPROMPTSetPreventsIntercept
// pins the negative gate: when STIRRUP_PROMPT resolves the prompt,
// the prompt-required error never fires, so the intercept must not
// fire either. Without this guard a regression that called
// writeBareHarnessHint on any stderr-TTY invocation (ignoring the
// error class) would silently absorb downstream validator failures.
//
// Uses the maxTurns downstream-validation trick from
// TestRunHarness_PromptFromEnvVar: the config invalidates on maxTurns
// after the prompt resolves, so a non-nil error containing "maxTurns"
// (and *not* "prompt is required") proves the prompt path completed
// without triggering the bare-invocation hint.
func TestRunHarness_BareInvocation_STIRRUPPROMPTSetPreventsIntercept(t *testing.T) {
	path := writePromptResolutionConfig(t)
	t.Setenv("STIRRUP_PROMPT", "hello from env")

	restoreTTY := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = restoreTTY })

	var buf bytes.Buffer
	restoreW := bareHintStderr
	bareHintStderr = &buf
	t.Cleanup(func() { bareHintStderr = restoreW })

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatalf("set config: %v", err)
	}

	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("expected downstream validation error after prompt resolution, got nil")
	}
	if strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("STIRRUP_PROMPT was not consulted; got prompt-required error: %v", err)
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected validator to reject maxTurns after prompt was resolved, got: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("bare-invocation hint fired despite STIRRUP_PROMPT being set: %q", buf.String())
	}
}

// TestRunHarness_BareInvocation_PositionalPromptPreventsIntercept is
// the same negative gate as the env-var case but exercises the
// positional-arg source. Together they pin that the intercept fires
// only on the actual prompt-required error path, not on any
// stderr-TTY invocation.
func TestRunHarness_BareInvocation_PositionalPromptPreventsIntercept(t *testing.T) {
	path := writePromptResolutionConfig(t)
	t.Setenv("STIRRUP_PROMPT", "")

	restoreTTY := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = restoreTTY })

	var buf bytes.Buffer
	restoreW := bareHintStderr
	bareHintStderr = &buf
	t.Cleanup(func() { bareHintStderr = restoreW })

	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatalf("set config: %v", err)
	}

	err := runHarness(cmd, []string{"do the thing"})
	if err == nil {
		t.Fatal("expected downstream validation error after prompt resolution, got nil")
	}
	if strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("positional prompt was not consulted; got prompt-required error: %v", err)
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected validator to reject maxTurns after prompt was resolved, got: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("bare-invocation hint fired despite positional prompt: %q", buf.String())
	}
}

// TestBareHarnessHintText_ColorEnabledEmitsANSI is a unit-level
// assertion on the template function: with color=true the output
// must carry the bold/dim opening sequences for at least one
// heading. Without this guard a refactor that broke the template
// (e.g. dropped the bold() wrapping) would still pass the TTY-stripping
// test because stripping a non-existent sequence is a no-op.
func TestBareHarnessHintText_ColorEnabledEmitsANSI(t *testing.T) {
	got := bareHarnessHintText(true)
	if !strings.Contains(got, "\x1b[1m") {
		t.Errorf("color=true output missing bold start sequence")
	}
	if !strings.Contains(got, "\x1b[2m") {
		t.Errorf("color=true output missing dim start sequence")
	}
}

// TestBareHarnessHintText_PlainOmitsANSI is the negative of the
// above: with color=false the template must emit zero escape
// sequences. Pins the fall-through path used by NO_COLOR / non-tty
// writers.
func TestBareHarnessHintText_PlainOmitsANSI(t *testing.T) {
	got := bareHarnessHintText(false)
	if strings.Contains(got, "\x1b[") {
		t.Errorf("color=false output should not contain ANSI escapes, got: %q", got)
	}
}

// TestIsPromptRequiredErr pins the sentinel-based detector. The
// detector wraps the typed errPromptRequired sentinel, so the test
// exists so a future edit to either side surfaces immediately
// instead of silently short-circuiting the intercept or routing
// every error through it.
func TestIsPromptRequiredErr(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", os.ErrNotExist, false},
		{"plain string match no longer triggers", &stringErr{"prompt is required: pass via --prompt flag"}, false},
		{"sentinel directly", errPromptRequired, true},
		{"sentinel wrapped via fmt.Errorf %w", fmt.Errorf("invalid config: %w", errPromptRequired), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPromptRequiredErr(tc.in); got != tc.want {
				t.Errorf("isPromptRequiredErr(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestColorEnabled_TTYStderrNoNoColor pins the production
// ANSI-enabled path. Every other test in this file writes to a
// bytes.Buffer, so the `w != os.Stderr` short-circuit always fires
// before stderrIsTTY is consulted. A regression that inverted the
// writer-identity guard would not be caught without this assertion.
func TestColorEnabled_TTYStderrNoNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")

	restore := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = restore })

	if !colorEnabled(os.Stderr) {
		t.Error("colorEnabled(os.Stderr) = false with NO_COLOR unset and stderr-TTY; want true")
	}
}

// TestColorEnabled_NOCOLORVariants pins no-color.org compliance:
// the spec says any non-empty value disables colour (including
// counterintuitive values like "0"). A future maintainer narrowing
// the check to `== "1"` would break the contract; this table is the
// guard.
func TestColorEnabled_NOCOLORVariants(t *testing.T) {
	restore := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = restore })

	cases := []struct {
		noColor string
		want    bool
	}{
		{"", true},          // unset → colour allowed
		{"1", false},        // canonical disable
		{"0", false},        // no-color.org: any non-empty value disables
		{"anything", false}, // any other non-empty value also disables
	}
	for _, tc := range cases {
		t.Run("NO_COLOR="+tc.noColor, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			if got := colorEnabled(os.Stderr); got != tc.want {
				t.Errorf("colorEnabled(os.Stderr) with NO_COLOR=%q = %v, want %v", tc.noColor, got, tc.want)
			}
		})
	}
}

// stringErr is a tiny error type so the TestIsPromptRequiredErr
// cases table can be a literal — errors.New allocates per case which
// is fine but reads noisier. Used for the plain-string case that
// must *not* match now that isPromptRequiredErr uses errors.Is on a
// typed sentinel.
type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }
