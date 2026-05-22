package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

func init() {
	evalCompletionRunModes = types.ValidRunModeValues()
}

// stirrup-eval uses the stdlib `flag` package rather than cobra, so the
// completion scripts below are hand-rolled rather than generated. The
// flag surface is small (seven subcommands, fewer than thirty distinct
// flags total) and stable enough that maintaining hand-rolled scripts
// is cheaper than dragging cobra into the eval module.
//
// Each script offers:
//   - subcommand completion at position 1 (run, compare, …)
//   - flag-name completion within a subcommand
//   - filesystem path completion for path-shaped flags (-suite, -output,
//     -junit, -harness, -from, -to-junit, -current, -baseline,
//     -lakehouse, -results)
//   - dynamic value completion for -mode (the closed set lives in
//     types/runconfig.go, exposed via types.ValidRunModeValues())
//
// The Go flag package accepts both `-flag` and `--flag`; the scripts
// emit single-dash forms to match the rest of the eval CLI's
// documentation.

// evalCompletionSubcommands enumerates the top-level subcommands. It
// is reused across every script so a new subcommand only needs to be
// added in one place.
var evalCompletionSubcommands = []string{
	"baseline",
	"compare",
	"compare-to-production",
	"convert",
	"drift",
	"mine-failures",
	"run",
}

// evalCompletionFlags maps each subcommand to the flag names it
// accepts. The ordering matches the flag declarations in cmdRun /
// cmdCompare / etc. so a reader can cross-reference at a glance.
// Flag names omit the leading dash.
var evalCompletionFlags = map[string][]string{
	"run":                   {"suite", "harness", "output", "concurrency", "dry-run", "junit"},
	"compare":               {"current", "baseline"},
	"baseline":              {"lakehouse", "after", "before", "mode", "model", "output"},
	"mine-failures":         {"lakehouse", "after", "limit", "output"},
	"drift":                 {"lakehouse", "window", "compare-window", "mode", "model"},
	"compare-to-production": {"lakehouse", "results", "experiment-id", "after", "before", "mode", "model", "output"},
	"convert":               {"from", "to-junit"},
}

// evalCompletionRunModes is the closed-set value list for the -mode
// filter flag on baseline / drift / compare-to-production. Sourced
// from types.ValidRunModeValues() at script-generation time so the
// completion surface tracks the validator.
//
// Bound at package init rather than embedded literally so a future
// addition to validRunModes flows through automatically. The slice is
// joined into shell array literals when each script is rendered.
var evalCompletionRunModes []string

// emitEvalCompletion writes the requested shell's completion script
// to w. The supported shells mirror those exposed by `stirrup
// completion`; a future shell only needs a new emit* helper and an
// added switch arm.
func emitEvalCompletion(shell string, w io.Writer) error {
	switch shell {
	case "bash":
		return emitEvalBashCompletion(w)
	case "zsh":
		return emitEvalZshCompletion(w)
	case "fish":
		return emitEvalFishCompletion(w)
	case "powershell":
		return emitEvalPowerShellCompletion(w)
	default:
		return fmt.Errorf("unsupported shell: %s", shell)
	}
}

func emitEvalBashCompletion(w io.Writer) error {
	var b strings.Builder
	b.WriteString(`# bash completion for stirrup-eval
_stirrup_eval() {
    local cur prev words cword
    _init_completion || return

    local subcommands="`)
	b.WriteString(strings.Join(evalCompletionSubcommands, " "))
	b.WriteString(`"
    local modes="`)
	b.WriteString(strings.Join(evalCompletionRunModes, " "))
	b.WriteString(`"

    # Position 1: subcommand selection.
    if [[ $cword -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "$subcommands" -- "$cur") )
        return
    fi

    local sub="${words[1]}"
    local flags=""
    case "$sub" in
`)
	for _, sub := range evalCompletionSubcommands {
		fmt.Fprintf(&b, "        %s) flags=\"%s\" ;;\n", sub, dashPrefix(evalCompletionFlags[sub]))
	}
	b.WriteString(`    esac

    # File / dir / enum completion for the value position after a flag.
    case "$prev" in
        -suite|-from) _filedir hcl; return ;;
        -harness) _filedir; return ;;
        -output|-junit|-to-junit|-current|-baseline|-results) _filedir; return ;;
        -lakehouse) _filedir -d; return ;;
        -mode)
            COMPREPLY=( $(compgen -W "$modes" -- "$cur") )
            return
            ;;
    esac

    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
        return
    fi
}
complete -F _stirrup_eval stirrup-eval
`)
	_, err := io.WriteString(w, b.String())
	return err
}

