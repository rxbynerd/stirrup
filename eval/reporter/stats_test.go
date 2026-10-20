package reporter

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// within checks got against want within tol. It reports NaN explicitly
// because InDelta alone accepts it.
func within(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	must := require.New(t)
	must.False(math.IsNaN(got), "%s = NaN, want %.6f ± %g", name, want, tol)
	must.InDelta(want, got, tol, "%s", name)
}

func TestStudentTQuantile_MatchesReferenceTables(t *testing.T) {
	cases := []struct {
		name string
		p    float64
		df   int
		want float64
	}{
		{"p 0.975 df 1", 0.975, 1, 12.706},
		{"p 0.95 df 1", 0.95, 1, 6.314},
		{"p 0.975 df 2", 0.975, 2, 4.303},
		{"p 0.95 df 4", 0.95, 4, 2.132},
		{"p 0.975 df 4", 0.975, 4, 2.776},
		{"p 0.95 df 9", 0.95, 9, 1.833},
		{"p 0.975 df 9", 0.975, 9, 2.262},
		{"p 0.975 df 30", 0.975, 30, 2.042},
		{"p 0.995 df 10", 0.995, 10, 3.169},
		{"p 0.975 df 1000", 0.975, 1000, 1.962},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			within(t, "upper tail", StudentTQuantile(c.p, c.df), c.want, 0.0005)
			within(t, "lower tail", StudentTQuantile(1-c.p, c.df), -c.want, 0.0005)
		})
	}

	t.Run("median is zero", func(t *testing.T) {
		must := require.New(t)
		must.Zero(StudentTQuantile(0.5, 3))
	})

	t.Run("invalid arguments are NaN", func(t *testing.T) {
		must := require.New(t)
		for _, bad := range []struct {
			p  float64
			df int
		}{{0.975, 0}, {0, 5}, {1, 5}, {math.NaN(), 5}} {
			got := StudentTQuantile(bad.p, bad.df)
			must.True(math.IsNaN(got), "StudentTQuantile(%v, %d) = %v, want NaN", bad.p, bad.df, got)
		}
	})
}

func TestSignFlipPValue_EdgeCases(t *testing.T) {
	t.Run("all-zero diffs have p 1 and are exact", func(t *testing.T) {
		must := require.New(t)
		p, exact := SignFlipPValue([]float64{0, 0, 0})
		must.InDelta(1.0, p, 1e-12)
		must.True(exact)
	})

	t.Run("single non-zero diff has p 1", func(t *testing.T) {
		must := require.New(t)
		p, _ := SignFlipPValue([]float64{-0.5})
		must.InDelta(1.0, p, 1e-12)
	})

	t.Run("five equal drops", func(t *testing.T) {
		must := require.New(t)
		// Only the two all-same-sign assignments reach |5|.
		p, exact := SignFlipPValue([]float64{-1, -1, -1, -1, -1})
		must.InDelta(2.0/32, p, 1e-12)
		must.True(exact)
	})

	t.Run("25 non-zero diffs use the normal approximation", func(t *testing.T) {
		must := require.New(t)
		many := make([]float64, 25)
		for i := range many {
			many[i] = -1
		}
		p, exact := SignFlipPValue(many)
		must.False(exact)
		within(t, "normal-approximation p", p, math.Erfc(5/math.Sqrt2), 1e-12)
	})

	t.Run("zeros do not count toward the enumeration bound", func(t *testing.T) {
		must := require.New(t)
		padded := append(make([]float64, 30), -1, -1, 1)
		_, exact := SignFlipPValue(padded)
		must.True(exact)
	})
}

func TestPaired_Degenerate(t *testing.T) {
	t.Run("no diffs are undefined", func(t *testing.T) {
		must := require.New(t)
		_, ok := Paired(nil)
		must.False(ok)
	})

	t.Run("one diff has no standard error", func(t *testing.T) {
		must := require.New(t)
		_, ok := Paired([]float64{-1})
		must.False(ok)
	})

	t.Run("all-zero diffs", func(t *testing.T) {
		must := require.New(t)
		zero, ok := Paired([]float64{0, 0, 0, 0})
		must.True(ok)
		must.InDelta(0.0, zero.MeanDelta, 1e-12)
		must.InDelta(0.0, zero.StdErr, 1e-12)
		must.InDelta(0.0, zero.CILow, 1e-12)
		must.InDelta(0.0, zero.CIHigh, 1e-12)
		must.InDelta(0.0, zero.UpperBound, 1e-12)
		must.InDelta(1.0, zero.PValue, 1e-12)
		// MDE is undefined at zero SE.
		must.Nil(zero.MDE)
	})

	t.Run("equal drops have zero SE and a negative upper bound", func(t *testing.T) {
		must := require.New(t)
		drop, ok := Paired([]float64{-1.0 / 3, -1.0 / 3, -1.0 / 3})
		must.True(ok)
		within(t, "equal-drop SE", drop.StdErr, 0, 1e-12)
		must.Less(drop.UpperBound, 0.0)
	})
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
			must := require.New(t)
			ps, ok := Paired(tc.diffs)
			must.True(ok)
			must.NotNil(ps.MDE)
			within(t, "MDE / SE", *ps.MDE/ps.StdErr, tc.tSum, 0.0005)
			must.Greater(*ps.MDE, zBased*ps.StdErr)
		})
	}

	small, _ := Paired(cases[0].diffs)
	within(t, "df 4 MDE over the z-based MDE", *small.MDE/(zBased*small.StdErr), (2.7764+0.9410)/zBased, 0.001)
}

