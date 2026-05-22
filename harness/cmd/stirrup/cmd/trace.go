package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// traceCmd is the root of the `stirrup trace …` subcommand family.
// The subcommands inspect JSONL trace files produced by
// traceEmitter.type=jsonl so operators do not have to reinvent
// chronological pretty-printing, follow-tailing, aggregate stats, and
// JSON-path filtering with ad-hoc jq pipelines.
var traceCmd = &cobra.Command{
	Use:   "trace",
	Short: "Inspect JSONL trace files written by stirrup runs",
	Long: `Inspect JSONL trace files produced by traceEmitter.type=jsonl.

Subcommands:
  show    Pretty-print a trace file in chronological order.
  tail    Stream new records as they are appended (tail -f semantics).
  stats   Aggregate token counts, tool calls, durations, security events.
  grep    Filter records by substring or a small JSON-path predicate.

Every subcommand accepts ` + "`-`" + ` as the file argument to read from
stdin, so the family composes with shell pipelines:

  tail -F live.jsonl | stirrup trace show -
  stirrup trace stats run.jsonl --output=json | jq .totalTurns`,
}

func init() {
	rootCmd.AddCommand(traceCmd)
}

// colorMode selects whether ANSI escapes are emitted on a writer.
type colorMode int

const (
	colorAuto colorMode = iota
	colorAlways
	colorNever
)

func parseColorMode(s string) (colorMode, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return colorAuto, nil
	case "always", "force", "yes":
		return colorAlways, nil
	case "never", "no", "off":
		return colorNever, nil
	default:
		return colorAuto, fmt.Errorf("invalid --color value %q (want auto|always|never)", s)
	}
}

// shouldColor reports whether the writer should receive ANSI escapes
// under the given mode. The auto mode disables colour when the writer
// is not a TTY (the standard behaviour ls/grep/git diff follow) AND
// honours the NO_COLOR convention (https://no-color.org/) — any
// non-empty NO_COLOR environment variable suppresses ANSI output.
// --color=always wins over NO_COLOR by design: an operator who
// explicitly opts in is not overridden by ambient env.
func shouldColor(mode colorMode, w io.Writer) bool {
	switch mode {
	case colorAlways:
		return true
	case colorNever:
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// ANSI escape codes used by `trace show`. Kept small and dependency-free
// — pulling in a colour library for four foreground colours and a reset
// would dwarf the actual code.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
	ansiGrey   = "\x1b[90m"
)

// colorize wraps s in the ANSI escape c when enabled is true. Returns
// s unchanged otherwise so call sites can write colour-or-not without
// branching at every print.
func colorize(enabled bool, c, s string) string {
	if !enabled {
		return s
	}
	return c + s + ansiReset
}

// addColorFlag registers --color on cmd with the canonical
// auto|always|never enum the family shares. The shell completion
// function surfaces the three values to bash/zsh tab completion,
// matching the convention every other closed-set flag in this
// package (e.g. --mode, --provider) already follows.
func addColorFlag(cmd *cobra.Command) {
	cmd.Flags().String("color", "auto", "Colourise output: auto|always|never. 'auto' enables colour only when stdout is a TTY.")
	_ = cmd.RegisterFlagCompletionFunc("color", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"auto", "always", "never"}, cobra.ShellCompDirectiveNoFileComp
	})
}

// resolveColorMode parses the --color flag value into a colorMode.
// Cobra returns "" for an unset flag; parseColorMode treats that as
// auto, matching the registered default.
func resolveColorMode(cmd *cobra.Command) (colorMode, error) {
	v, _ := cmd.Flags().GetString("color")
	return parseColorMode(v)
}
