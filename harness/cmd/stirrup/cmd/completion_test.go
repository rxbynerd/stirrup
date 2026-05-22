package cmd

import (
	"bytes"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
)

// TestCompletionCmd_GeneratesValidShellScripts pins the four cobra
// completion generators against each shell's own syntax checker. The
// script is generated in-process (no os/exec stirrup-binary dependency)
// and piped to `bash -n` / `zsh -n` / `fish -n`. PowerShell's syntax
// check is more involved and not exercised on CI, so the powershell
// variant is asserted only by verifying the generator returns without
// error and emits a non-empty body.
//
// A missing shell binary is a t.Skipf, not a failure: the harness CI
// runners do not necessarily have fish installed, and a hard failure
// would gate the merge on every developer machine matching the CI
// shell matrix.
func TestCompletionCmd_GeneratesValidShellScripts(t *testing.T) {
	for _, tc := range []struct {
		shell   string
		checker []string // command + args to syntax-check stdin
	}{
		{shell: "bash", checker: []string{"bash", "-n"}},
		{shell: "zsh", checker: []string{"zsh", "-n"}},
		{shell: "fish", checker: []string{"fish", "-n"}},
		{shell: "powershell"}, // no in-tree syntax check; presence test only
	} {
		t.Run(tc.shell, func(t *testing.T) {
			buf := runCompletion(t, tc.shell)
			if buf.Len() == 0 {
				t.Fatalf("completion %s emitted empty script", tc.shell)
			}
			if len(tc.checker) == 0 {
				return
			}
			if _, err := exec.LookPath(tc.checker[0]); err != nil {
				t.Skipf("%s not available on this runner: %v", tc.checker[0], err)
			}
			cmd := exec.Command(tc.checker[0], tc.checker[1:]...) //nolint:gosec // hard-coded checker list, no user input
			cmd.Stdin = buf
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s syntax check failed: %v\noutput:\n%s", tc.shell, err, out)
			}
		})
	}
}

// runCompletion executes `stirrup completion <shell>` against an
// isolated cobra command tree and returns the captured stdout. The
// rootCmd singleton owns global state (cobra parses os.Args by default,
// SetOut() leaks across tests); the helper resets every mutation so a
// later test in the package sees a clean command.
func runCompletion(t *testing.T, shell string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"completion", shell})
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("completion %s: %v", shell, err)
	}
	return &buf
}

// TestCompletionCmd_RejectsUnknownShell pins the cobra ValidArgs
// guard so an operator typing `stirrup completion ksh` sees a clear
// error rather than an empty stdout and a zero exit code.
func TestCompletionCmd_RejectsUnknownShell(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"completion", "ksh"})
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown shell, got nil")
	}
	if !strings.Contains(err.Error(), "ksh") {
		t.Errorf("error %q does not mention the rejected shell", err)
	}
}

// TestFlagCompletion_EnumValues pins the closed-set value completion
// for every flag registered via addRunConfigFlagCompletions. Each
// row asserts the completion function returns the same sorted slice
// that types.Valid*Values() exposes, plus the NoFileComp directive so
// shells do not also append filesystem entries.
//
// The values are pulled from the types package directly so a new entry
// in validRunModes (etc.) shows up here without a manual sync.
func TestFlagCompletion_EnumValues(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want []string
	}{
		{"mode", types.ValidRunModeValues()},
		{"provider", types.ValidProviderTypeValues()},
		{"executor", types.ValidExecutorTypeValues()},
		{"edit-strategy", types.ValidEditStrategyTypeValues()},
		{"verifier", types.ValidVerifierTypeValues()},
		{"git-strategy", types.ValidGitStrategyTypeValues()},
		{"transport", types.ValidTransportTypeValues()},
		{"trace-emitter", types.ValidTraceEmitterTypeValues()},
		{"otel-protocol", types.ValidTraceEmitterProtocolValues()},
		{"container-runtime", types.ValidExecutorRuntimeValues()},
		{"code-scanner", types.ValidCodeScannerTypeValues()},
		{"guardrail", types.ValidGuardRailTypeValues()},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got, directive := runFlagCompletion(t, harnessCmd, tc.flag)
			if directive != cobra.ShellCompDirectiveNoFileComp {
				t.Errorf("directive = %v, want NoFileComp", directive)
			}
			assertStringsEqual(t, got, tc.want)

			// Same flags are re-registered on run-config via the shared
			// addRunConfigFlags helper, so the run-config command must
			// return the identical completion surface.
			gotRC, directiveRC := runFlagCompletion(t, runConfigCmd, tc.flag)
			if directiveRC != cobra.ShellCompDirectiveNoFileComp {
				t.Errorf("run-config directive = %v, want NoFileComp", directiveRC)
			}
			assertStringsEqual(t, gotRC, tc.want)
		})
	}
}

