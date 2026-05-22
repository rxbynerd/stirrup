package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestRun_CompletionSubcommandValidShells pins the four hand-rolled
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
func TestRun_CompletionSubcommandValidShells(t *testing.T) {
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
			var stdout bytes.Buffer
			code := run([]string{"completion", tc.shell}, &stdout)
			if code != 0 {
				t.Fatalf("run(completion %s) exit code = %d", tc.shell, code)
			}
			if stdout.Len() == 0 {
				t.Fatalf("completion %s emitted empty script", tc.shell)
			}
			if len(tc.checker) == 0 {
				return
			}
			if _, err := exec.LookPath(tc.checker[0]); err != nil {
				t.Skipf("%s not available on this runner: %v", tc.checker[0], err)
			}
			cmd := exec.Command(tc.checker[0], tc.checker[1:]...) //nolint:gosec // hard-coded checker list, no user input
			cmd.Stdin = &stdout
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s syntax check failed: %v\noutput:\n%s", tc.shell, err, out)
			}
		})
	}
}

// TestRun_CompletionSubcommandMissingShell pins the dispatcher guard:
// `eval completion` (no shell arg) must return a non-zero exit code
// rather than silently writing nothing.
func TestRun_CompletionSubcommandMissingShell(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"completion"}, &stdout)
	if code == 0 {
		t.Fatal("run(completion) exit code = 0, want non-zero")
	}
}

// TestRun_CompletionSubcommandUnknownShell pins the emitter guard:
// `eval completion ksh` must return a non-zero exit code and surface
// the unsupported-shell error on stderr (cannot be asserted directly
// here because run() does not thread stderr).
func TestRun_CompletionSubcommandUnknownShell(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"completion", "ksh"}, &stdout)
	if code == 0 {
		t.Fatal("run(completion ksh) exit code = 0, want non-zero")
	}
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
			var buf bytes.Buffer
			if err := emitEvalCompletion(shell, &buf); err != nil {
				t.Fatalf("emit %s: %v", shell, err)
			}
			body := buf.String()
			for _, sub := range evalCompletionSubcommands {
				if !strings.Contains(body, sub) {
					t.Errorf("%s script missing subcommand %q", shell, sub)
				}
			}
			for _, mode := range types.ValidRunModeValues() {
				if !strings.Contains(body, mode) {
					t.Errorf("%s script missing mode %q", shell, mode)
				}
			}
			// The completion subcommand routes its shell-name suggestions
			// through evalCompletionFlags rather than a positional-argument
			// path, so every supported shell name must surface in the
			// rendered script. A regression that drops the completion
			// entry from evalCompletionFlags would silently revert
			// `stirrup-eval completion <TAB>` to producing no suggestions.
			for _, sh := range evalCompletionFlags["completion"] {
				if !strings.Contains(body, sh) {
					t.Errorf("%s script missing shell name %q for completion subcommand", shell, sh)
				}
			}
		})
	}
}

// TestEvalCompletionFishScript_QuotesValues pins the fish-script
// quoting contract for subcommand and mode values. Fish single-quote
// literals interpret no escape sequences, so wrapping the format
// argument keeps the generated script safe even if a future value
// contains a space or fish-special character. A regression that drops
// the surrounding single quotes would re-introduce the latent
// injection path described in the SF-3 finding.
func TestEvalCompletionFishScript_QuotesValues(t *testing.T) {
	var buf bytes.Buffer
	if err := emitEvalCompletion("fish", &buf); err != nil {
		t.Fatalf("emit fish: %v", err)
	}
	body := buf.String()
	for _, sub := range evalCompletionSubcommands {
		needle := "complete -c stirrup-eval -n __stirrup_eval_no_subcommand -a '" + sub + "'"
		if !strings.Contains(body, needle) {
			t.Errorf("fish script missing single-quoted subcommand line for %q (looking for %q)", sub, needle)
		}
	}
	for _, m := range types.ValidRunModeValues() {
		needle := "-l mode -a '" + m + "'"
		if !strings.Contains(body, needle) {
			t.Errorf("fish script missing single-quoted mode line for %q (looking for %q)", m, needle)
		}
	}
}

// TestEvalCompletionFlagMap_TracksDispatcher pins the static flag map
// against the real subcommand surface. A new subcommand wired into
// run() but forgotten in evalCompletionFlags would silently ship with
// no flag completion; the test fails loudly so the regression cannot
// reach a release.
func TestEvalCompletionFlagMap_TracksDispatcher(t *testing.T) {
	dispatcherSubs := []string{
		"run", "compare", "compare-to-production",
		"baseline", "mine-failures", "drift", "convert",
		"completion",
	}
	if len(evalCompletionSubcommands) != len(dispatcherSubs) {
		t.Fatalf("len(evalCompletionSubcommands) = %d, dispatcher has %d",
			len(evalCompletionSubcommands), len(dispatcherSubs))
	}
	for _, sub := range dispatcherSubs {
		if _, ok := evalCompletionFlags[sub]; !ok {
			t.Errorf("subcommand %q dispatched by run() but missing from evalCompletionFlags", sub)
		}
	}
}
