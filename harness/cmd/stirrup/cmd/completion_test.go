package cmd

import (
	"bytes"
	"os/exec"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestCompletionCmd_GeneratesValidShellScripts pins the four cobra
// completion generators against each shell's own syntax checker.
// PowerShell is asserted only for a non-empty body (no syntax checker on
// CI). A missing shell binary is a t.Skipf, not a failure.
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
			must := require.New(t)
			buf := runCompletion(t, tc.shell)
			must.NotEmpty(buf.String(), "completion %s emitted empty script", tc.shell)
			if len(tc.checker) == 0 {
				return
			}
			if _, err := exec.LookPath(tc.checker[0]); err != nil {
				t.Skipf("%s not available on this runner: %v", tc.checker[0], err)
			}
			cmd := exec.Command(tc.checker[0], tc.checker[1:]...) //nolint:gosec // hard-coded checker list, no user input
			cmd.Stdin = buf
			out, err := cmd.CombinedOutput()
			must.NoError(err, "%s syntax check failed; output:\n%s", tc.shell, out)
		})
	}
}

// executeRootCmd drives rootCmd with the given args and returns the
// captured stdout/stderr buffer along with the execution error. Resets
// rootCmd's mutated state via defer so later tests see a clean command.
func executeRootCmd(t *testing.T, args []string) (*bytes.Buffer, error) {
	t.Helper()
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(args)
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	err := rootCmd.Execute()
	return &buf, err
}

// runCompletion executes `stirrup completion <shell>` and returns the
// captured stdout, failing the test on a non-nil error (happy-path only;
// negative-path tests use executeRootCmd directly).
func runCompletion(t *testing.T, shell string) *bytes.Buffer {
	t.Helper()
	must := require.New(t)
	buf, err := executeRootCmd(t, []string{"completion", shell})
	must.NoError(err, "completion %s", shell)
	return buf
}

// TestCompletionCmd_RejectsUnknownShell pins the cobra ValidArgs
// guard so an operator typing `stirrup completion ksh` sees a clear
// error rather than an empty stdout and a zero exit code.
func TestCompletionCmd_RejectsUnknownShell(t *testing.T) {
	must := require.New(t)
	_, err := executeRootCmd(t, []string{"completion", "ksh"})
	must.ErrorContains(err, "ksh")
}

// TestFlagCompletion_EnumValues pins the closed-set value completion for
// every flag registered via addRunConfigFlagCompletions: each row
// asserts the completion function returns the same values as
// types.Valid*Values(), plus NoFileComp. log-level and api-key-header
// carry literal lists since neither is backed by a validator-closed set.
func TestFlagCompletion_EnumValues(t *testing.T) {
	for _, tc := range []struct {
		flag           string
		want           []string
		harnessCmdOnly bool // when true, --run-config does not register this flag
	}{
		{flag: "mode", want: types.ValidRunModeValues()},
		{flag: "provider", want: types.ValidProviderTypeValues()},
		{flag: "executor", want: types.ValidExecutorTypeValues()},
		{flag: "edit-strategy", want: types.ValidEditStrategyTypeValues()},
		{flag: "verifier", want: types.ValidVerifierTypeValues()},
		{flag: "git-strategy", want: types.ValidGitStrategyTypeValues()},
		{flag: "transport", want: types.ValidTransportTypeValues()},
		{flag: "trace-emitter", want: types.ValidTraceEmitterTypeValues()},
		{flag: "otel-protocol", want: types.ValidTraceEmitterProtocolValues()},
		{flag: "container-runtime", want: types.ValidExecutorRuntimeValues()},
		{flag: "code-scanner", want: types.ValidCodeScannerTypeValues()},
		{flag: "guardrail", want: types.ValidGuardRailTypeValues()},
		{flag: "log-level", want: []string{"debug", "error", "info", "warn"}},
		{flag: "api-key-header", want: []string{"Authorization", "api-key"}},
		// --output is harness-only: a hand-rolled RegisterFlagCompletionFunc
		// rather than a types.Valid*Values() call.
		{flag: "output", want: []string{"json", "none", "text"}, harnessCmdOnly: true},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			must := require.New(t)
			got, directive := runFlagCompletion(t, harnessCmd, tc.flag)
			must.Equal(cobra.ShellCompDirectiveNoFileComp, directive)
			must.ElementsMatch(tc.want, got)

			if tc.harnessCmdOnly {
				return
			}

			// run-config re-registers the same flags via addRunConfigFlags,
			// so it must return the identical completion surface.
			gotRC, directiveRC := runFlagCompletion(t, runConfigCmd, tc.flag)
			must.Equal(cobra.ShellCompDirectiveNoFileComp, directiveRC, "run-config directive")
			must.ElementsMatch(tc.want, gotRC)
		})
	}
}

