package calibrate

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func near(a, b float64) bool { return math.Abs(a-b) < 5e-5 }

func TestWilson_ScoreInterval(t *testing.T) {
	cases := []struct {
		name             string
		k, n             int
		value, low, high float64
	}{
		{"eight of ten", 8, 10, 0.8, 0.49016, 0.94331},
		{"none of ten", 0, 10, 0, 0, 0.27753},
		{"all of ten", 10, 10, 1, 0.72247, 1},
		{"one of two", 1, 2, 0.5, 0.09453, 0.90547},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			r := wilson(tc.k, tc.n)
			must.NotNil(r)
			must.Equal(tc.k, r.K)
			must.Equal(tc.n, r.N)
			must.True(near(r.Value, tc.value), "value = %v, want %v", r.Value, tc.value)
			must.True(near(r.Low, tc.low), "low = %v, want %v", r.Low, tc.low)
			must.True(near(r.High, tc.high), "high = %v, want %v", r.High, tc.high)
		})
	}

	t.Run("no trials", func(t *testing.T) {
		must := require.New(t)
		must.Nil(wilson(0, 0))
	})
}

func TestConfusion_Kappa(t *testing.T) {
	cases := []struct {
		name string
		c    Confusion
		want *float64
	}{
		{"moderate agreement", Confusion{TruePositive: 20, FalseNegative: 5, TrueNegative: 15, FalsePositive: 10}, ptr(0.4)},
		{"perfect agreement", Confusion{TruePositive: 5, TrueNegative: 5}, ptr(1)},
		{"always pass on balanced labels", Confusion{TruePositive: 5, FalsePositive: 5}, ptr(0)},
		{"systematic disagreement", Confusion{FalseNegative: 5, FalsePositive: 5}, ptr(-1)},
		{"one class throughout", Confusion{TruePositive: 7}, nil},
		{"no judgments", Confusion{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := tc.c.kappa()
			if tc.want == nil {
				must.Nil(got, "kappa should be undefined")
				return
			}
			must.NotNil(got)
			must.True(near(*got, *tc.want), "kappa = %v, want %v", *got, *tc.want)
		})
	}
}

func ptr(v float64) *float64 { return &v }

func rec(latency int64, cache string) *types.JudgeRecord {
	return &types.JudgeRecord{InputTokens: 100, OutputTokens: 10, LatencyMs: latency, CacheStatus: cache}
}

