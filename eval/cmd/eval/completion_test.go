package main

import (
	"bytes"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestRun_CompletionSubcommand_ScriptsPassShellSyntaxCheck pins the four hand-rolled
// completion scripts against each shell's syntax checker. The script is
// generated through the run() dispatcher (matching how a real operator
// reaches it) and piped to bash -n / zsh -n / fish -n. A shell missing
// from the runner's PATH is a t.Skipf, not a failure — stirrup-eval
// ships on Linux / macOS CI runners that do not necessarily carry every
// shell.
//
// The powershell variant is not syntax-checked in-tree (the cobra
// scripts have no equivalent for the hand-rolled ones, and pwsh is
// not on the default macOS / GHA runners). Presence and non-emptiness
// are asserted instead so a regression that drops the powershell case
// arm still fails the test.
func TestRun_CompletionSubcommand_ScriptsPassShellSyntaxCheck(t *testing.T) {
	for _, tc := range []struct {
		shell   string
		checker []string
	}{
		{shell: "bash", checker: []string{"bash", "-n"}},
		{shell: "zsh", checker: []string{"zsh", "-n"}},
		{shell: "fish", checker: []string{"fish", "-n"}},
		{shell: "powershell"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			must := require.New(t)
			var stdout bytes.Buffer
			code := run([]string{"completion", tc.shell}, &stdout)
			must.Zero(code, "run exit code")
			must.NotEmpty(stdout.String(), "completion script")
			if len(tc.checker) == 0 {
				return
			}
			if _, err := exec.LookPath(tc.checker[0]); err != nil {
				t.Skipf("%s not available on this runner: %v", tc.checker[0], err)
			}
			cmd := exec.Command(tc.checker[0], tc.checker[1:]...) //nolint:gosec // hard-coded checker list, no user input
			cmd.Stdin = &stdout
			out, err := cmd.CombinedOutput()
			must.NoError(err, "syntax check output:\n%s", out)
		})
	}
}

// TestRun_CompletionSubcommand_RequiresShellArg pins the dispatcher guard:
// `eval completion` (no shell arg) must return a non-zero exit code
// rather than silently writing nothing.
func TestRun_CompletionSubcommand_RequiresShellArg(t *testing.T) {
	must := require.New(t)
	var stdout bytes.Buffer
	code := run([]string{"completion"}, &stdout)
	must.NotZero(code, "run(completion) exit code")
}

// TestRun_CompletionSubcommand_RejectsUnknownShell pins the emitter guard:
// `eval completion ksh` must return a non-zero exit code and surface
// the unsupported-shell error on stderr (cannot be asserted directly
// here because run() does not thread stderr).
func TestRun_CompletionSubcommand_RejectsUnknownShell(t *testing.T) {
	must := require.New(t)
	var stdout bytes.Buffer
	code := run([]string{"completion", "ksh"}, &stdout)
	must.NotZero(code, "run(completion ksh) exit code")
}

// TestEvalCompletionScripts_MentionEverySubcommand pins the rendered
// scripts contain every subcommand name and every known mode value.
// The check is independent of the rendered shell so a regression in
// the static subcommand list (e.g. a new subcommand added to the
// dispatcher but forgotten in completion.go) surfaces consistently
// across all four shells.
func TestEvalCompletionScripts_MentionEverySubcommand(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer
			must.NoError(emitEvalCompletion(shell, &buf), "emit %s", shell)
			body := buf.String()

			for _, sub := range evalCompletionSubcommands {
				t.Run("subcommand "+sub, func(t *testing.T) {
					must := require.New(t)
					must.Contains(body, sub)
				})
			}
			for _, mode := range types.ValidRunModeValues() {
				t.Run("mode "+mode, func(t *testing.T) {
					must := require.New(t)
					must.Contains(body, mode)
				})
			}
			// The completion subcommand routes its shell-name suggestions
			// through evalCompletionFlags rather than a positional-argument
			// path, so every supported shell name must surface in the
			// rendered script. A regression that drops the completion
			// entry from evalCompletionFlags would silently revert
			// `stirrup-eval completion <TAB>` to producing no suggestions.
			for _, sh := range evalCompletionFlags["completion"] {
				t.Run("completion shell "+sh, func(t *testing.T) {
					must := require.New(t)
					must.Contains(body, sh)
				})
			}
		})
	}
}

// TestEvalCompletionFishScript_QuotesValues pins the fish-script
// quoting contract for subcommand and mode values: values must stay
// wrapped in single quotes, or a future value containing a space or
// fish-special character could break the generated script.
func TestEvalCompletionFishScript_QuotesValues(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	must.NoError(emitEvalCompletion("fish", &buf), "emit fish")
	body := buf.String()

	for _, sub := range evalCompletionSubcommands {
		needle := "complete -c stirrup-eval -n __stirrup_eval_no_subcommand -a '" + sub + "'"
		t.Run("subcommand "+sub, func(t *testing.T) {
			must := require.New(t)
			must.Contains(body, needle)
		})
	}
	for _, m := range types.ValidRunModeValues() {
		needle := "-l mode -a '" + m + "'"
		t.Run("mode "+m, func(t *testing.T) {
			must := require.New(t)
			must.Contains(body, needle)
		})
	}
}

// TestEvalCompletionFlagMap_TracksDispatcher pins the static flag map
// against the real subcommand surface. A new subcommand wired into
// run() but forgotten in evalCompletionFlags would silently ship with
// no flag completion; the test fails loudly so the regression cannot
// reach a release.
func TestEvalCompletionFlagMap_TracksDispatcher(t *testing.T) {
	must := require.New(t)
	dispatcherSubs := []string{
		"run", "compare", "compare-to-production",
		"baseline", "mine-failures", "drift", "ingest", "replay", "convert",
		"judge-calibrate",
		"completion",
	}
	must.Len(evalCompletionSubcommands, len(dispatcherSubs), "dispatcher and evalCompletionSubcommands differ")
	for _, sub := range dispatcherSubs {
		t.Run(sub, func(t *testing.T) {
			must := require.New(t)
			must.Contains(evalCompletionFlags, sub, "dispatched by run() but missing from evalCompletionFlags")
		})
	}
}
