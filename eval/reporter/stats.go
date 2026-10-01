package reporter

import (
	"math"

	"github.com/rxbynerd/stirrup/eval"
)

// The estimators follow Miller, "Adding Error Bars to Evals" (arXiv
// 2411.00640) and the tau-bench pass^k / pass@k definitions (arXiv
// 2406.12045 §3); docs/eval.md#statistics states each formula.

const (
	z975 = 1.959963984540054

	// maxExactSignFlip bounds the exact permutation test at 2^20
	// enumerated sign assignments.
	maxExactSignFlip = 20
)

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// sampleVariance uses the n-1 denominator; ok is false below two values.
func sampleVariance(xs []float64) (v float64, ok bool) {
	if len(xs) < 2 {
		return 0, false
	}
	m := mean(xs)
	var ss float64
	for _, x := range xs {
		d := x - m
		ss += d * d
	}
	return ss / float64(len(xs)-1), true
}

// StdErr is the standard error of the mean of per-task scores (Miller
// eq. 1); ok is false below two scores.
func StdErr(xs []float64) (se float64, ok bool) {
	v, ok := sampleVariance(xs)
	if !ok {
		return 0, false
	}
	return math.Sqrt(v / float64(len(xs))), true
}

// Paired computes the paired-difference statistics over per-task
// differences d_i = current - baseline (docs/eval.md#statistics). The MDE
// is nil when the SE is zero; ok is false below two differences, where
// the SE is undefined.
func Paired(diffs []float64) (eval.PairedSummary, bool) {
	se, ok := StdErr(diffs)
	if !ok {
		return eval.PairedSummary{}, false
	}
	n := len(diffs)
	df := n - 1
	m := mean(diffs)
	t2 := StudentTQuantile(0.975, df)
	t1 := StudentTQuantile(0.95, df)
	p, exact := SignFlipPValue(diffs)
	summary := eval.PairedSummary{
		Tasks:       n,
		MeanDelta:   m,
		StdErr:      se,
		DF:          df,
		CILow:       m - t2*se,
		CIHigh:      m + t2*se,
		UpperBound:  m + t1*se,
		PValue:      p,
		PValueExact: exact,
	}
	if se > 0 {
		mde := (t2 + StudentTQuantile(0.80, df)) * se
		summary.MDE = &mde
	}
	return summary, true
}

// SignFlipPValue is the two-sided paired permutation test: the share of
// sign assignments to |d_i| whose absolute sum reaches the observed one.
// Zero differences are dropped; above maxExactSignFlip non-zero
// differences a normal approximation replaces the exact enumeration.
func SignFlipPValue(diffs []float64) (p float64, exact bool) {
	var abs []float64
	var observed, scale float64
	for _, d := range diffs {
		if d == 0 {
			continue
		}
		abs = append(abs, math.Abs(d))
		observed += d
		scale += math.Abs(d)
	}
	if len(abs) == 0 {
		return 1, true
	}
	target := math.Abs(observed) - 1e-9*scale

	if len(abs) <= maxExactSignFlip {
		total := 1 << len(abs)
		hits := 0
		for mask := range total {
			var s float64
			for i, a := range abs {
				if mask&(1<<i) != 0 {
					s -= a
				} else {
					s += a
				}
			}
			if math.Abs(s) >= target {
				hits++
			}
		}
		return float64(hits) / float64(total), true
	}

	var ss float64
	for _, a := range abs {
		ss += a * a
	}
	z := math.Abs(observed) / math.Sqrt(ss)
	return math.Erfc(z / math.Sqrt2), false
}

