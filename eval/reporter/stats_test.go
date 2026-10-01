package reporter

import (
	"math"
	"math/rand/v2"
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
	if zero.MeanDelta != 0 || zero.StdErr != 0 || zero.CILow != 0 || zero.CIHigh != 0 || zero.UpperBound != 0 || zero.PValue != 1 {
		t.Errorf("all-zero diffs: %+v", zero)
	}
	if zero.MDE != nil {
		t.Errorf("all-zero diffs: MDE = %v, want nil (undefined at zero SE)", *zero.MDE)
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

func TestPaired_MDEUsesStudentT(t *testing.T) {
	zBased := (z975 + 0.8416212335729143)

	cases := []struct {
		name  string
		diffs []float64
		tSum  float64
	}{
		{"df 4", []float64{-0.4, -0.2, 0, 0.1, 0.2}, 2.7764 + 0.9410},
		{"df 9", []float64{-0.4, -0.2, 0, 0.1, 0.2, -0.1, 0, 0.3, -0.3, 0.1}, 2.2622 + 0.8834},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, ok := Paired(tc.diffs)
			if !ok || ps.MDE == nil {
				t.Fatalf("paired = %+v ok=%v, want a defined MDE", ps, ok)
			}
			within(t, "MDE / SE", *ps.MDE/ps.StdErr, tc.tSum, 0.0005)
			if *ps.MDE <= zBased*ps.StdErr {
				t.Errorf("MDE %v does not exceed the z-based %v", *ps.MDE, zBased*ps.StdErr)
			}
		})
	}

	small, _ := Paired(cases[0].diffs)
	within(t, "df 4 MDE over the z-based MDE", *small.MDE/(zBased*small.StdErr), (2.7764+0.9410)/zBased, 0.001)
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

	if got := FormatWilson95(1, 5); got != "56.6-100.0%" {
		t.Errorf("FormatWilson95(1, 5) = %q", got)
	}
	if got := FormatWilson95(0.5, 0); got != "n/a" {
		t.Errorf("FormatWilson95 with no samples = %q, want n/a", got)
	}
}

func TestFlipFalseAlarmRate(t *testing.T) {
	within(t, "p=0.98 n=5", FlipFalseAlarmRate(0.98, 5, 1), 0.096, 0.0005)
	within(t, "p=0.95 n=10", FlipFalseAlarmRate(0.95, 10, 1), 0.401, 0.0005)
	within(t, "single run equals 1-p^n", FlipFalseAlarmRate(0.9, 7, 1), 1-math.Pow(0.9, 7), 1e-12)
	within(t, "3-of-3 flip rule", FlipFalseAlarmRate(0.98, 5, 3), 1-math.Pow(1-0.02*0.02*0.02, 5), 1e-15)
	if FlipFalseAlarmRate(1, 5, 3) != 0 {
		t.Error("a perfectly reliable agent never false-alarms")
	}
}

func TestSignFlipPValue_ExactEnumerationBound(t *testing.T) {
	diffs := func(n int) []float64 {
		d := make([]float64, n)
		for i := range d {
			d[i] = -1.0 / 3
		}
		return d
	}

	p, exact := SignFlipPValue(diffs(20))
	if !exact {
		t.Error("20 non-zero diffs should be enumerated exactly")
	}
	within(t, "20 equal drops", p, math.Pow(2, -19), 1e-15)

	p, exact = SignFlipPValue(diffs(21))
	if exact {
		t.Error("21 non-zero diffs should use the normal approximation")
	}
	within(t, "21 equal drops", p, math.Erfc(math.Sqrt(21)/math.Sqrt2), 1e-12)
}

// TestSignFlipPValue_TiesMatchExactArithmetic compares the enumeration
// against integer arithmetic on thirds, where sums of 1/3-valued diffs tie
// exactly in rational arithmetic but not always in floating point.
func TestSignFlipPValue_TiesMatchExactArithmetic(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for range 400 {
		n := 2 + rng.IntN(11)
		thirds := make([]int, n)
		diffs := make([]float64, n)
		for i := range thirds {
			thirds[i] = rng.IntN(7) - 3
			diffs[i] = float64(thirds[i]) / 3
		}

		var abs []int
		observed := 0
		for _, v := range thirds {
			if v != 0 {
				abs = append(abs, max(v, -v))
				observed += v
			}
		}
		want := 1.0
		if len(abs) > 0 {
			hits := 0
			for mask := range 1 << len(abs) {
				sum := 0
				for i, a := range abs {
					if mask&(1<<i) != 0 {
						sum -= a
					} else {
						sum += a
					}
				}
				if max(sum, -sum) >= max(observed, -observed) {
					hits++
				}
			}
			want = float64(hits) / float64(int(1)<<len(abs))
		}

		got, exact := SignFlipPValue(diffs)
		if !exact || math.Abs(got-want) > 1e-12 {
			t.Fatalf("diffs %v (thirds %v): p = %v exact=%v, want %v", diffs, thirds, got, exact, want)
		}
	}
}
