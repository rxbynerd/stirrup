// Package reporter implements comparison reporting for eval suite results.
// It diffs a current SuiteResult against a baseline, computes paired
// statistics over per-task pass fractions, and decides the gate outcome.
package reporter

import (
	"fmt"
	"math"
	"strings"

	"github.com/rxbynerd/stirrup/eval"
)

const (
	// DefaultWarnMargin is the mean pass-fraction drop that warns when the
	// drop is not statistically confirmed.
	DefaultWarnMargin = 0.10

	// DefaultFlipThreshold is the pass fraction at or below which a task
	// that passed every baseline trial counts as a regression.
	DefaultFlipThreshold = 0.5

	// minGateTasks is the fewest paired tasks for which the paired interval
	// is allowed to decide the gate.
	minGateTasks = 3

	fractionEpsilon = 1e-9
)

// Options tunes the comparison.
type Options struct {
	WarnMargin    float64
	FlipThreshold float64
}

// DefaultOptions returns the options `stirrup-eval compare` uses when no
// flag overrides them.
func DefaultOptions() Options {
	return Options{WarnMargin: DefaultWarnMargin, FlipThreshold: DefaultFlipThreshold}
}

// Validate rejects margins outside [0, 1] and thresholds outside [0, 1).
func (o Options) Validate() error {
	if math.IsNaN(o.WarnMargin) || o.WarnMargin < 0 || o.WarnMargin > 1 {
		return fmt.Errorf("warn margin %v must be within [0, 1]", o.WarnMargin)
	}
	if math.IsNaN(o.FlipThreshold) || o.FlipThreshold < 0 || o.FlipThreshold >= 1 {
		return fmt.Errorf("flip threshold %v must be within [0, 1)", o.FlipThreshold)
	}
	return nil
}

// Compare diffs a current SuiteResult against a baseline. Per-task and
// paired statistics use only tasks present in both results; the side
// summaries and pass rates use every task on that side. A task without
// Trials counts as a single trial, so results written before trials
// existed compare unchanged.
func Compare(baseline, current eval.SuiteResult, opts Options) eval.ComparisonReport {
	baselineByID := indexByTaskID(baseline.Tasks)

	var (
		regressions  []eval.TaskRegression
		improvements []eval.TaskImprovement
		pairs        []eval.TaskComparison
		flips        []string
		diffs        []float64
		pooledPass   int
		pooledTotal  int
		allPassBase  = true
	)

	for i := range current.Tasks {
		ct := &current.Tasks[i]
		bt, exists := baselineByID[ct.TaskID]
		if !exists {
			continue
		}
		bc, cc := bt.Counts(), ct.Counts()
		bf, cf := bc.PassFraction(), cc.PassFraction()

		pairs = append(pairs, eval.TaskComparison{
			TaskID:               ct.TaskID,
			BaselinePassFraction: bf,
			CurrentPassFraction:  cf,
			BaselineTrials:       bc.Total(),
			CurrentTrials:        cc.Total(),
			Delta:                cf - bf,
		})
		diffs = append(diffs, cf-bf)

		baselineAllPass := bc.Total() > 0 && bc.Pass == bc.Total()
		if !baselineAllPass {
			allPassBase = false
		}
		pooledPass += bc.Pass + cc.Pass
		pooledTotal += bc.Total() + cc.Total()

		if baselineAllPass && cc.Total() > 0 && cc.Pass == 0 {
			flips = append(flips, ct.TaskID)
		}

		currentAllPass := cc.Total() > 0 && cc.Pass == cc.Total()
		switch {
		case baselineAllPass && cf <= opts.FlipThreshold+fractionEpsilon:
			regressions = append(regressions, eval.TaskRegression{
				TaskID:               ct.TaskID,
				BaselineOutcome:      bt.Outcome,
				CurrentOutcome:       ct.Outcome,
				BaselinePassFraction: bf,
				CurrentPassFraction:  cf,
				TurnsDelta:           turnsDelta(bt, ct),
			})
		case currentAllPass && bf <= opts.FlipThreshold+fractionEpsilon:
			improvements = append(improvements, eval.TaskImprovement{
				TaskID:               ct.TaskID,
				BaselineOutcome:      bt.Outcome,
				CurrentOutcome:       ct.Outcome,
				BaselinePassFraction: bf,
				CurrentPassFraction:  cf,
				TurnsDelta:           turnsDelta(bt, ct),
			})
		}
	}

	summary := eval.ComparisonSummary{
		HasRegressions:     len(regressions) > 0,
		WarnMargin:         opts.WarnMargin,
		FlipThreshold:      opts.FlipThreshold,
		DeterministicFlips: flips,
		Baseline:           rateSummary(baseline),
		Current:            rateSummary(current),
	}
	summary.BaselinePassRate = summary.Baseline.PassRate
	summary.CurrentPassRate = summary.Current.PassRate
	summary.PassRateDelta = summary.CurrentPassRate - summary.BaselinePassRate

	if ps, ok := Paired(diffs); ok {
		summary.Paired = &ps
	}

	if len(pairs) > 0 && allPassBase && pooledTotal > 0 {
		p := float64(pooledPass) / float64(pooledTotal)
		summary.NoiseFloor = &eval.NoiseFloor{
			PerTrialPassRate:    p,
			PooledTrials:        pooledTotal,
			SingleRunFalseAlarm: FlipFalseAlarmRate(p, len(pairs), 1),
			FlipRuleFalseAlarm:  FlipFalseAlarmRate(p, len(pairs), summary.Current.Trials),
		}
	}

	summary.Gate, summary.GateReasons = decideGate(gateInput{
		pairs:       len(pairs),
		paired:      summary.Paired,
		flips:       flips,
		regressions: regressions,
		warnMargin:  opts.WarnMargin,
	})

	return eval.ComparisonReport{
		CurrentID:    current.RunID,
		BaselineID:   baseline.RunID,
		Regressions:  regressions,
		Improvements: improvements,
		Tasks:        pairs,
		Summary:      summary,
	}
}

