package eval

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func trials(outcomes ...string) []TrialResult {
	out := make([]TrialResult, len(outcomes))
	for i, o := range outcomes {
		out[i] = TrialResult{Trial: i + 1, Outcome: o}
	}
	return out
}

func TestTrialCounts_MajorityOutcome(t *testing.T) {
	cases := []struct {
		name     string
		outcomes []string
		want     string
		fraction float64
	}{
		{"single pass", []string{"pass"}, "pass", 1},
		{"single fail", []string{"fail"}, "fail", 0},
		{"single error", []string{"error"}, "error", 0},
		{"two of three pass", []string{"pass", "fail", "pass"}, "pass", 2.0 / 3},
		{"two of three fail confirms the failure", []string{"fail", "pass", "fail"}, "fail", 1.0 / 3},
		{"errors never confirm a failure", []string{"fail", "error", "pass"}, "error", 1.0 / 3},
		{"error majority", []string{"error", "error", "pass"}, "error", 1.0 / 3},
		{"pass majority despite an error", []string{"pass", "error", "pass"}, "pass", 2.0 / 3},
		{"fail majority despite an error", []string{"fail", "fail", "error"}, "fail", 0},
		{"even tie has no majority", []string{"pass", "fail"}, "error", 0.5},
		{"two-two tie has no majority", []string{"pass", "pass", "fail", "fail"}, "error", 0.5},
		{"three of four pass", []string{"pass", "pass", "fail", "pass"}, "pass", 0.75},
		{"unknown outcome counts as error", []string{"skipped", "pass", "pass"}, "pass", 2.0 / 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := TaskResult{Trials: trials(tc.outcomes...)}.Counts()
			must.Equal(len(tc.outcomes), c.Total())
			must.Equal(tc.want, c.Outcome(), "counts %+v", c)
			must.InDelta(tc.fraction, c.PassFraction(), 1e-9)
		})
	}
}

func TestTrialCounts_Empty_ReportsError(t *testing.T) {
	must := require.New(t)
	var c TrialCounts
	must.Equal("error", c.Outcome())
	must.Zero(c.PassFraction())
	must.Zero(c.Total())
}

// TestTaskResult_Counts_FallsBackToOutcome pins that a result without Trials
// counts as one trial of its Outcome.
func TestTaskResult_Counts_FallsBackToOutcome(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		want    TrialCounts
	}{
		{"pass outcome", "pass", TrialCounts{Pass: 1}},
		{"fail outcome", "fail", TrialCounts{Fail: 1}},
		{"error outcome", "error", TrialCounts{Error: 1}},
		{"empty outcome", "", TrialCounts{Error: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, TaskResult{Outcome: tc.outcome}.Counts())
		})
	}
}

func TestTaskResult_Counts_PrefersTrials(t *testing.T) {
	must := require.New(t)
	r := TaskResult{Outcome: "pass", Trials: trials("fail", "fail", "pass")}
	must.Equal(TrialCounts{Pass: 1, Fail: 2}, r.Counts(), "trials take precedence over Outcome")
}
