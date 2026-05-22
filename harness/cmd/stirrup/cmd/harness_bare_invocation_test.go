package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestRunHarness_BareInvocation_GroupedHelpOnTTY pins the #249-B
// happy path: when stdin is a tty (interactive operator) and no
// prompt source resolves, runHarness must intercept the
// "prompt is required" error from BuildRunConfig, emit the grouped
// help block to its stderr seam, and return nil so the process
// exits 0.
//
// The test swaps stdinIsTTY to true (go test inherits a non-tty
// stdin, so the production check would short-circuit to the error
// path) and redirects bareHintStderr at a bytes.Buffer so the
// captured body can be asserted without rewriting the os.Stderr fd.
// NO_COLOR is set so the assertion can compare plain text directly
// without re-running the format pass.
func TestRunHarness_BareInvocation_GroupedHelpOnTTY(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("STIRRUP_PROMPT", "")

	restoreTTY := stdinIsTTY
	stdinIsTTY = func() bool { return true }
	t.Cleanup(func() { stdinIsTTY = restoreTTY })

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

// TestRunHarness_BareInvocation_NonTTYStdinKeepsError pins the
// scripted-use guard. When stdin is not a tty the operator is
// piping something (config, prompt) and the grouped help would
// clutter their logs. runHarness must fall through to the original
// "prompt is required" error so a CI run still exits non-zero.
//
// The test pins stdinIsTTY to false explicitly rather than relying
// on the go-test inherited stdin: that inherited shape is platform-
// dependent (char device on macOS, the test harness's pipe on
// Linux), and we want a deterministic assertion regardless.
func TestRunHarness_BareInvocation_NonTTYStdinKeepsError(t *testing.T) {
	t.Setenv("STIRRUP_PROMPT", "")

	restore := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { stdinIsTTY = restore })

	cmd := newTestHarnessCommand()
	err := runHarness(cmd, nil)
	if err == nil {
		t.Fatal("non-tty stdin must keep the prompt-required error path; got nil")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("expected prompt-required error, got: %v", err)
	}
}

// TestRunHarness_BareInvocation_NonTTYStderrStripsANSI pins the
// `stirrup harness 2>&1 | cat` acceptance criterion: when stderr is
// not a tty the grouped help must arrive as plain text with every
// ANSI escape from the closed set stripped. The test forces stdin
// to a tty (so the intercept fires) and captures into a bytes.Buffer
// whose identity (not os.Stderr) is what colorEnabled keys off — the
// same code path a piped real stderr would take.
func TestRunHarness_BareInvocation_NonTTYStderrStripsANSI(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("STIRRUP_PROMPT", "")

	restoreTTY := stdinIsTTY
	stdinIsTTY = func() bool { return true }
	t.Cleanup(func() { stdinIsTTY = restoreTTY })

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
		t.Errorf("non-tty stderr writer leaked ANSI escapes: %q", buf.String())
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

// TestIsPromptRequiredErr pins the sentinel-string match in
// runHarness. The detector is brittle by construction (string match
// rather than a typed error), so the test exists so a future edit
// to either side surfaces immediately instead of silently
// short-circuiting the intercept or routing every error through it.
func TestIsPromptRequiredErr(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", os.ErrNotExist, false},
		{"exact sentinel", &stringErr{"prompt is required: pass via --prompt flag"}, true},
		{"wrapped sentinel", &stringErr{"invalid config: prompt is required"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPromptRequiredErr(tc.in); got != tc.want {
				t.Errorf("isPromptRequiredErr(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// stringErr is a tiny error type so the cases table above can be a
// literal — errors.New allocates per case which is fine but reads
// noisier.
type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }
