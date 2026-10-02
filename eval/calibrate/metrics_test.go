package calibrate

import (
	"math"
	"slices"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func near(a, b float64) bool { return math.Abs(a-b) < 5e-5 }

func TestWilson(t *testing.T) {
	cases := []struct {
		k, n            int
		value, low, hig float64
	}{
		{8, 10, 0.8, 0.49016, 0.94331},
		{0, 10, 0, 0, 0.27753},
		{10, 10, 1, 0.72247, 1},
		{1, 2, 0.5, 0.09453, 0.90547},
	}
	for _, tc := range cases {
		r := wilson(tc.k, tc.n)
		if r == nil || r.K != tc.k || r.N != tc.n || !near(r.Value, tc.value) || !near(r.Low, tc.low) || !near(r.High, tc.hig) {
			t.Errorf("wilson(%d, %d) = %+v, want %.5f [%.5f, %.5f]", tc.k, tc.n, r, tc.value, tc.low, tc.hig)
		}
	}
	if r := wilson(0, 0); r != nil {
		t.Errorf("wilson(0, 0) = %+v, want nil", r)
	}
}

func TestKappa(t *testing.T) {
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
		got := tc.c.kappa()
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s: kappa = %v, want undefined", tc.name, *got)
		case tc.want != nil && (got == nil || !near(*got, *tc.want)):
			t.Errorf("%s: kappa = %v, want %v", tc.name, got, *tc.want)
		}
	}
}

func ptr(v float64) *float64 { return &v }

func rec(latency int64, cache string) *types.JudgeRecord {
	return &types.JudgeRecord{InputTokens: 100, OutputTokens: 10, LatencyMs: latency, CacheStatus: cache}
}

func TestSummarize(t *testing.T) {
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

	if m.Judgments != 12 || m.Decided != 10 || m.Errors != 2 || m.AdversarialErrors != 1 || m.CacheHits != 1 {
		t.Errorf("counts = %d judgments, %d decided, %d errors, %d adversarial errors, %d hits", m.Judgments, m.Decided, m.Errors, m.AdversarialErrors, m.CacheHits)
	}
	if want := (Confusion{TruePositive: 4, FalseNegative: 1, TrueNegative: 3, FalsePositive: 2}); m.Confusion != want {
		t.Errorf("confusion = %+v, want %+v", m.Confusion, want)
	}
	for name, got := range map[string]struct {
		r    *Rate
		k, n int
	}{
		"tpr":        {m.TPR, 4, 5},
		"tnr":        {m.TNR, 3, 5},
		"accuracy":   {m.Accuracy, 7, 10},
		"error rate": {m.ErrorRate, 2, 12},
		"flip rate":  {m.AdversarialFlipRate, 1, 3},
	} {
		if got.r == nil || got.r.K != got.k || got.r.N != got.n {
			t.Errorf("%s = %+v, want %d/%d", name, got.r, got.k, got.n)
		}
	}
	if m.Kappa == nil || !near(*m.Kappa, 0.4) {
		t.Errorf("kappa = %v, want 0.4", m.Kappa)
	}
	if m.Latency == nil || m.Latency.Calls != 9 || !near(m.Latency.MeanMs, 500) || m.Latency.P95Ms != 900 {
		t.Errorf("latency = %+v, want 9 calls, mean 500, p95 900", m.Latency)
	}
	if m.Tokens != (Tokens{Input: 1200, Output: 120}) {
		t.Errorf("tokens = %+v", m.Tokens)
	}
	if m.Cost == nil || !near(m.Cost.InputUSD, 0.0012) || !near(m.Cost.OutputUSD, 0.0006) || !near(m.Cost.TotalUSD, 0.0018) {
		t.Errorf("cost = %+v", m.Cost)
	}
	if Summarize(js, nil).Cost != nil {
		t.Error("a cost was estimated without prices")
	}
}

func TestSummarize_NoJudgments(t *testing.T) {
	m := Summarize(nil, nil)
	if m.TPR != nil || m.TNR != nil || m.Accuracy != nil || m.Kappa != nil || m.ErrorRate != nil || m.AdversarialFlipRate != nil || m.Latency != nil {
		t.Errorf("metrics of no judgments = %+v, want every rate undefined", m)
	}
}

func TestSummarizeLatency_P95(t *testing.T) {
	ms := make([]int64, 20)
	for i := range ms {
		ms[i] = int64(20 - i)
	}
	if l := summarizeLatency(ms); l.P95Ms != 19 || !near(l.MeanMs, 10.5) {
		t.Errorf("latency = %+v, want p95 19 and mean 10.5", l)
	}
	if l := summarizeLatency([]int64{42}); l.P95Ms != 42 {
		t.Errorf("single call latency = %+v", l)
	}
}

func TestUnstableCases(t *testing.T) {
	js := []Judgment{
		{Case: "a", Verdict: "pass"}, {Case: "a", Sample: 1, Verdict: "fail"},
		{Case: "b", Verdict: "pass"}, {Case: "b", Sample: 1, Verdict: "pass"}, {Case: "b", Sample: 2, Verdict: "error"},
		{Case: "c", Verdict: "fail"},
		{Case: "d", Verdict: "fail"}, {Case: "d", Sample: 1, Verdict: "pass"},
	}
	if got := unstableCases(js); !slices.Equal(got, []string{"a", "d"}) {
		t.Errorf("unstable = %v, want [a d]", got)
	}
}
