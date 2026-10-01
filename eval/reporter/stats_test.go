package reporter

import (
	"math"
	"testing"
)

func within(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f ± %g", name, got, want, tol)
	}
}

func TestStudentTQuantile(t *testing.T) {
	cases := []struct {
		p    float64
		df   int
		want float64
	}{
		{0.975, 1, 12.706},
		{0.95, 1, 6.314},
		{0.975, 2, 4.303},
		{0.95, 4, 2.132},
		{0.975, 4, 2.776},
		{0.95, 9, 1.833},
		{0.975, 9, 2.262},
		{0.975, 30, 2.042},
		{0.995, 10, 3.169},
		{0.975, 1000, 1.962},
	}
	for _, c := range cases {
		within(t, "t quantile", StudentTQuantile(c.p, c.df), c.want, 0.0005)
		within(t, "t quantile (lower tail)", StudentTQuantile(1-c.p, c.df), -c.want, 0.0005)
	}
	if StudentTQuantile(0.5, 3) != 0 {
		t.Error("median of t should be 0")
	}
	for _, bad := range []struct {
		p  float64
		df int
	}{{0.975, 0}, {0, 5}, {1, 5}, {math.NaN(), 5}} {
		if got := StudentTQuantile(bad.p, bad.df); !math.IsNaN(got) {
			t.Errorf("StudentTQuantile(%v, %d) = %v, want NaN", bad.p, bad.df, got)
		}
	}
}

func TestSignFlipPValue(t *testing.T) {
	if p, exact := SignFlipPValue([]float64{0, 0, 0}); p != 1 || !exact {
		t.Errorf("all-zero diffs: p=%v exact=%v, want 1 true", p, exact)
	}
	if p, _ := SignFlipPValue([]float64{-0.5}); p != 1 {
		t.Errorf("single non-zero diff: p=%v, want 1", p)
	}
	// Five equal drops: only the two all-same-sign assignments reach |5|.
	if p, exact := SignFlipPValue([]float64{-1, -1, -1, -1, -1}); math.Abs(p-2.0/32) > 1e-12 || !exact {
		t.Errorf("five equal drops: p=%v exact=%v, want 0.0625 true", p, exact)
	}

	many := make([]float64, 25)
	for i := range many {
		many[i] = -1
	}
	p, exact := SignFlipPValue(many)
	if exact {
		t.Error("25 non-zero diffs should use the normal approximation")
	}
	within(t, "normal-approximation p", p, math.Erfc(5/math.Sqrt2), 1e-12)

	// Zeros do not count toward the exact-enumeration bound.
	padded := append(make([]float64, 30), -1, -1, 1)
	if _, exact := SignFlipPValue(padded); !exact {
		t.Error("three non-zero diffs among 33 should be enumerated exactly")
	}
}

func TestPaired_Degenerate(t *testing.T) {
	if _, ok := Paired(nil); ok {
		t.Error("no diffs: want ok=false")
	}
	if _, ok := Paired([]float64{-1}); ok {
		t.Error("one diff: SE undefined, want ok=false")
	}

	zero, ok := Paired([]float64{0, 0, 0, 0})
	if !ok {
		t.Fatal("all-zero diffs should be defined")
	}
	if zero.MeanDelta != 0 || zero.StdErr != 0 || zero.CILow != 0 || zero.CIHigh != 0 || zero.UpperBound != 0 || zero.MDE != 0 || zero.PValue != 1 {
		t.Errorf("all-zero diffs: %+v", zero)
	}

	drop, ok := Paired([]float64{-1.0 / 3, -1.0 / 3, -1.0 / 3})
	if !ok {
		t.Fatal("equal non-zero diffs should be defined")
	}
	within(t, "equal-drop SE", drop.StdErr, 0, 1e-12)
	if drop.UpperBound >= 0 {
		t.Errorf("equal drops: upper bound %v, want < 0", drop.UpperBound)
	}
}

func TestStdErr(t *testing.T) {
	if _, ok := StdErr([]float64{1}); ok {
		t.Error("one score: want ok=false")
	}
	se, ok := StdErr([]float64{1, 0})
	if !ok {
		t.Fatal("two scores should be defined")
	}
	within(t, "SE of {1,0}", se, 0.5, 1e-12)
}

func TestPassKEstimators(t *testing.T) {
	within(t, "pass^2 at 2/3", PassHatK(2, 3, 2), 1.0/3, 1e-12)
	within(t, "pass@2 at 1/3", PassAtK(1, 3, 2), 2.0/3, 1e-12)
	within(t, "pass^1 is the pass fraction", PassHatK(7, 10, 1), 0.7, 1e-12)
	within(t, "pass@1 is the pass fraction", PassAtK(7, 10, 1), 0.7, 1e-12)
	if PassHatK(1, 3, 2) != 0 {
		t.Error("pass^k with fewer passes than k should be 0")
	}
	if PassAtK(3, 3, 3) != 1 {
		t.Error("pass@k with every trial passing should be 1")
	}
	if v := PassHatK(400, 500, 200); math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		t.Errorf("pass^200 over 500 trials = %v, want a probability", v)
	}
	if !math.IsNaN(PassHatK(1, 1, 2)) {
		t.Error("k greater than the trial count should be NaN")
	}
}

func TestWilson95(t *testing.T) {
	lo, hi := Wilson95(5, 5)
	within(t, "5/5 low", lo, 0.5655, 0.0001)
	within(t, "5/5 high", hi, 1, 1e-12)

	lo, hi = Wilson95(36, 40)
	within(t, "36/40 low", lo, 0.7695, 0.0001)
	within(t, "36/40 high", hi, 0.9605, 0.0001)

	lo, hi = Wilson95(0, 10)
	within(t, "0/10 low", lo, 0, 1e-12)
	within(t, "0/10 high", hi, 0.2775, 0.0001)

	if lo, hi := Wilson95(0, 0); lo != 0 || hi != 1 {
		t.Errorf("n=0: [%v, %v], want [0, 1]", lo, hi)
	}
}

func TestFlipFalseAlarmRate(t *testing.T) {
	// Packet 03 §4.3 single-run column.
	within(t, "p=0.98 n=5", FlipFalseAlarmRate(0.98, 5, 1), 0.096, 0.0005)
	within(t, "p=0.95 n=10", FlipFalseAlarmRate(0.95, 10, 1), 0.401, 0.0005)
	within(t, "single run equals 1-p^n", FlipFalseAlarmRate(0.9, 7, 1), 1-math.Pow(0.9, 7), 1e-12)
	within(t, "3-of-3 flip rule", FlipFalseAlarmRate(0.98, 5, 3), 1-math.Pow(1-math.Pow(0.02, 3), 5), 1e-15)
	if FlipFalseAlarmRate(1, 5, 3) != 0 {
		t.Error("a perfectly reliable agent never false-alarms")
	}
}