// TestFlagCompletion_FileAnnotations pins the MarkFlagFilename / MarkFlagDirname
// wiring: each row asserts the matching cobra annotation
// (BashCompFilenameExt / BashCompSubdirsInDir) is present. The wantNone
// row for --export-workspace-to is deliberate: that flag takes a gs://
// URI, not a local path, so file/dir completion would be wrong.
func TestFlagCompletion_FileAnnotations(t *testing.T) {
	for _, tc := range []struct {
		flag       string
		wantExt    []string // empty = any-file completion (annotation present with empty list)
		wantDir    bool
		wantNone   bool // assert neither file nor dir completion annotation is set
		onCommands []*cobra.Command
	}{
		{flag: "config", wantExt: []string{"json"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "prompt-file", wantExt: []string{}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "gcp-credentials-file", wantExt: []string{"json"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "replay-file", wantExt: []string{"jsonl"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "permission-policy-file", wantExt: []string{"cedar"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "trace", wantExt: []string{"jsonl"}, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "workspace", wantDir: true, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
		{flag: "output-runconfig", wantExt: []string{"json"}, onCommands: []*cobra.Command{harnessCmd}},
		{flag: "transport-tls-ca-file", wantExt: []string{"pem", "crt"}, onCommands: []*cobra.Command{harnessCmd}},
		{flag: "transport-tls-cert-file", wantExt: []string{"pem", "crt"}, onCommands: []*cobra.Command{harnessCmd}},
		{flag: "transport-tls-key-file", wantExt: []string{"pem", "key"}, onCommands: []*cobra.Command{harnessCmd}},
		{flag: "export-workspace-to", wantNone: true, onCommands: []*cobra.Command{harnessCmd, runConfigCmd}},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			must := require.New(t)
			for _, cmd := range tc.onCommands {
				fl := cmd.Flags().Lookup(tc.flag)
				must.NotNil(fl, "flag %s missing on %s", tc.flag, cmd.Use)
				if tc.wantNone {
					must.NotContains(fl.Annotations, cobra.BashCompFilenameExt,
						"flag %s on %s takes a gs:// URI, not a local path", tc.flag, cmd.Use)
					must.NotContains(fl.Annotations, cobra.BashCompSubdirsInDir,
						"flag %s on %s takes a gs:// URI, not a local path", tc.flag, cmd.Use)
					continue
				}
				if tc.wantDir {
					must.Contains(fl.Annotations, cobra.BashCompSubdirsInDir,
						"flag %s on %s", tc.flag, cmd.Use)
					return
				}
				must.Contains(fl.Annotations, cobra.BashCompFilenameExt,
					"flag %s on %s", tc.flag, cmd.Use)
				must.ElementsMatch(tc.wantExt, fl.Annotations[cobra.BashCompFilenameExt])
			}
		})
	}
}

// runFlagCompletion invokes the cobra completion function registered
// for the named flag and returns the (values, directive) pair.
func runFlagCompletion(t *testing.T, cmd *cobra.Command, flagName string) ([]string, cobra.ShellCompDirective) {
	t.Helper()
	must := require.New(t)
	fn, exists := flagCompletionFunc(cmd, flagName)
	must.True(exists, "no completion function registered for --%s on %s", flagName, cmd.Use)
	values, directive := fn(cmd, nil, "")
	return values, directive
}

// flagCompletionFunc reaches into cobra's per-flag completion map via
// (*Command).GetFlagCompletionFunc.
func flagCompletionFunc(cmd *cobra.Command, name string) (func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective), bool) {
	return cmd.GetFlagCompletionFunc(name)
}

// TestCompletionCmd_RequiresShellArg pins the cobra ExactArgs guard so
// `stirrup completion` (no shell argument) fails with a clear
// "accepts 1 arg(s)" message rather than silently writing nothing.
func TestCompletionCmd_RequiresShellArg(t *testing.T) {
	must := require.New(t)
	_, err := executeRootCmd(t, []string{"completion"})
	// cobra phrases the error as "accepts 1 arg(s), received 0".
	must.ErrorContains(err, "arg")
}