func TestStdErr_RequiresTwoScores(t *testing.T) {
	must := require.New(t)
	_, ok := StdErr([]float64{1})
	must.False(ok)

	se, ok := StdErr([]float64{1, 0})
	must.True(ok)
	within(t, "SE of {1,0}", se, 0.5, 1e-12)
}

func TestPassK_Estimators(t *testing.T) {
	t.Run("pass^k closed form", func(t *testing.T) {
		within(t, "pass^2 at 2/3", PassHatK(2, 3, 2), 1.0/3, 1e-12)
		within(t, "pass^1 is the pass fraction", PassHatK(7, 10, 1), 0.7, 1e-12)
	})

	t.Run("pass@k closed form", func(t *testing.T) {
		within(t, "pass@2 at 1/3", PassAtK(1, 3, 2), 2.0/3, 1e-12)
		within(t, "pass@1 is the pass fraction", PassAtK(7, 10, 1), 0.7, 1e-12)
	})

	t.Run("pass^k with fewer passes than k is zero", func(t *testing.T) {
		must := require.New(t)
		must.Zero(PassHatK(1, 3, 2))
	})

	t.Run("pass@k with every trial passing is one", func(t *testing.T) {
		must := require.New(t)
		must.InDelta(1.0, PassAtK(3, 3, 3), 1e-12)
	})

	t.Run("pass^k over many trials stays a probability", func(t *testing.T) {
		must := require.New(t)
		v := PassHatK(400, 500, 200)
		must.GreaterOrEqual(v, 0.0)
		must.LessOrEqual(v, 1.0)
	})

	t.Run("k above the trial count is NaN", func(t *testing.T) {
		must := require.New(t)
		must.True(math.IsNaN(PassHatK(1, 1, 2)))
	})
}

func TestWilson95_IntervalsAndFormatting(t *testing.T) {
	t.Run("all successes", func(t *testing.T) {
		lo, hi := Wilson95(5, 5)
		within(t, "5/5 low", lo, 0.5655, 0.0001)
		within(t, "5/5 high", hi, 1, 1e-12)
	})

	t.Run("mixed outcomes", func(t *testing.T) {
		lo, hi := Wilson95(36, 40)
		within(t, "36/40 low", lo, 0.7695, 0.0001)
		within(t, "36/40 high", hi, 0.9605, 0.0001)
	})

	t.Run("no successes", func(t *testing.T) {
		lo, hi := Wilson95(0, 10)
		within(t, "0/10 low", lo, 0, 1e-12)
		within(t, "0/10 high", hi, 0.2775, 0.0001)
	})

	t.Run("no samples spans the unit interval", func(t *testing.T) {
		must := require.New(t)
		lo, hi := Wilson95(0, 0)
		must.InDelta(0.0, lo, 1e-12)
		must.InDelta(1.0, hi, 1e-12)
	})

	t.Run("formats the interval as a percentage range", func(t *testing.T) {
		must := require.New(t)
		must.Equal("56.6-100.0%", FormatWilson95(1, 5))
	})

	t.Run("formats no samples as n/a", func(t *testing.T) {
		must := require.New(t)
		must.Equal("n/a", FormatWilson95(0.5, 0))
	})
}

func TestFlipFalseAlarmRate_ClosedForm(t *testing.T) {
	within(t, "p=0.98 n=5", FlipFalseAlarmRate(0.98, 5, 1), 0.096, 0.0005)
	within(t, "p=0.95 n=10", FlipFalseAlarmRate(0.95, 10, 1), 0.401, 0.0005)
	within(t, "single run equals 1-p^n", FlipFalseAlarmRate(0.9, 7, 1), 1-math.Pow(0.9, 7), 1e-12)
	within(t, "3-of-3 flip rule", FlipFalseAlarmRate(0.98, 5, 3), 1-math.Pow(1-0.02*0.02*0.02, 5), 1e-15)

	t.Run("perfectly reliable agent never false-alarms", func(t *testing.T) {
		must := require.New(t)
		must.Zero(FlipFalseAlarmRate(1, 5, 3))
	})
}

func TestSignFlipPValue_ExactEnumerationBound(t *testing.T) {
	must := require.New(t)
	diffs := func(n int) []float64 {
		d := make([]float64, n)
		for i := range d {
			d[i] = -1.0 / 3
		}
		return d
	}

	p, exact := SignFlipPValue(diffs(20))
	must.True(exact)
	within(t, "20 equal drops", p, math.Pow(2, -19), 1e-15)

	p, exact = SignFlipPValue(diffs(21))
	must.False(exact)
	within(t, "21 equal drops", p, math.Erfc(math.Sqrt(21)/math.Sqrt2), 1e-12)
}

// TestSignFlipPValue_TiesMatchExactArithmetic compares the enumeration
// against integer arithmetic on thirds, where sums of 1/3-valued diffs tie
// exactly in rational arithmetic but not always in floating point.
func TestSignFlipPValue_TiesMatchExactArithmetic(t *testing.T) {
	must := require.New(t)
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
		must.True(exact, "diffs %v (thirds %v)", diffs, thirds)
		must.InDelta(want, got, 1e-12, "diffs %v (thirds %v)", diffs, thirds)
	}
}