// gateInput carries what decideGate needs from a comparison.
type gateInput struct {
	pairs       int
	paired      *eval.PairedSummary
	flips       []string
	regressions []eval.TaskRegression
	warnMargin  float64
}

// decideGate applies the gate rules in precedence order: a deterministic
// flip blocks at any n; fewer than minGateTasks paired tasks (or an
// undefined SE) is inconclusive; a one-sided 95% upper bound below zero
// blocks; a mean drop beyond the warn margin warns; anything else passes.
// A listed regression then raises a pass or inconclusive gate to warn.
func decideGate(in gateInput) (string, []string) {
	gate, reasons := baseGate(in)
	if len(in.regressions) > 0 {
		gate, reasons = floorGate(gate, reasons, eval.GateWarn, "regressed: "+describeRegressions(in.regressions))
	}
	return gate, reasons
}

func baseGate(in gateInput) (string, []string) {
	if len(in.flips) > 0 {
		return eval.GateBlock, []string{fmt.Sprintf(
			"deterministic flip: %s passed every baseline trial and passed no current trial",
			strings.Join(in.flips, ", "))}
	}
	paired := in.paired
	if in.pairs < minGateTasks || paired == nil || math.IsNaN(paired.StdErr) || math.IsNaN(paired.UpperBound) {
		return eval.GateInconclusive, []string{fmt.Sprintf(
			"%d paired task(s); the paired interval needs at least %d", in.pairs, minGateTasks)}
	}
	if paired.UpperBound < 0 {
		return eval.GateBlock, []string{fmt.Sprintf(
			"one-sided 95%% upper bound on the mean delta is %+.3f, below 0",
			paired.UpperBound)}
	}
	if paired.MeanDelta < -in.warnMargin-fractionEpsilon {
		return eval.GateWarn, []string{fmt.Sprintf(
			"mean delta %+.3f is below -%.3f and the one-sided 95%% upper bound %+.3f is not below 0",
			paired.MeanDelta, in.warnMargin, paired.UpperBound)}
	}
	return eval.GatePass, nil
}

