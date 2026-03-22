package reporter

import (
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/eval"
)

// FormatText produces a human-readable text report from a ComparisonReport.
func FormatText(report eval.ComparisonReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Eval Comparison: %s vs %s\n\n", report.CurrentID, report.BaselineID)

	s := report.Summary
	fmt.Fprintf(&b, "Pass Rate: %.1f%% → %.1f%% (%s%.1f%%)\n",
		s.BaselinePassRate*100,
		s.CurrentPassRate*100,
		signPrefix(s.PassRateDelta*100),
		s.PassRateDelta*100,
	)

	b.WriteString("\n")

	if len(report.Regressions) > 0 {
		fmt.Fprintf(&b, "Regressions (%d):\n", len(report.Regressions))
		for _, r := range report.Regressions {
			fmt.Fprintf(&b, "  - %s: %s → %s\n", r.TaskID, r.BaselineOutcome, r.CurrentOutcome)
		}
	} else {
		b.WriteString("No regressions found.\n")
	}

	if len(report.Improvements) > 0 {
		b.WriteString("\n")
		fmt.Fprintf(&b, "Improvements (%d):\n", len(report.Improvements))
		for _, im := range report.Improvements {
			fmt.Fprintf(&b, "  - %s: %s → %s\n", im.TaskID, im.BaselineOutcome, im.CurrentOutcome)
		}
	}

	return b.String()
}

// signPrefix returns "+" for positive values, "-" for negative, and "+" for zero.
func signPrefix(v float64) string {
	if v < 0 {
		return "-"
	}
	return "+"
}