func emitEvalZshCompletion(w io.Writer) error {
	var b strings.Builder
	b.WriteString(`#compdef stirrup-eval
# zsh completion for stirrup-eval
_stirrup_eval() {
    local -a subcommands modes
    subcommands=(`)
	for _, sub := range evalCompletionSubcommands {
		b.WriteString(" " + sub)
	}
	b.WriteString(` )
    modes=(`)
	for _, m := range evalCompletionRunModes {
		b.WriteString(" " + m)
	}
	b.WriteString(` )

    if (( CURRENT == 2 )); then
        _describe 'subcommand' subcommands
        return
    fi

    local sub="${words[2]}"
    local -a flags
    case "$sub" in
`)
	for _, sub := range evalCompletionSubcommands {
		fmt.Fprintf(&b, "        %s) flags=(%s) ;;\n", sub, zshFlagArray(evalCompletionFlags[sub]))
	}
	b.WriteString(`    esac

    case "${words[CURRENT-1]}" in
        -suite|-from) _files -g '*.hcl'; return ;;
        -harness|-output|-junit|-to-junit|-current|-baseline|-results) _files; return ;;
        -lakehouse) _path_files -/; return ;;
        -mode) _describe 'mode' modes; return ;;
    esac

    _describe 'flag' flags
}
compdef _stirrup_eval stirrup-eval
`)
	_, err := io.WriteString(w, b.String())
	return err
}

func emitEvalFishCompletion(w io.Writer) error {
	var b strings.Builder
	b.WriteString(`# fish completion for stirrup-eval
function __stirrup_eval_no_subcommand
    set -l cmd (commandline -opc)
    if test (count $cmd) -lt 2
        return 0
    end
    for sub in `)
	b.WriteString(strings.Join(evalCompletionSubcommands, " "))
	b.WriteString(`
        if test "$cmd[2]" = "$sub"
            return 1
        end
    end
    return 0
end

function __stirrup_eval_using_subcommand
    set -l cmd (commandline -opc)
    if test (count $cmd) -lt 2
        return 1
    end
    test "$cmd[2]" = "$argv[1]"
end

`)
	for _, sub := range evalCompletionSubcommands {
		fmt.Fprintf(&b, "complete -c stirrup-eval -n __stirrup_eval_no_subcommand -a %s\n", sub)
	}
	b.WriteString("\n")
	for _, sub := range evalCompletionSubcommands {
		for _, flag := range evalCompletionFlags[sub] {
			fmt.Fprintf(&b, "complete -c stirrup-eval -n '__stirrup_eval_using_subcommand %s' -l %s\n", sub, flag)
			fmt.Fprintf(&b, "complete -c stirrup-eval -n '__stirrup_eval_using_subcommand %s' -o %s\n", sub, flag)
		}
	}
	// -mode value completion.
	b.WriteString("\n")
	for _, sub := range []string{"baseline", "drift", "compare-to-production"} {
		for _, m := range evalCompletionRunModes {
			fmt.Fprintf(&b, "complete -c stirrup-eval -n '__stirrup_eval_using_subcommand %s' -l mode -a %s\n", sub, m)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func emitEvalPowerShellCompletion(w io.Writer) error {
	var b strings.Builder
	b.WriteString(`# powershell completion for stirrup-eval
Register-ArgumentCompleter -Native -CommandName stirrup-eval -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $subcommands = @(`)
	for i, sub := range evalCompletionSubcommands {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("'" + sub + "'")
	}
	b.WriteString(`)
    $modes = @(`)
	for i, m := range evalCompletionRunModes {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("'" + m + "'")
	}
	b.WriteString(`)

    $tokens = $commandAst.CommandElements
    $position = $tokens.Count
    if ($wordToComplete -ne '') { $position = $position - 1 }

    if ($position -le 1) {
        $subcommands | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
            [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
        return
    }

    $sub = $tokens[1].ToString()
    $flagsBySub = @{
`)
	for _, sub := range evalCompletionSubcommands {
		flagsLit := make([]string, 0, len(evalCompletionFlags[sub]))
		for _, fl := range evalCompletionFlags[sub] {
			flagsLit = append(flagsLit, "'-"+fl+"'")
		}
		fmt.Fprintf(&b, "        '%s' = @(%s)\n", sub, strings.Join(flagsLit, ", "))
	}
	b.WriteString(`    }
    $flags = $flagsBySub[$sub]
    if (-not $flags) { return }

    $prev = if ($position -ge 2) { $tokens[$position - 1].ToString() } else { '' }
    if ($prev -eq '-mode') {
        $modes | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
            [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
        }
        return
    }

    $flags | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterName', $_)
    }
}
`)
	_, err := io.WriteString(w, b.String())
	return err
}

// dashPrefix joins a slice of bare flag names into a space-separated
// list of dash-prefixed forms suitable for embedding in a bash case
// arm. Centralised so a future change to the dash convention (e.g.
// double-dash) touches one helper rather than every Builder concat.
func dashPrefix(flags []string) string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, "-"+f)
	}
	return strings.Join(out, " ")
}

// zshFlagArray renders a slice of flag names as a parenthesised zsh
// array of dash-prefixed entries. Used by emitEvalZshCompletion.
func zshFlagArray(flags []string) string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, "-"+f)
	}
	return strings.Join(out, " ")
}