// gateSeverity orders the gates: a floor never lowers a more severe gate.
var gateSeverity = map[string]int{
	eval.GatePass:         0,
	eval.GateInconclusive: 1,
	eval.GateWarn:         2,
	eval.GateBlock:        3,
}

// floorGate raises gate to at least floor and records reason. A gate more
// severe than floor is returned unchanged.
func floorGate(gate string, reasons []string, floor, reason string) (string, []string) {
	if gateSeverity[gate] > gateSeverity[floor] {
		return gate, reasons
	}
	return floor, append(reasons, reason)
}

func describeRegressions(regressions []eval.TaskRegression) string {
	parts := make([]string, len(regressions))
	for i, r := range regressions {
		parts[i] = fmt.Sprintf("%s (pass fraction %.2f → %.2f)", r.TaskID, r.BaselinePassFraction, r.CurrentPassFraction)
	}
	return strings.Join(parts, ", ")
}

// singleTrialExceedsMargin reports whether losing one trial on one of n
// paired tasks, each run k times, lowers the mean pass fraction by more
// than the warn margin.
func singleTrialExceedsMargin(n, k int, margin float64) bool {
	return n > 0 && k > 0 && margin+fractionEpsilon < 1/float64(n*k)
}

// rateSummary describes one result: mean pass fraction, task-level SE,
// a Wilson interval over tasks (trials of one task are not independent,
// so they are not pooled), and pass^k / pass@k up to the smallest
// per-task trial count.
func rateSummary(r eval.SuiteResult) eval.RateSummary {
	s := eval.RateSummary{Tasks: len(r.Tasks), Trials: suiteTrials(r)}
	if len(r.Tasks) == 0 {
		s.WilsonLow, s.WilsonHigh = Wilson95(0, 0)
		return s
	}

	fractions := make([]float64, len(r.Tasks))
	counts := make([]eval.TrialCounts, len(r.Tasks))
	minTrials := 0
	for i, t := range r.Tasks {
		counts[i] = t.Counts()
		fractions[i] = counts[i].PassFraction()
		if minTrials == 0 || counts[i].Total() < minTrials {
			minTrials = counts[i].Total()
		}
	}

	s.PassRate = mean(fractions)
	if se, ok := StdErr(fractions); ok {
		s.StdErr = &se
	}
	s.WilsonLow, s.WilsonHigh = Wilson95(s.PassRate*float64(len(r.Tasks)), len(r.Tasks))

	for k := 1; k <= minTrials; k++ {
		var hat, at float64
		for _, c := range counts {
			hat += PassHatK(c.Pass, c.Total(), k)
			at += PassAtK(c.Pass, c.Total(), k)
		}
		s.PassHatK = append(s.PassHatK, hat/float64(len(counts)))
		s.PassAtK = append(s.PassAtK, at/float64(len(counts)))
	}
	return s
}

// suiteTrials is the result's declared trials per task, falling back to
// the largest per-task trial count for results that predate the field.
func suiteTrials(r eval.SuiteResult) int {
	if r.Trials > 0 {
		return r.Trials
	}
	k := 0
	for _, t := range r.Tasks {
		k = max(k, t.Counts().Total())
	}
	return max(k, 1)
}

func indexByTaskID(tasks []eval.TaskResult) map[string]*eval.TaskResult {
	m := make(map[string]*eval.TaskResult, len(tasks))
	for i := range tasks {
		m[tasks[i].TaskID] = &tasks[i]
	}
	return m
}

// turnsDelta returns 0 if either trace is nil.
func turnsDelta(baseline, current *eval.TaskResult) int {
	if baseline.Trace == nil || current.Trace == nil {
		return 0
	}
	return current.Trace.Turns - baseline.Trace.Turns
}