// StudentTQuantile returns t with P(T <= t) = p for Student's t with df
// degrees of freedom, by bisection on the exact CDF (regularised
// incomplete beta). It returns NaN for df < 1 or p outside (0, 1).
func StudentTQuantile(p float64, df int) float64 {
	if df < 1 || !(p > 0 && p < 1) {
		return math.NaN()
	}
	if p == 0.5 {
		return 0
	}
	if p < 0.5 {
		return -StudentTQuantile(1-p, df)
	}
	lo, hi := 0.0, 1.0
	for studentTCDF(hi, df) < p {
		lo = hi
		hi *= 2
	}
	for range 200 {
		mid := (lo + hi) / 2
		if studentTCDF(mid, df) < p {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-12*hi {
			break
		}
	}
	return (lo + hi) / 2
}

func studentTCDF(t float64, df int) float64 {
	v := float64(df)
	tail := 0.5 * regIncBeta(v/(v+t*t), v/2, 0.5)
	if t >= 0 {
		return 1 - tail
	}
	return tail
}

// regIncBeta is the regularised incomplete beta function I_x(a, b),
// evaluated with the continued fraction on whichever side converges
// quickly (Numerical Recipes §6.4).
func regIncBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(x, a, b) / a
	}
	return 1 - front*betaContinuedFraction(1-x, b, a)/b
}

func betaContinuedFraction(x, a, b float64) float64 {
	const (
		maxIter = 300
		eps     = 1e-15
		tiny    = 1e-300
	)
	clampTiny := func(v float64) float64 {
		if math.Abs(v) < tiny {
			return tiny
		}
		return v
	}
	c := 1.0
	d := 1 / clampTiny(1-(a+b)*x/(a+1))
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		even := fm * (b - fm) * x / ((a + 2*fm - 1) * (a + 2*fm))
		d = 1 / clampTiny(1+even*d)
		c = clampTiny(1 + even/c)
		h *= d * c
		odd := -(a + fm) * (a + b + fm) * x / ((a + 2*fm) * (a + 2*fm + 1))
		d = 1 / clampTiny(1+odd*d)
		c = clampTiny(1 + odd/c)
		step := d * c
		h *= step
		if math.Abs(step-1) < eps {
			break
		}
	}
	return h
}

// binomialRatio returns C(a, k) / C(n, k) without forming either
// coefficient, so large trial counts cannot overflow.
func binomialRatio(a, n, k int) float64 {
	if k < 0 || k > n {
		return math.NaN()
	}
	if a < k {
		return 0
	}
	r := 1.0
	for i := range k {
		r *= float64(a-i) / float64(n-i)
	}
	return r
}

// PassHatK is the unbiased estimate of the chance that k independent
// trials all pass, given c passes in n trials: C(c,k)/C(n,k).
func PassHatK(c, n, k int) float64 {
	return binomialRatio(c, n, k)
}

// PassAtK is the unbiased estimate of the chance that at least one of k
// independent trials passes, given c passes in n trials:
// 1 - C(n-c,k)/C(n,k).
func PassAtK(c, n, k int) float64 {
	return 1 - binomialRatio(n-c, n, k)
}

// Wilson95 is the 95% Wilson score interval for a proportion of successes
// out of n. successes may be fractional (a mean of per-task pass
// fractions times the task count). With n <= 0 it returns [0, 1].
func Wilson95(successes float64, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 1
	}
	nf := float64(n)
	p := math.Min(math.Max(successes/nf, 0), 1)
	z2 := z975 * z975
	denom := 1 + z2/nf
	center := p + z2/(2*nf)
	margin := z975 * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	lo = math.Max((center-margin)/denom, 0)
	hi = math.Min((center+margin)/denom, 1)
	return lo, hi
}

// FlipFalseAlarmRate is the per-push probability that an unchanged agent
// trips a gate that blocks when any of n tasks fails all k of its trials,
// with each trial passing independently with probability p:
// 1 - (1 - (1-p)^k)^n. At k = 1 this is the single-run 1 - p^n.
func FlipFalseAlarmRate(p float64, n, k int) float64 {
	perTask := math.Pow(1-p, float64(k))
	return 1 - math.Pow(1-perTask, float64(n))
}
