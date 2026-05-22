package cmd

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

var traceShowCmd = &cobra.Command{
	Use:   "show <file>",
	Short: "Pretty-print a JSONL trace file",
	Long: `Pretty-print a JSONL trace file in chronological order, surfacing
the run metadata, every recorded turn, every tool call, and the final
outcome. Pass ` + "`-`" + ` to read the trace from stdin.

Colours are emitted by default when stdout is a TTY; override with
--color=always to force, --color=never to disable (also honours plain
pipes via the auto detection).`,
	Args: cobra.ExactArgs(1),
	RunE: runTraceShow,
}

func init() {
	traceCmd.AddCommand(traceShowCmd)
	addColorFlag(traceShowCmd)
}

func runTraceShow(cmd *cobra.Command, args []string) error {
	mode, err := resolveColorMode(cmd)
	if err != nil {
		return err
	}
	return runTraceShowWith(args[0], cmd.OutOrStdout(), mode)
}

func runTraceShowWith(path string, out io.Writer, mode colorMode) error {
	r, err := tracereader.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	color := shouldColor(mode, out)

	any := false
	for {
		trace, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		any = true
		if err := renderRunTrace(out, trace, color); err != nil {
			return err
		}
	}
	if !any {
		return fmt.Errorf("trace file %q contained no well-formed records", path)
	}
	return nil
}

// renderRunTrace pretty-prints a single RunTrace block.
func renderRunTrace(out io.Writer, t *types.RunTrace, color bool) error {
	header := colorize(color, ansiBold+ansiCyan, fmt.Sprintf("=== run %s ===", strOrDash(t.ID)))
	if _, err := fmt.Fprintln(out, header); err != nil {
		return err
	}

	meta := fmt.Sprintf("  started:  %s\n  finished: %s\n  duration: %s\n  outcome:  %s\n  turns:    %d\n  tokens:   in=%d out=%d",
		formatTime(t.StartedAt),
		formatTime(t.CompletedAt),
		t.CompletedAt.Sub(t.StartedAt).Round(time.Millisecond),
		colorize(color, outcomeColor(t.Outcome), strOrDash(t.Outcome)),
		t.Turns,
		t.TokenUsage.Input,
		t.TokenUsage.Output,
	)
	if _, err := fmt.Fprintln(out, meta); err != nil {
		return err
	}

	if t.Config.RunID != "" {
		fmt.Fprintf(out, "  runId:    %s\n", t.Config.RunID)
	}

	// Tool calls in arrival order — the trace stores them in the
	// order they were recorded, which is also chronological.
	if len(t.ToolCalls) > 0 {
		fmt.Fprintln(out, colorize(color, ansiBold, "  tool calls:"))
		for i, tc := range t.ToolCalls {
			line := fmt.Sprintf("    %3d. %s  %s  %dms  in=%dB out=%dB",
				i+1,
				colorize(color, toolCallColor(tc.Success), tc.Name),
				toolStatus(tc, color),
				tc.DurationMs,
				tc.InputSize,
				tc.OutputSize,
			)
			if tc.ErrorReason != "" {
				line += "  " + colorize(color, ansiRed, "err="+tc.ErrorReason)
			}
			if tc.RunID != "" {
				line += colorize(color, ansiGrey, fmt.Sprintf("  [subagent %s]", tc.RunID))
			}
			fmt.Fprintln(out, line)
		}
	}

	if len(t.VerificationResults) > 0 {
		fmt.Fprintln(out, colorize(color, ansiBold, "  verification:"))
		for i, vr := range t.VerificationResults {
			passed := colorize(color, ansiRed, "FAIL")
			if vr.Passed {
				passed = colorize(color, ansiGreen, "PASS")
			}
			fmt.Fprintf(out, "    %3d. %s  %s\n", i+1, passed, vr.Feedback)
		}
	}

	// Aggregate tool-name counts so a long tool stream is summarised
	// before the wall of per-call detail goes by — useful when piping
	// to less.
	if len(t.ToolCalls) > 1 {
		counts := map[string]int{}
		for _, tc := range t.ToolCalls {
			counts[tc.Name]++
		}
		names := make([]string, 0, len(counts))
		for n := range counts {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(out, colorize(color, ansiGrey, "  tool counts:"))
		for _, n := range names {
			fmt.Fprintf(out, "    %-32s %d\n", n, counts[n])
		}
	}

	_, err := fmt.Fprintln(out)
	return err
}

func outcomeColor(outcome string) string {
	switch outcome {
	case "success":
		return ansiGreen
	case "error", "verification_failed", "verification_error", "tool_failures":
		return ansiRed
	case "max_turns", "max_tokens", "budget_exceeded", "stalled", "timeout":
		return ansiYellow
	case "cancelled":
		return ansiGrey
	default:
		return ansiBlue
	}
}

func toolCallColor(success bool) string {
	if success {
		return ansiBlue
	}
	return ansiRed
}

func toolStatus(tc types.ToolCallSummary, color bool) string {
	if tc.Success {
		return colorize(color, ansiGreen, "ok")
	}
	return colorize(color, ansiRed, "fail")
}

// strOrDash renders an empty string as "—" so the output table never
// has a hole the reader has to mentally fill in.
func strOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// formatTime renders a time.Time as RFC3339, treating the zero value
// as "—" so a partial trace does not surface "0001-01-01T00:00:00Z".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format(time.RFC3339)
}

