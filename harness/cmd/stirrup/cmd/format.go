package cmd

import (
	"io"
	"strings"
)

// The CLI entry-point usage hints (issue #249) are hand-authored
// templates, NOT generated from Cobra's flag list, so the wording is a
// single deliberate edit rather than a regenerated artifact that drifts
// toward the noise the hint exists to avoid. `--help` remains the
// authoritative, exhaustive reference.

// printRootUsageHint writes the bare-`stirrup` two-subcommand hint to w.
// Plain text only — no ANSI, no boxes, no headings beyond the single
// title line — because the hint must read identically whether stdout is
// a terminal, a pager, or captured into a file. The shape mirrors the
// issue #249 template verbatim so a future edit to the wording is a
// deliberate single-site change rather than a regenerated artifact.
func printRootUsageHint(w io.Writer) {
	const hint = `stirrup — a coding agent harness

Usage:
  stirrup harness --prompt "<task>"          Run the agentic loop (interactive use).
  stirrup job                                Run as a control-plane-driven job.

For full help: stirrup harness --help
For version:  stirrup --version
`
	_, _ = io.WriteString(w, hint)
}

// printHarnessUsageHint writes the grouped, example-led `stirrup harness`
// hint to w. It fires only when a bare invocation reaches the
// prompt-required gate on an interactive terminal (see
// resolvePromptForRun): scripted, non-TTY use keeps the opaque
// "prompt is required" error so log aggregators are not flooded with a
// multi-line template.
//
// color is decided by the caller via shouldColor so the TTY / NO_COLOR
// detection stays in one place (trace.go). When false the function
// emits plain text — every colorize call collapses to its argument —
// which is the form a piped `2>&1 | cat` must observe.
//
// The grouping mirrors the flag concerns documented in
// docs/configuration.md (Required, Run shape, Provider, Configuration)
// so an operator who reads the hint and then the doc sees the same
// mental model. It is deliberately a curated subset, not the full flag
// list: `stirrup harness --help` remains authoritative for the rest.
func printHarnessUsageHint(w io.Writer, color bool) {
	var b strings.Builder

	heading := func(s string) {
		b.WriteString(colorize(color, ansiBold, s))
		b.WriteByte('\n')
	}
	dim := func(s string) string {
		return colorize(color, ansiGrey, s)
	}

	b.WriteString(colorize(color, ansiBold, "stirrup harness — run the coding agent harness"))
	b.WriteString("\n\n")
	b.WriteString("No prompt supplied. A run needs a task; the most common shapes follow.\n")
	b.WriteString("Run `stirrup harness --help` for the full flag reference.\n\n")

	heading("USAGE")
	b.WriteString("  stirrup harness --prompt \"<task>\" [flags]\n")
	b.WriteString("  stirrup harness \"<task>\" [flags]\n\n")

	heading("REQUIRED")
	b.WriteString("  --prompt \"<task>\"        The task to run. Also accepted as a positional\n")
	b.WriteString("                           argument, via --prompt-file, or STIRRUP_PROMPT.\n\n")

	heading("RUN SHAPE")
	b.WriteString("  --mode " + dim("execution") + "        Enable edits and shell. Default is planning\n")
	b.WriteString("                           (read-only: no write_file / edit_file / run_command).\n")
	b.WriteString("  --max-turns " + dim("20") + "          Hard turn cap (max 100).\n")
	b.WriteString("  --timeout " + dim("600") + "           Wall-clock seconds (max 3600).\n\n")

	heading("PROVIDER")
	b.WriteString("  --provider " + dim("anthropic") + "     Provider adapter.\n")
	b.WriteString("  --model " + dim("<model-id>") + "       Model to route to.\n")
	b.WriteString("  --api-key-ref " + dim("secret://ANTHROPIC_API_KEY") + "\n")
	b.WriteString("                           Secret reference resolved at runtime (never a raw key).\n\n")

	heading("CONFIGURATION")
	b.WriteString("  --config <path>          Load a JSON RunConfig; flags then override fields.\n")
	b.WriteString("                           Pass --config - to read the config from stdin.\n")
	b.WriteString("  --output-runconfig <path>\n")
	b.WriteString("                           Write the resolved RunConfig and exit without running\n")
	b.WriteString("                           ('-' for stdout).\n\n")

	heading("EXAMPLES")
	b.WriteString("  " + dim("# Plan a change (read-only, safe by default):") + "\n")
	b.WriteString("  stirrup harness --prompt \"Audit error handling in the executor\"\n\n")
	b.WriteString("  " + dim("# Execute a change with shell + edits:") + "\n")
	b.WriteString("  stirrup harness --mode execution --prompt \"Fix the failing TestFoo\"\n\n")
	b.WriteString("  " + dim("# Capture the resolved config, then replay it via a pipeline:") + "\n")
	b.WriteString("  stirrup run-config --mode execution | stirrup harness --prompt \"Ship it\"\n")

	_, _ = io.WriteString(w, b.String())
}