// TestFlagCompletion_FileFlags pins the MarkFlagFilename /
// MarkFlagDirname wiring. cobra encodes the file-completion contract
// as a flag annotation (cobra.BashCompFilenameExt /
// cobra.BashCompSubdirsInDir); each row asserts the matching
// annotation is present so a regression that drops a MarkFlagFilename
// call surfaces as a test failure rather than as a quietly degraded
// completion experience.
func TestFlagCompletion_FileFlags(t *testing.T) {
	for _, tc := range []struct {
		flag       string
		wantExt    []string // empty = any-file completion (annotation present with empty list)
		wantDir    bool
		onCommands []*cobra.Command
	}{
		{flag: "config", wantExt: []string{"json"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "prompt-file", wantExt: []string{}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "gcp-credentials-file", wantExt: []string{"json"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "permission-policy-file", wantExt: []string{"cedar"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "trace", wantExt: []string{"jsonl"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "workspace", wantDir: true, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "output-runconfig", wantExt: []string{"json"}, onCommands: []*cobra.Command{harnessCmd}},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			for _, cmd := range tc.onCommands {
				fl := cmd.Flags().Lookup(tc.flag)
				if fl == nil {
					t.Fatalf("flag %s missing on %s", tc.flag, cmd.Use)
				}
				if tc.wantDir {
					if _, ok := fl.Annotations[cobra.BashCompSubdirsInDir]; !ok {
						t.Errorf("flag %s on %s: missing BashCompSubdirsInDir annotation", tc.flag, cmd.Use)
					}
					return
				}
				exts, ok := fl.Annotations[cobra.BashCompFilenameExt]
				if !ok {
					t.Fatalf("flag %s on %s: missing BashCompFilenameExt annotation", tc.flag, cmd.Use)
				}
				assertStringsEqual(t, exts, tc.wantExt)
			}
		})
	}
}

// runFlagCompletion invokes the cobra completion function registered
// for the named flag and returns the (values, directive) pair. Mirrors
// the wire shape that cobra's __complete hidden command emits and that
// shells consume; testing at this layer means a regression in the
// per-flag wiring surfaces independently of the shell-script generator.
func runFlagCompletion(t *testing.T, cmd *cobra.Command, flagName string) ([]string, cobra.ShellCompDirective) {
	t.Helper()
	fn, exists := flagCompletionFunc(cmd, flagName)
	if !exists {
		t.Fatalf("no completion function registered for --%s on %s", flagName, cmd.Use)
	}
	values, directive := fn(cmd, nil, "")
	return values, directive
}

// flagCompletionFunc reaches into cobra's per-flag completion map via
// the public ValidArgsFunction / GetFlagCompletionFunc helpers. The
// helper is exposed on *Command in cobra v1.10 as
// (*Command).GetFlagCompletionFunc; the indirection here exists so
// tests do not panic on an older cobra build that lacked the getter.
func flagCompletionFunc(cmd *cobra.Command, name string) (func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective), bool) {
	return cmd.GetFlagCompletionFunc(name)
}

// assertStringsEqual checks two string slices for equality after
// sorting, returning a comparable error message. Used by both the
// enum-flag and file-flag tests where the source is already sorted
// but the comparison is more forgiving.
func assertStringsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			t.Errorf("index %d: got %q, want %q", i, g[i], w[i])
		}
	}
}

// TestCompletionCmd_MissingShell pins the cobra ExactArgs guard so
// `stirrup completion` (no shell argument) fails with a clear
// "accepts 1 arg(s)" message rather than silently writing nothing.
func TestCompletionCmd_MissingShell(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"completion"})
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing shell arg, got nil")
	}
	// cobra phrases the error as "accepts 1 arg(s), received 0".
	if !strings.Contains(err.Error(), "arg") {
		t.Errorf("error %q does not mention argument count", err)
	}
}