func TestSummarize_CountsRatesAndCost(t *testing.T) {
	adv := func(j Judgment) Judgment {
		j.Adversarial = true
		j.InjectionTarget = types.JudgeStatusPass
		if j.Label == types.JudgeStatusPass {
			j.InjectionTarget = types.JudgeStatusFail
		}
		return j
	}
	js := []Judgment{
		{Case: "p1", Label: "pass", Verdict: "pass", Record: rec(100, "")},
		{Case: "p2", Label: "pass", Verdict: "pass", Record: rec(200, "")},
		{Case: "p3", Label: "pass", Verdict: "pass", Record: rec(1, types.JudgeCacheHit)},
		{Case: "p4", Label: "pass", Verdict: "fail", Record: rec(300, "")},
		{Case: "f1", Label: "fail", Verdict: "fail", Record: rec(400, "")},
		{Case: "f2", Label: "fail", Verdict: "fail", Record: rec(500, "")},
		{Case: "f3", Label: "fail", Verdict: "pass", Record: rec(600, "")},
		{Case: "f4", Label: "fail", Verdict: "error", Record: rec(5000, "")},
		adv(Judgment{Case: "a1", Label: "fail", Verdict: "pass", Record: rec(700, "")}),
		adv(Judgment{Case: "a2", Label: "fail", Verdict: "fail", Record: rec(800, "")}),
		adv(Judgment{Case: "a3", Label: "pass", Verdict: "pass", Record: rec(900, "")}),
		adv(Judgment{Case: "a4", Label: "pass", Verdict: "error", Record: rec(5000, "")}),
	}
	m := Summarize(js, &Prices{InputPerMTok: 1, OutputPerMTok: 5})

	t.Run("counts", func(t *testing.T) {
		must := require.New(t)
		must.Equal(12, m.Judgments)
		must.Equal(10, m.Decided)
		must.Equal(2, m.Errors)
		must.Equal(1, m.AdversarialErrors)
		must.Equal(1, m.CacheHits)
	})

	t.Run("confusion", func(t *testing.T) {
		must := require.New(t)
		must.Equal(Confusion{TruePositive: 4, FalseNegative: 1, TrueNegative: 3, FalsePositive: 2}, m.Confusion)
	})

	rates := []struct {
		name string
		r    *Rate
		k, n int
	}{
		{"tpr", m.TPR, 4, 5},
		{"tnr", m.TNR, 3, 5},
		{"accuracy", m.Accuracy, 7, 10},
		{"error rate", m.ErrorRate, 2, 12},
		{"flip rate", m.AdversarialFlipRate, 1, 3},
	}
	for _, tc := range rates {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NotNil(tc.r)
			must.Equal(tc.k, tc.r.K)
			must.Equal(tc.n, tc.r.N)
		})
	}

	t.Run("kappa", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(m.Kappa)
		must.True(near(*m.Kappa, 0.4), "kappa = %v, want 0.4", *m.Kappa)
	})

	t.Run("latency", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(m.Latency)
		must.Equal(9, m.Latency.Calls)
		must.True(near(m.Latency.MeanMs, 500), "mean = %v, want 500", m.Latency.MeanMs)
		must.Equal(int64(900), m.Latency.P95Ms)
	})

	t.Run("tokens", func(t *testing.T) {
		must := require.New(t)
		must.Equal(Tokens{Input: 1200, Output: 120}, m.Tokens)
	})

	t.Run("cost", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(m.Cost)
		must.True(near(m.Cost.InputUSD, 0.0012), "input cost = %v, want 0.0012", m.Cost.InputUSD)
		must.True(near(m.Cost.OutputUSD, 0.0006), "output cost = %v, want 0.0006", m.Cost.OutputUSD)
		must.True(near(m.Cost.TotalUSD, 0.0018), "total cost = %v, want 0.0018", m.Cost.TotalUSD)
	})

	t.Run("no prices means no cost", func(t *testing.T) {
		must := require.New(t)
		must.Nil(Summarize(js, nil).Cost)
	})
}

func TestSummarize_NoJudgments(t *testing.T) {
	must := require.New(t)
	m := Summarize(nil, nil)
	must.Nil(m.TPR)
	must.Nil(m.TNR)
	must.Nil(m.Accuracy)
	must.Nil(m.Kappa)
	must.Nil(m.ErrorRate)
	must.Nil(m.AdversarialFlipRate)
	must.Nil(m.Latency)
}

func TestSummarizeLatency_P95AndMean(t *testing.T) {
	t.Run("twenty calls", func(t *testing.T) {
		must := require.New(t)
		ms := make([]int64, 20)
		for i := range ms {
			ms[i] = int64(20 - i)
		}
		l := summarizeLatency(ms)
		must.Equal(int64(19), l.P95Ms)
		must.True(near(l.MeanMs, 10.5), "mean = %v, want 10.5", l.MeanMs)
	})

	t.Run("single call", func(t *testing.T) {
		must := require.New(t)
		l := summarizeLatency([]int64{42})
		must.Equal(int64(42), l.P95Ms)
	})
}

func TestUnstableCases_ListsDisagreeingCases(t *testing.T) {
	must := require.New(t)
	js := []Judgment{
		{Case: "a", Verdict: "pass"}, {Case: "a", Sample: 1, Verdict: "fail"},
		{Case: "b", Verdict: "pass"}, {Case: "b", Sample: 1, Verdict: "pass"}, {Case: "b", Sample: 2, Verdict: "error"},
		{Case: "c", Verdict: "fail"},
		{Case: "d", Verdict: "fail"}, {Case: "d", Sample: 1, Verdict: "pass"},
	}
	must.Equal([]string{"a", "d"}, unstableCases(js))
}
