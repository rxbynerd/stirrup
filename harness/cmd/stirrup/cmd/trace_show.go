package cmd

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
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

// renderRunTrace pretty-prints a single RunTrace block. The output is
// assembled into a single strings.Builder and flushed at the end so a
// short write on the underlying writer surfaces as one error rather
// than leaving a half-rendered record in the user's terminal.
func renderRunTrace(out io.Writer, t *types.RunTrace, color bool) error {
	var b strings.Builder

	b.WriteString(colorize(color, ansiBold+ansiCyan, fmt.Sprintf("=== run %s ===", strOrDash(t.ID))))
	b.WriteByte('\n')

	fmt.Fprintf(&b, "  started:  %s\n", formatTime(t.StartedAt))
	fmt.Fprintf(&b, "  finished: %s\n", formatTime(t.CompletedAt))
	fmt.Fprintf(&b, "  duration: %s\n", t.CompletedAt.Sub(t.StartedAt).Round(time.Millisecond))
	fmt.Fprintf(&b, "  outcome:  %s\n", colorize(color, outcomeColor(t.Outcome), strOrDash(t.Outcome)))
	fmt.Fprintf(&b, "  turns:    %d\n", t.Turns)
	fmt.Fprintf(&b, "  tokens:   in=%d out=%d\n", t.TokenUsage.Input, t.TokenUsage.Output)

	if t.Config.RunID != "" {
		fmt.Fprintf(&b, "  runId:    %s\n", t.Config.RunID)
	}

	if len(t.ToolCalls) > 0 {
		b.WriteString(colorize(color, ansiBold, "  tool calls:"))
		b.WriteByte('\n')
		for i, tc := range t.ToolCalls {
			fmt.Fprintf(&b, "    %3d. %s  %s  %dms  in=%dB out=%dB",
				i+1,
				colorize(color, toolCallColor(tc.Success), tc.Name),
				toolStatus(tc, color),
				tc.DurationMs,
				tc.InputSize,
				tc.OutputSize,
			)
			if tc.ErrorReason != "" {
				b.WriteString("  " + colorize(color, ansiRed, "err="+tc.ErrorReason))
			}
			if tc.RunID != "" {
				b.WriteString(colorize(color, ansiGrey, fmt.Sprintf("  [subagent %s]", tc.RunID)))
			}
			b.WriteByte('\n')
		}
	}

	if len(t.VerificationResults) > 0 {
		b.WriteString(colorize(color, ansiBold, "  verification:"))
		b.WriteByte('\n')
		for i, vr := range t.VerificationResults {
			passed := colorize(color, ansiRed, "FAIL")
			if vr.Passed {
				passed = colorize(color, ansiGreen, "PASS")
			}
			fmt.Fprintf(&b, "    %3d. %s  %s\n", i+1, passed, vr.Feedback)
		}
	}

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
		b.WriteString(colorize(color, ansiGrey, "  tool counts:"))
		b.WriteByte('\n')
		for _, n := range names {
			fmt.Fprintf(&b, "    %-32s %d\n", n, counts[n])
		}
	}

	b.WriteByte('\n')
	_, err := io.WriteString(out, b.String())
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
	// ErrorCategory is empty on older traces; fall back to a bare "fail".
	if tc.ErrorCategory != "" {
		return colorize(color, ansiRed, "fail ("+tc.ErrorCategory+")")
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
