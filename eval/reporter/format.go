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
	if s.Gate != "" {
		fmt.Fprintf(&b, "Gate: %s\n", strings.ToUpper(s.Gate))
		for _, r := range s.GateReasons {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Pass Rate: %.1f%% → %.1f%% (%+.1f%%)\n",
		s.BaselinePassRate*100,
		s.CurrentPassRate*100,
		s.PassRateDelta*100,
	)
	writeRate(&b, "baseline", s.Baseline)
	writeRate(&b, "current", s.Current)
	writePaired(&b, s.Paired, len(report.Tasks))
	writeNoiseFloor(&b, s.NoiseFloor, len(report.Tasks), s.Current.Trials)

	b.WriteString("\n")

	if len(report.Regressions) > 0 {
		fmt.Fprintf(&b, "Regressions (%d):\n", len(report.Regressions))
		for _, r := range report.Regressions {
			fmt.Fprintf(&b, "  - %s: %s → %s (pass fraction %.2f → %.2f)\n",
				r.TaskID, r.BaselineOutcome, r.CurrentOutcome, r.BaselinePassFraction, r.CurrentPassFraction)
		}
	} else {
		b.WriteString("No regressions found.\n")
	}

	if len(report.Improvements) > 0 {
		b.WriteString("\n")
		fmt.Fprintf(&b, "Improvements (%d):\n", len(report.Improvements))
		for _, im := range report.Improvements {
			fmt.Fprintf(&b, "  - %s: %s → %s (pass fraction %.2f → %.2f)\n",
				im.TaskID, im.BaselineOutcome, im.CurrentOutcome, im.BaselinePassFraction, im.CurrentPassFraction)
		}
	}

	return b.String()
}

// FormatWilson95 renders the 95% Wilson interval for rate over n samples
// as "lo-hi%", or "n/a" when n is not positive.
func FormatWilson95(rate float64, n int) string {
	if n <= 0 {
		return "n/a"
	}
	lo, hi := Wilson95(rate*float64(n), n)
	return fmt.Sprintf("%.1f-%.1f%%", lo*100, hi*100)
}

func writeRate(b *strings.Builder, label string, r eval.RateSummary) {
	if r.Tasks == 0 {
		fmt.Fprintf(b, "  %-9s no tasks\n", label+":")
		return
	}
	se := "n/a"
	if r.StdErr != nil {
		se = fmt.Sprintf("%.3f", *r.StdErr)
	}
	fmt.Fprintf(b, "  %-9s %.1f%% (95%% Wilson [%.1f%%, %.1f%%]), SE %s, n=%d tasks, K=%d\n",
		label+":", r.PassRate*100, r.WilsonLow*100, r.WilsonHigh*100, se, r.Tasks, r.Trials)
	if len(r.PassHatK) > 0 {
		fmt.Fprintf(b, "  %-9s pass^k (k=1..%d): %s; pass@k: %s\n",
			"", len(r.PassHatK), joinFloats(r.PassHatK), joinFloats(r.PassAtK))
	}
}

func writePaired(b *strings.Builder, p *eval.PairedSummary, n int) {
	if p == nil {
		fmt.Fprintf(b, "Paired: n=%d task(s); paired statistics need at least 2\n", n)
		return
	}
	exactness := "normal approx."
	if p.PValueExact {
		exactness = "exact"
	}
	mde := "n/a"
	if p.MDE != nil {
		mde = fmt.Sprintf("%.2f", *p.MDE)
	}
	fmt.Fprintf(b, "Paired: n=%d, mean delta %+.3f, SE %.4f, two-sided 95%% t(%d) CI [%+.3f, %+.3f], one-sided 95%% upper bound %+.3f, sign-flip p %.3f (%s), MDE %s\n",
		p.Tasks, p.MeanDelta, p.StdErr, p.DF, p.CILow, p.CIHigh, p.UpperBound, p.PValue, exactness, mde)
}

func writeNoiseFloor(b *strings.Builder, nf *eval.NoiseFloor, n, k int) {
	if nf == nil {
		return
	}
	fmt.Fprintf(b, "Noise floor (all-pass baseline, n=%d): per-trial pass rate %.3f over %d pooled trials; a single-run flip gate false-alarms on %.2f%% of pushes, the %d-of-%d flip rule on %.2f%%\n",
		n, nf.PerTrialPassRate, nf.PooledTrials, nf.SingleRunFalseAlarm*100, k, k, nf.FlipRuleFalseAlarm*100)
}

func joinFloats(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.3f", x)
	}
	return strings.Join(parts, ", ")
}
